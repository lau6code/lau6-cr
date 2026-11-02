package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/crypto"
	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// ErrInvalidPersonalToken 表示个人令牌无效、已过期或已被吊销。
var ErrInvalidPersonalToken = errors.New("invalid personal token")

// ExpiryPolicy 描述个人令牌的失效策略。
//
// 只有三种合法组合，其余一律报错（**不静默取默认值**）：
//
//	{NeverExpires: true}        → 永久有效（expires_at 落 NULL）
//	{ExpiresAt: &t}             → t 时刻失效（unix 秒）
//	{}（两者都不给）             → 走系统默认（core.pat_ttl_seconds，默认 30 天）
//
// NeverExpires 与 ExpiresAt **同时给**是矛盾请求 → 报错。
//
// ⚠️ "永久"是用户**显式选**的，不是默认值：
//
//	默认值永远是"系统默认 TTL"，永久必须说出来。
//	否则用户以为拿到的是 30 天，实际拿到的是永不过期的凭据——那是安全事故。
type ExpiryPolicy struct {
	// NeverExpires 为 true 表示永久有效。
	NeverExpires bool
	// ExpiresAt 是指定的失效时间（unix 秒）；nil 表示"没指定"。
	ExpiresAt *int64
}

// PersonalTokenService 管理个人访问令牌（PAT）。
//
// PAT 是用户**自己生成、自己管理**的长期凭据：
// 用户在 CI / 脚本里用它推拉需要权限的镜像，不必把账户口令交出去。
//
// 口径参考实现 apc 的 `personal_token`：
//
//	原始令牌 = lau6_u_ + base64url(32 字节随机数)
//	入库     = SHA-256(原始令牌) 的十六进制
//	展示     = lau6_u_ + 前 6 位
//
// ⚠️ **原始令牌只返回一次**：之后既取不回来，也没法从库里还原。
//
//	这是"数据库泄露也拿不到可用凭据"的前提。
type PersonalTokenService struct {
	db *store.DB
	// ttl 取值是函数：设置页改了 core.pat_ttl_seconds 后立即生效。
	ttl func() time.Duration
	// logger 记录"可容忍但不该静默"的失败（如最近使用时间写不进去）。
	logger Logger
}

// PersonalTokenOptions 是 PersonalTokenService 的构造参数。
type PersonalTokenOptions struct {
	// TTL 是个人令牌的有效期；传 nil 时用 DefaultPersonalTokenTTL。
	TTL func() time.Duration
	// Logger 可选：用于记录被容忍的失败。
	Logger Logger
}

// DefaultPersonalTokenTTL 是个人令牌的默认有效期（30 天）。
//
// 与用户签名密钥、用户令牌同寿命口径（02-auth.md §2.6）。
const DefaultPersonalTokenTTL = 30 * 24 * time.Hour

// NewPersonalTokenService 构造个人令牌服务。
func NewPersonalTokenService(db *store.DB, o PersonalTokenOptions) *PersonalTokenService {
	ttl := o.TTL
	if ttl == nil {
		ttl = func() time.Duration { return DefaultPersonalTokenTTL }
	}
	return &PersonalTokenService{db: db, ttl: ttl, logger: o.Logger}
}

func (s *PersonalTokenService) TTL() time.Duration {
	return tokenTTL(s.ttl, DefaultPersonalTokenTTL)
}

// 返回的 raw 是**原始令牌**，只在这一次可见。
//
// policy 决定失效策略（见 ExpiryPolicy）：永久 / 指定时间 / 系统默认。
func (s *PersonalTokenService) Create(ctx context.Context, userID int64, name string, policy ExpiryPolicy) (string, *domain.PersonalToken, error) {
	name, err := validateTokenName(name)
	if err != nil {
		return "", nil, err
	}
	expiresAt, err := s.resolveExpiry(policy)
	if err != nil {
		return "", nil, err
	}
	raw, hash, prefix, err := crypto.GeneratePersonalToken()
	if err != nil {
		return "", nil, err
	}
	tok := &domain.PersonalToken{
		UserID:      userID,
		Name:        name,
		TokenHash:   hash,
		TokenPrefix: prefix,
		ExpiresAt:   expiresAt,
	}
	if err := s.db.CreatePersonalToken(ctx, tok); err != nil {
		return "", nil, fmt.Errorf("写个人令牌: %w", err)
	}
	return raw, tok, nil
}

// 返回 **nil = 永久有效**（落 NULL）。
func (s *PersonalTokenService) resolveExpiry(policy ExpiryPolicy) (*int64, error) {
	return resolveTokenExpiry(policy, s.TTL())
}

// SetExpiry 改一枚令牌的失效时间（限本人）。
//
// 只改失效时间，**不动散列**——令牌本体不变，
// CI 里已经配好的凭据不用重新配置。
//
// 永久 ↔ 定时可以来回改；都不给策略参数 = 重置为系统默认 TTL。
func (s *PersonalTokenService) SetExpiry(ctx context.Context, id, userID int64, policy ExpiryPolicy) error {
	expiresAt, err := s.resolveExpiry(policy)
	if err != nil {
		return err
	}
	if err := s.db.SetPersonalTokenExpiry(ctx, id, userID, expiresAt); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrInvalidPersonalToken
		}
		return err
	}
	return nil
}

func (s *PersonalTokenService) List(ctx context.Context, userID int64) ([]domain.PersonalToken, error) {
	return s.db.ListPersonalTokens(ctx, userID)
}

// Revoke 吊销一枚令牌（限本人）。
func (s *PersonalTokenService) Revoke(ctx context.Context, id, userID int64) error {
	if err := s.db.DeletePersonalToken(ctx, id, userID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrInvalidPersonalToken
		}
		return err
	}
	return nil
}

// 用于"吊销该用户所有凭据"：封禁、删号、改密后用。
func (s *PersonalTokenService) RevokeAll(ctx context.Context, userID int64) (int64, error) {
	return s.db.DeletePersonalTokensOfUser(ctx, userID)
}

// Verify 校验一枚个人令牌，返回它的属主与令牌记录。
//
// 令牌不存在 / 已过期 / 属主被禁用 → ErrInvalidPersonalToken。
//
// ⚠️ **不区分这三种情况**：调用方（认证路径）只需要知道"能不能用"，
//
//	细分原因对攻击者没有价值，对用户也没有可操作性。
func (s *PersonalTokenService) Verify(ctx context.Context, raw string) (*domain.User, *domain.PersonalToken, error) {
	if !crypto.IsPersonalToken(raw) {
		return nil, nil, ErrInvalidPersonalToken
	}
	hash := crypto.HashPersonalToken(raw)
	user, err := s.db.PersonalTokenOwner(ctx, hash)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil, ErrInvalidPersonalToken
		}
		return nil, nil, fmt.Errorf("读个人令牌: %w", err)
	}
	tok, err := s.db.PersonalTokenByHash(ctx, hash)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil, ErrInvalidPersonalToken
		}
		return nil, nil, fmt.Errorf("读个人令牌: %w", err)
	}
	// 最近使用时间只是**观测数据**，写失败不影响鉴权结论——
	// 把它当鉴权的一部分会让一次写失败放大成"令牌不可用"。
	//
	// ⚠️ 但"不影响结论"不等于"可以静默"：
	//    静默的话，"最近使用时间"会永远停在旧值，页面上的"最后使用"就成了假数据，
	//    而没人知道为什么。至少要留一条日志。
	if err := s.db.TouchPersonalToken(ctx, tok.ID); err != nil && s.logger != nil {
		s.logger.Warn("更新个人令牌最近使用时间失败", "err", err, "token_id", tok.ID)
	}
	return user, tok, nil
}
