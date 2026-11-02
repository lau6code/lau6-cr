package config

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

func (c *Config) validate() error {
	switch c.Database.Driver {
	case "sqlite":
		if c.Database.SQLitePath == "" {
			return fmt.Errorf("database.sqlite_path 不能为空")
		}
	case "postgres", "mysql":
		if c.Database.DSN == "" {
			return fmt.Errorf("database.dsn 在 driver=%s 时必填", c.Database.Driver)
		}
	default:
		return fmt.Errorf("未知的 database.driver: %q", c.Database.Driver)
	}
	switch c.Storage.Driver {
	case "local":
		if c.Storage.Local.Root == "" {
			return fmt.Errorf("storage.local.root 不能为空")
		}
	case "s3":
		if c.Storage.S3.Endpoint == "" || c.Storage.S3.Bucket == "" {
			return fmt.Errorf("storage.s3.endpoint 与 storage.s3.bucket 在 driver=s3 时必填")
		}
	default:
		return fmt.Errorf("未知的 storage.driver: %q", c.Storage.Driver)
	}
	switch c.Cache.Driver {
	case "local":
	case "redis":
		if c.Cache.Redis.Addr == "" {
			return fmt.Errorf("cache.redis.addr 在 driver=redis 时必填")
		}
	default:
		return fmt.Errorf("未知的 cache.driver: %q", c.Cache.Driver)
	}
	if c.Core.TokenTTLSeconds < 60 {
		return fmt.Errorf("core.token_ttl_seconds 不得小于 60（distribution 会拒绝过短窗口）")
	}
	// public_url 是 **token realm 的来源**：
	// WWW-Authenticate 里的 realm 就是 <public_url>/token，
	// docker 客户端会拿着它去换 token。
	//
	// 它写错的后果非常隐蔽：客户端换不到 token → push/pull 报 unauthorized，
	// 而后端这边一切正常（因为请求根本没到鉴权那一步）。
	// 所以配置期就把它挡住，不留到运行期。
	if c.Core.PublicURL == "" {
		return fmt.Errorf("core.public_url 不能为空（它是 token realm 的来源）")
	}
	pub, err := url.Parse(c.Core.PublicURL)
	if err != nil || pub.Host == "" || (pub.Scheme != "http" && pub.Scheme != "https") {
		return fmt.Errorf("core.public_url 必须是绝对 URL（http/https）：%q", c.Core.PublicURL)
	}
	// ⚠️ 上游地址**不在这里强制**：
	//    setup / config 这类子命令不碰上游，硬性要求它会造成死锁——
	//    容器起不来 → /setup 页面也访问不到 → 永远没法初始化。
	//    serve 路径单独调 ValidateRegistry（见该方法的注释）。
	if c.Registry.UpstreamAddr != "" {
		if err := c.ValidateUpstream(); err != nil {
			return err
		}
	}
	switch c.Registry.Mode {
	case ModeEmbedded, ModeExternal:
	default:
		return fmt.Errorf("registry.mode 非法: %q（支持 %s|%s）",
			c.Registry.Mode, ModeEmbedded, ModeExternal)
	}
	if c.LDAP.Enabled {
		if c.LDAP.URL == "" {
			return fmt.Errorf("ldap.enabled=true 时 ldap.url 不能为空")
		}
		if !strings.Contains(c.LDAP.UserSegment, "{0}") {
			return fmt.Errorf("ldap.user_segment 必须包含 {0} 占位符")
		}
	}
	if err := c.validateLogging(); err != nil {
		return err
	}
	if err := c.validateCleanup(); err != nil {
		return err
	}
	return nil
}

// validateCleanup 校验清理策略的运行参数。
//
// 参数写错必须在启动时报错：清理周期为 0 会让定时任务变成忙循环，
// 而"忙循环地删镜像"是不能接受的。
func (c *Config) validateCleanup() error {
	if c.Cleanup.IntervalMinutes <= 0 {
		return fmt.Errorf("cleanup.interval_minutes 必须大于 0")
	}
	if c.Cleanup.PushTriggerCooldown < 0 {
		return fmt.Errorf("cleanup.push_trigger_cooldown 不能为负")
	}
	if c.Cleanup.GC.Enabled {
		// 回收由 distribution 执行，用的是**registry 自己在用的那份配置**。
		// external 形态下那份配置不在本进程手里——
		// 没指路径就根本无法执行，不如启动时就说清楚。
		if c.Registry.Mode == ModeExternal && strings.TrimSpace(c.Registry.DistributionConfigPath) == "" {
			return fmt.Errorf("cleanup.gc.enabled 为 true 时，独立部署 registry 的形态必须配置" +
				" registry.distribution_config_path（registry 自己的配置文件路径，" +
				"blob 回收靠它调度 distribution）")
		}
		if c.Cleanup.GC.DrainTimeout <= 0 {
			return fmt.Errorf("cleanup.gc.drain_timeout 必须大于 0")
		}
	}
	if c.Cleanup.GC.ScheduleEnabled {
		// 时间点写错 → 定时回收永远不触发，而且没有任何报错。
		// 这是"配了等于没配"的典型，必须在启动时拦下来。
		if _, _, err := ParseClock(c.Cleanup.GC.ScheduleAt); err != nil {
			return fmt.Errorf("cleanup.gc.schedule_at 非法: %w", err)
		}
	}
	return nil
}

// validateLogging 校验日志配置。
//
// 配置写错必须在启动时报错，不能拖到运行期：
// 日志初始化失败时进程已经起来了，运维看到的现象是"服务在跑但没日志"。
func (c *Config) validateLogging() error {
	switch strings.ToLower(strings.TrimSpace(c.Logging.Level)) {
	case "", "debug", "info", "warn", "warning", "error":
	default:
		return fmt.Errorf("logging.level 非法（%q）：支持 debug|info|warn|error", c.Logging.Level)
	}
	switch strings.ToLower(strings.TrimSpace(c.Logging.Output)) {
	case "", "console", "file", "both":
	default:
		return fmt.Errorf("logging.output 非法（%q）：支持 console|file|both", c.Logging.Output)
	}
	output := strings.ToLower(strings.TrimSpace(c.Logging.Output))
	if output == "" {
		output = "both"
	}
	if output == "console" {
		// 只写控制台，文件相关项不参与校验。
		return nil
	}
	if !c.Logging.File.Enabled {
		return fmt.Errorf("logging.output=%q 要求 logging.file.enabled=true", output)
	}
	if c.Logging.File.Dir == "" {
		return fmt.Errorf("logging.file.dir 不能为空")
	}
	if c.Logging.File.Filename == "" {
		return fmt.Errorf("logging.file.filename 不能为空")
	}
	if c.Logging.File.MaxSizeMB <= 0 {
		return fmt.Errorf("logging.file.max_size_mb 缺失或非法（需 >0）")
	}
	if c.Logging.File.RetainDays <= 0 {
		return fmt.Errorf("logging.file.retain_days 缺失或非法（需 >0）")
	}
	switch strings.ToLower(strings.TrimSpace(c.Logging.File.Rotate)) {
	case "", "daily", "size":
	default:
		return fmt.Errorf("logging.file.rotate 非法（%q）：支持 daily|size", c.Logging.File.Rotate)
	}
	return nil
}

// ValidateUpstream 校验上游 distribution 的地址（external 模式）。
//
// ⚠️ **只有 serve 路径需要它**：external 模式下上游是独立容器/独立主机的服务名，
//
//	没有可猜的默认值（见 RegistryConfig.UpstreamAddr 的说明）。
//	serve 没配就是**致命错误**，立刻退出；
//	setup / config 子命令不碰它（死锁理由见 validate 的注释）。
func (c *Config) ValidateUpstream() error {
	if c.Registry.UpstreamAddr == "" {
		return fmt.Errorf("registry.upstream_addr 不能为空（用 CR_UPSTREAM_REGISTRY 指定上游 distribution 的地址，如 http://registry:5000）")
	}
	u, err := url.Parse(c.Registry.UpstreamAddr)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("registry.upstream_addr 不是合法的 URL: %q", c.Registry.UpstreamAddr)
	}
	return nil
}

// ValidateRegistry 校验 distribution 的部署形态（serve 路径专用）。
//
// 与 ValidateUpstream 同一个理由：只有 serve 需要它（死锁理由见 validate 的注释）。
func (c *Config) ValidateRegistry() error {
	switch c.Registry.Mode {
	case ModeEmbedded:
		// embedded 模式下上游地址由 DistributionAddr 推导，
		// 不需要也不能配 upstream_addr：
		// 配了却被忽略 = 假开关（改了不生效却显示"已保存"）。
		if strings.TrimSpace(c.Registry.UpstreamAddr) != "" {
			return fmt.Errorf("registry.mode=%s 与 registry.upstream_addr 互斥："+
				"内嵌模式下上游地址由 registry.distribution_addr 推导，不需要也不能配 upstream_addr"+
				"（要连独立部署的 distribution，请设 CR_REGISTRY_MODE=%s）",
				ModeEmbedded, ModeExternal)
		}
		if strings.TrimSpace(c.Registry.DistributionAddr) == "" {
			return fmt.Errorf("registry.distribution_addr 不能为空（内嵌模式下它是 distribution 的监听地址，如 127.0.0.1:5001）")
		}
		if strings.TrimSpace(c.Registry.DistributionBin) == "" {
			return fmt.Errorf("registry.distribution_bin 不能为空（内嵌模式下它是 distribution 可执行文件的路径）")
		}
		return c.validateDistributionAddrs()
	case ModeExternal:
		return c.ValidateUpstream()
	default:
		return fmt.Errorf("registry.mode 非法: %q（支持 %s|%s）",
			c.Registry.Mode, ModeEmbedded, ModeExternal)
	}
}

// validateDistributionAddrs 校验内嵌模式下三个监听地址互不冲突。
//
// 内嵌模式一共起三个监听：
//
//	distribution 本体   registry.distribution_addr
//	distribution debug  registry.distribution_debug_addr（/metrics）
//	lau6-cr 本体        server.listen_addr
//
// ⚠️ 端口撞了的后果是 **bind 失败**：谁后起来谁退出，
//
//	症状是"日志里 distribution 已就绪、接着 lau6-cr 报
//	address already in use 然后退出"——不校验就得靠猜。
func (c *Config) validateDistributionAddrs() error {
	debug := strings.TrimSpace(c.Registry.DistributionDebugAddr)
	if debug == "" {
		// 留空 = 不开 debug server（也就没有 /metrics）。这是允许的。
		return nil
	}
	if _, _, err := net.SplitHostPort(debug); err != nil {
		return fmt.Errorf("registry.distribution_debug_addr 必须是 host:port 形式: %q", debug)
	}
	dist := strings.TrimSpace(c.Registry.DistributionAddr)
	if dist != "" && AddrsConflict(dist, debug) {
		return fmt.Errorf("registry.distribution_debug_addr (%s) 与 registry.distribution_addr (%s) 端口冲突"+
			"（同一个端口上绑两个监听必然 bind 失败）", debug, dist)
	}
	if AddrsConflict(c.Server.ListenAddr, debug) {
		return fmt.Errorf("registry.distribution_debug_addr (%s) 与 server.listen_addr (%s) 端口冲突"+
			"（同一个端口上绑两个监听必然 bind 失败）", debug, c.Server.ListenAddr)
	}
	return nil
}

// AddrsConflict 判定两个 host:port 监听地址是否会在同一端口上打架。
//
// 判据：端口相同，且两边的 host 要么相同、要么有一边是通配
// （0.0.0.0 / :: / 空）——通配会覆盖另一边的具体地址。
//
// 不同端口、不同具体主机名 → 不冲突。
func AddrsConflict(a, b string) bool {
	hostA, portA, err := net.SplitHostPort(strings.TrimSpace(a))
	if err != nil {
		return false
	}
	hostB, portB, err := net.SplitHostPort(strings.TrimSpace(b))
	if err != nil {
		return false
	}
	if portA != portB {
		return false
	}
	if hostA == hostB {
		return true
	}
	return isWildcardHost(hostA) || isWildcardHost(hostB)
}

// isWildcardHost 判断 host 是否是通配地址（0.0.0.0 / :: / 空）。
func isWildcardHost(host string) bool {
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		return true
	}
	return false
}
