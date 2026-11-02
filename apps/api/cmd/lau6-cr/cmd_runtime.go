package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/lau6code/lau6-cr/apps/api/internal/cache"
	"github.com/lau6code/lau6-cr/apps/api/internal/config"
	"github.com/lau6code/lau6-cr/apps/api/internal/crypto"
	"github.com/lau6code/lau6-cr/apps/api/internal/distribution"
	"github.com/lau6code/lau6-cr/apps/api/internal/fsatomic"
	applog "github.com/lau6code/lau6-cr/apps/api/internal/log"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// distributionConfigPath 返回 distribution 实际在用的配置文件路径。
//
// 取值来自**配置**（registry.distribution_config_path）：
// 环境变量 CR_DISTRIBUTION_CONFIG 在 config.applyEnv 里映射到同一个字段，
// 优先级 env > YAML > 默认值。
//
// ⚠️ 渲染配置、拉起内嵌 distribution、跑 blob 回收三处必须拿到同一个路径：
//
//	回收按这份配置里的 storage 段去删对象，
//	路径不是同一份就会清到别的地方去（症状是"回收跑完了，桶里的东西还在"）。
func distributionConfigPath(cfg *config.Config) string {
	return cfg.Registry.DistributionConfigPath
}

// mainLogger 按配置构造日志器。
//
// 配置来自 application.yaml 与系统设置表（设置页改完即生效）。
// 写文件失败是**致命错误**：进程起来却不写日志，比起不来更难排查。
//
// 返回完整的 *applog.Logger 而不是裸的 *slog.Logger：
// 运行期调级别要靠它的 SetLevel（slog.LevelVar，原子生效）。
func mainLogger(cfg *config.Config) (*applog.Logger, error) {
	return applog.New(applog.Options{
		Level:  cfg.Logging.Level,
		Output: cfg.Logging.Output,
		File: applog.FileOptions{
			Enabled:    cfg.Logging.File.Enabled,
			Dir:        cfg.Logging.File.Dir,
			Filename:   cfg.Logging.File.Filename,
			MaxSizeMB:  cfg.Logging.File.MaxSizeMB,
			Rotate:     cfg.Logging.File.Rotate,
			RetainDays: cfg.Logging.File.RetainDays,
		},
	})
}

// runtime 是一次命令运行期的基础设施：配置、日志器、数据库。
type runtime struct {
	cfg    *config.Config
	logger *applog.Logger
	db     *store.DB
}

// openRuntime 搭起运行期基础设施：加载配置、建日志器、开库并跑迁移。
//
// serve 与 setup 走同一套启动流程，配置/日志/数据库的失败一律致命退出——
// 两处各写一份必然漂移。
//
// beforeLogger 在日志器建好**之前**跑：那时还没有日志可写，
// 校验失败只能往 stderr 写。为 nil 时不校验。
func openRuntime(beforeLogger func(*config.Config)) *runtime {
	// ⚠️ 顺序：先加载配置，再建日志器——日志器的输出目标/级别来自配置。
	//
	// 但配置加载本身失败时还没有日志器，只能退回 stderr。
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载配置失败: %v\n", err)
		os.Exit(1)
	}
	if beforeLogger != nil {
		beforeLogger(cfg)
	}
	logger, err := mainLogger(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "初始化日志失败: %v\n", err)
		os.Exit(1)
	}
	// 包级 slog 调用（slog.Error 等）也走同一个 logger。
	slog.SetDefault(logger.Logger)
	// 老编排文件里可能还留着已废弃的变量——点名它们，
	// 不然"变量还在、却没人读"是最难查的一种静默失效。
	config.WarnDeprecatedEnv(logger.Logger)

	if err := cfg.EnsureDirs(); err != nil {
		logger.Logger.Error("创建运行目录失败", "err", err)
		os.Exit(1)
	}
	db, err := store.Open(cfg.Database.Driver, dsnFor(cfg), 20, 10)
	if err != nil {
		logger.Logger.Error("打开数据库失败", "err", err)
		os.Exit(1)
	}
	if err := db.Migrate(context.Background()); err != nil {
		logger.Logger.Error("执行迁移失败", "err", err)
		os.Exit(1)
	}
	return &runtime{cfg: cfg, logger: logger, db: db}
}

// Close 收尾：先关数据库，再关日志文件。
func (r *runtime) Close() {
	r.db.Close()
	if err := r.logger.Closer.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "关闭日志文件失败: %v\n", err)
	}
}

// loadConfig 加载配置。
//
// ⚠️ 只读 **/data/config/application.yaml**（setup 的产物 + 设置页的回写目标）。
// 镜像里没有 /etc 层的配置文件——默认值在代码里（config.Default()），
// 需要覆盖就挂 /data 或用环境变量。
//
// 路径跟着 CR_DATA_DIR 走：与 cfg.ApplicationYAMLPath() 保持一致，
// 否则设置页把配置回写到 A 路径、启动却读 B 路径，两边分裂。
func loadConfig() (*config.Config, error) {
	dataDir := strings.TrimSpace(os.Getenv("CR_DATA_DIR"))
	if dataDir == "" {
		dataDir = "/data"
	}
	cfg, err := config.Load(
		filepath.Join(dataDir, "config", "application.yaml"),
	)
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

// buildCache 按配置构造缓存后端。
//
// 启动与运行期热切换走同一个入口——两处逻辑必须一致，
// 否则"页面上改的"和"启动时用的"会不是同一个后端。
func buildCache(cfg *config.Config) (cache.Cache, error) {
	return cache.New(cache.Options{
		Driver:     cfg.Cache.Driver,
		MaxEntries: cfg.Cache.Local.MaxEntries,
		Redis: cache.RedisOptions{
			Addr:        cfg.Cache.Redis.Addr,
			Password:    cfg.Cache.Redis.Password,
			DB:          cfg.Cache.Redis.DB,
			Prefix:      cfg.Cache.Redis.Prefix,
			TLS:         cfg.Cache.Redis.TLS,
			DialTimeout: cfg.Cache.Redis.DialTimeout,
			ReadTimeout: cfg.Cache.Redis.ReadTimeout,
		},
	})
}

// migrateCacheKeys 在缓存后端热切换时搬迁"不可丢"的键。
//
// 搬五样：
//   - 签名私钥（redis 模式：cr:signing-key；local 模式：jwt-private.pem）
//   - JWKS（cr:jwks）
//   - 每个用户的签名密钥（cr:user-secret:<username>）
//   - 每枚项目令牌的签名密钥（cr:project-secret:<token_id>）
//   - 口令加密私钥（redis 模式：cr:password-key；local 模式：password-key.pem）
//
// 不搬的后果：
//   - 签名私钥没了 → 下次启动重新生成 → **全体用户被登出**；
//   - 用户密钥没了 → 该用户的存量令牌全部作废（要重新签发）；
//   - 项目令牌密钥没了 → 该令牌换发的存量 JWT 全部作废；
//   - 口令私钥没了 → 下次启动重新生成 → kid 变了 →
//     **正在用旧 kid 的客户端全部登录失败**（要重新拉公钥才能自愈）。
//
// 方向由**新后端**的类型决定（不是配置）：
// 新后端是 redis → 缓存是权威源；新后端是 local → 文件是权威源。
func migrateCacheKeys(ctx context.Context, old, fresh cache.Cache,
	keys *crypto.Keystore, userSecrets *crypto.UserSecretStore,
	projectSecrets *crypto.ProjectSecretStore,
	passwordKeys *crypto.PasswordKeyStore,
	db *store.DB, cfg *config.Config, logger *slog.Logger) error {

	if fresh != nil && fresh.Driver() == "redis" {
		// 切到 redis：缓存是权威源，私钥得在缓存里。
		if err := keys.SeedCache(ctx, fresh); err != nil {
			return fmt.Errorf("迁移签名密钥到新缓存: %w", err)
		}
		// 口令私钥同构：redis 模式下缓存是权威源。
		if passwordKeys != nil {
			if err := passwordKeys.SeedCache(ctx, fresh); err != nil {
				return fmt.Errorf("迁移口令加密密钥到新缓存: %w", err)
			}
		}
	} else {
		// 切回 local：文件是权威源，得有一份。
		if err := keys.PersistPrivate(cfg.Keystore.PrivatePath); err != nil {
			return fmt.Errorf("迁移签名密钥到文件: %w", err)
		}
		if passwordKeys != nil {
			if err := passwordKeys.PersistPrivate(cfg.Keystore.PasswordKeyPath); err != nil {
				return fmt.Errorf("迁移口令加密密钥到文件: %w", err)
			}
		}
	}

	// 用户密钥按用户名逐个搬。用户名来自账户表——
	// 用户密钥只在用户存在时才有意义，删号后残留的没有搬迁价值。
	users, err := db.ListUsers(ctx)
	if err != nil {
		return fmt.Errorf("读取用户列表以迁移密钥: %w", err)
	}
	names := make([]string, 0, len(users))
	for _, u := range users {
		names = append(names, u.Username)
	}
	moved, err := userSecrets.Migrate(ctx, old, fresh, names)
	if err != nil {
		return fmt.Errorf("迁移用户密钥: %w", err)
	}
	// 搬了多少个必须留痕：密钥没搬全 = 那些用户的存量令牌作废，
	// 这是"事后才发现"的那种问题（用户来投诉"我的令牌突然失效了"）。
	logger.Info("用户签名密钥已迁移到新缓存后端", "count", moved)

	// 项目令牌密钥同理：按令牌 ID 逐个搬。
	tokenIDs, err := db.ProjectTokenIDs(ctx)
	if err != nil {
		return fmt.Errorf("读取项目令牌列表以迁移密钥: %w", err)
	}
	if projectSecrets != nil {
		movedTokens, err := projectSecrets.Migrate(ctx, old, fresh, tokenIDs)
		if err != nil {
			return fmt.Errorf("迁移项目令牌密钥: %w", err)
		}
		logger.Info("项目令牌签名密钥已迁移到新缓存后端", "count", movedTokens)
	}
	return nil
}
func dsnFor(cfg *config.Config) string {
	if cfg.Database.Driver == "sqlite" {
		return cfg.SQLiteDSN()
	}
	return cfg.Database.DSN
}

// renderDistributionConfig 渲染 distribution 的运行期配置。
//
// 只在 **embedded 模式**下有意义：这份文件是内嵌 distribution 的
// **唯一**配置来源（JWKS 来源、http.secret、事件回调、存储后端）。
// external 模式下 distribution 有自己的配置，本进程不渲染。
func renderDistributionConfig(cfg *config.Config, keys *crypto.Keystore,
	webhookSecret, registrySecret string, logger *slog.Logger) error {

	callbackBase := cfg.CallbackBaseURL()
	if callbackBase == "" {
		return fmt.Errorf("无法确定 distribution 事件回调地址（server.listen_addr=%q 不是 host:port 形式）",
			cfg.Server.ListenAddr)
	}
	b, err := distribution.Render(distribution.RenderInputs{
		Cfg:             *cfg,
		JWKSPath:        cfg.Keystore.JWKSPath,
		RegistrySecret:  registrySecret,
		WebhookSecret:   webhookSecret,
		CallbackBaseURL: callbackBase,
		ListenAddr:      cfg.Registry.DistributionAddr,
		DebugAddr:       cfg.Registry.DistributionDebugAddr,
		LogLevel:        "info",
	})
	if err != nil {
		return err
	}
	path := distributionConfigPath(cfg)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// 原子写：tmp + rename（**不先删旧文件**——见 internal/fsatomic）。
	if err := fsatomic.Write(path, b, 0o600); err != nil {
		return err
	}
	logger.Info("已渲染 distribution 配置", "path", path,
		"storage_driver", cfg.Storage.Driver,
		"listen_addr", cfg.Registry.DistributionAddr)
	return nil
}
