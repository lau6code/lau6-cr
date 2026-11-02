package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// envOverride 描述"环境变量覆盖哪个配置字段"。
//
// 字段用 YAML 路径定位（与设置层的 SettingDef.YAMLPath 同一套），
// 值的解析方式由字段类型决定。
type envOverride struct {
	Var  string
	Path []string
}

// envOverrides 是全部"环境变量 → 配置字段"的覆盖关系。
//
// 这是环境变量名的**唯一真源**：applyEnv 按它赋值，
// 设置层也按它判断"某项是不是被环境变量钉死"（EnvVarFor）。
// 两处各写一份的话，改了一处忘了另一处就会出现
// "页面说这项被 env 钉死、实际没人读"的假开关。
var envOverrides = []envOverride{
	{Var: "CR_LISTEN_ADDR", Path: []string{"server", "listen_addr"}},
	{Var: "CR_STATIC_DIR", Path: []string{"server", "static_dir"}},

	{Var: "CR_UPSTREAM_REGISTRY", Path: []string{"registry", "upstream_addr"}},
	{Var: "CR_REGISTRY_MODE", Path: []string{"registry", "mode"}},
	{Var: "CR_DISTRIBUTION_ADDR", Path: []string{"registry", "distribution_addr"}},
	{Var: "CR_DISTRIBUTION_BIN", Path: []string{"registry", "distribution_bin"}},
	{Var: "CR_DISTRIBUTION_CONFIG", Path: []string{"registry", "distribution_config_path"}},
	{Var: "CR_DISTRIBUTION_DEBUG_ADDR", Path: []string{"registry", "distribution_debug_addr"}},

	{Var: "CR_DB_DRIVER", Path: []string{"database", "driver"}},
	{Var: "CR_DB_SQLITE_PATH", Path: []string{"database", "sqlite_path"}},
	{Var: "CR_DB_DSN", Path: []string{"database", "dsn"}},

	{Var: "CR_STORAGE_DRIVER", Path: []string{"storage", "driver"}},
	{Var: "CR_S3_ENDPOINT", Path: []string{"storage", "s3", "endpoint"}},
	{Var: "CR_S3_REGION", Path: []string{"storage", "s3", "region"}},
	{Var: "CR_S3_BUCKET", Path: []string{"storage", "s3", "bucket"}},
	{Var: "CR_S3_ACCESS_KEY", Path: []string{"storage", "s3", "access_key"}},
	{Var: "CR_S3_SECRET_KEY", Path: []string{"storage", "s3", "secret_key"}},
	{Var: "CR_S3_PATH_STYLE", Path: []string{"storage", "s3", "path_style"}},

	{Var: "CR_CACHE_DRIVER", Path: []string{"cache", "driver"}},
	{Var: "CR_REDIS_ADDR", Path: []string{"cache", "redis", "addr"}},
	{Var: "CR_REDIS_PASSWORD", Path: []string{"cache", "redis", "password"}},
	{Var: "CR_REDIS_DB", Path: []string{"cache", "redis", "db"}},
	{Var: "CR_REDIS_PREFIX", Path: []string{"cache", "redis", "prefix"}},
	{Var: "CR_REDIS_TLS", Path: []string{"cache", "redis", "tls"}},

	{Var: "CR_PUBLIC_URL", Path: []string{"core", "public_url"}},
	{Var: "CR_SESSION_TTL", Path: []string{"core", "session_ttl_seconds"}},
	{Var: "CR_TOKEN_TTL", Path: []string{"core", "token_ttl_seconds"}},
	{Var: "CR_USER_TOKEN_TTL", Path: []string{"core", "user_token_ttl_seconds"}},
	{Var: "CR_PAT_TTL", Path: []string{"core", "pat_ttl_seconds"}},
	{Var: "CR_PROJECT_TOKEN_TTL", Path: []string{"core", "project_token_ttl_seconds"}},

	{Var: "CR_ANON_READ_ENABLED", Path: []string{"access", "anonymous_read_enabled"}},
	{Var: "CR_ANON_TOKEN_RPM", Path: []string{"access", "anonymous_token_rpm"}},
	{Var: "CR_ANON_READ_RPM", Path: []string{"access", "anonymous_read_rpm"}},
	{Var: "CR_LOGIN_FAIL_LIMIT", Path: []string{"access", "login_fail_limit"}},
	{Var: "CR_LOGIN_LOCK_SECONDS", Path: []string{"access", "login_lock_seconds"}},

	{Var: "CR_PREVENT_TAG_OVERWRITE", Path: []string{"registry", "prevent_tag_overwrite"}},

	{Var: "CR_STATS_ENABLED", Path: []string{"stats", "enabled"}},
	{Var: "CR_STATS_FLUSH_INTERVAL", Path: []string{"stats", "flush_interval"}},
	{Var: "CR_STATS_HOURLY_RETENTION", Path: []string{"stats", "hourly_retention"}},
	{Var: "CR_STATS_DAILY_RETENTION", Path: []string{"stats", "daily_retention"}},
	{Var: "CR_STATS_RECORD_IP", Path: []string{"stats", "record_ip"}},
	{Var: "CR_STATS_PRUNE_HOUR", Path: []string{"stats", "prune_hour"}},
	{Var: "CR_STATS_MAX_BUCKETS", Path: []string{"stats", "max_buckets"}},

	{Var: "CR_PASSWORD_MODE", Path: []string{"password_transport", "mode"}},

	{Var: "CR_LDAP_ENABLED", Path: []string{"ldap", "enabled"}},
	{Var: "CR_LDAP_URL", Path: []string{"ldap", "url"}},
	{Var: "CR_LDAP_DC", Path: []string{"ldap", "dc"}},
	{Var: "CR_LDAP_USER_SEGMENT", Path: []string{"ldap", "user_segment"}},
	{Var: "CR_LDAP_START_TLS", Path: []string{"ldap", "start_tls"}},
	{Var: "CR_LDAP_ALLOW_CREATE", Path: []string{"ldap", "allow_create"}},
	{Var: "CR_LDAP_TIMEOUT_SEC", Path: []string{"ldap", "timeout_sec"}},

	{Var: "CR_CLEANUP_ENABLED", Path: []string{"cleanup", "enabled"}},
	{Var: "CR_CLEANUP_INTERVAL_MINUTES", Path: []string{"cleanup", "interval_minutes"}},
	{Var: "CR_CLEANUP_TRIGGER_ON_PUSH", Path: []string{"cleanup", "trigger_on_push"}},
	{Var: "CR_CLEANUP_PUSH_TRIGGER_COOLDOWN", Path: []string{"cleanup", "push_trigger_cooldown"}},
	{Var: "CR_CLEANUP_PROTECT_LATEST", Path: []string{"cleanup", "protect_latest"}},
	{Var: "CR_CLEANUP_DRY_RUN", Path: []string{"cleanup", "dry_run"}},

	{Var: "CR_CLEANUP_GC_ENABLED", Path: []string{"cleanup", "gc", "enabled"}},
	{Var: "CR_CLEANUP_GC_DELETE_UNTAGGED", Path: []string{"cleanup", "gc", "delete_untagged"}},
	{Var: "CR_CLEANUP_GC_DRY_RUN", Path: []string{"cleanup", "gc", "dry_run"}},
	{Var: "CR_CLEANUP_GC_TRIGGER_AFTER_CLEANUP", Path: []string{"cleanup", "gc", "trigger_after_cleanup"}},
	{Var: "CR_CLEANUP_GC_SCHEDULE_ENABLED", Path: []string{"cleanup", "gc", "schedule_enabled"}},
	{Var: "CR_CLEANUP_GC_SCHEDULE_AT", Path: []string{"cleanup", "gc", "schedule_at"}},
	{Var: "CR_CLEANUP_GC_DRAIN_TIMEOUT", Path: []string{"cleanup", "gc", "drain_timeout"}},
}

// envVarByPath 是 YAML 路径 → 环境变量名的索引。
var envVarByPath = func() map[string]string {
	m := make(map[string]string, len(envOverrides))
	for _, o := range envOverrides {
		m[strings.Join(o.Path, ".")] = o.Var
	}
	return m
}()

// EnvVarFor 返回某条 YAML 路径对应的环境变量名（没有则空串）。
//
// 设置层用它填 SettingDef.EnvVar——设置项声明的 env 变量
// 与配置真正读的是同一个来源，不会各写一份而漂移。
func EnvVarFor(path []string) string {
	return envVarByPath[strings.Join(path, ".")]
}

// applyEnv 用环境变量覆盖配置。环境变量优先级最高（见包注释）。
//
// 写错的环境变量必须在这里暴露成错误，而不是静默保留默认值：
// 静默兜底会让设置页按"变量存在"显示"来源：环境变量"，
// 而实际生效的是 YAML/默认值——页面在骗人，写错的人也看不出来。
//
// 错误一次性收集完再报出，运维改一轮就能修好所有写错的变量，
// 而不是改一个、重启一次、再发现下一个。
func (c *Config) applyEnv() error {
	var errs []error

	// CR_DATA_DIR 不在表里：DataDir 的 yaml tag 是 "-"（它不是配置文件的一部分），
	// 没法用 YAML 路径定位。
	if v, ok := os.LookupEnv("CR_DATA_DIR"); ok && v != "" {
		c.DataDir = v
	}

	for _, o := range envOverrides {
		raw, ok := os.LookupEnv(o.Var)
		// 空串与"未设置"等价：编排文件里写 CR_SESSION_TTL= 与不写是同一个意思。
		if !ok || raw == "" {
			continue
		}
		f, ok := FieldByYAMLPath(c, o.Path)
		if !ok {
			errs = append(errs, fmt.Errorf("环境变量 %s 对应的配置路径 %v 不存在", o.Var, o.Path))
			continue
		}
		if err := setFieldFromString(f, raw); err != nil {
			errs = append(errs, fmt.Errorf("环境变量 %s=%s %w", o.Var, raw, err))
		}
	}

	// ⚠️ 管理员凭据（CR_ADMIN_USERNAME / CR_ADMIN_PASSWORD）**不在这里读**，
	//    而且这两个变量已经废弃：管理员凭据只从界面来
	//    （Web 向导的表单 / `lau6-cr setup` 的终端交互）。
	//    放进 Config 会让它跟着每次热重载走，还会被导进 application.yaml。

	return errors.Join(errs...)
}
