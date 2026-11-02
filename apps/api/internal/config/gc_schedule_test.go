package config

import "testing"

// TestParseClock 验证时间点解析的严格性。
//
// 时间点决定"什么时候删文件"，解析宽松 = 配错了还自以为配好了。
func TestParseClock(t *testing.T) {
	cases := []struct {
		in      string
		wantH   int
		wantM   int
		wantErr bool
	}{
		{"02:00", 2, 0, false},
		{"00:00", 0, 0, false},
		{"23:59", 23, 59, false},
		{"02:30", 2, 30, false},
		{" 02:00 ", 2, 0, false},
		{"2:00", 0, 0, true},
		{"02:0", 0, 0, true},
		{"24:00", 0, 0, true},
		{"02:60", 0, 0, true},
		{"02-00", 0, 0, true},
		{"", 0, 0, true},
		{"凌晨两点", 0, 0, true},
	}
	for _, c := range cases {
		h, m, err := ParseClock(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseClock(%q) 应当报错，实际返回 %02d:%02d", c.in, h, m)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseClock(%q) 意外报错: %v", c.in, err)
			continue
		}
		if h != c.wantH || m != c.wantM {
			t.Errorf("ParseClock(%q) = %02d:%02d，期望 %02d:%02d", c.in, h, m, c.wantH, c.wantM)
		}
	}
}

// TestValidateCleanup_ScheduleAt 验证定时回收时间点的校验。
//
// 时间点写错 → 定时回收永远不触发，且没有任何报错——必须在启动时拦下来。
func TestValidateCleanup_ScheduleAt(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(c *Config)
		wantErr bool
	}{
		{"默认合法", func(c *Config) {}, false},
		{"合法时间点", func(c *Config) { c.Cleanup.GC.ScheduleAt = "03:30" }, false},
		{"时间点格式非法", func(c *Config) { c.Cleanup.GC.ScheduleAt = "2:30" }, true},
		{"小时越界", func(c *Config) { c.Cleanup.GC.ScheduleAt = "25:00" }, true},
		{"空时间点", func(c *Config) { c.Cleanup.GC.ScheduleAt = "" }, true},
		{"关掉定时则不校验时间点", func(c *Config) {
			c.Cleanup.GC.ScheduleEnabled = false
			c.Cleanup.GC.ScheduleAt = "乱写"
		}, false},
		{"GC 开启且是内嵌形态", func(c *Config) {
			c.Cleanup.GC.Enabled = true
		}, false},
		{"GC 开启、独立 registry 但没指配置文件", func(c *Config) {
			c.Cleanup.GC.Enabled = true
			c.Registry.Mode = ModeExternal
			c.Registry.DistributionConfigPath = ""
		}, true},
		{"GC 开启、独立 registry 且指了配置文件", func(c *Config) {
			c.Cleanup.GC.Enabled = true
			c.Registry.Mode = ModeExternal
			c.Registry.DistributionConfigPath = "/etc/registry/config.yml"
		}, false},
	}
	for _, c := range cases {
		cfg := Default()
		c.mutate(cfg)
		err := cfg.validateCleanup()
		if c.wantErr && err == nil {
			t.Errorf("%s：期望报错，实际通过", c.name)
		}
		if !c.wantErr && err != nil {
			t.Errorf("%s：意外报错: %v", c.name, err)
		}
	}
}

// TestDefault_GCSchedule 验证定时回收的默认值。
func TestDefault_GCSchedule(t *testing.T) {
	cfg := Default()
	if !cfg.Cleanup.GC.ScheduleEnabled {
		t.Errorf("cleanup.gc.schedule_enabled 默认应为 true")
	}
	if cfg.Cleanup.GC.ScheduleAt != "02:00" {
		t.Errorf("cleanup.gc.schedule_at 默认应为 02:00，实际 %q", cfg.Cleanup.GC.ScheduleAt)
	}
}
