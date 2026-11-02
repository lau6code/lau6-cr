package service

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/crypto"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// RegistryService 负责 /v2/* 的鉴权与转发。
type RegistryService struct {
	upstream         *http.Client
	upstreamURL      string
	perm             *PermissionEngine
	issuer           *crypto.TokenIssuer
	db               *store.DB
	locks            *keyLocker
	stats            *StatsAggregator
	maxManifestBytes int64
	policyFn         func() Policy
	realmFn          func() string
	// refreshQueue 用于"检测到 manifest 写入就排队刷新目录"。
	// 可为 nil（不启用这条路）。
	refreshQueue *RefreshQueue
	// cleanupTrigger 在 push 成功后触发该项目的保留策略。
	// 可为 nil（不启用清理）。
	cleanupTrigger func(ctx context.Context, projectID int64, projectName string)
	// pats 是个人令牌服务——/v2/* 直接接受 PAT 作为 Bearer 令牌。
	// 可为 nil（不启用这条路径）。
	pats *PersonalTokenService
	// projTokens 是项目令牌服务——/v2/* 直接接受项目令牌作为 Bearer 令牌。
	// 可为 nil（不启用这条路径）。
	projTokens *ProjectTokenService
	// logger 用于排障细粒度日志（debug 级别）。
	logger Logger
}

// RegistryDeps 是 RegistryService 的依赖。
type RegistryDeps struct {
	Upstream         *http.Client
	UpstreamURL      string
	Perm             *PermissionEngine
	Issuer           *crypto.TokenIssuer
	DB               *store.DB
	Stats            *StatsAggregator
	MaxManifestBytes int64
	PolicyFn         func() Policy
	RealmFn          func() string
	// RefreshQueue 是刷新队列；传 nil 则不启用"push 后立刻刷新目录"。
	RefreshQueue *RefreshQueue
	// PATs 是个人令牌服务；传 nil 则不启用"PAT 直接当 Bearer 令牌"。
	PATs *PersonalTokenService
	// ProjectTokens 是项目令牌服务；传 nil 则不启用"项目令牌直接当 Bearer 令牌"。
	ProjectTokens *ProjectTokenService
	// Logger 是日志器，可为 nil（不记 debug 日志）。
	Logger Logger
}

// NewRegistryService 构造 registry 服务。
func NewRegistryService(d RegistryDeps) *RegistryService {
	maxBody := d.MaxManifestBytes
	if maxBody <= 0 {
		maxBody = 16 << 20
	}
	return &RegistryService{
		upstream:         d.Upstream,
		upstreamURL:      strings.TrimRight(d.UpstreamURL, "/"),
		perm:             d.Perm,
		issuer:           d.Issuer,
		db:               d.DB,
		locks:            newKeyLocker(),
		stats:            d.Stats,
		maxManifestBytes: maxBody,
		policyFn:         d.PolicyFn,
		realmFn:          d.RealmFn,
		refreshQueue:     d.RefreshQueue,
		pats:             d.PATs,
		projTokens:       d.ProjectTokens,
		logger:           d.Logger,
	}
}

// SetCleanupTrigger 设置 push 成功后的清理触发回调。
//
// 分两步接是有意的：RegistryService 与 CleanupService 互相持有会形成环，
// 用回调打断依赖。
func (s *RegistryService) SetCleanupTrigger(fn func(ctx context.Context, projectID int64, projectName string)) {
	s.cleanupTrigger = fn
}

// Forward 处理 /v2/* 的常规请求（blob 传输、catalog、读 manifest 等）。
func (s *RegistryService) Forward(w http.ResponseWriter, r *http.Request) {
	// 版本检查端点（GET /v2/）
	//
	// 这里**不能**透传上游的 200，必须回 401 + WWW-Authenticate。
	//
	// docker 客户端的认证握手是"先 ping /v2/ 拿挑战"：
	//   - 拿到 200 → 认定"本 registry 不需要认证" → **根本不去申请 token**
	//     → 后续 blob 上传不带 Authorization → 被 401 拒 → push 报 unauthorized；
	//   - 拿到 401 + Bearer 挑战 → 才去 realm 换 token，重试时带上 Bearer。
	//
	// 顺带修掉另一个坑：回 200 时 `docker login` **根本不校验口令**——
	// 客户端把 200 当成"凭据有效"，密码写错也显示 Login Succeeded。
	//
	// ⚠️ 这个判断必须在 ParseRegistryPath **之前**：
	//    "/v2"（无尾斜杠）不匹配 "/v2/" 前缀，会被当成非法路径打回 400。
	if isVersionCheck(r.URL.Path) {
		s.versionCheck(w, r)
		return
	}

	rp, ok := ParseRegistryPath(r.URL.Path)
	if !ok {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if rp.Kind == "catalog" {
		s.catalog(w, r)
		return
	}
	if rp.Repo == "" {
		// 其余无仓库上下文的请求：按匿名读处理。
		if !s.authorizeAnonymous(w, r, "", ActionPull) {
			return
		}
		s.stream(w, r, rp, nil, nil)
		return
	}

	// 动作由**路径形态 + 方法**决定：写路径要 push，读路径要 pull。
	action := requiredAction(rp, r.Method)
	claims, ok := s.authorize(w, r, rp.Repo, action)
	if !ok {
		return
	}
	// ⚠️ 跨仓库 mount（`?from=<源仓库>&mount=<digest>`）还会**读源仓库的 blob**：
	//    源仓库也要有 pull 权限，否则能把私有仓库的 blob"搬"进自己的仓库再拉走。
	if action == ActionPush {
		if src := r.URL.Query().Get("from"); src != "" {
			if _, ok := s.authorize(w, r, src, ActionPull); !ok {
				return
			}
		}
	}
	s.stream(w, r, rp, nil, claims)
}

// logWarn / logError 是带请求上下文的日志包装。
//
// logger 可能为 nil（测试里不注入），所以每个出口都要判空。
func (s *RegistryService) logWarn(msg string, r *http.Request, args ...any) {
	if s.logger == nil {
		return
	}
	s.logger.Warn(msg, append([]any{
		"method", r.Method, "path", r.URL.Path, "ip", clientIPOf(r),
		"auth", AuthScheme(r),
	}, args...)...)
}

func (s *RegistryService) logError(msg string, r *http.Request, args ...any) {
	if s.logger == nil {
		return
	}
	s.logger.Error(msg, append([]any{
		"method", r.Method, "path", r.URL.Path, "ip", clientIPOf(r),
		"auth", AuthScheme(r),
	}, args...)...)
}

const streamTimeout = 30 * time.Minute
