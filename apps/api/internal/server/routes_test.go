package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lau6code/lau6-cr/apps/api/internal/config"
)

// TestIsAPIPath 锁定 SPA fallback 与 API 路径的边界。
//
// 关键一条是 "/setup"：它是**引导页**，不是 API。
// 把 "/setup" 当 API 前缀会让浏览器直接开 /setup 拿到 404 JSON，
// 引导向导（以及未初始化实例的唯一入口）就断了。
func TestIsAPIPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/api/v1/auth/me", true},
		{"/v2/team-a/app/manifests/v1", true},
		{"/token", true},
		{"/healthz", true},
		{"/readyz", true},
		{"/jwks.json", true},
		{"/setup/status", true},
		{"/setup/whatever", true},
		// realm 端点本身是 API
		{"/token", true},
		// 引导页本体：要走 SPA，返回 index.html
		{"/setup", false},
		{"/setup/", false},
		// 普通前端路由：也走 SPA
		{"/", false},
		{"/login", false},
		{"/projects/team-a", false},
		{"/index.html", false},
		// ⚠️ "/tokens" 是前端路由「访问令牌」，**不能**被 "/token" 前缀吃掉——
		//    吃掉了浏览器刷新该页面就拿到 404 JSON。
		{"/tokens", false},
	}
	for _, tc := range cases {
		if got := isAPIPath(tc.path); got != tc.want {
			t.Errorf("isAPIPath(%q) = %v，期望 %v", tc.path, got, tc.want)
		}
	}
}

// TestSetupPageServesSPA 锁定「浏览器直接开 /setup 能拿到引导页」。
//
// 前端在 setup_required 时会 window.location.assign('/setup')，
// 拿到的必须是引导页而不是 JSON 错误响应。
//
// 用嵌入副本（CR_STATIC_DIR 为空）跑，干净 checkout 下也成立。
func TestSetupPageServesSPA(t *testing.T) {
	logger, _ := newTestLogger()
	app := &App{Cfg: &config.Config{}, Logger: logger}

	rec := httptest.NewRecorder()
	app.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/setup", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /setup 状态码 = %d，期望 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("GET /setup Content-Type = %q，期望 text/html", ct)
	}
	if body := rec.Body.String(); !strings.Contains(body, "<html") {
		t.Fatalf("GET /setup 返回的不是 HTML 页面：%q", body)
	}
}

// TestTokensPageServesSPA 锁定「浏览器直接开 /tokens 能拿到 SPA 页面」。
//
// isAPIPath 用**前缀**匹配 "/token"，不能把前端路由 "/tokens"（访问令牌页）
// 一起当成 API 吃掉——否则"应用内跳转正常、刷新就 404"，很容易漏掉。
func TestTokensPageServesSPA(t *testing.T) {
	logger, _ := newTestLogger()
	app := &App{Cfg: &config.Config{}, Logger: logger}

	rec := httptest.NewRecorder()
	app.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/tokens", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /tokens 状态码 = %d，期望 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("GET /tokens Content-Type = %q，期望 text/html", ct)
	}
	if body := rec.Body.String(); !strings.Contains(body, "<html") {
		t.Fatalf("GET /tokens 返回的不是 HTML 页面：%q", body)
	}
}

// TestAPIPathsStillReturnJSON 锁定 API 前缀仍然走 API，不会被 SPA 兜底吞掉。
//
// 直接调静态兜底 handler——被兜底吞掉的典型症状就是"前端拿到 HTML 却按 JSON 解析"。
// 这里不断言具体状态码：API 路径要查库，这里没构造依赖，
// 断言的是**没有落进 SPA 兜底**（没有返回 HTML 页面）。
func TestAPIPathsStillReturnJSON(t *testing.T) {
	logger, _ := newTestLogger()
	app := &App{Cfg: &config.Config{}, Logger: logger}

	for _, path := range []string{"/setup/status", "/api/v1/auth/me", "/v2/team-a/app/tags/list"} {
		rec := httptest.NewRecorder()
		app.handleStatic(rec, httptest.NewRequest(http.MethodGet, path, nil))

		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s 状态码 = %d，期望 404（API 路径不该被 SPA 兜底接住）", path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("GET %s Content-Type = %q，期望 application/json", path, ct)
		}
	}
}
