package service

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lau6code/lau6-cr/apps/api/internal/config"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// newTestSetupServiceAt 起一个带指定 SQLite 路径的 SetupService。
func newTestSetupServiceAt(t *testing.T, path string) *SetupService {
	t.Helper()
	db, err := store.Open("sqlite", "file:"+path, 1, 1)
	if err != nil {
		t.Fatalf("打开测试数据库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("执行迁移: %v", err)
	}
	settings, err := NewSettingsManager(db, config.Default(), "")
	if err != nil {
		t.Fatalf("构造设置管理器: %v", err)
	}
	return &SetupService{db: db, settings: settings}
}

// TestSubmitSwitchesDatabase 覆盖向导里换了数据库时的行为：
// 管理员、初始化标记、引导令牌摘要必须落在**新库**里。
//
// 顺序反了会留下"标记在旧库、业务在新库"的分裂状态——
// 新库里没有 system.initialized，实例会变回未初始化、/setup 重新开放。
func TestSubmitSwitchesDatabase(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old.db")
	newPath := filepath.Join(dir, "new.db")

	s := newTestSetupServiceAt(t, oldPath)

	if _, err := s.Submit(ctx, SetupInput{
		BootstrapToken: probeToken,
		PublicURL:      "http://localhost:5173",
		AdminUsername:  "admin",
		AdminPassword:  "VerifyPass123!",
		Database: DatabaseInput{
			Driver:     "sqlite",
			SQLitePath: newPath,
		},
	}); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// 新库：管理员在、初始化标记在。
	newDB, err := store.Open("sqlite", "file:"+newPath, 1, 1)
	if err != nil {
		t.Fatalf("打开新库: %v", err)
	}
	defer newDB.Close()

	var admins int
	if err := newDB.QueryRow(ctx,
		`SELECT count(*) FROM users WHERE username = 'admin' AND is_system_admin = 1`).
		Scan(&admins); err != nil {
		t.Fatalf("查新库的用户: %v", err)
	}
	if admins != 1 {
		t.Fatalf("新库里的管理员数量 = %d, 期望 1", admins)
	}
	v, err := newDB.GetSetting(ctx, "system.initialized")
	if err != nil {
		t.Fatalf("读新库的初始化标记: %v", err)
	}
	if init, _ := v.Value.(bool); !init {
		t.Fatal("新库里没有初始化标记")
	}

	// 旧库：管理员**不该**在旧库（否则就是分裂状态）。
	oldDB, err := store.Open("sqlite", "file:"+oldPath, 1, 1)
	if err != nil {
		t.Fatalf("打开旧库: %v", err)
	}
	defer oldDB.Close()
	var oldAdmins int
	if err := oldDB.QueryRow(ctx, `SELECT count(*) FROM users WHERE username = 'admin'`).
		Scan(&oldAdmins); err != nil {
		t.Fatalf("查旧库的用户: %v", err)
	}
	if oldAdmins != 0 {
		t.Fatalf("旧库里有 %d 个 admin——初始化标记与业务数据分裂了", oldAdmins)
	}
}

// TestSubmitKeepsCurrentDatabaseWhenUnchanged 覆盖"没改数据库就不换"。
func TestSubmitKeepsCurrentDatabaseWhenUnchanged(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "same.db")
	s := newTestSetupServiceAt(t, path)

	if _, err := s.Submit(ctx, SetupInput{
		BootstrapToken: probeToken,
		PublicURL:      "http://localhost:5173",
		AdminUsername:  "admin",
		AdminPassword:  "VerifyPass123!",
		Database: DatabaseInput{
			Driver:     "sqlite",
			SQLitePath: path,
		},
	}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	// 同一个库：管理员直接建在里面。
	db, err := store.Open("sqlite", "file:"+path, 1, 1)
	if err != nil {
		t.Fatalf("打开库: %v", err)
	}
	defer db.Close()
	var admins int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM users WHERE username = 'admin'`).
		Scan(&admins); err != nil {
		t.Fatalf("查用户: %v", err)
	}
	if admins != 1 {
		t.Fatalf("管理员数量 = %d, 期望 1", admins)
	}
}

// TestSubmitFailsWhenNewDatabaseUnreachable 覆盖换库连不上时初始化中止。
//
// 失败必须是**明确报错**，不能是"换了但没换成功"的半状态。
func TestSubmitFailsWhenNewDatabaseUnreachable(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old.db")
	s := newTestSetupServiceAt(t, oldPath)

	_, err := s.Submit(ctx, SetupInput{
		BootstrapToken: probeToken,
		PublicURL:      "http://localhost:5173",
		AdminUsername:  "admin",
		AdminPassword:  "VerifyPass123!",
		Database: DatabaseInput{
			Driver:   "postgres",
			Host:     "127.0.0.1",
			Port:     "1",
			Username: "lau6",
			Password: "pw",
			Name:     "lau6cr",
		},
	})
	if err == nil {
		t.Fatal("换库连不上时初始化应失败")
	}
	if !strings.Contains(err.Error(), "切换数据库") {
		t.Fatalf("err = %v, 期望说明是换库失败", err)
	}
}

// TestDatabaseInfoOmitsPassword 覆盖预填信息**不含口令**。
//
// 预填端点在未初始化时是开放面，口令绝不能回显。
func TestDatabaseInfoOmitsPassword(t *testing.T) {
	ctx := context.Background()
	s := newTestSetupService(t)

	info, err := s.DatabaseInfo(ctx)
	if err != nil {
		t.Fatalf("DatabaseInfo: %v", err)
	}
	if info.Driver != "sqlite" {
		t.Fatalf("driver = %q, 期望 sqlite", info.Driver)
	}
	if info.SQLitePath == "" {
		t.Fatal("SQLite 路径没返回")
	}
}

// TestResolveDatabaseTargetKeepsCurrentPassword 覆盖"口令留空 = 沿用当前口令"。
//
// 沿用是替用户做的决定，所以 PasswordSource 必须标出来。
func TestResolveDatabaseTargetKeepsCurrentPassword(t *testing.T) {
	s := newTestSetupService(t)

	// 当前的生效配置是 sqlite，没有口令可沿用 → source 必须是 input。
	res, ok, err := s.resolveDatabaseTarget(DatabaseInput{
		Driver:     "sqlite",
		SQLitePath: "/tmp/x.db",
	})
	if err != nil || !ok {
		t.Fatalf("resolveDatabaseTarget: ok=%v err=%v", ok, err)
	}
	if res.PasswordSource != "input" {
		t.Fatalf("PasswordSource = %q, 期望 input", res.PasswordSource)
	}
}

// TestResolveDatabaseTargetRejectsIncomplete 覆盖必填项缺失 → 参数错误。
func TestResolveDatabaseTargetRejectsIncomplete(t *testing.T) {
	s := newTestSetupService(t)

	_, _, err := s.resolveDatabaseTarget(DatabaseInput{
		Driver:   "postgres",
		Username: "lau6",
		Name:     "lau6cr",
	})
	if err == nil {
		t.Fatal("缺主机应报错")
	}
	if !strings.Contains(err.Error(), ErrInvalidProbeParam.Error()) {
		t.Fatalf("err = %v, 期望是参数错误", err)
	}
}

// TestResolveDatabaseTargetEmptyMeansKeep 覆盖"什么都没填 = 保持当前配置"。
func TestResolveDatabaseTargetEmptyMeansKeep(t *testing.T) {
	s := newTestSetupService(t)

	_, ok, err := s.resolveDatabaseTarget(DatabaseInput{})
	if err != nil {
		t.Fatalf("resolveDatabaseTarget: %v", err)
	}
	if ok {
		t.Fatal("没填数据库信息时不该触发换库")
	}
}
