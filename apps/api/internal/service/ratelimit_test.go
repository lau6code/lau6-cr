package service

import (
	"context"
	"testing"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/cache"
)

// TestRateLimiterBlocksOverLimit 覆盖"超限被拒"。
func TestRateLimiterBlocksOverLimit(t *testing.T) {
	c := cache.NewLocal(100)
	defer c.Close()
	rl := NewRateLimiter(
		func() cache.Limiter { return cache.LimiterOf(c) },
		func(RateLimitScope) int { return 2 },
		nil,
	)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		allowed, err := rl.Allow(ctx, RateLimitScopeToken, "1.2.3.4")
		if err != nil {
			t.Fatalf("限流检查: %v", err)
		}
		if !allowed {
			t.Fatalf("第 %d 次应放行", i+1)
		}
	}
	allowed, err := rl.Allow(ctx, RateLimitScopeToken, "1.2.3.4")
	if err != nil {
		t.Fatalf("限流检查: %v", err)
	}
	if allowed {
		t.Fatal("超限后不该放行")
	}
	// 换个 IP → 独立额度。
	allowed, err = rl.Allow(ctx, RateLimitScopeToken, "5.6.7.8")
	if err != nil {
		t.Fatalf("限流检查: %v", err)
	}
	if !allowed {
		t.Fatal("不同来源应当各算各的")
	}
}

// TestRateLimiterScopesAreIndependent 覆盖"不同用途各算各的"。
func TestRateLimiterScopesAreIndependent(t *testing.T) {
	c := cache.NewLocal(100)
	defer c.Close()
	rl := NewRateLimiter(
		func() cache.Limiter { return cache.LimiterOf(c) },
		func(RateLimitScope) int { return 1 },
		nil,
	)
	ctx := context.Background()

	if allowed, _ := rl.Allow(ctx, RateLimitScopeToken, "1.2.3.4"); !allowed {
		t.Fatal("token 桶第一次应放行")
	}
	if allowed, _ := rl.Allow(ctx, RateLimitScopeToken, "1.2.3.4"); allowed {
		t.Fatal("token 桶超限")
	}
	// read 桶是另一个桶，不受 token 桶影响。
	if allowed, _ := rl.Allow(ctx, RateLimitScopeRead, "1.2.3.4"); !allowed {
		t.Fatal("read 桶第一次应放行")
	}
}

// TestRateLimiterZeroLimitMeansUnlimited 覆盖"limit<=0 = 不限流"。
func TestRateLimiterZeroLimitMeansUnlimited(t *testing.T) {
	c := cache.NewLocal(100)
	defer c.Close()
	rl := NewRateLimiter(
		func() cache.Limiter { return cache.LimiterOf(c) },
		func(RateLimitScope) int { return 0 },
		nil,
	)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		allowed, err := rl.Allow(ctx, RateLimitScopeToken, "1.2.3.4")
		if err != nil || !allowed {
			t.Fatalf("不限流时应一直放行（allowed=%v err=%v）", allowed, err)
		}
	}
}

// TestRateLimiterUnwiredFails 覆盖"限流器没接线 → 报错，不放行"。
//
// ⚠️ 这条最关键：没接线却当成"没超限"放行，等于限流是假的。
func TestRateLimiterUnwiredFails(t *testing.T) {
	rl := NewRateLimiter(nil, func(RateLimitScope) int { return 10 }, nil)
	allowed, err := rl.Allow(context.Background(), RateLimitScopeToken, "1.2.3.4")
	if err == nil {
		t.Fatal("没接线时必须报错")
	}
	if allowed {
		t.Fatal("没接线时不能放行")
	}
}

// TestRateLimiterBackendWithoutLimiterFails 覆盖"后端不支持限流 → 报错"。
func TestRateLimiterBackendWithoutLimiterFails(t *testing.T) {
	rl := NewRateLimiter(
		func() cache.Limiter { return nil },
		func(RateLimitScope) int { return 10 },
		nil,
	)
	allowed, err := rl.Allow(context.Background(), RateLimitScopeRead, "1.2.3.4")
	if err == nil {
		t.Fatal("后端不支持限流时必须报错")
	}
	if allowed {
		t.Fatal("后端不支持限流时不能放行")
	}
}

// TestLoginGuardLocksAfterFailures 覆盖"连续失败到阈值 → 锁定"。
func TestLoginGuardLocksAfterFailures(t *testing.T) {
	c := cache.NewLocal(100)
	defer c.Close()
	g := NewLoginGuard(
		func() cache.Cache { return c },
		func() int { return 3 },
		func() time.Duration { return 5 * time.Minute },
		nil,
	)
	ctx := context.Background()

	// 未锁定时 Check 通过。
	if err := g.Check(ctx, "alice", "1.2.3.4"); err != nil {
		t.Fatalf("不该锁定: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := g.RecordFailure(ctx, "alice", "1.2.3.4"); err != nil {
			t.Fatalf("记录失败: %v", err)
		}
	}
	if err := g.Check(ctx, "alice", "1.2.3.4"); err == nil {
		t.Fatal("达到阈值后应当锁定")
	}
}

// TestLoginGuardSuccessResets 覆盖"登录成功清零"。
func TestLoginGuardSuccessResets(t *testing.T) {
	c := cache.NewLocal(100)
	defer c.Close()
	g := NewLoginGuard(
		func() cache.Cache { return c },
		func() int { return 2 },
		func() time.Duration { return time.Minute },
		nil,
	)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		if err := g.RecordFailure(ctx, "alice", "1.2.3.4"); err != nil {
			t.Fatalf("记录失败: %v", err)
		}
	}
	if err := g.Check(ctx, "alice", "1.2.3.4"); err == nil {
		t.Fatal("达到阈值后应当锁定")
	}
	if err := g.RecordSuccess(ctx, "alice", "1.2.3.4"); err != nil {
		t.Fatalf("清零: %v", err)
	}
	if err := g.Check(ctx, "alice", "1.2.3.4"); err != nil {
		t.Fatalf("登录成功后不该还锁着: %v", err)
	}
}

// TestLoginGuardDimensionsAreIndependent 覆盖"账号维度与来源维度各锁各的"。
func TestLoginGuardDimensionsAreIndependent(t *testing.T) {
	c := cache.NewLocal(100)
	defer c.Close()
	g := NewLoginGuard(
		func() cache.Cache { return c },
		func() int { return 2 },
		func() time.Duration { return time.Minute },
		nil,
	)
	ctx := context.Background()

	// 同一个账号失败两次 → 账号维度锁定。
	for i := 0; i < 2; i++ {
		if err := g.RecordFailure(ctx, "alice", "1.2.3.4"); err != nil {
			t.Fatalf("记录失败: %v", err)
		}
	}
	if err := g.Check(ctx, "alice", "1.2.3.4"); err == nil {
		t.Fatal("账号维度应当锁定")
	}
	// 另一个账号、同一个 IP：IP 维度也被锁了（双维度）。
	if err := g.Check(ctx, "bob", "1.2.3.4"); err == nil {
		t.Fatal("来源维度也应当锁定")
	}
	// 另一个 IP、同一个账号：账号维度仍然锁着。
	if err := g.Check(ctx, "alice", "5.6.7.8"); err == nil {
		t.Fatal("账号维度应当仍然锁定")
	}
	// 另一个 IP + 另一个账号 → 都不锁。
	if err := g.Check(ctx, "bob", "5.6.7.8"); err != nil {
		t.Fatalf("无关的账号与来源不该被锁: %v", err)
	}
}
