package crypto

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/cache"
)

const (
	// CacheKeyUserSecretPrefix 是用户签名密钥的缓存键前缀。
	//
	// 键名 = 前缀 + 用户名。用户名来自账户表（见 store.DB.ListUsers），
	// 不是外部输入，但仍要经过用户名校验，避免键名里出现控制字符。
	CacheKeyUserSecretPrefix = "cr:user-secret:"

	// DefaultUserSecretTTL 是用户密钥与用户令牌的有效期（30 天）。
	//
	// 口径与参考实现 apc 一致（secret TTL 30 天、令牌 exp 30 天）：
	// 密钥到期后该用户**所有**未过期令牌一起作废，需要重新签发。
	DefaultUserSecretTTL = 30 * 24 * time.Hour

	// userSecretBytes 是用户密钥的字节数。
	//
	// ⚠️ 参考实现 apc 用的是 11 位随机字母数字（约 65 bit 熵）。
	//    那是 HMAC-SHA256 的密钥，熵低于哈希输出长度（256 bit）时
	//    抗离线爆破能力偏弱。这里按本仓库的密钥标准取 256 bit。
	userSecretBytes = 32
)

// UserSecret 是一个用户的签名密钥记录。
//
// 有效期从**创建时刻**起算，不因读取而续期——
// 与 apc 的口径一致：密钥 30 天后自然作废，令牌随之失效。
type UserSecret struct {
	// Secret 是 HMAC 密钥（base64url，无填充）。
	Secret string `json:"secret"`
	// CreatedAt 是密钥的创建时刻（UTC）。
	CreatedAt time.Time `json:"created_at"`
}

func (u UserSecret) Expired(ttl time.Duration) bool {
	return time.Since(u.CreatedAt) >= ttl
}

// secretValue 返回 HMAC 密钥（供泛型实现读取，不进导出 API）。
func (u UserSecret) secretValue() string { return u.Secret }

// createdAt 返回密钥的创建时刻（供泛型实现读取，不进导出 API）。
func (u UserSecret) createdAt() time.Time { return u.CreatedAt }

// UserSecretStore 管理**每个用户自己的** JWT 签名密钥。
//
// 存放策略与实例签名密钥同构（docs/design/12-cache.md §3.3）：
//
//	cache.driver=redis → 缓存（Redis）是权威源，键 cr:user-secret:<username>
//	cache.driver=local  → 文件是权威源，<dir>/<sha256(username)>.key
//
// 两种后端的语义完全一致：一条记录 = {密钥, 创建时间}。
//
// ⚠️ 与实例密钥的差别：实例密钥是**启动源**（启动时读一次，常驻内存），
//
//	用户密钥是**请求路径上的数据**（每个令牌都要用它验签），
//	所以它必须能按用户增删查，且能整体搬迁（换缓存后端时）。
type UserSecretStore struct {
	*secretStore[string, UserSecret]
}

// UserSecretOptions 是 UserSecretStore 的构造参数。
type UserSecretOptions struct {
	// Cache 是缓存后端持有者（热切换后取新后端用）。
	// 传 nil 时只能走文件后端。
	Cache *cache.Holder
	// Dir 是 local 模式下用户密钥文件的存放目录。
	Dir string
	// TTL 是用户密钥（与用户令牌）的有效期。
	// 传 nil 时用 DefaultUserSecretTTL。
	TTL func() time.Duration
}

// NewUserSecretStore 构造用户密钥库。
func NewUserSecretStore(o UserSecretOptions) *UserSecretStore {
	return &UserSecretStore{
		secretStore: newSecretStore[string, UserSecret](o.Cache, o.Dir, o.TTL, userSecretSpec{}),
	}
}

// Get 读取用户密钥。密钥不存在或已过期时返回 ok=false。
//
// 读失败**必须报错**，不能当成"没有密钥"——
// 那会让一枚本该有效的令牌被静默判成无效。
func (s *UserSecretStore) Get(ctx context.Context, username string) (UserSecret, bool, error) {
	return s.secretStore.Get(ctx, username)
}

// Ensure 读取用户密钥；没有就生成一把并写回去。
//
// 并发安全：两个请求同时来只有一个会真正写入，
// 另一个拿到先写好的那份——否则后写的会覆盖先写的，
// 先前签发的令牌会突然验签失败。
func (s *UserSecretStore) Ensure(ctx context.Context, username string) (UserSecret, error) {
	return s.secretStore.Ensure(ctx, username)
}

// Revoke 吊销用户密钥。
//
// 删掉之后该用户**所有**未过期令牌立刻作废（验签时找不到密钥），
// 下次签发会生成一把新的。
func (s *UserSecretStore) Revoke(ctx context.Context, username string) error {
	return s.secretStore.Revoke(ctx, username)
}

// Migrate 把用户密钥从旧后端搬到新后端（换缓存后端时用）。
//
// 有效期**按剩余寿命**搬，不重新给满 TTL：
// 重新给满等于把"30 天过期"变成"每换一次后端就续命 30 天"。
//
// usernames 来自账户表——用户密钥只在用户存在时才有意义，
// 删号后残留的密钥没有搬迁价值。
func (s *UserSecretStore) Migrate(ctx context.Context, from, to cache.Cache, usernames []string) (int, error) {
	return s.secretStore.Migrate(ctx, from, to, usernames)
}

// TTL 返回用户密钥的有效期。
func (s *UserSecretStore) TTL() time.Duration {
	return s.secretStore.TTL()
}

// userSecretSpec 是用户密钥与项目令牌密钥的差异点。
type userSecretSpec struct{}

func (userSecretSpec) label() string                  { return "用户密钥" }
func (userSecretSpec) defaultTTL() time.Duration      { return DefaultUserSecretTTL }
func (userSecretSpec) cacheKeyPrefix() string         { return CacheKeyUserSecretPrefix }
func (userSecretSpec) fileNamePrefix() string         { return "" }
func (userSecretSpec) idKey(id string) string         { return id }
func (userSecretSpec) formatID(id string) string      { return id }
func (userSecretSpec) invalidIDSuffix() string        { return "用户名为空" }
func (userSecretSpec) skipInvalidInMigrate() bool     { return false }
func (userSecretSpec) newRecord() (UserSecret, error) { return newUserSecret() }

// valid 报告用户名是否合法。
func (userSecretSpec) valid(id string) bool { return strings.TrimSpace(id) != "" }

// newUserSecret 生成一把新的用户密钥。
func newUserSecret() (UserSecret, error) {
	raw := make([]byte, userSecretBytes)
	if _, err := rand.Read(raw); err != nil {
		return UserSecret{}, newSecretError(userSecretSpec{}.label(), err)
	}
	return UserSecret{
		Secret:    base64.RawURLEncoding.EncodeToString(raw),
		CreatedAt: time.Now().UTC(),
	}, nil
}
