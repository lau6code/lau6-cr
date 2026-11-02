package config

import (
	"time"
)

// Config 是应用的全量配置。
type Config struct {
	// DataDir 是运行期数据根目录，默认 /data。
	// 设 CR_DATA_DIR 可整体重定位（本地开发、测试用）。
	DataDir  string         `yaml:"-"`
	Server   ServerConfig   `yaml:"server"`
	Database DatabaseConfig `yaml:"database"`
	Keystore KeystoreConfig `yaml:"keystore"`
	Storage  StorageConfig  `yaml:"storage"`
	Cache    CacheConfig    `yaml:"cache"`
	Token    TokenConfig    `yaml:"token"`
	Session  SessionConfig  `yaml:"session"`
	Core     CoreConfig     `yaml:"core"`
	Access   AccessConfig   `yaml:"access"`
	Registry RegistryConfig `yaml:"registry"`
	Stats    StatsConfig    `yaml:"stats"`
	// Password 的 YAML 段名是 `password_transport`（**不是** `password`）。
	//
	// yaml.Unmarshal 是非严格模式，不认识的段**静默忽略**——
	// 段名必须与 `14-settings.md` §6 的映射表一致：**password_transport**。
	Password   PasswordConfig   `yaml:"password_transport"`
	LDAP       LDAPConfig       `yaml:"ldap"`
	Limits     LimitsConfig     `yaml:"limits"`
	Reconciler ReconcilerConfig `yaml:"reconciler"`
	Cleanup    CleanupConfig    `yaml:"cleanup"`
	Logging    LoggingConfig    `yaml:"logging"`
}

// LoggingConfig 是日志配置。
//
// 日志同时写 stdout 与文件：stdout 供容器日志采集，
// 文件供**长期留存的排障档案**（见 docs/design/05-build-deploy.md §7.2）。
type LoggingConfig struct {
	// Level 是日志级别：debug|info|warn|error。
	Level string `yaml:"level"`
	// Output 是输出目标：console|file|both。
	Output string `yaml:"output"`
	// File 控制文件日志的落盘与轮转。
	File FileLogConfig `yaml:"file"`
}

// FileLogConfig 是文件日志配置。
type FileLogConfig struct {
	// Enabled 是否写文件日志。
	Enabled bool `yaml:"enabled"`
	// Dir 是日志目录。默认 /data/logs（随数据卷持久化）。
	Dir string `yaml:"dir"`
	// Filename 是日志文件名。实际落盘会带日期后缀。
	Filename string `yaml:"filename"`
	// MaxSizeMB 是单文件大小上限（MB），超出即滚动。
	MaxSizeMB int `yaml:"max_size_mb"`
	// Rotate 是滚动方式：daily（按天）| size（只按大小）。
	Rotate string `yaml:"rotate"`
	// RetainDays 是保留天数，超期文件被删除。
	RetainDays int `yaml:"retain_days"`
}
type ServerConfig struct {
	ListenAddr string `yaml:"listen_addr"`
	StaticDir  string `yaml:"static_dir"`
}
type DatabaseConfig struct {
	Driver     string `yaml:"driver"`
	SQLitePath string `yaml:"sqlite_path"`
	DSN        string `yaml:"dsn"`
}
type KeystoreConfig struct {
	PrivatePath        string `yaml:"private_path"`
	JWKSPath           string `yaml:"jwks_path"`
	WebhookSecretPath  string `yaml:"webhook_secret_path"`
	RegistrySecretPath string `yaml:"registry_secret_path"`
	// PasswordKeyPath 是口令加密私钥（X25519）的文件路径。
	//
	// ⚠️ 必须与签名私钥（PrivatePath）**分开存放**：
	//    签名私钥被读走 = 攻击者可伪造任意用户的 token；
	//    口令私钥被读走 = 只能解开抓到的密文（13-password-transport.md §4.2 / ADR 4）。
	PasswordKeyPath string `yaml:"password_key_path"`
}
type StorageConfig struct {
	Driver string       `yaml:"driver"`
	Local  LocalStorage `yaml:"local"`
	S3     S3Storage    `yaml:"s3"`
}
type LocalStorage struct {
	Root string `yaml:"root"`
}
type S3Storage struct {
	Endpoint  string `yaml:"endpoint"`
	Region    string `yaml:"region"`
	Bucket    string `yaml:"bucket"`
	AccessKey string `yaml:"access_key"`
	SecretKey string `yaml:"secret_key"`
	PathStyle bool   `yaml:"path_style"`
	Secure    bool   `yaml:"secure"`
}
type CacheConfig struct {
	Driver string     `yaml:"driver"`
	Local  LocalCache `yaml:"local"`
	Redis  RedisCache `yaml:"redis"`
}
type LocalCache struct {
	MaxEntries int `yaml:"max_entries"`
}

// ConnChanged 判断缓存**连接参数**是否变了。
//
// 只看连接参数：dial_timeout 这类不影响"连到哪个后端"，
// 不该为它们重建客户端（重建 local 后端等于把缓存清空）。
func (c CacheConfig) ConnChanged(o CacheConfig) bool {
	return c.Driver != o.Driver ||
		c.Redis.Addr != o.Redis.Addr ||
		c.Redis.Password != o.Redis.Password ||
		c.Redis.DB != o.Redis.DB ||
		c.Redis.Prefix != o.Redis.Prefix ||
		c.Redis.TLS != o.Redis.TLS
}

type RedisCache struct {
	Addr        string        `yaml:"addr"`
	Password    string        `yaml:"password"`
	DB          int           `yaml:"db"`
	Prefix      string        `yaml:"prefix"`
	TLS         bool          `yaml:"tls"`
	DialTimeout time.Duration `yaml:"dial_timeout"`
	ReadTimeout time.Duration `yaml:"read_timeout"`
}
type TokenConfig struct {
	Issuer   string `yaml:"issuer"`
	Audience string `yaml:"audience"`
}
type SessionConfig struct {
	CookieName string `yaml:"cookie_name"`
	TTLSeconds int    `yaml:"ttl_seconds"`
	// Secure 控制会话 Cookie 的 Secure 属性。
	//
	//	"auto"（默认）：请求看起来是 HTTPS 时才加 Secure。
	//	                形态 B（外层反代终结 TLS）下由 X-Forwarded-Proto 判定，
	//	                但**只信任配置里的反代 IP**。
	//	"true"： 永远加 Secure（纯 HTTPS 部署）。
	//	"false"：永远不加（仅本地 HTTP 调试用）。
	//
	// ⚠️ 固定为 "true" 时，明文 HTTP 部署将**无法登录**——
	//    浏览器不会回传 Secure Cookie。
	Secure string `yaml:"secure"`
}
type CoreConfig struct {
	PublicURL         string `yaml:"public_url"`
	SessionTTLSeconds int    `yaml:"session_ttl_seconds"`
	TokenTTLSeconds   int    `yaml:"token_ttl_seconds"`
	// UserTokenTTLSeconds 是**客户端令牌**（用户级）的有效期。
	//
	// 与 TokenTTLSeconds 是两件事：
	//   - TokenTTLSeconds   → 转发给上游的实例级令牌（一次性通行证，短）
	//   - UserTokenTTLSeconds → 发给客户端的用户级令牌（长期凭据）
	//
	// 它同时决定用户密钥的有效期：令牌不会活得比它的密钥久。
	UserTokenTTLSeconds int `yaml:"user_token_ttl_seconds"`
	// PatTTLSeconds 是**个人令牌（PAT）**的有效期。
	//
	// PAT 是用户自己生成的长期凭据（CI / 脚本推拉镜像用），
	// 与用户签名密钥、用户令牌同寿命口径。
	PatTTLSeconds int `yaml:"pat_ttl_seconds"`
	// ProjectTokenTTLSeconds 是**项目令牌**的有效期。
	//
	// 项目令牌是项目管理员生成的长期凭据。
	// 它同时决定项目令牌签名密钥的有效期：
	// 令牌不会活得比它的密钥久，否则会出现"令牌还在有效期内却验不过"。
	ProjectTokenTTLSeconds int      `yaml:"project_token_ttl_seconds"`
	TrustedProxies         []string `yaml:"trusted_proxies"`
}
type AccessConfig struct {
	AnonymousReadEnabled bool `yaml:"anonymous_read_enabled"`
	AnonymousTokenRPM    int  `yaml:"anonymous_token_rpm"`
	AnonymousReadRPM     int  `yaml:"anonymous_read_rpm"`
	LoginFailLimit       int  `yaml:"login_fail_limit"`
	LoginLockSeconds     int  `yaml:"login_lock_seconds"`
}

// distribution 的部署形态。
const (
	// ModeEmbedded：distribution 与 lau6-cr **同容器**，
	// 由 lau6-cr 自己拉起子进程（镜像里已内置 distribution 二进制）。
	//
	// 这是默认形态，也是 blob 回收（GC）能工作的前提：
	// lau6-cr 与 distribution 共用同一份存储，
	// 回收才能按同一份配置去清。
	ModeEmbedded = "embedded"
	// ModeExternal：distribution 独立部署（别的容器 / 别的机器），
	// lau6-cr 只通过 registry.upstream_addr 调用它。
	//
	// ⚠️ 该形态下 blob 回收需要用 CR_DISTRIBUTION_CONFIG
	//    指向 registry 自己的配置文件（挂进容器里让 lau6-cr 读得到），
	//    否则 GC 不可用——回收必须按 registry 实际在用的配置来。
	ModeExternal = "external"
)

type RegistryConfig struct {
	PreventTagOverwrite    bool     `yaml:"prevent_tag_overwrite"`
	TagOverwriteExclusions []string `yaml:"tag_overwrite_exclusions"`
	// Mode 是 distribution 的部署形态：embedded | external。
	//
	// 默认 embedded（同容器）：镜像里内置 distribution 二进制，
	// lau6-cr 启动时把它拉起来，两者共享同一个数据目录。
	//
	// ⚠️ 它**不是**"上游地址的别名"——embedded 模式下上游地址由
	//    DistributionAddr 推导，根本不需要配置。
	Mode string `yaml:"mode"`
	// UpstreamAddr 是 distribution 的地址，**仅 external 模式使用**。
	//
	// external 模式（distribution 独立部署）时指向它的服务名，
	// 如 http://registry:5000。
	//
	// ⚠️ **故意不给默认值**：embedded 模式下这个值由
	//    DistributionAddr 推导；external 模式下它取决于部署拓扑，
	//    猜一个默认值只会让服务"看起来正常地跑着、实际全连不上"。
	//    没给就在启动时报错（见 ValidateRegistry）。
	//
	// ⚠️ embedded 模式下**给这个值是错误**：它会被忽略，
	//    而"配了不生效"就是假开关——所以启动时直接报错。
	UpstreamAddr string `yaml:"upstream_addr"`
	// DistributionAddr 是 embedded 模式下 distribution 的监听地址
	//（host:port，不带 scheme）。默认 127.0.0.1:5001。
	//
	// 它同时决定 lau6-cr 调用上游的地址（http://<DistributionAddr>），
	// 所以必须与渲染出来的 distribution 配置里的 http.addr 一致。
	DistributionAddr string `yaml:"distribution_addr"`
	// DistributionBin 是 embedded 模式下 distribution 可执行文件的路径。
	//
	// 默认 /usr/local/bin/distribution（镜像里的安装位置）。
	// 文件不存在时启动失败——不静默降级成"不启 distribution"。
	DistributionBin string `yaml:"distribution_bin"`
	// DistributionConfigPath 是 **distribution 使用的配置文件**路径。
	//
	// 两种形态下它都指"那个 registry 实际在用的配置文件"：
	//
	//   - embedded：lau6-cr 自己渲染到这里的配置；
	//   - external：独立部署的 registry 自己的配置文件
	//     （需要挂进容器里让 lau6-cr 读得到）。
	//
	// ⚠️ blob 回收按这份配置里的 storage 段去清理对象——
	//    配置不是同一份就会清到别的地方去
	//    （症状是"回收跑完了，桶里的东西还在"）。
	//
	// 默认 /run/distribution-config.yml（容器内是 tmpfs，不持久化）。
	DistributionConfigPath string `yaml:"distribution_config_path"`
	// DistributionDebugAddr 是 embedded 模式下 distribution 的
	// debug/metrics 监听地址（host:port）。默认 127.0.0.1:5002。
	//
	// ⚠️ 它是**另一个**监听端口，与 distribution 自身的监听端口、
	//    lau6-cr 自身的监听端口都不能撞——撞了就是
	//    "bind: address already in use"，lau6-cr 起不来。
	//    所以启动时校验这三个地址（见 ValidateRegistry）。
	//
	// 留空 = 不开 debug server（也就没有 /metrics）。
	DistributionDebugAddr string `yaml:"distribution_debug_addr"`
}
type StatsConfig struct {
	Enabled         bool          `yaml:"enabled"`
	FlushInterval   time.Duration `yaml:"flush_interval"`
	HourlyRetention time.Duration `yaml:"hourly_retention"`
	DailyRetention  time.Duration `yaml:"daily_retention"`
	RecordIP        bool          `yaml:"record_ip"`
	PruneHour       int           `yaml:"prune_hour"`
	MaxBuckets      int           `yaml:"max_buckets"`
}
type PasswordConfig struct {
	// Mode 是口令传输模式：plaintext / optional / required。
	//
	// ⚠️ 这个字段的**默认值**必须与设置项 `password.mode` 的定义
	//    （service.settings_defs）保持一致，否则两层默认值会打架：
	//    设置页显示"默认值 optional"、实际生效的却是这里的 plaintext
	//    ——页面骗人，而且用户看到的是明文过网。
	Mode string `yaml:"mode"`
}
type LDAPConfig struct {
	Enabled     bool   `yaml:"enabled"`
	URL         string `yaml:"url"`
	DC          string `yaml:"dc"`
	UserSegment string `yaml:"user_segment"`
	StartTLS    bool   `yaml:"start_tls"`
	AllowCreate bool   `yaml:"allow_create"`
	TimeoutSec  int    `yaml:"timeout_sec"`
}
type LimitsConfig struct {
	MaxManifestBytes int `yaml:"max_manifest_bytes"`
}
type ReconcilerConfig struct {
	IntervalMinutes int `yaml:"interval_minutes"`
	StaleAfterHours int `yaml:"stale_after_hours"`
}

// CleanupConfig 是镜像清理策略的运行参数。
//
// 分两层：
//
//	① 保留策略——决定删哪些 tag（Enabled / IntervalMinutes / …）
//	② blob 回收——把"已解引用但仍占盘"的 blob 清掉（GC）
//
// ② 只在 lau6-cr 与 distribution **共享同一份数据目录**时才可用：
// lau6-cr 必须能直连 distribution 的存储根目录才能扫盘。
// 见 docs/design/03-registry.md §5.5。
type CleanupConfig struct {
	// Enabled 是清理总开关。关闭时定时与 push 触发都不跑。
	//
	// 默认关闭：清理是破坏性操作，必须显式开启。
	Enabled bool `yaml:"enabled"`
	// IntervalMinutes 是定时清理周期（分钟）。
	IntervalMinutes int `yaml:"interval_minutes"`
	// TriggerOnPush 控制 push 成功后是否立即对该项目跑一次保留策略。
	TriggerOnPush bool `yaml:"trigger_on_push"`
	// PushTriggerCooldown 是同一项目的 push 触发冷却时间。
	//
	// 连续推 10 个 tag 不该触发 10 次全量扫描。
	PushTriggerCooldown time.Duration `yaml:"push_trigger_cooldown"`
	// ProtectLatest 控制是否豁免 latest tag。
	//
	// latest 是目录页的"代表 tag"，删掉会让列表页显示的最新版跳走。
	ProtectLatest bool `yaml:"protect_latest"`
	// DryRun 是默认的**试运行**开关。
	//
	// 默认 true：清理不可逆，先看再删。手动执行时可以在请求里显式关掉。
	DryRun bool `yaml:"dry_run"`
	// GC 是第 ② 层 blob 回收的参数。
	GC GCConfig `yaml:"gc"`
}

// GCConfig 是 blob 回收（第 ② 层）的运行参数。
//
// ⚠️ 它与 ① 层的保留策略是**两件独立的事**：
//
//	保留策略删 tag/manifest（只解除引用），
//	GC 才真正把磁盘空间还给操作系统。
type GCConfig struct {
	// Enabled 是 blob 回收总开关。关闭时手动与自动触发都不跑。
	//
	// 默认关闭：GC 会直接删文件，必须显式开启。
	Enabled bool `yaml:"enabled"`
	// DeleteUntagged 控制是否删除无 tag 的 manifest。
	//
	// 对齐官方 `registry garbage-collect --delete-untagged`。
	// 默认关闭：无 tag 的镜像仍可按 digest 拉取，删掉等于毁掉它。
	DeleteUntagged bool `yaml:"delete_untagged"`
	// DryRun 是默认的**试运行**开关。
	//
	// 默认 true：GC 直接删文件，先看再删。
	DryRun bool `yaml:"dry_run"`
	// TriggerAfterCleanup 控制保留策略执行完后是否自动跑一次 GC。
	//
	// 默认 true：删了 tag 就顺手把空间收回来。
	TriggerAfterCleanup bool `yaml:"trigger_after_cleanup"`
	// ScheduleEnabled 控制是否启用**每日定时回收**。
	//
	// 默认 true：开了 GC 就默认每天定点跑一次（见 ScheduleAt）。
	ScheduleEnabled bool `yaml:"schedule_enabled"`
	// ScheduleAt 是每日定时回收的执行时间，格式 "HH:MM"（24 小时制，服务器本地时区）。
	//
	// 默认 "02:00"（凌晨两点）。
	ScheduleAt string `yaml:"schedule_at"`
	// DrainTimeout 是等待在途上传排空的超时。
	//
	// GC 期间写入被挂起，正在传的大层必须传完才能开扫，
	// 否则会删掉正在上传的层（官方文档明确警告过这个坑）。
	DrainTimeout time.Duration `yaml:"drain_timeout"`
}
