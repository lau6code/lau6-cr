package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/config"
	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
	"github.com/lau6code/lau6-cr/apps/api/internal/service"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// newProjectTestApp 造一个能跑完整路由链的 App。
//
// 完整链路包含 requireSetupDone 中间件——它会问 SetupSvc"实例初始化了吗"，
// 所以这里必须把 SetupSvc 装上并把实例标记成已初始化，
// 否则测的就不是"详情接口的权限"，而是"没初始化时的 503"。
func newProjectTestApp(t *testing.T) (*App, *store.DB) {
	t.Helper()
	app, db := newRepoTestApp(t)
	settings, err := service.NewSettingsManager(db, config.Default(), filepath.Join(t.TempDir(), "application.yaml"))
	if err != nil {
		t.Fatalf("构造设置管理器: %v", err)
	}
	app.SetupSvc = service.NewSetupService(service.SetupDeps{DB: db, Settings: settings})
	if err := db.UpsertSetting(context.Background(), "system.initialized", true, "test"); err != nil {
		t.Fatalf("标记实例已初始化: %v", err)
	}
	return app, db
}

// TestGetProjectRequiresLogin 锁定「项目详情必须登录」。
//
// 公开项目的详情会连**成员名单**一起返回，
// 匿名用户据此就能枚举出项目成员（用户名 / 昵称 / 角色），
// 与 /projects/{name}/members（项目管理员）口径不一致。
// 契约（04-api-data.md §3）写的层级本来就是 session。
func TestGetProjectRequiresLogin(t *testing.T) {
	app, db := newProjectTestApp(t)
	ctx := context.Background()

	if _, err := db.CreateProject(ctx, "team-a", "团队 A", "", domain.VisibilityPublic, nil); err != nil {
		t.Fatalf("建项目: %v", err)
	}
	// 项目里有个成员：匿名一旦能读到详情，就等于能枚举出他。
	user, err := db.CreateUser(ctx, "alice", "hash", "local", "Alice", false)
	if err != nil {
		t.Fatalf("建用户: %v", err)
	}
	project, err := db.ProjectByName(ctx, "team-a")
	if err != nil {
		t.Fatalf("读项目: %v", err)
	}
	if err := db.AddMember(ctx, project.ID, user.ID, domain.RoleDeveloper, nil); err != nil {
		t.Fatalf("加成员: %v", err)
	}

	rec := httptest.NewRecorder()
	app.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/projects/team-a", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("匿名访问项目详情的状态码 = %d，期望 401（body：%s）", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); strings.Contains(body, "alice") {
		t.Fatalf("匿名响应里泄露了成员信息：%s", body)
	}
}

// TestGetProjectWithSessionOK 锁定「登录后照常能读详情」。
//
// 收紧权限不能把正常用户挡在门外——项目详情页是登录用户的主入口。
func TestGetProjectWithSessionOK(t *testing.T) {
	app, db := newProjectTestApp(t)
	ctx := context.Background()
	app.Cfg.Session.CookieName = "lau6cr_session"

	if _, err := db.CreateProject(ctx, "team-a", "团队 A", "", domain.VisibilityPublic, nil); err != nil {
		t.Fatalf("建项目: %v", err)
	}
	user, err := db.CreateUser(ctx, "alice", "hash", "local", "Alice", false)
	if err != nil {
		t.Fatalf("建用户: %v", err)
	}

	raw := "test-session-raw"
	if err := db.CreateSession(ctx, store.HashSessionID(raw), user.ID, time.Now().Add(time.Hour).Unix(), "1.2.3.4", "test"); err != nil {
		t.Fatalf("建会话: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/team-a", nil)
	req.AddCookie(&http.Cookie{Name: "lau6cr_session", Value: raw})
	rec := httptest.NewRecorder()
	app.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("登录后访问项目详情的状态码 = %d，期望 200（body：%s）", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, "team-a") {
		t.Fatalf("响应里没有项目名：%s", body)
	}
}
