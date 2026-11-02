package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
	"github.com/lau6code/lau6-cr/apps/api/internal/service"
)

// 它是 registry 鉴权链路的**唯一入口**：客户端拿到 401 挑战后
// 来这里换 token。所以这里的每一次失败都要留痕——
// "docker 报 unauthorized 但后端没日志"多半就出在这一步。
//
// ⚠️ **响应体不套业务信封**。
//
// Docker 客户端按 OCI 规范解析 token 响应：顶层找 `token`（或 `access_token`），
// 找不到就报 "authorization server did not include a token in the response"。
// 我们要是图省事用 OK() 包一层（{"code":0,"data":{...}}），
// 客户端在**顶层**看不到 token —— 登录与 push 全部失败，
// 而且失败信息指向"没有 token"，极具误导性。
// 规范见 `02-auth.md` §5.3。
func (a *App) handleToken(w http.ResponseWriter, r *http.Request) {
	scopes := r.URL.Query()["scope"]
	basic := r.Header.Get("Authorization")
	if basic != "" && !strings.HasPrefix(strings.ToLower(basic), "basic ") {
		// 非 Basic 的 Authorization 头不是有效的 CLI 凭据。
		basic = ""
	}

	// service 参数校验：token 是"给某个 service 用的"，
	// 与本实例的 audience 对不上就拒——否则一枚在本 registry 签发的 token
	// 能被挪到别的 service 上用。
	svc := r.URL.Query().Get("service")
	if !tokenServiceAllowed(svc, a.Cfg.Token.Audience, publicHostOf(a.Cfg.Core.PublicURL)) {
		a.Logger.Warn("registry token 签发被拒：service 不匹配",
			"ip", clientIP(r), "service", svc,
			"expect", a.Cfg.Token.Audience, "scopes", scopes)
		// 凭据无效时带 Basic 挑战：客户端据此知道该用 Basic 重试。
		w.Header().Set("WWW-Authenticate", `Basic realm="lau6-cr"`)
		writeRegistryError(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}

	// 匿名限流
	//
	// `/token` 是**唯一无认证可达的入口**（02-auth.md §5.4）：
	// 匿名也能换到一枚"只含 pull"的合法令牌。没有限流，
	// 它就是一个不限速的公开入口。
	//
	// 只限**匿名**（没带 Basic 凭据）的请求：带凭据的请求有身份，
	// 可追溯，且账号侧还有登录退避兜着。
	ip := a.clientIPForLimit(r)
	if basic == "" {
		allowed, err := a.RateLimiter.Allow(r.Context(), service.RateLimitScopeToken, ip)
		if err != nil {
			// 限流器坏了 → **不能当"没超限"放行**：
			// 那等于限流形同虚设，而且没人知道限流器已经坏了。
			a.Logger.Error("限流检查失败", "scope", "token", "ip", ip, "err", err)
			writeRegistryError(w, http.StatusInternalServerError, "DENIED", "rate limit check failed")
			return
		}
		if !allowed {
			a.Logger.Warn("匿名 token 签发被限流", "ip", ip)
			w.Header().Set("Retry-After", "60")
			writeRegistryError(w, http.StatusTooManyRequests, "DENIED", "too many requests")
			return
		}
	}

	res, err := a.TokenSvc.Issue(r.Context(), basic, scopes)
	if err != nil {
		// 项目不存在：直说"项目 xxx 不存在"。
		//
		// 为什么不在这里吞掉、只签空 access：
		// 客户端拿到空 access 后会在 /v2/ 上**反复重试**（每次 403），
		// 运维看到的是一串 403 日志，完全看不出真正原因。
		// 在换 token 这一步就拒掉，客户端立刻带着这句话停下。
		var pnf *service.ProjectNotFoundError
		if errors.As(err, &pnf) {
			a.Logger.Warn("registry token 签发被拒：项目不存在",
				"ip", clientIP(r), "project", pnf.Project, "scopes", scopes)
			writeRegistryError(w, http.StatusForbidden, "DENIED", pnf.Error())
			return
		}
		// 匿名主体申请写权限（push / delete）：直接 401 + Basic 挑战。
		//
		// 为什么不在这里签一枚"只有 pull"的降级 token：
		// 客户端会拿着它继续推 → registry 回 401 + 挑战 →
		// 客户端按挑战再来换 token → **死循环**（日志里一串同一条拒绝）。
		// 在换 token 这一步就拒，客户端立刻带着"未登录"停下。
		var aws *service.AnonymousWriteScopeError
		if errors.As(err, &aws) {
			a.Logger.Warn("registry token 签发被拒：匿名主体申请写权限",
				"ip", clientIP(r), "scope", aws.Scope, "action", aws.Action,
				"reason", "匿名不能 push/delete，需要登录")
			w.Header().Set("WWW-Authenticate", `Basic realm="lau6-cr"`)
			writeRegistryError(w, http.StatusUnauthorized, "UNAUTHORIZED", aws.Error())
			return
		}
		// 旧前缀（cr_pat_）：**明确说清为什么**。
		//
		// ⚠️ 必须与"凭据无效"分开：笼统的 "authentication required" 会让用户
		//    以为令牌被吊销了，去查权限、翻审计，越查越找不到原因
		//    （02-auth.md §2.6.4）。
		if errors.Is(err, service.ErrLegacyTokenPrefix) {
			a.Logger.Warn("registry token 签发被拒：令牌前缀已变更",
				"ip", clientIP(r), "service", svc, "scopes", scopes, "err", err)
			w.Header().Set("WWW-Authenticate", `Basic realm="lau6-cr"`)
			writeRegistryError(w, http.StatusUnauthorized, "UNAUTHORIZED", err.Error())
			return
		}
		// 凭据无效 / 账号被禁用 / 个人令牌无效 / 项目令牌无效 → 一律 **401 + Basic 挑战**。
		//
		// ⚠️ 这四种都是**终态**：客户端再重试一万次也不会变好。
		//    给 500 会让客户端当成"服务端坏了"而反复重试，
		//    拿 401 + 挑战它才知道"重新登录/换一枚凭据"。
		if errors.Is(err, service.ErrInvalidCredentials) ||
			errors.Is(err, service.ErrDisabled) ||
			errors.Is(err, service.ErrInvalidPersonalToken) ||
			errors.Is(err, service.ErrInvalidProjectToken) {
			a.Logger.Warn("registry token 签发被拒：凭据无效",
				"ip", clientIP(r), "service", svc, "scopes", scopes,
				"auth", service.AuthScheme(r), "err", err)
			w.Header().Set("WWW-Authenticate", `Basic realm="lau6-cr"`)
			writeRegistryError(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
			return
		}
		a.Logger.Error("registry token 签发失败",
			"ip", clientIP(r), "service", svc, "scopes", scopes, "err", err)
		// 内部错误也用 registry 信封：客户端解析的是 registry 协议，
		// 业务信封它看不懂（而且会把"内部错误"显示成一段无意义的 JSON）。
		writeRegistryError(w, http.StatusInternalServerError, "DENIED", "cannot issue token")
		return
	}
	a.Logger.Info("已签发 registry token",
		"subject", res.Subject, "service", svc, "scopes", scopes,
		"granted", grantedNames(res.Granted), "ip", clientIP(r))

	// ⚠️ 顶层就是 token 字段——**不套业务信封**（原因见函数注释）。
	writeTokenResponse(w, res.Token, res.ExpiresIn)
}

// ⚠️ **不套业务信封**：客户端在**顶层**找 `token` / `access_token`，
// 套一层 {"code":0,"data":{...}} 它就找不到，
// 报 "authorization server did not include a token in the response"。
func writeTokenResponse(w http.ResponseWriter, token string, expiresIn int) {
	// ⚠️ 先序列化再写状态码：编码失败时还能回 500。
	//    先 WriteHeader 的话状态码就锁死了，客户端只能拿到半截 body。
	buf, err := json.Marshal(map[string]any{
		"token":        token,
		"access_token": token,
		"expires_in":   expiresIn,
		"issued_at":    time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		slog.Error("序列化令牌响应失败", "err", err)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(buf); err != nil {
		slog.Warn("写出令牌响应体失败", "err", err)
	}
}

// 写 Docker Registry 错误信封。
//
// 形如 {"errors":[{"code":"UNAUTHORIZED","message":"..."}]}。
//
// ⚠️ registry 协议的端点**不用**业务信封（{"code":..,"message":..,"data":..}）：
// 那是给前端业务 API 用的，Docker 客户端看不懂。
//
// ⚠️ 先序列化再写状态码：编码失败时还能回一个能解析的 500 信封，
// 而不是把状态码锁死在一个截断的 body 上。
func writeRegistryError(w http.ResponseWriter, status int, code, msg string) {
	buf, err := json.Marshal(map[string]any{
		"errors": []map[string]any{{"code": code, "message": msg}},
	})
	if err != nil {
		slog.Error("序列化 registry 错误信封失败", "status", status, "code", code, "err", err)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	w.WriteHeader(status)
	if _, err := w.Write(buf); err != nil {
		slog.Warn("写出 registry 错误信封失败", "status", status, "code", code, "err", err)
	}
}

// 把授予的访问列表压成 "name:actions" 的字符串列表，
// 便于在日志里一眼看清"这枚 token 到底能干什么"。
func grantedNames(access []domain.Access) []string {
	out := make([]string, 0, len(access))
	for _, a := range access {
		out = append(out, a.Name+":"+strings.Join(a.Actions, ","))
	}
	return out
}

// 判断 token 请求里的 service 参数是否可接受。
//
// token 是"给某个 service 用的"：与本实例的 audience 对不上就拒——
// 否则一枚在本 registry 签发的 token 能被挪到别的 service 上用。
//
// 放行两种写法：
//   - 与 audience 一致（我们在挑战里宣告的就是它）；
//   - 等于 registry 自己的对外 host（OCI 规范允许 service 用实现自定义的名字，
//     不少客户端就填 host:port——那仍然是"本 registry 自己"，不是别的 service）。
//
// ⚠️ 缺省（不带 service 参数）是允许的：service 在规范里是可选参数，
//
//	省略不等于"服务名不匹配"。
func tokenServiceAllowed(requested, audience, publicHost string) bool {
	if requested == "" || requested == audience {
		return true
	}
	return publicHost != "" && requested == publicHost
}

// 取对外地址的 host[:port]。
func publicHostOf(publicURL string) string {
	u, err := url.Parse(publicURL)
	if err != nil {
		return ""
	}
	return u.Host
}
