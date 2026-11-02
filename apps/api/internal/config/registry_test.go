package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestValidateRegistry 覆盖部署形态校验：互斥、必填、非法值。
func TestValidateRegistry(t *testing.T) {
	cases := []struct {
		name    string
		cfg     func() *Config
		wantErr bool
	}{
		{
			name: "embedded 模式不带 upstream_addr 合法",
			cfg:  func() *Config { return Default() },
		},
		{
			name: "embedded 模式带 upstream_addr 报错（配了不生效 = 假开关）",
			cfg: func() *Config {
				c := Default()
				c.Registry.UpstreamAddr = "http://registry:5000"
				return c
			},
			wantErr: true,
		},
		{
			name: "external 模式缺 upstream_addr 报错",
			cfg: func() *Config {
				c := Default()
				c.Registry.Mode = ModeExternal
				return c
			},
			wantErr: true,
		},
		{
			name: "external 模式带 upstream_addr 合法",
			cfg: func() *Config {
				c := Default()
				c.Registry.Mode = ModeExternal
				c.Registry.UpstreamAddr = "http://registry:5000"
				return c
			},
		},
		{
			name: "external 模式 upstream_addr 不是 URL 报错",
			cfg: func() *Config {
				c := Default()
				c.Registry.Mode = ModeExternal
				c.Registry.UpstreamAddr = "registry:5000"
				return c
			},
			wantErr: true,
		},
		{
			name: "非法 mode 报错",
			cfg: func() *Config {
				c := Default()
				c.Registry.Mode = "embeded"
				return c
			},
			wantErr: true,
		},
		{
			name: "embedded 模式缺监听地址报错",
			cfg: func() *Config {
				c := Default()
				c.Registry.DistributionAddr = ""
				return c
			},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg().ValidateRegistry()
			if tc.wantErr && err == nil {
				t.Fatal("期望报错，实际通过")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("期望通过，实际报错: %v", err)
			}
		})
	}
}

// TestValidateRegistry_AddrConflicts 覆盖"三个监听地址互不冲突"的校验。
//
// 撞端口的后果是 bind 失败：谁后起来谁退出，
// 症状是"日志里 distribution 已就绪、接着 lau6-cr 报 address already in use"，
// 不校验就只能靠猜。
func TestValidateRegistry_AddrConflicts(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{
			name:   "默认配置无冲突",
			mutate: func(*Config) {},
		},
		{
			name: "debug 地址与 distribution 监听端口相同 → 报错",
			mutate: func(c *Config) {
				c.Registry.DistributionDebugAddr = c.Registry.DistributionAddr
			},
			wantErr: true,
		},
		{
			name: "debug 端口与 lau6-cr 监听端口相同 → 报错（通配监听）",
			mutate: func(c *Config) {
				c.Server.ListenAddr = "0.0.0.0:5002"
				c.Registry.DistributionDebugAddr = "127.0.0.1:5002"
			},
			wantErr: true,
		},
		{
			name: "debug 地址留空 → 不开 debug server，合法",
			mutate: func(c *Config) {
				c.Registry.DistributionDebugAddr = ""
			},
		},
		{
			name: "debug 地址不是 host:port → 报错",
			mutate: func(c *Config) {
				c.Registry.DistributionDebugAddr = "5002"
			},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			tc.mutate(c)
			err := c.ValidateRegistry()
			if tc.wantErr && err == nil {
				t.Fatal("期望报错，实际通过")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("期望通过，实际报错: %v", err)
			}
		})
	}
}

// TestAddrsConflict 覆盖地址冲突判定本身。
func TestAddrsConflict(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"127.0.0.1:5001", "127.0.0.1:5001", true},
		{"0.0.0.0:5000", "127.0.0.1:5000", true},
		{"127.0.0.1:5000", "0.0.0.0:5000", true},
		{"127.0.0.1:5001", "127.0.0.1:5002", false},
		{"127.0.0.1:5001", "192.168.1.10:5001", false},
		{"not-a-host-port", "127.0.0.1:5001", false},
	}
	for _, tc := range cases {
		if got := AddrsConflict(tc.a, tc.b); got != tc.want {
			t.Errorf("AddrsConflict(%q, %q) 期望 %v，实际 %v", tc.a, tc.b, tc.want, got)
		}
	}
}

// TestEffectiveUpstream 覆盖上游地址的推导。
func TestEffectiveUpstream(t *testing.T) {
	c := Default()
	c.Registry.DistributionAddr = "127.0.0.1:5011"
	got, err := c.EffectiveUpstream()
	if err != nil {
		t.Fatalf("embedded 模式推导上游地址失败: %v", err)
	}
	if got != "http://127.0.0.1:5011" {
		t.Errorf("embedded 上游地址期望 http://127.0.0.1:5011，实际 %q", got)
	}

	c.Registry.Mode = ModeExternal
	c.Registry.UpstreamAddr = "http://registry:5000/"
	got, err = c.EffectiveUpstream()
	if err != nil {
		t.Fatalf("external 模式解析上游地址失败: %v", err)
	}
	if got != "http://registry:5000" {
		t.Errorf("external 上游地址期望 http://registry:5000，实际 %q", got)
	}
}

// TestDistributionConfigPathDefault 覆盖 distribution 配置路径的默认值与读取。
//
// 这条路径决定 blob 回收调度的是**哪份** distribution 配置——
// 必须是 registry 自己在用的那份（03-registry.md §5.5）。
func TestDistributionConfigPathDefault(t *testing.T) {
	c := Default()
	if c.Registry.DistributionConfigPath != "/run/distribution-config.yml" {
		t.Errorf("期望默认 /run/distribution-config.yml，实际 %q", c.Registry.DistributionConfigPath)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "application.yaml")
	body := "registry:\n  distribution_config_path: /etc/registry/config.yml\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if cfg.Registry.DistributionConfigPath != "/etc/registry/config.yml" {
		t.Errorf("期望 /etc/registry/config.yml，实际 %q", cfg.Registry.DistributionConfigPath)
	}
}

// TestCallbackBaseURL 覆盖事件回调基地址的推导。
func TestCallbackBaseURL(t *testing.T) {
	c := Default()
	c.Server.ListenAddr = "0.0.0.0:5000"
	if got := c.CallbackBaseURL(); got != "http://127.0.0.1:5000" {
		t.Errorf("期望 http://127.0.0.1:5000，实际 %q", got)
	}

	c.Server.ListenAddr = "127.0.0.1:5099"
	if got := c.CallbackBaseURL(); got != "http://127.0.0.1:5099" {
		t.Errorf("期望 http://127.0.0.1:5099，实际 %q", got)
	}

	c.Server.ListenAddr = "not-a-host-port"
	if got := c.CallbackBaseURL(); got != "" {
		t.Errorf("非法监听地址期望空串，实际 %q", got)
	}
}

// TestLoad_RejectsInvalidMode 覆盖非法 mode 在加载期就报错。
func TestLoad_RejectsInvalidMode(t *testing.T) {
	t.Setenv("CR_REGISTRY_MODE", "embeded")
	if _, err := Load(); err == nil {
		t.Fatal("非法 registry.mode 期望加载失败")
	}
}

// TestLoad_EmbeddedIgnoresNothing 覆盖"内嵌模式 + 给了上游地址 → 明确报错"。
//
// 不是静默忽略：静默忽略 = 配了不生效却显示"已保存"。
func TestLoad_EmbeddedWithUpstreamAddr(t *testing.T) {
	t.Setenv("CR_REGISTRY_MODE", ModeEmbedded)
	t.Setenv("CR_UPSTREAM_REGISTRY", "http://registry:5000")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}
	if err := cfg.ValidateRegistry(); err == nil {
		t.Fatal("内嵌模式给了 upstream_addr 期望报错")
	}
}
