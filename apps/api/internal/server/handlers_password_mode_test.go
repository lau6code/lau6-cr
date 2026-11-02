package server

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/lau6code/lau6-cr/apps/api/internal/config"
	"github.com/lau6code/lau6-cr/apps/api/internal/service"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// TestPasswordModeFailsClosedOnGarbage 覆盖「非法口令传输模式按 required 处理」。
//
// 一个安全控制不能被静默降级（fail-open）。
//
// 代价是"登录暂时失败并报明确错误"——那是看得见的失败，
// 比悄悄降级成明文安全得多。
func TestPasswordModeFailsClosedOnGarbage(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	db, err := store.Open("sqlite", dsn, 1, 1)
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("执行迁移: %v", err)
	}
	cfg := config.Default()
	settings, err := service.NewSettingsManager(db, cfg, "")
	if err != nil {
		t.Fatalf("构造设置管理器: %v", err)
	}
	logger, buf := newTestLogger()
	app := &App{Logger: logger, Cfg: cfg, DB: db, SettingsSvc: settings}

	// 把模式改成非法值（模拟手改库 / 迁移出错），再让运行期快照重算。
	//
	// ⚠️ 必须绕过设置层直接改库：Apply 会按 Enum 拦掉非法值，
	//    而这里要测的正是"非法值真的进了运行期"这条路径。
	if err := db.UpsertSetting(context.Background(), "password.mode", "not-a-mode", "test"); err != nil {
		t.Fatalf("写非法模式: %v", err)
	}
	if err := settings.SetBase(cfg); err != nil {
		t.Fatalf("重算快照: %v", err)
	}
	if got := app.passwordMode(); got != passwordModeRequired {
		t.Fatalf("passwordMode() = %q, 期望 %q（fail-closed）", got, passwordModeRequired)
	}
	// 必须留痕：降级成明文时至少还有一条 warn，现在改成明文更不能无声无息。
	if buf.Len() == 0 {
		t.Fatal("非法模式没有留下任何日志")
	}
}

// TestPasswordModeFreshInstanceIsOptional 锁定「全新实例的运行期生效模式」。
//
// ⚠️ 设置项定义（service.settings_defs）与内置默认值（config.Default）
//
//	是两层独立的默认值，任何一层单独写错都会让全新实例的实际生效值
//	与页面显示的不一致 ⇒ 公钥接口返回 enabled:false ⇒ 前端发明文。
//
//	文档写的"默认 optional"必须真的等于运行期的 optional。
func TestPasswordModeFreshInstanceIsOptional(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "test.db")
	// store.Open 会跑迁移（含 0008：把 password.mode 提升为 optional）。
	db, err := store.Open("sqlite", dsn, 1, 1)
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	defer func() { _ = db.Close() }()

	cfg := config.Default()
	settings, err := service.NewSettingsManager(db, cfg, "")
	if err != nil {
		t.Fatalf("构造设置管理器: %v", err)
	}
	logger, _ := newTestLogger()
	app := &App{Logger: logger, Cfg: cfg, DB: db, SettingsSvc: settings}

	if got := app.passwordMode(); got != passwordModeOptional {
		t.Fatalf("passwordMode() = %q, 期望 %q", got, passwordModeOptional)
	}
}

// TestPasswordModeKeepsValidValues 覆盖合法值不被这次修改带坏。
func TestPasswordModeKeepsValidValues(t *testing.T) {
	cases := []struct {
		name string
		mode string
		want string
	}{
		{"明文", passwordModePlaintext, passwordModePlaintext},
		{"可选", passwordModeOptional, passwordModeOptional},
		{"强制密文", passwordModeRequired, passwordModeRequired},
		// 没配置来源 ⇒ 回到内置默认值（optional），不是明文。
		{"空值回落到内置默认", "", passwordModeOptional},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dsn := filepath.Join(t.TempDir(), "test.db")
			db, err := store.Open("sqlite", dsn, 1, 1)
			if err != nil {
				t.Fatalf("打开测试库: %v", err)
			}
			defer func() { _ = db.Close() }()
			ctx := context.Background()
			cfg := config.Default()
			settings, err := service.NewSettingsManager(db, cfg, "")
			if err != nil {
				t.Fatalf("构造设置管理器: %v", err)
			}
			logger, _ := newTestLogger()
			app := &App{Logger: logger, Cfg: cfg, DB: db, SettingsSvc: settings}

			// 清掉 0008 迁移写入的行，让每种模式都由本用例显式决定。
			if err := db.DeleteSetting(ctx, "password.mode"); err != nil {
				t.Fatalf("清理迁移写入的行: %v", err)
			}
			if c.mode == "" {
				cfg.Password.Mode = ""
			} else if err := db.UpsertSetting(ctx, "password.mode", c.mode, "test"); err != nil {
				t.Fatalf("写模式: %v", err)
			}
			if err := settings.SetBase(cfg); err != nil {
				t.Fatalf("重算快照: %v", err)
			}
			if got := app.passwordMode(); got != c.want {
				t.Errorf("mode=%q → %q, 期望 %q", c.mode, got, c.want)
			}
		})
	}
}
