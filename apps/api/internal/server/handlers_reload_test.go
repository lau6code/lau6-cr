package server

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/lau6code/lau6-cr/apps/api/internal/config"
)

// pendingKeys 返回视图里的"待重启"清单。
func pendingKeys(t *testing.T, app *App) []string {
	t.Helper()
	view, err := app.SettingsSvc.View(context.Background())
	if err != nil {
		t.Fatalf("读设置视图: %v", err)
	}
	return view.PendingRestart
}

func containsKey(keys []string, want string) bool {
	for _, k := range keys {
		if k == want {
			return true
		}
	}
	return false
}

// TestReloadLoggingFailureMarksPending 锁定"热切换失败要让用户看得见"。
//
// 日志级别切不过去时，设置本身是保存成功的（200），但运行期还沿用旧级别。
// 这时该项必须被标成"重启后生效"——否则用户只看到"已保存"，
// 以为改完当场就管用了（AGENTS.md 规则 1：调用方要能看出失败过）。
func TestReloadLoggingFailureMarksPending(t *testing.T) {
	app, _ := newSettingsTestApp(t)
	app.ReloadLogging = func(cfg *config.Config) error { return errors.New("改不动") }

	rec := patchSettings(t, app, map[string]any{"logging.level": "debug"})
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（设置本身保存成功了）：%s", rec.Code, rec.Body.String())
	}

	keys := pendingKeys(t, app)
	if !containsKey(keys, "logging.level") {
		t.Fatalf("热切换失败后 logging.level 应标成待重启，实际 pending_restart=%v", keys)
	}
}

// TestReloadLoggingSuccessClearsPending 锁定"热切换成功就不该再挂着待重启"。
//
// 标记是给用户看的运行时状态：这一项已经在运行期生效了，
// 再挂着"重启后生效"就是假提示。
func TestReloadLoggingSuccessClearsPending(t *testing.T) {
	app, _ := newSettingsTestApp(t)
	// 第一次切不过去 → 标成待重启。
	app.ReloadLogging = func(cfg *config.Config) error { return errors.New("改不动") }
	patchSettings(t, app, map[string]any{"logging.level": "debug"})
	if !containsKey(pendingKeys(t, app), "logging.level") {
		t.Fatalf("第一次热切换失败后应标成待重启，实际 %v", pendingKeys(t, app))
	}

	// 第二次切换成功 → 标记要撤掉。
	app.ReloadLogging = func(cfg *config.Config) error { return nil }
	patchSettings(t, app, map[string]any{"logging.level": "warn"})

	keys := pendingKeys(t, app)
	if containsKey(keys, "logging.level") {
		t.Fatalf("热切换成功后不该再标待重启，实际 pending_restart=%v", keys)
	}
}
