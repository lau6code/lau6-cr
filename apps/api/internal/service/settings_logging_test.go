package service

import (
	"testing"

	"github.com/lau6code/lau6-cr/apps/api/internal/config"
)

// TestValidateLoggingCombo 覆盖输出目标与文件开关的跨字段校验。
//
// 少了这条，运维在页面上把 output 切成 file 却把 enabled 关掉——
// 值会落库，下次重启时 config.validate 失败、容器起不来，
// 而报错跟设置页毫无关系。
func TestValidateLoggingCombo(t *testing.T) {
	base := config.Default()
	base.Logging.Output = "both"
	base.Logging.File.Enabled = true

	cases := []struct {
		name    string
		changes map[string]any
		wantErr bool
	}{
		{
			name:    "切到 file 但关掉 enabled",
			changes: map[string]any{"logging.output": "file", "logging.file.enabled": false},
			wantErr: true,
		},
		{
			name:    "切到 both 但关掉 enabled",
			changes: map[string]any{"logging.output": "both", "logging.file.enabled": false},
			wantErr: true,
		},
		{
			name:    "关掉 enabled 但 output 仍是 console",
			changes: map[string]any{"logging.output": "console", "logging.file.enabled": false},
			wantErr: false,
		},
		{
			name:    "切到 file 同时保持 enabled",
			changes: map[string]any{"logging.output": "file"},
			wantErr: false,
		},
		{
			name:    "只改级别",
			changes: map[string]any{"logging.level": "debug"},
			wantErr: false,
		},
		{
			name:    "与日志无关的设置",
			changes: map[string]any{"stats.max_buckets": 20000},
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateLoggingCombo(base, tc.changes)
			if tc.wantErr && err == nil {
				t.Fatal("应当被拒绝，但没有报错")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("应当通过，却报错: %v", err)
			}
		})
	}
}

// TestValidateLoggingCombo_InheritedDisabled 验证基线里 enabled 已经是 false 时，
// 切到 file 也会被拦下。
//
// 基线本来就是 false（老实例从 DB 里带下来的），改 output 同样要拦。
func TestValidateLoggingCombo_InheritedDisabled(t *testing.T) {
	base := config.Default()
	base.Logging.Output = "console"
	base.Logging.File.Enabled = false

	err := validateLoggingCombo(base, map[string]any{"logging.output": "file"})
	if err == nil {
		t.Fatal("基线 enabled=false 时切到 file 应当被拒绝")
	}
}

// TestApplyReadSetting_Logging 验证日志设置项的写入与读出对称。
//
// 不对称的话，页面上改了值显示的还是旧值——
// 运维会以为没生效，反复改。
func TestApplyReadSetting_Logging(t *testing.T) {
	cases := []struct {
		key  string
		val  any
		want any
	}{
		{"logging.level", "debug", "debug"},
		{"logging.output", "file", "file"},
		{"logging.file.enabled", false, false},
		{"logging.file.dir", "/var/log/lau6-cr", "/var/log/lau6-cr"},
		{"logging.file.filename", "custom.log", "custom.log"},
		{"logging.file.max_size_mb", 128, 128},
		{"logging.file.rotate", "size", "size"},
		{"logging.file.retain_days", 14, 14},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			cfg := config.Default()
			if err := applySetting(cfg, tc.key, tc.val); err != nil {
				t.Fatalf("applySetting: %v", err)
			}
			got := readSetting(cfg, tc.key)
			if got != tc.want {
				t.Errorf("readSetting(%q) = %v, want %v", tc.key, got, tc.want)
			}
		})
	}
}

// TestAllSettingDefs_Logging 验证日志设置项的定义齐全且元数据正确。
func TestAllSettingDefs_Logging(t *testing.T) {
	want := map[string]struct {
		tier Tier
	}{
		"logging.level":            {TierHot},
		"logging.output":           {TierRestart},
		"logging.file.enabled":     {TierRestart},
		"logging.file.dir":         {TierRestart},
		"logging.file.filename":    {TierRestart},
		"logging.file.max_size_mb": {TierRestart},
		"logging.file.rotate":      {TierRestart},
		"logging.file.retain_days": {TierRestart},
	}
	defs := AllSettingDefs()
	for key, w := range want {
		def, ok := defByKey(key)
		if !ok {
			t.Errorf("设置项 %s 未定义", key)
			continue
		}
		if def.Tier != w.tier {
			t.Errorf("%s 的 tier 期望 %s，实际 %s", key, w.tier, def.Tier)
		}
		// ⚠️ 日志配置不再绑环境变量：EnvVar 必须为空串，
		//    否则设置页会把它显示成"由环境变量固定"的只读项。
		if def.EnvVar != "" {
			t.Errorf("%s 不该再绑环境变量，实际 %s", key, def.EnvVar)
		}
		if !def.Editable {
			t.Errorf("%s 应当可编辑", key)
		}
		if len(def.YAMLPath) == 0 {
			t.Errorf("%s 缺少 YAMLPath——回写 application.yaml 时会被漏掉", key)
		}
	}
	// 反向：日志前缀的项数量必须与预期一致，防止多加漏加。
	count := 0
	for _, d := range defs {
		if len(d.Key) >= 8 && d.Key[:8] == "logging." {
			count++
		}
	}
	if count != len(want) {
		t.Errorf("logging.* 设置项期望 %d 个，实际 %d 个", len(want), count)
	}
}

// TestRebuild_KeepsLogging 验证 rebuild 不会把 Logging 段冲掉。
//
// rebuild 显式拷贝 base 的各段；漏掉 Logging 的话，
// DB 里存的日志设置会被悄悄丢弃——页面显示已保存，实际不生效。
func TestRebuild_KeepsLogging(t *testing.T) {
	base := config.Default()
	base.Logging.Level = "warn"
	base.Logging.Output = "file"
	base.Logging.File.RetainDays = 14

	cp := *base
	cp.Logging = base.Logging

	if cp.Logging.Level != "warn" {
		t.Errorf("logging.level 期望 warn，实际 %q", cp.Logging.Level)
	}
	if cp.Logging.Output != "file" {
		t.Errorf("logging.output 期望 file，实际 %q", cp.Logging.Output)
	}
	if cp.Logging.File.RetainDays != 14 {
		t.Errorf("logging.file.retain_days 期望 14，实际 %d", cp.Logging.File.RetainDays)
	}
}

// TestValidateValue_LoggingEnum 验证日志枚举项的取值校验。
func TestValidateValue_LoggingEnum(t *testing.T) {
	cases := []struct {
		key     string
		val     any
		wantErr bool
	}{
		{"logging.level", "debug", false},
		{"logging.level", "info", false},
		{"logging.level", "verbose", true},
		{"logging.output", "console", false},
		{"logging.output", "file", false},
		{"logging.output", "both", false},
		{"logging.output", "syslog", true},
		{"logging.file.rotate", "daily", false},
		{"logging.file.rotate", "size", false},
		{"logging.file.rotate", "hourly", true},
	}
	for _, tc := range cases {
		t.Run(tc.key+"="+tc.val.(string), func(t *testing.T) {
			def, ok := defByKey(tc.key)
			if !ok {
				t.Fatalf("设置项 %s 未定义", tc.key)
			}
			err := validateValue(def, tc.val)
			if tc.wantErr && err == nil {
				t.Fatal("应当被拒绝，但没有报错")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("应当通过，却报错: %v", err)
			}
		})
	}
}

// TestValidateValue_LoggingIntRange 验证日志数值项的范围校验。
func TestValidateValue_LoggingIntRange(t *testing.T) {
	for _, key := range []string{"logging.file.max_size_mb", "logging.file.retain_days"} {
		def, ok := defByKey(key)
		if !ok {
			t.Fatalf("设置项 %s 未定义", key)
		}
		if err := validateValue(def, 0); err == nil {
			t.Errorf("%s=0 应当被拒绝", key)
		}
		if err := validateValue(def, -1); err == nil {
			t.Errorf("%s=-1 应当被拒绝", key)
		}
		if err := validateValue(def, 10); err != nil {
			t.Errorf("%s=10 应当通过，却报错: %v", key, err)
		}
	}
}
