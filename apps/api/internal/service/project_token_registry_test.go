package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lau6code/lau6-cr/apps/api/internal/crypto"
	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// seedRegistryProjectToken 建项目 + 仓库 + 项目令牌，返回原始令牌与项目 ID。
func seedRegistryProjectToken(t *testing.T, db *store.DB, projectName string, role domain.Role) (string, int64) {
	t.Helper()
	ctx := context.Background()
	p, err := db.CreateProject(ctx, projectName, projectName, "", domain.VisibilityPrivate, nil)
	if err != nil {
		t.Fatalf("创建项目: %v", err)
	}
	if _, err := db.CreateRepository(ctx, p.ID, "app", domain.VisibilityPrivate, ""); err != nil {
		t.Fatalf("创建仓库: %v", err)
	}
	raw, hash, prefix, err := crypto.GenerateProjectToken()
	if err != nil {
		t.Fatalf("生成项目令牌: %v", err)
	}
	tok := &domain.ProjectToken{
		ProjectID:   p.ID,
		Name:        "CI",
		Role:        role,
		TokenHash:   hash,
		TokenPrefix: prefix,
	}
	if err := db.CreateProjectToken(ctx, tok); err != nil {
		t.Fatalf("落库项目令牌: %v", err)
	}
	return raw, p.ID
}

// TestRegistryAuthorizeWithProjectToken 覆盖"项目令牌直接当 Bearer 令牌"。
func TestRegistryAuthorizeWithProjectToken(t *testing.T) {
	svc, _, db := testRegistry(t, &fakeLogger{})
	raw, _ := seedRegistryProjectToken(t, db, "team-a", domain.RoleDeveloper)

	r := httptest.NewRequest(http.MethodGet, "/v2/team-a/app/manifests/latest", nil)
	r.Header.Set("Authorization", "Bearer "+raw)
	claims, ok := svc.authorize(httptest.NewRecorder(), r, "team-a/app", ActionPull)
	if !ok {
		t.Fatal("项目令牌应被放行")
	}
	if claims == nil || len(claims.Access) != 1 || claims.Access[0].Name != "team-a/app" {
		t.Fatalf("载荷 = %+v", claims)
	}
}

// TestRegistryAuthorizeWithProjectTokenCrossProject 覆盖"项目令牌跨项目被拒"。
func TestRegistryAuthorizeWithProjectTokenCrossProject(t *testing.T) {
	svc, _, db := testRegistry(t, &fakeLogger{})
	raw, _ := seedRegistryProjectToken(t, db, "team-a", domain.RoleDeveloper)
	seedRegistryProjectToken(t, db, "team-b", domain.RoleDeveloper)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v2/team-b/app/manifests/latest", nil)
	r.Header.Set("Authorization", "Bearer "+raw)
	if _, ok := svc.authorize(w, r, "team-b/app", ActionPull); ok {
		t.Fatal("跨项目不该被放行")
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("状态码 = %d，期望 403", w.Code)
	}
}

// TestRegistryAuthorizeWithProjectTokenReaderCannotPush 覆盖"reader 不能推"。
func TestRegistryAuthorizeWithProjectTokenReaderCannotPush(t *testing.T) {
	svc, _, db := testRegistry(t, &fakeLogger{})
	raw, _ := seedRegistryProjectToken(t, db, "team-a", domain.RoleReader)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPut, "/v2/team-a/app/manifests/v1", nil)
	r.Header.Set("Authorization", "Bearer "+raw)
	if _, ok := svc.authorize(w, r, "team-a/app", ActionPush); ok {
		t.Fatal("reader 不该能 push")
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("状态码 = %d，期望 403", w.Code)
	}
}

// TestRegistryAuthorizeWithInvalidProjectToken 覆盖"无效项目令牌被拒"。
func TestRegistryAuthorizeWithInvalidProjectToken(t *testing.T) {
	svc, _, _ := testRegistry(t, &fakeLogger{})

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v2/team-a/app/manifests/latest", nil)
	r.Header.Set("Authorization", "Bearer lau6_p_不是一枚真令牌")
	if _, ok := svc.authorize(w, r, "team-a/app", ActionPull); ok {
		t.Fatal("无效项目令牌不该被放行")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d，期望 401", w.Code)
	}
}

// TestRegistryAuthorizeWithLegacyPrefixSaysWhy 覆盖"旧前缀当 Bearer 令牌"。
//
// ⚠️ 必须**说清是前缀变更**：笼统的"令牌无效"会让用户以为令牌被吊销了，
//
//	去查权限、翻审计，越查越找不到原因（02-auth.md §2.6.4）。
func TestRegistryAuthorizeWithLegacyPrefixSaysWhy(t *testing.T) {
	svc, _, _ := testRegistry(t, &fakeLogger{})

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v2/team-a/app/manifests/latest", nil)
	r.Header.Set("Authorization", "Bearer cr_pat_旧令牌")
	if _, ok := svc.authorize(w, r, "team-a/app", ActionPull); ok {
		t.Fatal("旧前缀令牌不该被放行")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d，期望 401", w.Code)
	}
	if !strings.Contains(w.Body.String(), "前缀已变更") {
		t.Fatalf("响应体没说清是前缀变更：%s", w.Body.String())
	}
}

// TestCatalogResolvesProjectPrincipal 覆盖"catalog 能解析项目令牌主体"。
//
// 主体解析原本是按 sub 反查**用户**的；
// sub = project:<项目名> 查不到用户，会被当成"账号不存在"降级成匿名，
// 项目令牌在目录页就只看得见公开仓库。
func TestCatalogResolvesProjectPrincipal(t *testing.T) {
	svc, _, db := testRegistry(t, &fakeLogger{})
	_, projectID := seedRegistryProjectToken(t, db, "team-a", domain.RoleDeveloper)

	// 先换一枚 JWT（模拟 docker login 拿到的那枚）。
	issuer := svc.issuer
	jwtTok, err := issuer.IssueProject(context.Background(), projectID, "project:team-a", nil)
	if err != nil {
		t.Fatalf("签发项目令牌 JWT: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, "/v2/_catalog", nil)
	r.Header.Set("Authorization", "Bearer "+jwtTok)
	p := svc.catalogPrincipal(context.Background(), r)
	if p.Project == nil {
		t.Fatalf("主体 = %+v，应是项目主体", p)
	}
	if p.Project.Role != domain.RoleDeveloper {
		t.Fatalf("角色 = %q", p.Project.Role)
	}
	if p.IsAnonymous() {
		t.Fatal("项目主体不该被当成匿名")
	}
}

// TestVersionCheckAcceptsProjectToken 覆盖"项目令牌能探活"。
//
// CI 脚本常拿长期凭据 ping /v2/——项目令牌和 PAT 一样是长期凭据，
// 不该在版本检查这一步就被拒。
func TestVersionCheckAcceptsProjectToken(t *testing.T) {
	svc, _, db := testRegistry(t, &fakeLogger{})
	raw, _ := seedRegistryProjectToken(t, db, "team-a", domain.RoleDeveloper)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v2/", nil)
	r.Header.Set("Authorization", "Bearer "+raw)
	svc.versionCheck(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", w.Code)
	}
}
