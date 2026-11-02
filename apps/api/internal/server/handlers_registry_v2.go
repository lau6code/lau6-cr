package server

import (
	"net/http"

	"github.com/lau6code/lau6-cr/apps/api/internal/service"
)

// 把 /v2/* 转发到 distribution。
func (a *App) handleProxyRegistry(w http.ResponseWriter, r *http.Request) {
	// 匿名读限流：**没有 Authorization 头**的读请求就是匿名读。
	//
	// 带 Bearer 令牌的不在这里限——那枚匿名令牌是拿 /token 换来的，
	// 它在那边已经被限过（access.anonymous_token_rpm）。
	// 两处各限各的，额度不互相影响。
	if (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
		r.Header.Get("Authorization") == "" {
		if !a.allowAnonymousRead(w, r, true) {
			return
		}
	}
	a.RegistrySvc.Forward(w, r)
}

// 匿名读路径的限流。
//
// 返回 false 表示已经写过响应，调用方必须立刻 return。
//
// ⚠️ 只挡**匿名**主体：已登录的访问有身份、可追溯，
// 不该被匿名桶一起限掉。
//
// registryError=true 时用 registry 的错误格式（/v2/* 走 docker 协议），
// 否则用业务信封（/api/v1/registry/* 是 UI 的接口）。
func (a *App) allowAnonymousRead(w http.ResponseWriter, r *http.Request, registryError bool) bool {
	if !principalFrom(r.Context()).IsAnonymous() {
		return true
	}
	ip := a.clientIPForLimit(r)
	allowed, err := a.RateLimiter.Allow(r.Context(), service.RateLimitScopeRead, ip)
	if err != nil {
		// 限流器坏了 → **不能当"没超限"放行**（那等于限流形同虚设）。
		a.Logger.Error("限流检查失败", "scope", "read", "ip", ip, "err", err)
		if registryError {
			writeRegistryError(w, http.StatusInternalServerError, "DENIED", "rate limit check failed")
		} else {
			Fail(w, http.StatusInternalServerError, CodeInternal, "限流检查失败")
		}
		return false
	}
	if !allowed {
		a.Logger.Warn("匿名读被限流", "ip", ip)
		w.Header().Set("Retry-After", "60")
		if registryError {
			writeRegistryError(w, http.StatusTooManyRequests, "DENIED", "too many requests")
		} else {
			Fail(w, http.StatusTooManyRequests, CodeRateLimited, "请求过于频繁")
		}
		return false
	}
	return true
}

// 处理 PUT /v2/*。
//
// manifest 写入走守卫（tag 覆盖保护）；blob 分片上传的最终 PUT 直通——
// blob 上传与 tag 覆盖策略正交（见 03-registry.md §3.1）。
func (a *App) handleV2Put(w http.ResponseWriter, r *http.Request) {
	rp, ok := service.ParseRegistryPath(r.URL.Path)
	if ok && rp.Kind == "manifests" {
		a.handlePutManifest(w, r)
		return
	}
	a.handleProxyRegistry(w, r)
}

func (a *App) handlePutManifest(w http.ResponseWriter, r *http.Request) {
	a.RegistrySvc.PutManifest(w, r)
}
