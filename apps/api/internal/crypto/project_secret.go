package crypto

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"strconv"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/cache"
)

const (
	// CacheKeyProjectSecretPrefix 是项目令牌签名密钥的缓存键前缀。
	//
	// 键名 = 前缀 + 令牌 ID。令牌 ID 来自项目令牌表（不是外部输入），
	// 但仍要经过校验——键名里的控制字符会污染 Redis 的键空间。
	CacheKeyProjectSecretPrefix = "cr:project-secret:"

	// projectSecretBytes 是项目令牌密钥的字节数。
	//
	// 与用户密钥同口径：256 bit，满足 HMAC-SHA256 的密钥强度要求。
	projectSecretBytes = 32
)

// ProjectSecret 是一枚项目令牌的签名密钥记录。
//
// 有效期从**创建时刻**起算，不因读取而续期——
// 密钥到期即该令牌换发的所有 JWT 一起作废。
type ProjectSecret struct {
	// Secret 是 HMAC 密钥（base64url，无填充）。
	Secret string `json:"secret"`
	// CreatedAt 是密钥的创建时刻（UTC）。
	CreatedAt time.Time `json:"created_at"`
}

func (p ProjectSecret) Expired(ttl time.Duration) bool {
	return time.Since(p.CreatedAt) >= ttl
}

// secretValue 返回 HMAC 密钥（供泛型实现读取，不进导出 API）。
func (p ProjectSecret) secretValue() string { return p.Secret }

// createdAt 返回密钥的创建时刻（供泛型实现读取，不进导出 API）。
func (p ProjectSecret) createdAt() time.Time { return p.CreatedAt }

// ProjectSecretStore 管理**每枚项目令牌自己的** JWT 签名密钥。
//
// 存放策略与用户密钥同构（docs/design/12-cache.md §3.6）：
//
//	cache.driver=redis → 缓存（Redis）是权威源，键 cr:project-secret:<token_id>
//	cache.driver=local  → 文件是权威源，<dir>/project-<sha256(token_id)>.key
//
// 两种后端的语义完全一致：一条记录 = {密钥, 创建时间}。
//
// ⚠️ 与用户密钥的差别：用户密钥是"每用户一把"，吊销粒度是一个用户；
// 本文件是"每枚令牌一把"，吊销粒度是**一枚令牌**——
// 吊销某枚项目令牌时，它换发出去的所有 JWT 立刻作废。
type ProjectSecretStore struct {
	*secretStore[int64, ProjectSecret]
}

// ProjectSecretOptions 是 ProjectSecretStore 的构造参数。
type ProjectSecretOptions struct {
	// Cache 是缓存后端持有者（热切换后取新后端用）。
	// 传 nil 时只能走文件后端。
	Cache *cache.Holder
	// Dir 是 local 模式下项目令牌密钥文件的存放目录。
	Dir string
	// TTL 是项目令牌密钥（与项目令牌的客户端令牌）的有效期。
	// 传 nil 时用 DefaultProjectSecretTTL。
	TTL func() time.Duration
}

// DefaultProjectSecretTTL 是项目令牌密钥的默认有效期（30 天）。
//
// 与个人令牌同口径（core.pat_ttl_seconds / core.project_token_ttl_seconds）。
const DefaultProjectSecretTTL = 30 * 24 * time.Hour

// NewProjectSecretStore 构造项目令牌密钥库。
func NewProjectSecretStore(o ProjectSecretOptions) *ProjectSecretStore {
	return &ProjectSecretStore{
		secretStore: newSecretStore[int64, ProjectSecret](o.Cache, o.Dir, o.TTL, projectSecretSpec{}),
	}
}

// Get 读取项目令牌密钥。密钥不存在或已过期时返回 ok=false。
//
// 读失败**必须报错**，不能当成"没有密钥"——
// 那会让一枚本该有效的令牌被静默判成无效。
func (s *ProjectSecretStore) Get(ctx context.Context, tokenID int64) (ProjectSecret, bool, error) {
	return s.secretStore.Get(ctx, tokenID)
}

// Ensure 读取项目令牌密钥；没有就生成一把并写回去。
//
// 并发安全：两个请求同时来只有一个会真正写入，
// 另一个拿到先写好的那份——否则后写的会覆盖先写的，
// 先前签发的令牌会突然验签失败。
func (s *ProjectSecretStore) Ensure(ctx context.Context, tokenID int64) (ProjectSecret, error) {
	return s.secretStore.Ensure(ctx, tokenID)
}

// Revoke 吊销项目令牌密钥。
//
// 删掉之后该令牌换发的**所有** JWT 立刻作废（验签时找不到密钥），
// 下次签发会生成一把新的。
func (s *ProjectSecretStore) Revoke(ctx context.Context, tokenID int64) error {
	return s.secretStore.Revoke(ctx, tokenID)
}

// Migrate 把项目令牌密钥从旧后端搬到新后端（换缓存后端时用）。
//
// 有效期**按剩余寿命**搬，不重新给满 TTL：
// 重新给满等于把"30 天过期"变成"每换一次后端就续命 30 天"。
//
// tokenIDs 来自项目令牌表——令牌不存在后残留的密钥没有搬迁价值。
func (s *ProjectSecretStore) Migrate(ctx context.Context, from, to cache.Cache, tokenIDs []int64) (int, error) {
	return s.secretStore.Migrate(ctx, from, to, tokenIDs)
}

// TTL 返回项目令牌密钥的有效期。
func (s *ProjectSecretStore) TTL() time.Duration {
	return s.secretStore.TTL()
}

// projectSecretSpec 是项目令牌密钥与用户密钥的差异点。
type projectSecretSpec struct{}

func (projectSecretSpec) label() string                     { return "项目令牌密钥" }
func (projectSecretSpec) defaultTTL() time.Duration         { return DefaultProjectSecretTTL }
func (projectSecretSpec) cacheKeyPrefix() string            { return CacheKeyProjectSecretPrefix }
func (projectSecretSpec) fileNamePrefix() string            { return "project-" }
func (projectSecretSpec) idKey(id int64) string             { return strconv.FormatInt(id, 10) }
func (projectSecretSpec) formatID(id int64) string          { return strconv.FormatInt(id, 10) }
func (projectSecretSpec) invalidIDSuffix() string           { return "令牌 ID 无效" }
func (projectSecretSpec) skipInvalidInMigrate() bool        { return true }
func (projectSecretSpec) newRecord() (ProjectSecret, error) { return newProjectSecret() }

// valid 报告令牌 ID 是否合法。
func (projectSecretSpec) valid(id int64) bool { return id > 0 }

// newProjectSecret 生成一把新的项目令牌密钥。
func newProjectSecret() (ProjectSecret, error) {
	raw := make([]byte, projectSecretBytes)
	if _, err := rand.Read(raw); err != nil {
		return ProjectSecret{}, newSecretError(projectSecretSpec{}.label(), err)
	}
	return ProjectSecret{
		Secret:    base64.RawURLEncoding.EncodeToString(raw),
		CreatedAt: time.Now().UTC(),
	}, nil
}
