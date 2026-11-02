package crypto

import (
	"context"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestKeystoreLocalUsesFile 覆盖 local 模式：文件是权威源。
//
// local 是进程内的，进程一没就没了——不能当"启动源"，
// 那等于每次重启都换密钥（全体登出）。
func TestKeystoreLocalUsesFile(t *testing.T) {
	dir := t.TempDir()
	privPath := filepath.Join(dir, "priv.pem")
	jwksPath := filepath.Join(dir, "jwks.json")

	ks, err := NewKeystore(Options{PrivatePath: privPath, JWKSPath: jwksPath})
	if err != nil {
		t.Fatalf("构造密钥库: %v", err)
	}
	if _, err := os.Stat(privPath); err != nil {
		t.Fatalf("私钥文件没写出来: %v", err)
	}

	// 再建一次：必须拿到**同一把**密钥。
	ks2, err := NewKeystore(Options{PrivatePath: privPath, JWKSPath: jwksPath})
	if err != nil {
		t.Fatalf("第二次构造: %v", err)
	}
	if ks.KID() != ks2.KID() {
		t.Fatalf("重启后密钥变了：%s → %s", ks.KID(), ks2.KID())
	}
}

// TestKeystoreRedisCacheIsSource 覆盖 redis 模式：缓存是权威源。
//
// §3.3：缓存里有 → 载入（**不读文件**）；没有 → 生成并写回缓存。
func TestKeystoreRedisCacheIsSource(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	privPath := filepath.Join(dir, "priv.pem")
	jwksPath := filepath.Join(dir, "jwks.json")
	c := newFakeRedis()

	ks, err := NewKeystore(Options{PrivatePath: privPath, JWKSPath: jwksPath, Cache: c})
	if err != nil {
		t.Fatalf("构造密钥库: %v", err)
	}

	// 私钥必须在缓存里，且**不设 TTL**（设 TTL = 给"全体登出"埋雷）。
	raw, ok, err := c.Get(ctx, CacheKeySigningKey)
	if err != nil || !ok {
		t.Fatalf("缓存里没有签名密钥: ok=%v err=%v", ok, err)
	}
	priv, err := parsePrivateKey(raw, "缓存")
	if err != nil {
		t.Fatalf("缓存里的私钥解析失败: %v", err)
	}
	if got := JWKThumbprint(priv.Public().(ed25519.PublicKey)); got != ks.KID() {
		t.Fatalf("缓存里的私钥与内存中的不一致：%s → %s", ks.KID(), got)
	}

	// JWKS 也要进缓存（TTL 15 分钟）。
	if _, ok, err := c.Get(ctx, CacheKeyJWKS); err != nil || !ok {
		t.Fatalf("缓存里没有 JWKS: ok=%v err=%v", ok, err)
	}
}

// TestKeystoreRedisReadsFromCacheNotFile 覆盖"缓存里有就别读文件"。
//
// 这是 §3.3 的关键：redis 模式下**文件不是启动源**。
// 文件里放一把不一样的密钥，缓存里的那把才是权威的。
func TestKeystoreRedisReadsFromCacheNotFile(t *testing.T) {
	dir := t.TempDir()
	privPath := filepath.Join(dir, "priv.pem")
	jwksPath := filepath.Join(dir, "jwks.json")
	c := newFakeRedis()

	// 先建一个密钥库（生成到文件+缓存）。
	first, err := NewKeystore(Options{PrivatePath: privPath, JWKSPath: jwksPath, Cache: c})
	if err != nil {
		t.Fatalf("首次构造: %v", err)
	}

	// 把文件换成**另一把**密钥，缓存里保持第一把。
	other, err := generatePrivateKey()
	if err != nil {
		t.Fatalf("生成对照密钥: %v", err)
	}
	pemBytes, err := encodePrivateKey(other)
	if err != nil {
		t.Fatalf("编码对照密钥: %v", err)
	}
	if err := os.WriteFile(privPath, pemBytes, 0o600); err != nil {
		t.Fatalf("改写文件: %v", err)
	}

	second, err := NewKeystore(Options{PrivatePath: privPath, JWKSPath: jwksPath, Cache: c})
	if err != nil {
		t.Fatalf("二次构造: %v", err)
	}
	if second.KID() != first.KID() {
		t.Fatalf("redis 模式下应当以缓存为准：文件 %s，缓存 %s", second.KID(), first.KID())
	}
}

// TestKeystoreFailsClosedWhenCacheUnreachable 覆盖缓存不可达 → **启动失败**。
//
// 降级成"连不上就生成新密钥"会把一次缓存抖动放大成全体登出——
// 正确做法是启动失败并重试，让容器编排来兜。
func TestKeystoreFailsClosedWhenCacheUnreachable(t *testing.T) {
	dir := t.TempDir()
	broken := &brokenCache{driver: "redis"}

	_, err := NewKeystore(Options{
		PrivatePath: filepath.Join(dir, "priv.pem"),
		JWKSPath:    filepath.Join(dir, "jwks.json"),
		Cache:       broken,
	})
	if err == nil {
		t.Fatal("缓存不可达时必须启动失败，不能降级成生成新密钥")
	}
}

// TestSeedCacheKeepsSameKey 覆盖换后端时迁移密钥。
//
// 迁移前后必须是同一把密钥——否则等于换了一次密钥（全体登出）。
func TestSeedCacheKeepsSameKey(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	ks, err := NewKeystore(Options{
		PrivatePath: filepath.Join(dir, "priv.pem"),
		JWKSPath:    filepath.Join(dir, "jwks.json"),
	})
	if err != nil {
		t.Fatalf("构造密钥库: %v", err)
	}

	fresh := newFakeRedis()
	if err := ks.SeedCache(ctx, fresh); err != nil {
		t.Fatalf("SeedCache: %v", err)
	}
	raw, ok, err := fresh.Get(ctx, CacheKeySigningKey)
	if err != nil || !ok {
		t.Fatalf("迁移后缓存里没有密钥: ok=%v err=%v", ok, err)
	}
	priv, err := parsePrivateKey(raw, "缓存")
	if err != nil {
		t.Fatalf("解析迁移过来的私钥: %v", err)
	}
	if got := JWKThumbprint(priv.Public().(ed25519.PublicKey)); got != ks.KID() {
		t.Fatalf("迁移后密钥变了：%s → %s", ks.KID(), got)
	}
}

// TestJWKSHasTTL 覆盖 JWKS 的 TTL 是 15 分钟。
//
// TTL 必须 > token TTL（300s），取 3× 余量（§4.2）。
func TestJWKSHasTTL(t *testing.T) {
	if jwksTTL != 15*time.Minute {
		t.Fatalf("jwksTTL = %v, 期望 15 分钟", jwksTTL)
	}
}

// brokenCache 是一个"连不上"的 Redis 后端（所有读都失败）。
type brokenCache struct {
	driver string
}

func (b *brokenCache) Get(context.Context, string) ([]byte, bool, error) {
	return nil, false, errCacheBroken{}
}
func (b *brokenCache) Set(context.Context, string, []byte, time.Duration) error {
	return errCacheBroken{}
}
func (b *brokenCache) SetNX(context.Context, string, []byte, time.Duration) (bool, error) {
	return false, errCacheBroken{}
}
func (b *brokenCache) Delete(context.Context, string) error { return errCacheBroken{} }
func (b *brokenCache) Close() error                         { return nil }
func (b *brokenCache) Driver() string                       { return b.driver }

type errCacheBroken struct{}

func (errCacheBroken) Error() string { return "cache: 连接失败" }

// fakeRedis 是"redis 语义"的内存缓存替身。
//
// 为什么不用 cache.NewLocal：local 是进程内的，进程一没就没了——
// 不能当签名密钥的启动源（§3.3 只让 redis 当权威源）。
// 但单元测试不该依赖真 Redis，所以这里做一个 Driver() 返回 "redis" 的替身。
type fakeRedis struct {
	mu     sync.Mutex
	items  map[string][]byte
	broken bool
}

func newFakeRedis() *fakeRedis {
	return &fakeRedis{items: map[string][]byte{}}
}

func (f *fakeRedis) Get(_ context.Context, key string) ([]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.broken {
		return nil, false, errCacheBroken{}
	}
	v, ok := f.items[key]
	return v, ok, nil
}

func (f *fakeRedis) Set(_ context.Context, key string, value []byte, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.broken {
		return errCacheBroken{}
	}
	f.items[key] = value
	return nil
}

func (f *fakeRedis) SetNX(_ context.Context, key string, value []byte, _ time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.broken {
		return false, errCacheBroken{}
	}
	if _, ok := f.items[key]; ok {
		return false, nil
	}
	f.items[key] = value
	return true, nil
}

func (f *fakeRedis) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.items, key)
	return nil
}

func (f *fakeRedis) Close() error { return nil }

func (f *fakeRedis) Driver() string { return "redis" }
