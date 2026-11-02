package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/cache"
	"github.com/lau6code/lau6-cr/apps/api/internal/service"
)

// TestHandleTokenAnonymousRateLimited 覆盖"匿名 /token 超限 → 429"。
//
// `/token` 是唯一无认证可达的入口——不限速的话它就是个公开的令牌工厂。
func TestHandleTokenAnonymousRateLimited(t *testing.T) {
	app, _ := newTokenTestApp(t)
	// 把匿名桶的额度压到 1。
	c := cache.NewLocal(100)
	defer c.Close()
	app.RateLimiter = service.NewRateLimiter(
		func() cache.Limiter { return cache.LimiterOf(c) },
		func(sc service.RateLimitScope) int {
			if sc == service.RateLimitScopeToken {
				return 1
			}
			return 10000
		},
		nil,
	)

	first := httptest.NewRecorder()
	app.handleToken(first, httptest.NewRequest(http.MethodGet, "/token?scope=repository:team-a/app:pull", nil))
	if first.Code != http.StatusOK && first.Code != http.StatusUnauthorized {
		t.Fatalf("第一次请求状态码 = %d，期望 200 或 401（body：%s）", first.Code, first.Body.String())
	}

	second := httptest.NewRecorder()
	app.handleToken(second, httptest.NewRequest(http.MethodGet, "/token?scope=repository:team-a/app:pull", nil))
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("超限后状态码 = %d，期望 429（body：%s）", second.Code, second.Body.String())
	}
}

// TestHandleTokenAuthenticatedNotLimitedByAnonymousBucket 覆盖"带凭据的请求不占匿名额度"。
func TestHandleTokenAuthenticatedNotLimitedByAnonymousBucket(t *testing.T) {
	app, _ := newTokenTestApp(t)
	c := cache.NewLocal(100)
	defer c.Close()
	app.RateLimiter = service.NewRateLimiter(
		func() cache.Limiter { return cache.LimiterOf(c) },
		func(sc service.RateLimitScope) int {
			if sc == service.RateLimitScopeToken {
				return 1
			}
			return 10000
		},
		nil,
	)

	// 先用掉匿名额度。
	first := httptest.NewRecorder()
	app.handleToken(first, httptest.NewRequest(http.MethodGet, "/token?scope=repository:team-a/app:pull", nil))

	// 再带凭据来：不该被匿名桶挡住（凭据无效会返 401，但不是 429）。
	second := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/token?scope=repository:team-a/app:pull", nil)
	req.Header.Set("Authorization", "Basic YWxpY2U6d3Jvbmc=")
	app.handleToken(second, req)
	if second.Code == http.StatusTooManyRequests {
		t.Fatalf("带凭据的请求不该被匿名桶限流（body：%s）", second.Body.String())
	}
}

// TestLoginLockedRespondsLikeWrongPassword 覆盖"被锁定时的响应与口令错完全一致"。
//
// ⚠️ 这是防探测的关键：锁定与"口令错"必须**一模一样**，
// 否则就成了"这个账号是否存在"的探测接口。
func TestLoginLockedRespondsLikeWrongPassword(t *testing.T) {
	app, db := newTokenTestApp(t)
	ctx := context.Background()

	c := cache.NewLocal(100)
	defer c.Close()
	guard := service.NewLoginGuard(
		func() cache.Cache { return c },
		func() int { return 1 },
		func() time.Duration { return 5 * time.Minute },
		nil,
	)
	app.AuthSvc = service.NewAuthService(db, service.AuthOptions{Guard: guard})

	// 先失败一次 → 进入锁定期。
	if err := guard.RecordFailure(ctx, "alice", "1.2.3.4"); err != nil {
		t.Fatalf("记录失败: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
		strings.NewReader(`{"username":"alice","password":"wrong"}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(service.WithClientIP(req.Context(), "1.2.3.4"))
	app.handleLogin(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("被锁定时状态码 = %d，期望 401（body：%s）", rec.Code, rec.Body.String())
	}
	// 文案必须与普通口令错一致。
	if !strings.Contains(rec.Body.String(), "用户名或密码错误") {
		t.Fatalf("被锁定时的文案与口令错不一致：%s", rec.Body.String())
	}
}
