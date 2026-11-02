// Package server 负责 HTTP 路由装配与通用中间件。
package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/lau6code/lau6-cr/apps/api/internal/cache"
	"github.com/lau6code/lau6-cr/apps/api/internal/config"
	"github.com/lau6code/lau6-cr/apps/api/internal/crypto"
	"github.com/lau6code/lau6-cr/apps/api/internal/gc"
	"github.com/lau6code/lau6-cr/apps/api/internal/service"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// 业务错误码。0 表示成功。
const (
	CodeOK              = 0
	CodeInvalidParam    = 40001
	CodeUnauthorized    = 40101
	CodeForbidden       = 40301
	CodeNotVisible      = 40302
	CodeNotFound        = 40401
	CodeConflict        = 40901
	CodeVersionConflict = 40902
	CodeRateLimited     = 42901
	CodeInternal        = 50001
	CodeUnavailable     = 50301
	CodeSetupRequired   = 50302
)

// Envelope 是业务 API 的统一响应信封。
type Envelope struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// App 持有全部依赖与路由。
type App struct {
	Cfg *config.Config
	DB  *store.DB
	// Cache 是当前生效的缓存后端；支持运行期热切换（换 driver / 换连接参数）。
	Cache  *cache.Holder
	Keys   *crypto.Keystore
	Tokens *crypto.TokenIssuer
	// UserSecrets 是每用户的 JWT 签名密钥库（用于按用户吊销令牌）。
	UserSecrets *crypto.UserSecretStore
	// PasswordKeys 是口令加密的 X25519 密钥库（应用层口令加密）。
	//
	// ⚠️ 必须显式注入：口令传输模式启用而它为 nil 时，
	//    公钥接口会**每个请求**都 500（见 CheckPasswordTransport）。
	PasswordKeys *crypto.PasswordKeyStore
	Perm         *service.PermissionEngine
	Upstream     *http.Client
	Stats        *service.StatsAggregator
	Logger       *slog.Logger
	// RateLimiter 是按来源 IP 的令牌桶限流器（匿名 /token 与匿名读路径）。
	RateLimiter *service.RateLimiter

	// 各业务服务。
	AuthSvc  *service.AuthService
	TokenSvc *service.TokenService
	// Pats 是个人令牌服务（PAT）——用户自己生成的长期凭据。
	Pats *service.PersonalTokenService
	// ProjectTokenSvc 是项目令牌服务——项目管理员生成的项目身份长期凭据。
	ProjectTokenSvc *service.ProjectTokenService
	RegistrySvc     *service.RegistryService
	SetupSvc        *service.SetupService
	SettingsSvc     *service.SettingsManager
	TrafficSvc      *service.TrafficService
	RefreshQ        *service.RefreshQueue
	Refresher       *service.Refresher
	// CleanupSvc 执行保留策略（决定删哪些 tag）。
	CleanupSvc *service.CleanupService
	// GCSvc 执行 blob 回收（第 ② 层，把磁盘空间收回来）。
	GCSvc *service.GCService
	// WriteBarrier 是仓库写屏障。
	//
	// blob 回收期间它会挂起 /v2/* 的写操作（push / delete），
	// 等在途上传排空后再扫盘——官方 GC 要求 registry 停写，
	// lau6-cr 是唯一入口，所以停写由它自己做。
	WriteBarrier *gc.Barrier

	// UpstreamAddr 是 distribution 的地址。
	UpstreamAddr string
	// WebhookSecret 是 distribution 事件回调的共享密钥。
	WebhookSecret string
	// ReadyFunc 供 /readyz 判断上游是否就绪。
	ReadyFunc func(context.Context) error
	// ReloadConfig 在配置变更后重建运行时状态。
	ReloadConfig func() error
	// ReloadCache 按新配置重建缓存后端并热切换。
	//
	// 缓存连接参数（driver / Redis 地址、密码、前缀）改了之后调用它，
	// 新后端立即接管，旧的择时关闭。
	ReloadCache func(cfg *config.Config) error
	// ReloadLogging 按新配置调整运行中的日志器。
	//
	// 只处理**日志级别**——它用 slog.LevelVar 实现，原子生效。
	// 文件相关参数（目录、滚动方式、保留天数）改动需要重建写入器，
	// 走重启，不在这里处理。
	ReloadLogging func(cfg *config.Config) error
	// trusted 是可信反代前缀列表（热更新）。
	trusted atomic.Pointer[[]netip.Prefix]
}

func OK(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, Envelope{Code: CodeOK, Message: "ok", Data: data})
}

// Fail 返回业务错误响应。
func Fail(w http.ResponseWriter, status, code int, msg string) {
	writeJSON(w, status, Envelope{Code: code, Message: msg})
}

// FailWithData 返回**带数据**的业务错误响应。
//
// 用于"执行失败了，但失败详情本身是有用信息"的场景——
// 例如清理执行失败时把执行记录（失败明细、跳过原因）一起返回，
// 否则前端只能拿到一句"失败了"，用户还得自己去翻历史才能看到为什么。
//
// ⚠️ HTTP 状态码仍然是非 2xx：**失败就是失败**，
// 不能用 200 带一个 status=failed 的对象去假装成功。
func FailWithData(w http.ResponseWriter, status, code int, msg string, data any) {
	writeJSON(w, status, Envelope{Code: code, Message: msg, Data: data})
}

// writeJSON 写业务信封响应。
//
// ⚠️ **先序列化、再写状态码**：
// 顺序反过来（先 WriteHeader 再编码）的话，编码失败时状态码已经发出去了，
// 改不回来——客户端拿到的是 200 + 半截 body，
// 而日志里若不记就什么都没有（无声的失败，AGENTS.md 规则 3）。
//
// 先序列化就还有机会把它变成一次**调用方看得见的失败**（500）。
func writeJSON(w http.ResponseWriter, status int, body Envelope) {
	// Data 是 any：里面塞了 chan / func / NaN 时 json.Marshal 会失败。
	// 这种失败必须暴露，不能回一个 200 + 空 body 假装成功。
	buf, err := json.Marshal(body)
	if err != nil {
		slog.Error("序列化响应失败", "status", status, "err", err)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		// 兜底的 body 也序列化一次——它只含固定字段，出错概率极低；
		// 万一还是失败，就让连接断开（客户端看到的是"没有完整响应"，
		// 而不是一份格式正确、内容却错乱的 body）。
		if fallback, ferr := json.Marshal(Envelope{
			Code: CodeInternal, Message: "内部错误",
		}); ferr == nil {
			_, _ = w.Write(fallback)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if _, err := w.Write(buf); err != nil {
		// 客户端中途断开等。状态码已经发出去了，改不了——
		// 但要留痕，否则"响应没写完"这件事没人知道。
		slog.Warn("写出响应体失败", "status", status, "err", err)
	}
}

// 解析请求体：上限 1MB，且未知字段直接报错——
// 前端字段名拼错时必须当场失败，不能被静默忽略。
func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

// decodeJSONBody 解析请求体；解析失败写 400 并返回 false。
//
// 返回 false 表示错误响应已经写出去了，调用方必须立刻 return。
func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := decodeJSON(r, dst); err != nil {
		Fail(w, http.StatusBadRequest, CodeInvalidParam, "请求格式不正确")
		return false
	}
	return true
}

// 中间件

// withSession 解析 session cookie 并把主体放进 context。
//
// 未登录**不是错误**——公开镜像仓允许匿名浏览，所以这里只解析不拦截。
//
// 同时把客户端 IP 放进 context：限流与登录退避在**服务层**，
// 而 IP 只在 HTTP 层拿得到（请求对象）——用 context 传递，
// 就不用把 request 一路透传到服务层。
func (a *App) withSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := service.Principal{}
		if ck, err := r.Cookie(a.Cfg.Session.CookieName); err == nil && ck.Value != "" {
			hash := store.HashSessionID(ck.Value)
			u, err := a.DB.SessionUser(r.Context(), hash)
			switch {
			case err != nil:
				// ⚠️ 读库失败**不等于**"未登录"。
				//    当成未登录会让整个站点在库故障时静默变成匿名：
				//    匿名能拉公开仓库，管理员却以为什么都没发生，
				//    而真正的原因（数据库挂了）被这条路径完全吞掉。
				//    这里必须显式失败：503 说清是服务端的问题。
				a.log().Error("读取会话失败", "err", err)
				Fail(w, http.StatusServiceUnavailable, CodeUnavailable, "服务暂时不可用")
				return
			case u != nil:
				p = service.Principal{User: u}
				// 续期失败不影响本次鉴权结论（会话还有效），
				// 但必须留痕：否则会话续期会静默失效，表现为"用着用着被登出"。
				if err := a.DB.TouchSession(r.Context(), hash); err != nil {
					a.log().Warn("续期会话失败", "err", err)
				}
			}
		}
		ctx := service.WithClientIP(r.Context(), a.clientIPForLimit(r))
		next.ServeHTTP(w, r.WithContext(withPrincipal(ctx, p)))
	})
}

// log 返回 App 的日志器；未设置时退回 slog.Default()。
//
// withSession 在**每个**请求上都会走，测试里构造的 App 常常不带 Logger——
// 直接 a.Logger.Xxx 会 panic。
func (a *App) log() *slog.Logger {
	if a.Logger != nil {
		return a.Logger
	}
	return slog.Default()
}

// clientIPForLimit 取限流用的客户端 IP。
//
// ⚠️ X-Forwarded-For 由客户端可控，**只有在请求来自可信反代时**才采信；
// 可信反代列表为空（默认）时一律用 TCP 对端地址。
//
// 为什么必须处理反代：反代后面所有客户端的 RemoteAddr 都是反代自己，
// 不解析 XFF 的话整个公网会被算成**一个** IP ——
// 限流要么形同虚设，要么一键把所有人一起限掉。
func (a *App) clientIPForLimit(r *http.Request) string {
	host := remoteHost(r.RemoteAddr)
	prefixes := a.trusted.Load()
	if prefixes == nil || len(*prefixes) == 0 {
		return host
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	addr = addr.Unmap()
	for _, p := range *prefixes {
		if !p.Contains(addr) {
			continue
		}
		if ip := lastUntrustedFromXFF(r.Header.Get("X-Forwarded-For"), prefixes); ip != "" {
			return ip
		}
		break
	}
	return host
}

// lastUntrustedFromXFF 从 X-Forwarded-For 里取**最右侧的非可信**地址。
//
// 为什么取最右侧：XFF 是逐跳追加的（`client, proxy1, proxy2`），
// 最右侧那个"不是可信反代"的地址才是真实来源。
// 取最左侧会被客户端自己伪造的头欺骗——
// 客户端可以往 XFF 里塞任意值。
func lastUntrustedFromXFF(xff string, prefixes *[]netip.Prefix) string {
	if xff == "" {
		return ""
	}
	parts := strings.Split(xff, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		addr, err := netip.ParseAddr(strings.TrimSpace(parts[i]))
		if err != nil {
			continue
		}
		addr = addr.Unmap()
		trusted := false
		for _, p := range *prefixes {
			if p.Contains(addr) {
				trusted = true
				break
			}
		}
		if !trusted {
			return addr.String()
		}
	}
	return ""
}

// 从 RemoteAddr 里取出主机部分；不带端口时保留原串。
func remoteHost(remote string) string {
	ip := remote
	if host, _, err := net.SplitHostPort(remote); err == nil {
		ip = host
	}
	if len(ip) > 45 {
		ip = ip[:45]
	}
	return ip
}

func (a *App) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := principalFrom(r.Context())
		if p.IsAnonymous() {
			Fail(w, http.StatusUnauthorized, CodeUnauthorized, "请先登录")
			return
		}
		if !p.User.IsSystemAdmin {
			Fail(w, http.StatusForbidden, CodeForbidden, "需要系统管理员权限")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireLogin 拦截未登录请求。
//
// 语义比 requireAdmin 宽：只要求"已登录"，不要求任何角色。
// 用于"登录后可读、但不该让匿名枚举"的接口（项目列表、设置读取等）。
func (a *App) requireLogin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if principalFrom(r.Context()).IsAnonymous() {
			Fail(w, http.StatusUnauthorized, CodeUnauthorized, "请先登录")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireProjectAdmin 校验当前主体是该项目的管理员。
//
// 看不见的项目一律按"不存在"回（404），不泄漏项目是否存在；
// 看得见但不是项目管理员的才回 403。
func (a *App) requireProjectAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		project, ok := a.resolveProject(w, r, CodeNotVisible, "项目不存在或无权访问")
		if !ok {
			return
		}
		if !a.canViewProject(r, project, principalFrom(r.Context())) {
			Fail(w, http.StatusNotFound, CodeNotVisible, "项目不存在或无权访问")
			return
		}
		if !a.isProjectAdmin(r, project) {
			Fail(w, http.StatusForbidden, CodeForbidden, "需要项目管理员权限")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withWriteBarrier 在 blob 回收期间挂起仓库写操作。
//
// 官方 `registry garbage-collect` 是 stop-the-world 的：
// 官方文档明确警告过"上传中的镜像层可能被误删，导致镜像损坏"。
//
// lau6-cr 是 distribution 的**唯一入口**（compose 里 registry 不对外暴露端口），
// 所以"停写"可以由 lau6-cr 自己做：
//
//	屏障关闭 → 新的 push / delete 被拒（503）
//	在途的上传传完 → 扫盘 → 开闸
//
// 只读路径（pull）**不受影响**，GC 期间照常服务。
//
// ⚠️ 拒绝时必须用 registry 错误信封（Docker 客户端看不懂业务信封），
//
//	并且明确告诉客户端这是"稍后重试"，而不是协议错误。
func (a *App) withWriteBarrier(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.WriteBarrier == nil {
			next.ServeHTTP(w, r)
			return
		}
		if !a.WriteBarrier.Enter() {
			w.Header().Set("Retry-After", "30")
			writeRegistryError(w, http.StatusServiceUnavailable, "UNAVAILABLE",
				"仓库正在回收磁盘空间，写操作暂时不可用，请稍后重试")
			return
		}
		defer a.WriteBarrier.Leave()
		next.ServeHTTP(w, r)
	})
}

// requireSetupDone 拦截未初始化的实例。
//
// 注意：/setup 与 /healthz 自身不走这个中间件。
func (a *App) requireSetupDone(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		initialized, err := a.isInitialized(r.Context())
		if err != nil {
			Fail(w, http.StatusInternalServerError, CodeInternal, "读取初始化状态失败")
			return
		}
		if !initialized {
			Fail(w, http.StatusServiceUnavailable, CodeSetupRequired, "实例尚未初始化，请先完成 setup")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isInitialized 判断实例是否已完成初始化。
func (a *App) isInitialized(ctx context.Context) (bool, error) {
	st, err := a.SetupSvc.Status(ctx)
	if err != nil {
		return false, err
	}
	return st.Initialized, nil
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// requestID 给每个请求打上可追踪的 ID。
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = strconv.FormatInt(time.Now().UnixNano(), 36)
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(withRequestID(r.Context(), id)))
	})
}

// accessLog 记录每个请求的方法/路径/状态/耗时。
//
// ⚠️ 没有它，"客户端报 unauthorized 但后端一片安静"根本没法查：
// 鉴权拒绝、token 签发失败、上游 4xx 全都发生在 handler 内部，
// 没有访问日志就只能靠猜。
//
// 级别：5xx → error，4xx → warn，其余 → info。
// 探活路径（/healthz /readyz）不记——它们每 30s 一次，
// 记下来会把真正的日志淹掉。
func (a *App) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isProbePath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		// 用 chi 的包装器而不是自己写一个：
		// 它实现了 Flusher/Hijacker，**blob 流式转发必须靠它**
		// （自己包一层会丢掉 Flush，几 GiB 的上传会被缓冲住）。
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.Status(),
			"bytes", ww.BytesWritten(),
			"duration_ms", time.Since(start).Milliseconds(),
			"ip", clientIP(r),
			"request_id", requestIDFrom(r.Context()),
		}
		// ⚠️ 只记 Authorization 的**方案名**，绝不记值——值就是凭据本身。
		//    排障需要的是"这次请求带没带凭据"，不是"带的是哪枚凭据"。
		if scheme := service.AuthScheme(r); scheme != "" {
			attrs = append(attrs, "auth", scheme)
		}
		switch {
		case ww.Status() >= 500:
			a.Logger.Error("请求处理失败", attrs...)
		case ww.Status() >= 400:
			// 版本检查端点的 401 是**协议握手的一部分**，不是错误：
			// 每个 docker 命令都会 ping 一次，按 4xx 记 warn 会把日志刷爆。
			if isVersionCheckPath(r.URL.Path) && ww.Status() == http.StatusUnauthorized {
				a.Logger.Info("registry 版本检查", attrs...)
				return
			}
			a.Logger.Warn("请求被拒绝", attrs...)
		default:
			a.Logger.Info("请求完成", attrs...)
		}
	})
}

func isProbePath(p string) bool {
	return p == "/healthz" || p == "/readyz"
}

func isVersionCheckPath(p string) bool {
	return p == "/v2/" || p == "/v2"
}

// context

type ctxKey int

const (
	principalKey ctxKey = iota
	requestIDKey
)

func withPrincipal(ctx context.Context, p service.Principal) context.Context {
	return context.WithValue(ctx, principalKey, p)
}

// withRequestID 把请求 ID 放进 context，供访问日志关联。
func withRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// principalFrom 取出请求主体。
func principalFrom(ctx context.Context) service.Principal {
	p, _ := ctx.Value(principalKey).(service.Principal)
	return p
}

// actorName 返回审计用的主体名。
func actorName(ctx context.Context) string {
	return subjectOf(principalFrom(ctx))
}

// 取客户端 IP（不做 XFF 解析——反代场景下由反代侧传入更可信）。
func clientIP(r *http.Request) string {
	return remoteHost(r.RemoteAddr)
}

// HTTPS 判定

// SetTrustedProxies 更新可信反代列表。
func (a *App) SetTrustedProxies(list []string) {
	prefixes := parseTrustedProxies(list)
	a.trusted.Store(&prefixes)
}

// parseTrustedProxies 解析 CIDR 列表；单条非法则跳过并返回空。
func parseTrustedProxies(list []string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(list))
	for _, s := range list {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if p, err := netip.ParsePrefix(s); err == nil {
			out = append(out, p)
			continue
		}
		if addr, err := netip.ParseAddr(s); err == nil {
			out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
		}
	}
	return out
}

// requestIsHTTPS 判断请求是否经 HTTPS 到达。
//
// 形态 B（外层反代终结 TLS）下 lau6-cr 收到的是明文 HTTP，
// 只能靠 X-Forwarded-Proto 判断——但**该头由客户端可控**，
// 所以只信任配置里的反代 IP。
func (a *App) requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	prefixes := a.trusted.Load()
	if prefixes == nil || len(*prefixes) == 0 {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	for _, p := range *prefixes {
		if p.Contains(addr) {
			return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
		}
	}
	return false
}

// cookieSecure 决定会话 Cookie 是否加 Secure 属性。
func (a *App) cookieSecure(r *http.Request) bool {
	switch strings.ToLower(strings.TrimSpace(a.Cfg.Session.Secure)) {
	case "true", "yes", "1":
		return true
	case "false", "no", "0":
		return false
	default: // auto
		return a.requestIsHTTPS(r)
	}
}
