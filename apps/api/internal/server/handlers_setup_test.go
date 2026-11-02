package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lau6code/lau6-cr/apps/api/internal/config"
	"github.com/lau6code/lau6-cr/apps/api/internal/service"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// probeToken 是测试用的引导令牌。
const probeToken = "0123456789abcdef0123456789abcdef"

// newSetupTestApp 造一个能处理 /setup/* 的 App。
func newSetupTestApp(t *testing.T) *App {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db")
	db, err := store.Open("sqlite", dsn, 1, 1)
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("执行迁移: %v", err)
	}
	settings, err := service.NewSettingsManager(db, setupTestBaseConfig(t), "")
	if err != nil {
		t.Fatalf("构造设置管理器: %v", err)
	}
	logger, _ := newTestLogger()
	return &App{
		Logger: logger,
		Cfg:    &config.Config{},
		DB:     db,
		SetupSvc: service.NewSetupService(service.SetupDeps{
			DB:       db,
			Settings: settings,
		}),
	}
}

// setupTestBaseConfig 造一份给 setup 测试用的配置基线。
//
// 存储根目录指向临时目录：默认的 /data/registry 在测试机上未必可写，
// 而初始化会当场验它能不能写（不可写的话第一次 push 才炸）。
func setupTestBaseConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Storage.Local.Root = filepath.Join(t.TempDir(), "registry")
	return cfg
}

// newSetupTestAppWithConfig 造一个用指定配置基线的 setup App。
func newSetupTestAppWithConfig(t *testing.T, cfg *config.Config) *App {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db")
	db, err := store.Open("sqlite", dsn, 1, 1)
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("执行迁移: %v", err)
	}
	settings, err := service.NewSettingsManager(db, cfg, "")
	if err != nil {
		t.Fatalf("构造设置管理器: %v", err)
	}
	logger, _ := newTestLogger()
	return &App{
		Logger: logger,
		Cfg:    cfg,
		DB:     db,
		SetupSvc: service.NewSetupService(service.SetupDeps{
			DB:       db,
			Settings: settings,
		}),
	}
}

// postProbe 发一个连通性测试请求并返回状态码与响应体。
func postProbe(t *testing.T, app *App, path string, body any) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("序列化请求体: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	app.Routes().ServeHTTP(rec, req)

	var env map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("解析响应体 %q: %v", rec.Body.String(), err)
	}
	return rec.Code, env
}

// TestSetupTestDatabaseOK 覆盖数据库连通性测试端点：通的路径返回 200 + 结果。
func TestSetupTestDatabaseOK(t *testing.T) {
	app := newSetupTestApp(t)

	code, env := postProbe(t, app, "/setup/test/database", map[string]any{
		"bootstrap_token": probeToken,
		"database": map[string]any{
			"driver":      "sqlite",
			"sqlite_path": filepath.Join(t.TempDir(), "probe.db"),
		},
	})
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；响应 %v", code, env)
	}
	if env["code"] != float64(0) {
		t.Fatalf("业务码 = %v，期望 0", env["code"])
	}
	data, _ := env["data"].(map[string]any)
	if data == nil || data["detail"] == nil {
		t.Fatalf("响应里没有结果明细：%v", env)
	}
}

// TestSetupTestDatabaseBadParam 覆盖参数不合法 → 400（不是 200 + 假数据）。
func TestSetupTestDatabaseBadParam(t *testing.T) {
	app := newSetupTestApp(t)

	code, env := postProbe(t, app, "/setup/test/database", map[string]any{
		"bootstrap_token": probeToken,
		"database": map[string]any{
			"driver": "oracle",
		},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400；响应 %v", code, env)
	}
}

// TestSetupTestDatabaseUnreachable 覆盖连不上 → 503，且带上原因。
func TestSetupTestDatabaseUnreachable(t *testing.T) {
	app := newSetupTestApp(t)

	code, env := postProbe(t, app, "/setup/test/database", map[string]any{
		"bootstrap_token": probeToken,
		"database": map[string]any{
			"driver":   "postgres",
			"host":     "127.0.0.1",
			"port":     "1",
			"username": "user",
			"password": "pass",
			"name":     "lau6_cr",
		},
	})
	if code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d，期望 503；响应 %v", code, env)
	}
	msg, _ := env["message"].(string)
	if !strings.Contains(msg, "连接失败") {
		t.Fatalf("message = %q，期望说明是连接失败", msg)
	}
}

// TestSetupTestEndpointsClosedAfterInit 覆盖实例初始化后探测端点关闭。
//
// 这两个端点能连任意目标，留在已初始化的实例上就是长期探测面。
func TestSetupTestEndpointsClosedAfterInit(t *testing.T) {
	app := newSetupTestApp(t)

	if _, err := app.SetupSvc.Submit(context.Background(), service.SetupInput{
		BootstrapToken: probeToken,
		PublicURL:      "http://localhost:5173",
		AdminUsername:  "admin",
		AdminPassword:  "VerifyPass123!",
	}); err != nil {
		t.Fatalf("首次 Submit: %v", err)
	}

	code, env := postProbe(t, app, "/setup/test/database", map[string]any{
		"bootstrap_token": probeToken,
		"database": map[string]any{
			"driver":      "sqlite",
			"sqlite_path": filepath.Join(t.TempDir(), "after.db"),
		},
	})
	if code != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 409；响应 %v", code, env)
	}

	code, env = postProbe(t, app, "/setup/test/ldap", map[string]any{
		"bootstrap_token": probeToken,
		"url":             "ldap://127.0.0.1:1",
	})
	if code != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 409；响应 %v", code, env)
	}
}

// TestSetupTestLDAPUnreachable 覆盖 LDAP 连不上 → 503。
func TestSetupTestLDAPUnreachable(t *testing.T) {
	app := newSetupTestApp(t)

	code, env := postProbe(t, app, "/setup/test/ldap", map[string]any{
		"bootstrap_token": probeToken,
		"url":             "ldap://127.0.0.1:1",
		"user_segment":    "uid={0},ou=people",
	})
	if code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d，期望 503；响应 %v", code, env)
	}
}

// TestSetupTestRejectsBadToken 覆盖令牌不对 → 401。
func TestSetupTestRejectsBadToken(t *testing.T) {
	app := newSetupTestApp(t)

	code, env := postProbe(t, app, "/setup/test/ldap", map[string]any{
		"bootstrap_token": "short",
		"url":             "ldap://127.0.0.1:1",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400（令牌格式不对）；响应 %v", code, env)
	}
}

// TestSetupDatabasePrefillOmitsPassword 覆盖预填端点**不回显口令**。
//
// 未初始化时这个端点是开放面（那时还没有任何账号）——
// 口令出现在响应里等于把凭证发给了任何能访问实例的人。
func TestSetupDatabasePrefillOmitsPassword(t *testing.T) {
	app := newSetupTestApp(t)

	req := httptest.NewRequest(http.MethodGet, "/setup/database", nil)
	rec := httptest.NewRecorder()
	app.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；响应 %q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "password") {
		t.Fatalf("响应里出现了口令字段：%q", body)
	}
	var env map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	data, _ := env["data"].(map[string]any)
	if data == nil {
		t.Fatalf("响应里没有 data：%v", env)
	}
	if data["driver"] != "sqlite" {
		t.Fatalf("driver = %v, 期望 sqlite", data["driver"])
	}
}

// TestSetupStatusReportsEffectivePublicURL 覆盖引导状态里带上**生效**的公共访问 URL。
//
// 被 env 钉死的那一栏在向导里是只读的——
// 只给一个空的只读输入框过不了前端的"必须是 http(s) 开头"校验，初始化走不下去。
func TestSetupStatusReportsEffectivePublicURL(t *testing.T) {
	t.Setenv("CR_PUBLIC_URL", "https://registry.example.com")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("加载配置: %v", err)
	}
	app := newSetupTestAppWithConfig(t, cfg)

	req := httptest.NewRequest(http.MethodGet, "/setup/status", nil)
	rec := httptest.NewRecorder()
	app.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；响应 %q", rec.Code, rec.Body.String())
	}
	var env map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	data, _ := env["data"].(map[string]any)
	if data == nil {
		t.Fatalf("响应里没有 data：%v", env)
	}
	if data["public_url"] != "https://registry.example.com" {
		t.Fatalf("public_url = %v, 期望生效值 https://registry.example.com", data["public_url"])
	}
}

// TestSetupDatabaseClosedAfterInit 覆盖实例初始化后预填端点关闭。
func TestSetupDatabaseClosedAfterInit(t *testing.T) {
	app := newSetupTestApp(t)

	if _, err := app.SetupSvc.Submit(context.Background(), service.SetupInput{
		BootstrapToken: probeToken,
		PublicURL:      "http://localhost:5173",
		AdminUsername:  "admin",
		AdminPassword:  "VerifyPass123!",
	}); err != nil {
		t.Fatalf("首次 Submit: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/setup/database", nil)
	rec := httptest.NewRecorder()
	app.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 409；响应 %q", rec.Code, rec.Body.String())
	}
}

// TestSetupTestStorageLocalOK 覆盖本地存储的连通性测试端点。
func TestSetupTestStorageLocalOK(t *testing.T) {
	cfg := config.Default()
	// 根目录指向临时目录：默认的 /data/registry 不该在测试机上被创建。
	cfg.Storage.Local.Root = filepath.Join(t.TempDir(), "registry")
	app := newSetupTestAppWithConfig(t, cfg)

	code, env := postProbe(t, app, "/setup/test/storage", map[string]any{
		"bootstrap_token": probeToken,
		"storage": map[string]any{
			"driver": "local",
		},
	})
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；响应 %v", code, env)
	}
}

// TestSetupTestStorageBadParam 覆盖参数不全时的 400。
func TestSetupTestStorageBadParam(t *testing.T) {
	app := newSetupTestApp(t)

	code, _ := postProbe(t, app, "/setup/test/storage", map[string]any{
		"bootstrap_token": probeToken,
		"storage": map[string]any{
			"driver": "s3",
		},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400", code)
	}
}

// TestSetupTestStorageClosedAfterInit 覆盖"初始化完成后探测端点关闭"。
//
// 那个端点能连任意目标，留着就是长期可用的探测面。
func TestSetupTestStorageClosedAfterInit(t *testing.T) {
	app := newSetupTestApp(t)
	if err := app.DB.UpsertSetting(context.Background(), "system.initialized", true, "test"); err != nil {
		t.Fatalf("标记实例已初始化: %v", err)
	}

	code, _ := postProbe(t, app, "/setup/test/storage", map[string]any{
		"bootstrap_token": probeToken,
		"storage": map[string]any{
			"driver": "local",
		},
	})
	if code != http.StatusConflict {
		t.Fatalf("状态码 = %d，期望 409", code)
	}
}

// TestSetupStoragePrefillOmitsCredentials 覆盖存储预填**不含凭据**。
//
// 未初始化时这个端点是开放面，密钥绝不能回显。
func TestSetupStoragePrefillOmitsCredentials(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.Driver = "s3"
	cfg.Storage.S3.AccessKey = "AKIAEXAMPLE"
	cfg.Storage.S3.SecretKey = "s3cr3t"
	app := newSetupTestAppWithConfig(t, cfg)

	req := httptest.NewRequest(http.MethodGet, "/setup/storage", nil)
	rec := httptest.NewRecorder()
	app.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；响应 %q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "AKIAEXAMPLE") || strings.Contains(body, "s3cr3t") {
		t.Fatalf("响应里漏出了凭据：%q", body)
	}
	var env map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	data, _ := env["data"].(map[string]any)
	if data == nil {
		t.Fatalf("响应里没有 data：%v", env)
	}
	if data["driver"] != "s3" {
		t.Fatalf("driver = %v，期望 s3", data["driver"])
	}
}
