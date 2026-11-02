package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
	"github.com/lau6code/lau6-cr/apps/api/internal/ldap"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// ErrInvalidCredentials 表示凭据无效。
//
// 对外**不区分**"用户不存在" / "密码错误" / "账号已禁用"——
// 区分它们等于开放账号枚举接口。
var ErrInvalidCredentials = errors.New("invalid credentials")

// ErrDisabled 表示账号已被禁用。
var ErrDisabled = errors.New("account disabled")

// ErrAuthUnavailable 表示**认证后端不可用**（LDAP 目录连不上）。
//
// 它和 ErrInvalidCredentials 是**两类**事：前者是服务故障，后者是用户没输对口令。
//
// ⚠️ 只有 ErrInvalidCredentials 计入登录失败退避/锁定
//
//	（access.login_fail_limit / access.login_lock_seconds）。
//	把目录抖动算成"口令错"，一次网络故障就能把所有 LDAP 账号
//	连同来源 IP 一起锁死，而用户看到的还是"用户名或密码错误"。
var ErrAuthUnavailable = errors.New("认证服务不可用")

// dummyHash 是一份固定的 bcrypt 散列，仅用于时序均衡。
//
// 用户不存在时拿它跑一次 bcrypt，让"用户不存在"与"密码错误"的
// 响应时间不可区分。
var dummyHash = []byte("$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy")

// burnTiming 跑一次注定失败的 bcrypt 比较，用于时序均衡。
//
// 在**不查用户散列**的拒绝路径上调用：没有真实散列可比时，
// 补一次等价的 bcrypt 开销，让调用方看不出"这个账号存在"。
func burnTiming(password string) {
	_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
}

// AuthResult 是认证结果。
type AuthResult struct {
	User            *domain.User
	AutoProvisioned bool
}

// AuthService 负责口令校验与会话签发。
type AuthService struct {
	db         *store.DB
	ldapFn     func() ldap.Authenticator
	ldapEnabFn func() bool
	allowCrtFn func() bool
	// guard 是登录退避守卫（连续失败 → 锁定）。
	guard *LoginGuard
	// logger 用于记录"退避计数写不进去"这类内部问题。
	logger *slog.Logger
}

// AuthOptions 是认证服务的配置。
type AuthOptions struct {
	// LDAP 返回**当前生效**的 LDAP 认证器；返回 nil 表示 LDAP 不可用。
	//
	// ⚠️ 用函数而不是值：ldap.* 是 T0 热生效设置
	//    （14-settings.md §3.4：「切换后立即影响登录」），
	//    客户端必须跟着设置快照走——启动时构造好就冻住的话，
	//    设置页上的 T0 就是个假开关。
	LDAP func() ldap.Authenticator
	// LDAPEnabled 返回 LDAP 当前是否启用。
	LDAPEnabled func() bool
	// AllowCreate 返回是否允许 LDAP 首次登录时自动建影子账号。
	AllowCreate func() bool
	// Guard 可选：登录失败退避 / 锁定（access.login_fail_limit、login_lock_seconds）。
	Guard *LoginGuard
	// Logger 可选。
	Logger *slog.Logger
}

// NewAuthService 构造认证服务。
func NewAuthService(db *store.DB, o AuthOptions) *AuthService {
	return &AuthService{
		db:         db,
		ldapFn:     o.LDAP,
		ldapEnabFn: o.LDAPEnabled,
		allowCrtFn: o.AllowCreate,
		guard:      o.Guard,
		logger:     o.Logger,
	}
}

// VerifyCredentials 校验用户名口令。
//
// 分派逻辑（与 06-ldap.md §3 一致）：
//
//	找到用户 → 按 source 决定校验方式
//	找不到   → 允许时走 LDAP 自动建号
//
// source 是**单一真理源**：source=local 的账号绝不回退到 LDAP。
func (s *AuthService) VerifyCredentials(ctx context.Context, username, password string) (*AuthResult, error) {
	// 登录退避 / 锁定
	//
	// 连续失败达到 access.login_fail_limit 后锁定（access.login_lock_seconds 上限）。
	// ⚠️ 锁定期的响应必须与"口令错"**完全一样**（调用方统一返 401 + 同一句文案），
	//    否则就成了"这个账号是否存在"的探测接口（13-password-transport.md §5.4）。
	ip := ClientIPFromContext(ctx)
	if err := s.guard.Check(ctx, username, ip); err != nil {
		return nil, err
	}

	res, err := s.verifyCredentials(ctx, username, password)
	if err != nil {
		// 只有"口令不对"算失败：账号被禁用、LDAP 不可达这些不是用户猜错了口令，
		// 算进去会让正常运维操作把账号锁住。
		if errors.Is(err, ErrInvalidCredentials) {
			if ferr := s.guard.RecordFailure(ctx, username, ip); ferr != nil && s.logger != nil {
				s.logger.Error("记录登录失败计数失败", "user", username, "err", ferr)
			}
		}
		return nil, err
	}
	// 登录成功 → 之前的失败是手滑，清零。
	if serr := s.guard.RecordSuccess(ctx, username, ip); serr != nil && s.logger != nil {
		s.logger.Error("清除登录失败计数失败", "user", username, "err", serr)
	}
	return res, nil
}

// verifyCredentials 是口令校验本体（不含退避计数）。
func (s *AuthService) verifyCredentials(ctx context.Context, username, password string) (*AuthResult, error) {
	// 空密码一律拒绝
	//
	// 必须**在查用户之前**拦：大量 LDAP 服务器在密码为空时会执行
	// "匿名 bind"并返回成功，不拦就是认证绕过。
	if password == "" {
		burnTiming(password)
		return nil, ErrInvalidCredentials
	}

	user, err := s.db.UserByUsername(ctx, username)
	if err == store.ErrNotFound {
		return s.provision(ctx, username, password)
	}
	if err != nil {
		return nil, err
	}

	if user.Disabled {
		burnTiming(password)
		return nil, ErrDisabled
	}

	switch user.Source {
	case "local":
		if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
			return nil, ErrInvalidCredentials
		}
		return &AuthResult{User: user}, nil

	case "ldap":
		auth := s.ldapAuthenticator()
		if !s.ldapEnabled() || auth == nil {
			// LDAP 未启用但本地行标了 ldap → 保守拒绝。
			// 降级为"本地口令能登"会让人误以为身份来源没变。
			burnTiming(password)
			return nil, ErrInvalidCredentials
		}
		if err := auth.Bind(ctx, username, password); err != nil {
			return nil, classifyLDAPError(err)
		}
		return &AuthResult{User: user}, nil

	default:
		return nil, ErrInvalidCredentials
	}
}

func (s *AuthService) ldapAuthenticator() ldap.Authenticator {
	if s.ldapFn == nil {
		return nil
	}
	return s.ldapFn()
}

func (s *AuthService) ldapEnabled() bool {
	if s.ldapEnabFn == nil {
		return false
	}
	return s.ldapEnabFn()
}

func (s *AuthService) allowCreate() bool {
	if s.allowCrtFn == nil {
		return false
	}
	return s.allowCrtFn()
}

// classifyLDAPError 把 LDAP 的失败翻译成认证层的错误。
//
// ⚠️ 只有"凭据无效"才算用户猜错了口令：
//
//	目录不可达（网络抖动、目录挂了）是**服务故障**，
//	算进登录失败计数会让一次目录抖动把所有 LDAP 账号锁死。
func classifyLDAPError(err error) error {
	if errors.Is(err, ldap.ErrInvalidCredentials) {
		return ErrInvalidCredentials
	}
	return fmt.Errorf("%w: %w", ErrAuthUnavailable, err)
}

// provision 在 LDAP bind 成功且允许自动建号时创建影子账号。
func (s *AuthService) provision(ctx context.Context, username, password string) (*AuthResult, error) {
	auth := s.ldapAuthenticator()
	if !s.ldapEnabled() || auth == nil || !s.allowCreate() {
		burnTiming(password)
		return nil, ErrInvalidCredentials
	}
	if err := auth.Bind(ctx, username, password); err != nil {
		if errors.Is(err, ldap.ErrInvalidCredentials) {
			burnTiming(password)
			return nil, ErrInvalidCredentials
		}
		// 目录不可达：这是故障，不是"口令错"——不能计入失败锁定。
		return nil, classifyLDAPError(err)
	}
	// 影子账号存哨兵串——**不存任何可解密形式**。
	u, err := s.db.CreateUser(ctx, username, ldapPasswordSentinel, "ldap", username, false)
	if err != nil {
		if store.IsDuplicate(err) {
			// 并发：另一个请求抢先建好了，查回来用。
			other, gerr := s.db.UserByUsername(ctx, username)
			if gerr == nil && other.Source == "ldap" && !other.Disabled {
				return &AuthResult{User: other, AutoProvisioned: true}, nil
			}
			// 抢到的是 local 行 → 运维错配，拒绝。
			return nil, ErrInvalidCredentials
		}
		return nil, fmt.Errorf("创建影子账号: %w", err)
	}
	return &AuthResult{User: u, AutoProvisioned: true}, nil
}

// ldapPasswordSentinel 是 LDAP 账号的本地口令占位符。
//
// 永远不与任何真实口令相等；bcrypt 校验它必然失败。
const ldapPasswordSentinel = "!no-local-password!"

// ErrOldPasswordMismatch 表示旧口令不正确。
var ErrOldPasswordMismatch = errors.New("old password mismatch")

// ErrPasswordNotLocal 表示账号口令不由本地管理（LDAP 影子账号）。
var ErrPasswordNotLocal = errors.New("password is managed by the directory")

// ChangeLocalPassword 校验旧口令后，把本地账号的口令改成新口令。
//
// 只支持本地账号：LDAP 用户的口令在目录里，改本地散列等于造出一个
// 与目录不一致的影子账号——登录时认证源会错配（见 06-ldap.md §3）。
//
// 旧口令在这里直接比对本地散列，**不走** VerifyCredentials：
// 那条路径会回退到 LDAP bind，LDAP 用户拿着目录口令就能"改"一个
// 本地口令出来，正好是上面要避免的情况。
func (s *AuthService) ChangeLocalPassword(ctx context.Context, userID int64, oldPassword, newPassword string) error {
	u, err := s.db.UserByID(ctx, userID)
	if err != nil {
		return err
	}
	if u.Source != "local" || u.PasswordHash == ldapPasswordSentinel {
		return ErrPasswordNotLocal
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(oldPassword)); err != nil {
		return ErrOldPasswordMismatch
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	return s.db.UpdatePassword(ctx, u.ID, string(hash))
}

func (s *AuthService) CreateSession(ctx context.Context, userID int64, ttl time.Duration, ip, ua string) (string, error) {
	raw, err := newSessionID()
	if err != nil {
		return "", err
	}
	if err := s.db.CreateSession(ctx, store.HashSessionID(raw), userID, time.Now().Add(ttl).Unix(), ip, ua); err != nil {
		return "", err
	}
	return raw, nil
}

func (s *AuthService) DestroySession(ctx context.Context, raw string) error {
	if raw == "" {
		return nil
	}
	return s.db.DeleteSession(ctx, store.HashSessionID(raw))
}

func newSessionID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
