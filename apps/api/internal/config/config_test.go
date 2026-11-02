package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDefault_Logging 验证日志默认配置。
func TestDefault_Logging(t *testing.T) {
	cfg := Default()
	if cfg.Logging.Level != "info" {
		t.Errorf("logging.level 期望 info，实际 %q", cfg.Logging.Level)
	}
	if cfg.Logging.Output != "both" {
		t.Errorf("logging.output 期望 both，实际 %q", cfg.Logging.Output)
	}
	if !cfg.Logging.File.Enabled {
		t.Error("logging.file.enabled 期望 true")
	}
	if cfg.Logging.File.Dir != "/data/logs" {
		t.Errorf("logging.file.dir 期望 /data/logs，实际 %q", cfg.Logging.File.Dir)
	}
	if cfg.Logging.File.Filename != "lau6-cr.log" {
		t.Errorf("logging.file.filename 期望 lau6-cr.log，实际 %q", cfg.Logging.File.Filename)
	}
	if cfg.Logging.File.MaxSizeMB <= 0 {
		t.Errorf("logging.file.max_size_mb 应 >0，实际 %d", cfg.Logging.File.MaxSizeMB)
	}
	if cfg.Logging.File.Rotate != "daily" {
		t.Errorf("logging.file.rotate 期望 daily，实际 %q", cfg.Logging.File.Rotate)
	}
	if cfg.Logging.File.RetainDays <= 0 {
		t.Errorf("logging.file.retain_days 应 >0，实际 %d", cfg.Logging.File.RetainDays)
	}
}

// TestApplyEnv_LoggingIgnored 验证日志配置**不再**从环境变量读。
//
// 日志配置统一走「系统设置 → 日志」（14-settings.md §3.7）与 application.yaml。
// 留着 CR_LOG_* 会造成"变量还在、设置页显示来源是环境变量、实际没人读"的假开关。
func TestApplyEnv_LoggingIgnored(t *testing.T) {
	t.Setenv("CR_LOG_LEVEL", "debug")
	t.Setenv("CR_LOG_OUTPUT", "file")
	t.Setenv("CR_LOG_FILE_ENABLED", "true")
	t.Setenv("CR_LOG_DIR", "/var/log/lau6-cr")
	t.Setenv("CR_LOG_FILENAME", "custom.log")
	t.Setenv("CR_LOG_MAX_SIZE_MB", "128")
	t.Setenv("CR_LOG_ROTATE", "size")
	t.Setenv("CR_LOG_RETAIN_DAYS", "14")

	cfg := Default()
	if err := cfg.applyEnv(); err != nil {
		t.Fatalf("applyEnv: %v", err)
	}

	if cfg.Logging.Level != "info" {
		t.Errorf("CR_LOG_LEVEL 不该再生效，logging.level 期望 info，实际 %q", cfg.Logging.Level)
	}
	if cfg.Logging.Output != "both" {
		t.Errorf("CR_LOG_OUTPUT 不该再生效，logging.output 期望 both，实际 %q", cfg.Logging.Output)
	}
	if cfg.Logging.File.Dir != "/data/logs" {
		t.Errorf("CR_LOG_DIR 不该再生效，logging.file.dir 期望 /data/logs，实际 %q", cfg.Logging.File.Dir)
	}
	if cfg.Logging.File.Filename != "lau6-cr.log" {
		t.Errorf("CR_LOG_FILENAME 不该再生效，logging.file.filename 期望 lau6-cr.log，实际 %q", cfg.Logging.File.Filename)
	}
	if cfg.Logging.File.MaxSizeMB != 64 {
		t.Errorf("CR_LOG_MAX_SIZE_MB 不该再生效，logging.file.max_size_mb 期望 64，实际 %d", cfg.Logging.File.MaxSizeMB)
	}
	if cfg.Logging.File.Rotate != "daily" {
		t.Errorf("CR_LOG_ROTATE 不该再生效，logging.file.rotate 期望 daily，实际 %q", cfg.Logging.File.Rotate)
	}
	if cfg.Logging.File.RetainDays != 7 {
		t.Errorf("CR_LOG_RETAIN_DAYS 不该再生效，logging.file.retain_days 期望 7，实际 %d", cfg.Logging.File.RetainDays)
	}
}

// TestResolvePaths_LoggingDir 验证日志目录跟随 CR_DATA_DIR 重定位。
//
// 不重定位的话，本地开发会把日志写到容器的 /data/logs，
// 在 Windows 上就是 C:\data\logs——一个谁也没打算建的地方。
func TestResolvePaths_LoggingDir(t *testing.T) {
	cfg := Default()
	cfg.DataDir = filepath.FromSlash("/tmp/lau6cr-data")
	cfg.resolvePaths()

	want := filepath.Join(cfg.DataDir, "logs")
	if cfg.Logging.File.Dir != want {
		t.Errorf("logging.file.dir 期望 %q，实际 %q", want, cfg.Logging.File.Dir)
	}
}

// TestResolvePaths_LoggingDirOutsideDataUnchanged 验证非 /data 前缀的日志目录不被改。
func TestResolvePaths_LoggingDirOutsideDataUnchanged(t *testing.T) {
	cfg := Default()
	cfg.DataDir = filepath.FromSlash("/tmp/lau6cr-data")
	cfg.Logging.File.Dir = "/var/log/lau6-cr"
	cfg.resolvePaths()

	if cfg.Logging.File.Dir != "/var/log/lau6-cr" {
		t.Errorf("非 /data 前缀的日志目录不应被重定位，实际 %q", cfg.Logging.File.Dir)
	}
}

// TestValidateLogging 验证日志配置校验。
func TestValidateLogging(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(c *Config)
		wantErr bool
	}{
		{"默认合法", func(c *Config) {}, false},
		{"level 非法", func(c *Config) { c.Logging.Level = "verbose" }, true},
		{"output 非法", func(c *Config) { c.Logging.Output = "syslog" }, true},
		{"output=file 但 enabled=false", func(c *Config) {
			c.Logging.Output = "file"
			c.Logging.File.Enabled = false
		}, true},
		{"output=both 但 enabled=false", func(c *Config) {
			c.Logging.Output = "both"
			c.Logging.File.Enabled = false
		}, true},
		{"dir 为空", func(c *Config) { c.Logging.File.Dir = "" }, true},
		{"filename 为空", func(c *Config) { c.Logging.File.Filename = "" }, true},
		{"max_size_mb 非法", func(c *Config) { c.Logging.File.MaxSizeMB = 0 }, true},
		{"retain_days 非法", func(c *Config) { c.Logging.File.RetainDays = 0 }, true},
		{"rotate 非法", func(c *Config) { c.Logging.File.Rotate = "hourly" }, true},
		{"output=console 忽略文件项", func(c *Config) {
			c.Logging.Output = "console"
			c.Logging.File.Enabled = false
			c.Logging.File.Dir = ""
			c.Logging.File.MaxSizeMB = 0
			c.Logging.File.RetainDays = 0
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(cfg)
			err := cfg.validateLogging()
			if tc.wantErr && err == nil {
				t.Fatal("期望报错，实际 nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("期望不报错，实际 %v", err)
			}
		})
	}
}

// TestLoad_EnvParseError 验证写错的环境变量会让加载直接失败。
//
// 静默跳过（保留默认值）会让设置页按"变量存在"显示
// "来源：环境变量"，而实际生效的是 YAML/默认值——页面在骗人。
// 所以这里必须启动即报错，把变量名和坏值一起抛出来。
func TestLoad_EnvParseError(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		value string
		want  string
	}{
		{"整数解析失败", "CR_SESSION_TTL", "abc", "环境变量 CR_SESSION_TTL=abc 无法解析为整数"},
		{"布尔解析失败", "CR_ANON_READ_ENABLED", "maybe", "环境变量 CR_ANON_READ_ENABLED=maybe 无法解析为布尔值"},
		{"时长缺单位", "CR_STATS_FLUSH_INTERVAL", "60", "环境变量 CR_STATS_FLUSH_INTERVAL=60 无法解析为时长"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)

			_, err := Load()
			if err == nil {
				t.Fatalf("环境变量 %s=%s 期望报错，实际 nil", tc.key, tc.value)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息期望包含 %q，实际 %q", tc.want, err.Error())
			}
		})
	}
}

// TestLoad_EnvValidValue 验证合法的环境变量照常生效。
func TestLoad_EnvValidValue(t *testing.T) {
	t.Setenv("CR_SESSION_TTL", "600")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Core.SessionTTLSeconds != 600 {
		t.Errorf("core.session_ttl_seconds 期望 600，实际 %d", cfg.Core.SessionTTLSeconds)
	}
}

// TestLoad_EnvEmptyValueKeepsDefault 验证空串环境变量不算错误、不覆盖默认值。
//
// 空串与"未设置"等价：编排文件里写 CR_SESSION_TTL= 与不写是同一个意思。
func TestLoad_EnvEmptyValueKeepsDefault(t *testing.T) {
	t.Setenv("CR_SESSION_TTL", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Core.SessionTTLSeconds != Default().Core.SessionTTLSeconds {
		t.Errorf("空串环境变量不应覆盖默认值，期望 %d，实际 %d",
			Default().Core.SessionTTLSeconds, cfg.Core.SessionTTLSeconds)
	}
}

// TestDefault_NoUpstreamAddr 验证上游地址**没有内置默认值**。
//
// 上游地址取决于部署形态（同容器回环 vs 独立容器服务名），猜一个默认值
// 只会让服务"看起来正常地跑着、实际全连不上"。
func TestDefault_NoUpstreamAddr(t *testing.T) {
	if got := Default().Registry.UpstreamAddr; got != "" {
		t.Errorf("registry.upstream_addr 期望空（无默认值），实际 %q", got)
	}
}

// TestValidateUpstream 验证上游地址的校验口径。
//
// 空值与非法 URL 都要报错——serve 路径据此致命退出，
// 不给"服务起来了但连不上上游"的假象。
func TestValidateUpstream(t *testing.T) {
	cases := []struct {
		name    string
		addr    string
		wantErr bool
	}{
		{"回环（同容器形态）", "http://127.0.0.1:5001", false},
		{"服务名（独立容器形态）", "http://registry:5000", false},
		{"空", "", true},
		{"缺 scheme", "registry:5000", true},
		{"缺 host", "http://", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.Registry.UpstreamAddr = tc.addr
			err := cfg.ValidateUpstream()
			if tc.wantErr && err == nil {
				t.Fatal("期望报错，实际 nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("期望不报错，实际 %v", err)
			}
		})
	}
}

// TestLoad_WithoutUpstreamAddr 验证不配上游也能加载配置。
//
// setup / config 这类子命令不碰上游，硬性要求它会造成死锁：
// 容器起不来 → /setup 页面也访问不到 → 永远没法初始化。
func TestLoad_WithoutUpstreamAddr(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Registry.UpstreamAddr != "" {
		t.Errorf("registry.upstream_addr 期望空，实际 %q", cfg.Registry.UpstreamAddr)
	}
}

// TestLoad_UpstreamFromEnv 验证上游地址可以用环境变量给出。
func TestLoad_UpstreamFromEnv(t *testing.T) {
	t.Setenv("CR_UPSTREAM_REGISTRY", "http://registry:5000")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Registry.UpstreamAddr != "http://registry:5000" {
		t.Errorf("registry.upstream_addr 期望 http://registry:5000，实际 %q", cfg.Registry.UpstreamAddr)
	}
}

// TestLoad_DistributionConfigPathFromEnv 验证 CR_DISTRIBUTION_CONFIG 仍然生效，
// 且优先级高于 YAML（运行期只从配置字段取值，环境变量必须在这里就落到同一个字段）。
func TestLoad_DistributionConfigPathFromEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "application.yaml")
	body := "registry:\n  distribution_config_path: /etc/registry/from-yaml.yml\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CR_DISTRIBUTION_CONFIG", "/etc/registry/from-env.yml")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if cfg.Registry.DistributionConfigPath != "/etc/registry/from-env.yml" {
		t.Errorf("期望 /etc/registry/from-env.yml，实际 %q", cfg.Registry.DistributionConfigPath)
	}
}

// TestEnsureDirs_CreatesLogDir 验证日志目录会被建出来。
func TestEnsureDirs_CreatesLogDir(t *testing.T) {
	base := t.TempDir()
	cfg := Default()
	cfg.DataDir = base
	cfg.resolvePaths()
	cfg.Database.SQLitePath = filepath.Join(base, "db", "lau6-cr.db")
	cfg.Keystore.PrivatePath = filepath.Join(base, "config", "jwt-private.pem")
	cfg.Keystore.JWKSPath = filepath.Join(base, "config", "jwt-jwks.json")
	cfg.Keystore.WebhookSecretPath = filepath.Join(base, "config", "webhook-secret")
	cfg.Keystore.RegistrySecretPath = filepath.Join(base, "config", "registry-secret")
	cfg.Storage.Local.Root = filepath.Join(base, "registry")

	if err := cfg.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}
	info, err := os.Stat(cfg.Logging.File.Dir)
	if err != nil {
		t.Fatalf("日志目录应被创建: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("日志路径不是目录: %s", cfg.Logging.File.Dir)
	}
}

// TestEnsureDirs_SkipsLogDirWhenConsoleOnly 验证 output=console 时不建日志目录。
func TestEnsureDirs_SkipsLogDirWhenConsoleOnly(t *testing.T) {
	base := t.TempDir()
	cfg := Default()
	cfg.DataDir = base
	cfg.resolvePaths()
	cfg.Logging.Output = "console"
	cfg.Logging.File.Enabled = false
	cfg.Database.SQLitePath = filepath.Join(base, "db", "lau6-cr.db")
	cfg.Keystore.PrivatePath = filepath.Join(base, "config", "jwt-private.pem")
	cfg.Keystore.JWKSPath = filepath.Join(base, "config", "jwt-jwks.json")
	cfg.Keystore.WebhookSecretPath = filepath.Join(base, "config", "webhook-secret")
	cfg.Keystore.RegistrySecretPath = filepath.Join(base, "config", "registry-secret")
	cfg.Storage.Local.Root = filepath.Join(base, "registry")

	if err := cfg.EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs: %v", err)
	}
	if _, err := os.Stat(cfg.Logging.File.Dir); !os.IsNotExist(err) {
		t.Errorf("console 模式不应创建日志目录，实际 err=%v", err)
	}
}
