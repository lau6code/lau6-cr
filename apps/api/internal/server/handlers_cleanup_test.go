package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// adminSessionReq 造一个"系统管理员已登录"的请求。
//
// 走真实的 session Cookie，而不是往 context 里塞主体——
// 中间件链上还有会话解析那一环，绕过去测的就不是真实链路了。
func adminSessionReq(t *testing.T, app *App, path string) *http.Request {
	t.Helper()
	// 测试用 App 的配置里没设 cookie 名，这里显式给一个。
	app.Cfg.Session.CookieName = "lau6cr_session"
	ctx := context.Background()
	user, err := app.DB.CreateUser(ctx, "root", "hash", "local", "Root", true)
	if err != nil {
		t.Fatalf("建系统管理员: %v", err)
	}
	raw := "cleanup-admin-session"
	if err := app.DB.CreateSession(ctx, store.HashSessionID(raw), user.ID,
		time.Now().Add(time.Hour).Unix(), "1.2.3.4", "test"); err != nil {
		t.Fatalf("建会话: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(&http.Cookie{Name: app.Cfg.Session.CookieName, Value: raw})
	return req
}

// TestListRetentionPolicies 锁定「管理端能列出全局默认 + 各项目级覆盖」。
//
// 项目级策略是**逐项目覆盖**全局默认的（03-registry.md §5.2.3），
// 没有这张列表，管理员看不出全局默认对哪些项目根本不生效。
func TestListRetentionPolicies(t *testing.T) {
	app, db := newProjectTestApp(t)
	ctx := context.Background()

	if _, err := db.CreateProject(ctx, "team-a", "团队 A", "", domain.VisibilityPublic, nil); err != nil {
		t.Fatalf("建项目: %v", err)
	}
	project, err := db.ProjectByName(ctx, "team-a")
	if err != nil {
		t.Fatalf("读项目: %v", err)
	}

	global := &domain.RetentionPolicy{
		ScopeKey: store.GlobalRetentionScope, Enabled: true, Algorithm: "or", TagSelector: "**",
		Rules: []domain.RetentionRule{{Template: domain.RuleKeepLatestK, Params: map[string]int{"k": 1}}},
	}
	if err := db.UpsertRetentionPolicy(ctx, global); err != nil {
		t.Fatalf("写全局策略: %v", err)
	}
	projectPolicy := &domain.RetentionPolicy{
		ScopeKey: store.ProjectRetentionScope(project.ID), ProjectID: &project.ID,
		Enabled: true, Algorithm: "or", TagSelector: "**",
		Rules: []domain.RetentionRule{{Template: domain.RuleKeepLatestK, Params: map[string]int{"k": 5}}},
	}
	if err := db.UpsertRetentionPolicy(ctx, projectPolicy); err != nil {
		t.Fatalf("写项目级策略: %v", err)
	}

	rec := httptest.NewRecorder()
	app.Routes().ServeHTTP(rec, adminSessionReq(t, app, "/api/v1/admin/cleanup/policies"))

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（body：%s）", rec.Code, rec.Body.String())
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			Policies []domain.RetentionPolicy `json:"policies"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("解析响应: %v（body：%s）", err, rec.Body.String())
	}
	if len(env.Data.Policies) != 2 {
		t.Fatalf("策略条数 = %d，期望 2（body：%s）", len(env.Data.Policies), rec.Body.String())
	}

	var globalFound, projectFound bool
	for _, p := range env.Data.Policies {
		if p.ProjectID == nil {
			globalFound = true
			continue
		}
		projectFound = true
		// 项目级那条必须带项目名——页面上要靠它显示"是谁覆盖了全局默认"。
		if p.ProjectName != "team-a" {
			t.Errorf("项目级策略的 project_name = %q，期望 team-a", p.ProjectName)
		}
	}
	if !globalFound {
		t.Errorf("返回的策略里没有全局默认：%s", rec.Body.String())
	}
	if !projectFound {
		t.Errorf("返回的策略里没有项目级策略：%s", rec.Body.String())
	}
}

// TestListRetentionPoliciesRequiresAdmin 锁定「这份列表只有系统管理员能读」。
//
// 里面是全局默认 + 各项目的覆盖情况，属于系统级配置。
func TestListRetentionPoliciesRequiresAdmin(t *testing.T) {
	app, _ := newProjectTestApp(t)

	rec := httptest.NewRecorder()
	app.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/cleanup/policies", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("匿名访问的状态码 = %d，期望 401（body：%s）", rec.Code, rec.Body.String())
	}
}
