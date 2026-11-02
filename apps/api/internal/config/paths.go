package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// resolvePaths 把以 /data 开头的路径重定位到 DataDir 下。
//
// 这样本地开发与自定义数据目录都只需设 CR_DATA_DIR，
// 不必为每个路径单独设环境变量。
func (c *Config) resolvePaths() {
	if c.DataDir == "" || c.DataDir == "/data" {
		return
	}
	base := strings.TrimRight(c.DataDir, `/\`)
	rel := func(p string) string {
		if p == "" {
			return p
		}
		if strings.HasPrefix(p, "/data/") {
			// 用 filepath.Join 拼，而不是 base + 剩余子串——
			// 后者在 Windows 上会拼出 "D:\x/data/logs" 这种混分隔符的路径。
			return filepath.Join(base, filepath.FromSlash(strings.TrimPrefix(p, "/data/")))
		}
		if p == "/data" {
			return base
		}
		return p
	}
	c.Database.SQLitePath = rel(c.Database.SQLitePath)
	c.Keystore.PrivatePath = rel(c.Keystore.PrivatePath)
	c.Keystore.JWKSPath = rel(c.Keystore.JWKSPath)
	c.Keystore.WebhookSecretPath = rel(c.Keystore.WebhookSecretPath)
	c.Keystore.RegistrySecretPath = rel(c.Keystore.RegistrySecretPath)
	c.Keystore.PasswordKeyPath = rel(c.Keystore.PasswordKeyPath)
	c.Storage.Local.Root = rel(c.Storage.Local.Root)
	// 日志目录默认在 /data/logs，同样要跟着 DataDir 走——
	// 否则本地开发（CR_DATA_DIR 指向别处）会往容器的 /data/logs 写。
	c.Logging.File.Dir = rel(c.Logging.File.Dir)
}

// EffectiveUpstream 返回当前生效的上游 distribution 地址。
//
//   - embedded 模式：由 registry.distribution_addr 推导（http://<addr>）；
//   - external 模式：registry.upstream_addr（必须已通过校验）。
//
// ⚠️ 它**不读环境变量**，只读当前配置快照——
//
//	运行期用的是设置快照（live），不是启动时的原始配置。
func (c *Config) EffectiveUpstream() (string, error) {
	switch c.Registry.Mode {
	case ModeEmbedded:
		addr := strings.TrimSpace(c.Registry.DistributionAddr)
		if addr == "" {
			return "", fmt.Errorf("registry.distribution_addr 不能为空（内嵌模式下它是 distribution 的监听地址）")
		}
		return "http://" + strings.TrimRight(addr, "/"), nil
	case ModeExternal:
		if err := c.ValidateUpstream(); err != nil {
			return "", err
		}
		return strings.TrimRight(c.Registry.UpstreamAddr, "/"), nil
	default:
		return "", fmt.Errorf("registry.mode 非法: %q（支持 %s|%s）",
			c.Registry.Mode, ModeEmbedded, ModeExternal)
	}
}

// CallbackBaseURL 返回 distribution 回调 lau6-cr 时该用的基地址。
//
// 内嵌模式下 distribution 与 lau6-cr 同容器，回调走回环地址最省事
// （不走公网、不依赖 public_url 配得对不对）。
//
// ⚠️ 它由 server.listen_addr 推导：监听 0.0.0.0 / :: 时用 127.0.0.1，
//
//	否则用监听地址里的 host。
func (c *Config) CallbackBaseURL() string {
	host, port, err := net.SplitHostPort(c.Server.ListenAddr)
	if err != nil {
		// listen_addr 不是 host:port 形式（理论上 validate 已挡住）：
		// 这里不猜地址，返回空串让调用方报错。
		return ""
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// ApplicationYAMLPath 返回运行时配置文件路径。
//
// 它位于 DataDir 下，**不是**镜像里的 /etc ——
// setup 写在那里，升级换镜像才不会丢。
func (c *Config) ApplicationYAMLPath() string {
	return filepath.Join(c.DataDir, "config", "application.yaml")
}

// SQLiteDSN 拼出 SQLite 的 DSN（含每连接 PRAGMA）。
func (c *Config) SQLiteDSN() string {
	return "file:" + c.Database.SQLitePath +
		"?_pragma=foreign_keys(1)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(NORMAL)"
}

// DatabaseEnvVar 返回把数据库配置钉死的环境变量名（空串 = 没钉死）。
//
// env 的优先级最高（见包注释）：被 env 钉死时，
// 向导里改的数据库连接**重启后就没了**——所以 UI 必须把它显示成只读，
// 不能让人以为"改了会生效"。
//
// 这也是初始化向导里数据库步骤的边界：
// env 钉死 → 只读；没钉死 → 可以改，且写进 /data/config/application.yaml。
func (c *Config) DatabaseEnvVar() string {
	if os.Getenv("CR_DB_DRIVER") != "" {
		return "CR_DB_DRIVER"
	}
	switch c.Database.Driver {
	case "sqlite":
		if os.Getenv("CR_DB_SQLITE_PATH") != "" {
			return "CR_DB_SQLITE_PATH"
		}
	default:
		if os.Getenv("CR_DB_DSN") != "" {
			return "CR_DB_DSN"
		}
	}
	return ""
}

// EnsureDirs 创建运行期需要的目录。
func (c *Config) EnsureDirs() error {
	dirs := []string{
		filepath.Dir(c.Database.SQLitePath),
		filepath.Dir(c.Keystore.PrivatePath),
		filepath.Dir(c.Keystore.JWKSPath),
		filepath.Dir(c.Keystore.WebhookSecretPath),
		filepath.Dir(c.Keystore.RegistrySecretPath),
		filepath.Dir(c.Keystore.PasswordKeyPath),
	}
	if c.Storage.Driver == "local" {
		dirs = append(dirs, c.Storage.Local.Root)
	}
	// 日志目录：写文件日志时必须先建出来，
	// 否则首次写入会因目录不存在而失败（此时进程已经起来了，更难排查）。
	if c.loggingWritesFile() && c.Logging.File.Dir != "" {
		dirs = append(dirs, c.Logging.File.Dir)
	}
	for _, d := range dirs {
		if d == "" || d == "." {
			continue
		}
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("创建目录 %s: %w", d, err)
		}
	}
	return nil
}

// loggingWritesFile 判断是否要写文件日志。
func (c *Config) loggingWritesFile() bool {
	if !c.Logging.File.Enabled {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(c.Logging.Output)) {
	case "file", "both":
		return true
	default:
		return false
	}
}
