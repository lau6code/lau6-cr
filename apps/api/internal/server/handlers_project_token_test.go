package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// projectTokenSession 造一个项目管理员的会话 Cookie 值。
//
// 返回 (cookieValue, projectID)。
func projectTokenSession(t *testing.T, app *App, db *store.DB, projectName, username string) (string, int64) {
	t.Helper()
	ctx := context.Background()
	project, err := db.CreateProject(ctx, projectName, projectName, "", domain.VisibilityPrivate, nil)
	if err != nil {
		t.Fatalf("建项目: %v", err)
	}
	user, err := db.CreateUser(ctx, username, "hash", "local", username, false)
	if err != nil {
		t.Fatalf("建用户: %v", err)
	}
	if err := db.AddMember(ctx, project.ID, user.ID, domain.RoleAdmin, nil); err != nil {
		t.Fatalf("加成员: %v", err)
	}
	raw := "session-" + username
	if err := db.CreateSession(ctx, store.HashSessionID(raw), user.ID,
		time.Now().Add(time.Hour).Unix(), "1.2.3.4", "test"); err != nil {
		t.Fatalf("建会话: %v", err)
	}
	app.Cfg.Session.CookieName = "lau6cr_session"
	return raw, project.ID
}

// decodeEnvelope 解开业务信封。
func decodeEnvelope(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var env struct {
		Code    int            `json:"code"`
		Message string         `json:"message"`
		Data    map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("解析响应: %v（body：%s）", err, body)
	}
	if env.Data == nil {
		env.Data = map[string]any{}
	}
	return env.Data
}

// TestProjectTokenHandlers 覆盖项目令牌的完整生命周期。
func TestProjectTokenHandlers(t *testing.T) {
	app, db := newProjectTestApp(t)
	cookie, _ := projectTokenSession(t, app, db, "team-a", "alice")

	// 创建
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/team-a/tokens",
		strings.NewReader(`{"name":"CI 推送","role":"developer"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "lau6cr_session", Value: cookie})
	app.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("创建项目令牌的状态码 = %d，期望 200（body：%s）", rec.Code, rec.Body.String())
	}
	data := decodeEnvelope(t, rec.Body.Bytes())
	raw, _ := data["token"].(string)
	if !strings.HasPrefix(raw, "lau6_p_") {
		t.Fatalf("原始令牌 = %q，期望 lau6_p_ 前缀", raw)
	}
	id := int64(data["id"].(float64))

	// 列表：只有前缀，没有令牌本体
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/projects/team-a/tokens", nil)
	req.AddCookie(&http.Cookie{Name: "lau6cr_session", Value: cookie})
	app.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("列项目令牌的状态码 = %d，期望 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, raw) {
		t.Fatalf("列表里泄露了令牌本体：%s", body)
	}
	if !strings.Contains(body, "lau6_p_") {
		t.Fatalf("列表里没有展示前缀：%s", body)
	}

	// 改期
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPatch,
		"/api/v1/projects/team-a/tokens/"+itoa(id), strings.NewReader(`{"expires_at": 1799000000}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "lau6cr_session", Value: cookie})
	app.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("改期的状态码 = %d，期望 200（body：%s）", rec.Code, rec.Body.String())
	}

	// 改角色 → 400（角色不可改）
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPatch,
		"/api/v1/projects/team-a/tokens/"+itoa(id), strings.NewReader(`{"role":"admin"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "lau6cr_session", Value: cookie})
	app.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("改角色的状态码 = %d，期望 400", rec.Code)
	}

	// 吊销
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/projects/team-a/tokens/"+itoa(id), nil)
	req.AddCookie(&http.Cookie{Name: "lau6cr_session", Value: cookie})
	app.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("吊销的状态码 = %d，期望 200（body：%s）", rec.Code, rec.Body.String())
	}

	// 再查 → 404
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/projects/team-a/tokens/"+itoa(id), nil)
	req.AddCookie(&http.Cookie{Name: "lau6cr_session", Value: cookie})
	app.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("重复吊销的状态码 = %d，期望 404", rec.Code)
	}
}

// TestProjectTokenHandlerRejectsInvalidRole 覆盖"非法角色被拒"。
func TestProjectTokenHandlerRejectsInvalidRole(t *testing.T) {
	app, db := newProjectTestApp(t)
	cookie, _ := projectTokenSession(t, app, db, "team-a", "alice")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/team-a/tokens",
		strings.NewReader(`{"name":"CI","role":"owner"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "lau6cr_session", Value: cookie})
	app.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法角色的状态码 = %d，期望 400（body：%s）", rec.Code, rec.Body.String())
	}
}

// TestProjectTokenHandlersRequireProjectAdmin 覆盖"非项目管理员被拒"。
func TestProjectTokenHandlersRequireProjectAdmin(t *testing.T) {
	app, db := newProjectTestApp(t)
	ctx := context.Background()
	project, err := db.CreateProject(ctx, "team-a", "team-a", "", domain.VisibilityPrivate, nil)
	if err != nil {
		t.Fatal(err)
	}
	user, err := db.CreateUser(ctx, "bob", "hash", "local", "bob", false)
	if err != nil {
		t.Fatal(err)
	}
	// 只是普通成员，不是 admin。
	if err := db.AddMember(ctx, project.ID, user.ID, domain.RoleDeveloper, nil); err != nil {
		t.Fatal(err)
	}
	raw := "session-bob"
	if err := db.CreateSession(ctx, store.HashSessionID(raw), user.ID,
		time.Now().Add(time.Hour).Unix(), "1.2.3.4", "test"); err != nil {
		t.Fatal(err)
	}
	app.Cfg.Session.CookieName = "lau6cr_session"

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/team-a/tokens", nil)
	req.AddCookie(&http.Cookie{Name: "lau6cr_session", Value: raw})
	app.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("普通成员访问的状态码 = %d，期望 403", rec.Code)
	}
}

// TestProjectTokenHandlersRequireLogin 覆盖"匿名被拒"。
//
// ⚠️ 私有项目对匿名返 **404**（不是 401）：项目管理员这一层的约定是
// "项目不存在或无权访问"一律 404，不泄露项目名存不存在
// （requireProjectAdmin 的既有行为）。
func TestProjectTokenHandlersRequireLogin(t *testing.T) {
	app, db := newProjectTestApp(t)
	if _, err := db.CreateProject(context.Background(), "team-a", "team-a", "",
		domain.VisibilityPrivate, nil); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	app.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/projects/team-a/tokens", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("匿名访问的状态码 = %d，期望 404（body：%s）", rec.Code, rec.Body.String())
	}
}

// TestProjectTokenAuditRecordsProjectSubject 覆盖"审计里的 actor 是项目"。
func TestProjectTokenAuditRecordsProjectSubject(t *testing.T) {
	app, db := newProjectTestApp(t)
	cookie, _ := projectTokenSession(t, app, db, "team-a", "alice")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/team-a/tokens",
		strings.NewReader(`{"name":"CI","role":"developer"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "lau6cr_session", Value: cookie})
	app.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("创建的状态码 = %d，期望 200", rec.Code)
	}

	var actor string
	err := db.QueryRow(context.Background(),
		`SELECT actor FROM audit_logs WHERE action = ? ORDER BY id DESC LIMIT 1`,
		"project_token.created").Scan(&actor)
	if err != nil {
		t.Fatalf("没找到 project_token.created 审计记录: %v", err)
	}
	if actor != "project:team-a" {
		t.Fatalf("审计 actor = %q，期望 project:team-a", actor)
	}
}
