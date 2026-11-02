package service

import (
	"context"
	"strings"
	"testing"
)

// TestTestCacheLocalNeedsNoConnection 覆盖 local 后端：没有连接可测。
func TestTestCacheLocalNeedsNoConnection(t *testing.T) {
	ctx := context.Background()
	s := newTestSetupService(t)

	res, err := s.TestCache(ctx, CacheTestInput{
		BootstrapToken: probeToken,
		Cache:          CacheInput{Driver: "local"},
	})
	if err != nil {
		t.Fatalf("local 缓存不该报错: %v", err)
	}
	if res.Detail == "" {
		t.Fatal("没给出结果说明")
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("local 不该有风险提示: %v", res.Warnings)
	}
}

// TestTestCacheRejectsBadToken 覆盖令牌不对时拒绝探测。
func TestTestCacheRejectsBadToken(t *testing.T) {
	ctx := context.Background()
	s := newTestSetupService(t)

	_, err := s.TestCache(ctx, CacheTestInput{
		BootstrapToken: "short",
		Cache:          CacheInput{Driver: "redis"},
	})
	if err == nil {
		t.Fatal("弱令牌应被拒绝")
	}
	if !strings.Contains(err.Error(), ErrBadBootstrapTokenFormat.Error()) {
		t.Fatalf("err = %v, 期望是令牌格式错误", err)
	}
}

// TestTestCacheFailsWhenUnreachable 覆盖 Redis 连不上时必须报错。
//
// 不能返回"看起来成功"——那会让用户以为缓存配好了，
// 重启后实例连不上缓存才炸。
func TestTestCacheFailsWhenUnreachable(t *testing.T) {
	ctx := context.Background()
	s := newTestSetupService(t)

	db := 0
	_, err := s.TestCache(ctx, CacheTestInput{
		BootstrapToken: probeToken,
		Cache: CacheInput{
			Driver: "redis",
			Redis:  CacheRedisInput{Addr: "127.0.0.1:1", DB: &db},
		},
	})
	if err == nil {
		t.Fatal("连不上时必须返回错误")
	}
}

// TestTestCacheRequiresAddr 覆盖 Redis 地址为空 → 参数错误。
func TestTestCacheRequiresAddr(t *testing.T) {
	ctx := context.Background()
	s := newTestSetupService(t)

	_, err := s.TestCache(ctx, CacheTestInput{
		BootstrapToken: probeToken,
		Cache:          CacheInput{Driver: "redis"},
	})
	if err == nil {
		t.Fatal("缺地址应被拒绝")
	}
	if !strings.Contains(err.Error(), ErrInvalidProbeParam.Error()) {
		t.Fatalf("err = %v, 期望是参数错误", err)
	}
}

// TestSubmitRejectsUnreachableRedis 覆盖选 redis 却连不上时初始化中止。
//
// 宁可初始化失败，也不要留一个"配置写着 redis、运行期连不上"的实例。
func TestSubmitRejectsUnreachableRedis(t *testing.T) {
	ctx := context.Background()
	s := newTestSetupService(t)

	db := 0
	_, err := s.Submit(ctx, SetupInput{
		BootstrapToken: probeToken,
		PublicURL:      "http://localhost:5173",
		AdminUsername:  "admin",
		AdminPassword:  "VerifyPass123!",
		Cache: CacheInput{
			Driver: "redis",
			Redis:  CacheRedisInput{Addr: "127.0.0.1:1", DB: &db},
		},
	})
	if err == nil {
		t.Fatal("Redis 连不上时初始化应失败")
	}
	if !strings.Contains(err.Error(), "Redis") {
		t.Fatalf("err = %v, 期望说明是 Redis 连接失败", err)
	}
}
