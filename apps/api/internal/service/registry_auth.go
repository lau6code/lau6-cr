package service

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/lau6code/lau6-cr/apps/api/internal/crypto"
	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
)

// authorize 校验请求主体对仓库是否有指定动作的权限。
//
// 返回值：
//   - claims：客户端令牌的**已验签**载荷（请求没带令牌时为 nil）；
//   - ok：是否放行。
//
// claims 要带回给转发层：转发给上游前用它换发一枚实例级令牌
// （客户端令牌是用户级 HS256，distribution 只认 JWKS 里的 EdDSA）。
//
// 优先级：Bearer token 的 access claim（权威）> 匿名（走权限引擎）。
func (s *RegistryService) authorize(w http.ResponseWriter, r *http.Request, repo string, action Action) (*crypto.RegistryClaims, bool) {
	if raw := bearerOf(r); raw != "" {
		// 个人令牌（PAT）直接作为 Bearer 令牌
		//
		// CI / 脚本可以把 PAT 直接放进 Authorization 头，
		// 不必先走 realm 换一枚 JWT。
		if crypto.IsPersonalToken(raw) {
			return s.authorizeWithPAT(w, r, repo, action, raw)
		}
		// 项目令牌（lau6_p_ 前缀）同样可以直接当 Bearer 令牌。
		if crypto.IsProjectToken(raw) {
			return s.authorizeWithProjectToken(w, r, repo, action, raw)
		}
		// ⚠️ 必须在 JWT 验签之前判定：旧前缀（cr_pat_）的令牌不是 JWT，
		//    走验签只能得到笼统的"令牌无效"——用户会以为令牌被吊销了，
		//    去查权限、翻审计，越查越找不到原因（02-auth.md §2.6.4）。
		if crypto.IsLegacyPersonalToken(raw) {
			s.denied(r, repo, action, "令牌前缀已变更（cr_pat_ → lau6_u_），请重新生成")
			s.challengeWithMessage(w, repo, action, "令牌前缀已变更（cr_pat_ → lau6_u_），请重新生成")
			return nil, false
		}
		claims, err := s.issuer.Verify(r.Context(), raw)
		if err != nil {
			// token 验签不过（过期 / 伪造 / 密钥轮换后失效）：
			// 回 401 **并带上挑战**——客户端据此重新换一枚 token。
			// 只回 401 不带挑战的话，它会拿着同一枚坏 token 反复重试。
			s.denied(r, repo, action, "token 校验失败: "+err.Error())
			s.challenge(w, repo, action)
			return nil, false
		}
		if !crypto.AllowsAccess(claims, repo, string(action)) {
			// token 有效，但没授予这个动作。
			//
			// ⚠️ 这里必须区分**匿名 token**与**已登录用户的 token**——
			//    两者要回**不同的状态码**，否则客户端会一直重试：
			//
			//	匿名（没登录）→ **401 + 挑战**："你需要认证"。
			//	    回 403 是错的信号：403 说的是"你被拒绝了"，
			//	    客户端据此认定"我是有身份只是没权限"，
			//	    于是拿着同一枚匿名 token 反复重试。
			//	    401 + 挑战才让它知道"去登录/换个有权限的 token"。
			//
			//	已登录但无权限 → **403 DENIED**：这是终态。
			//	    再给挑战会让客户端去换新 token 反复重试——
			//	    权限不会凭空多出来，挑战只会把"拒绝"变成"死循环"。
			if claims.Subject == anonymousSubject {
				s.denied(r, repo, action, "匿名 token 未授予该仓库的对应动作（未登录）")
				s.challenge(w, repo, action)
				return nil, false
			}
			s.denied(r, repo, action, "token 未授予该仓库的对应动作")
			s.writeRegistryError(w, http.StatusForbidden, "DENIED",
				"requested access to the resource is denied", nil)
			return nil, false
		}
		return claims, true
	}
	return nil, s.authorizeAnonymous(w, r, repo, action)
}

// authorizeWithPAT 用个人令牌（PAT）授权。
//
// PAT **不带 scope**：它是一把"身份钥匙"，能推拉哪些镜像由权限引擎
// 按用户的**当前**角色实时判定——角色改了，令牌的下一次使用立刻按新权限算，
// 不留"权限变更前的旧授权还在流通"的窗口。
func (s *RegistryService) authorizeWithPAT(w http.ResponseWriter, r *http.Request, repo string, action Action, raw string) (*crypto.RegistryClaims, bool) {
	if s.pats == nil {
		s.denied(r, repo, action, "未启用个人令牌")
		s.challenge(w, repo, action)
		return nil, false
	}
	user, _, err := s.pats.Verify(r.Context(), raw)
	if err != nil {
		s.denied(r, repo, action, "个人令牌无效: "+err.Error())
		s.challenge(w, repo, action)
		return nil, false
	}
	d, err := s.perm.Decide(r.Context(), Principal{User: user}, repo, []string{string(action)})
	if err != nil {
		s.logError("个人令牌鉴权失败", r, "err", err)
		s.writeRegistryError(w, http.StatusInternalServerError, "DENIED", "authorization check failed", nil)
		return nil, false
	}
	if !d.Allow(action) {
		s.denied(r, repo, action, "个人令牌所属用户对该仓库无所需权限")
		s.writeRegistryError(w, http.StatusForbidden, "DENIED",
			"requested access to the resource is denied", nil)
		return nil, false
	}
	// 载荷要给上游换发令牌用（上游只认 EdDSA，PAT 它验不了）。
	return crypto.NewRegistryClaims(user.Username, []domain.Access{{
		Type:    "repository",
		Name:    repo,
		Actions: d.Granted,
	}}), true
}

// authorizeWithProjectToken 用项目令牌授权。
//
// 项目令牌的权限**就是它自带的角色**：角色创建时选定、此后不变，
// 覆盖该项目内的全部仓库（含私有仓库）。
//
// 跨项目一律拒绝——项目令牌是项目身份，别的项目与它无关。
func (s *RegistryService) authorizeWithProjectToken(w http.ResponseWriter, r *http.Request, repo string, action Action, raw string) (*crypto.RegistryClaims, bool) {
	if s.projTokens == nil {
		s.denied(r, repo, action, "未启用项目令牌")
		s.challenge(w, repo, action)
		return nil, false
	}
	pt, err := s.projTokens.Verify(r.Context(), raw)
	if err != nil {
		s.denied(r, repo, action, "项目令牌无效: "+err.Error())
		s.challenge(w, repo, action)
		return nil, false
	}
	d, err := s.perm.Decide(r.Context(), Principal{Project: pt}, repo, []string{string(action)})
	if err != nil {
		s.logError("项目令牌鉴权失败", r, "err", err)
		s.writeRegistryError(w, http.StatusInternalServerError, "DENIED", "authorization check failed", nil)
		return nil, false
	}
	if !d.Allow(action) {
		s.denied(r, repo, action, "项目令牌对该仓库无所需权限")
		s.writeRegistryError(w, http.StatusForbidden, "DENIED",
			"requested access to the resource is denied", nil)
		return nil, false
	}
	// 载荷要给上游换发令牌用（上游只认 EdDSA，项目令牌它验不了）。
	subject, err := s.projTokens.Subject(r.Context(), pt)
	if err != nil {
		s.logError("项目令牌主体解析失败", r, "err", err)
		s.writeRegistryError(w, http.StatusInternalServerError, "DENIED", "authorization check failed", nil)
		return nil, false
	}
	return crypto.NewRegistryClaims(subject, []domain.Access{{
		Type:    "repository",
		Name:    repo,
		Actions: d.Granted,
	}}), true
}

// authorizeAnonymous 校验匿名主体（公开镜像免登录拉取）。
func (s *RegistryService) authorizeAnonymous(w http.ResponseWriter, r *http.Request, repo string, action Action) bool {
	if repo == "" {
		// 无仓库上下文（目录列举等）：匿名放行，可见性由 catalog 自己的过滤负责。
		return true
	}
	d, err := s.perm.Decide(r.Context(), Principal{}, repo, []string{string(action)})
	if err != nil {
		s.logError("registry 权限判定失败", r, "repo", repo, "action", action, "err", err)
		s.writeRegistryError(w, http.StatusInternalServerError, "DENIED", "authorization check failed", nil)
		return false
	}
	if !d.Allow(action) {
		s.denied(r, repo, action, "匿名主体无该仓库权限")
		s.challenge(w, repo, action)
		return false
	}
	return true
}

// denied 记录一次鉴权拒绝。
//
// 鉴权失败**必须留痕**：它是"docker 报 unauthorized 但后端一片安静"
// 这类问题的唯一线索。不记日志就只能靠猜。
func (s *RegistryService) denied(r *http.Request, repo string, action Action, reason string) {
	s.logWarn("registry 请求被拒绝", r,
		"repo", repo, "action", action, "reason", reason)
}

// AuthScheme 返回 Authorization 头的**方案名**（Basic / Bearer / …）。
//
// 没有 Authorization 头时返回空串。
//
// ⚠️ 绝不返回凭据本身：那是口令/令牌，进了日志就是泄露。
//
//	但"这次请求带没带凭据"是排障的关键信息——
//	"客户端压根没带 Bearer"和"带了但没权限"是两回事。
func AuthScheme(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	if i := strings.IndexByte(h, ' '); i > 0 {
		return strings.ToLower(h[:i])
	}
	return "unknown"
}

// challenge 返回 Docker 标准的 401 挑战头。
func (s *RegistryService) challenge(w http.ResponseWriter, repo string, action Action) {
	realm := s.realm()
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(
		`Bearer realm="%s",service="%s",scope="%s"`,
		realm, registryServiceName, challengeScope(repo, action)))
	s.writeRegistryError(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required", nil)
}

// challengeWithMessage 返回带**明确原因**的 401 挑战。
//
// 与 challenge 只差响应体的 message：用于"凭据格式用错了"这类终态错误——
// 笼统的 "authentication required" 会让用户以为令牌被吊销了，
// 去查权限、翻审计，越查越找不到原因（02-auth.md §2.6.4）。
func (s *RegistryService) challengeWithMessage(w http.ResponseWriter, repo string, action Action, msg string) {
	realm := s.realm()
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(
		`Bearer realm="%s",service="%s",scope="%s"`,
		realm, registryServiceName, challengeScope(repo, action)))
	s.writeRegistryError(w, http.StatusUnauthorized, "UNAUTHORIZED", msg, nil)
}

// versionCheck 处理版本检查端点（GET /v2/）。
//
// ⚠️ **带有效 Bearer 时必须回 200**。
//
// docker login 的成功判据是："用换到的 token 再 ping 一次 `/v2/`，拿到 200"。
// 无脑回 401 会让登录**永远失败**——客户端报
// `login attempt to ... failed with status: 401 Unauthorized`。
//
// 三种情况：
//   - 无 Authorization 头 → 401 + 挑战（客户端据此去 realm 换 token）；
//   - 有 token 但验签不过 → 401 + 挑战（让客户端重新换）；
//   - 有 token 且验签通过 → **200**（版本检查通过）。
//
// 登录探活拿到的 token 是**空 access** 的（客户端请求时 scope 为空），
// 这里只看签名与 issuer/audience，不看 access——
// access 是"能不能读某个仓库"，与"这个端点认不认我"是两回事。
//
// 200 的响应体直接给 `{}`（Docker Registry V2 规范），
// 不转发上游：版本检查探的是**本端点**是否活着，
// 不该被上游的可用性连累。
func (s *RegistryService) versionCheck(w http.ResponseWriter, r *http.Request) {
	if raw := bearerOf(r); raw != "" {
		// 个人令牌（PAT）也可以直接用于版本检查——CI 脚本拿 PAT 探活。
		if crypto.IsPersonalToken(raw) {
			if s.pats == nil {
				s.logWarn("registry 版本检查：未启用个人令牌", r)
				s.challengeVersion(w, r)
				return
			}
			if _, _, err := s.pats.Verify(r.Context(), raw); err != nil {
				s.logWarn("registry 版本检查：个人令牌无效", r, "err", err)
				s.challengeVersion(w, r)
				return
			}
		} else if crypto.IsProjectToken(raw) {
			// 项目令牌同理——CI 脚本拿项目令牌探活。
			if s.projTokens == nil {
				s.logWarn("registry 版本检查：未启用项目令牌", r)
				s.challengeVersion(w, r)
				return
			}
			if _, err := s.projTokens.Verify(r.Context(), raw); err != nil {
				s.logWarn("registry 版本检查：项目令牌无效", r, "err", err)
				s.challengeVersion(w, r)
				return
			}
		} else if _, err := s.issuer.Verify(r.Context(), raw); err != nil {
			s.logWarn("registry 版本检查：token 验签失败", r, "err", err)
			s.challengeVersion(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
		return
	}
	s.challengeVersion(w, r)
}

// challengeVersion 返回版本检查端点的挑战。
//
// ⚠️ 必须回 **401**，不能回 200。理由见 Forward 里的注释。
//
// 挑战里**不带 scope**：版本检查没有仓库上下文。
func (s *RegistryService) challengeVersion(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(
		`Bearer realm="%s",service="%s"`, s.realm(), registryServiceName))
	// info 而不是 warn：这是**协议握手的一部分**，不是错误。
	// 每个 docker 命令（pull/push/login）都会 ping 一次，
	// 按 warn 记会把真正的告警淹掉。realm 拼错时看启动日志的 token_realm。
	if s.logger != nil {
		s.logger.Info("registry 版本检查返回挑战", "method", r.Method, "path", r.URL.Path,
			"ip", clientIPOf(r), "realm", s.realm())
	}
	s.writeRegistryError(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required", nil)
}

func (s *RegistryService) realm() string {
	if s.realmFn == nil {
		return ""
	}
	return s.realmFn()
}

func bearerOf(r *http.Request) string {
	raw, err := crypto.ParseBearer(r.Header.Get("Authorization"))
	if err != nil {
		return ""
	}
	return raw
}

// clientIPOf 取客户端 IP。
//
// 不解析 X-Forwarded-For —— 那个头由客户端可控，
// 除非明确配置了可信反代，否则它不可信。
func clientIPOf(r *http.Request) string {
	ip := r.RemoteAddr
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	if len(ip) > 45 {
		ip = ip[:45]
	}
	return ip
}

// statsClientIP 取流量统计用的客户端地址。
//
// 优先用 context 里的那个——server 层已经按**可信反代**解析过
// X-Forwarded-For（限流用的是同一个值，见 server.clientIPForLimit）。
// 反代后面所有客户端的 RemoteAddr 都是反代自己，
// 不解析 XFF 的话整个公网会被算成同一个网段。
//
// context 里没有（单测直接调 handler）就退回 TCP 对端地址。
func statsClientIP(r *http.Request) string {
	if ip := ClientIPFromContext(r.Context()); ip != "" {
		return ip
	}
	return clientIPOf(r)
}

// isAnonymousClaims 判断这次请求是不是匿名主体。
//
// 两种匿名：完全没带令牌（claims 为 nil），
// 或者带的是匿名令牌（subject 是 anonymous）。
//
// 带真实身份的请求不记 IP——已经有身份了，再收 IP 是白收集个人数据。
func isAnonymousClaims(c *crypto.RegistryClaims) bool {
	return c == nil || c.Subject == anonymousSubject
}

// tokenSubjectHint 尽力解出令牌载荷里的 subject（**未验签**）。
//
// ⚠️ 只用于日志定位，**绝不作为授权依据**：载荷是客户端可控的，
// 验签没过恰恰说明它不可信。它的唯一用途是回答
// "这枚坏令牌是谁的"——令牌过期、密钥轮换、账号被删
// 在外部表现上都是"验签失败"，没有这行日志就只能靠猜。
//
// 解不出来（不是 JWT 形态 / 载荷不是合法 JSON）时返回空串——
// 空串表示"没有线索"，不是错误。
func tokenSubjectHint(raw string) string {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var p struct {
		Subject string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return ""
	}
	// 载荷是客户端可控的：限长，避免日志被撑爆。
	if len(p.Subject) > 64 {
		return p.Subject[:64]
	}
	return p.Subject
}
