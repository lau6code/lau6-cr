package service

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/lau6code/lau6-cr/apps/api/internal/config"
)

// newStorageTestManager 造一个设置管理器，形态由 embedded 决定。
func newStorageTestManager(t *testing.T, base *config.Config, embedded bool) *SettingsManager {
	t.Helper()
	db := newTestDB(t)
	m, err := NewSettingsManager(db, base, filepath.Join(t.TempDir(), "application.yaml"),
		WithDistributionEmbedded(embedded))
	if err != nil {
		t.Fatalf("构造设置管理器失败: %v", err)
	}
	return m
}

// TestStorageSettingsLocked 锁定 storage.* 全部只读。
//
// 契约（docs/design/14-settings.md §3.6）：
//
//	存储后端在 setup 阶段选定，之后整段锁死——
//	driver、endpoint、bucket、region、path_style 任何一项改动，
//	都会让"已存在的 blob"与"配置指向的位置"对不上。
func TestStorageSettingsLocked(t *testing.T) {
	keys := []string{
		"storage.driver",
		"storage.s3.endpoint",
		"storage.s3.bucket",
		"storage.s3.region",
		"storage.s3.path_style",
	}
	for _, key := range keys {
		def, ok := defByKey(key)
		if !ok {
			t.Fatalf("设置项 %s 不存在", key)
		}
		if def.Editable {
			t.Errorf("%s 应当是只读的", key)
		}
		if def.Hint == "" {
			t.Errorf("%s 缺少 hint（用户看到禁用控件时要知道为什么不能改）", key)
		}
	}
}

// TestStorageSettingsApplyRejected 覆盖 embedded 形态下改存储参数。
//
// embedded 时 lau6-cr 自己渲染 distribution 的配置，但那不意味着
// 存储参数可以随时改：blob 已经写进旧位置了。
func TestStorageSettingsApplyRejected(t *testing.T) {
	ctx := context.Background()
	m := newStorageTestManager(t, config.Default(), true)

	version, err := m.db.GlobalSettingsVersion(ctx)
	if err != nil {
		t.Fatalf("读设置版本失败: %v", err)
	}
	if _, err := m.Apply(ctx, "admin", version, map[string]any{
		"storage.s3.endpoint":   "https://minio.example.com",
		"storage.s3.bucket":     "lau6-cr",
		"storage.s3.region":     "us-east-1",
		"storage.s3.path_style": true,
	}); err == nil {
		t.Fatal("改存储参数应当被拒绝")
	}
	snap := m.Snapshot()
	if snap.Storage.S3.Endpoint != "" {
		t.Errorf("被拒绝的修改不该进快照，endpoint = %q", snap.Storage.S3.Endpoint)
	}
}

// TestStorageSettingsResetRejected 覆盖"恢复默认"。
//
// 删掉 DB 行 = 回到配置文件/默认值 = 换了一个存储位置，
// 效果和直接改一样——所以只读的项连恢复默认都不行。
func TestStorageSettingsResetRejected(t *testing.T) {
	ctx := context.Background()
	m := newStorageTestManager(t, config.Default(), true)

	for _, key := range []string{"storage.driver", "storage.s3.endpoint", "storage.s3.bucket"} {
		if _, err := m.Reset(ctx, "admin", key); err == nil {
			t.Errorf("恢复默认 %s 应当被拒绝", key)
		}
	}
}

// TestStorageSettingsHiddenInExternal 覆盖 external 形态。
//
// 那时 distribution 用**它自己**的配置文件：lau6-cr 渲染不了也重启不了它。
// 所以 storage.* 既不能出现在设置页（显示一个改了没人读的入口），
// 也不能被 PATCH 改（改了不生效）。
func TestStorageSettingsHiddenInExternal(t *testing.T) {
	ctx := context.Background()
	m := newStorageTestManager(t, config.Default(), false)

	view, err := m.View(ctx)
	if err != nil {
		t.Fatalf("读取设置视图失败: %v", err)
	}
	for _, s := range view.Settings {
		if IsStorageSetting(s.Key) {
			t.Errorf("external 形态下 %s 不应出现在设置页", s.Key)
		}
	}

	version, err := m.db.GlobalSettingsVersion(ctx)
	if err != nil {
		t.Fatalf("读设置版本失败: %v", err)
	}
	if _, err := m.Apply(ctx, "admin", version, map[string]any{
		"storage.s3.endpoint": "https://minio.example.com",
	}); err == nil {
		t.Fatal("external 形态下改存储参数应当被拒绝")
	}
	if got := m.Snapshot().Storage.S3.Endpoint; got != "" {
		t.Errorf("被拒绝的修改不该进快照，endpoint = %q", got)
	}
}

// TestStorageSettingsVisibleInEmbedded 覆盖 embedded 形态下 storage.* 仍然可见。
//
// 可见但只读：用户得能看见"blob 到底放在哪"——
// 不显示等于让人猜，而猜错的后果（以为在本地、实际在对象存储）很难查。
func TestStorageSettingsVisibleInEmbedded(t *testing.T) {
	ctx := context.Background()
	m := newStorageTestManager(t, config.Default(), true)

	view, err := m.View(ctx)
	if err != nil {
		t.Fatalf("读取设置视图失败: %v", err)
	}
	seen := map[string]bool{}
	for _, s := range view.Settings {
		if IsStorageSetting(s.Key) {
			seen[s.Key] = true
			if s.Editable {
				t.Errorf("%s 在设置页上应当是只读的", s.Key)
			}
		}
	}
	for _, key := range []string{
		"storage.driver", "storage.s3.endpoint",
		"storage.s3.bucket", "storage.s3.region", "storage.s3.path_style",
	} {
		if !seen[key] {
			t.Errorf("%s 在 embedded 形态下应当出现在设置页", key)
		}
	}
}
