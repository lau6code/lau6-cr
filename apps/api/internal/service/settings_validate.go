package service

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/config"
)

// reTagName 是 tag 名的合法形态
// （与 docs/design/14-settings.md §3.3 的 registry.tag_overwrite_exclusions 规则一致）。
var reTagName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

// validateValue 校验一个设置值。
func validateValue(def SettingDef, v any) error {
	switch def.Type {
	case "bool":
		switch v.(type) {
		case bool, float64, int:
			return nil
		}
		return fmt.Errorf("%s 期望布尔值", def.Key)
	case "int":
		n, err := toInt64(v)
		if err != nil {
			return fmt.Errorf("%s 期望数字", def.Key)
		}
		if def.Min != nil && n < *def.Min {
			return fmt.Errorf("%s 不得小于 %d", def.Key, *def.Min)
		}
		if def.Max != nil && n > *def.Max {
			return fmt.Errorf("%s 不得大于 %d", def.Key, *def.Max)
		}
		return nil
	case "string":
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("%s 期望字符串", def.Key)
		}
		if len(def.Enum) > 0 {
			for _, e := range def.Enum {
				if e == s {
					return nil
				}
			}
			return fmt.Errorf("%s 只能是 %v 之一", def.Key, def.Enum)
		}
		switch def.Key {
		case "ldap.user_segment":
			if !strings.Contains(s, "{0}") {
				return fmt.Errorf("ldap.user_segment 必须包含 {0} 占位符")
			}
		}
		return nil
	case "duration":
		switch d := v.(type) {
		case string:
			if _, err := time.ParseDuration(d); err != nil {
				return fmt.Errorf("%s 非法时长 %q", def.Key, d)
			}
			return nil
		case float64:
			return nil
		}
		return fmt.Errorf("%s 期望时长字符串", def.Key)
	case "time":
		// 时间点（"HH:MM"）——复用 config.ParseClock 的严格校验，
		// 免得"设置页认为合法、启动校验认为非法"两套标准。
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("%s 期望时间字符串", def.Key)
		}
		if _, _, err := config.ParseClock(s); err != nil {
			return fmt.Errorf("%s 非法: %w", def.Key, err)
		}
		return nil
	case "strings":
		list, err := toStringSlice(v)
		if err != nil {
			return fmt.Errorf("%s 期望字符串数组", def.Key)
		}
		// 长度上限来自 schema 的 MaxItems（见 SettingDef.MaxItems），
		// 不在代码里另写一份魔法数字。
		if def.MaxItems > 0 && len(list) > def.MaxItems {
			return fmt.Errorf("%s 最多 %d 条", def.Key, def.MaxItems)
		}
		if def.Key == "registry.tag_overwrite_exclusions" {
			for _, t := range list {
				if !reTagName.MatchString(t) {
					return fmt.Errorf("非法 tag 名 %q", t)
				}
			}
		}
		return nil
	}
	return nil
}

func toStringSlice(v any) ([]string, error) {
	switch s := v.(type) {
	case nil:
		return nil, nil
	case []string:
		return s, nil
	case []any:
		out := make([]string, 0, len(s))
		for _, item := range s {
			str, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("元素不是字符串")
			}
			out = append(out, str)
		}
		return out, nil
	}
	return nil, fmt.Errorf("不是字符串数组")
}

// applyChanges 把本次要改的值叠加到 base 的**副本**上，返回叠加后的副本。
//
// 只叠加白名单里的键：跨字段校验关心的是自己那一组字段，
// 同一个请求里一起改的其它键不该影响这条规则。
//
// ⚠️ 返回的是副本，不动 base——base 是当前生效配置，
// 校验过程把它改脏就等于一次校验改掉了运行期配置。
func applyChanges(base *config.Config, changes map[string]any, keys ...string) (*config.Config, error) {
	allowed := make(map[string]bool, len(keys))
	for _, k := range keys {
		allowed[k] = true
	}
	cp := *base
	for key, val := range changes {
		if !allowed[key] {
			continue
		}
		if err := applySetting(&cp, key, val); err != nil {
			return nil, err
		}
	}
	return &cp, nil
}

// validateRedisAddr 校验"选了 redis 缓存后端却没填地址"。
//
// ⚠️ 三处把关的是同一个条件（设置层、setup 提交、缓存探测），
// 错误文案也统一，免得同一个问题在三个入口给出三种说法。
//
// ⚠️ 判定用 trim 之后的空串：前端判空用的是 `form.cache.redis.addr.trim()`
// （SetupView.vue），后端不 trim 就会出现"前端拦住了、后端放行"的两套语义。
// 纯空格填出来的地址在这里会被拒绝。
func validateRedisAddr(cfg *config.Config) error {
	if cfg.Cache.Driver == "redis" && strings.TrimSpace(cfg.Cache.Redis.Addr) == "" {
		return fmt.Errorf("选择 redis 缓存时 addr 必填")
	}
	return nil
}

// validateRedisDB 校验 Redis 数据库编号。
//
// ⚠️ 范围与设置项 cache.redis.db 的约束是同一对常量
// （redisDBMin / redisDBMax）：设置页与初始化向导两处口径必须一致。
//
// ⚠️ 上限不是 Redis 的限制（databases 指令可以调大），
//
//	它只挡手滑多打的几位——真超出 Redis 的库数由 Redis 自己报错。
func validateRedisDB(db int) error {
	if db < redisDBMin || db > redisDBMax {
		return fmt.Errorf("Redis 数据库编号必须在 %d-%d 之间", redisDBMin, redisDBMax)
	}
	return nil
}
