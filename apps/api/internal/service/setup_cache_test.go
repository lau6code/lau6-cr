package service

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// redisCacheInput 造一份"选 redis"的缓存入参。
//
// db 传 nil = 向导里没填数据库编号。
func redisCacheInput(db *int) CacheInput {
	in := CacheInput{Driver: "redis"}
	in.Redis.Addr = "127.0.0.1:6379"
	in.Redis.DB = db
	return in
}

// TestComposeConfigRequiresRedisDB 覆盖"选 redis 就必须给数据库编号"。
//
// 契约（docs/design/08-setup.md §12h）：
//   - 没给编号 → 初始化中止（**不兜底成 0**：共享实例上 db 0 常被别人占着）
//   - 给了 0 → 接受（0 是合法编号，"没填"与"填了 0"必须分得开）
//   - 给了别的编号 → 原样落进配置
func TestComposeConfigRequiresRedisDB(t *testing.T) {
	s, _ := newStorageSetupService(t, true)

	t.Run("没给编号", func(t *testing.T) {
		in := SetupInput{}
		in.Cache = redisCacheInput(nil)
		_, _, err := s.composeConfig(in, &envGuard{ignored: []EnvPinnedItem{}})
		if err == nil {
			t.Fatal("应当被拒绝，但没有报错")
		}
		if !strings.Contains(err.Error(), "数据库编号") {
			t.Fatalf("报错应当点明是数据库编号的问题，实际是: %v", err)
		}
	})

	t.Run("给了 0", func(t *testing.T) {
		db := 0
		in := SetupInput{}
		in.Cache = redisCacheInput(&db)
		cfg, _, err := s.composeConfig(in, &envGuard{ignored: []EnvPinnedItem{}})
		if err != nil {
			t.Fatalf("db=0 是合法编号，应当通过，却报错: %v", err)
		}
		if cfg.Cache.Redis.DB != 0 {
			t.Fatalf("db 应当是 0，实际是 %d", cfg.Cache.Redis.DB)
		}
	})

	t.Run("给了 3", func(t *testing.T) {
		db := 3
		in := SetupInput{}
		in.Cache = redisCacheInput(&db)
		cfg, _, err := s.composeConfig(in, &envGuard{ignored: []EnvPinnedItem{}})
		if err != nil {
			t.Fatalf("应当通过，却报错: %v", err)
		}
		if cfg.Cache.Redis.DB != 3 {
			t.Fatalf("db 应当是 3，实际是 %d", cfg.Cache.Redis.DB)
		}
	})

	t.Run("选了 local 不需要编号", func(t *testing.T) {
		in := SetupInput{}
		in.Cache = CacheInput{Driver: "local"}
		cfg, _, err := s.composeConfig(in, &envGuard{ignored: []EnvPinnedItem{}})
		if err != nil {
			t.Fatalf("local 后端不需要数据库编号，却报错: %v", err)
		}
		if cfg.Cache.Driver != "local" {
			t.Fatalf("driver 应当是 local，实际是 %q", cfg.Cache.Driver)
		}
	})
}

// TestComposeConfigRejectsRedisDBOutOfRange 覆盖"数据库编号越界要报错"。
//
// 越界值在 Redis 侧才会被拒（"DB index is out of range"），
// 但那是"连得上、然后失败"——这里先挡住，别让用户填完才看到。
func TestComposeConfigRejectsRedisDBOutOfRange(t *testing.T) {
	s, _ := newStorageSetupService(t, true)

	db := redisDBMax + 1
	in := SetupInput{}
	in.Cache = redisCacheInput(&db)
	_, _, err := s.composeConfig(in, &envGuard{ignored: []EnvPinnedItem{}})
	if err == nil {
		t.Fatal("越界的数据库编号应当被拒绝，但没有报错")
	}
}

// TestComposeConfigAcceptsMissingDBWhenEnvPinned 覆盖"被 env 钉死时向导不用填"。
//
// 口径与设置页一致（变量存在即钉死）：值由环境变量说了算，
// 拿"必须填"去卡一个改不了的字段只会把初始化卡死。
func TestComposeConfigAcceptsMissingDBWhenEnvPinned(t *testing.T) {
	t.Setenv("CR_REDIS_DB", "5")
	s, _ := newStorageSetupService(t, true)

	in := SetupInput{}
	in.Cache = redisCacheInput(nil)
	cfg, _, err := s.composeConfig(in, &envGuard{ignored: []EnvPinnedItem{}})
	if err != nil {
		t.Fatalf("编号被 env 钉死时应当放行，却报错: %v", err)
	}
	// 基线里的编号（真实运行时是 env 那个值）不能被向导的"没填"改掉。
	if cfg.Cache.Redis.DB != 0 {
		t.Fatalf("基线里的编号被改成了 %d", cfg.Cache.Redis.DB)
	}
}

// TestTestCacheRequiresRedisDB 覆盖"连通性测试也要数据库编号"。
//
// 缺了编号就只能去试 db 0——那测的根本不是用户要用的那个库。
func TestTestCacheRequiresRedisDB(t *testing.T) {
	s, _ := newStorageSetupService(t, true)

	in := CacheTestInput{BootstrapToken: strings.Repeat("a", 32)}
	in.Cache = redisCacheInput(nil)
	_, err := s.TestCache(context.Background(), in)
	if !errors.Is(err, ErrInvalidProbeParam) {
		t.Fatalf("应当是参数错误（缺数据库编号），实际是: %v", err)
	}
}

// TestRedisDBIsManagedSetting 覆盖"数据库编号是托管设置，落进配置文件"。
//
// 少这一条：向导里填的编号只在内存里活一次，重启后回到 0。
func TestRedisDBIsManagedSetting(t *testing.T) {
	m, yamlPath := newBackupTestManager(t)

	applyChange(t, m, "cache.redis.db", 3)

	b, err := os.ReadFile(yamlPath)
	if err != nil {
		t.Fatalf("读配置文件: %v", err)
	}
	if !strings.Contains(string(b), "db: 3") {
		t.Fatalf("配置文件里没有 cache.redis.db: %s", string(b))
	}
}
