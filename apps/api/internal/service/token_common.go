package service

import (
	"errors"
	"strings"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// resolveTokenExpiry 把失效策略解析成落库的 expires_at。
//
// 返回 **nil = 永久有效**（落 NULL）。
//
// ⚠️ "永久"必须由用户**显式选**，不给任何参数走的是系统默认 TTL：
//
//	否则用户以为拿到的是 30 天，实际拿到的是永不过期的凭据——那是安全事故。
//
// ttl 来自设置项（个人令牌是 core.pat_ttl_seconds，项目令牌是
// core.project_token_ttl_seconds），是设置页里可见、可改的配置项。
func resolveTokenExpiry(policy ExpiryPolicy, ttl time.Duration) (*int64, error) {
	if policy.NeverExpires && policy.ExpiresAt != nil {
		return nil, errors.New("不能同时指定失效时间和永久有效")
	}
	if policy.NeverExpires {
		return nil, nil
	}
	now := store.Now()
	if policy.ExpiresAt == nil {
		// 系统默认：从现在起算一个 TTL。
		v := now + int64(ttl/time.Second)
		return &v, nil
	}
	if *policy.ExpiresAt <= now {
		return nil, errors.New("失效时间必须晚于当前时间")
	}
	v := *policy.ExpiresAt
	return &v, nil
}

// tokenTTL 取当前生效的令牌有效期。
//
// 没接线（fn 为 nil）或取到非正数时用默认值：
// 有效期是 0 等于"一出生就作废"，那不是用户要的语义。
// 设置项本身有下限（core.pat_ttl_seconds 的 Min=3600），
// 这里是接线缺失时的兜底值，不是隐藏的默认策略。
func tokenTTL(fn func() time.Duration, def time.Duration) time.Duration {
	if fn == nil {
		return def
	}
	d := fn()
	if d <= 0 {
		return def
	}
	return d
}

// validateTokenName 校验令牌名称，返回去掉首尾空白后的名称。
func validateTokenName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("令牌名称不能为空")
	}
	if len(name) > 64 {
		return "", errors.New("令牌名称不能超过 64 个字符")
	}
	return name, nil
}
