package service

import (
	"testing"

	"github.com/lau6code/lau6-cr/apps/api/internal/config"
)

// TestValidateCacheCombo 覆盖缓存后端与 Redis 地址的跨字段校验。
//
// 少了这条，运维在页面上把后端切成 redis 却没填地址——
// 值会落库、重建后端直接失败，或者重启时 config.validate 失败、
// 服务起不来，而报错跟设置页毫无关系。
func TestValidateCacheCombo(t *testing.T) {
	base := config.Default()
	base.Cache.Driver = "local"
	base.Cache.Redis.Addr = ""

	cases := []struct {
		name    string
		changes map[string]any
		wantErr bool
	}{
		{
			name:    "切到 redis 但没地址",
			changes: map[string]any{"cache.driver": "redis"},
			wantErr: true,
		},
		{
			name:    "切到 redis 同时给地址",
			changes: map[string]any{"cache.driver": "redis", "cache.redis.addr": "127.0.0.1:6379"},
			wantErr: false,
		},
		{
			name:    "只改地址不切后端",
			changes: map[string]any{"cache.redis.addr": "127.0.0.1:6379"},
			wantErr: false,
		},
		{
			name:    "切回 local",
			changes: map[string]any{"cache.driver": "local"},
			wantErr: false,
		},
		{
			name:    "与缓存无关的设置",
			changes: map[string]any{"stats.max_buckets": 20000},
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCacheCombo(base, tc.changes)
			if tc.wantErr && err == nil {
				t.Fatal("应当被拒绝，但没有报错")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("应当通过，却报错: %v", err)
			}
		})
	}
}

// TestValidateCacheComboKeepsBaseIntact 确认校验过程不污染基线配置。
//
// 它拿的是**当前生效配置**的副本——改副本里的值不能漏回基线，
// 否则一次校验就把 base 改脏了。
func TestValidateCacheComboKeepsBaseIntact(t *testing.T) {
	base := config.Default()
	base.Cache.Driver = "local"
	base.Cache.Redis.Addr = ""

	_ = validateCacheCombo(base, map[string]any{
		"cache.driver":     "redis",
		"cache.redis.addr": "127.0.0.1:6379",
	})

	if base.Cache.Driver != "local" || base.Cache.Redis.Addr != "" {
		t.Fatalf("基线被改脏了：driver=%q addr=%q", base.Cache.Driver, base.Cache.Redis.Addr)
	}
}
