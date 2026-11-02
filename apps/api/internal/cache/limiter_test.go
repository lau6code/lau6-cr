package cache

import (
	"context"
	"testing"
	"time"
)

// TestLocalTakeConsumesTokens 覆盖"令牌桶会真的扣令牌"。
func TestLocalTakeConsumesTokens(t *testing.T) {
	l := NewLocal(100)
	defer l.Close()
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		allowed, err := l.Take(ctx, "k", 3, time.Minute)
		if err != nil {
			t.Fatalf("取令牌: %v", err)
		}
		if !allowed {
			t.Fatalf("第 %d 次取令牌应该放行", i+1)
		}
	}
	// 桶空了 → 超限。
	allowed, err := l.Take(ctx, "k", 3, time.Minute)
	if err != nil {
		t.Fatalf("取令牌: %v", err)
	}
	if allowed {
		t.Fatal("桶空之后不该再放行")
	}
}

// TestLocalTakeRefillsOverTime 覆盖"桶会随时间匀速补充"。
func TestLocalTakeRefillsOverTime(t *testing.T) {
	l := NewLocal(100)
	defer l.Close()
	ctx := context.Background()

	// 容量 1，立刻用掉。
	if allowed, err := l.Take(ctx, "k", 1, 40*time.Millisecond); err != nil || !allowed {
		t.Fatalf("第一次取令牌应放行（allowed=%v err=%v）", allowed, err)
	}
	if allowed, err := l.Take(ctx, "k", 1, 40*time.Millisecond); err != nil || allowed {
		t.Fatalf("桶空时不该放行（allowed=%v err=%v）", allowed, err)
	}
	// 等一个补充周期 → 恢复一个令牌。
	time.Sleep(60 * time.Millisecond)
	if allowed, err := l.Take(ctx, "k", 1, 40*time.Millisecond); err != nil || !allowed {
		t.Fatalf("补充后应放行（allowed=%v err=%v）", allowed, err)
	}
}

// TestLocalResetClearsBucket 覆盖"Reset 清空额度"。
func TestLocalResetClearsBucket(t *testing.T) {
	l := NewLocal(100)
	defer l.Close()
	ctx := context.Background()

	if allowed, _ := l.Take(ctx, "k", 1, time.Minute); !allowed {
		t.Fatal("第一次应放行")
	}
	if allowed, _ := l.Take(ctx, "k", 1, time.Minute); allowed {
		t.Fatal("桶空时不该放行")
	}
	if err := l.Reset(ctx, "k"); err != nil {
		t.Fatalf("重置: %v", err)
	}
	if allowed, _ := l.Take(ctx, "k", 1, time.Minute); !allowed {
		t.Fatal("重置后应放行")
	}
}

// TestTakeRejectsBadParams 覆盖"参数非法要报错，不能默默放行"。
func TestTakeRejectsBadParams(t *testing.T) {
	l := NewLocal(100)
	defer l.Close()
	ctx := context.Background()

	if _, err := l.Take(ctx, "k", 0, time.Minute); err == nil {
		t.Fatal("limit<=0 应报错")
	}
	if _, err := l.Take(ctx, "k", 10, 0); err == nil {
		t.Fatal("window<=0 应报错")
	}
}

// TestLimiterOfFindsLimiter 覆盖"后端能自报限流器"。
func TestLimiterOfFindsLimiter(t *testing.T) {
	l := NewLocal(100)
	defer l.Close()
	if LimiterOf(l) == nil {
		t.Fatal("Local 应当自带限流器")
	}
}
