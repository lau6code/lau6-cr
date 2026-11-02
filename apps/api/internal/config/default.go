package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseClock 解析 "HH:MM" 形式的时间点（24 小时制）。
//
// 只认**严格**的 "HH:MM"：小时 00-23、分钟 00-59。
// 时间点决定"什么时候删文件"，宽松解析（"2:00"、"25:00"、"2点"）会让
// 配错的人以为配好了，实际上永远不触发。
func ParseClock(s string) (hour, minute int, err error) {
	s = strings.TrimSpace(s)
	if len(s) != 5 || s[2] != ':' {
		return 0, 0, fmt.Errorf("时间格式必须是 HH:MM，收到 %q", s)
	}
	h, err1 := strconv.Atoi(s[:2])
	m, err2 := strconv.Atoi(s[3:])
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("时间格式必须是 HH:MM，收到 %q", s)
	}
	if h < 0 || h > 23 {
		return 0, 0, fmt.Errorf("小时必须在 00-23 之间，收到 %q", s)
	}
	if m < 0 || m > 59 {
		return 0, 0, fmt.Errorf("分钟必须在 00-59 之间，收到 %q", s)
	}
	return h, m, nil
}

// Default 返回内置默认值，是所有配置层的兜底。
func Default() *Config {
	return &Config{
		DataDir: "/data",
		Server: ServerConfig{
			ListenAddr: "0.0.0.0:5000",
			StaticDir:  "/usr/share/lau6-cr/web",
		},
		Database: DatabaseConfig{Driver: "sqlite", SQLitePath: "/data/db/lau6-cr.db"},
		Keystore: KeystoreConfig{
			PrivatePath:        "/data/config/jwt-private.pem",
			JWKSPath:           "/data/config/jwt-jwks.json",
			WebhookSecretPath:  "/data/config/webhook-secret",
			RegistrySecretPath: "/data/config/registry-secret",
			PasswordKeyPath:    "/data/config/password-key.pem",
		},
		Storage: StorageConfig{
			Driver: "local",
			Local:  LocalStorage{Root: "/data/registry"},
			S3:     S3Storage{Region: "us-east-1", PathStyle: true, Secure: true},
		},
		Cache: CacheConfig{
			Driver: "local",
			Local:  LocalCache{MaxEntries: 10000},
			Redis: RedisCache{
				Prefix:      "lau6-cr:",
				DialTimeout: 3 * time.Second,
				ReadTimeout: 2 * time.Second,
			},
		},
		Token:   TokenConfig{Issuer: "lau6-cr", Audience: "lau6-cr"},
		Session: SessionConfig{CookieName: "cr_session", TTLSeconds: 86400, Secure: "auto"},
		Core: CoreConfig{
			PublicURL:              "http://localhost:5001",
			SessionTTLSeconds:      86400,
			TokenTTLSeconds:        300,
			UserTokenTTLSeconds:    2592000, // 30 天
			PatTTLSeconds:          2592000, // 30 天
			ProjectTokenTTLSeconds: 2592000, // 30 天
		},
		Access: AccessConfig{
			AnonymousReadEnabled: true,
			AnonymousTokenRPM:    60,
			AnonymousReadRPM:     120,
			LoginFailLimit:       5,
			LoginLockSeconds:     900,
		},
		// ⚠️ UpstreamAddr 故意不给默认值（理由见 RegistryConfig.UpstreamAddr）。
		Registry: RegistryConfig{
			TagOverwriteExclusions: []string{},
			// 要连独立部署的 distribution 就显式设 CR_REGISTRY_MODE=external。
			Mode:                   ModeEmbedded,
			DistributionAddr:       "127.0.0.1:5001",
			DistributionBin:        "/usr/local/bin/distribution",
			DistributionConfigPath: "/run/distribution-config.yml",
			DistributionDebugAddr:  "127.0.0.1:5002",
		},
		Stats: StatsConfig{
			Enabled:         true,
			FlushInterval:   60 * time.Second,
			HourlyRetention: 336 * time.Hour,
			DailyRetention:  9600 * time.Hour,
			PruneHour:       3,
			MaxBuckets:      10000,
		},
		Password:   PasswordConfig{Mode: "optional"},
		LDAP:       LDAPConfig{AllowCreate: true, TimeoutSec: 5},
		Limits:     LimitsConfig{MaxManifestBytes: 16 << 20},
		Reconciler: ReconcilerConfig{IntervalMinutes: 30, StaleAfterHours: 24},
		Cleanup: CleanupConfig{
			Enabled:             false,
			IntervalMinutes:     1440,
			TriggerOnPush:       true,
			PushTriggerCooldown: 5 * time.Minute,
			ProtectLatest:       true,
			DryRun:              true,
			GC: GCConfig{
				Enabled:             false,
				DeleteUntagged:      false,
				DryRun:              true,
				TriggerAfterCleanup: true,
				ScheduleEnabled:     true,
				ScheduleAt:          "02:00",
				DrainTimeout:        2 * time.Minute,
			},
		},
		Logging: LoggingConfig{
			Level:  "info",
			Output: "both",
			File: FileLogConfig{
				Enabled:    true,
				Dir:        "/data/logs",
				Filename:   "lau6-cr.log",
				MaxSizeMB:  64,
				Rotate:     "daily",
				RetainDays: 7,
			},
		},
	}
}
