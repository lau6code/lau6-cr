package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/lau6code/lau6-cr/apps/api/internal/config"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// ErrSetupAlreadyDone 表示实例已初始化。
var ErrSetupAlreadyDone = errors.New("setup already completed")

// ErrBadBootstrapToken 表示引导令牌不正确。
var ErrBadBootstrapToken = errors.New("invalid bootstrap token")

// ErrBadBootstrapTokenFormat 表示引导令牌**格式**不合法（长度不对）。
//
// 与 ErrBadBootstrapToken 分开：格式不对是请求写错了（400），
// 令牌不匹配是凭证不符（401）。两者都归到"连不上"里就说错了原因。
var ErrBadBootstrapTokenFormat = errors.New("引导令牌格式不正确")

// ErrSetupConfigReloadFailed 表示**初始化已提交**、但运行期配置重载失败。
//
// 它与"初始化失败"是两件事：数据库事务已经提交（管理员已建、标记已写），
// 内存里的配置快照却还是旧的。所以调用方必须让用户知道
// "要重启实例才能让新配置生效"，而不是当成普通的 400 参数错误。
var ErrSetupConfigReloadFailed = errors.New("初始化已提交，但运行期配置重载失败")

// SettingKeyBootstrapToken 是引导令牌在 system_settings 里的键。
//
// 存的是令牌的 **sha256 摘要**，不是明文——库里不该躺着一个能直接用的凭证。
const SettingKeyBootstrapToken = "setup.bootstrap_token"

// 引导令牌的长度约束。
//
// 下限挡住"随手敲一个 1234"这种弱令牌；上限防超长输入白占库。
const (
	bootstrapTokenMinLen = 16
	bootstrapTokenMaxLen = 128
)

// SetupService 负责首次初始化的引导流程。
type SetupService struct {
	db       *store.DB
	settings *SettingsManager
	yamlPath string

	// ReloadCache 缓存后端热切换钩子。
	//
	// 由 main.go 注入（它持有 cache.Holder）。
	// 向导里改了缓存后端 → 立刻重建并切换，不等重启容器。
	//
	// ⚠️ "不可丢"的键（签名私钥、用户密钥）由**这个钩子**统一搬，
	//    不在这里搬——设置页切后端走的是同一个钩子，两处必须一致。
	ReloadCache func(cfg *config.Config) error
}

// SetupDeps 是 SetupService 的依赖。
type SetupDeps struct {
	DB       *store.DB
	Settings *SettingsManager
	YAMLPath string
	// ReloadCache 缓存后端热切换钩子（可为 nil——那时只在配置里改，等重启生效）。
	ReloadCache func(cfg *config.Config) error
}

func NewSetupService(d SetupDeps) *SetupService {
	return &SetupService{
		db:          d.DB,
		settings:    d.Settings,
		yamlPath:    d.YAMLPath,
		ReloadCache: d.ReloadCache,
	}
}

// SetupStatus 是引导状态。
type SetupStatus struct {
	Initialized           bool     `json:"initialized"`
	SetupAvailable        bool     `json:"setup_available"`
	RequireBootstrapToken bool     `json:"require_bootstrap_token"`
	DatabaseDriver        string   `json:"database_driver"`
	StepsCompleted        []string `json:"steps_completed"`
	// EnvPinned 列出当前**被环境变量钉死**的设置项。
	//
	// ⚠️ env 的优先级最高（env > system_settings > application.yaml > 默认值），
	//    被钉死的项向导改不了——改了也是白改：运行期短暂按向导的值走，
	//    重启后 env 又盖回去，出现"初始化后关、重启后开"的翻转。
	//    向导据此把这些字段显示成只读（docs/design/08-setup.md §8）。
	EnvPinned []EnvPinnedItem `json:"env_pinned"`
	// PublicURL 是当前**生效**的公共访问 URL（env > application.yaml > 默认值）。
	//
	// ⚠️ 存在的理由：被 env 钉死时向导里的这一栏是**只读**的，
	//    用户既改不了也看不到值——一个空的只读输入框会被前端的
	//    "必须是 http:// 或 https:// 开头"校验卡住，初始化根本走不下去。
	//    把生效值给出去，向导就能把它显示出来（前端只对被钉死的项预填）。
	//
	// 它是**公开地址**（token realm 就取自它），不是敏感信息。
	PublicURL string `json:"public_url"`
	// DistributionEmbedded 报告 distribution 是否与本进程同容器。
	//
	// ⚠️ external 形态下向导**不显示存储步骤**：blob 存不存对象存储
	//    是上游 distribution 自己的事，lau6-cr 这边没有 blob 可放。
	//    显示一个"选了也没人读"的开关就是假开关（AGENTS.md 规则 1）。
	DistributionEmbedded bool `json:"distribution_embedded"`
	// StorageDriver 是当前**生效**的存储后端（env > 表 > 文件 > 默认值）。
	//
	// 被 env 钉死时向导里的这一栏是只读的，得让用户看见生效值，
	// 否则面对一个空的只读输入框会以为向导坏了。
	StorageDriver string `json:"storage_driver"`
	// CacheDriver 是当前**生效**的缓存后端（env > 表 > 文件 > 默认值）。
	//
	// ⚠️ 存在的理由：向导里的缓存后端必须显示成生效值——
	//    生效的是 redis 而向导显示 local 时，
	//    "选 redis 要填数据库编号"这条要求会被整段跳过。
	CacheDriver string `json:"cache_driver"`
	// CacheRedisDB 是当前**生效**的 Redis 数据库编号。
	//
	// ⚠️ 被 env 钉死时向导里的这一栏是**只读**的，
	//    空着的只读输入框会被当成"没填"——
	//    把生效值给出去，向导就能把它显示出来（前端只对被钉死的项预填）。
	CacheRedisDB int `json:"cache_redis_db"`
}

func (s *SetupService) Status(ctx context.Context) (*SetupStatus, error) {
	initialized, err := s.isInitialized(ctx)
	if err != nil {
		return nil, err
	}
	st := &SetupStatus{
		Initialized:    initialized,
		SetupAvailable: !initialized,
		// 未初始化就一定要带令牌（令牌由客户端生成，见 §4）。
		RequireBootstrapToken: !initialized,
		DatabaseDriver:        s.settings.Snapshot().Database.Driver,
		StepsCompleted:        []string{},
		EnvPinned:             envPinnedKeys(),
		PublicURL:             s.settings.Snapshot().Core.PublicURL,
		DistributionEmbedded:  s.settings.DistributionEmbedded(),
		StorageDriver:         s.settings.Snapshot().Storage.Driver,
		CacheDriver:           s.settings.Snapshot().Cache.Driver,
		CacheRedisDB:          s.settings.Snapshot().Cache.Redis.DB,
	}
	if initialized {
		st.StepsCompleted = []string{"database", "cache", "storage", "public_url", "admin"}
	}
	return st, nil
}

// isInitialized 判断实例是否已初始化。
//
// 依据是**显式标记**而不是"users 表为空"——
// 接了 LDAP 后，第一个目录用户登录就会让 users 表非空，
// 用表空判断会导致实例永远卡在"无法初始化"。
//
// ⚠️ **表不存在 = 一定没初始化过**，不是错误：
//
//	目标库还没建表（迁移没跑过，或跑了一半）时，
//	这个判断必须返回"未初始化"，让向导继续走下去把表建出来。
//	否则用户会看到一句莫名其妙的
//	「连接失败：relation "system_settings" does not exist」——
//	连接明明是通的，缺的是表。
func (s *SetupService) isInitialized(ctx context.Context) (bool, error) {
	v, err := s.db.GetSetting(ctx, "system.initialized")
	if err == store.ErrNotFound {
		return false, nil
	}
	if err != nil {
		if store.IsTableMissing(err) {
			return false, nil
		}
		return false, err
	}
	b, _ := v.Value.(bool)
	return b, nil
}

// hashBootstrapToken 算引导令牌的 sha256 摘要（hex）。
//
// 库里存摘要、不存明文：数据库被读走也不等于拿到可直接使用的凭证。
func hashBootstrapToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func validateBootstrapToken(tok string) error {
	if tok == "" {
		return fmt.Errorf("引导令牌不能为空")
	}
	if len(tok) < bootstrapTokenMinLen {
		return fmt.Errorf("引导令牌至少 %d 位", bootstrapTokenMinLen)
	}
	if len(tok) > bootstrapTokenMaxLen {
		return fmt.Errorf("引导令牌最多 %d 位", bootstrapTokenMaxLen)
	}
	return nil
}

// 首次初始化时不存在（返回空串）——此时库里没有令牌可比。
//
// ⚠️ **表不存在时也返回空串**（而不是报错）：
// 见表不存在的说明同上——这个库还没建表，
// 相当于"从没初始化过"，没有摘要可比。
func (s *SetupService) StoredBootstrapToken(ctx context.Context) (string, error) {
	v, err := s.db.GetSetting(ctx, SettingKeyBootstrapToken)
	if err == store.ErrNotFound {
		return "", nil
	}
	if err != nil {
		if store.IsTableMissing(err) {
			return "", nil
		}
		return "", err
	}
	sum, _ := v.Value.(string)
	return sum, nil
}

// DatabaseInput 是向导里的数据库连接信息。
//
// 提交（POST /setup）与连通性测试（POST /setup/test/database）共用同一套字段——
// 两处各写一套迟早会走偏。
//
// 入参是**结构化**的（主机 / 端口 / 用户名 / 口令 / 库名 / schema），
// 不接受整条连接串——两套拼法写在前端就是两处坑。
//
// 拼 DSN 的活儿在 store 层（有驱动依赖的一侧）：postgres 走 URL、
// mysql 走 `user:pass@tcp(host:port)/db`。
type DatabaseInput struct {
	Driver     string `json:"driver"`
	SQLitePath string `json:"sqlite_path"`
	Host       string `json:"host"`
	Port       string `json:"port"`
	Username   string `json:"username"`
	Password   string `json:"password"`
	Name       string `json:"name"`
	// Schema 只对 postgres 有意义（search_path）；mysql 里库名即 schema。
	Schema  string `json:"schema"`
	SSLMode string `json:"ssl_mode"`
}

// CacheRedisInput 是向导里的 Redis 连接参数。
type CacheRedisInput struct {
	Addr     string `json:"addr"`
	Password string `json:"password"`
	Prefix   string `json:"prefix"`
	// DB 是 Redis 数据库编号。
	//
	// ⚠️ 是**指针**：0 是合法编号，"没填"与"填了 0"必须分得开。
	//
	// 选 redis 时它**必填**（nil = 没填 = 初始化中止）：
	// 共享 Redis 上 db 0 常常是别的应用在用，
	// 默默用 0 等于把签名密钥写进别人的库（12-cache.md §2）。
	DB *int `json:"db"`
}

// CacheInput 是向导里的缓存配置。
//
// 提交（POST /setup）与连通性测试（POST /setup/test/cache）共用同一套字段。
type CacheInput struct {
	Driver string `json:"driver"`
	Local  struct {
		MaxEntries int `json:"max_entries"`
	} `json:"local"`
	Redis CacheRedisInput `json:"redis"`
}

// LDAPInput 是向导里的 LDAP 配置。
type LDAPInput struct {
	Enabled     bool   `json:"enabled"`
	URL         string `json:"url"`
	DC          string `json:"dc"`
	UserSegment string `json:"user_segment"`
	StartTLS    bool   `json:"start_tls"`
	AllowCreate bool   `json:"allow_create"`
	TimeoutSec  int    `json:"timeout_sec"`
}

// StorageInput 是向导里的镜像存储配置。
//
// 提交（POST /setup）与连通性测试（POST /setup/test/storage）共用同一套字段——
// 两处各写一套迟早会走偏。
//
// ⚠️ 这里**只有** endpoint / region / bucket / path_style 与凭据；
//
//	local 的根目录**不在向导里**（11-storage.md §4.1）：
//	它是部署的事（数据卷挂在哪），不是"选哪个后端"的一部分，
//	配在 application.yaml / 环境变量里。
//	secure 也不在这里——它由 endpoint 的 scheme 推导
//	（https → true），让用户再单独填一次就是两处可能打架的配置。
type StorageInput struct {
	Driver string `json:"driver"`
	S3     struct {
		Endpoint  string `json:"endpoint"`
		Region    string `json:"region"`
		Bucket    string `json:"bucket"`
		PathStyle bool   `json:"path_style"`
		// AccessKey / SecretKey 留空表示"走环境变量"
		// （CR_S3_ACCESS_KEY / CR_S3_SECRET_KEY）。
		AccessKey string `json:"access_key"`
		SecretKey string `json:"secret_key"`
	} `json:"s3"`
}

// StorageTestResult 是存储连通性测试的结果。
type StorageTestResult struct {
	// Detail 是给人看的一句话（"对象存储可用" / "本地目录可写"）。
	Detail string `json:"detail"`
	// LatencyMS 是探测耗时（毫秒）。
	LatencyMS int64 `json:"latency_ms"`
	// Warnings 是"能连上但有风险"的提示。
	//
	// ⚠️ 探测失败**不阻断**向导（11-storage.md §4.4）：
	//    setup 所在的管理网可能访问不到对象存储，而运行期网络是通的。
	//    所以这里给警告 + 允许继续，而不是硬拦。
	Warnings []string `json:"warnings,omitempty"`
}

// StorageTestInput 是存储连通性测试的请求参数。
type StorageTestInput struct {
	BootstrapToken string       `json:"bootstrap_token"`
	Storage        StorageInput `json:"storage"`
}

// SetupInput 是引导提交的参数。
type SetupInput struct {
	BootstrapToken string `json:"bootstrap_token"`
	PublicURL      string `json:"public_url"`
	AdminUsername  string `json:"admin_username"`
	AdminPassword  string `json:"admin_password"`
	AnonymousRead  *bool  `json:"anonymous_read"`

	Database DatabaseInput `json:"database"`
	Cache    CacheInput    `json:"cache"`

	// Storage 是镜像存储配置（blob 放哪）。
	//
	// ⚠️ **锁定不可改**（11-storage.md §6）：改配置不会让已存在的 blob
	//    出现在新位置，结果是"仓库记录都在、每次拉取都 blob unknown"。
	//
	// ⚠️ 是**指针**：nil = "这次不碰存储"。
	//    用值类型的话，"没传"与"传了 local"分不开——
	//    CLI 路径与 external 形态都不问存储，却会把 local 写进配置。
	Storage *StorageInput `json:"storage"`

	// LDAP 是 LDAP 配置段。
	//
	// ⚠️ **指针**：nil = 向导整段没碰 LDAP，保持现状；
	//    值类型会把"没填"和"显式关掉"混成同一个零值——
	//    CLI 路径（lau6-cr setup）根本不问 LDAP，
	//    用值类型会把 LDAP 悄悄关掉（哪怕 CR_LDAP_ENABLED=true 也会被覆盖）。
	LDAP *LDAPInput `json:"ldap"`
}

// EnvPinnedItem 描述一个**被环境变量钉死**的设置项。
type EnvPinnedItem struct {
	// Key 是设置项键（如 ldap.enabled）。
	Key string `json:"key"`
	// EnvVar 是钉死它的环境变量名（如 CR_LDAP_ENABLED）。
	EnvVar string `json:"env_var"`
}

// SetupResult 是初始化的结果。
type SetupResult struct {
	// IgnoredEnvPinned 列出**向导填了、却被环境变量盖掉**的设置项。
	//
	// ⚠️ 只报"真的被盖掉"的：向导里被 env 钉死的项是**只读**的
	//    （前端预填生效值并禁用输入框），用户根本没机会改它——
	//    把它报成"值被忽略"是在说一件没发生过的事。
	//
	//    剩下的那些是真有冲突：向导填的与生效的不同，
	//    用户会以为"我明明填了 LDAP 怎么没开"，必须让人看见。
	IgnoredEnvPinned []EnvPinnedItem `json:"ignored_env_pinned"`
}

// envGuard 汇总"被环境变量钉死、因而被向导忽略"的字段。
//
// 口径与设置页**完全一致**（settings.go 的 envPinned）：变量存在即钉死。
// 同一份配置不能两套规矩——设置页拒绝改 env 钉死的项，
// 向导也必须（docs/design/08-setup.md §8）。
type envGuard struct {
	// effective 是**当前生效**的配置（env > 表 > 文件 > 默认值）。
	//
	// 用途：判断向导提交的值是不是真的和生效值不同。
	effective *config.Config
	ignored   []EnvPinnedItem
}

// canWrite 判断某设置项能不能被向导改（**不记录**）。
//
// 与 allow 的判定口径完全一致，区别只是不往 ignored 里追加：
// 有些地方只需要"能不能改"这个结论（如提交前的公共 URL 校验）。
func (g *envGuard) canWrite(key string) bool {
	def, ok := defByKey(key)
	if !ok || !envPinned(def) {
		return true
	}
	return false
}

// allow 判断向导能不能写某一项。
//
// 返回 false 时：既不改配置，也不报错；**只有在真的被盖掉时**才记进 ignored。
//
// ⚠️ 记录的条件是"向导填了、且填的和生效值**不同**"：
//
//   - 空串 = 没填（与 LDAP 文本字段同一口径），不是"想清空它"；
//
//   - 只读栏预填的就是生效值，两者相同 → 没什么可忽略的。
//
//     被 env 钉死的项在向导里是**只读**的（前端预填生效值并禁用输入框），
//     用户根本没机会改它——报成"向导里的值已被忽略"是在说一件没发生过的事。
func (g *envGuard) allow(key string, submitted any) bool {
	if g.canWrite(key) {
		return true
	}
	if isUnsubmitted(submitted) || g.sameAsEffective(key, submitted) {
		return false
	}
	def, _ := defByKey(key)
	g.ignored = append(g.ignored, EnvPinnedItem{Key: key, EnvVar: def.EnvVar})
	return false
}

// sameAsEffective 判断向导提交的值与**生效值**是否相同。
func (g *envGuard) sameAsEffective(key string, submitted any) bool {
	if g.effective == nil {
		return false
	}
	return reflect.DeepEqual(readSetting(g.effective, key), submitted)
}

// isUnsubmitted 报告"向导没填这个值"。
//
// ⚠️ 只认空串：布尔与数字都有合法值（false / 0），
// 把它们当成"没填"会把用户明明关掉的开关当成他没说。
func isUnsubmitted(v any) bool {
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) == ""
}
