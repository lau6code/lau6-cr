package service

import (
	"github.com/lau6code/lau6-cr/apps/api/internal/config"
)

func i64(v int64) *int64 { return &v }

// Redis 数据库编号的取值范围。
//
// ⚠️ 上限 1023 **不是 Redis 的限制**（databases 指令可以调得更大），
//
//	它是给手滑留的闸门：10000 这种多半是打错了。
//
// 范围在这里定一次：设置页的约束（def.Min / def.Max）与
// 初始化向导的校验共用同一对常量——两处各写一份必然漂移。
const (
	redisDBMin = 0
	redisDBMax = 1023
)

// AllSettingDefs 返回全部可配置项的定义。
//
// 这是设置页与导出逻辑的**唯一真源**——新增设置项只改这里。
//
// EnvVar 不在这里写死：环境变量名的唯一真源是 config 包的 envOverrides
// （config.EnvVarFor）。两处各写一份必然漂移——漏改一处，设置页就不再显示
// "这项被 env 钉死"，改设置还会绕过 env 覆盖（env 优先级最高，改了也不生效）。
func AllSettingDefs() []SettingDef {
	defs := []SettingDef{
		// 基础
		{
			Key: "core.public_url", Type: "string", Default: "http://localhost:5001", YAMLPath: []string{"core", "public_url"},
			Tier: TierHot, Editable: true,
			Description: "外部访问根 URL。docker CLI 拿到的 WWW-Authenticate realm 由它派生。**改完立即生效**",
		},
		{
			Key: "core.session_ttl_seconds", Type: "int", Default: 86400, YAMLPath: []string{"core", "session_ttl_seconds"},
			Tier: TierHot, Editable: true, Min: i64(300), Max: i64(2592000),
			Description: "UI 会话有效期（秒）",
		},
		{
			Key: "core.token_ttl_seconds", Type: "int", Default: 300, YAMLPath: []string{"core", "token_ttl_seconds"},
			Tier: TierHot, Editable: true, Min: i64(60), Max: i64(86400),
			Description: "转发给上游 registry 的实例级令牌有效期（秒）。不得小于 60——distribution 会拒绝过短窗口",
		},
		{
			Key: "core.user_token_ttl_seconds", Type: "int", Default: 2592000, YAMLPath: []string{"core", "user_token_ttl_seconds"},
			Tier: TierHot, Editable: true, Min: i64(300), Max: i64(2592000),
			Description: "客户端令牌（用户级）的有效期（秒，默认 30 天）。同时决定用户密钥的有效期——密钥到期后该用户所有令牌一起作废",
		},
		{
			Key: "core.pat_ttl_seconds", Type: "int", Default: 2592000, YAMLPath: []string{"core", "pat_ttl_seconds"},
			Tier: TierHot, Editable: true, Min: i64(3600), Max: i64(31536000),
			Description: "个人令牌（PAT）的有效期（秒，默认 30 天）。PAT 是用户自己生成、用来在 CI/脚本里推拉镜像的长期凭据",
		},
		{
			Key: "core.project_token_ttl_seconds", Type: "int", Default: 2592000, YAMLPath: []string{"core", "project_token_ttl_seconds"},
			Tier: TierHot, Editable: true, Min: i64(3600), Max: i64(31536000),
			Description: "项目令牌的有效期（秒，默认 30 天）。同时决定该令牌签名密钥的有效期——两者必须一致，否则会出现“令牌还在有效期内却验不过”",
		},
		{
			Key: "core.trusted_proxies", Type: "strings", Default: []string{},
			YAMLPath: []string{"core", "trusted_proxies"},
			Tier:     TierHot, Editable: true,
			Description: "可信反代 IP / CIDR 列表。留空则 X-Forwarded-Proto 不可信",
		},

		// 访问控制
		{
			Key: "access.anonymous_read_enabled", Type: "bool", Default: true, YAMLPath: []string{"access", "anonymous_read_enabled"},
			Tier: TierHot, Editable: true,
			Description: "是否允许未登录用户拉取公开项目里的公开仓库。**紧急开关**",
		},
		{
			Key: "access.anonymous_token_rpm", Type: "int", Default: 60, YAMLPath: []string{"access", "anonymous_token_rpm"},
			Tier: TierHot, Editable: true, Min: i64(1), Max: i64(10000),
			Description: "匿名 /token 每 IP 每分钟请求上限",
		},
		{
			Key: "access.anonymous_read_rpm", Type: "int", Default: 120, YAMLPath: []string{"access", "anonymous_read_rpm"},
			Tier: TierHot, Editable: true, Min: i64(1), Max: i64(10000),
			Description: "匿名读路径每 IP 每分钟请求上限",
		},
		{
			Key: "access.login_fail_limit", Type: "int", Default: 5,
			YAMLPath: []string{"access", "login_fail_limit"},
			Tier:     TierHot, Editable: true, Min: i64(1), Max: i64(100),
			Description: "连续登录失败多少次后开始退避",
		},
		{
			Key: "access.login_lock_seconds", Type: "int", Default: 900,
			YAMLPath: []string{"access", "login_lock_seconds"},
			Tier:     TierHot, Editable: true, Min: i64(60), Max: i64(86400),
			Description: "触发后锁定时长（秒）",
		},

		// 仓库策略
		{
			Key: "registry.prevent_tag_overwrite", Type: "bool", Default: false, YAMLPath: []string{"registry", "prevent_tag_overwrite"},
			Tier: TierHot, Editable: true,
			Description: "tag 覆盖保护总开关。开启后已有 tag 不可改写",
		},
		{
			Key: "registry.tag_overwrite_exclusions", Type: "strings", Default: []string{},
			YAMLPath: []string{"registry", "tag_overwrite_exclusions"},
			Tier:     TierHot, Editable: true, MaxItems: 128,
			Description: "精确豁免的 tag 列表。上限 128 条",
		},

		// 流量统计
		{
			Key: "stats.enabled", Type: "bool", Default: true, YAMLPath: []string{"stats", "enabled"},
			Tier: TierHot, Editable: true,
			Description: "流量统计总开关",
		},
		{
			Key: "stats.flush_interval", Type: "duration", Default: "60s", YAMLPath: []string{"stats", "flush_interval"},
			Tier: TierHot, Editable: true,
			Description: "刷盘间隔 = 崩溃时的统计丢失窗口",
		},
		{
			Key: "stats.hourly_retention", Type: "duration", Default: "336h", YAMLPath: []string{"stats", "hourly_retention"},
			Tier: TierHot, Editable: true,
			Description: "小时粒度流量保留时长",
		},
		{
			Key: "stats.daily_retention", Type: "duration", Default: "9600h", YAMLPath: []string{"stats", "daily_retention"},
			Tier: TierHot, Editable: true,
			Description: "天粒度流量保留时长",
		},
		{
			Key: "stats.record_ip", Type: "bool", Default: false, YAMLPath: []string{"stats", "record_ip"},
			Tier: TierHot, Editable: true,
			Description: "是否记录客户端 IP（截断+哈希）。**有隐私后果**",
		},
		{
			Key: "stats.prune_hour", Type: "int", Default: 3, YAMLPath: []string{"stats", "prune_hour"},
			Tier: TierHot, Editable: true, Min: i64(0), Max: i64(23),
			Description: "每日清理过期统计的 UTC 小时",
		},
		{
			Key: "stats.max_buckets", Type: "int", Default: 10000, YAMLPath: []string{"stats", "max_buckets"},
			Tier: TierHot, Editable: true, Min: i64(100), Max: i64(1000000),
			Description: "内存桶数上限。与 cache.local.max_entries 是两个不同的池",
		},

		// 认证
		{
			Key: "ldap.enabled", Type: "bool", Default: false, YAMLPath: []string{"ldap", "enabled"},
			Tier: TierHot, Editable: true,
			Description: "启用 LDAP 认证。关闭后已有 LDAP 账号将无法登录",
		},
		{
			Key: "ldap.url", Type: "string", Default: "", YAMLPath: []string{"ldap", "url"},
			Tier: TierHot, Editable: true,
			Description: "ldap://host:389 或 ldaps://host:636",
		},
		{
			Key: "ldap.dc", Type: "string", Default: "", YAMLPath: []string{"ldap", "dc"},
			Tier: TierHot, Editable: true,
			Description: "如 dc=lau6code,dc=com",
		},
		{
			Key: "ldap.user_segment", Type: "string", Default: "", YAMLPath: []string{"ldap", "user_segment"},
			Tier: TierHot, Editable: true,
			Description: "如 uid={0},ou=people。必须包含 {0} 占位符",
		},
		{
			Key: "ldap.start_tls", Type: "bool", Default: false, YAMLPath: []string{"ldap", "start_tls"},
			Tier: TierHot, Editable: true,
			Description: "ldap:// 时是否升级 TLS。为 false 且用 ldap:// 则口令明文传输",
		},
		{
			Key: "ldap.allow_create", Type: "bool", Default: true, YAMLPath: []string{"ldap", "allow_create"},
			Tier: TierHot, Editable: true,
			Description: "首次 bind 成功是否自动建影子账号",
		},
		{
			Key: "ldap.timeout_sec", Type: "int", Default: 5, YAMLPath: []string{"ldap", "timeout_sec"},
			Tier: TierHot, Editable: true, Min: i64(1), Max: i64(60),
			Description: "dial + bind 超时（秒）",
		},
		{
			Key: "password.mode", Type: "string", Default: "optional", YAMLPath: []string{"password_transport", "mode"},
			Tier: TierHot, Editable: true,
			Enum:        []string{"plaintext", "optional", "required"},
			Description: "口令传输模式。默认 optional：前后端都发密文，旧客户端发明文仍兼容。切到 required 会让未升级的旧前端无法登录",
		},

		// 存储
		// embedded 形态下 lau6-cr **自己渲染** distribution 的配置
		// （internal/distribution/render.go），所以这几个项有真实消费点。
		//
		// ⚠️ 整段**锁定不可改**（Editable: false）：
		//    存储后端在 setup 阶段选定，之后任何一项改动都会让
		//    "已存在的 blob"与"配置指向的位置"对不上——
		//    换 endpoint / bucket 是 blob 失联，
		//    换 path_style / region 是新对象写到别处、
		//    老对象读不出来。症状都是"仓库记录都在、每次拉取都 blob unknown"。
		//    真要换，先按 11-storage.md §6 迁移 blob。
		//
		// ⚠️ external 形态下 distribution 用**它自己**那份配置文件，
		//    lau6-cr 渲染不了也重启不了它——那几项在设置页上不出现
		//    （service 侧的 WithDistributionEmbedded 负责挡）。
		{
			Key: "storage.driver", Type: "string", Default: "local", YAMLPath: []string{"storage", "driver"},
			Tier: TierRestart, Editable: false,
			Enum:        []string{"local", "s3"},
			Description: "镜像存储后端。setup 阶段选定，之后锁定不可改",
			Hint:        "在 setup 阶段选定，之后不能改：换后端不会让已存在的 blob 出现在新位置",
		},
		{
			Key: "storage.s3.endpoint", Type: "string", Default: "", YAMLPath: []string{"storage", "s3", "endpoint"},
			Tier: TierRegistry, Editable: false,
			Description: "S3 endpoint，如 https://minio.lau6code.com",
			Hint:        "setup 阶段选定，之后锁定不可改：换 endpoint 等于换了一个存储位置，旧 blob 读不出来",
		},
		{
			Key: "storage.s3.bucket", Type: "string", Default: "", YAMLPath: []string{"storage", "s3", "bucket"},
			Tier: TierRegistry, Editable: false,
			Description: "S3 bucket 名",
			Hint:        "setup 阶段选定，之后锁定不可改：换 bucket 后旧 blob 全部失联",
		},
		{
			Key: "storage.s3.region", Type: "string", Default: "us-east-1", YAMLPath: []string{"storage", "s3", "region"},
			Tier: TierRegistry, Editable: false,
			Description: "S3 region。**填错是最常见的 SignatureDoesNotMatch 成因**",
			Hint:        "setup 阶段选定，之后锁定不可改",
		},
		{
			Key: "storage.s3.path_style", Type: "bool", Default: true, YAMLPath: []string{"storage", "s3", "path_style"},
			Tier: TierRegistry, Editable: false,
			Description: "是否用 path style 寻址（bucket 放在路径里而不是主机名里）。MinIO / Ceph 必为 true，AWS S3 一般 false",
			Hint:        "setup 阶段选定，之后锁定不可改：改它会让新对象写到另一个位置",
		},

		// 缓存
		// 缓存与存储是两回事：缓存是 lau6-cr 自己的运行时缓存，
		// 存储后端是 distribution 放 blob 的地方（见上面 storage.*）。
		{
			Key: "cache.driver", Type: "string", Default: "local", YAMLPath: []string{"cache", "driver"},
			Tier: TierHot, Editable: true,
			Enum:        []string{"local", "redis"},
			Description: "缓存后端。切到 redis 要**同时**填 cache.redis.addr。**改完立即生效**（热切换后端）",
		},
		{
			Key: "cache.redis.addr", Type: "string", Default: "", YAMLPath: []string{"cache", "redis", "addr"},
			Tier: TierHot, Editable: true,
			Description: "Redis 地址，如 redis:6379。**改完立即生效**（重建缓存客户端）",
		},
		{
			Key: "cache.redis.password", Type: "string", Default: "", YAMLPath: []string{"cache", "redis", "password"},
			Tier: TierHot, Editable: true, Secret: true,
			Description: "Redis 密码。**拿到签名私钥即可签发任意用户的 token，务必设置**",
		},
		{
			Key: "cache.redis.db", Type: "int", Default: 0, YAMLPath: []string{"cache", "redis", "db"},
			Tier: TierHot, Editable: true, Min: i64(redisDBMin), Max: i64(redisDBMax),
			Description: "Redis 数据库编号（0 起；Redis 默认 16 个库）。**共享实例上要挑一个没被占用的编号**——db 0 常常是别人的。**改完立即生效**（重建缓存客户端并搬迁密钥）",
		},
		{
			Key: "cache.redis.prefix", Type: "string", Default: "lau6-cr:", YAMLPath: []string{"cache", "redis", "prefix"},
			Tier: TierHot, Editable: true,
			Description: "命名空间前缀。多实例共用 Redis 时必须区分",
		},

		// 日志
		// ⚠️ 只有**级别**是 T0 热生效：改完立刻能看到 debug 日志，
		//    排障时不用重启容器。
		//    其余项（输出目标、目录、文件名、大小上限、滚动方式、保留天数）
		//    都涉及重建文件写入器，走 T2 重启。
		{
			Key: "logging.level", Type: "string", Default: "info", YAMLPath: []string{"logging", "level"},
			Tier: TierHot, Editable: true,
			Enum:        []string{"debug", "info", "warn", "error"},
			Description: "日志级别。**改完立即生效**，无需重启——排障时最常用的一项",
		},
		{
			Key: "logging.output", Type: "string", Default: "both", YAMLPath: []string{"logging", "output"},
			Tier: TierRestart, Editable: true,
			Enum:        []string{"console", "file", "both"},
			Description: "日志输出目标。console=仅控制台，file=仅文件，both=两者都写。**改完需重启**",
		},
		{
			Key: "logging.file.enabled", Type: "bool", Default: true, YAMLPath: []string{"logging", "file", "enabled"},
			Tier: TierRestart, Editable: true,
			Description: "是否写文件日志。output 含 file 时必须为 true。**改完需重启**",
		},
		{
			Key: "logging.file.dir", Type: "string", Default: "/data/logs", YAMLPath: []string{"logging", "file", "dir"},
			Tier: TierRestart, Editable: true,
			Description: "日志目录。随 CR_DATA_DIR 一起重定位。**改完需重启**",
		},
		{
			Key: "logging.file.filename", Type: "string", Default: "lau6-cr.log", YAMLPath: []string{"logging", "file", "filename"},
			Tier: TierRestart, Editable: true,
			Description: "日志文件名。实际落盘带日期后缀，如 lau6-cr.2026-10-27.log。**改完需重启**",
		},
		{
			Key: "logging.file.max_size_mb", Type: "int", Default: 64, YAMLPath: []string{"logging", "file", "max_size_mb"},
			Tier: TierRestart, Editable: true, Min: i64(1), Max: i64(4096),
			Description: "单文件大小上限（MB），超出滚动为 .001/.002。**改完需重启**",
		},
		{
			Key: "logging.file.rotate", Type: "string", Default: "daily", YAMLPath: []string{"logging", "file", "rotate"},
			Tier: TierRestart, Editable: true,
			Enum:        []string{"daily", "size"},
			Description: "滚动方式。daily=按天滚动（文件名含日期），size=只按大小滚动。**改完需重启**",
		},
		{
			Key: "logging.file.retain_days", Type: "int", Default: 7, YAMLPath: []string{"logging", "file", "retain_days"},
			Tier: TierRestart, Editable: true, Min: i64(1), Max: i64(365),
			Description: "日志保留天数，超期文件被删除。**改完需重启**",
		},

		// 镜像清理
		//
		// 只管保留策略这一层；blob 空间回收由 registry 内核负责，
		// 见 docs/design/03-registry.md §5.5。
		{
			Key: "cleanup.enabled", Type: "bool", Default: false, YAMLPath: []string{"cleanup", "enabled"},
			Tier: TierHot, Editable: true,
			Description: "镜像清理总开关。关闭时定时与 push 触发都不跑。**清理会删镜像，默认关闭**",
		},
		{
			Key: "cleanup.interval_minutes", Type: "int", Default: 1440, YAMLPath: []string{"cleanup", "interval_minutes"},
			Tier: TierHot, Editable: true, Min: i64(5), Max: i64(10080),
			Description: "定时清理周期（分钟）。1440 = 每天一次",
		},
		{
			Key: "cleanup.trigger_on_push", Type: "bool", Default: true, YAMLPath: []string{"cleanup", "trigger_on_push"},
			Tier: TierHot, Editable: true,
			Description: "push 成功后是否立即对该项目跑一次保留策略",
		},
		{
			Key: "cleanup.push_trigger_cooldown", Type: "duration", Default: "5m", YAMLPath: []string{"cleanup", "push_trigger_cooldown"},
			Tier: TierHot, Editable: true,
			Description: "同一项目的 push 触发冷却时间。连续推多个 tag 不该触发多次全量扫描",
		},
		{
			Key: "cleanup.protect_latest", Type: "bool", Default: true, YAMLPath: []string{"cleanup", "protect_latest"},
			Tier: TierHot, Editable: true,
			Description: "是否豁免 latest tag。它是目录页的「代表 tag」，删掉会让列表页显示的最新版跳走",
		},
		{
			Key: "cleanup.dry_run", Type: "bool", Default: true, YAMLPath: []string{"cleanup", "dry_run"},
			Tier: TierHot, Editable: true,
			Description: "默认试运行：只评估不删除。**清理不可逆，建议先试运行**",
		},

		// blob 回收（第 ② 层）
		//
		// 把"已解引用但仍占盘"的 blob 清掉，磁盘空间才真正回来。
		// ⚠️ 它要求 lau6-cr 与 distribution 共享同一份数据目录，
		//    见 docs/design/03-registry.md §5.5。
		{
			Key: "cleanup.gc.enabled", Type: "bool", Default: false, YAMLPath: []string{"cleanup", "gc", "enabled"},
			Tier: TierHot, Editable: true,
			Description: "blob 回收总开关。**GC 会直接删文件，默认关闭**",
		},
		{
			Key: "cleanup.gc.delete_untagged", Type: "bool", Default: false, YAMLPath: []string{"cleanup", "gc", "delete_untagged"},
			Tier: TierHot, Editable: true,
			Description: "是否删除无 tag 的 manifest。对齐官方 --delete-untagged。" +
				"**默认关闭**：无 tag 的镜像仍可按 digest 拉取，删掉等于毁掉它",
		},
		{
			Key: "cleanup.gc.dry_run", Type: "bool", Default: true, YAMLPath: []string{"cleanup", "gc", "dry_run"},
			Tier: TierHot, Editable: true,
			Description: "默认试运行：只评估不删除。**GC 直接删文件，建议先试运行**",
		},
		{
			Key: "cleanup.gc.trigger_after_cleanup", Type: "bool", Default: true,
			YAMLPath: []string{"cleanup", "gc", "trigger_after_cleanup"},
			Tier:     TierHot, Editable: true,
			Description: "保留策略删完 tag 后是否自动跑一次回收，把磁盘空间收回来",
		},
		{
			Key: "cleanup.gc.schedule_enabled", Type: "bool", Default: true,
			YAMLPath: []string{"cleanup", "gc", "schedule_enabled"},
			Tier:     TierHot, Editable: true,
			Description: "是否启用**每日定时回收**。关闭后只保留手动与「清理后自动」两种触发",
		},
		{
			Key: "cleanup.gc.schedule_at", Type: "time", Default: "02:00",
			YAMLPath: []string{"cleanup", "gc", "schedule_at"},
			Tier:     TierHot, Editable: true,
			Description: "每日定时回收的执行时间（24 小时制，服务器本地时区）。" +
				"**进程在该时刻没跑着则当天不执行**（不做补跑）",
		},
		{
			Key: "cleanup.gc.drain_timeout", Type: "duration", Default: "2m", YAMLPath: []string{"cleanup", "gc", "drain_timeout"},
			Tier: TierHot, Editable: true,
			Description: "GC 期间等待在途上传传完的超时。超时则放弃本次回收，不删任何东西",
		},
	}
	for i := range defs {
		defs[i].EnvVar = config.EnvVarFor(defs[i].YAMLPath)
	}
	return defs
}

// publicSettingKeys 是**普通已登录用户**可见的设置项。
//
// 设计与实现约定：设置是全局的，只有系统管理员能改；
// 普通用户只读，且**只看得到这些无敏感性的项**。
// S3 endpoint / Redis 地址 / LDAP URL 这类会暴露基础设施拓扑的项不在其中。
var publicSettingKeys = map[string]bool{
	"core.public_url":                   true,
	"access.anonymous_read_enabled":     true,
	"access.anonymous_token_rpm":        true,
	"access.anonymous_read_rpm":         true,
	"registry.prevent_tag_overwrite":    true,
	"registry.tag_overwrite_exclusions": true,
	"stats.enabled":                     true,
	"password.mode":                     true,
}

// IsPublicSetting 判断某项是否对普通用户可见。
func IsPublicSetting(key string) bool { return publicSettingKeys[key] }

// storageSettingKeys 是"会进 distribution 配置"的设置项。
//
// 这些项只在 **embedded 形态**下有意义：那时 lau6-cr 自己渲染
// distribution 的配置（internal/distribution/render.go），
// 改了能重渲染 + 重启子进程使其生效。
//
// external 形态下 distribution 用**它自己**那份配置文件——
// lau6-cr 既渲染不了也重启不了它，那几项在这里就是"改了没人读"的假开关，
// 所以设置页不展示、接口也拒收（见 WithDistributionEmbedded）。
var storageSettingKeys = map[string]bool{
	"storage.driver":        true,
	"storage.s3.endpoint":   true,
	"storage.s3.bucket":     true,
	"storage.s3.region":     true,
	"storage.s3.path_style": true,
}

// IsStorageSetting 判断某项是否属于存储配置。
func IsStorageSetting(key string) bool { return storageSettingKeys[key] }

// internalSettingKeys 是**内部键**：存在 system_settings 表里，
// 由各自的子系统直接读写，不对外暴露也不能改。
//
// - system.initialized：初始化标记，setup 写、setup 读。
// - setup.bootstrap_token：引导令牌的 sha256 摘要。
// - stats.rollup_cursor：小时→天汇总的水位游标。
//
// 它们不是配置项：投影进配置快照只会得到"改了没人读"的字段，
// 所以设置层认得它们（不当作"已删除的遗留键"报警）、但一律跳过。
var internalSettingKeys = map[string]bool{
	"system.initialized":    true,
	"setup.bootstrap_token": true,
	"stats.rollup_cursor":   true,
}

// IsInternalSetting 判断某项是否是内部键。
func IsInternalSetting(key string) bool { return internalSettingKeys[key] }

// settingDefs 是全部设置项定义的索引。
//
// 定义只有 AllSettingDefs 一处；索引在包初始化时建好，
// 免得每次查一个键都重新分配整个定义列表。
var settingDefs = func() map[string]SettingDef {
	defs := AllSettingDefs()
	m := make(map[string]SettingDef, len(defs))
	for _, d := range defs {
		m[d.Key] = d
	}
	return m
}()

func defByKey(key string) (SettingDef, bool) {
	d, ok := settingDefs[key]
	return d, ok
}
