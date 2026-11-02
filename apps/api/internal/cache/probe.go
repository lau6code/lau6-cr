package cache

import (
	"context"
	"fmt"
	"strings"
)

// RedisStatus 是 Redis 实例的关键配置状态。
//
// setup 的「测试连接」要读这些——**地址密码填对了，不代表 Redis 配对了**：
// 淘汰策略一错，签名密钥会被当普通键淘汰掉，全体用户直接被登出
// （见 docs/design/12-cache.md §5）。
type RedisStatus struct {
	// Version 是 Redis 版本（读不到时为空）。
	Version string `json:"version"`
	// MaxmemoryPolicy 是淘汰策略；空串 = 读不到（CONFIG 被禁用）。
	MaxmemoryPolicy string `json:"maxmemory_policy"`
	// AppendOnly 是 AOF 开关；空串 = 读不到。
	AppendOnly string `json:"append_only"`
	// RequirePass 表示是否设了 requirepass；nil = 读不到。
	//
	// ⚠️ 只留"有没有"，**不留口令本身**——
	// CONFIG GET requirepass 返回的就是明文口令。
	RequirePass *bool `json:"require_pass"`
}

// Warnings 返回配置风险提示。
//
// 这些是**提示不是阻断**：Redis 是不是独立实例、要不要 requirepass，
// 是运维的判断，程序无权替他决定。但不提示更糟——
// 他会以为"填对了就万事大吉"。
func (s *RedisStatus) Warnings() []string {
	var out []string
	if s.MaxmemoryPolicy != "" && s.MaxmemoryPolicy != "noeviction" {
		out = append(out, fmt.Sprintf(
			"maxmemory-policy = %s：内存压力下会把签名密钥当普通键淘汰掉，导致全体用户被登出（应为 noeviction）",
			s.MaxmemoryPolicy))
	}
	if s.AppendOnly == "no" {
		out = append(out,
			"appendonly = no：Redis 重启会丢数据，签名密钥丢失会导致全体用户被登出（建议开启 AOF 或 RDB）")
	}
	if s.RequirePass != nil && !*s.RequirePass {
		out = append(out,
			"未设置 requirepass：任何能连上这个 Redis 的人都能签发任意用户的 registry token")
	}
	return out
}

// ProbeRedis 连上 Redis：PING 一下，再读回关键配置。
//
// ⚠️ CONFIG 命令在托管 Redis 上常被禁用（ElastiCache / MemoryDB 等）——
//
//	读不到就如实标成"读不到"（空串 / nil），
//	**不能**当成"检查过了没问题"。
func ProbeRedis(ctx context.Context, o RedisOptions) (*RedisStatus, error) {
	r := NewRedis(o)
	defer func() { _ = r.Close() }()

	if err := r.Ping(ctx); err != nil {
		return nil, fmt.Errorf("连接 Redis: %w", err)
	}

	st := &RedisStatus{}
	if info, err := r.client.Info(ctx, "server").Result(); err == nil {
		st.Version = infoField(info, "redis_version")
	}
	if v, err := r.client.ConfigGet(ctx, "maxmemory-policy").Result(); err == nil {
		st.MaxmemoryPolicy = v["maxmemory-policy"]
	}
	if v, err := r.client.ConfigGet(ctx, "appendonly").Result(); err == nil {
		st.AppendOnly = v["appendonly"]
	}
	if v, err := r.client.ConfigGet(ctx, "requirepass").Result(); err == nil {
		// ⚠️ 返回值就是明文口令——只留"有没有"。
		has := v["requirepass"] != ""
		st.RequirePass = &has
	}
	return st, nil
}

// infoField 从 INFO 的文本里取某个字段的值。
func infoField(info, key string) string {
	for _, line := range strings.Split(info, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(name), key) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
