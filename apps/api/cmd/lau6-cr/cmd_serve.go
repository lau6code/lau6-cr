package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/cache"
	"github.com/lau6code/lau6-cr/apps/api/internal/config"
	"github.com/lau6code/lau6-cr/apps/api/internal/gc"
	"github.com/lau6code/lau6-cr/apps/api/internal/ldap"
	"github.com/lau6code/lau6-cr/apps/api/internal/server"
	"github.com/lau6code/lau6-cr/apps/api/internal/service"
)

// runServe 启动 HTTP 服务。
func runServe() {
	rt := openRuntime(func(cfg *config.Config) {
		// ⚠️ distribution 的部署形态必须**显式合法**（CR_REGISTRY_MODE）：
		//    embedded = 同容器，lau6-cr 自己拉起内嵌的 distribution；
		//    external = 连独立部署的 distribution（地址用 CR_UPSTREAM_REGISTRY 给）。
		//    这里致命退出，不给"连不上"的假象。
		if err := cfg.ValidateRegistry(); err != nil {
			fmt.Fprintf(os.Stderr, "distribution 配置无效: %v\n", err)
			os.Exit(1)
		}
	})
	defer rt.Close()
	cfg := rt.cfg
	logger := rt.logger
	db := rt.db

	// 缓存
	//
	// ⚠️ 必须在**密钥库之前**建：Redis 模式下**缓存是签名密钥的权威源**
	//（docs/design/12-cache.md §3.3），密钥库启动时要从缓存读。
	c, err := buildCache(cfg)
	if err != nil {
		logger.Logger.Error("初始化缓存失败", "err", err)
		os.Exit(1)
	}
	cacheHolder := cache.NewHolder(c)
	defer c.Close()

	sec := setupSecrets(cfg, c, logger.Logger)
	keys, passwordKeys := sec.keys, sec.passwordKeys
	webhookSecret, registrySecret := sec.webhookSecret, sec.registrySecret
	// distribution 部署模式
	// embedded：distribution 与本进程同容器，由本进程把它拉起来
	//          （两者共享同一个数据目录，blob 回收才能直接扫盘）。
	// external：distribution 独立部署，本进程只通过 upstream_addr 调用它。
	//
	// ⚠️ 必须在构造设置管理器**之前**算好：external 形态下 storage.*
	//    不进设置页（那份配置由上游容器自己管，lau6-cr 渲染不了
	//    也重启不了它——摆一个改了没人读的入口就是假开关）。
	embeddedDistribution := cfg.Registry.Mode == config.ModeEmbedded

	// 设置管理器
	settings, err := service.NewSettingsManager(db, cfg, cfg.ApplicationYAMLPath(),
		service.WithLogger(logger.Logger),
		service.WithDistributionEmbedded(embeddedDistribution))
	if err != nil {
		logger.Logger.Error("初始化设置管理器失败", "err", err)
		os.Exit(1)
	}
	live := settings.Snapshot()

	tok := setupTokenIssuer(cfg, live, cacheHolder, settings, keys, logger.Logger)
	userSecrets, projectSecrets, issuer := tok.userSecrets, tok.projectSecrets, tok.issuer

	// LDAP
	//
	// ⚠️ ldap.* 是 T0 热生效设置（14-settings.md §3.4：
	//    「切换后立即影响登录」），所以这里**不能**构造一次客户端就用到底——
	//    Dynamic 按当前设置快照取客户端：配置没变就复用，变了就重建。
	ldapDyn := ldap.NewDynamic(func() ldap.Config {
		cfg := settings.Snapshot()
		return ldap.Config{
			URL:         cfg.LDAP.URL,
			DC:          cfg.LDAP.DC,
			UserSegment: cfg.LDAP.UserSegment,
			StartTLS:    cfg.LDAP.StartTLS,
			Timeout:     time.Duration(cfg.LDAP.TimeoutSec) * time.Second,
		}
	})

	// 业务组件
	//
	// 权限引擎要读"匿名读总开关"（access.anonymous_read_enabled）——
	// 那是公开面出问题时的一键止血阀，必须接上设置快照，
	// 否则它在设置页上改了也不起作用。
	perm := service.NewPermissionEngine(db, service.PermissionOptions{
		AnonymousReadEnabled: func() bool {
			return settings.Snapshot().Access.AnonymousReadEnabled
		},
		Logger: logger.Logger,
	})

	// 上游 distribution 的地址。
	//
	// embedded：由 registry.distribution_addr 推导（回环）；
	// external：用 registry.upstream_addr（部署者显式给的）。
	//
	// ⚠️ 两种形态都**不允许猜地址**：解析失败就是致命错误。
	upstreamAddr, err := live.EffectiveUpstream()
	if err != nil {
		logger.Logger.Error("解析上游 distribution 地址失败", "err", err)
		os.Exit(1)
	}

	sup := startEmbeddedDistribution(live, embeddedDistribution, upstreamAddr, sec, logger.Logger)

	// 限流器与登录退避
	//
	// 两者都把状态存在缓存后端里：redis 模式下多副本共享
	// （local 模式各算各的，限额会被放大 N 倍——12-cache.md §7）。
	rateLimiter := service.NewRateLimiter(
		func() cache.Limiter { return cache.LimiterOf(cacheHolder.Get()) },
		func(sc service.RateLimitScope) int {
			cfg := settings.Snapshot()
			switch sc {
			case service.RateLimitScopeToken:
				return cfg.Access.AnonymousTokenRPM
			case service.RateLimitScopeRead:
				return cfg.Access.AnonymousReadRPM
			default:
				return 0
			}
		},
		logger.Logger,
	)
	loginGuard := service.NewLoginGuard(
		func() cache.Cache { return cacheHolder.Get() },
		func() int { return settings.Snapshot().Access.LoginFailLimit },
		func() time.Duration {
			return time.Duration(settings.Snapshot().Access.LoginLockSeconds) * time.Second
		},
		logger.Logger,
	)

	authSvc := service.NewAuthService(db, service.AuthOptions{
		// LDAP 未启用时返回 nil：认证服务据此拒绝 LDAP 账号登录
		//（14-settings.md §3.4：关闭后已有 LDAP 账号无法登录）。
		LDAP: func() ldap.Authenticator {
			if !settings.Snapshot().LDAP.Enabled {
				return nil
			}
			return ldapDyn
		},
		LDAPEnabled: func() bool { return settings.Snapshot().LDAP.Enabled },
		AllowCreate: func() bool { return settings.Snapshot().LDAP.AllowCreate },
		Guard:       loginGuard,
		Logger:      logger.Logger,
	})
	// 个人访问令牌（PAT）：用户自己生成的长期凭据，
	// 用来在 CI / 脚本里推拉需要权限的镜像（02-auth.md §2.6.1）。
	patSvc := service.NewPersonalTokenService(db, service.PersonalTokenOptions{
		TTL: func() time.Duration {
			return time.Duration(settings.Snapshot().Core.PatTTLSeconds) * time.Second
		},
		Logger: logger.Logger,
	})
	// 项目令牌：项目管理员生成的**项目身份**长期凭据（02-auth.md §2.6.2）。
	projectTokenSvc := service.NewProjectTokenService(db, service.ProjectTokenOptions{
		TTL: func() time.Duration {
			return time.Duration(settings.Snapshot().Core.ProjectTokenTTLSeconds) * time.Second
		},
		Secrets: projectSecrets,
		Logger:  logger.Logger,
	})
	tokenSvc := service.NewTokenService(perm, issuer, authSvc, patSvc, projectTokenSvc)

	stats := service.NewStatsAggregator(service.StatsOptions{
		DB: db, Enabled: live.Stats.Enabled,
		Interval: live.Stats.FlushInterval, MaxBucket: live.Stats.MaxBuckets,
		// 匿名 IP 记录：初始值取配置基线，之后每轮刷盘按设置快照刷新
		// （RecordIPFn）——改了设置最多一个刷盘周期生效，不必重启。
		RecordIP:   live.Stats.RecordIP,
		RecordIPFn: func() bool { return settings.Snapshot().Stats.RecordIP },
		// 刷盘失败必须留下痕迹（见 StatsAggregator.flushAndLog）。
		Logger: logger.Logger,
	})
	defer stats.Close()

	upstream := &http.Client{
		// 不设整体 Timeout——大文件传输会超时。用 context 控制。
		Transport: &http.Transport{
			MaxIdleConns:          256,
			MaxIdleConnsPerHost:   64,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			DisableCompression:    true,
		},
	}

	// 刷新队列要在 registry 服务之前建好：
	// registry 服务检测到 manifest 写入时要用它排队刷新目录。
	refreshQ := service.NewRefreshQueue()
	defer refreshQ.Close()

	registrySvc := service.NewRegistryService(service.RegistryDeps{
		Upstream:         upstream,
		UpstreamURL:      upstreamAddr,
		Perm:             perm,
		Issuer:           issuer,
		DB:               db,
		Stats:            stats,
		PATs:             patSvc,
		ProjectTokens:    projectTokenSvc,
		MaxManifestBytes: int64(live.Limits.MaxManifestBytes),
		PolicyFn:         settings.Policy,
		// realm 由 core.public_url 派生。
		//
		// ⚠️ 必须读**当前**生效值（设置快照），不能用启动时的快照：
		//    初始化向导与设置页都会改 public_url，而 realm 是 docker CLI
		//    唯一认的地址——拿旧值就等于把客户端指到错误的 host 上。
		RealmFn:      func() string { return settings.Snapshot().Core.PublicURL + "/token" },
		RefreshQueue: refreshQ,
		Logger:       logger.Logger,
	})

	setupSvc := service.NewSetupService(service.SetupDeps{
		DB:       db,
		Settings: settings,
		YAMLPath: cfg.ApplicationYAMLPath(),
	})
	trafficSvc := service.NewTrafficService(db)

	refresher := service.NewRefresher(db, upstream, upstreamAddr, issuer, refreshQ, logger.Logger)

	// 仓库写屏障：blob 回收期间挂起 /v2/* 的写操作。
	//
	// 它同时被 GC 执行器与 HTTP 写路径使用——
	// 屏障必须与"判断某个请求是不是写请求"的地方共享同一个实例。
	writeBarrier := gc.NewBarrier()

	// 进程生命周期上下文：后台协程（目录对账、定时清理、blob 回收）都挂在它下面。
	//
	// ⚠️ 回收子进程也靠它收场：进程退出时 ctx 被取消 →
	//    `distribution garbage-collect` 子进程随之被杀，
	//    不留一个孤儿进程在 lau6-cr 死后继续删 blob。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// blob 回收执行器（第 ② 层）：把"已解引用但仍占盘"的 blob 清掉。
	//
	// ⚠️ 回收动作由 **distribution 自己**执行：
	//    lau6-cr 调度 `distribution garbage-collect <config>`，
	//    用的是 registry 自己在用的那份配置——
	//    所以对象存储下也能清（11-storage.md §7）。
	gcExecutor := gc.NewExecutor(live.Registry.DistributionBin, distributionConfigPath(live), logger.Logger)
	gcSvc := service.NewGCService(service.GCDeps{
		DB:         db,
		Logger:     logger.Logger,
		SettingsFn: settings.Snapshot,
		Barrier:    writeBarrier,
		Executor:   gcExecutor,
		Ctx:        ctx,
	})

	// 清理执行器：执行保留策略（决定删哪些 tag），
	// 删完再由 blob 回收把磁盘空间收回来。
	cleanupSvc := service.NewCleanupService(service.CleanupDeps{
		DB:           db,
		Refresher:    refresher,
		Logger:       logger.Logger,
		SettingsFn:   settings.Snapshot,
		RefreshQueue: refreshQ,
		GC:           gcSvc,
	})
	// push 成功后触发该项目的保留策略（带冷却）。
	registrySvc.SetCleanupTrigger(func(ctx context.Context, projectID int64, projectName string) {
		cleanupSvc.TriggerPush(ctx, projectID, projectName)
	})

	app := &server.App{
		Cfg:             cfg,
		DB:              db,
		Cache:           cacheHolder,
		Keys:            keys,
		Tokens:          issuer,
		UserSecrets:     userSecrets,
		PasswordKeys:    passwordKeys,
		Perm:            perm,
		Upstream:        upstream,
		Stats:           stats,
		Logger:          logger.Logger,
		RateLimiter:     rateLimiter,
		AuthSvc:         authSvc,
		TokenSvc:        tokenSvc,
		Pats:            patSvc,
		ProjectTokenSvc: projectTokenSvc,
		RegistrySvc:     registrySvc,
		SetupSvc:        setupSvc,
		SettingsSvc:     settings,
		TrafficSvc:      trafficSvc,
		RefreshQ:        refreshQ,
		Refresher:       refresher,
		CleanupSvc:      cleanupSvc,
		GCSvc:           gcSvc,
		WriteBarrier:    writeBarrier,
		UpstreamAddr:    upstreamAddr,
		WebhookSecret:   webhookSecret,
	}
	// 启动自检：口令传输模式启用时，App 必须带着口令加密密钥库。
	//
	// ⚠️ 缺了它，公钥接口会**每个请求**都 500（加密登录链路整个是坏的），
	//    而服务照常启动、照常接受登录——那种坏法排查起来最费时间。
	if err := app.CheckPasswordTransport(); err != nil {
		logger.Logger.Error("启动自检失败", "err", err)
		os.Exit(1)
	}
	app.SetTrustedProxies(live.Core.TrustedProxies)
	app.ReadyFunc = func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstreamAddr+"/v2/", nil)
		if err != nil {
			return err
		}
		resp, err := upstream.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.StatusCode >= 500 {
			return fmt.Errorf("上游返回 %d", resp.StatusCode)
		}
		return nil
	}
	app.ReloadConfig = func() error {
		// external 模式下 distribution 用**它自己的**配置，本进程不渲染任何东西。
		if !embeddedDistribution {
			return nil
		}
		// 重新渲染 distribution 配置（存储后端等可能变了），
		// 然后重启子进程。
		//
		// ⚠️ 必须取**当前**快照，不能用启动时捕获的 live——
		//    那份是启动时的配置，设置改过之后它就过期了，
		//    拿它渲染等于"改了设置但 distribution 还是老参数"。
		//
		// ⚠️ distribution 不支持热加载配置（3.x 没有 SIGHUP 重载），
		//    所以只能重启子进程。重启失败要报错给调用方——
		//    "设置保存成功但 distribution 没起来"是最难查的一种。
		if err := renderDistributionConfig(settings.Snapshot(), keys, webhookSecret, registrySecret, logger.Logger); err != nil {
			return err
		}
		return sup.Restart(context.Background())
	}
	// 缓存后端热切换：设置里改了 driver / Redis 连接参数就重建后端。
	//
	// 新后端立即接管；旧的后端延迟关闭——正在跑的请求可能还持有它，
	// 立刻 Close 会让它们拿到 "use of closed connection"。
	//
	// ⚠️ **搬键必须在这个钩子里做**，不能只在初始化向导里做：
	//    初始化向导与设置页走的是同一个钩子，两处逻辑必须一致。
	//    只搬一半的后果是"设置页切了后端 → 重启后密钥没了 → 全体登出"。
	app.ReloadCache = func(newCfg *config.Config) error {
		fresh, err := buildCache(newCfg)
		if err != nil {
			return err
		}
		old := cacheHolder.Get()
		// 先搬键、搬成功再换指针：搬失败时旧后端还在，数据没丢，
		// 可以在旧后端上继续跑（配置已改，重启后才会切到新后端）。
		if err := migrateCacheKeys(context.Background(), old, fresh, keys, userSecrets, projectSecrets, passwordKeys, db, cfg, logger.Logger); err != nil {
			return err
		}
		old = cacheHolder.Swap(fresh)
		logger.Logger.Info("缓存后端已热切换", "driver", newCfg.Cache.Driver)
		if old != nil {
			go func() {
				time.Sleep(30 * time.Second)
				// 关旧后端失败不拦切换：数据已经搬到新后端了。
				// 但必须留痕——否则旧连接的泄漏在日志里查不出来。
				if err := old.Close(); err != nil {
					logger.Logger.Warn("关闭旧缓存后端失败", "err", err)
				}
			}()
		}
		return nil
	}

	// 初始化向导里改了缓存后端 → 同样热切换，不用重启容器。
	//
	// 与设置页走同一个钩子：两处逻辑必须一致，
	// 否则"向导里选的"和"设置里改的"会不是同一个后端。
	setupSvc.ReloadCache = app.ReloadCache

	// 日志级别热生效：设置页把级别改成 debug 后立刻见效，不用重启容器。
	//
	// 只处理级别——文件写入器用 slog.LevelVar 之外的结构，
	// 改目录/滚动方式/保留天数要重建写入器，走重启（对应设置项 T2）。
	app.ReloadLogging = func(newCfg *config.Config) error {
		if err := logger.SetLevel(newCfg.Logging.Level); err != nil {
			return err
		}
		logger.Logger.Info("日志级别已热切换", "level", newCfg.Logging.Level)
		return nil
	}

	// 后台协程
	go refresher.Run(ctx)

	// 目录对账（兜底）：上游 distribution 没配事件通知时，
	// push 能成功但目录页永远是空的——这个协程把目录拉回来。
	// 见 `03-registry.md` §4.3 路径三。
	reconciler := service.NewReconciler(service.ReconcilerDeps{
		DB:          db,
		Upstream:    upstream,
		UpstreamURL: upstreamAddr,
		Issuer:      issuer,
		Queue:       refreshQ,
		Logger:      logger.Logger,
		Interval:    time.Duration(live.Reconciler.IntervalMinutes) * time.Minute,
		StaleAfter:  time.Duration(live.Reconciler.StaleAfterHours) * time.Hour,
	})
	go reconciler.Run(ctx)

	// 定时清理（保留策略）。
	//
	// 周期是热改设置项，所以调度器每分钟醒一次看是否到期，
	// 而不是把周期写死进 ticker 的间隔。
	// 见 `03-registry.md` §5.4。
	go cleanupSvc.RunScheduler(ctx)

	// 定时磁盘回收：每天到点跑一次（时间点 cleanup.gc.schedule_at 可热改）。
	//
	// 每 10 秒醒一次看是否到点——**不能是 1 分钟**：
	// 整分钟的 ticker 只会在"启动时刻的分偏移"上醒，
	// 配置成"02:00"的定时任务在 18:22:30 启动的进程里永远不会触发。
	// 见 `03-registry.md` §5.5.4。
	go gcSvc.RunScheduler(ctx)

	// 流量统计的汇总与清理：
	//
	//   - 每小时把**已关闭**的小时桶累加进天桶（写天桶 + 推水位游标同一事务）
	//   - 每日按 stats.prune_hour（UTC）清理过期的小时桶/天桶/IP 记录
	//
	// 见 `10-traffic-stats.md` §3.3、§3.4。
	//
	// ⚠️ 保留期与清理时刻读的是**设置快照**（T0 热生效）：
	// 管理员改了 stats.hourly_retention，不用重启就生效。
	rollup := service.NewStatsRollup(db, service.StatsRollupOptions{
		HourlyRetention: func() time.Duration { return settings.Snapshot().Stats.HourlyRetention },
		DailyRetention:  func() time.Duration { return settings.Snapshot().Stats.DailyRetention },
		PruneHour:       func() int { return settings.Snapshot().Stats.PruneHour },
		Logger:          logger.Logger,
	})
	go rollup.Run(ctx)

	// HTTP 服务
	srv := &http.Server{
		Addr:              live.Server.ListenAddr,
		Handler:           app.Routes(),
		ReadHeaderTimeout: 30 * time.Second,
		// ⚠️ ReadTimeout / WriteTimeout 必须为 0。
		// 非 0 会在传输中途掐断大文件，docker 侧表现为 unexpected EOF。
		ReadTimeout:  0,
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		logger.Logger.Info("HTTP 服务启动", "addr", live.Server.ListenAddr,
			"public_url", live.Core.PublicURL,
			// token realm 单独打一条：docker 客户端只认 WWW-Authenticate 里的 realm，
			// 它指向哪儿决定了 push/pull 能不能走通。指向一个**客户端够不着**的地址
			// （比如前端 dev server 端口）时，症状是"docker 报 unauthorized，
			// 后端一片安静"——启动日志里能直接看出 realm 写错没有。
			"token_realm", live.Core.PublicURL+"/token",
			"version", version)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Logger.Error("HTTP 服务异常退出", "err", err)
			cancel()
		}
	}()

	// 优雅退出
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)

	// 内嵌 distribution 意外退出的通知通道。
	// external 模式没有子进程 → nil 通道（永远不触发）。
	var supDone <-chan struct{}
	if sup != nil {
		supDone = sup.Done()
	}

	// exitCode 非 0 = 异常退出（容器编排会按 restart 策略拉起来）。
	exitCode := 0
	select {
	case <-sig:
		logger.Logger.Info("收到退出信号，开始优雅关闭")
	case <-supDone:
		// ⚠️ distribution 挂了 → 整个实例退出。
		//    继续对外服务 = 页面一切正常、push/pull 全失败，
		//    用户只能从"拉不动镜像"倒推回来。宁可容器重启。
		logger.Logger.Error("内嵌 distribution 进程退出，lau6-cr 一并退出",
			"err", sup.ExitError())
		exitCode = 1
	case <-ctx.Done():
		// HTTP 服务异常退出等内部故障：不能继续挂着。
		logger.Logger.Error("服务上下文被取消（内部组件异常），退出")
		exitCode = 1
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Logger.Error("关闭 HTTP 服务失败", "err", err)
	}
	cancel()
	// 退出前把统计刷盘。刷盘失败不能静默——这一轮的流量就丢在这里了。
	if err := stats.Flush(context.Background()); err != nil {
		logger.Logger.Error("退出前刷盘统计失败", "err", err)
	}
	// 停掉内嵌 distribution：它是本进程的子进程，
	// 不收掉就会变成孤儿进程占着端口。
	if sup != nil {
		if err := sup.Stop(context.Background()); err != nil {
			logger.Logger.Error("停止内嵌 distribution 失败", "err", err)
		}
	}
	logger.Logger.Info("已退出")
	if exitCode != 0 {
		os.Exit(exitCode)
	}
}
