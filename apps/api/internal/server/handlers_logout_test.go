package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/lau6code/lau6-cr/apps/api/internal/config"
	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
	"github.com/lau6code/lau6-cr/apps/api/internal/service"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// newLogoutTestApp 造一个已初始化、能处理 /auth/logout 的 App。
func newLogoutTestApp(t *testing.T) (*App, *store.DB, *domain.User) {
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
	logger, _ := newTestLogger()
	cfg := config.Default()
	settings, err := service.NewSettingsManager(db, cfg, "")
	if err != nil {
		t.Fatalf("构造设置管理器: %v", err)
	}
	ctx := context.Background()
	// 标记成"已初始化"——否则 requireSetupDone 会把请求挡在门外。
	if err := db.UpsertSetting(ctx, "system.initialized", true, "test"); err != nil {
		t.Fatalf("写初始化标记: %v", err)
	}
	user, err := db.CreateUser(ctx, "admin", "hash", "local", "管理员", true)
	if err != nil {
		t.Fatalf("建用户: %v", err)
	}
	return &App{
		Logger: logger,
		Cfg:    cfg,
		DB:     db,
		SetupSvc: service.NewSetupService(service.SetupDeps{
			DB:       db,
			Settings: settings,
		}),
		AuthSvc: service.NewAuthService(db, service.AuthOptions{Logger: logger}),
	}, db, user
}

// doLogout 发一个带会话 Cookie 的注销请求。
func doLogout(t *testing.T, app *App, sessionID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: app.Cfg.Session.CookieName, Value: sessionID})
	rec := httptest.NewRecorder()
	app.Routes().ServeHTTP(rec, req)
	return rec
}

// TestLogoutOK 覆盖正常注销：会话行删掉，回 200。
func TestLogoutOK(t *testing.T) {
	app, db, user := newLogoutTestApp(t)
	ctx := context.Background()
	if err := db.CreateSession(ctx, store.HashSessionID("raw-session-id"), user.ID, 9_999_999_999, "", ""); err != nil {
		t.Fatalf("造会话: %v", err)
	}

	rec := doLogout(t, app, "raw-session-id")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200；响应 %s", rec.Code, rec.Body.String())
	}
	var env map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("解析响应体: %v", err)
	}
	if env["code"] != float64(0) {
		t.Fatalf("业务码 = %v，期望 0", env["code"])
	}
	// 会话行必须真的没了。
	u, err := db.SessionUser(ctx, store.HashSessionID("raw-session-id"))
	if err != nil {
		t.Fatalf("读会话: %v", err)
	}
	if u != nil {
		t.Fatal("会话行还在，注销没有真的吊销会话")
	}
}

// TestLogoutFailsWhenSessionDestroyFails 覆盖「销毁会话失败必须回 5xx」。
//
// 销毁失败时那一把会话**仍然有效**（Cookie 清了、库里还在）——
// 用户以为登出了，实际没有，被截获的 Cookie 值在服务端照样能用，
// 而调用方无从得知（AGENTS.md 规则 3：记了日志却仍然返回成功 = 静默失败）。
func TestLogoutFailsWhenSessionDestroyFails(t *testing.T) {
	app, db, user := newLogoutTestApp(t)
	ctx := context.Background()
	if err := db.CreateSession(ctx, store.HashSessionID("raw-session-id"), user.ID, 9_999_999_999, "", ""); err != nil {
		t.Fatalf("造会话: %v", err)
	}
	// 让删除必然失败、但读取照常：触发器拦住 DELETE。
	//
	// ⚠️ 不能用 DROP TABLE——那会让 withSession 中间件读会话时先失败，
	//    请求根本到不了 handler，测的就不是这段逻辑了。
	if _, err := db.Exec(ctx, `
		CREATE TRIGGER block_session_delete BEFORE DELETE ON sessions
		BEGIN SELECT RAISE(ABORT, '会话行删不掉'); END;`); err != nil {
		t.Fatalf("造触发器: %v", err)
	}

	rec := doLogout(t, app, "raw-session-id")
	if rec.Code == http.StatusOK {
		t.Fatalf("销毁会话失败却回了 200：%s", rec.Body.String())
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500；响应 %s", rec.Code, rec.Body.String())
	}
	// 会话行必须还在——那把会话确实仍然有效，所以必须告诉调用方。
	u, err := db.SessionUser(ctx, store.HashSessionID("raw-session-id"))
	if err != nil {
		t.Fatalf("读会话: %v", err)
	}
	if u == nil {
		t.Fatal("会话行没了，与触发器拦住删除的前提矛盾")
	}
}
