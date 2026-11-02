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

// TestRepoGrantsLifecycle 覆盖仓库级显式授权的完整生命周期：
// 授权 → 列出 → 重复授权被拒 → 取消 → 再取消返 404。
//
// 重复授权必须被明确拒绝：静默成功会让审计里多出一条假的"刚刚授权"。
func TestRepoGrantsLifecycle(t *testing.T) {
	app, db := newRepoTestApp(t)
	ctx := context.Background()
	project, err := db.CreateProject(ctx, "team-a", "项目 A", "", domain.VisibilityPrivate, nil)
	if err != nil {
		t.Fatalf("创建项目: %v", err)
	}
	if _, err := db.CreateRepository(ctx, project.ID, "app", domain.VisibilityPrivate, ""); err != nil {
		t.Fatalf("建仓: %v", err)
	}
	if _, err := db.CreateUser(ctx, "bob", "hash", "local", "Bob", false); err != nil {
		t.Fatalf("创建用户: %v", err)
	}

	add := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/team-a/repositories/app/grants",
			strings.NewReader(`{"username":"bob"}`))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(withPrincipal(req.Context(), adminPrincipal()))
		req = withRouteParams(req, map[string]string{"name": "team-a", "repo": "app"})
		rec := httptest.NewRecorder()
		app.handleAddRepoGrant(rec, req)
		return rec
	}

	if rec := add(); rec.Code != http.StatusOK {
		t.Fatalf("授权状态码 = %d，期望 200（body：%s）", rec.Code, rec.Body.String())
	}

	// 列表里应当看得到这一条。
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/team-a/repositories/app/grants", nil)
	req = req.WithContext(withPrincipal(req.Context(), adminPrincipal()))
	req = withRouteParams(req, map[string]string{"name": "team-a", "repo": "app"})
	rec := httptest.NewRecorder()
	app.handleListRepoGrants(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("列表状态码 = %d，期望 200（body：%s）", rec.Code, rec.Body.String())
	}
	var payload struct {
		Data struct {
			Grants []domain.RepositoryGrant `json:"grants"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if len(payload.Data.Grants) != 1 || payload.Data.Grants[0].Username != "bob" {
		t.Fatalf("授权列表 = %+v，期望恰好一条 bob", payload.Data.Grants)
	}

	// 重复授权 → 409。
	if rec := add(); rec.Code != http.StatusConflict {
		t.Fatalf("重复授权状态码 = %d，期望 409（body：%s）", rec.Code, rec.Body.String())
	}

	remove := func(userID string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodDelete,
			"/api/v1/projects/team-a/repositories/app/grants/"+userID, nil)
		req = req.WithContext(withPrincipal(req.Context(), adminPrincipal()))
		req = withRouteParams(req, map[string]string{"name": "team-a", "repo": "app", "userId": userID})
		rec := httptest.NewRecorder()
		app.handleRemoveRepoGrant(rec, req)
		return rec
	}

	if rec := remove("1"); rec.Code != http.StatusOK {
		t.Fatalf("取消授权状态码 = %d，期望 200（body：%s）", rec.Code, rec.Body.String())
	}
	// 再取消一次 → 404（没有这条授权，不当成功）。
	if rec := remove("1"); rec.Code != http.StatusNotFound {
		t.Fatalf("重复取消状态码 = %d，期望 404（body：%s）", rec.Code, rec.Body.String())
	}
}

// TestRepoGrantsRejectNonAdmin 覆盖"非项目管理员不能改授权"。
func TestRepoGrantsRejectNonAdmin(t *testing.T) {
	app, db := newRepoTestApp(t)
	ctx := context.Background()
	project, err := db.CreateProject(ctx, "team-a", "项目 A", "", domain.VisibilityPrivate, nil)
	if err != nil {
		t.Fatalf("创建项目: %v", err)
	}
	if _, err := db.CreateRepository(ctx, project.ID, "app", domain.VisibilityPrivate, ""); err != nil {
		t.Fatalf("建仓: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/team-a/repositories/app/grants",
		strings.NewReader(`{"username":"bob"}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(withPrincipal(req.Context(), service.Principal{
		User: &domain.User{ID: 99, Username: "bob"},
	}))
	req = withRouteParams(req, map[string]string{"name": "team-a", "repo": "app"})
	rec := httptest.NewRecorder()
	app.handleAddRepoGrant(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("状态码 = %d，期望 403（body：%s）", rec.Code, rec.Body.String())
	}
}
