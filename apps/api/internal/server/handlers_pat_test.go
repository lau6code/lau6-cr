package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
	"github.com/lau6code/lau6-cr/apps/api/internal/service"
)

// patPrincipal 造一个普通用户主体（直接塞进 context，不走 session）。
func patPrincipal(u *domain.User) service.Principal {
	return service.Principal{User: u}
}

// newPATTestApp 造一个带 PAT 服务的 App，并返回一个真实用户。
func newPATTestApp(t *testing.T) (*App, *domain.User) {
	t.Helper()
	app, db := newTokenTestApp(t)
	app.Pats = service.NewPersonalTokenService(db, service.PersonalTokenOptions{})

	user, err := db.CreateUser(context.Background(), "alice", "hash", "local", "", false)
	if err != nil {
		t.Fatalf("创建用户: %v", err)
	}
	return app, user
}

// patRequest 造一个带主体与路由参数的请求。
func patRequest(t *testing.T, method, path, body string, u *domain.User, params map[string]string) *http.Request {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(withPrincipal(req.Context(), patPrincipal(u)))
	if len(params) > 0 {
		req = withRouteParams(req, params)
	}
	return req
}

// patData 解析成功响应里的 data 字段。
func patData(t *testing.T, rec *httptest.ResponseRecorder, dst any) {
	t.Helper()
	var payload struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("解析响应: %v（body：%s）", err, rec.Body.String())
	}
	if err := json.Unmarshal(payload.Data, dst); err != nil {
		t.Fatalf("解析 data: %v（body：%s）", err, rec.Body.String())
	}
}

// TestCreatePersonalTokenNeverExpires 覆盖"永久有效"令牌。
//
// 永久是用户**显式选**的：请求里必须写 never_expires=true，
// 落库 expires_at 为 NULL，列表回读也是 null。
func TestCreatePersonalTokenNeverExpires(t *testing.T) {
	app, user := newPATTestApp(t)

	rec := httptest.NewRecorder()
	req := patRequest(t, http.MethodPost, "/api/v1/auth/tokens",
		`{"name":"ci","never_expires":true}`, user, nil)
	app.handleCreatePersonalToken(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（body：%s）", rec.Code, rec.Body.String())
	}
	var created struct {
		Token     string `json:"token"`
		ExpiresAt *int64 `json:"expires_at"`
	}
	patData(t, rec, &created)
	if created.Token == "" {
		t.Fatal("响应里没有令牌本体")
	}
	if created.ExpiresAt != nil {
		t.Fatalf("永久令牌的 expires_at = %d，期望 null", *created.ExpiresAt)
	}

	// 列表回读也必须是 null（NULL 落库、NULL 读出）。
	rec = httptest.NewRecorder()
	req = patRequest(t, http.MethodGet, "/api/v1/auth/tokens", "", user, nil)
	app.handleListPersonalTokens(rec, req)
	var list struct {
		Tokens []domain.PersonalToken `json:"tokens"`
	}
	patData(t, rec, &list)
	if len(list.Tokens) != 1 {
		t.Fatalf("列出了 %d 枚令牌，期望 1 枚", len(list.Tokens))
	}
	if list.Tokens[0].ExpiresAt != nil {
		t.Fatalf("列表里的 expires_at = %d，期望 null（永久）", *list.Tokens[0].ExpiresAt)
	}
}

// TestCreatePersonalTokenWithExpiry 覆盖"指定失效时间"。
func TestCreatePersonalTokenWithExpiry(t *testing.T) {
	app, user := newPATTestApp(t)

	want := time.Now().Add(72 * time.Hour).Unix()
	rec := httptest.NewRecorder()
	req := patRequest(t, http.MethodPost, "/api/v1/auth/tokens",
		`{"name":"ci","expires_at":`+itoa(want)+`}`, user, nil)
	app.handleCreatePersonalToken(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（body：%s）", rec.Code, rec.Body.String())
	}
	var created struct {
		ExpiresAt *int64 `json:"expires_at"`
	}
	patData(t, rec, &created)
	if created.ExpiresAt == nil {
		t.Fatal("指定了失效时间，expires_at 却是 null")
	}
	if *created.ExpiresAt != want {
		t.Fatalf("expires_at = %d，期望 %d", *created.ExpiresAt, want)
	}
}

// TestCreatePersonalTokenDefaultExpiry 覆盖"不给参数 → 走系统默认"。
func TestCreatePersonalTokenDefaultExpiry(t *testing.T) {
	app, user := newPATTestApp(t)

	rec := httptest.NewRecorder()
	req := patRequest(t, http.MethodPost, "/api/v1/auth/tokens", `{"name":"ci"}`, user, nil)
	app.handleCreatePersonalToken(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（body：%s）", rec.Code, rec.Body.String())
	}
	var created struct {
		ExpiresAt *int64 `json:"expires_at"`
	}
	patData(t, rec, &created)
	if created.ExpiresAt == nil {
		t.Fatal("默认失效时间不该是永久")
	}
	// 默认 = 系统默认 TTL（30 天），不是永久、也不是自定义。
	delta := *created.ExpiresAt - time.Now().Unix()
	if delta < 29*24*3600 || delta > 31*24*3600 {
		t.Fatalf("默认失效时间距今 %d 秒，不在 30 天附近", delta)
	}
}

// TestCreatePersonalTokenRejectsContradiction 覆盖"永久 + 指定时间 → 400"。
//
// 矛盾请求必须报错，不能悄悄挑一个。
func TestCreatePersonalTokenRejectsContradiction(t *testing.T) {
	app, user := newPATTestApp(t)

	future := time.Now().Add(time.Hour).Unix()
	rec := httptest.NewRecorder()
	req := patRequest(t, http.MethodPost, "/api/v1/auth/tokens",
		`{"name":"ci","never_expires":true,"expires_at":`+itoa(future)+`}`, user, nil)
	app.handleCreatePersonalToken(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400（body：%s）", rec.Code, rec.Body.String())
	}
}

// TestCreatePersonalTokenRejectsPastExpiry 覆盖"失效时间在过去 → 400"。
func TestCreatePersonalTokenRejectsPastExpiry(t *testing.T) {
	app, user := newPATTestApp(t)

	past := time.Now().Add(-time.Hour).Unix()
	rec := httptest.NewRecorder()
	req := patRequest(t, http.MethodPost, "/api/v1/auth/tokens",
		`{"name":"ci","expires_at":`+itoa(past)+`}`, user, nil)
	app.handleCreatePersonalToken(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400（body：%s）", rec.Code, rec.Body.String())
	}
}

// TestPatchPersonalTokenExpiry 覆盖"改失效时间"。
//
// 改的是失效时间，**不是令牌本体**：散列不动，CI 里已配好的凭据继续可用。
func TestPatchPersonalTokenExpiry(t *testing.T) {
	app, user := newPATTestApp(t)
	ctx := context.Background()

	// 先建一枚永久令牌。
	raw, tok, err := app.Pats.Create(ctx, user.ID, "ci", service.ExpiryPolicy{NeverExpires: true})
	if err != nil {
		t.Fatalf("生成令牌: %v", err)
	}
	if tok.ExpiresAt != nil {
		t.Fatal("起始令牌不该有失效时间")
	}

	// 改成定时。
	want := time.Now().Add(48 * time.Hour).Unix()
	rec := httptest.NewRecorder()
	req := patRequest(t, http.MethodPatch, "/api/v1/auth/tokens/"+itoa(tok.ID),
		`{"expires_at":`+itoa(want)+`}`, user, map[string]string{"id": itoa(tok.ID)})
	app.handlePatchPersonalToken(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("改失效时间的状态码 = %d，期望 200（body：%s）", rec.Code, rec.Body.String())
	}

	list, err := app.Pats.List(ctx, user.ID)
	if err != nil {
		t.Fatalf("列令牌: %v", err)
	}
	if list[0].ExpiresAt == nil || *list[0].ExpiresAt != want {
		t.Fatalf("失效时间没改成 %d：%+v", want, list[0].ExpiresAt)
	}

	// 令牌本体没变：还是能校验通过（散列没被动过）。
	if _, _, err := app.Pats.Verify(ctx, raw); err != nil {
		t.Fatalf("改失效时间后令牌不该失效: %v", err)
	}

	// 再改回永久。
	rec = httptest.NewRecorder()
	req = patRequest(t, http.MethodPatch, "/api/v1/auth/tokens/"+itoa(tok.ID),
		`{"never_expires":true}`, user, map[string]string{"id": itoa(tok.ID)})
	app.handlePatchPersonalToken(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("改永久的状态码 = %d，期望 200（body：%s）", rec.Code, rec.Body.String())
	}
	list, err = app.Pats.List(ctx, user.ID)
	if err != nil {
		t.Fatalf("列令牌: %v", err)
	}
	if list[0].ExpiresAt != nil {
		t.Fatalf("失效时间没改成永久：%+v", list[0].ExpiresAt)
	}
}

// TestPatchPersonalTokenRejectsOthers 覆盖"改不了别人的令牌"。
//
// 别人的令牌 ID 传进来 → 404（不泄露"存在但不属于你"）。
func TestPatchPersonalTokenRejectsOthers(t *testing.T) {
	app, user := newPATTestApp(t)
	ctx := context.Background()

	other, err := app.DB.CreateUser(ctx, "bob", "hash", "local", "", false)
	if err != nil {
		t.Fatalf("创建用户: %v", err)
	}
	_, otherTok, err := app.Pats.Create(ctx, other.ID, "bob 的令牌", service.ExpiryPolicy{})
	if err != nil {
		t.Fatalf("生成令牌: %v", err)
	}

	rec := httptest.NewRecorder()
	req := patRequest(t, http.MethodPatch, "/api/v1/auth/tokens/"+itoa(otherTok.ID),
		`{"never_expires":true}`, user, map[string]string{"id": itoa(otherTok.ID)})
	app.handlePatchPersonalToken(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404（body：%s）", rec.Code, rec.Body.String())
	}
}

// TestPatchPersonalTokenNotFound 覆盖"改一枚不存在的令牌 → 404"。
func TestPatchPersonalTokenNotFound(t *testing.T) {
	app, user := newPATTestApp(t)

	rec := httptest.NewRecorder()
	req := patRequest(t, http.MethodPatch, "/api/v1/auth/tokens/9999",
		`{"never_expires":true}`, user, map[string]string{"id": "9999"})
	app.handlePatchPersonalToken(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404（body：%s）", rec.Code, rec.Body.String())
	}
}

// TestPatchPersonalTokenRejectsContradiction 覆盖"PATCH 矛盾请求 → 400"。
func TestPatchPersonalTokenRejectsContradiction(t *testing.T) {
	app, user := newPATTestApp(t)
	ctx := context.Background()

	_, tok, err := app.Pats.Create(ctx, user.ID, "ci", service.ExpiryPolicy{})
	if err != nil {
		t.Fatalf("生成令牌: %v", err)
	}
	future := time.Now().Add(time.Hour).Unix()
	rec := httptest.NewRecorder()
	req := patRequest(t, http.MethodPatch, "/api/v1/auth/tokens/"+itoa(tok.ID),
		`{"never_expires":true,"expires_at":`+itoa(future)+`}`, user,
		map[string]string{"id": itoa(tok.ID)})
	app.handlePatchPersonalToken(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400（body：%s）", rec.Code, rec.Body.String())
	}
}

// itoa 把 int64 转成十进制字符串（拼 JSON 用）。
func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}

// TestCreatePersonalTokenAuditFailureRevokesToken 覆盖"审计写失败 → 令牌作废"。
//
// ⚠️ 原始令牌只返回一次，之后库里只有散列、取不回来。
// 审计写失败时如果把令牌留在库里，就是一条
// "活跃但谁都用不了"的凭据行——用户重试一次还会多留一条孤儿。
func TestCreatePersonalTokenAuditFailureRevokesToken(t *testing.T) {
	app, user := newPATTestApp(t)
	ctx := context.Background()
	// 让审计写失败：把审计表删掉。
	if _, err := app.DB.Exec(ctx, `DROP TABLE audit_logs`); err != nil {
		t.Fatalf("删除审计表: %v", err)
	}

	rec := httptest.NewRecorder()
	req := patRequest(t, http.MethodPost, "/api/v1/auth/tokens", `{"name":"ci"}`, user, nil)
	app.handleCreatePersonalToken(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500（body：%s）", rec.Code, rec.Body.String())
	}
	// 库里不能留下这枚令牌。
	list, err := app.Pats.List(ctx, user.ID)
	if err != nil {
		t.Fatalf("列令牌: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("审计写失败后应吊销令牌，库里却还有 %d 条", len(list))
	}
}
