package server

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/cache"
	"github.com/lau6code/lau6-cr/apps/api/internal/config"
	"github.com/lau6code/lau6-cr/apps/api/internal/crypto"
	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
	"github.com/lau6code/lau6-cr/apps/api/internal/service"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// newTokenTestApp 造一个能处理 /token 的 App（DB + 权限引擎 + 签发器）。
func newTokenTestApp(t *testing.T) (*App, *store.DB) {
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
	app := &App{
		Logger:      logger,
		RateLimiter: newTestRateLimiter(),
		Cfg: &config.Config{
			Token: config.TokenConfig{Audience: "lau6-cr"},
			Core:  config.CoreConfig{PublicURL: "http://10.10.32.2:5000"},
		},
		DB: db,
		TokenSvc: service.NewTokenService(
			service.NewPermissionEngine(db, service.PermissionOptions{AnonymousReadEnabled: func() bool { return true }}), issuer, service.NewAuthService(db, service.AuthOptions{}),
			service.NewPersonalTokenService(db, service.PersonalTokenOptions{}),
			service.NewProjectTokenService(db, service.ProjectTokenOptions{Secrets: projectSecrets})),
	}
	return app, db
}

// newTestRateLimiter 造一个限流器（额度足够大，不干扰其它断言）。
//
// ⚠️ 必须接线：限流器没接线时 Allow 会**报错**（500），
// 那正是生产里的真实行为——测试不能"绕过"它。
func newTestRateLimiter() *service.RateLimiter {
	c := cache.NewLocal(100)
	return service.NewRateLimiter(
		func() cache.Limiter { return cache.LimiterOf(c) },
		func(service.RateLimitScope) int { return 10000 },
		nil,
	)
}

// TestHandleTokenRejectsAnonymousWriteScope 覆盖 handler 层：
// 匿名主体申请 push/delete → **401 + Basic 挑战**。
//
// 这是止住"客户端不断重试"的关键：签一枚降级 token 会让客户端
// 拿着它继续推 → registry 回 401 + 挑战 → 再来换 token → 死循环。
func TestHandleTokenRejectsAnonymousWriteScope(t *testing.T) {
	app, _ := newTokenTestApp(t)

	for _, scope := range []string{
		"repository:team-a/app:push",
		"repository:team-a/app:pull,push",
		"repository:team-a/app:delete",
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet,
			"/token?service=lau6-cr&scope="+urlEscape(scope), nil)
		app.handleToken(rec, req)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("scope=%q 状态码 = %d，期望 401", scope, rec.Code)
		}
		if got := rec.Header().Get("WWW-Authenticate"); got != `Basic realm="lau6-cr"` {
			t.Fatalf("scope=%q WWW-Authenticate = %q，期望 Basic 挑战", scope, got)
		}
		// ⚠️ 响应体不能泄露仓库/项目名——匿名是未认证攻击面。
		if strings.Contains(rec.Body.String(), "team-a") {
			t.Fatalf("scope=%q 错误响应泄露了仓库名：%s", scope, rec.Body.String())
		}
	}
}

// TestHandleTokenAnonymousPullOnPublicRepo 覆盖 handler 层：
// 公开仓库的匿名**只读**请求 → 正常签发 token（含 pull）。
//
// 这是"公开"的字面含义：写权限被拒**不能**连累读权限。
func TestHandleTokenAnonymousPullOnPublicRepo(t *testing.T) {
	app, db := newTokenTestApp(t)
	ctx := context.Background()

	project, err := db.CreateProject(ctx, "team-a", "team-a", "", domain.VisibilityPublic, nil)
	if err != nil {
		t.Fatalf("创建项目: %v", err)
	}
	if _, err := db.CreateRepository(ctx, project.ID, "app", domain.VisibilityPublic, ""); err != nil {
		t.Fatalf("创建仓库: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/token?service=lau6-cr&scope=repository:team-a/app:pull", nil)
	app.handleToken(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("匿名拉取公开仓库的状态码 = %d，期望 200（body：%s）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"token"`) {
		t.Fatalf("响应顶层没有 token 字段：%s", rec.Body.String())
	}
}

// urlEscape 做最小的 URL 编码（scope 里有冒号与逗号）。
func urlEscape(s string) string {
	r := strings.NewReplacer(":", "%3A", ",", "%2C", "/", "%2F")
	return r.Replace(s)
}

// basicAuth 拼出 Basic Auth 头。
func basicAuth(user, password string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))
}

// TestHandleTokenLegacyPrefixSaysWhy 覆盖 handler 层：
// 旧前缀（cr_pat_）的凭据 → **401 + 明确原因**。
//
// ⚠️ 这条不能退化成笼统的 "authentication required"：
//
//	用户会以为令牌被吊销了，去查权限、翻审计，越查越找不到原因
//	（02-auth.md §2.6.4）。
func TestHandleTokenLegacyPrefixSaysWhy(t *testing.T) {
	app, _ := newTokenTestApp(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/token?service=lau6-cr&scope="+urlEscape("repository:team-a/app:pull"), nil)
	req.Header.Set("Authorization", basicAuth("admin", "cr_pat_旧令牌"))
	app.handleToken(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d，期望 401（body：%s）", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != `Basic realm="lau6-cr"` {
		t.Fatalf("WWW-Authenticate = %q，期望 Basic 挑战", got)
	}
	if !strings.Contains(rec.Body.String(), "前缀已变更") {
		t.Fatalf("响应体没说清是前缀变更：%s", rec.Body.String())
	}
}

// TestHandleTokenInvalidProjectTokenIsUnauthorized 覆盖 handler 层：
// 无效 / 已吊销的项目令牌 → **401 + Basic 挑战**，不是 500。
//
// ⚠️ 凭据无效是**终态**：给 500 会让客户端当成"服务端坏了"反复重试，
//
//	拿 401 + 挑战它才知道"换一枚凭据"（02-auth.md §5.3）。
func TestHandleTokenInvalidProjectTokenIsUnauthorized(t *testing.T) {
	app, _ := newTokenTestApp(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/token?service=lau6-cr&scope="+urlEscape("repository:team-a/app:pull"), nil)
	req.Header.Set("Authorization", basicAuth("admin", crypto.ProjectTokenPrefix+"并不存在的令牌"))
	app.handleToken(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d，期望 401（body：%s）", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != `Basic realm="lau6-cr"` {
		t.Fatalf("WWW-Authenticate = %q，期望 Basic 挑战", got)
	}
}
