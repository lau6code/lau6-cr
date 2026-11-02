package crypto

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/cache"
	"github.com/lau6code/lau6-cr/apps/api/internal/fsatomic"
)

// 「每个主体一把」的签名密钥库。
//
// 用户密钥（按用户名索引）与项目令牌密钥（按令牌 ID 索引）只有
// ID 类型、键名/路径拼法、错误文案、默认 TTL 不同，
// 存取语义完全一致——所以主体逻辑收在这里，差异点交给 secretSpec。

// secretRecord 是两类密钥记录的共同形状。
//
// 约束用未导出方法：不扩大导出 API，同时让泛型代码拿得到密钥内容与创建时刻。
type secretRecord interface {
	// secretValue 返回 HMAC 密钥。
	secretValue() string
	// createdAt 返回密钥的创建时刻。
	createdAt() time.Time
	// Expired 判断该记录是否已超过有效期。
	Expired(ttl time.Duration) bool
}

// secretSpec 是两类密钥的差异点。
type secretSpec[K comparable, R secretRecord] interface {
	// label 是错误文案里的名字（"用户密钥" / "项目令牌密钥"）。
	label() string
	// defaultTTL 是该类密钥的默认有效期。
	defaultTTL() time.Duration
	// cacheKeyPrefix 是 Redis 键前缀。
	cacheKeyPrefix() string
	// fileNamePrefix 是密钥文件名前缀（用户密钥为空）。
	fileNamePrefix() string
	// idKey 返回参与文件名哈希的 ID 文本。
	idKey(id K) string
	// formatID 返回错误文案里的 ID 文本。
	formatID(id K) string
	// valid 报告 ID 是否合法。
	valid(id K) bool
	// invalidIDSuffix 是"ID 非法"时的错误说明。
	invalidIDSuffix() string
	// skipInvalidInMigrate 报告搬迁时是否跳过非法 ID。
	skipInvalidInMigrate() bool
	// newRecord 生成一条新记录。
	newRecord() (R, error)
}

// secretStore 管理「每个主体一把」的签名密钥。
type secretStore[K comparable, R secretRecord] struct {
	holder *cache.Holder
	dir    string
	// ttl 取值是**函数**：设置页改了对应的 TTL 设置项后
	// 立即生效，不用重启容器（TierHot 的语义）。
	ttl  func() time.Duration
	spec secretSpec[K, R]
}

// newSecretStore 造一个密钥库。
func newSecretStore[K comparable, R secretRecord](holder *cache.Holder, dir string, ttl func() time.Duration, spec secretSpec[K, R]) *secretStore[K, R] {
	if ttl == nil {
		ttl = func() time.Duration { return spec.defaultTTL() }
	}
	return &secretStore[K, R]{holder: holder, dir: dir, ttl: ttl, spec: spec}
}

func (s *secretStore[K, R]) TTL() time.Duration {
	if s.ttl == nil {
		return s.spec.defaultTTL()
	}
	d := s.ttl()
	if d <= 0 {
		return s.spec.defaultTTL()
	}
	return d
}

// backend 返回当前生效的后端。
//
// 判定只看**缓存后端的类型**，不看配置：
// redis 模式 → 缓存权威；local 模式 → 文件权威。
func (s *secretStore[K, R]) backend() secretBackend[K, R] {
	// holder 为 nil 时（测试、CLI 子命令）走文件后端。
	var c cache.Cache
	if s.holder != nil {
		c = s.holder.Get()
	}
	return s.backendFor(c)
}

// backendFor 按给定的缓存后端选出密钥后端。
//
// 抽成函数是为了搬迁：换后端时要用**旧**后端读、**新**后端写。
func (s *secretStore[K, R]) backendFor(c cache.Cache) secretBackend[K, R] {
	if c != nil && c.Driver() == "redis" {
		return &redisSecretBackend[K, R]{cache: c, spec: s.spec}
	}
	return &fileSecretBackend[K, R]{dir: s.dir, ttl: s.TTL(), spec: s.spec}
}

// Get 读取密钥。密钥不存在或已过期时返回 ok=false。
//
// 读失败**必须报错**，不能当成"没有密钥"——
// 那会让一枚本该有效的令牌被静默判成无效。
func (s *secretStore[K, R]) Get(ctx context.Context, id K) (R, bool, error) {
	var zero R
	if !s.spec.valid(id) {
		return zero, false, errors.New("读取" + s.spec.label() + "：" + s.spec.invalidIDSuffix())
	}
	return s.backend().Get(ctx, id)
}

// Ensure 读取密钥；没有就生成一把并写回去。
//
// 并发安全：两个请求同时来只有一个会真正写入，
// 另一个拿到先写好的那份——否则后写的会覆盖先写的，
// 先前签发的令牌会突然验签失败。
func (s *secretStore[K, R]) Ensure(ctx context.Context, id K) (R, error) {
	var zero R
	if !s.spec.valid(id) {
		return zero, errors.New("生成" + s.spec.label() + "：" + s.spec.invalidIDSuffix())
	}
	b := s.backend()
	rec, ok, err := b.Get(ctx, id)
	if err != nil {
		return zero, fmt.Errorf("读取%s（%s）: %w", s.spec.label(), s.spec.formatID(id), err)
	}
	if ok {
		return rec, nil
	}

	fresh, err := s.spec.newRecord()
	if err != nil {
		return zero, err
	}
	created, err := b.Create(ctx, id, fresh, s.TTL())
	if err != nil {
		return zero, fmt.Errorf("写%s（%s）: %w", s.spec.label(), s.spec.formatID(id), err)
	}
	if created {
		return fresh, nil
	}

	// 并发：别的请求先写好了 → 用它那份，本地这把作废。
	rec, ok, err = b.Get(ctx, id)
	if err != nil {
		return zero, fmt.Errorf("读取%s（%s）: %w", s.spec.label(), s.spec.formatID(id), err)
	}
	if !ok {
		return zero, fmt.Errorf("%s（%s）写入后读不到", s.spec.label(), s.spec.formatID(id))
	}
	return rec, nil
}

// Revoke 吊销密钥。
//
// 删掉之后该主体换发出去的**所有** JWT 立刻作废（验签时找不到密钥），
// 下次签发会生成一把新的。
func (s *secretStore[K, R]) Revoke(ctx context.Context, id K) error {
	if !s.spec.valid(id) {
		return errors.New("吊销" + s.spec.label() + "：" + s.spec.invalidIDSuffix())
	}
	if err := s.backend().Delete(ctx, id); err != nil {
		return fmt.Errorf("吊销%s（%s）: %w", s.spec.label(), s.spec.formatID(id), err)
	}
	return nil
}

// Migrate 把密钥从旧后端搬到新后端（换缓存后端时用）。
//
// 有效期**按剩余寿命**搬，不重新给满 TTL：
// 重新给满等于把"30 天过期"变成"每换一次后端就续命 30 天"。
//
// ids 来自账户表/项目令牌表——主体不存在后残留的密钥没有搬迁价值。
func (s *secretStore[K, R]) Migrate(ctx context.Context, from, to cache.Cache, ids []K) (int, error) {
	src := s.backendFor(from)
	dst := s.backendFor(to)
	moved := 0
	for _, id := range ids {
		if s.spec.skipInvalidInMigrate() && !s.spec.valid(id) {
			continue
		}
		rec, ok, err := src.Get(ctx, id)
		if err != nil {
			return moved, fmt.Errorf("搬迁%s（读取 %s）: %w", s.spec.label(), s.spec.formatID(id), err)
		}
		if !ok {
			continue
		}
		remaining := s.TTL() - time.Since(rec.createdAt())
		if remaining <= 0 {
			// 已过期的密钥不必搬——搬过去也是一条立刻作废的记录。
			continue
		}
		if err := dst.Put(ctx, id, rec, remaining); err != nil {
			return moved, fmt.Errorf("搬迁%s（写入 %s）: %w", s.spec.label(), s.spec.formatID(id), err)
		}
		moved++
	}
	return moved, nil
}

// newSecretError 造"生成密钥失败"的错误（文案按类别区分）。
func newSecretError(label string, err error) error {
	return fmt.Errorf("生成%s: %w", label, err)
}

// secretBackend 是密钥的存取接口。
type secretBackend[K comparable, R secretRecord] interface {
	// Get 读取记录；不存在或已过期返回 ok=false。
	Get(ctx context.Context, id K) (R, bool, error)
	// Create 原子地写入记录。已存在时返回 created=false 且不覆盖。
	Create(ctx context.Context, id K, rec R, ttl time.Duration) (bool, error)
	// Put 写入记录（覆盖）。
	Put(ctx context.Context, id K, rec R, ttl time.Duration) error
	// Delete 删除记录；记录不存在不算错误。
	Delete(ctx context.Context, id K) error
}

// redis 后端

// redisSecretBackend 把密钥存在 Redis 里。
type redisSecretBackend[K comparable, R secretRecord] struct {
	cache cache.Cache
	spec  secretSpec[K, R]
}

func (b *redisSecretBackend[K, R]) key(id K) string {
	return b.spec.cacheKeyPrefix() + b.spec.formatID(id)
}

func (b *redisSecretBackend[K, R]) Get(ctx context.Context, id K) (R, bool, error) {
	var zero R
	raw, ok, err := b.cache.Get(ctx, b.key(id))
	if err != nil {
		return zero, false, fmt.Errorf("读 Redis: %w", err)
	}
	if !ok {
		return zero, false, nil
	}
	var rec R
	if err := json.Unmarshal(raw, &rec); err != nil {
		return zero, false, fmt.Errorf("解析%s: %w", b.spec.label(), err)
	}
	if rec.secretValue() == "" {
		return zero, false, fmt.Errorf("%s（%s）内容为空", b.spec.label(), b.spec.formatID(id))
	}
	return rec, true, nil
}

func (b *redisSecretBackend[K, R]) Create(ctx context.Context, id K, rec R, ttl time.Duration) (bool, error) {
	raw, err := json.Marshal(rec)
	if err != nil {
		return false, fmt.Errorf("序列化%s: %w", b.spec.label(), err)
	}
	ok, err := b.cache.SetNX(ctx, b.key(id), raw, ttl)
	if err != nil {
		return false, fmt.Errorf("写 Redis: %w", err)
	}
	return ok, nil
}

func (b *redisSecretBackend[K, R]) Put(ctx context.Context, id K, rec R, ttl time.Duration) error {
	raw, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("序列化%s: %w", b.spec.label(), err)
	}
	if err := b.cache.Set(ctx, b.key(id), raw, ttl); err != nil {
		return fmt.Errorf("写 Redis: %w", err)
	}
	return nil
}

func (b *redisSecretBackend[K, R]) Delete(ctx context.Context, id K) error {
	if err := b.cache.Delete(ctx, b.key(id)); err != nil {
		return fmt.Errorf("删 Redis: %w", err)
	}
	return nil
}

// 文件后端

// fileSecretBackend 把密钥存在文件里（local 缓存模式）。
//
// 文件名用 **sha256(ID)** 而不是 ID 本身：
// 用户名来自 LDAP 目录时可能是任意字符串，直接拼进路径等于开门迎路径穿越。
//
// 有效期存在记录里（created_at），读取时判断——
// 与 Redis 的原生 TTL 语义一致。
type fileSecretBackend[K comparable, R secretRecord] struct {
	dir  string
	ttl  time.Duration
	spec secretSpec[K, R]
}

func (b *fileSecretBackend[K, R]) path(id K) string {
	sum := sha256.Sum256([]byte(b.spec.idKey(id)))
	return filepath.Join(b.dir, b.spec.fileNamePrefix()+hex.EncodeToString(sum[:])+".key")
}

func (b *fileSecretBackend[K, R]) Get(_ context.Context, id K) (R, bool, error) {
	var zero R
	if b.dir == "" {
		return zero, false, errors.New(b.spec.label() + "目录未配置")
	}
	raw, err := os.ReadFile(b.path(id))
	if err != nil {
		if os.IsNotExist(err) {
			return zero, false, nil
		}
		return zero, false, fmt.Errorf("读%s文件: %w", b.spec.label(), err)
	}
	var rec R
	if err := json.Unmarshal(raw, &rec); err != nil {
		return zero, false, fmt.Errorf("解析%s文件: %w", b.spec.label(), err)
	}
	if rec.secretValue() == "" {
		return zero, false, fmt.Errorf("%s文件（%s）内容为空", b.spec.label(), b.spec.formatID(id))
	}
	if rec.Expired(b.ttl) {
		// 已过期：顺手删掉，避免过期记录一直堆着。
		//
		// ⚠️ 删除失败**不影响本次返回值**：过期密钥已作废
		//	（凭过期时间判定，不看文件是否还在），删除只是清理残留。
		//	但必须留痕——否则磁盘上多一把没人知道、也没人清理的残留密钥。
		//	文件已经不在（NotExist）说明残留已被清理，无需记录。
		if rmErr := os.Remove(b.path(id)); rmErr != nil && !os.IsNotExist(rmErr) {
			slog.Default().Error("删除过期密钥文件失败",
				"kind", b.spec.label(), "id", b.spec.formatID(id), "path", b.path(id), "err", rmErr)
		}
		return zero, false, nil
	}
	return rec, true, nil
}

func (b *fileSecretBackend[K, R]) Create(_ context.Context, id K, rec R, _ time.Duration) (bool, error) {
	if b.dir == "" {
		return false, errors.New(b.spec.label() + "目录未配置")
	}
	if err := os.MkdirAll(b.dir, 0o700); err != nil {
		return false, fmt.Errorf("创建%s目录: %w", b.spec.label(), err)
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return false, fmt.Errorf("序列化%s: %w", b.spec.label(), err)
	}
	// O_EXCL：文件已存在就说明别人先写好了，**不覆盖**。
	f, err := os.OpenFile(b.path(id), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("写%s文件: %w", b.spec.label(), err)
	}
	closed := false
	defer func() {
		if !closed {
			f.Close()
		}
	}()
	if _, err := f.Write(raw); err != nil {
		f.Close()
		closed = true
		// ⚠️ 必须清掉半截文件：O_EXCL 意味着下次重试会因为"文件已存在"
		//    直接返回 false——那个主体的密钥就**永远写不进去了**，
		//    而调用方看到的是"已存在，不是我的问题"。
		if rmErr := os.Remove(b.path(id)); rmErr != nil && !os.IsNotExist(rmErr) {
			return false, fmt.Errorf("写%s文件: %w（清理半截文件也失败: %v）", b.spec.label(), err, rmErr)
		}
		return false, fmt.Errorf("写%s文件: %w", b.spec.label(), err)
	}
	if err := f.Close(); err != nil {
		closed = true
		// 关闭失败 → 内容可能没落全，同样清掉。
		if rmErr := os.Remove(b.path(id)); rmErr != nil && !os.IsNotExist(rmErr) {
			return false, fmt.Errorf("关闭%s文件: %w（清理半截文件也失败: %v）", b.spec.label(), err, rmErr)
		}
		return false, fmt.Errorf("关闭%s文件: %w", b.spec.label(), err)
	}
	closed = true
	return true, nil
}

func (b *fileSecretBackend[K, R]) Put(_ context.Context, id K, rec R, _ time.Duration) error {
	if b.dir == "" {
		return errors.New(b.spec.label() + "目录未配置")
	}
	if err := os.MkdirAll(b.dir, 0o700); err != nil {
		return fmt.Errorf("创建%s目录: %w", b.spec.label(), err)
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("序列化%s: %w", b.spec.label(), err)
	}
	return fsatomic.Write(b.path(id), raw, 0o600)
}

func (b *fileSecretBackend[K, R]) Delete(_ context.Context, id K) error {
	if b.dir == "" {
		return errors.New(b.spec.label() + "目录未配置")
	}
	if err := os.Remove(b.path(id)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删%s文件: %w", b.spec.label(), err)
	}
	return nil
}
