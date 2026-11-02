package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lau6code/lau6-cr/apps/api/internal/config"
	"github.com/lau6code/lau6-cr/apps/api/internal/service"
)

// newStorageSettingsTestApp 造一个 embedded 形态的设置页 App。
func newStorageSettingsTestApp(t *testing.T, reload func() error) *App {
	t.Helper()
	app, db := newRepoTestApp(t)
	base := config.Default()
	settings, err := service.NewSettingsManager(db, base,
		filepath.Join(t.TempDir(), "application.yaml"),
		service.WithDistributionEmbedded(true))
	if err != nil {
		t.Fatalf("构造设置管理器: %v", err)
	}
	app.SettingsSvc = settings
	app.SetupSvc = service.NewSetupService(service.SetupDeps{DB: db, Settings: settings})
	app.Stats = service.NewStatsAggregator(service.StatsOptions{DB: db})
	t.Cleanup(func() { app.Stats.Close() })
	app.ReloadConfig = reload
	if err := db.UpsertSetting(context.Background(), "system.initialized", true, "test"); err != nil {
		t.Fatalf("标记实例已初始化: %v", err)
	}
	return app
}

// patchSettings 发一次 PATCH /settings。
func patchSettings(t *testing.T, app *App, changes map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	version, err := app.DB.GlobalSettingsVersion(context.Background())
	if err != nil {
		t.Fatalf("读设置版本: %v", err)
	}
	body, err := json.Marshal(map[string]any{"version": version, "changes": changes})
	if err != nil {
		t.Fatalf("序列化请求: %v", err)
	}
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/settings", strings.NewReader(string(body)))
	req = req.WithContext(withPrincipal(req.Context(), adminPrincipal()))
	rec := httptest.NewRecorder()
	app.handlePatchSettings(rec, req)
	return rec
}

// TestPatchStorageSettingRejected 锁定"存储参数改不了"。
//
// 存储后端在 setup 阶段选定，之后整段锁死（docs/design/11-storage.md §6）：
// 换后端 / 换 bucket / 换 path style 都会让"已存在的 blob"与
// "配置指向的位置"对不上——症状是仓库记录都在、每次拉取都 blob unknown。
func TestPatchStorageSettingRejected(t *testing.T) {
	reloads := 0
	app := newStorageSettingsTestApp(t, func() error {
		reloads++
		return nil
	})

	keys := []string{
		"storage.driver",
		"storage.s3.endpoint",
		"storage.s3.bucket",
		"storage.s3.region",
		"storage.s3.path_style",
	}
	for _, key := range keys {
		rec := patchSettings(t, app, map[string]any{key: "https://minio.example.com"})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: 状态码 = %d，期望 400（body：%s）", key, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "只读") {
			t.Errorf("%s: 响应体应当说明只读：%s", key, rec.Body.String())
		}
	}
	if reloads != 0 {
		t.Fatalf("distribution 重启次数 = %d，期望 0", reloads)
	}
	if got := app.SettingsSvc.Snapshot().Storage.S3.Endpoint; got != "" {
		t.Fatalf("被拒绝的修改不该进快照，endpoint = %q", got)
	}
}

// TestPatchNonStorageSettingDoesNotRestartDistribution 锁定"改别的设置不重启 distribution"。
//
// 否则每次改个日志级别都要把 registry 抖一下。
func TestPatchNonStorageSettingDoesNotRestartDistribution(t *testing.T) {
	reloads := 0
	app := newStorageSettingsTestApp(t, func() error {
		reloads++
		return nil
	})

	rec := patchSettings(t, app, map[string]any{"logging.level": "debug"})
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（body：%s）", rec.Code, rec.Body.String())
	}
	if reloads != 0 {
		t.Fatalf("distribution 重启次数 = %d，期望 0", reloads)
	}
}
