// Package cache 提供进程内与 Redis 两种缓存后端。
package cache

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

// tlsConfig 是 Redis TLS 连接使用的配置。
//
// MinVersion 固定为 TLS 1.2 —— 低于此版本的 TLS 已被主流实现弃用。
var tlsConfig = tls.Config{MinVersion: tls.VersionTLS12}

// ErrNotFound 表示键不存在。
var ErrNotFound = errors.New("cache: key not found")

// Cache 是缓存的统一接口。
//
// 语义约定：
//   - Get 命中返回 (value, true, nil)；未命中返回 (nil, false, nil)。
//   - SetNX 用于分布式互斥与去重，键已存在时返回 (false, nil) 且**不覆盖**。
type Cache interface {
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	// SetNX 仅在键不存在时写入。返回 true 表示写入成功。
	SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error)
	Delete(ctx context.Context, key string) error
	Close() error
	// Driver 返回后端名（local / redis）。
	//
	// 有些决策依赖后端类型——最典型的是"Redis 模式下签名密钥以缓存为权威源"
	//（docs/design/12-cache.md §3.3）。
	// 不能靠类型断言去猜：那是把实现细节漏到调用方。
	Driver() string
}

// 本地实现

type localEntry struct {
	val     []byte
	expires time.Time
}

// Local 是进程内缓存。
type Local struct {
	mu         sync.RWMutex
	items      map[string]localEntry
	maxEntries int
	stopCh     chan struct{}
	once       sync.Once
	// bucketMu / buckets 是限流桶的独立状态（见 limiter.go）。
	//
	// ⚠️ 与 items 的锁**分开**：限流在每个请求的热路径上，
	// 共用一把锁会把所有镜像请求串起来。
	bucketMu sync.Mutex
	buckets  map[string]*localBucket
}

// NewLocal 创建本地缓存。maxEntries <= 0 时用默认 10000。
func NewLocal(maxEntries int) *Local {
	if maxEntries <= 0 {
		maxEntries = 10000
	}
	l := &Local{
		items:      make(map[string]localEntry),
		maxEntries: maxEntries,
		stopCh:     make(chan struct{}),
		buckets:    newBucketMap(),
	}
	go l.janitor()
	return l
}

func (l *Local) janitor() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			l.evictExpired()
			l.evictBuckets(time.Now())
		case <-l.stopCh:
			return
		}
	}
}

func (l *Local) evictExpired() {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, e := range l.items {
		if !e.expires.IsZero() && now.After(e.expires) {
			delete(l.items, k)
		}
	}
}

// Get 读取缓存。
func (l *Local) Get(_ context.Context, key string) ([]byte, bool, error) {
	l.mu.RLock()
	e, ok := l.items[key]
	l.mu.RUnlock()
	if !ok {
		return nil, false, nil
	}
	if !e.expires.IsZero() && time.Now().After(e.expires) {
		l.mu.Lock()
		delete(l.items, key)
		l.mu.Unlock()
		return nil, false, nil
	}
	return e.val, true, nil
}

// Set 写入缓存。ttl <= 0 表示不过期。
func (l *Local) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.setLocked(key, value, ttl)
	return nil
}

// SetNX 仅在键不存在时写入。
func (l *Local) SetNX(_ context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.items[key]; ok {
		if e.expires.IsZero() || time.Now().Before(e.expires) {
			return false, nil
		}
	}
	l.setLocked(key, value, ttl)
	return true, nil
}

// setLocked 写入一条缓存。调用方必须已持有 l.mu。
func (l *Local) setLocked(key string, value []byte, ttl time.Duration) {
	if len(l.items) >= l.maxEntries {
		l.evictSomeLocked(1)
	}
	var exp time.Time
	if ttl > 0 {
		exp = time.Now().Add(ttl)
	}
	l.items[key] = localEntry{val: value, expires: exp}
}

// Delete 删除键。
func (l *Local) Delete(_ context.Context, key string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.items, key)
	return nil
}

// Close 停止后台清理。
func (l *Local) Close() error {
	l.once.Do(func() { close(l.stopCh) })
	return nil
}

// Driver 返回后端名。
func (l *Local) Driver() string { return "local" }

// evictSomeLocked 淘汰若干条目（优先过期项，否则随机）。
func (l *Local) evictSomeLocked(n int) {
	now := time.Now()
	for k, e := range l.items {
		if n <= 0 {
			return
		}
		if !e.expires.IsZero() && now.After(e.expires) {
			delete(l.items, k)
			n--
		}
	}
	for k := range l.items {
		if n <= 0 {
			return
		}
		delete(l.items, k)
		n--
	}
}

// Redis 实现

// Redis 是基于 Redis 的缓存后端。
type Redis struct {
	client *redis.Client
	prefix string
}

// RedisOptions 是 Redis 连接参数。
type RedisOptions struct {
	Addr        string
	Password    string
	DB          int
	Prefix      string
	TLS         bool
	DialTimeout time.Duration
	ReadTimeout time.Duration
}

// NewRedis 建立 Redis 客户端（不立即连接，首次使用时才拨号）。
func NewRedis(o RedisOptions) *Redis {
	opts := &redis.Options{
		Addr:         o.Addr,
		Password:     o.Password,
		DB:           o.DB,
		DialTimeout:  o.DialTimeout,
		ReadTimeout:  o.ReadTimeout,
		WriteTimeout: o.ReadTimeout,
	}
	if o.TLS {
		opts.TLSConfig = &tlsConfig
	}
	if o.Prefix == "" {
		o.Prefix = "lau6-cr:"
	}
	return &Redis{client: redis.NewClient(opts), prefix: o.Prefix}
}

func (r *Redis) key(k string) string { return r.prefix + k }

// Driver 返回后端名。
func (r *Redis) Driver() string { return "redis" }

// Get 读取缓存。
func (r *Redis) Get(ctx context.Context, key string) ([]byte, bool, error) {
	b, err := r.client.Get(ctx, r.key(key)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return b, true, nil
}

// Set 写入缓存。
func (r *Redis) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return r.client.Set(ctx, r.key(key), value, ttl).Err()
}

// SetNX 仅在键不存在时写入。
func (r *Redis) SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	ok, err := r.client.SetNX(ctx, r.key(key), value, ttl).Result()
	return ok, err
}

// Delete 删除键。
func (r *Redis) Delete(ctx context.Context, key string) error {
	return r.client.Del(ctx, r.key(key)).Err()
}

// Close 关闭连接。
func (r *Redis) Close() error { return r.client.Close() }

// Ping 探活，用于 setup 时的连接检测。
func (r *Redis) Ping(ctx context.Context) error {
	return r.client.Ping(ctx).Err()
}

// 构造入口

// Options 决定用哪个后端。
type Options struct {
	Driver     string
	MaxEntries int
	Redis      RedisOptions
}

// New 按配置构造缓存。
func New(o Options) (Cache, error) {
	switch o.Driver {
	case "local", "":
		return NewLocal(o.MaxEntries), nil
	case "redis":
		if o.Redis.Addr == "" {
			return nil, fmt.Errorf("cache.redis.addr 在 driver=redis 时必填")
		}
		return NewRedis(o.Redis), nil
	}
	return nil, fmt.Errorf("未知的 cache.driver: %q", o.Driver)
}

// 运行期热切换

// Holder 持有当前生效的缓存后端，支持运行期热切换。
//
// 用 atomic.Pointer 而不是普通字段：读侧无锁、热切换无竞态。
// 换后端（local ↔ redis）时，旧后端要等在途请求用完再关，
// 否则正在跑的请求会拿到 "use of closed connection"——
// 所以 Swap 只换指针，关旧后端由调用方择时做。
type Holder struct {
	v atomic.Pointer[Cache]
}

// NewHolder 包装一个初始后端。
func NewHolder(c Cache) *Holder {
	h := &Holder{}
	h.v.Store(&c)
	return h
}

// Get 返回当前后端；未设置时返回 nil。
func (h *Holder) Get() Cache {
	p := h.v.Load()
	if p == nil {
		return nil
	}
	return *p
}

// Swap 换掉当前后端，返回**旧后端**（由调用方决定何时关）。
func (h *Holder) Swap(c Cache) Cache {
	old := h.Get()
	h.v.Store(&c)
	return old
}
