package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/lau6code/lau6-cr/apps/api/internal/config"
	"github.com/lau6code/lau6-cr/apps/api/internal/crypto"
	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
	"github.com/lau6code/lau6-cr/apps/api/internal/service"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// newRepoTestApp 造一个能处理仓库接口的 App（DB + 权限引擎 + 签发器）。
func newRepoTestApp(t *testing.T) (*App, *store.DB) {
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

	dir := t.TempDir()
	ks, err := crypto.NewKeystore(crypto.Options{PrivatePath: filepath.Join(dir, "priv.pem"), JWKSPath: filepath.Join(dir, "jwks.json")})
	if err != nil {
		t.Fatalf("构造密钥: %v", err)
	}
	secrets := crypto.NewUserSecretStore(crypto.UserSecretOptions{Dir: filepath.Join(dir, "user-secrets")})
	projectSecrets := crypto.NewProjectSecretStore(crypto.ProjectSecretOptions{Dir: filepath.Join(dir, "project-secrets")})
	issuer, err := crypto.NewTokenIssuer(ks, secrets, projectSecrets, "lau6-cr", "lau6-cr", func() time.Duration { return 5 * time.Minute })
	if err != nil {
		t.Fatalf("构造签发器: %v", err)
	}

	logger, _ := newTestLogger()
	return &App{
		Logger:          logger,
		ProjectTokenSvc: service.NewProjectTokenService(db, service.ProjectTokenOptions{Secrets: projectSecrets}),
		Cfg: &config.Config{
			Token: config.TokenConfig{Audience: "lau6-cr"},
			Core:  config.CoreConfig{PublicURL: "http://10.10.32.2:5000"},
		},
		DB: db,
		TokenSvc: service.NewTokenService(
			service.NewPermissionEngine(db, service.PermissionOptions{AnonymousReadEnabled: func() bool { return true }}), issuer, service.NewAuthService(db, service.AuthOptions{}),
			service.NewPersonalTokenService(db, service.PersonalTokenOptions{}),
			service.NewProjectTokenService(db, service.ProjectTokenOptions{Secrets: projectSecrets})),
	}, db
}

// adminPrincipal 造一个系统管理员主体（直接塞进 context，不走 session）。
func adminPrincipal() service.Principal {
	return service.Principal{
		User: &domain.User{ID: 1, Username: "admin", IsSystemAdmin: true},
	}
}

// withRouteParams 给请求补上 chi 的 URL 参数（直接调 handler 时绕过路由树，
// URLParam 取不到值）。
func withRouteParams(req *http.Request, params map[string]string) *http.Request {
	rctx := chi.NewRouteContext()
	for k, v := range params {
		rctx.URLParams.Add(k, v)
	}
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

// TestCreateRepoFollowsProjectByDefault 覆盖"预建的仓库默认跟随项目"。
//
// 这是"公开项目里的镜像匿名可拉"的前提：预建时不该默认写死 private，
// 否则公开项目里的镜像还得挨个改。
func TestCreateRepoFollowsProjectByDefault(t *testing.T) {
	app, db := newRepoTestApp(t)
	ctx := context.Background()
	if _, err := db.CreateProject(ctx, "pub-team", "公开项目", "", domain.VisibilityPublic, nil); err != nil {
		t.Fatalf("创建项目: %v", err)
	}

	body := `{"name":"app","description":"","visibility":""}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/pub-team/repositories",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(withPrincipal(req.Context(), adminPrincipal()))
	req = withRouteParams(req, map[string]string{"name": "pub-team"})
	rec := httptest.NewRecorder()
	app.handleCreateRepo(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（body：%s）", rec.Code, rec.Body.String())
	}
	var payload struct {
		Data struct {
			Repository domain.Repository `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	repo := payload.Data.Repository
	if repo.VisibilitySource != domain.VisibilitySourceInherit {
		t.Fatalf("可见性来源 = %q，期望 inherit", repo.VisibilitySource)
	}
	if repo.Visibility != domain.VisibilityPublic {
		t.Fatalf("生效可见性 = %q，期望 public（跟随公开项目）", repo.Visibility)
	}
}

// TestUpdateRepoVisibilityRejectsBadValue 覆盖"非法可见性 → 400"。
func TestUpdateRepoVisibilityRejectsBadValue(t *testing.T) {
	app, db := newRepoTestApp(t)
	ctx := context.Background()
	project, err := db.CreateProject(ctx, "team-a", "项目 A", "", domain.VisibilityPrivate, nil)
	if err != nil {
		t.Fatalf("创建项目: %v", err)
	}
	if _, err := db.CreateRepository(ctx, project.ID, "app", domain.VisibilityPrivate, ""); err != nil {
		t.Fatalf("建仓: %v", err)
	}

	body := `{"visibility":"whatever"}`
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/projects/team-a/repositories/app",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(withPrincipal(req.Context(), adminPrincipal()))
	req = withRouteParams(req, map[string]string{"name": "team-a", "repo": "app"})
	rec := httptest.NewRecorder()
	app.handleUpdateRepo(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400（body：%s）", rec.Code, rec.Body.String())
	}
}
