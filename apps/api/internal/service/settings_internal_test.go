package service

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/lau6code/lau6-cr/apps/api/internal/config"
)

// TestInternalSetting_NotExposedInView 验证内部键不出现在设置页视图里。
//
// 契约（docs/design/14-settings.md §3.9）：内部键"不暴露在
// GET /api/v1/settings 的返回里"。
//
// ⚠️ 为什么要在意：stats.rollup_cursor 是汇总任务自己维护的水位游标，
// 对管理员没有可操作性——显示出来只会让人想去改它。
func TestInternalSetting_NotExposedInView(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	base := config.Default()
	yamlPath := filepath.Join(t.TempDir(), "application.yaml")
	m, err := NewSettingsManager(db, base, yamlPath)
	if err != nil {
		t.Fatalf("构造设置管理器失败: %v", err)
	}

	view, err := m.View(ctx)
	if err != nil {
		t.Fatalf("读取设置视图失败: %v", err)
	}
	for _, s := range view.Settings {
		if IsInternalSetting(s.Key) {
			t.Fatalf("内部键 %s 出现在设置页视图里（应按 14-settings.md §3.9 隐藏）", s.Key)
		}
	}
}

// TestInternalSetting_NotAConfigDef 验证内部键不是设置项定义。
//
// 内部键由各自的子系统直接读写 DB（初始化标记、引导令牌、汇总游标），
// 不是配置项：投影进配置快照只会得到"改了没人读"的字段。
func TestInternalSetting_NotAConfigDef(t *testing.T) {
	for key := range internalSettingKeys {
		if _, ok := defByKey(key); ok {
			t.Errorf("内部键 %s 不该出现在设置项定义里", key)
		}
	}
}

// TestInternalSetting_RejectsPatch 验证内部键不能被 PATCH 改。
//
// 人手改游标会让汇总跳过或重复一段数据——必须拒。
func TestInternalSetting_RejectsPatch(t *testing.T) {
	db := newTestDB(t)

	base := config.Default()
	yamlPath := filepath.Join(t.TempDir(), "application.yaml")
	m, err := NewSettingsManager(db, base, yamlPath)
	if err != nil {
		t.Fatalf("构造设置管理器失败: %v", err)
	}

	version, err := db.GlobalSettingsVersion(context.Background())
	if err != nil {
		t.Fatalf("读取设置版本失败: %v", err)
	}
	for key := range internalSettingKeys {
		if _, err := m.Apply(context.Background(), "admin", version, map[string]any{key: int64(42)}); err == nil {
			t.Fatalf("应当拒绝修改内部键 %s，但通过了", key)
		}
		if _, err := m.Reset(context.Background(), "admin", key); err == nil {
			t.Fatalf("应当拒绝重置内部键 %s，但通过了", key)
		}
	}
}

// TestSettingDef_YAMLPathResolves 验证每个设置项的 YAMLPath 都能在配置里定位到字段。
//
// ⚠️ 设置项与配置字段之间的读写**完全靠 YAMLPath**（见 settings_field.go）：
// 路径写错或 yaml tag 对不上，读写会静默变成"没有这个设置项"。
// 这里把它钉死——新增设置项写错 YAMLPath 时立刻失败，而不是上线后才发现。
func TestSettingDef_YAMLPathResolves(t *testing.T) {
	cfg := config.Default()
	for _, def := range AllSettingDefs() {
		if len(def.YAMLPath) == 0 {
			t.Errorf("设置项 %s 没有 YAMLPath", def.Key)
			continue
		}
		if _, ok := config.FieldByYAMLPath(cfg, def.YAMLPath); !ok {
			t.Errorf("设置项 %s 的 YAMLPath %v 在配置里定位不到字段", def.Key, def.YAMLPath)
		}
	}
}

// TestProjectTokenTTLSetting 覆盖"项目令牌 TTL 设置项"。
func TestProjectTokenTTLSetting(t *testing.T) {
	def, ok := defByKey("core.project_token_ttl_seconds")
	if !ok {
		t.Fatal("缺少 core.project_token_ttl_seconds 设置项")
	}
	if def.Default != int(2592000) {
		t.Fatalf("默认值 = %v，期望 2592000（30 天）", def.Default)
	}
	if def.Min == nil || *def.Min != 3600 {
		t.Fatalf("下限 = %v，期望 3600", def.Min)
	}
	if def.Max == nil || *def.Max != 31536000 {
		t.Fatalf("上限 = %v，期望 31536000", def.Max)
	}
}

// TestPasswordModeDefaultIsOptional 覆盖"口令传输默认走 optional"。
//
// 默认 optional：前后端都发密文，旧客户端发明文仍兼容。
func TestPasswordModeDefaultIsOptional(t *testing.T) {
	def, ok := defByKey("password.mode")
	if !ok {
		t.Fatal("缺少 password.mode 设置项")
	}
	if def.Default != "optional" {
		t.Fatalf("默认值 = %v，期望 optional", def.Default)
	}
}
