package service

import (
	"context"
	"crypto/subtle"
	"fmt"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"github.com/lau6code/lau6-cr/apps/api/internal/config"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// Submit 执行初始化。
//
// 流程：校验令牌与参数 → 写配置文件 → 建管理员 + 打标记 + 存令牌（同一事务）→
// 重建设置快照。
//
// 返回的 SetupResult 带上"被环境变量钉死、因而被忽略"的字段清单——
// 必须在结果里说，不能默默丢掉（AGENTS.md 规则 1）。
func (s *SetupService) Submit(ctx context.Context, in SetupInput) (SetupResult, error) {
	result := SetupResult{IgnoredEnvPinned: []EnvPinnedItem{}}

	initialized, err := s.isInitialized(ctx)
	if err != nil {
		return result, err
	}
	if initialized {
		return result, ErrSetupAlreadyDone
	}

	// 校验引导令牌
	//
	// 令牌由**客户端**生成并随请求带来（setup 阶段还没有任何配置文件，
	// 服务端没有可预发的令牌）。两种情形：
	//
	//	首次初始化：库里没有令牌 → 校验格式后**存下来**。
	//	重新初始化（setup reset 之后）：库里已有令牌 → 必须匹配。
	tok := strings.TrimSpace(in.BootstrapToken)
	stored, err := s.StoredBootstrapToken(ctx)
	if err != nil {
		return result, err
	}
	if stored != "" {
		if subtle.ConstantTimeCompare([]byte(stored), []byte(hashBootstrapToken(tok))) != 1 {
			return result, ErrBadBootstrapToken
		}
	} else if err := validateBootstrapToken(tok); err != nil {
		return result, err
	}

	// 公共访问 URL
	//
	// ⚠️ 被 env 钉死时（CR_PUBLIC_URL）向导**改不动**它：
	//    值由环境变量说了算，向导传什么都不作数（guard.allow 会拦下）。
	//    这时就不能再拿向导的值做校验——那一栏在 UI 上是只读的，
	//    用户没碰过它，却因为"不是 http(s) 开头"被卡住，初始化无法完成。
	//    生效值由配置期校验保证（config.Validate 要求绝对 URL），
	//    初始化的目标是"把实例配好"，不是"把 env 钉死的项再确认一遍"。
	//
	// ignored 初始化成空切片：没有项被忽略时序列化成 [] 而不是 null。
	guard := &envGuard{effective: s.settings.Snapshot(), ignored: []EnvPinnedItem{}}
	publicURLWritable := guard.allow("core.public_url", in.PublicURL)
	if publicURLWritable {
		if in.PublicURL == "" {
			return result, fmt.Errorf("public_url 不能为空")
		}
		if !strings.HasPrefix(in.PublicURL, "http://") && !strings.HasPrefix(in.PublicURL, "https://") {
			return result, fmt.Errorf("public_url 必须以 http:// 或 https:// 开头")
		}
	}

	// 校验参数
	//
	// ⚠️ 凭据只从**请求/命令行输入**来，没有"没传就退回配置"的兜底：
	//    两个来源意味着调用方说不清自己实际用了哪个。
	if err := ValidateAdminCredentials(in.AdminUsername, in.AdminPassword); err != nil {
		return result, err
	}
	username := in.AdminUsername
	password := in.AdminPassword

	// 合成新的配置
	cfg, storageExtra, err := s.composeConfig(in, guard)
	if err != nil {
		return result, err
	}

	// 数据库：可能换库（运行期热切换）
	//
	// ⚠️ 被环境变量钉死时（CR_DB_DSN / CR_DB_DRIVER / CR_DB_SQLITE_PATH）：
	//     env 的优先级最高，向导里改了**重启后就没了**——
	//     所以这里直接忽略向导的数据库字段，不换库、不写 database 段。
	//     UI 会把这一段显示成只读并说明原因（见 DatabaseInfo.EnvVar）。
	var dbRes databaseResolution
	dbSpecified := false
	dbSwitched := false
	envLocked := cfg.DatabaseEnvVar()
	if envLocked == "" {
		res, ok, err := s.resolveDatabaseTarget(in.Database)
		if err != nil {
			return result, err
		}
		dbRes, dbSpecified = res, ok
	}

	// 校验生效后的配置
	if err := s.validateSetupInput(in, cfg); err != nil {
		return result, err
	}

	// 缓存：先验通
	//
	// 选 redis 却连不上 → **初始化中止**：把配置写成 redis 而运行期连不上，
	// 重启后实例直接起不来。宁可现在失败，也不要留一个起不来的实例。
	if err := s.validateCache(ctx, cfg); err != nil {
		return result, err
	}

	// 换库（如果向导里换了数据库）
	//
	// ⚠️ 必须**先切库再建管理员**：管理员、初始化标记、引导令牌摘要
	//    都要落在新库里。反过来会留下"标记在旧库、业务在新库"的分裂状态——
	//    新库里没有 system.initialized，实例会变回未初始化、/setup 重新开放。
	//
	// 切库失败 → 旧连接原封不动，初始化中止（不会留下半切换的状态）。
	if dbSpecified {
		switched, err := s.switchDatabase(ctx, dbRes)
		if err != nil {
			return result, err
		}
		dbSwitched = switched
		if switched {
			cfg.Database.Driver = dbRes.target.Driver
			cfg.Database.SQLitePath = dbRes.target.SQLitePath
			cfg.Database.DSN = dbRes.dsn
			if dbRes.target.Driver == "sqlite" {
				cfg.Database.DSN = ""
			}
		}
	}

	// 写配置文件
	//
	// database 段只在真的换了库时写：没换就保持文件原样，
	// 免得把 env 提供的 DSN（含口令）抄进文件。
	//
	// storage 段同理：凭据只在**向导里填了**的时候写，
	// env 提供的 AK/SK 不进文件（密钥不留在备份里）。
	if err := s.writeConfig(cfg, s.databaseSection(dbRes, dbSwitched), &storageExtra); err != nil {
		return result, err
	}

	// 缓存后端热切换
	//
	// 换了缓存后端就立刻重建并切换，不等重启容器。
	//
	// ⚠️ 顺序在写配置**之后**：写配置失败就什么都没换；
	//    切不成功则报错——配置已经是 redis 了，运行期却还连着旧的 local，
	//    那是"改了不生效"，必须说出来。
	if err := s.switchCache(ctx, cfg); err != nil {
		return result, err
	}

	// 建管理员 + 打标记 + 存令牌（同一事务）
	//
	// ⚠️ 用 store.Tx 而不是裸 sql.Tx：sql.Tx 不做占位符改写，
	//    PostgreSQL 需要 $1，裸 ? 会直接语法报错。
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	if err := s.commitSetup(ctx, tx, username, password, tok); err != nil {
		return result, err
	}
	// 存储配置与管理员、初始化标记、引导令牌**同一事务**落库：
	// 表是运行期真源（env > 表 > 文件 > 默认值）。
	// 少了这一步，重启后读到的还是文件里的旧值——
	// 而文件里的值可能已经被 env 钉死项盖过一遍，两边对不上。
	if err := s.writeStorageSettings(ctx, tx, cfg); err != nil {
		return result, err
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}

	// 重建设置快照（基线变了）
	//
	// ⚠️ 这里的错误必须报出来，不能吞掉：
	//    数据库事务**已经提交**（管理员已建、初始化标记已写），
	//    但运行期配置还停在旧值上——实例处在
	//    "数据库已初始化、内存配置是旧的"的分裂状态。
	//    这时候回一句"初始化完成"是假成功（AGENTS.md 规则 1 / 3）。
	if err := s.settings.SetBase(cfg); err != nil {
		return result, fmt.Errorf("%w: %w", ErrSetupConfigReloadFailed, err)
	}
	// ⚠️ 这里**不**重启 distribution：重渲染要读设置快照，
	//    而重载钩子（server 层 reloadAfterSetup）在 Submit 返回之后才跑，
	//    那时快照已经是新的了。在这儿重启等于重启两次。
	result.IgnoredEnvPinned = guard.ignored
	return result, nil
}

// composeConfig 把向导提交的配置合成进当前生效的配置。
//
// 返回要写进配置文件的存储附加内容（凭据等非托管设置）。
//
// ⚠️ **被环境变量钉死的项一律不改**：
//
//	env 的优先级最高（env > system_settings > application.yaml > 默认值），
//	向导改了也是白改——运行期短暂按向导的值走，重启后 env 又盖回去，
//	出现"初始化后关、重启后开"的翻转，配置文件里还写着与 env 相反的值。
//	设置页对同一批项也是只读的（见 settings.go 的 envPinned），
//	两处口径必须一致：同一份配置不能两套规矩。
//
// 被忽略的项记进 SetupResult.IgnoredEnvPinned，由调用方回报给用户——
// 不能默默丢掉（AGENTS.md 规则 1）。
func (s *SetupService) composeConfig(
	in SetupInput, guard *envGuard,
) (*config.Config, storageFileExtra, error) {
	cfg := *s.settings.Snapshot()
	if guard.canWrite("core.public_url") {
		cfg.Core.PublicURL = in.PublicURL
	}
	if in.AnonymousRead != nil && guard.allow("access.anonymous_read_enabled", *in.AnonymousRead) {
		cfg.Access.AnonymousReadEnabled = *in.AnonymousRead
	}
	// 镜像存储：blob 放哪
	//
	// ⚠️ **storage 段在这里定下之后就不能再改**（11-storage.md §6）：
	//    改配置不会让已存在的 blob 出现在新位置，结果是
	//    "仓库记录都在、每次拉取都 blob unknown"。
	//    所以这里既要写文件（备份/迁移用），也要写表（运行期真源）。
	storageExtra, err := s.applyStorage(&cfg, in.Storage, guard)
	if err != nil {
		return nil, storageFileExtra{}, err
	}

	if in.Cache.Driver != "" && guard.allow("cache.driver", in.Cache.Driver) {
		cfg.Cache.Driver = in.Cache.Driver
	}
	if in.Cache.Local.MaxEntries > 0 {
		cfg.Cache.Local.MaxEntries = in.Cache.Local.MaxEntries
	}
	if in.Cache.Redis.Addr != "" && guard.allow("cache.redis.addr", in.Cache.Redis.Addr) {
		cfg.Cache.Redis.Addr = in.Cache.Redis.Addr
	}
	if in.Cache.Redis.Password != "" && guard.allow("cache.redis.password", in.Cache.Redis.Password) {
		cfg.Cache.Redis.Password = in.Cache.Redis.Password
	}
	if in.Cache.Redis.Prefix != "" && guard.allow("cache.redis.prefix", in.Cache.Redis.Prefix) {
		cfg.Cache.Redis.Prefix = in.Cache.Redis.Prefix
	}
	// Redis 数据库编号：选了 redis 就必须给。
	//
	// ⚠️ **没有默认 0**：共享 Redis 上 db 0 常常是别的应用在用，
	//    默默用 0 等于把签名密钥写进别人的库（12-cache.md §2）。
	//    所以这里报错，而不是兜底成 0（AGENTS.md 规则 1）。
	//
	// ⚠️ 被环境变量钉死时（CR_REDIS_DB）向导**不用**给：
	//    值由环境变量说了算，向导给什么都不作数。
	if in.Cache.Driver == "redis" && in.Cache.Redis.DB == nil && guard.canWrite("cache.redis.db") {
		return nil, storageFileExtra{}, fmt.Errorf(
			"选择 redis 缓存时必须指定数据库编号（cache.redis.db）：共享实例上 db 0 常被别人占用")
	}
	if in.Cache.Redis.DB != nil {
		if err := validateRedisDB(*in.Cache.Redis.DB); err != nil {
			return nil, storageFileExtra{}, err
		}
		if guard.allow("cache.redis.db", *in.Cache.Redis.DB) {
			cfg.Cache.Redis.DB = *in.Cache.Redis.DB
		}
	}

	// LDAP 段：**没传就不碰**（CLI 路径根本不问 LDAP）。
	//
	// ⚠️ 这与"显式关掉 LDAP"是两回事：向导里把开关关掉是**有意**，
	//    必须生效（只要没被 env 钉死）；请求里根本没这段 = 没这个意图。
	if in.LDAP != nil {
		if guard.allow("ldap.enabled", in.LDAP.Enabled) {
			cfg.LDAP.Enabled = in.LDAP.Enabled
		}
		// ⚠️ LDAP **关着**时，细节字段在向导里是**看不见**的
		//    （模板按 enabled 展开）——用户没机会改它们。
		//
		//    不写、不报：拿看不见的字段去覆盖生效值，
		//    还报成"向导里的值被忽略"，都是噪声。
		//    （LDAP 关着时这些字段本就不起作用，留着原值更诚实。）
		if in.LDAP.Enabled {
			// ⚠️ 文本字段：**向导里的空串不抹掉已有值**。
			//    否则会出现"向导没法完成初始化"：env 把 LDAP 打开了
			//    （CR_LDAP_ENABLED=true），URL 却来自配置文件——
			//    向导里没填 URL（空串）就把配置文件里的 URL 抹了，
			//    紧接着的校验报"启用 LDAP 时 url 不能为空"，初始化直接卡死。
			//
			//    空串只意味着"我没填"，不意味着"把它清空"——
			//    要清空这些字段去设置页（与同一函数里 cache.* 的处理一致）。
			if in.LDAP.URL != "" && guard.allow("ldap.url", in.LDAP.URL) {
				cfg.LDAP.URL = in.LDAP.URL
			}
			if in.LDAP.DC != "" && guard.allow("ldap.dc", in.LDAP.DC) {
				cfg.LDAP.DC = in.LDAP.DC
			}
			if in.LDAP.UserSegment != "" && guard.allow("ldap.user_segment", in.LDAP.UserSegment) {
				cfg.LDAP.UserSegment = in.LDAP.UserSegment
			}
			if guard.allow("ldap.start_tls", in.LDAP.StartTLS) {
				cfg.LDAP.StartTLS = in.LDAP.StartTLS
			}
			if guard.allow("ldap.allow_create", in.LDAP.AllowCreate) {
				cfg.LDAP.AllowCreate = in.LDAP.AllowCreate
			}
			if in.LDAP.TimeoutSec > 0 && guard.allow("ldap.timeout_sec", in.LDAP.TimeoutSec) {
				cfg.LDAP.TimeoutSec = in.LDAP.TimeoutSec
			}
		}
	}
	return &cfg, storageExtra, nil
}

// validateSetupInput 校验合成后的配置能不能用。
//
// ⚠️ 校验的是**生效后**的值：被 env 钉死的项用的是 env 里的值，
// 向导填的非法值不该把它们带偏。
//
// 调用点在配置合成之后——LDAP 与缓存这两项要看合成之后的结果。
func (s *SetupService) validateSetupInput(in SetupInput, cfg *config.Config) error {
	if cfg.LDAP.Enabled {
		if cfg.LDAP.URL == "" {
			return fmt.Errorf("启用 LDAP 时 url 不能为空")
		}
		if !strings.Contains(cfg.LDAP.UserSegment, "{0}") {
			return fmt.Errorf("ldap.user_segment 必须包含 {0} 占位符")
		}
	}
	// redis 缓存必须填地址——与设置页、缓存探测走同一处校验（validateRedisAddr）。
	if err := validateRedisAddr(cfg); err != nil {
		return err
	}
	return nil
}

// commitSetup 在给定事务里建管理员、写初始化标记与引导令牌摘要。
//
// ⚠️ 三者必须落在**同一个事务**里：要么都成功，要么都不存在
// （半截状态会让后续 reset 校验对不上）。
//
// 存的是 sha256 摘要，不是明文：库里不该躺着一个能直接用的凭证。
func (s *SetupService) commitSetup(
	ctx context.Context, tx *store.Tx, username, password, tok string,
) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("散列管理员口令: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO users
		(username, password_hash, source, display_name, is_system_admin, disabled, created_at, updated_at)
		VALUES (?, ?, 'local', ?, 1, 0, ?, ?)`,
		username, string(hash), username, store.Now(), store.Now()); err != nil {
		return fmt.Errorf("创建系统管理员: %w", err)
	}
	if err := tx.UpsertSetting(ctx, "system.initialized", true, "setup"); err != nil {
		return fmt.Errorf("写初始化标记: %w", err)
	}
	if err := tx.UpsertSetting(ctx, SettingKeyBootstrapToken, hashBootstrapToken(tok), "setup"); err != nil {
		return fmt.Errorf("写引导令牌: %w", err)
	}
	return nil
}

// writeStorageSettings 把存储配置写进 system_settings。
//
// ⚠️ external 形态（distribution 独立部署）**不写**：
//
//	那时 blob 存不存对象存储是上游 distribution 自己的事，
//	lau6-cr 这边没有 blob 可放——写进去就是"配置看着生效、实际没人读"。
//
// ⚠️ 只写**当前后端用到**的键：选了 local 就别写 s3 的空值，
//
//	否则设置页会显示"来源：数据库、值：空"，
//	看着像配过、其实是空的。
func (s *SetupService) writeStorageSettings(ctx context.Context, tx *store.Tx, cfg *config.Config) error {
	if !s.settings.DistributionEmbedded() {
		return nil
	}
	if err := tx.UpsertSetting(ctx, "storage.driver", cfg.Storage.Driver, "setup"); err != nil {
		return fmt.Errorf("写存储后端: %w", err)
	}
	if cfg.Storage.Driver != "s3" {
		// 从 s3 换回 local（或首次初始化就是 local）：
		// 把 s3 的行删掉，免得留着一份"看起来生效、其实用不上"的配置。
		for _, key := range []string{
			"storage.s3.endpoint", "storage.s3.bucket",
			"storage.s3.region", "storage.s3.path_style",
		} {
			if err := tx.DeleteSetting(ctx, key); err != nil {
				return fmt.Errorf("清理存储配置 %s: %w", key, err)
			}
		}
		return nil
	}
	rows := []struct {
		key   string
		value any
	}{
		{"storage.s3.endpoint", cfg.Storage.S3.Endpoint},
		{"storage.s3.bucket", cfg.Storage.S3.Bucket},
		{"storage.s3.region", cfg.Storage.S3.Region},
		{"storage.s3.path_style", cfg.Storage.S3.PathStyle},
	}
	for _, row := range rows {
		if err := tx.UpsertSetting(ctx, row.key, row.value, "setup"); err != nil {
			return fmt.Errorf("写存储配置 %s: %w", row.key, err)
		}
	}
	return nil
}

// writeConfig 把配置写到 application.yaml。
//
// database 不为 nil 时额外写 database 段（换过库时用）。
// storage 不为 nil 时额外写 storage 段里的非托管设置（根目录、凭据）。
func (s *SetupService) writeConfig(
	cfg *config.Config, database *config.DatabaseConfig, storage *storageFileExtra,
) error {
	if s.yamlPath == "" {
		return nil
	}
	m := &SettingsManager{yamlPath: s.yamlPath}
	if err := m.exportYAMLDoc(cfg, database, storage); err != nil {
		return fmt.Errorf("写配置 %s: %w", s.yamlPath, err)
	}
	return nil
}
