package service

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/cache"
)

// RateLimitScope 是限流用途——不同用途**各自计桶**。
type RateLimitScope string

const (
	// RateLimitScopeToken 是 `/token` 的匿名签发路径。
	RateLimitScopeToken RateLimitScope = "token"
	// RateLimitScopeRead 是匿名读路径。
	RateLimitScopeRead RateLimitScope = "read"
)

// RateLimiter 按来源 IP 做令牌桶限流。
//
// 桶存在缓存后端里：redis 模式下多副本**共享**同一个桶
// （限额不会被放大 N 倍），local 模式下每个副本各算各的
// （`12-cache.md` §7 已知的多副本限制）。
type RateLimiter struct {
	// limiter 取当前生效的缓存后端的限流器（热切换后自动跟随）。
	limiter func() cache.Limiter
	// limit 按 scope 返回上限（T0 热生效，读设置快照）。
	limit  func(RateLimitScope) int
	logger *slog.Logger
}

// NewRateLimiter 构造限流器。
//
// limiter 返回 nil（后端不支持限流）时，Allow 会**报错**而不是放行——
// 限流器没接线却当"不限流"处理，等于这个开关是假的。
func NewRateLimiter(
	limiter func() cache.Limiter,
	limit func(RateLimitScope) int,
	logger *slog.Logger,
) *RateLimiter {
	return &RateLimiter{limiter: limiter, limit: limit, logger: logger}
}

// scope 区分用途，不同用途各自计桶。
// limit <= 0 = 不限流（放行）。
//
// ⚠️ 后端不可用时返回 error：调用方必须把失败暴露出去。
//
//	悄悄放行 = 限流形同虚设，而且没人知道限流器已经坏了。
func (l *RateLimiter) Allow(ctx context.Context, scope RateLimitScope, ip string) (bool, error) {
	if l == nil || l.limit == nil {
		return false, fmt.Errorf("限流器未接线（scope=%s）", scope)
	}
	limit := l.limit(scope)
	if limit <= 0 {
		return true, nil
	}
	if l.limiter == nil {
		return false, fmt.Errorf("限流器未接线（scope=%s）", scope)
	}
	lim := l.limiter()
	if lim == nil {
		return false, fmt.Errorf("当前缓存后端不支持限流（scope=%s）", scope)
	}
	allowed, err := lim.Take(ctx, RateLimitKey(scope, ip), limit, time.Minute)
	if err != nil {
		return false, fmt.Errorf("限流检查失败（scope=%s）: %w", scope, err)
	}
	if !allowed && l.logger != nil {
		l.logger.Warn("触发限流", "scope", scope, "limit", limit)
	}
	return allowed, nil
}

// RateLimitKey 生成限流桶的键。
//
// 键里带 scope：不同用途的额度不能互相影响。
func RateLimitKey(scope RateLimitScope, ip string) string {
	return "cr:rl:" + string(scope) + ":" + ip
}
