package server

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"golang.org/x/crypto/bcrypt"

	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

func (a *App) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := a.DB.ListUsers(r.Context())
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "读取用户列表失败")
		return
	}
	if users == nil {
		users = []domain.User{}
	}
	OK(w, map[string]any{"users": users})
}

func (a *App) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username      string `json:"username"`
		Password      string `json:"password"`
		DisplayName   string `json:"display_name"`
		IsSystemAdmin bool   `json:"is_system_admin"`
		// 应用层加密信封（password.mode != plaintext 时使用）。
		// 加密模式下初始口令在信封里，username 仍然明文带着
		//（HKDF 的 info 绑定了它）。
		Kid   string `json:"kid"`
		Epk   string `json:"epk"`
		Nonce string `json:"nonce"`
		Ct    string `json:"ct"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Username == "" {
		Fail(w, http.StatusBadRequest, CodeInvalidParam, "用户名不能为空，密码至少 8 位")
		return
	}
	// 口令来源（明文 / 密文）：建用户的初始口令也要过模式闸门。
	secret, err := a.resolvePassword(r.Context(),
		passwordEnvelope{Kid: req.Kid, Epk: req.Epk, Nonce: req.Nonce, Ct: req.Ct},
		req.Username, req.Password, "", false)
	if err != nil {
		a.failPassword(w, err)
		return
	}
	if len(secret.Password) < 8 {
		Fail(w, http.StatusBadRequest, CodeInvalidParam, "用户名不能为空，密码至少 8 位")
		return
	}
	hash, err := a.hashPassword(secret.Password)
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "散列口令失败")
		return
	}
	user, err := a.DB.CreateUser(r.Context(), req.Username, hash, "local", req.DisplayName, req.IsSystemAdmin)
	if err == store.ErrDuplicate {
		Fail(w, http.StatusConflict, CodeConflict, "用户名已存在")
		return
	}
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "创建用户失败")
		return
	}
	if !a.mustAudit(w, r, actorName(r.Context()), "user.created", user.Username, "", clientIP(r), true, nil) {
		return
	}
	OK(w, map[string]any{"user": publicUser(user)})
}

func (a *App) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		Fail(w, http.StatusBadRequest, CodeInvalidParam, "用户 ID 不合法")
		return
	}
	var req struct {
		DisplayName   *string `json:"display_name"`
		IsSystemAdmin *bool   `json:"is_system_admin"`
		Disabled      *bool   `json:"disabled"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if err := a.DB.UpdateUser(r.Context(), id, req.DisplayName, req.IsSystemAdmin, req.Disabled); err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "更新用户失败")
		return
	}
	if !a.mustAudit(w, r, actorName(r.Context()), "user.updated", strconv.FormatInt(id, 10), "", clientIP(r), true, nil) {
		return
	}
	user, err := a.DB.UserByID(r.Context(), id)
	if err != nil {
		// 用户已经改完了，但回读失败：不能说"更新失败"，
		// 也不能拿 nil 当用户对象返回。
		a.Logger.Error("回读用户失败", "user", id, "err", err)
		Fail(w, http.StatusInternalServerError, CodeInternal, "更新已完成，但读取用户信息失败，请刷新确认")
		return
	}
	OK(w, map[string]any{"user": publicUser(user)})
}

// handleAdminResetPassword 让系统管理员替某个用户重置口令。
//
// 与自助改密（`POST /api/v1/auth/password`）的区别：
//   - 不验旧口令——用户忘了口令，管理员没有旧口令可验；
//   - 改的是**别人**的口令。
//
// 两条边界（`02-auth.md` §1.2）：
//   - **只支持本地账号**：LDAP 的口令在目录里，改本地散列会造出
//     一个与目录不一致的影子账号，登录时认证源错配；
//   - **只支持非系统管理员**：管理员能重置管理员的口令 =
//     能登录别人的管理员账号（横向接管），账号本人毫无察觉。
//
// ⚠️ **不吊销该用户的会话与令牌**：已登录的会话与 PAT 保持有效。
//
//	重置只改口令散列。若重置的原因是"疑似凭据泄露"，
//	仅改口令不足以止损，需要另行吊销（`POST /users/{id}/revoke-tokens`）。
//
// 口令的传输方式（明文 / 应用层加密）走 `password.mode` 闸门，
// 与其它四条入口同构（`13-password-transport.md` §4.4）。
//
// ⚠️ 加密模式下用**目标用户的用户名**参与 HKDF 派生：
//
//	用户名从 URL 里的 id 查库得到，请求体不必带 username——
//	也就没有"传错用户名"这一面。
func (a *App) handleAdminResetPassword(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		Fail(w, http.StatusBadRequest, CodeInvalidParam, "用户 ID 不合法")
		return
	}
	user, err := a.DB.UserByID(r.Context(), id)
	if err == store.ErrNotFound || user == nil {
		Fail(w, http.StatusNotFound, CodeNotFound, "用户不存在")
		return
	}
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "读取用户失败")
		return
	}
	if user.Source != "local" {
		Fail(w, http.StatusBadRequest, CodeInvalidParam, "LDAP 账号的密码由目录维护，无法在此修改")
		return
	}
	if user.IsSystemAdmin {
		Fail(w, http.StatusForbidden, CodeForbidden, "不能重置系统管理员的密码")
		return
	}
	var req struct {
		NewPassword string `json:"new_password"`
		// 应用层加密信封（password.mode != plaintext 时使用）。
		Kid   string `json:"kid"`
		Epk   string `json:"epk"`
		Nonce string `json:"nonce"`
		Ct    string `json:"ct"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	// 口令来源（明文 / 密文）：重置口令也要过模式闸门。
	secret, err := a.resolvePassword(r.Context(),
		passwordEnvelope{Kid: req.Kid, Epk: req.Epk, Nonce: req.Nonce, Ct: req.Ct},
		user.Username, req.NewPassword, "", false)
	if err != nil {
		a.failPassword(w, err)
		return
	}
	// 强度下限与自助改密一致（≥12 位）。
	if len(secret.Password) < 12 {
		Fail(w, http.StatusBadRequest, CodeInvalidParam, "新密码至少 12 位")
		return
	}
	hash, err := a.hashPassword(secret.Password)
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "散列口令失败")
		return
	}
	if err := a.DB.UpdatePassword(r.Context(), user.ID, hash); err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "重置密码失败")
		return
	}
	if !a.mustAudit(w, r, actorName(r.Context()), "user.password.reset", user.Username, "", clientIP(r), true, nil) {
		return
	}
	OK(w, map[string]any{"username": user.Username})
}

func (a *App) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		Fail(w, http.StatusBadRequest, CodeInvalidParam, "用户 ID 不合法")
		return
	}
	if err := a.DB.DeleteUser(r.Context(), id); err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "删除用户失败")
		return
	}
	if !a.mustAudit(w, r, actorName(r.Context()), "user.deleted", strconv.FormatInt(id, 10), "", clientIP(r), true, nil) {
		return
	}
	OK(w, map[string]any{})
}

// 删掉该用户的签名密钥 → 他名下所有未过期令牌**立刻**作废
// （验签时找不到密钥），下次换令牌会生成一把新的。
//
// 同时吊销他的**个人令牌（PAT）**——PAT 是另一把长期凭据，
// 只作废签名密钥的话，CI 里那把 PAT 还能继续推拉，
// "吊销全部令牌"就成了名不副实的半吊子。
//
// 这是"每用户一把密钥"换来的核心能力：
// 实例密钥只有一把时，想让某个人的凭据失效只能轮换实例密钥——
// 那会把**所有人**的令牌一起作废。
//
// 权限：系统管理员可以吊销任何人；用户可以吊销自己。
func (a *App) handleRevokeUserTokens(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		Fail(w, http.StatusBadRequest, CodeInvalidParam, "用户 ID 不合法")
		return
	}
	user, err := a.DB.UserByID(r.Context(), id)
	if err == store.ErrNotFound || user == nil {
		Fail(w, http.StatusNotFound, CodeNotFound, "用户不存在")
		return
	}
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "读取用户失败")
		return
	}
	if !a.isSystemAdmin(r) {
		p := principalFrom(r.Context())
		if p.IsAnonymous() || p.User.ID != user.ID {
			Fail(w, http.StatusForbidden, CodeForbidden, "无权吊销该用户的令牌")
			return
		}
	}
	if a.UserSecrets == nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "用户密钥库不可用")
		return
	}
	if err := a.UserSecrets.Revoke(r.Context(), user.Username); err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "吊销令牌失败")
		return
	}
	// PAT 一并吊销——它是同一类"长期凭据"，留着就等于吊销没做完。
	revokedPATs := int64(0)
	if a.Pats != nil {
		n, err := a.Pats.RevokeAll(r.Context(), user.ID)
		if err != nil {
			Fail(w, http.StatusInternalServerError, CodeInternal, "吊销个人令牌失败")
			return
		}
		revokedPATs = n
	}
	if !a.mustAudit(w, r, actorName(r.Context()), "user.tokens.revoked",
		user.Username, "", clientIP(r), true,
		map[string]any{"personal_tokens": revokedPATs}) {
		return
	}
	OK(w, map[string]any{"username": user.Username, "personal_tokens": revokedPATs})
}

func (a *App) hashPassword(pw string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
