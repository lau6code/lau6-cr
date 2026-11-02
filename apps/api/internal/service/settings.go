package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/lau6code/lau6-cr/apps/api/internal/config"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// ErrUnknownSetting 表示这个设置项**不存在**。
//
// 最常见的原因是设置项被删了，而老实例的 system_settings 表里还留着那行
// ——那不是故障，是遗留数据。
var ErrUnknownSetting = errors.New("未知的设置项")

// Tier 是设置的生效级别。
type Tier string

const (
	// TierHot 立即生效（内存快照切换）。
	TierHot Tier = "T0"
	// TierRegistry 需要重启 distribution（重渲染配置 + restart registry）。
	TierRegistry Tier = "T1"
	// TierRestart 需要重启容器。
	TierRestart Tier = "T2"
)

// SettingDef 描述一个可配置项。
type SettingDef struct {
	Key      string
	Type     string // bool | int | string | duration | strings
	Default  any
	EnvVar   string
	YAMLPath []string
	Tier     Tier
	Min      *int64
	Max      *int64
	Enum     []string
	// MaxItems 是**数组长度上限**（type=strings 时生效），0 表示不限。
	//
	// ⚠️ 上限写在 schema 里，不写成单独的 YAML/env 配置项：
	//    一个"能改但没有消费点"的开关就是假开关（AGENTS.md）。
	MaxItems    int
	Description string
	// Hint 是给"为什么不能改"的解释，只在不可编辑的项上出现。
	//
	// ⚠️ 与 Description 分开：Description 说的是"这是什么"，
	//    Hint 说的是"为什么现在动不了"——后者是用户看到禁用控件时
	//    真正想知道的东西。
	Hint   string
	Secret bool
	// Editable=false 表示只读（如存储/缓存后端类型，初始化后锁定）。
	Editable bool
}

// SettingView 是 API 返回给 UI 的单项视图。
type SettingView struct {
	Key            string `json:"key"`
	Value          any    `json:"value"`
	Default        any    `json:"default"`
	Source         string `json:"source"` // env | db | yaml | default
	Tier           Tier   `json:"tier"`
	Editable       bool   `json:"editable"`
	PendingRestart bool   `json:"pending_restart"`
	EnvVar         string `json:"env_var,omitempty"`
	// Type 是设置项的类型（bool | int | string | duration | strings | time）。
	//
	// 前端靠它决定渲染哪种控件——比如 "time" 渲染时间选择器而不是文本框。
	Type        string         `json:"type,omitempty"`
	Enum        []string       `json:"enum,omitempty"`
	Constraints map[string]any `json:"constraints,omitempty"`
	Description string         `json:"description,omitempty"`
	// Hint 是"为什么不能改"的解释（只在不可编辑的项上有值）。
	Hint      string `json:"hint,omitempty"`
	UpdatedAt int64  `json:"updated_at,omitempty"`
	UpdatedBy string `json:"updated_by,omitempty"`
}

// SettingsView 是设置页的完整响应。
type SettingsView struct {
	Version        int64         `json:"version"`
	ExportedAt     int64         `json:"exported_at"`
	ExportStale    bool          `json:"export_stale"`
	PendingRestart []string      `json:"pending_restart"`
	Settings       []SettingView `json:"settings"`
}

// SettingsManager 管理可热改的配置。
//
// 优先级：环境变量 > system_settings 表 > application.yaml > 内置默认值。
// **运行时读的是内存快照**，不读表也不读文件。
type SettingsManager struct {
	db          *store.DB
	base        *config.Config
	yamlPath    string
	snapshot    atomic.Pointer[config.Config]
	mu          sync.Mutex
	exportedAt  int64
	exportStale bool
	// pendingMu 只护 pending 这张表。
	//
	// ⚠️ 不能用 mu 护它：View 在 Apply 持有 mu 期间被调用（Apply 末尾返回视图），
	//    而 View 自己也要读 pending——同一个 mutex 会在 Apply 里自锁。
	pendingMu sync.Mutex
	pending   map[string]bool
	// distributionEmbedded：distribution 是否与本进程同容器。
	//
	// 决定 storage.* 这几项能不能改（见 storageSettingKeys）。
	distributionEmbedded bool
	// logger 可选：用于把"设置项被跳过"这类内部问题记下来。
	logger *slog.Logger
}

// SettingsOption 是设置管理器的可选依赖。
type SettingsOption func(*SettingsManager)

// WithLogger 让设置管理器把内部问题（非法值被跳过等）记进日志。
//
// 没有它，"跳过"就是静默的：设置项在页面上显示已保存，实际没生效，
// 而没有任何痕迹可查。
func WithLogger(l *slog.Logger) SettingsOption {
	return func(m *SettingsManager) { m.logger = l }
}

// WithDistributionEmbedded 告诉设置管理器 distribution 的部署形态。
//
// embedded：lau6-cr 自己渲染 distribution 的配置（internal/distribution/render.go），
// storage.s3.* 改完能重渲染 + 重启子进程使其生效 → 可以编辑。
//
// external：distribution 用它自己那份配置文件，lau6-cr 渲染不了也重启不了它。
// 那时 storage.* 就是"页面能改、实际没人读"的假开关，
// 所以设置页不展示、接口也拒收。
//
// ⚠️ 默认值是 false（按 external 处理）：宁可少给一个入口，
//
//	也不能给一个改了不生效的入口。调用方必须显式声明形态。
func WithDistributionEmbedded(embedded bool) SettingsOption {
	return func(m *SettingsManager) { m.distributionEmbedded = embedded }
}

func (m *SettingsManager) DistributionEmbedded() bool { return m.distributionEmbedded }

func (m *SettingsManager) markPending(key string) {
	m.pendingMu.Lock()
	defer m.pendingMu.Unlock()
	m.pending[key] = true
}

func (m *SettingsManager) clearPending(key string) {
	m.pendingMu.Lock()
	defer m.pendingMu.Unlock()
	delete(m.pending, key)
}

func (m *SettingsManager) pendingSnapshot() map[string]bool {
	m.pendingMu.Lock()
	defer m.pendingMu.Unlock()
	out := make(map[string]bool, len(m.pending))
	for k, v := range m.pending {
		out[k] = v
	}
	return out
}

// MarkPendingRestart 把指定设置项标记成"重启后生效"。
//
// 用于"设置已保存、运行期却没能热切换"的场景（热切换失败时）：
// 这些项会被视图列进 pending_restart，设置页据此显示"重启后生效"——
// 否则用户只会看到"已保存"，以为改动当场就管用了。
func (m *SettingsManager) MarkPendingRestart(keys ...string) {
	m.pendingMu.Lock()
	defer m.pendingMu.Unlock()
	for _, k := range keys {
		m.pending[k] = true
	}
}

// ClearPendingRestart 撤掉"重启后生效"标记。
//
// 热切换成功时调用：那一项已经在运行期生效了，再挂着待重启的标记就是假提示。
func (m *SettingsManager) ClearPendingRestart(keys ...string) {
	m.pendingMu.Lock()
	defer m.pendingMu.Unlock()
	for _, k := range keys {
		delete(m.pending, k)
	}
}

// ChangedSettingKeys 返回 old 与 new 两份配置之间取值不同、且键名以 prefix 开头的设置项。
//
// 取值比较走 readSetting —— 键与配置字段的映射只有 AllSettingDefs 一份，
// 这里不另建映射，避免两处漂移。
func ChangedSettingKeys(old, new *config.Config, prefix string) []string {
	var out []string
	for _, def := range AllSettingDefs() {
		if !strings.HasPrefix(def.Key, prefix) {
			continue
		}
		if !reflect.DeepEqual(readSetting(old, def.Key), readSetting(new, def.Key)) {
			out = append(out, def.Key)
		}
	}
	return out
}

// NewSettingsManager 构造设置管理器。
func NewSettingsManager(db *store.DB, base *config.Config, yamlPath string, opts ...SettingsOption) (*SettingsManager, error) {
	m := &SettingsManager{
		db:       db,
		base:     base,
		yamlPath: yamlPath,
		pending:  map[string]bool{},
	}
	for _, opt := range opts {
		opt(m)
	}
	cfg, err := m.rebuild()
	if err != nil {
		return nil, err
	}
	m.snapshot.Store(cfg)
	return m, nil
}

func (m *SettingsManager) Snapshot() *config.Config {
	return m.snapshot.Load()
}

// ExportedAt 返回最近一次成功导出 application.yaml 的时间（unix 秒）。
//
// 0 表示还没成功导出过。
func (m *SettingsManager) ExportedAt() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.exportedAt
}

func (m *SettingsManager) YAMLPath() string {
	return m.yamlPath
}

// SetBase 替换配置基线并重算快照。
//
// setup 提交后会调用它——基线（配置文件）变了，
// 但 DB 覆盖层仍要重新叠加上去。
func (m *SettingsManager) SetBase(cfg *config.Config) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.base = cfg
	newCfg, err := m.rebuild()
	if err != nil {
		return err
	}
	m.snapshot.Store(newCfg)
	return nil
}

func (m *SettingsManager) Policy() Policy {
	cfg := m.Snapshot()
	return Policy{
		PreventTagOverwrite:    cfg.Registry.PreventTagOverwrite,
		TagOverwriteExclusions: cfg.Registry.TagOverwriteExclusions,
	}
}

func (m *SettingsManager) rebuild() (*config.Config, error) {
	return m.rebuildFrom(context.Background(), m.db)
}

// settingsReader 是设置行的读取口。
//
// *store.DB 与 *store.Tx 都满足它。
type settingsReader interface {
	AllSettings(ctx context.Context) (map[string]store.SettingValue, error)
}

// rebuildFrom 从指定来源读覆盖层并重算配置。
//
// ⚠️ source 可以是**事务句柄**：Apply 用它把"写库 + 重算"变成一个原子步骤——
//
//	重算失败就回滚，不会留下"库里已落库、运行期快照还是旧值"的分裂状态。
func (m *SettingsManager) rebuildFrom(ctx context.Context, source settingsReader) (*config.Config, error) {
	// 复制一份，避免污染 base。
	//
	// ⚠️ 浅拷贝共享切片底层数组，但覆盖层是**整体替换**字段（见 setFieldValue），
	//    不会就地改元素——base 的切片不受影响。
	cp := *m.base

	settings, err := source.AllSettings(ctx)
	if err != nil {
		return nil, err
	}
	for key, s := range settings {
		// 内部键不是配置项：它们由各自的子系统直接读 DB
		// （初始化标记、引导令牌、汇总游标），
		// 投影进配置快照只会得到"改了没人读"的字段。
		if internalSettingKeys[key] {
			continue
		}
		// env 优先于 DB：被环境变量钉死的键，表里的行**不参与覆盖**。
		//
		// ⚠️ 这条优先级必须在这里落地，不能只在页面上显示"来源：环境变量"：
		//    先在设置页存过某项、之后才在 compose 里加同名环境变量时，
		//    DB 行会盖掉 env 值——页面显示"来源：环境变量"、实际生效的却是库里的旧值，
		//    realm 这类由配置派生的东西就会拿着旧值继续对外服务。
		if def, ok := defByKey(key); ok && envPinned(def) {
			// 留痕：这一行还在库里但已经不生效，运维看不到它就不知道为什么改了没用。
			if m.logger != nil {
				m.logger.Info("设置项由环境变量固定，已忽略数据库中的值",
					"key", key, "env", def.EnvVar, "db_value", s.Value)
			}
			continue
		}
		if err := applySetting(&cp, key, s.Value); err != nil {
			// 数据库里存了非法值（可能是手工改的）——跳过并记录，不让整个服务起不来。
			//
			// ⚠️ "记录"不是可选的：静默跳过等于"这个设置没生效"却没有任何痕迹。
			// 老实例的 system_settings 表里还会留着**已删除的设置项**——
			// 它们同样走这条路，运维看到这条日志才知道那个开关已经不存在了。
			if m.logger != nil {
				if errors.Is(err, ErrUnknownSetting) {
					// 设置项已经不存在（多半是它被删了，库里还留着老行）。
					// 这是预期内的遗留数据，不是故障——但要留下痕迹，
					// 否则运维不知道"那个开关已经没了"。
					m.logger.Warn("设置项已不存在，已跳过（老实例残留）", "key", key)
				} else {
					m.logger.Error("设置项无法应用，已跳过", "key", key, "value", s.Value, "err", err)
				}
			}
			continue
		}
	}
	return &cp, nil
}

// applySetting 把一个设置值写进配置副本。
//
// storage.* 已经是正经设置项了（embedded 形态下 lau6-cr 渲染 distribution 的配置，
// 见 internal/distribution/render.go）。
func applySetting(cfg *config.Config, key string, v any) error {
	def, ok := defByKey(key)
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownSetting, key)
	}
	f, ok := config.FieldByYAMLPath(cfg, def.YAMLPath)
	if !ok {
		return fmt.Errorf("%w: %s", ErrUnknownSetting, key)
	}
	return setFieldValue(f, v)
}

// View 构造设置页视图。
func (m *SettingsManager) View(ctx context.Context) (*SettingsView, error) {
	version, err := m.db.GlobalSettingsVersion(ctx)
	if err != nil {
		return nil, err
	}
	dbSettings, err := m.db.AllSettings(ctx)
	if err != nil {
		return nil, err
	}
	cfg := m.Snapshot()
	view := &SettingsView{
		Version:        version,
		ExportedAt:     m.exportedAt,
		ExportStale:    m.exportStale,
		PendingRestart: []string{},
	}
	for key := range m.pendingSnapshot() {
		view.PendingRestart = append(view.PendingRestart, key)
	}
	for _, def := range AllSettingDefs() {
		// external 形态下 storage.* 根本不出现（storageSettingKeys 的说明）：
		// 那时 distribution 用它自己的配置，这里显示出来的值对上游没有意义，
		// 摆一个"改了也没人读"的入口就是假开关。
		if storageSettingKeys[def.Key] && !m.distributionEmbedded {
			continue
		}
		v := SettingView{
			Key:         def.Key,
			Default:     def.Default,
			Tier:        def.Tier,
			Editable:    def.Editable,
			EnvVar:      def.EnvVar,
			Type:        def.Type,
			Enum:        def.Enum,
			Description: def.Description,
			Hint:        def.Hint,
		}
		if def.Min != nil || def.Max != nil {
			v.Constraints = map[string]any{}
			if def.Min != nil {
				v.Constraints["min"] = *def.Min
			}
			if def.Max != nil {
				v.Constraints["max"] = *def.Max
			}
		}
		// 来源判定：env > db > yaml > default
		val, source := m.effectiveValue(cfg, def, dbSettings)
		if def.Secret && val != nil && source != "default" {
			// 敏感项不回显具体值，只回显"已设置"。
			val = map[string]any{"set": true}
		}
		v.Value = val
		v.Source = source
		if s, ok := dbSettings[def.Key]; ok {
			v.UpdatedAt = s.UpdatedAt
			v.UpdatedBy = s.UpdatedBy
		}
		view.Settings = append(view.Settings, v)
	}
	return view, nil
}

// envPinned 判断某个设置项是否被环境变量钉死。
//
// 判定口径与 Apply 的拒绝逻辑一致：**变量存在即钉死**（含空串），
// 两处口径必须一样——否则会出现"页面说不可改、实际又能改"的分裂。
//
// env 的优先级最高（env > db > yaml > default，见包注释）：
// 被钉死的键，DB 行不参与覆盖，见 rebuild。
func envPinned(def SettingDef) bool {
	if def.EnvVar == "" {
		return false
	}
	_, ok := os.LookupEnv(def.EnvVar)
	return ok
}

// envPinnedKeys 返回当前被环境变量钉死的全部设置项。
//
// 口径与 envPinned 一致（变量存在即钉死）。
// 初始化向导用它把对应字段显示成只读——
// 设置页能告诉用户"这项被 env 固定"，向导也得能。
func envPinnedKeys() []EnvPinnedItem {
	out := []EnvPinnedItem{}
	for _, def := range AllSettingDefs() {
		if envPinned(def) {
			out = append(out, EnvPinnedItem{Key: def.Key, EnvVar: def.EnvVar})
		}
	}
	return out
}

// effectiveValue 算出某设置的有效值与来源。
func (m *SettingsManager) effectiveValue(cfg *config.Config, def SettingDef, dbSettings map[string]store.SettingValue) (any, string) {
	if def.EnvVar != "" {
		if _, ok := os.LookupEnv(def.EnvVar); ok {
			// ⚠️ 这里返回的是**快照里的值**：rebuild 已经跳过了被 env 钉死的 DB 行，
			//    快照里存的就是 env 的值。读 DB 行会拿到"页面不生效的那个旧值"。
			return readSetting(cfg, def.Key), "env"
		}
	}
	if s, ok := dbSettings[def.Key]; ok {
		return s.Value, "db"
	}
	// 配置文件里没写这一项时，快照里是 config.Default() 的值——
	// 对用户来说同样是"配置文件说的"，标成 yaml。
	return readSetting(cfg, def.Key), "yaml"
}

func readSetting(cfg *config.Config, key string) any {
	def, ok := defByKey(key)
	if !ok {
		return nil
	}
	f, ok := config.FieldByYAMLPath(cfg, def.YAMLPath)
	if !ok {
		return nil
	}
	return readFieldValue(f)
}

// validateCacheCombo 校验缓存后端与 Redis 地址的组合。
//
// 单看每个字段都合规是不够的：driver=redis 本身是个合法值，
// 但没有地址的 redis 后端在启动时会让 cache.New 直接报错。
//
// base 取当前生效配置（已叠加 DB 覆盖层），changes 是本次要改的值。
func validateCacheCombo(base *config.Config, changes map[string]any) error {
	cp, err := applyChanges(base, changes,
		"cache.driver", "cache.redis.addr", "cache.redis.password", "cache.redis.prefix")
	if err != nil {
		return err
	}
	return validateRedisAddr(cp)
}

// validateGCCombo 校验 blob 回收的开关与存储根目录组合。
//
// 开了 GC 却没填存储根目录 = lau6-cr 没有直连 distribution 存储的路径，
// GC 根本无法执行——必须在保存设置时就拒绝。
//
// base 取当前生效配置（已叠加 DB 覆盖层），changes 是本次要改的值。
func validateGCCombo(base *config.Config, changes map[string]any) error {
	cp, err := applyChanges(base, changes,
		"cleanup.gc.enabled", "cleanup.gc.delete_untagged",
		"cleanup.gc.dry_run", "cleanup.gc.trigger_after_cleanup", "cleanup.gc.drain_timeout")
	if err != nil {
		return err
	}
	if cp.Cleanup.GC.Enabled && cp.Registry.Mode == config.ModeExternal &&
		strings.TrimSpace(cp.Registry.DistributionConfigPath) == "" {
		return fmt.Errorf("cleanup.gc.enabled=true 时，独立部署 registry 的形态必须配置" +
			" registry.distribution_config_path（registry 自己的配置文件路径，blob 回收靠它调度 distribution）")
	}
	if cp.Cleanup.GC.Enabled && cp.Cleanup.GC.DrainTimeout <= 0 {
		return fmt.Errorf("cleanup.gc.drain_timeout 必须大于 0")
	}
	return nil
}

// validateLoggingCombo 校验日志输出目标与文件开关的组合。
//
// 与 validateCacheCombo 同理：单看每个字段都合规是不够的。
// output=file/both 但 enabled=false 是个合法的单字段组合，
// 但进程下次启动会直接失败——报错发生在容器启动阶段，
// 与设置页毫无关系，只能靠猜。
//
// base 取当前生效配置（已叠加 DB 覆盖层），changes 是本次要改的值。
func validateLoggingCombo(base *config.Config, changes map[string]any) error {
	cp, err := applyChanges(base, changes,
		"logging.level", "logging.output", "logging.file.enabled",
		"logging.file.dir", "logging.file.filename", "logging.file.max_size_mb",
		"logging.file.rotate", "logging.file.retain_days")
	if err != nil {
		return err
	}
	output := strings.ToLower(strings.TrimSpace(cp.Logging.Output))
	if output == "" {
		output = "both"
	}
	if output != "console" && !cp.Logging.File.Enabled {
		return fmt.Errorf("logging.output=%s 时必须同时开启 logging.file.enabled", output)
	}
	return nil
}

// Apply 应用一批设置修改。
//
// 流程：校验 → 乐观锁 → 写 DB → 重算快照 → 导出 YAML。
// 校验不通过或写库失败时，**运行中的配置完全不变**。
func (m *SettingsManager) Apply(ctx context.Context, actor string, version int64, changes map[string]any) (*SettingsView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	current, err := m.db.GlobalSettingsVersion(ctx)
	if err != nil {
		return nil, err
	}
	if version != current {
		return nil, ErrVersionConflict
	}

	// ① 校验
	for key, val := range changes {
		if internalSettingKeys[key] {
			return nil, fmt.Errorf("设置项 %s 是内部键，不对外暴露也不能修改", key)
		}
		def, ok := defByKey(key)
		if !ok {
			return nil, fmt.Errorf("未知的设置项: %s", key)
		}
		if !def.Editable {
			return nil, fmt.Errorf("设置项 %s 只读，不可修改", key)
		}
		// external 形态下 storage.* 不可改：distribution 用它自己的配置文件，
		// lau6-cr 渲染不了也重启不了它——改了就是"页面能改、实际没人读"。
		if storageSettingKeys[key] && !m.distributionEmbedded {
			return nil, fmt.Errorf("设置项 %s 在 external 形态下不可修改："+
				"distribution 用它自己的配置文件，存储参数请改那个容器", key)
		}
		// 被环境变量钉死的项不允许在设置页改：
		// 改了也存不进生效值（rebuild 会跳过被 env 固定的 DB 行），
		// 页面上却显示"已保存"——那是纯粹的假象。
		if envPinned(def) {
			return nil, fmt.Errorf("设置项 %s 由环境变量 %s 固定，无法在此修改", key, def.EnvVar)
		}
		if err := validateValue(def, val); err != nil {
			return nil, err
		}
	}
	// 跨字段校验：缓存后端与 Redis 地址是一对，不能拆开看。
	//
	// 少了这条，运维在页面上把后端切成 redis 却没填地址——
	// 值会被导出进 application.yaml，下次重启时 config.validate 直接失败，
	// 服务起不来（boot loop），而报错跟设置页毫无关系。
	if err := validateCacheCombo(m.Snapshot(), changes); err != nil {
		return nil, err
	}
	// 跨字段校验：输出目标与文件开关是一对。
	//
	// output=file/both 但 enabled=false 时，进程下次启动会因为
	// "logging.output=file 要求 logging.file.enabled=true" 直接起不来——
	// 而报错发生在容器启动阶段，跟设置页毫无关系，只能靠猜。
	if err := validateLoggingCombo(m.Snapshot(), changes); err != nil {
		return nil, err
	}
	// 跨字段校验：GC 开关与存储根目录是一对。
	//
	// 开了 GC 却没填存储根目录 = lau6-cr 没有直连 distribution 存储的路径，
	// GC 根本无法执行。与其等到点"回收"按钮才报错，
	// 不如在保存设置时就说清楚。
	if err := validateGCCombo(m.Snapshot(), changes); err != nil {
		return nil, err
	}
	// 存储后端与 S3 连接参数的组合**不在**这里校验：
	// storage.* 全部只读，Apply 收不到它们（11-storage.md §6）。
	// driver=s3 却没有 endpoint / bucket 的组合由两处把关：
	// setup 提交时（applyStorage）与启动时（config.validate）。

	// ② 写 DB + 重算快照（**同一事务**）
	//
	// ⚠️ 必须是一个事务：先写库再重算的话，重算失败（读库失败）时
	//    函数返回错误、快照不更新，但 DB 行**已经落库**了——
	//    库里是新值、运行期是旧值，两边分裂，且没有事务能回滚。
	//    放进同一个事务后：重算在提交**之前**完成，失败就整体回滚。
	tx, err := m.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for key, val := range changes {
		if err := tx.UpsertSetting(ctx, key, val, actor); err != nil {
			return nil, err
		}
	}
	newCfg, err := m.rebuildFrom(ctx, tx)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	m.snapshot.Store(newCfg)

	// ④ 标记待重启项
	for key := range changes {
		if def, ok := defByKey(key); ok && def.Tier != TierHot {
			m.markPending(key)
		}
	}

	// ⑤ 导出 YAML（非致命，但失败要告警）
	if err := m.exportYAML(newCfg); err != nil {
		m.exportStale = true
		// ⚠️ 导出失败**不阻断**设置生效：DB 与内存快照已经是新值，
		//    界面上的 export_stale 告警条就是给用户的信号。
		//    但日志必须留下痕迹，否则"配置文件没跟上"只剩一个界面提示，
		//    事后查不出原因（AGENTS.md 规则 3）。
		if m.logger != nil {
			m.logger.Error("导出 application.yaml 失败，配置文件内容已过期",
				"path", m.yamlPath, "err", err)
		}
	} else {
		m.exportStale = false
		m.exportedAt = store.Now()
	}
	return m.View(ctx)
}

// Reset 把某设置恢复默认（删掉 DB 行）。
//
// ⚠️ 删除与重算也在**同一事务**里：重算失败就回滚，
//
//	否则会留下"库里行没了、运行期快照还是旧值"的分裂状态。
func (m *SettingsManager) Reset(ctx context.Context, actor, key string) (*SettingsView, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	def, ok := defByKey(key)
	if !ok {
		if internalSettingKeys[key] {
			return nil, fmt.Errorf("设置项 %s 是内部键，不对外暴露也不能修改", key)
		}
		return nil, fmt.Errorf("未知的设置项: %s", key)
	}
	// ⚠️ 只读的项连"恢复默认"都不行：
	//    删掉 DB 行 = 回到配置文件/默认值 = 换了一个存储位置，
	//    效果和直接改一样（11-storage.md §6）。
	if !def.Editable {
		if def.Hint != "" {
			return nil, fmt.Errorf("设置项 %s 只读，不可恢复默认：%s", key, def.Hint)
		}
		return nil, fmt.Errorf("设置项 %s 只读，不可恢复默认", key)
	}
	// external 形态下 storage.* 不可改（与 Apply 同一口径）：
	// 恢复默认也是改——它同样会让 lau6-cr 的配置与上游对不上。
	if storageSettingKeys[key] && !m.distributionEmbedded {
		return nil, fmt.Errorf("设置项 %s 在 external 形态下不可修改："+
			"distribution 用它自己的配置文件，存储参数请改那个容器", key)
	}
	tx, err := m.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := tx.DeleteSetting(ctx, key); err != nil {
		return nil, err
	}
	newCfg, err := m.rebuildFrom(ctx, tx)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	m.snapshot.Store(newCfg)
	m.clearPending(key)
	// ⚠️ 与 Apply 同一口径：导出失败不阻断恢复默认（DB 与快照已是新值），
	//    但必须留下日志痕迹——export_stale 只是界面上的告警条。
	if err := m.exportYAML(newCfg); err != nil {
		m.exportStale = true
		if m.logger != nil {
			m.logger.Error("导出 application.yaml 失败，配置文件内容已过期",
				"path", m.yamlPath, "err", err)
		}
	} else {
		m.exportStale = false
		m.exportedAt = store.Now()
	}
	return m.View(ctx)
}
