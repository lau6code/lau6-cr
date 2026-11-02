package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/cache"
	"github.com/lau6code/lau6-cr/apps/api/internal/config"
)

// CacheTestInput 是缓存连通性测试的入参。
type CacheTestInput struct {
	BootstrapToken string     `json:"bootstrap_token"`
	Cache          CacheInput `json:"cache"`
}

// CacheTestResult 是缓存连通性测试的结果。
type CacheTestResult struct {
	// Driver 是被测后端（local / redis）。
	Driver string `json:"driver"`
	// Target 是被测目标（Redis 地址；不含口令）。
	Target string `json:"target"`
	// LatencyMS 是握手 + PING 往返耗时（毫秒）。
	LatencyMS int64 `json:"latency_ms"`
	// Detail 是人类可读的结果说明。
	Detail string `json:"detail"`
	// Warnings 是配置风险提示（淘汰策略、AOF、requirepass）。
	//
	// 它们**不阻断**初始化——Redis 怎么配是运维的判断，
	// 但不提示更糟：用户会以为"地址密码填对了就万事大吉"。
	Warnings []string `json:"warnings"`
	// Info 是读到的 Redis 关键配置（读不到时字段为空）。
	Info *cache.RedisStatus `json:"info,omitempty"`
}

// local 是进程内的，没有"连接"可测；Redis 要 PING 通，
// 还要读回淘汰策略——**地址密码填对了不代表 Redis 配对了**
// （见 docs/design/12-cache.md §5）。
func (s *SetupService) TestCache(ctx context.Context, in CacheTestInput) (*CacheTestResult, error) {
	if err := s.requireSetupPhase(ctx, in.BootstrapToken); err != nil {
		return nil, err
	}
	if in.Cache.Driver != "redis" {
		return &CacheTestResult{
			Driver: in.Cache.Driver,
			Detail: "本地缓存，无需连接测试",
		}, nil
	}
	addr := strings.TrimSpace(in.Cache.Redis.Addr)
	if addr == "" {
		return nil, fmt.Errorf("%w: Redis 地址不能为空", ErrInvalidProbeParam)
	}
	// 数据库编号：**必填**，不能默认成 0。
	//
	// ⚠️ 探测的是"向导里填的那一套"，缺了编号就只能去试 db 0——
	//    那测的根本不是用户要用的那个库（db 0 常被别人占着）。
	if in.Cache.Redis.DB == nil {
		return nil, fmt.Errorf("%w: 必须指定 Redis 数据库编号", ErrInvalidProbeParam)
	}
	if err := validateRedisDB(*in.Cache.Redis.DB); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidProbeParam, err.Error())
	}

	ctx, cancel := context.WithTimeout(ctx, connectivityProbeTimeout)
	defer cancel()
	start := time.Now()
	status, err := cache.ProbeRedis(ctx, cache.RedisOptions{
		Addr:     addr,
		Password: in.Cache.Redis.Password,
		DB:       *in.Cache.Redis.DB,
		Prefix:   in.Cache.Redis.Prefix,
	})
	if err != nil {
		return nil, err
	}
	latency := time.Since(start).Milliseconds()

	return &CacheTestResult{
		Driver:    "redis",
		Target:    addr,
		LatencyMS: latency,
		Detail:    "连接成功",
		Warnings:  status.Warnings(),
		Info:      status,
	}, nil
}

// validateCache 在初始化提交时验通缓存后端。
//
// 选 redis 却连不上 → **初始化中止**：
// 把配置写成 redis 而运行期连不上，重启后实例直接起不来。
func (s *SetupService) validateCache(ctx context.Context, cfg *config.Config) error {
	if cfg.Cache.Driver != "redis" {
		return nil
	}
	if err := validateRedisAddr(cfg); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, connectivityProbeTimeout)
	defer cancel()
	if _, err := cache.ProbeRedis(ctx, cache.RedisOptions{
		Addr:     cfg.Cache.Redis.Addr,
		Password: cfg.Cache.Redis.Password,
		DB:       cfg.Cache.Redis.DB,
		Prefix:   cfg.Cache.Redis.Prefix,
		TLS:      cfg.Cache.Redis.TLS,
	}); err != nil {
		return fmt.Errorf("连接 Redis 失败: %w", err)
	}
	return nil
}

// switchCache 热切换缓存后端（不改配置、不重启容器）。
//
// 只在**连接参数真的变了**时才切：重建 local 后端等于把缓存清空，
// 为了"没变"也重建一次是白白丢数据。
func (s *SetupService) switchCache(ctx context.Context, cfg *config.Config) error {
	cur := s.settings.Snapshot().Cache
	if !cur.ConnChanged(cfg.Cache) {
		return nil
	}
	if s.ReloadCache == nil {
		// 没有热切换通道就不能说"已经切好了"。
		//
		// 配置已经写进文件，重启后确实会生效——但用户要的是**立刻**生效，
		// 默默推到重启就是"改了不生效"，必须说出来。
		return fmt.Errorf("缓存后端热切换不可用：向导要求立即生效，但当前实例没有可用的热切换通道")
	}
	if err := s.ReloadCache(cfg); err != nil {
		return fmt.Errorf("切换缓存后端: %w", err)
	}
	// ⚠️ "不可丢"的键（签名私钥、用户密钥）由**热切换钩子**统一搬。
	//
	// 搬在钩子里而不是这里，是因为设置页切后端走的是同一个钩子——
	// 两处各搬一半的后果是"设置页切了后端 → 重启后密钥没了 → 全体登出"。
	return nil
}
