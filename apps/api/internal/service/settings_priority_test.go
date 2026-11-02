package service

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/lau6code/lau6-cr/apps/api/internal/config"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// newPriorityTestManager 造一个设置管理器。
//
// base 用来模拟"env 已经覆盖到 YAML 之上"的加载结果——
// 真实路径是 config.Load()（env > yaml > default），测试里只能手工构造。
func newPriorityTestManager(t *testing.T, base *config.Config) (*SettingsManager, *store.DB) {
	t.Helper()
	db := newTestDB(t)
	m, err := NewSettingsManager(db, base, filepath.Join(t.TempDir(), "application.yaml"))
	if err != nil {
		t.Fatalf("构造设置管理器失败: %v", err)
	}
	return m, db
}

// TestSettingEnvBeatsDB 覆盖 "env > DB" 的优先级（14-settings.md §1）。
//
// 场景：先在设置页把 core.public_url 存成 A，之后才在 compose 里加上
// CR_PUBLIC_URL=B。此时 env 必须赢——库里那行 A 不参与覆盖。
//
// ⚠️ 判定看的是**生效值**，不是页面文案：
//
//	页面写着"来源：环境变量"、实际生效的却是库里的旧值，
//	是最坑的一种不一致——realm 会拿着旧地址继续对外服务。
func TestSettingEnvBeatsDB(t *testing.T) {
	ctx := context.Background()

	const envValue = "https://from-env.example.com"
	t.Setenv("CR_PUBLIC_URL", envValue)

	base := config.Default()
	base.Core.PublicURL = envValue
	m, db := newPriorityTestManager(t, base)

	// 模拟"设置页先存过 A，之后才加环境变量"：直接往表里写一行。
	if err := db.UpsertSetting(ctx, "core.public_url", "https://from-db.example.com", "admin"); err != nil {
		t.Fatalf("写 DB 设置失败: %v", err)
	}
	// 触发一次重建（改一个无关的键即可）。
	version, err := db.GlobalSettingsVersion(ctx)
	if err != nil {
		t.Fatalf("读设置版本失败: %v", err)
	}
	if _, err := m.Apply(ctx, "admin", version, map[string]any{"core.session_ttl_seconds": 3600}); err != nil {
		t.Fatalf("应用设置失败: %v", err)
	}

	if got := m.Snapshot().Core.PublicURL; got != envValue {
		t.Errorf("生效的 public_url = %q，期望 env 的 %q（DB 行必须被 env 压住）", got, envValue)
	}

	view, err := m.View(ctx)
	if err != nil {
		t.Fatalf("读取设置视图失败: %v", err)
	}
	for _, s := range view.Settings {
		if s.Key != "core.public_url" {
			continue
		}
		if s.Source != "env" {
			t.Errorf("core.public_url 的来源 = %q，期望 env", s.Source)
		}
		if s.Value != envValue {
			t.Errorf("core.public_url 的展示值 = %v，期望 %q", s.Value, envValue)
		}
		return
	}
	t.Fatal("设置视图里找不到 core.public_url")
}

// TestSettingDBBeatsYAML 覆盖 "DB > YAML"。
//
// 没有 env 介入时，设置页里存的值必须压过配置文件里的值。
func TestSettingDBBeatsYAML(t *testing.T) {
	ctx := context.Background()

	base := config.Default()
	base.Core.PublicURL = "https://from-yaml.example.com"
	m, db := newPriorityTestManager(t, base)

	version, err := db.GlobalSettingsVersion(ctx)
	if err != nil {
		t.Fatalf("读设置版本失败: %v", err)
	}
	if _, err := m.Apply(ctx, "admin", version, map[string]any{
		"core.public_url": "https://from-db.example.com",
	}); err != nil {
		t.Fatalf("应用设置失败: %v", err)
	}

	if got := m.Snapshot().Core.PublicURL; got != "https://from-db.example.com" {
		t.Errorf("生效的 public_url = %q，期望 DB 的值", got)
	}
}

// TestSettingApplyRejectsEnvPinned 验证"被 env 固定的项改不了"。
//
// 改了也存不进生效值（rebuild 会跳过被 env 固定的 DB 行），
// 页面却显示"已保存"——那是纯粹的假象，必须当场拒绝。
func TestSettingApplyRejectsEnvPinned(t *testing.T) {
	ctx := context.Background()

	t.Setenv("CR_PUBLIC_URL", "https://from-env.example.com")

	base := config.Default()
	base.Core.PublicURL = "https://from-env.example.com"
	m, db := newPriorityTestManager(t, base)

	version, err := db.GlobalSettingsVersion(ctx)
	if err != nil {
		t.Fatalf("读设置版本失败: %v", err)
	}
	_, err = m.Apply(ctx, "admin", version, map[string]any{
		"core.public_url": "https://from-db.example.com",
	})
	if err == nil {
		t.Fatal("被环境变量固定的设置项必须拒绝修改")
	}
}
