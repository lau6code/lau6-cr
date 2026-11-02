package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
	"github.com/lau6code/lau6-cr/apps/api/internal/service"
)

// createProjectReq 造一个"创建项目"的请求（系统管理员主体）。
func createProjectReq(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(withPrincipal(req.Context(), adminPrincipal()))
	return req
}

// TestCreateProjectRejectsBadVisibility 覆盖"非法可见性 → 400，不静默改成 private"。
//
// 静默改写是假成功：调用方以为设成了公开，实际是私有。
func TestCreateProjectRejectsBadVisibility(t *testing.T) {
	app, db := newRepoTestApp(t)
	ctx := context.Background()

	for _, vis := range []string{"pbulic", "PUBLIC", ""} {
		req := createProjectReq(`{"name":"team-a","display_name":"","description":"","visibility":"` + vis + `"}`)
		rec := httptest.NewRecorder()
		app.handleCreateProject(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("visibility=%q 状态码 = %d，期望 400（body：%s）", vis, rec.Code, rec.Body.String())
		}
		// 不能留下项目行：请求被拒了，库里也不该有。
		if _, err := db.ProjectByName(ctx, "team-a"); err == nil {
			t.Fatalf("visibility=%q 被拒后不该创建项目", vis)
		}
	}
}

// TestCreateProjectMakesCreatorAdmin 覆盖"创建者自动成为项目管理员"。
func TestCreateProjectMakesCreatorAdmin(t *testing.T) {
	app, db := newRepoTestApp(t)
	ctx := context.Background()

	req := createProjectReq(`{"name":"team-a","display_name":"团队 A","description":"","visibility":"private"}`)
	rec := httptest.NewRecorder()
	app.handleCreateProject(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（body：%s）", rec.Code, rec.Body.String())
	}

	project, err := db.ProjectByName(ctx, "team-a")
	if err != nil {
		t.Fatalf("读取项目: %v", err)
	}
	m, err := db.Membership(ctx, project.ID, adminPrincipal().User.ID)
	if err != nil || m == nil {
		t.Fatalf("创建者应是项目成员：m=%v err=%v", m, err)
	}
	if m.Role != domain.RoleAdmin {
		t.Fatalf("创建者角色 = %q，期望 admin", m.Role)
	}

	// 审计必须落库。
	logs, err := db.ListAudit(ctx, "", "project.created", 10, 0)
	if err != nil {
		t.Fatalf("查询审计: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("审计条数 = %d，期望 1", len(logs))
	}
}

// TestCreateProjectAuditFailureRollsBack 覆盖"审计写失败 → 整体回滚"。
//
// 审计写不进去时，项目行也必须没建出来——
// 否则会留下一个"建好了但审计里查不到"的孤儿项目。
func TestCreateProjectAuditFailureRollsBack(t *testing.T) {
	app, db := newRepoTestApp(t)
	ctx := context.Background()
	// 让审计写失败：把审计表删掉。
	if _, err := db.Exec(ctx, `DROP TABLE audit_logs`); err != nil {
		t.Fatalf("删除审计表: %v", err)
	}

	req := createProjectReq(`{"name":"team-a","display_name":"","description":"","visibility":"private"}`)
	rec := httptest.NewRecorder()
	app.handleCreateProject(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500（body：%s）", rec.Code, rec.Body.String())
	}
	if _, err := db.ProjectByName(ctx, "team-a"); err == nil {
		t.Fatalf("审计写失败时项目不该留下")
	}
}

// TestAddMemberAuditFailureReturns500 覆盖"管理写操作的审计写失败 → 5xx"。
func TestAddMemberAuditFailureReturns500(t *testing.T) {
	app, db := newRepoTestApp(t)
	ctx := context.Background()

	if _, err := db.CreateProject(ctx, "team-a", "团队 A", "", domain.VisibilityPrivate, nil); err != nil {
		t.Fatalf("创建项目: %v", err)
	}
	if _, err := db.CreateUser(ctx, "bob", "", "local", "Bob", false); err != nil {
		t.Fatalf("创建用户: %v", err)
	}
	if _, err := db.Exec(ctx, `DROP TABLE audit_logs`); err != nil {
		t.Fatalf("删除审计表: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/team-a/members",
		strings.NewReader(`{"username":"bob","role":"developer"}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(withPrincipal(req.Context(), adminPrincipal()))
	req = withRouteParams(req, map[string]string{"name": "team-a"})
	rec := httptest.NewRecorder()
	app.handleAddMember(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500（body：%s）", rec.Code, rec.Body.String())
	}
	// 审计写失败必须让调用方看得见——响应里要写明是审计失败。
	var env struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if !strings.Contains(env.Message, "审计") {
		t.Fatalf("错误消息 = %q，应当点明是审计写入失败", env.Message)
	}
}

// TestOverviewFailsWhenRepoMetaUnreadable 覆盖"读缓存失败不许伪装成空数据"。
//
// 读失败与"仓库还没被刷新过"在返回结构里长得一样（标签数 0、大小 0），
// 调用方分不出来 → 整个列表必须失败。
func TestOverviewFailsWhenRepoMetaUnreadable(t *testing.T) {
	app, db := newRepoTestApp(t)
	ctx := context.Background()
	app.Perm = service.NewPermissionEngine(db, service.PermissionOptions{AnonymousReadEnabled: func() bool { return true }})

	if _, err := db.CreateProject(ctx, "pub-team", "公开项目", "", domain.VisibilityPublic, nil); err != nil {
		t.Fatalf("创建项目: %v", err)
	}
	if _, err := db.CreateRepository(ctx, 1, "app", domain.VisibilityPublic, ""); err != nil {
		t.Fatalf("创建仓库: %v", err)
	}
	if _, err := db.Exec(ctx, `DROP TABLE repo_meta`); err != nil {
		t.Fatalf("删除缓存表: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/registry/overview", nil)
	req = req.WithContext(withPrincipal(req.Context(), adminPrincipal()))
	rec := httptest.NewRecorder()
	app.handleOverview(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500（body：%s）", rec.Code, rec.Body.String())
	}
}
