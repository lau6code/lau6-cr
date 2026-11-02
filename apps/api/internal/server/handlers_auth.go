package server

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
	"github.com/lau6code/lau6-cr/apps/api/internal/service"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// loginRequest 是登录请求体。
//
// 明文模式：{username, password}。
//
// 加密模式（password.mode != plaintext）：{username, kid, epk, nonce, ct}——
// 口令在信封里，但 **username 仍然明文带着**：HKDF 的 info 绑定了它，
// 服务端得先有用户名才能派生密钥（13-password-transport.md §4.1）。
type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	// 应用层加密信封（password.mode != plaintext 时使用）。
	Kid   string `json:"kid"`
	Epk   string `json:"epk"`
	Nonce string `json:"nonce"`
	Ct    string `json:"ct"`
}

func (r loginRequest) envelope() passwordEnvelope {
	return passwordEnvelope{Kid: r.Kid, Epk: r.Epk, Nonce: r.Nonce, Ct: r.Ct}
}

// 两条登录失败路径（认证后端不可用 / 凭据不对）共用同一份审计内容，
// 只有响应文案与状态码不同。
func (a *App) auditLoginFailure(r *http.Request, username, ip string, err error) {
	a.auditFailure(r, username, "auth.login.failure", "", "", ip,
		map[string]any{"reason": classifyAuthError(err)})
}

// handleLogin 处理 UI 登录。
func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	// 口令来源（明文 / 密文）
	//
	// 闸门在 password.mode 上：required 只收密文、plaintext 只收明文、
	// optional 两者都收（优先解密）。
	secret, err := a.resolvePassword(r.Context(), req.envelope(), req.Username, req.Password, "", false)
	if err != nil {
		a.failPassword(w, err)
		return
	}
	ip := clientIP(r)
	res, err := a.AuthSvc.VerifyCredentials(r.Context(), secret.Username, secret.Password)
	if err != nil {
		// LDAP 目录不可达 ≠ 口令错：这时必须说真话（503），
		// 否则运维会照着"用户名或密码错误"去查谁在暴力破解，
		// 而真正的原因是目录挂了。
		//
		// 它不泄露账号是否存在（两种情况都不指向具体用户），
		// 与 13-password-transport.md §5.4 的防枚举口径不冲突。
		if errors.Is(err, service.ErrAuthUnavailable) {
			a.Logger.Error("认证后端不可用，登录未完成", "err", err)
			a.auditLoginFailure(r, secret.Username, ip, err)
			Fail(w, http.StatusServiceUnavailable, CodeUnavailable, "认证服务暂时不可用，请稍后重试")
			return
		}
		// 统一返回同一条文案，不区分"用户不存在"与"密码错误"。
		//
		// ⚠️ 失败路径：响应本来就是 401，审计写失败**不能**把它改写成 500——
		// 那会把"口令不对"说成"服务器炸了"，是另一种撒谎。
		// 但绝不静默：记 error 日志 + 失败计数。
		a.auditLoginFailure(r, secret.Username, ip, err)
		Fail(w, http.StatusUnauthorized, CodeUnauthorized, "用户名或密码错误")
		return
	}
	ttl := time.Duration(a.SettingsSvc.Snapshot().Core.SessionTTLSeconds) * time.Second
	raw, err := a.AuthSvc.CreateSession(r.Context(), res.User.ID, ttl, ip, r.UserAgent())
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "创建会话失败")
		return
	}
	cookie := &http.Cookie{
		Name:     a.Cfg.Session.CookieName,
		Value:    raw,
		Path:     "/",
		HttpOnly: true,
		Secure:   a.cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(ttl.Seconds()),
	}
	// 审计写不进去 → 这次登录**不算数**：销毁刚签发的会话、撤掉 Cookie，
	// 再回 5xx。不能一边报"登录失败"一边留着一个能用的会话。
	if err := a.auditErr(r, res.User.Username, "auth.login.success", "", "", ip, true,
		map[string]any{"auto_provisioned": res.AutoProvisioned}); err != nil {
		if derr := a.AuthSvc.DestroySession(r.Context(), raw); derr != nil {
			a.Logger.Error("审计写失败后销毁会话失败", "user", res.User.Username, "err", derr)
		}
		clear := *cookie
		clear.Value = ""
		clear.MaxAge = -1
		http.SetCookie(w, &clear)
		Fail(w, http.StatusInternalServerError, CodeInternal, "写审计日志失败，登录未完成")
		return
	}
	http.SetCookie(w, cookie)
	OK(w, map[string]any{"user": publicUser(res.User)})
}

// handleLogout 注销（真吊销：删库里的会话行 + 清 Cookie）。
//
// ⚠️ **销毁会话失败必须让调用方知道**：
//
//	删不掉会话行 = 那一把会话**仍然有效**（Cookie 清了、库里还在）。
//	只记日志就回 200 是静默失败——用户以为登出了，实际没有，
//	被截获的 Cookie 值在服务端照样能用，调用方却无从得知
//	（AGENTS.md 规则 3）。
//
//	语义与登录路径一致：登录时审计写失败 → 销毁会话 + 清 Cookie + 回 5xx。
func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	var destroyErr error
	if ck, err := r.Cookie(a.Cfg.Session.CookieName); err == nil {
		destroyErr = a.AuthSvc.DestroySession(r.Context(), ck.Value)
	}
	// 无论成败都清 Cookie：浏览器这一侧不该继续带着它。
	http.SetCookie(w, &http.Cookie{
		Name:     a.Cfg.Session.CookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   a.cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	if destroyErr != nil {
		a.Logger.Error("注销时销毁会话失败，会话仍然有效", "err", destroyErr)
		Fail(w, http.StatusInternalServerError, CodeInternal,
			"注销未完成：服务端会话销毁失败，该会话仍然有效，请重试或联系管理员")
		return
	}
	OK(w, map[string]any{})
}

// handleChangePassword 修改当前用户的口令。
//
// 语义与 lf-db 同族产品一致：
//   - 必须校验旧口令（防止拿到会话就改死账号）
//   - 只支持本地账号；LDAP 账号的口令在目录里
//   - 改密后吊销该用户的**其他**会话，当前会话保留
func (a *App) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p.IsAnonymous() {
		Fail(w, http.StatusUnauthorized, CodeUnauthorized, "请先登录")
		return
	}
	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
		// Username 是**加密模式**下请求体里带的用户名。
		//
		// HKDF 的 info 绑定了它，服务端要先有用户名才能派生密钥；
		// 它必须与当前登录用户一致（不一致直接拒）。
		Username string `json:"username"`
		// 应用层加密信封（password.mode != plaintext 时使用）。
		// 加密模式下：旧口令与新口令都在信封里（old_password + password）。
		Kid   string `json:"kid"`
		Epk   string `json:"epk"`
		Nonce string `json:"nonce"`
		Ct    string `json:"ct"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Username != "" && req.Username != p.User.Username {
		Fail(w, http.StatusBadRequest, CodeInvalidParam, "用户名与加密数据不一致")
		return
	}
	// 口令来源（明文 / 密文）：改密要**两个**口令（旧的 + 新的）。
	secret, err := a.resolvePassword(r.Context(),
		passwordEnvelope{Kid: req.Kid, Epk: req.Epk, Nonce: req.Nonce, Ct: req.Ct},
		p.User.Username, req.NewPassword, req.OldPassword, true)
	if err != nil {
		a.failPassword(w, err)
		return
	}
	if secret.OldPassword == "" {
		Fail(w, http.StatusBadRequest, CodeInvalidParam, "请输入当前密码")
		return
	}
	if len(secret.Password) < 12 {
		Fail(w, http.StatusBadRequest, CodeInvalidParam, "新密码至少 12 位")
		return
	}
	if secret.Password == secret.OldPassword {
		Fail(w, http.StatusBadRequest, CodeInvalidParam, "新密码不能与当前密码相同")
		return
	}
	if err := a.AuthSvc.ChangeLocalPassword(r.Context(), p.User.ID, secret.OldPassword, secret.Password); err != nil {
		switch {
		case errors.Is(err, service.ErrPasswordNotLocal):
			Fail(w, http.StatusBadRequest, CodeInvalidParam, "LDAP 账号的密码由目录维护，无法在此修改")
			return
		case errors.Is(err, service.ErrOldPasswordMismatch):
			Fail(w, http.StatusUnauthorized, CodeUnauthorized, "当前密码不正确")
			return
		}
		Fail(w, http.StatusInternalServerError, CodeInternal, "修改密码失败")
		return
	}

	// 吊销别处登录的会话——口令变了，旧口令登进来的会话不该继续有效。
	if ck, err := r.Cookie(a.Cfg.Session.CookieName); err == nil {
		if _, derr := a.DB.DeleteOtherSessions(r.Context(), p.User.ID, store.HashSessionID(ck.Value)); derr != nil {
			// 口令已经改了（那才是主路径），这里的吊销失败必须留痕——
			// 漏掉的旧会话还能继续用，静默丢弃等于没做。
			a.Logger.Error("改密后吊销旧会话失败", "user", p.User.Username, "err", derr)
		}
	}
	if !a.mustAudit(w, r, p.User.Username, "auth.password.changed", "", "", clientIP(r), true, nil) {
		return
	}
	OK(w, map[string]any{})
}

// handleMe 返回当前主体。
//
// 未登录返回 **200 + {anonymous:true}**（不是 401）——
// 前端据此决定是否显示登录入口。
func (a *App) handleMe(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p.IsAnonymous() {
		OK(w, map[string]any{"anonymous": true})
		return
	}
	OK(w, map[string]any{"anonymous": false, "user": publicUser(p.User)})
}

// 个人访问令牌（PAT）

// handleListPersonalTokens 列出当前用户的个人令牌。
//
// 列表里**没有令牌本体**——只有前缀、创建时间、过期时间、最近使用时间。
//
// default_ttl_seconds 是系统默认有效期，供前端把"按系统默认"显示成具体天数，
// 并给"指定失效时间"预填一个合理的值。
func (a *App) handleListPersonalTokens(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	toks, err := a.Pats.List(r.Context(), p.User.ID)
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "读取个人令牌失败")
		return
	}
	OK(w, map[string]any{
		"tokens":              toks,
		"default_ttl_seconds": int64(a.Pats.TTL() / time.Second),
	})
}

// personalTokenExpiryRequest 是"设置失效时间"的请求体。
//
// 三种写法（见 service.ExpiryPolicy）：
//
//	{"never_expires": true}                → 永久有效
//	{"expires_at": 1799000000}             → 该时刻失效（unix 秒）
//	{}                                     → 重置为系统默认（core.pat_ttl_seconds）
//
// 两个都给 → 矛盾，400。
type personalTokenExpiryRequest struct {
	NeverExpires bool   `json:"never_expires"`
	ExpiresAt    *int64 `json:"expires_at"`
}

// expiryPolicy 把请求体翻译成 service 层的失效策略。
func (req personalTokenExpiryRequest) expiryPolicy() service.ExpiryPolicy {
	return service.ExpiryPolicy{NeverExpires: req.NeverExpires, ExpiresAt: req.ExpiresAt}
}

// handleCreatePersonalToken 生成一枚个人令牌。
//
// ⚠️ **原始令牌只在这里返回一次。**
//
// 之后既取不回来，也没法从库里还原（库里只有 SHA-256 散列）。
// 前端必须让用户当场复制走——错过就只能重新生成。
func (a *App) handleCreatePersonalToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		personalTokenExpiryRequest
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	p := principalFrom(r.Context())
	raw, tok, err := a.Pats.Create(r.Context(), p.User.ID, req.Name, req.expiryPolicy())
	if err != nil {
		Fail(w, http.StatusBadRequest, CodeInvalidParam, err.Error())
		return
	}
	// 审计写失败 → 这枚令牌**作废**。
	//
	// ⚠️ 为什么必须吊销：原始令牌只在这一刻可见，之后既取不回来、
	// 也无法从库里还原（只有 SHA-256 散列）。这里一报错，
	// 用户就永远拿不到这枚令牌——库里留着的是一条
	// "活跃但谁都用不了"的凭据行，纯孤儿。
	// 吊销掉，用户重试一次就干净了。
	if err := a.auditErr(r, p.User.Username, "personal_token.created",
		tok.TokenPrefix, "", clientIP(r), true,
		map[string]any{"never_expires": tok.ExpiresAt == nil}); err != nil {
		if rerr := a.Pats.Revoke(r.Context(), tok.ID, p.User.ID); rerr != nil {
			a.Logger.Error("审计写失败后吊销个人令牌失败", "user", p.User.Username, "err", rerr)
		}
		Fail(w, http.StatusInternalServerError, CodeInternal, "写审计日志失败，令牌未创建")
		return
	}
	OK(w, map[string]any{
		"token":        raw,
		"id":           tok.ID,
		"name":         tok.Name,
		"token_prefix": tok.TokenPrefix,
		"created_at":   tok.CreatedAt,
		"expires_at":   tok.ExpiresAt,
	})
}

// handlePatchPersonalToken 改一枚个人令牌的失效时间（限本人）。
//
// 令牌本体不变（散列不动）——CI 里已经配好的凭据不用重配。
// 永久 ↔ 定时可以来回改。
func (a *App) handlePatchPersonalToken(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		Fail(w, http.StatusBadRequest, CodeInvalidParam, "令牌 ID 不合法")
		return
	}
	var req personalTokenExpiryRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	p := principalFrom(r.Context())
	if err := a.Pats.SetExpiry(r.Context(), id, p.User.ID, req.expiryPolicy()); err != nil {
		if errors.Is(err, service.ErrInvalidPersonalToken) {
			Fail(w, http.StatusNotFound, CodeNotFound, "令牌不存在")
			return
		}
		// 矛盾请求 / 失效时间不合法 → 参数错误。
		Fail(w, http.StatusBadRequest, CodeInvalidParam, err.Error())
		return
	}
	if !a.mustAudit(w, r, p.User.Username, "personal_token.expiry_updated",
		strconv.FormatInt(id, 10), "", clientIP(r), true, nil) {
		return
	}
	OK(w, map[string]any{})
}

// handleDeletePersonalToken 吊销一枚个人令牌。
//
// 只能吊销**自己**的（service 层按 user_id 限定）。
func (a *App) handleDeletePersonalToken(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		Fail(w, http.StatusBadRequest, CodeInvalidParam, "令牌 ID 不合法")
		return
	}
	p := principalFrom(r.Context())
	if err := a.Pats.Revoke(r.Context(), id, p.User.ID); err != nil {
		if errors.Is(err, service.ErrInvalidPersonalToken) {
			Fail(w, http.StatusNotFound, CodeNotFound, "令牌不存在")
			return
		}
		Fail(w, http.StatusInternalServerError, CodeInternal, "吊销令牌失败")
		return
	}
	if !a.mustAudit(w, r, p.User.Username, "personal_token.revoked",
		strconv.FormatInt(id, 10), "", clientIP(r), true, nil) {
		return
	}
	OK(w, map[string]any{})
}

// publicUser 是不含口令散列的用户视图——散列绝不能出现在响应里。
func publicUser(u *domain.User) map[string]any {
	return map[string]any{
		"id":              u.ID,
		"username":        u.Username,
		"display_name":    u.DisplayName,
		"is_system_admin": u.IsSystemAdmin,
		"source":          u.Source,
		"disabled":        u.Disabled,
	}
}

// classifyAuthError 把内部错误归类，用于审计（**不返回给客户端**）。
func classifyAuthError(err error) string {
	switch {
	case errors.Is(err, service.ErrDisabled):
		return "disabled"
	case errors.Is(err, service.ErrLoginLocked):
		// ⚠️ 被锁定时对客户端仍然是 401 + "用户名或密码错误"（防账号探测），
		// 只有审计里的 reason 不同。
		return "locked"
	case errors.Is(err, service.ErrInvalidCredentials):
		return "bad_credentials"
	case errors.Is(err, service.ErrAuthUnavailable):
		// 认证后端（LDAP）不可达：不是"口令错"，审计里要能分开看。
		return "auth_unavailable"
	default:
		return "error"
	}
}
