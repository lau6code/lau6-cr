package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/cache"
	"github.com/lau6code/lau6-cr/apps/api/internal/config"
	"github.com/lau6code/lau6-cr/apps/api/internal/crypto"
	"github.com/lau6code/lau6-cr/apps/api/internal/distribution"
	"github.com/lau6code/lau6-cr/apps/api/internal/service"
)

// serveSecrets 是 serve 路径上的密钥材料。
type serveSecrets struct {
	keys           *crypto.Keystore
	passwordKeys   *crypto.PasswordKeyStore
	webhookSecret  string
	registrySecret string
}

// setupSecrets 装好签名密钥库、口令加密密钥库与两个共享密钥。
//
// ⚠️ 两类密钥库都**fail-closed**：缓存连不上就启动失败，
// 不降级成"生成新密钥"——那会让 Redis 每次重启都换一把 kid，
// 正在用旧 kid 的客户端会突然全部登录失败。
func setupSecrets(cfg *config.Config, c cache.Cache, logger *slog.Logger) serveSecrets {
	// 签名密钥库
	//
	// redis 模式下：私钥从缓存读（读不到就生成并写回缓存），
	// 缓存连不上则**启动失败**（fail-closed）。
	keys, err := crypto.NewKeystore(crypto.Options{
		PrivatePath: cfg.Keystore.PrivatePath,
		JWKSPath:    cfg.Keystore.JWKSPath,
		Cache:       c,
	})
	if err != nil {
		logger.Error("初始化密钥库失败", "err", err)
		os.Exit(1)
	}

	// 口令加密密钥库（X25519）
	//
	// 与签名密钥**完全独立**：签名密钥被读走 = 可伪造任意用户的 token，
	// 口令私钥被读走 = 只能解开抓到的密文（13-password-transport.md §4.2 / ADR 4）。
	//
	// 存放策略与签名密钥同构：redis 模式 → 缓存权威（连不上就**启动失败**，
	// fail-closed——降级成"生成新密钥"会让每次缓存抖动都换一把 kid，
	// 正在用旧 kid 的客户端会突然全部登录失败）；local 模式 → 文件权威。
	passwordKeys, err := crypto.NewPasswordKeyStore(crypto.PasswordKeyOptions{
		PrivatePath: cfg.Keystore.PasswordKeyPath,
		Cache:       c,
	})
	if err != nil {
		logger.Error("初始化口令加密密钥库失败", "err", err)
		os.Exit(1)
	}

	webhookSecret, err := crypto.LoadOrGenerateSecret(cfg.Keystore.WebhookSecretPath, 32)
	if err != nil {
		logger.Error("生成 webhook 密钥失败", "err", err)
		os.Exit(1)
	}
	registrySecret, err := crypto.LoadOrGenerateSecret(cfg.Keystore.RegistrySecretPath, 32)
	if err != nil {
		logger.Error("生成 registry 密钥失败", "err", err)
		os.Exit(1)
	}
	return serveSecrets{
		keys:           keys,
		passwordKeys:   passwordKeys,
		webhookSecret:  webhookSecret,
		registrySecret: registrySecret,
	}
}

// serveTokens 是 serve 路径上的签名密钥与令牌签发器。
type serveTokens struct {
	userSecrets    *crypto.UserSecretStore
	projectSecrets *crypto.ProjectSecretStore
	issuer         *crypto.TokenIssuer
}

// setupTokenIssuer 装好用户密钥库、项目令牌密钥库与令牌签发器。
func setupTokenIssuer(cfg *config.Config, live *config.Config, cacheHolder *cache.Holder,
	settings *service.SettingsManager, keys *crypto.Keystore, logger *slog.Logger) serveTokens {

	// 用户密钥库：每用户一把 JWT 签名密钥（30 天有效）。
	//
	// 存放策略与实例签名密钥同构（12-cache.md §3.3）：
	// redis 模式 → 缓存权威；local 模式 → 文件权威（/data/config/user-secrets/）。
	userSecrets := crypto.NewUserSecretStore(crypto.UserSecretOptions{
		Cache: cacheHolder,
		Dir:   filepath.Join(filepath.Dir(cfg.Keystore.PrivatePath), "user-secrets"),
		TTL: func() time.Duration {
			return time.Duration(settings.Snapshot().Core.UserTokenTTLSeconds) * time.Second
		},
	})
	// 项目令牌密钥库：每枚项目令牌一把 JWT 签名密钥。
	//
	// 存放策略与用户密钥同构（12-cache.md §3.6），但目录分开——
	// 两类密钥混在一个目录里，排查时说不清哪个文件是谁的。
	projectSecrets := crypto.NewProjectSecretStore(crypto.ProjectSecretOptions{
		Cache: cacheHolder,
		Dir:   filepath.Join(filepath.Dir(cfg.Keystore.PrivatePath), "project-secrets"),
		TTL: func() time.Duration {
			return time.Duration(settings.Snapshot().Core.ProjectTokenTTLSeconds) * time.Second
		},
	})
	// 令牌 TTL 是 T0 热生效设置（core.token_ttl_seconds）：
	// 签发器必须读当前快照，不能把启动时的窗口冻住。
	issuer, err := crypto.NewTokenIssuer(keys, userSecrets, projectSecrets, live.Token.Issuer, live.Token.Audience,
		func() time.Duration {
			return time.Duration(settings.Snapshot().Core.TokenTTLSeconds) * time.Second
		})
	if err != nil {
		logger.Error("初始化令牌签发器失败", "err", err)
		os.Exit(1)
	}
	return serveTokens{
		userSecrets:    userSecrets,
		projectSecrets: projectSecrets,
		issuer:         issuer,
	}
}

// startEmbeddedDistribution 在 embedded 形态下拉起内嵌的 distribution 子进程。
//
// embedded 模式：lau6-cr 自己把 distribution 拉起来（子进程），
// 等它**真的能服务**之后才开始对外提供 HTTP 接口。
//
// ⚠️ 起不来就得起不来：distribution 挂了还照常 ready，
// 现象是"页面正常、push/pull 全失败"——最难排查的那种。
//
// external 形态返回 nil：本进程不拉起它，只记一条日志说明去哪配。
func startEmbeddedDistribution(live *config.Config, embedded bool, upstreamAddr string,
	sec serveSecrets, logger *slog.Logger) *distribution.Supervisor {

	if !embedded {
		logger.Info("使用独立部署的 distribution，本进程不拉起它",
			"upstream", upstreamAddr,
			"hint", "请在远程 distribution 上配置 JWKS 来源与事件回调；"+
				"接入清单见 lau6-cr admin integration-info")
		return nil
	}
	sup := distribution.NewSupervisor(distribution.SupervisorOptions{
		Bin:        live.Registry.DistributionBin,
		ConfigPath: distributionConfigPath(live),
		ListenAddr: live.Registry.DistributionAddr,
		Logger:     logger,
	})
	if err := renderDistributionConfig(live, sec.keys, sec.webhookSecret, sec.registrySecret, logger); err != nil {
		logger.Error("渲染 distribution 配置失败", "err", err)
		os.Exit(1)
	}
	if err := sup.Start(context.Background()); err != nil {
		logger.Error("启动内嵌 distribution 失败", "err", err)
		os.Exit(1)
	}
	return sup
}
