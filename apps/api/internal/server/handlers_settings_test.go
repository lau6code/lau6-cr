package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lau6code/lau6-cr/apps/api/internal/config"
	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
	"github.com/lau6code/lau6-cr/apps/api/internal/service"
)

// newSettingsTestApp 造一个带设置管理器的 App。
//
// yamlPath 指向临时目录里的 application.yaml——备份与回滚都写在那儿。
func newSettingsTestApp(t *testing.T) (*App, string) {
	t.Helper()
	app, db := newRepoTestApp(t)
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "application.yaml")
	settings, err := service.NewSettingsManager(db, config.Default(), yamlPath)
	if err != nil {
		t.Fatalf("构造设置管理器: %v", err)
	}
	app.SettingsSvc = settings
	app.SetupSvc = service.NewSetupService(service.SetupDeps{DB: db, Settings: settings})
	// onSettingsChanged 会碰 a.Stats（生产里 main.go 一定会装上）。
	// 测试里也得装，否则会 nil 解引用。
	app.Stats = service.NewStatsAggregator(service.StatsOptions{DB: db})
	t.Cleanup(func() { app.Stats.Close() })
	if err := db.UpsertSetting(context.Background(), "system.initialized", true, "test"); err != nil {
		t.Fatalf("标记实例已初始化: %v", err)
	}
	return app, yamlPath
}

// applySettings 改一项设置，并带上**当前**版本号。
//
// ⚠️ 版本号必须对：Apply 用乐观锁，版本不匹配会返回
// ErrVersionConflict（"设置已被他人修改"）。
func applySettings(t *testing.T, app *App, changes map[string]any) {
	t.Helper()
	version, err := app.DB.GlobalSettingsVersion(context.Background())
	if err != nil {
		t.Fatalf("读设置版本: %v", err)
	}
	if _, err := app.SettingsSvc.Apply(context.Background(), "admin", version, changes); err != nil {
		t.Fatalf("改设置: %v", err)
	}
}

// TestGetSettingReturnsSingleItem 锁定 GET /settings/{key} 返回单项。
func TestGetSettingReturnsSingleItem(t *testing.T) {
	app, _ := newSettingsTestApp(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/settings/core.public_url", nil)
	req = req.WithContext(withPrincipal(req.Context(), adminPrincipal()))
	req = withRouteParams(req, map[string]string{"key": "core.public_url"})
	rec := httptest.NewRecorder()
	app.handleGetSetting(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（body：%s）", rec.Code, rec.Body.String())
	}
	var env struct {
		Data struct {
			Key string `json:"key"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if env.Data.Key != "core.public_url" {
		t.Fatalf("返回的 key = %q，期望 core.public_url", env.Data.Key)
	}
}

// TestGetSettingUnknownKeyIs404 锁定"未知设置项 → 404"。
//
// 回 200 + 空对象会让调用方以为"这项没有值"，
// 实际是"这项不存在"。
func TestGetSettingUnknownKeyIs404(t *testing.T) {
	app, _ := newSettingsTestApp(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/settings/no.such.key", nil)
	req = req.WithContext(withPrincipal(req.Context(), adminPrincipal()))
	req = withRouteParams(req, map[string]string{"key": "no.such.key"})
	rec := httptest.NewRecorder()
	app.handleGetSetting(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404（body：%s）", rec.Code, rec.Body.String())
	}
}

// TestGetSettingHidesNonPublicFromNormalUser 锁定"普通用户看不到非公开项"。
//
// ⚠️ 必须回 404 而不是 403：
// 403 等于告诉调用方"这项存在"，
// 而设置项的 key 本身就泄露了基础设施拓扑（Redis 地址、LDAP URL…）。
func TestGetSettingHidesNonPublicFromNormalUser(t *testing.T) {
	app, _ := newSettingsTestApp(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/settings/cache.redis.addr", nil)
	req = req.WithContext(withPrincipal(req.Context(), normalUserPrincipal()))
	req = withRouteParams(req, map[string]string{"key": "cache.redis.addr"})
	rec := httptest.NewRecorder()
	app.handleGetSetting(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404（body：%s）", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); strings.Contains(body, "redis.addr") {
		t.Fatalf("响应体泄露了设置项名：%s", body)
	}
}

// TestResetSettingDeletesDBRow 锁定"reset 删掉 DB 行、值回到默认"。
func TestResetSettingDeletesDBRow(t *testing.T) {
	app, _ := newSettingsTestApp(t)

	// 先改一项，再 reset 回默认。
	applySettings(t, app, map[string]any{
		"core.public_url": "https://changed.example.com",
	})
	if got := app.SettingsSvc.Snapshot().Core.PublicURL; got != "https://changed.example.com" {
		t.Fatalf("改后的值 = %q，期望 https://changed.example.com", got)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/settings/core.public_url/reset", nil)
	req = req.WithContext(withPrincipal(req.Context(), adminPrincipal()))
	req = withRouteParams(req, map[string]string{"key": "core.public_url"})
	rec := httptest.NewRecorder()
	app.handleResetSetting(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（body：%s）", rec.Code, rec.Body.String())
	}
	if got := app.SettingsSvc.Snapshot().Core.PublicURL; got != config.Default().Core.PublicURL {
		t.Fatalf("reset 后的值 = %q，期望回到默认 %q", got, config.Default().Core.PublicURL)
	}
}

// TestResetUnknownKeyIsRejected 锁定"未知设置项 → 400，不静默成功"。
func TestResetUnknownKeyIsRejected(t *testing.T) {
	app, _ := newSettingsTestApp(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/settings/no.such.key/reset", nil)
	req = req.WithContext(withPrincipal(req.Context(), adminPrincipal()))
	req = withRouteParams(req, map[string]string{"key": "no.such.key"})
	rec := httptest.NewRecorder()
	app.handleResetSetting(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400（body：%s）", rec.Code, rec.Body.String())
	}
}

// TestConfigExportWritesYAML 锁定"强制导出把生效配置写进 application.yaml"。
func TestConfigExportWritesYAML(t *testing.T) {
	app, yamlPath := newSettingsTestApp(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/config/export", nil)
	req = req.WithContext(withPrincipal(req.Context(), adminPrincipal()))
	rec := httptest.NewRecorder()
	app.handleConfigExport(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（body：%s）", rec.Code, rec.Body.String())
	}
	b, err := os.ReadFile(yamlPath)
	if err != nil {
		t.Fatalf("读配置文件: %v", err)
	}
	if !strings.Contains(string(b), "public_url") {
		t.Fatalf("导出的配置里没有 public_url：%s", string(b))
	}
}

// TestConfigBackupsListsFiles 锁定"备份列表能列出文件"。
func TestConfigBackupsListsFiles(t *testing.T) {
	app, yamlPath := newSettingsTestApp(t)

	// 先写一次配置，产生备份。
	if err := os.WriteFile(yamlPath, []byte("core:\n  public_url: http://x\n"), 0o600); err != nil {
		t.Fatalf("写配置文件: %v", err)
	}
	applySettings(t, app, map[string]any{
		"core.public_url": "https://y.example.com",
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/config/backups", nil)
	req = req.WithContext(withPrincipal(req.Context(), adminPrincipal()))
	rec := httptest.NewRecorder()
	app.handleConfigBackups(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（body：%s）", rec.Code, rec.Body.String())
	}
	var env struct {
		Data struct {
			Backups []service.ConfigBackup `json:"backups"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if len(env.Data.Backups) == 0 {
		t.Fatalf("备份列表为空，但写过一次配置（body：%s）", rec.Body.String())
	}
}

// TestConfigRollbackRestoresFile 锁定"回滚把备份内容写回配置文件"。
//
// ⚠️ 回滚只改**文件**：运行期的内存快照不动，
// 所以这里断言的是文件内容变了、而快照没变。
func TestConfigRollbackRestoresFile(t *testing.T) {
	app, yamlPath := newSettingsTestApp(t)

	original := "core:\n  public_url: http://original.example.com\n"
	if err := os.WriteFile(yamlPath, []byte(original), 0o600); err != nil {
		t.Fatalf("写配置文件: %v", err)
	}
	// 触发一次写入 → 产生备份（备份里是 original）。
	applySettings(t, app, map[string]any{
		"core.public_url": "https://changed.example.com",
	})
	// 确认文件已经不是原来的内容了。
	b, err := os.ReadFile(yamlPath)
	if err != nil {
		t.Fatalf("读配置文件: %v", err)
	}
	if strings.Contains(string(b), "original.example.com") {
		t.Fatalf("导出没有覆盖原内容，测试前提不成立：%s", string(b))
	}

	// 找一份备份并回滚。
	backups, err := app.SettingsSvc.ListBackups()
	if err != nil {
		t.Fatalf("列备份: %v", err)
	}
	if len(backups) == 0 {
		t.Fatal("没有备份可回滚")
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/config/rollback",
		strings.NewReader(`{"name":"`+backups[0].Name+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(withPrincipal(req.Context(), adminPrincipal()))
	rec := httptest.NewRecorder()
	app.handleConfigRollback(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（body：%s）", rec.Code, rec.Body.String())
	}
	b, err = os.ReadFile(yamlPath)
	if err != nil {
		t.Fatalf("读配置文件: %v", err)
	}
	if !strings.Contains(string(b), "original.example.com") {
		t.Fatalf("回滚后文件内容不是备份里的那份：%s", string(b))
	}
	// 响应必须说明"要重启才生效"——否则用户会以为回滚已经生效了。
	if !strings.Contains(rec.Body.String(), "pending_restart") {
		t.Fatalf("响应里没有说明需要重启：%s", rec.Body.String())
	}
}

// TestConfigRollbackRejectsPathTraversal 锁定"备份名不能带路径分隔符"。
//
// 不拦的话 "../../etc/passwd" 就能把任意文件读出来。
func TestConfigRollbackRejectsPathTraversal(t *testing.T) {
	app, _ := newSettingsTestApp(t)

	for _, name := range []string{"../application.yaml", "..\\application.yaml", ".."} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/config/rollback",
			strings.NewReader(`{"name":"`+name+`"}`))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(withPrincipal(req.Context(), adminPrincipal()))
		rec := httptest.NewRecorder()
		app.handleConfigRollback(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("备份名 %q 的状态码 = %d，期望 400（body：%s）", name, rec.Code, rec.Body.String())
		}
	}
}

// TestConfigEndpointsRequireAdmin 锁定"配置端点必须是系统管理员"。
//
// 回滚会覆盖配置文件——普通用户不该有这个权限。
//
// ⚠️ 这里直接测 requireAdmin 中间件，不走 app.Routes()：
// Routes() 里的 withSession 会用 cookie 解析出的主体**覆盖** context 里
// 预置的主体，测的就不是"管理员校验"而是"会话解析"了。
func TestConfigEndpointsRequireAdmin(t *testing.T) {
	app, _ := newSettingsTestApp(t)

	// 中间件包一个"如果进来了就说明放行了"的 handler。
	reached := false
	guarded := app.requireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))

	// 普通已登录用户 → 403。
	req := httptest.NewRequest(http.MethodPost, "/api/v1/config/rollback", nil)
	req = req.WithContext(withPrincipal(req.Context(), normalUserPrincipal()))
	rec := httptest.NewRecorder()
	guarded.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("普通用户的状态码 = %d，期望 403（body：%s）", rec.Code, rec.Body.String())
	}
	if reached {
		t.Fatal("普通用户竟然通过了 requireAdmin")
	}

	// 匿名 → 401。
	rec = httptest.NewRecorder()
	guarded.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/config/rollback", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("匿名的状态码 = %d，期望 401（body：%s）", rec.Code, rec.Body.String())
	}

	// 系统管理员 → 放行。
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/config/rollback", nil)
	req = req.WithContext(withPrincipal(req.Context(), adminPrincipal()))
	guarded.ServeHTTP(rec, req)
	if !reached {
		t.Fatal("系统管理员被 requireAdmin 拦下了")
	}
}

// normalUserPrincipal 造一个普通已登录用户（非系统管理员）。
func normalUserPrincipal() service.Principal {
	return service.Principal{
		User: &domain.User{ID: 2, Username: "alice", IsSystemAdmin: false},
	}
}
