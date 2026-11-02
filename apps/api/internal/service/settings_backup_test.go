package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lau6code/lau6-cr/apps/api/internal/config"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// newBackupTestManager 造一个指向临时目录的设置管理器。
func newBackupTestManager(t *testing.T) (*SettingsManager, string) {
	t.Helper()
	dir := t.TempDir()
	dsn := filepath.Join(dir, "test.db")
	db, err := store.Open("sqlite", dsn, 1, 1)
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("执行迁移: %v", err)
	}
	yamlPath := filepath.Join(dir, "application.yaml")
	m, err := NewSettingsManager(db, config.Default(), yamlPath)
	if err != nil {
		t.Fatalf("构造设置管理器: %v", err)
	}
	return m, yamlPath
}

// applyChange 改一项设置（带当前版本号，绕过乐观锁）。
func applyChange(t *testing.T, m *SettingsManager, key string, val any) {
	t.Helper()
	ctx := context.Background()
	version, err := m.db.GlobalSettingsVersion(ctx)
	if err != nil {
		t.Fatalf("读设置版本: %v", err)
	}
	if _, err := m.Apply(ctx, "admin", version, map[string]any{key: val}); err != nil {
		t.Fatalf("改设置 %s: %v", key, err)
	}
}

// TestBackupCreatedBeforeWrite 锁定"写文件之前先备份旧文件"。
//
// ⚠️ 这是回滚能成立的前提：旧配置被覆盖后就找不回来了。
func TestBackupCreatedBeforeWrite(t *testing.T) {
	m, yamlPath := newBackupTestManager(t)

	// 先手工放一份"旧配置"。
	old := "core:\n  public_url: http://old.example.com\n"
	if err := os.WriteFile(yamlPath, []byte(old), 0o600); err != nil {
		t.Fatalf("写配置文件: %v", err)
	}

	applyChange(t, m, "core.public_url", "https://new.example.com")

	backups, err := m.ListBackups()
	if err != nil {
		t.Fatalf("列备份: %v", err)
	}
	if len(backups) != 1 {
		t.Fatalf("备份份数 = %d，期望 1", len(backups))
	}
	// 备份里必须是**旧**内容，不是新内容。
	b, err := os.ReadFile(backups[0].File)
	if err != nil {
		t.Fatalf("读备份: %v", err)
	}
	if !strings.Contains(string(b), "old.example.com") {
		t.Fatalf("备份内容不是旧配置：%s", string(b))
	}
}

// TestBackupKeepsOnlyLatest20 锁定"只保留最近 20 份备份"。
func TestBackupKeepsOnlyLatest20(t *testing.T) {
	m, yamlPath := newBackupTestManager(t)

	if err := os.WriteFile(yamlPath, []byte("core:\n  public_url: http://v0\n"), 0o600); err != nil {
		t.Fatalf("写配置文件: %v", err)
	}
	// 写 25 次 → 25 份备份。
	for i := 0; i < 25; i++ {
		applyChange(t, m, "core.public_url", fmt.Sprintf("https://v%d.example.com", i+1))
	}

	backups, err := m.ListBackups()
	if err != nil {
		t.Fatalf("列备份: %v", err)
	}
	if len(backups) != backupKeep {
		t.Fatalf("备份份数 = %d，期望 %d", len(backups), backupKeep)
	}
}

// TestBackupDirIsSeparateFromConfig 锁定"备份放在独立的 backups/ 目录"。
//
// 放在同一目录的话，备份文件会被当成配置文件读进来。
func TestBackupDirIsSeparateFromConfig(t *testing.T) {
	m, yamlPath := newBackupTestManager(t)

	if err := os.WriteFile(yamlPath, []byte("core:\n  public_url: http://x\n"), 0o600); err != nil {
		t.Fatalf("写配置文件: %v", err)
	}
	applyChange(t, m, "core.public_url", "https://y.example.com")

	dir := m.backupsDir()
	if dir == filepath.Dir(yamlPath) {
		t.Fatalf("备份目录与配置文件同目录：%s", dir)
	}
	if filepath.Base(dir) != "backups" {
		t.Fatalf("备份目录名 = %q，期望 backups", filepath.Base(dir))
	}
}

// TestRollbackRestoresOldContent 锁定"回滚把备份内容写回配置文件"。
func TestRollbackRestoresOldContent(t *testing.T) {
	m, yamlPath := newBackupTestManager(t)

	old := "core:\n  public_url: http://old.example.com\n"
	if err := os.WriteFile(yamlPath, []byte(old), 0o600); err != nil {
		t.Fatalf("写配置文件: %v", err)
	}
	applyChange(t, m, "core.public_url", "https://new.example.com")

	backups, err := m.ListBackups()
	if err != nil {
		t.Fatalf("列备份: %v", err)
	}
	if len(backups) == 0 {
		t.Fatal("没有备份")
	}

	if _, err := m.Rollback(context.Background(), backups[0].Name); err != nil {
		t.Fatalf("回滚: %v", err)
	}
	b, err := os.ReadFile(yamlPath)
	if err != nil {
		t.Fatalf("读配置文件: %v", err)
	}
	if !strings.Contains(string(b), "old.example.com") {
		t.Fatalf("回滚后内容不对：%s", string(b))
	}
}

// TestRollbackRejectsPathTraversal 锁定"备份名不能带路径分隔符"。
//
// 不拦的话，"../application.yaml" 就能把备份目录外的文件读出来。
func TestRollbackRejectsPathTraversal(t *testing.T) {
	m, _ := newBackupTestManager(t)

	for _, name := range []string{"../application.yaml", "..\\application.yaml", "..", "."} {
		if _, err := m.Rollback(context.Background(), name); err == nil {
			t.Fatalf("备份名 %q 竟然回滚成功了", name)
		}
	}
}

// TestRollbackMissingBackupIsError 锁定"回滚到不存在的备份 → 报错"。
func TestRollbackMissingBackupIsError(t *testing.T) {
	m, _ := newBackupTestManager(t)

	if _, err := m.Rollback(context.Background(), "2020-01-01T00-00-00Z"); err == nil {
		t.Fatal("回滚到不存在的备份竟然成功了")
	}
}

// TestExportDoesNotDuplicateBackup 锁定"强制导出不会产生重复备份"。
//
// 备份由 exportYAMLDoc 内部完成；
// Export() 再单独备一次就会多出一份没意义的重复备份。
func TestExportDoesNotDuplicateBackup(t *testing.T) {
	m, yamlPath := newBackupTestManager(t)

	if err := os.WriteFile(yamlPath, []byte("core:\n  public_url: http://x\n"), 0o600); err != nil {
		t.Fatalf("写配置文件: %v", err)
	}
	applyChange(t, m, "core.public_url", "https://y.example.com")

	before, err := m.ListBackups()
	if err != nil {
		t.Fatalf("列备份: %v", err)
	}

	if err := m.Export(); err != nil {
		t.Fatalf("强制导出: %v", err)
	}

	after, err := m.ListBackups()
	if err != nil {
		t.Fatalf("列备份: %v", err)
	}
	if len(after) != len(before)+1 {
		t.Fatalf("导出后备份份数 = %d，期望 %d（导出也备份一次，但只该多一份）",
			len(after), len(before)+1)
	}
}
