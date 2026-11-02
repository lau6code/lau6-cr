package cache

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// 限流原语
//
// 限流用**令牌桶**：桶容量 = limit，每 window 匀速补满一个。
//
// 为什么桶要放在缓存后端里：redis 模式下多个副本必须共享同一个桶，
// 否则限额会被放大 N 倍（`12-cache.md` §7）。
//
// 为什么单独一个接口而不是塞进 Cache：限流是"后端的能力"，
// 不是"缓存的读法"——单独一个接口不必让每个 Cache 实现都跟着实现它
//（测试替身也是实现者）。

// Limiter 是令牌桶限流器。
type Limiter interface {
	// Take 从 key 的桶里取一个令牌。
	//
	// 返回 false = 超限（调用方应回 429）。
	// 返回 error = 后端不可用——调用方**必须**把失败暴露出去，
	// 不能当成"没超限"放行（那等于限流形同虚设，而且没人知道限流器坏了）。
	Take(ctx context.Context, key string, limit int, window time.Duration) (bool, error)
	// Reset 清空某个桶（额度恢复的场景，如登录成功）。
	Reset(ctx context.Context, key string) error
}

// LimiterOf 返回后端自带的限流器。
//
// 返回 nil 表示该后端不支持限流——调用方必须处理，
// 不能当成"不限流"。
func LimiterOf(c Cache) Limiter {
	if l, ok := c.(Limiter); ok {
		return l
	}
	return nil
}

// 本地实现

// localBucket 是一个令牌桶。
type localBucket struct {
	// tokens 是当前余额。
	tokens float64
	// last 是上次补充的时刻。
	last time.Time
	// window 是补充周期（用于判断这个桶该不该被回收）。
	window time.Duration
}

// Local 的限流实现：进程内桶 + 独立的锁。
//
// ⚠️ 锁与缓存 items 的锁**分开**：限流在每个请求的热路径上，
// 跟缓存读抢同一把锁会把所有镜像请求串起来。
func (l *Local) Take(_ context.Context, key string, limit int, window time.Duration) (bool, error) {
	if limit <= 0 || window <= 0 {
		return false, fmt.Errorf("限流参数非法: limit=%d window=%s", limit, window)
	}
	now := time.Now()
	l.bucketMu.Lock()
	defer l.bucketMu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		b = &localBucket{tokens: float64(limit), last: now, window: window}
		l.buckets[key] = b
	}
	// 匀速补充：过了多久就补多少，最多补满。
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens += elapsed.Seconds() * float64(limit) / window.Seconds()
		if b.tokens > float64(limit) {
			b.tokens = float64(limit)
		}
		b.last = now
	}
	if b.tokens < 1 {
		return false, nil
	}
	b.tokens--
	return true, nil
}

// Reset 清空某个桶。
func (l *Local) Reset(_ context.Context, key string) error {
	l.bucketMu.Lock()
	defer l.bucketMu.Unlock()
	delete(l.buckets, key)
	return nil
}

// evictBuckets 回收长期不用的桶——桶的键来自 IP，不回收就是内存泄漏。
func (l *Local) evictBuckets(now time.Time) {
	l.bucketMu.Lock()
	defer l.bucketMu.Unlock()
	for k, b := range l.buckets {
		if b.window > 0 && now.Sub(b.last) > 2*b.window {
			delete(l.buckets, k)
		}
	}
}

// Redis 实现

// takeTokenScript 是 Redis 侧的令牌桶脚本。
//
// ⚠️ 必须在 Lua 里做（原子）：读-改-写分成多条命令时，
// 并发请求会同时读到同一个余额、一起放行——限额就漏了。
//
// 时间取 Redis 自己的 TIME：**不用各副本的本地时钟**，
// 否则时钟漂移会让补充速率算错。
var takeTokenScript = redis.NewScript(`
local cap = tonumber(ARGV[1])
local win = tonumber(ARGV[2])
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
local data = redis.call('HMGET', KEYS[1], 'tokens', 'ts')
local tokens = tonumber(data[1])
local ts = tonumber(data[2])
if tokens == nil then tokens = cap end
if ts == nil then ts = now end
local elapsed = now - ts
if elapsed < 0 then elapsed = 0 end
tokens = tokens + (elapsed * cap / win)
if tokens > cap then tokens = cap end
local allowed = 0
if tokens >= 1 then
  tokens = tokens - 1
  allowed = 1
end
redis.call('HSET', KEYS[1], 'tokens', tokens, 'ts', now)
redis.call('PEXPIRE', KEYS[1], win * 2)
return allowed
`)

// Take 从 Redis 侧的桶里取一个令牌。
func (r *Redis) Take(ctx context.Context, key string, limit int, window time.Duration) (bool, error) {
	if limit <= 0 || window <= 0 {
		return false, fmt.Errorf("限流参数非法: limit=%d window=%s", limit, window)
	}
	res, err := takeTokenScript.Run(ctx, r.client, []string{r.key(key)},
		limit, window.Milliseconds()).Int64()
	if err != nil {
		return false, fmt.Errorf("取限流令牌失败: %w", err)
	}
	return res == 1, nil
}

// Reset 清空某个桶。
func (r *Redis) Reset(ctx context.Context, key string) error {
	if err := r.client.Del(ctx, r.key(key)).Err(); err != nil {
		return fmt.Errorf("清限流桶失败: %w", err)
	}
	return nil
}

// bucketMap 初始化用的小工具（Local 构造时调用）。
func newBucketMap() map[string]*localBucket {
	return make(map[string]*localBucket)
}
