package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lau6code/lau6-cr/apps/api/internal/config"
)

// TestTokenServiceAllowed 覆盖 service 参数校验规则。
//
// token 是"给某个 service 用的"。与本实例的 audience 对不上就拒——
// 否则一枚在本 registry 签发的 token 能被挪到别的 service 上用。
//
// 缺省 service 是**允许**的：OCI 规范里 service 是可选参数，
// 省略不等于"服务名不匹配"。
func TestTokenServiceAllowed(t *testing.T) {
	cases := []struct {
		name      string
		requested string
		audience  string
		publicHos string
		want      bool
	}{
		{"匹配 audience", "lau6-cr", "lau6-cr", "10.10.32.2:5000", true},
		{"匹配 registry host", "10.10.32.2:5000", "lau6-cr", "10.10.32.2:5000", true},
		{"别的 service", "other-registry", "lau6-cr", "10.10.32.2:5000", false},
		{"缺省", "", "lau6-cr", "10.10.32.2:5000", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tokenServiceAllowed(tc.requested, tc.audience, tc.publicHos); got != tc.want {
				t.Fatalf("tokenServiceAllowed(%q, %q, %q) = %v，期望 %v",
					tc.requested, tc.audience, tc.publicHos, got, tc.want)
			}
		})
	}
}

// TestPublicHostOf 覆盖对外地址的 host 提取。
func TestPublicHostOf(t *testing.T) {
	cases := []struct{ in, want string }{
		{"http://10.10.32.2:5000", "10.10.32.2:5000"},
		{"https://cr.example.com", "cr.example.com"},
		{"http://10.10.32.2:5000/", "10.10.32.2:5000"},
		{"", ""},
		{"://坏地址", ""},
	}
	for _, tc := range cases {
		if got := publicHostOf(tc.in); got != tc.want {
			t.Fatalf("publicHostOf(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}

// TestHandleTokenRejectsMismatchedService 覆盖 handler 层：service 不匹配直接 401。
//
// 这条分支**在碰 TokenSvc 之前**就返回，所以不需要构造完整依赖。
func TestHandleTokenRejectsMismatchedService(t *testing.T) {
	logger, buf := newTestLogger()
	app := &App{
		Logger: logger,
		Cfg: &config.Config{
			Token: config.TokenConfig{Audience: "lau6-cr"},
			Core:  config.CoreConfig{PublicURL: "http://10.10.32.2:5000"},
		},
	}

	rec := httptest.NewRecorder()
	app.handleToken(rec, httptest.NewRequest(http.MethodGet, "/token?service=other-registry", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d，期望 401", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != `Basic realm="lau6-cr"` {
		t.Fatalf("WWW-Authenticate = %q", got)
	}
	if len(logRecords(t, buf)) == 0 {
		t.Fatalf("service 不匹配没有留日志")
	}
}

// TestHandleTokenResponseHasTopLevelToken 锁定 token 响应的形状。
//
// **这是 docker 能登录的前提**：客户端按 OCI 规范在**顶层**找
// `token`（或 `access_token`），找不到就报
// "authorization server did not include a token in the response"。
//
// 我们要是图省事套上业务信封 {"code":0,"data":{...}}，
// 客户端看不到 token → 登录与 push 全部失败，
// 而且报错信息极具误导性（像是"没发 token"，实际是"发在了 data 里"）。
//
// 规范见 docs/design/02-auth.md §5.3。
func TestHandleTokenResponseHasTopLevelToken(t *testing.T) {
	// 注入一个假的 TokenSvc 会比较重（要 DB + 权限引擎 + 签发器），
	// 这里直接验证"响应写出的形状"——handleToken 的成功路径就是它。
	rec := httptest.NewRecorder()
	writeTokenResponse(rec, "JWT-VALUE", 300)
	body := rec.Body.Bytes()

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("解析响应: %v（body=%s）", err, body)
	}
	if payload["token"] != "JWT-VALUE" {
		t.Fatalf("顶层 token = %v，期望 JWT-VALUE（body=%s）", payload["token"], body)
	}
	if payload["access_token"] != "JWT-VALUE" {
		t.Fatalf("顶层 access_token = %v", payload["access_token"])
	}
	if _, ok := payload["data"]; ok {
		t.Fatalf("响应被套了业务信封（出现 data 字段）：%s", body)
	}
	if _, ok := payload["code"]; ok {
		t.Fatalf("响应被套了业务信封（出现 code 字段）：%s", body)
	}
	if payload["expires_in"] != float64(300) {
		t.Fatalf("expires_in = %v", payload["expires_in"])
	}
}

// TestWriteRegistryErrorShape 覆盖 registry 错误信封的形状。
func TestWriteRegistryErrorShape(t *testing.T) {
	rec := httptest.NewRecorder()
	writeRegistryError(rec, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")

	var payload struct {
		Errors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("解析响应: %v（body=%s）", err, rec.Body.String())
	}
	if len(payload.Errors) != 1 {
		t.Fatalf("errors 条数 = %d，期望 1", len(payload.Errors))
	}
	if payload.Errors[0].Code != "UNAUTHORIZED" {
		t.Fatalf("code = %q", payload.Errors[0].Code)
	}
	if rec.Header().Get("Docker-Distribution-Api-Version") != "registry/2.0" {
		t.Fatalf("缺少 Docker-Distribution-Api-Version 头")
	}
}
