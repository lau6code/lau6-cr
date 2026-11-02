package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/crypto"
	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// ───────────────────────── 测试脚手架 ─────────────────────────

// fakeLogger 收集日志，用于断言"该记的有没有记"。
type fakeLogger struct {
	mu   sync.Mutex
	recs []fakeLogRec
}

type fakeLogRec struct {
	level string
	msg   string
	args  []any
}

func (l *fakeLogger) add(level, msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.recs = append(l.recs, fakeLogRec{level: level, msg: msg, args: args})
}

func (l *fakeLogger) Error(msg string, args ...any) { l.add("error", msg, args...) }
func (l *fakeLogger) Warn(msg string, args ...any)  { l.add("warn", msg, args...) }
func (l *fakeLogger) Info(msg string, args ...any)  { l.add("info", msg, args...) }
func (l *fakeLogger) Debug(msg string, args ...any) { l.add("debug", msg, args...) }

// hasLevelMsg 判断是否有一条指定级别且包含子串的日志。
func (l *fakeLogger) hasLevelMsg(level, substr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.recs {
		if r.level != level {
			continue
		}
		if strings.Contains(r.msg, substr) {
			return true
		}
	}
	return false
}

// newTestDB 建一个跑完迁移的 SQLite 库。
func newTestDB(t *testing.T) *store.DB {
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
	return db
}

// newTestIssuer 造一个签发器（密钥落在临时目录）。
func newTestIssuer(t *testing.T) *crypto.TokenIssuer {
	t.Helper()
	dir := t.TempDir()
	ks, err := crypto.NewKeystore(crypto.Options{PrivatePath: filepath.Join(dir, "priv.pem"), JWKSPath: filepath.Join(dir, "jwks.json")})
	if err != nil {
		t.Fatalf("构造密钥: %v", err)
	}
	secrets := crypto.NewUserSecretStore(crypto.UserSecretOptions{Dir: filepath.Join(dir, "user-secrets")})
	issuer, err := crypto.NewTokenIssuer(ks, secrets, nil, "lau6-cr", "lau6-cr", func() time.Duration { return 5 * time.Minute })
	if err != nil {
		t.Fatalf("构造签发器: %v", err)
	}
	return issuer
}

// upstreamRecorder 记录上游收到的请求，并按固定响应回复。
type upstreamRecorder struct {
	mu       sync.Mutex
	paths    []string
	authHdrs []string
	status   int
	body     string
}

func (u *upstreamRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	u.paths = append(u.paths, r.URL.Path)
	u.authHdrs = append(u.authHdrs, r.Header.Get("Authorization"))
	status, body := u.status, u.body
	u.mu.Unlock()
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func (u *upstreamRecorder) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.paths)
}

// testRegistry 拼一套完整的 registry 服务（DB + 权限 + 签发器 + 假上游）。
func testRegistry(t *testing.T, logger Logger) (*RegistryService, *upstreamRecorder, *store.DB) {
	t.Helper()
	db := newTestDB(t)
	up := &upstreamRecorder{status: http.StatusOK, body: `{}`}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)

	issuer, projectSecrets := newTestProjectTokenIssuer(t)
	svc := NewRegistryService(RegistryDeps{
		Upstream:      srv.Client(),
		UpstreamURL:   srv.URL,
		Perm:          NewPermissionEngine(db, PermissionOptions{AnonymousReadEnabled: func() bool { return true }}),
		Issuer:        issuer,
		DB:            db,
		PATs:          NewPersonalTokenService(db, PersonalTokenOptions{}),
		ProjectTokens: NewProjectTokenService(db, ProjectTokenOptions{Secrets: projectSecrets}),
		RealmFn:       func() string { return "http://registry.example.com/token" },
		Logger:        logger,
	})
	return svc, up, db
}

// seedProject 建项目 + 仓库 + 成员，返回项目 ID。
func seedProject(t *testing.T, db *store.DB, name string, visibility domain.Visibility) int64 {
	t.Helper()
	ctx := context.Background()
	p, err := db.CreateProject(ctx, name, name, "", visibility, nil)
	if err != nil {
		t.Fatalf("创建项目: %v", err)
	}
	if _, err := db.CreateRepository(ctx, p.ID, "app", domain.VisibilityPrivate, ""); err != nil {
		t.Fatalf("创建仓库: %v", err)
	}
	return p.ID
}

// ───────────────────────── 版本检查端点 ─────────────────────────

// TestVersionCheckReturnsChallenge 锁定 docker 客户端能用的握手形态。
//
// **这是 push 能走通的前提**：docker 客户端 ping /v2/ 拿挑战，
// 拿到 200 就认定"不需要认证"，于是根本不去申请 token，
// 后续 blob 上传不带 Authorization → 被 401 拒 → push 报 unauthorized。
//
// 回归表现：这里变回 200（或挑战头缺失），docker push 立刻又挂。
func TestVersionCheckReturnsChallenge(t *testing.T) {
	svc, up, _ := testRegistry(t, &fakeLogger{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v2/", nil)
	svc.Forward(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("版本检查端点状态码 = %d，期望 401（回 200 会让 docker 客户端不去取 token）", rec.Code)
	}
	challenge := rec.Header().Get("WWW-Authenticate")
	if !strings.HasPrefix(challenge, "Bearer ") {
		t.Fatalf("WWW-Authenticate = %q，期望 Bearer 挑战", challenge)
	}
	if !strings.Contains(challenge, `realm="http://registry.example.com/token"`) {
		t.Fatalf("挑战头缺少 realm：%q", challenge)
	}
	if !strings.Contains(challenge, `service="lau6-cr"`) {
		t.Fatalf("挑战头缺少 service：%q", challenge)
	}
	if strings.Contains(challenge, "scope=") {
		t.Fatalf("版本检查端点不该带 scope（没有仓库上下文）：%q", challenge)
	}
	if up.count() != 0 {
		t.Fatalf("版本检查端点不该转发到上游，实际转发了 %d 次", up.count())
	}
}

// TestVersionCheckSlashlessPath 覆盖不带尾斜杠的写法。
func TestVersionCheckSlashlessPath(t *testing.T) {
	svc, _, _ := testRegistry(t, &fakeLogger{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v2", nil)
	svc.Forward(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("/v2 状态码 = %d，期望 401", rec.Code)
	}
}

// TestVersionCheckWithValidTokenReturnsOK 覆盖"带有效 token 的版本检查"。
//
// docker login 的成功判据就是这一步：换到 token 后再 ping 一次 /v2/，
// **拿到 200** 才算登录成功。无脑回 401 会让登录永远失败
// （客户端报 "login attempt ... failed with status: 401 Unauthorized"）。
func TestVersionCheckWithValidTokenReturnsOK(t *testing.T) {
	svc, up, _ := testRegistry(t, &fakeLogger{})

	// 登录探活拿到的 token 是空 access 的（客户端请求时 scope 为空）。
	token, err := svc.issuer.Issue("admin", nil)
	if err != nil {
		t.Fatalf("签发 token: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v2/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	svc.Forward(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("带有效 token 的版本检查状态码 = %d，期望 200（docker login 的成功判据）", rec.Code)
	}
	if rec.Header().Get("Docker-Distribution-Api-Version") != "registry/2.0" {
		t.Fatalf("缺少 Docker-Distribution-Api-Version 头")
	}
	if up.count() != 0 {
		t.Fatalf("版本检查不该转发到上游，实际转发了 %d 次", up.count())
	}
}

// TestVersionCheckWithInvalidTokenChallenges 覆盖"带了坏 token 仍要挑战"。
func TestVersionCheckWithInvalidTokenChallenges(t *testing.T) {
	svc, _, _ := testRegistry(t, &fakeLogger{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v2/", nil)
	req.Header.Set("Authorization", "Bearer not-a-real-token")
	svc.Forward(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d，期望 401", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("WWW-Authenticate"), "realm=") {
		t.Fatalf("缺少挑战头：%q", rec.Header().Get("WWW-Authenticate"))
	}
}

// ───────────────────────── 挑战里的 scope ─────────────────────────

// TestChallengeScopeForWritePath 覆盖写路径的 scope。
//
// blob 上传属于"写"，scope 必须带 push：部分客户端按挑战里的 scope
// 去申请 token，只给 pull 会让它们最后一步 manifest PUT 才 403。
func TestChallengeScopeForWritePath(t *testing.T) {
	svc, _, _ := testRegistry(t, &fakeLogger{})

	cases := []struct {
		name   string
		method string
		path   string
		want   string
	}{
		{"blob 上传", http.MethodPost, "/v2/team-a/app/blobs/uploads/", "repository:team-a/app:pull,push"},
		{"blob 分片", http.MethodPatch, "/v2/team-a/app/blobs/uploads/abc", "repository:team-a/app:pull,push"},
		{"manifest 写入", http.MethodPut, "/v2/team-a/app/manifests/v1", "repository:team-a/app:pull,push"},
		{"manifest 读取", http.MethodGet, "/v2/team-a/app/manifests/v1", "repository:team-a/app:pull"},
		{"blob 读取", http.MethodGet, "/v2/team-a/app/blobs/sha256:" + strings.Repeat("a", 64), "repository:team-a/app:pull"},
		{"manifest 删除", http.MethodDelete, "/v2/team-a/app/manifests/sha256:" + strings.Repeat("a", 64), "repository:team-a/app:pull,delete"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, tc.path, nil)
			svc.Forward(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("状态码 = %d，期望 401", rec.Code)
			}
			challenge := rec.Header().Get("WWW-Authenticate")
			want := `scope="` + tc.want + `"`
			if !strings.Contains(challenge, want) {
				t.Fatalf("挑战头 = %q，期望包含 %s", challenge, want)
			}
		})
	}
}

// TestUploadRequiresPushPermission 覆盖"只有 pull 权限的 token 不能写 blob"。
//
// blob 上传虽然不涉及 tag 覆盖策略，但它是在往仓库里写数据，
// 用 pull 放行等于把写权限白送。
func TestUploadRequiresPushPermission(t *testing.T) {
	svc, up, db := testRegistry(t, &fakeLogger{})
	ctx := context.Background()

	seedProject(t, db, "team-a", domain.VisibilityPrivate)
	user, err := db.CreateUser(ctx, "reader", "hash", "local", "", false)
	if err != nil {
		t.Fatalf("创建用户: %v", err)
	}
	project, err := db.ProjectByName(ctx, "team-a")
	if err != nil {
		t.Fatalf("查项目: %v", err)
	}
	if err := db.AddMember(ctx, project.ID, user.ID, domain.RoleReader, nil); err != nil {
		t.Fatalf("加成员: %v", err)
	}

	// 只授予 pull 的 token。
	token, err := svc.issuer.Issue("reader", []domain.Access{{
		Type: "repository", Name: "team-a/app", Actions: []string{"pull"},
	}})
	if err != nil {
		t.Fatalf("签发 token: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v2/team-a/app/blobs/uploads/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	svc.Forward(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("只读 token 上传 blob 的状态码 = %d，期望 403", rec.Code)
	}
	if up.count() != 0 {
		t.Fatalf("鉴权失败不应转发到上游，实际转发了 %d 次", up.count())
	}
}

// TestAnonymousTokenPushReturns401 锁定"未登录的推送回 401 + 挑战"。
//
// ⚠️ 这里回 403 是**错的信号**：403 说的是"你被拒绝了"，
// 客户端据此认定"我是有身份只是没权限"，于是拿着同一枚匿名 token
// 反复重试（用户看到的现象）。
//
// 401 + 挑战才让客户端知道"这是没登录，去认证"。
func TestAnonymousTokenPushReturns401(t *testing.T) {
	svc, up, db := testRegistry(t, &fakeLogger{})

	// seedProject 连带建好 team-a/app 仓库。
	seedProject(t, db, "team-a", domain.VisibilityPublic)

	// 匿名 token：只给 pull（公开仓库的匿名可见性）。
	token, err := svc.issuer.Issue(anonymousSubject, []domain.Access{{
		Type: "repository", Name: "team-a/app", Actions: []string{"pull"},
	}})
	if err != nil {
		t.Fatalf("签发匿名 token: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v2/team-a/app/blobs/uploads/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	svc.Forward(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("匿名 token 上传 blob 的状态码 = %d，期望 401（403 会让客户端反复重试）", rec.Code)
	}
	challenge := rec.Header().Get("WWW-Authenticate")
	if !strings.Contains(challenge, `scope="repository:team-a/app:pull,push"`) {
		t.Fatalf("挑战头 = %q，期望声明 pull,push scope（客户端据此知道要什么权限）", challenge)
	}
	if up.count() != 0 {
		t.Fatalf("鉴权失败不应转发到上游，实际转发了 %d 次", up.count())
	}
}

// TestInvalidTokenPushReturns401WithChallenge 覆盖"坏 token 也要带挑战"。
//
// 只回 401 不带挑战的话，客户端没地方可去换 token，
// 只会拿着同一枚坏 token 反复重试。
func TestInvalidTokenPushReturns401WithChallenge(t *testing.T) {
	svc, up, _ := testRegistry(t, &fakeLogger{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v2/team-a/app/blobs/uploads/", nil)
	req.Header.Set("Authorization", "Bearer not-a-real-token")
	svc.Forward(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("坏 token 上传 blob 的状态码 = %d，期望 401", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("WWW-Authenticate"), "realm=") {
		t.Fatalf("缺少挑战头：%q", rec.Header().Get("WWW-Authenticate"))
	}
	if up.count() != 0 {
		t.Fatalf("鉴权失败不应转发到上游，实际转发了 %d 次", up.count())
	}
}

// TestPushPermissionReachesUpstream 覆盖"有 push 权限时请求确实到了上游"。
func TestPushPermissionReachesUpstream(t *testing.T) {
	svc, up, db := testRegistry(t, &fakeLogger{})
	ctx := context.Background()

	seedProject(t, db, "team-a", domain.VisibilityPrivate)
	user, err := db.CreateUser(ctx, "dev", "hash", "local", "", false)
	if err != nil {
		t.Fatalf("创建用户: %v", err)
	}
	project, err := db.ProjectByName(ctx, "team-a")
	if err != nil {
		t.Fatalf("查项目: %v", err)
	}
	if err := db.AddMember(ctx, project.ID, user.ID, domain.RoleDeveloper, nil); err != nil {
		t.Fatalf("加成员: %v", err)
	}

	token, err := svc.issuer.Issue("dev", []domain.Access{{
		Type: "repository", Name: "team-a/app", Actions: []string{"pull", "push"},
	}})
	if err != nil {
		t.Fatalf("签发 token: %v", err)
	}

	up.mu.Lock()
	up.status = http.StatusAccepted
	up.body = `{"uuid":"x"}`
	up.mu.Unlock()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v2/team-a/app/blobs/uploads/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	svc.Forward(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("状态码 = %d，期望 202（鉴权应通过并转发上游）", rec.Code)
	}
	if up.count() != 1 {
		t.Fatalf("上游收到 %d 次请求，期望 1 次", up.count())
	}
	up.mu.Lock()
	gotAuth := up.authHdrs[0]
	up.mu.Unlock()
	// ⚠️ 上游**不能**收到客户端那枚令牌：
	// 客户端令牌是用户级 HS256，distribution 只认 JWKS 里的 EdDSA 公钥。
	// 转发前必须换发一枚实例级令牌（载荷一致）。
	if gotAuth == "Bearer "+token {
		t.Fatalf("转发给上游的 Authorization 与客户端令牌相同——上游只应看到 lau6-cr 自己签的令牌")
	}
	upstreamTok := strings.TrimPrefix(gotAuth, "Bearer ")
	claims, err := svc.issuer.Verify(context.Background(), upstreamTok)
	if err != nil {
		t.Fatalf("上游令牌验签失败: %v", err)
	}
	if claims.Subject != "dev" {
		t.Fatalf("上游令牌 subject = %q，期望 dev", claims.Subject)
	}
	if !crypto.AllowsAccess(claims, "team-a/app", "push") {
		t.Fatalf("上游令牌没有 team-a/app 的 push 权限")
	}
	if alg := tokenAlg(t, upstreamTok); alg != "EdDSA" {
		t.Fatalf("上游令牌 alg = %q，期望 EdDSA（distribution 只认 JWKS 里的 EdDSA 公钥）", alg)
	}
}

// tokenAlg 取 JWT 头里的 alg。
func tokenAlg(t *testing.T, raw string) string {
	t.Helper()
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT 结构不对：%q", raw)
	}
	head, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("解 JWT 头: %v", err)
	}
	var h struct {
		Alg string `json:"alg"`
	}
	if err := json.Unmarshal(head, &h); err != nil {
		t.Fatalf("解 JWT 头 JSON: %v", err)
	}
	return h.Alg
}

// ───────────────────────── 日志可观测性 ─────────────────────────

// TestAuthDenialIsLogged 锁定"鉴权失败必须留痕"。
//
// 没有这条日志，客户端报 unauthorized 时后端就是一片安静，
// 排障只能靠猜——这正是当初这个 bug 难查的原因。
func TestAuthDenialIsLogged(t *testing.T) {
	logger := &fakeLogger{}
	svc, _, _ := testRegistry(t, logger)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v2/team-a/app/blobs/uploads/", nil)
	svc.Forward(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d，期望 401", rec.Code)
	}
	if !logger.hasLevelMsg("warn", "registry 请求被拒绝") {
		t.Fatalf("鉴权拒绝没有记 warn 日志，日志内容：%+v", logger.recs)
	}
}

// TestUpstreamErrorStatusIsLogged 覆盖"上游 4xx 必须落在 warn 及以上"。
//
// 只记 debug 等于没记——默认日志级别是 info。
func TestUpstreamErrorStatusIsLogged(t *testing.T) {
	logger := &fakeLogger{}
	svc, up, db := testRegistry(t, logger)
	ctx := context.Background()

	seedProject(t, db, "team-a", domain.VisibilityPublic)
	project, err := db.ProjectByName(ctx, "team-a")
	if err != nil {
		t.Fatalf("查项目: %v", err)
	}
	if _, err := db.CreateRepository(ctx, project.ID, "pub", domain.VisibilityPublic, ""); err != nil {
		t.Fatalf("创建公开仓库: %v", err)
	}

	up.mu.Lock()
	up.status = http.StatusUnauthorized
	up.body = `{"errors":[{"code":"UNAUTHORIZED"}]}`
	up.mu.Unlock()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v2/team-a/pub/manifests/v1", nil)
	svc.Forward(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d，期望透传上游的 401", rec.Code)
	}
	if !logger.hasLevelMsg("warn", "上游 registry 返回错误状态") {
		t.Fatalf("上游 4xx 没有记 warn 日志，日志内容：%+v", logger.recs)
	}
}

// TestManifestPutEnqueuesRefresh 覆盖"push 成功后立刻排队刷新目录"。
//
// lau6-cr 就在 push 的数据路径上：manifest 写入它看得见。
// 所以目录**不必**等上游的事件回调（远程 distribution 常常没配）
// 或 30 分钟一轮的对账——push 成功的那一刻就该排队刷新。
func TestManifestPutEnqueuesRefresh(t *testing.T) {
	svc, up, db := testRegistry(t, &fakeLogger{})
	ctx := context.Background()
	seedProjectOnly(t, db, "team-a")

	q := NewRefreshQueue()
	svc.refreshQueue = q

	up.mu.Lock()
	up.status = http.StatusCreated
	up.body = `{"digest":"sha256:abc"}`
	up.mu.Unlock()

	// 用带 push 权限的 token 走 manifest PUT。
	token, err := svc.issuer.Issue("dev", []domain.Access{{
		Type: "repository", Name: "team-a/app", Actions: []string{"pull", "push"},
	}})
	if err != nil {
		t.Fatalf("签发 token: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v2/team-a/app/manifests/v1", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+token)
	svc.PutManifest(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("状态码 = %d，期望 201", rec.Code)
	}
	key, ok := q.Take()
	if !ok {
		t.Fatalf("push 成功后队列里没有待刷新的仓库（目录不会立刻更新）")
	}
	if key != "team-a/app" {
		t.Fatalf("队列键 = %q，期望 team-a/app", key)
	}
	// 仓库行也应当被建出来（上游第一次出现的仓库）。
	project, err := db.ProjectByName(ctx, "team-a")
	if err != nil {
		t.Fatalf("查项目: %v", err)
	}
	if _, err := db.RepositoryByPath(ctx, project.ID, "app"); err != nil {
		t.Fatalf("push 后应当自动建仓: %v", err)
	}
}

// TestManifestPutRequiresPushPermission 覆盖"写 manifest 必须有 push 权限"。
//
// ⚠️ 这是 P0 级别的洞：tag 覆盖保护被当成了鉴权的替代品——
// "策略关闭 → 直接透传"那条分支**完全不做鉴权**，
// 于是匿名 `PUT /v2/<repo>/manifests/<tag>` 会被原样转发给上游，
// 上游没配鉴权时就真写进去了：**任何人都能覆盖任意仓库的 tag**。
//
// 无论策略开关如何，写 manifest 都必须有 push 权限。
func TestManifestPutRequiresPushPermission(t *testing.T) {
	svc, up, db := testRegistry(t, &fakeLogger{})
	seedProjectOnly(t, db, "team-a")

	up.mu.Lock()
	up.status = http.StatusCreated
	up.body = `{"digest":"sha256:abc"}`
	up.mu.Unlock()

	// ① 匿名：必须被拒，且**不能**转发到上游。
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v2/team-a/app/manifests/v1", strings.NewReader("{}"))
	svc.PutManifest(rec, req)

	if rec.Code == http.StatusCreated || rec.Code == http.StatusOK {
		t.Fatalf("匿名写 manifest 的状态码 = %d，期望被拒（此前会 201 并真写进上游）", rec.Code)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("匿名写 manifest 的状态码 = %d，期望 401", rec.Code)
	}
	if up.count() != 0 {
		t.Fatalf("被拒的写入不该转发到上游，实际转发了 %d 次", up.count())
	}
}

// TestManifestPutDeniedForTokenWithoutPush 覆盖"token 没有 push 权限 → 403"。
func TestManifestPutDeniedForTokenWithoutPush(t *testing.T) {
	svc, up, db := testRegistry(t, &fakeLogger{})
	seedProjectOnly(t, db, "team-a")

	up.mu.Lock()
	up.status = http.StatusCreated
	up.body = `{"digest":"sha256:abc"}`
	up.mu.Unlock()

	// 只授予 pull 的 token。
	token, err := svc.issuer.Issue("reader", []domain.Access{{
		Type: "repository", Name: "team-a/app", Actions: []string{"pull"},
	}})
	if err != nil {
		t.Fatalf("签发 token: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v2/team-a/app/manifests/v1", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+token)
	svc.PutManifest(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("只读 token 写 manifest 的状态码 = %d，期望 403", rec.Code)
	}
	if up.count() != 0 {
		t.Fatalf("被拒的写入不该转发到上游，实际转发了 %d 次", up.count())
	}
}

// TestCrossRepoMountRequiresSourcePull 覆盖"跨仓库 mount 要校验源仓库的 pull 权限"。
//
// mount 会把**源仓库的 blob** 搬进目标仓库：
// 源仓库没有 pull 权限却能 mount，等于把私有仓库的镜像内容搬到自己名下再拉走。
func TestCrossRepoMountRequiresSourcePull(t *testing.T) {
	svc, up, db := testRegistry(t, &fakeLogger{})
	seedProjectOnly(t, db, "team-a")

	up.mu.Lock()
	up.status = http.StatusCreated
	up.body = `{"uuid":"x"}`
	up.mu.Unlock()

	// 目标仓库有 push，源仓库没有。
	token, err := svc.issuer.Issue("dev", []domain.Access{{
		Type: "repository", Name: "team-a/app", Actions: []string{"pull", "push"},
	}})
	if err != nil {
		t.Fatalf("签发 token: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost,
		"/v2/team-a/app/blobs/uploads/?mount=sha256:"+strings.Repeat("a", 64)+"&from=secret/app", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	svc.Forward(rec, req)

	if rec.Code == http.StatusCreated || rec.Code == http.StatusAccepted {
		t.Fatalf("源仓库无 pull 权限时 mount 状态码 = %d，期望被拒", rec.Code)
	}
	if up.count() != 0 {
		t.Fatalf("被拒的 mount 不该转发到上游，实际转发了 %d 次", up.count())
	}
}
func TestManifestPutFailureDoesNotEnqueue(t *testing.T) {
	svc, up, db := testRegistry(t, &fakeLogger{})
	seedProjectOnly(t, db, "team-a")

	q := NewRefreshQueue()
	svc.refreshQueue = q

	up.mu.Lock()
	up.status = http.StatusInternalServerError
	up.body = `{"errors":[{"code":"UNKNOWN"}]}`
	up.mu.Unlock()

	token, err := svc.issuer.Issue("dev", []domain.Access{{
		Type: "repository", Name: "team-a/app", Actions: []string{"pull", "push"},
	}})
	if err != nil {
		t.Fatalf("签发 token: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v2/team-a/app/manifests/v1", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+token)
	svc.PutManifest(rec, req)

	if _, ok := q.Take(); ok {
		t.Fatalf("失败的写入不该触发刷新")
	}
}

// TestCatalogFiltersByVisibility 覆盖"_catalog 不得泄露私有仓库名"。
//
// 上游返回的是全部仓库；直接透传等于把私有仓库名公开。
func TestCatalogFiltersByVisibility(t *testing.T) {
	svc, up, db := testRegistry(t, &fakeLogger{})
	ctx := context.Background()

	// 公开面要求**项目与仓库都公开**，所以公开仓库要建在公开项目里。
	seedProject(t, db, "pub-team", domain.VisibilityPublic)
	pubProject, err := db.ProjectByName(ctx, "pub-team")
	if err != nil {
		t.Fatalf("查公开项目: %v", err)
	}
	if _, err := db.CreateRepository(ctx, pubProject.ID, "pub", domain.VisibilityPublic, ""); err != nil {
		t.Fatalf("创建公开仓库: %v", err)
	}
	// 私有项目 + 私有仓库。
	seedProject(t, db, "team-a", domain.VisibilityPrivate)

	up.mu.Lock()
	up.body = `{"repositories":["pub-team/pub","team-a/app"]}`
	up.mu.Unlock()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v2/_catalog", nil)
	svc.catalog(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}
	var payload struct {
		Repositories []string `json:"repositories"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("解析响应: %v（body=%s）", err, rec.Body.String())
	}
	if len(payload.Repositories) != 1 || payload.Repositories[0] != "pub-team/pub" {
		t.Fatalf("目录 = %v，期望只含公开仓库 [pub-team/pub]", payload.Repositories)
	}
}

// TestPersonalTokenPushesThroughRegistry 覆盖"PAT 直接当 Bearer 令牌能推拉"。
//
// 这是 CI / 脚本的最短路径：把 PAT 放进 Authorization 头，不必先走 realm。
//
// 权限判定走**权限引擎**（按用户当前角色实时算），
// 而不是令牌里带的 scope——PAT 本身不带 scope。
func TestPersonalTokenPushesThroughRegistry(t *testing.T) {
	svc, up, db := testRegistry(t, &fakeLogger{})
	ctx := context.Background()

	seedProject(t, db, "team-a", domain.VisibilityPrivate)
	user, err := db.CreateUser(ctx, "dev", "hash", "local", "", false)
	if err != nil {
		t.Fatalf("创建用户: %v", err)
	}
	project, err := db.ProjectByName(ctx, "team-a")
	if err != nil {
		t.Fatalf("查项目: %v", err)
	}
	if err := db.AddMember(ctx, project.ID, user.ID, domain.RoleDeveloper, nil); err != nil {
		t.Fatalf("加成员: %v", err)
	}
	// 私有仓库还要**显式授权**（02-auth.md §4.3 第 7 步）——
	// PAT 走的是权限引擎，与 realm 换 token 的判定口径一致。
	repo, err := db.RepositoryByPath(ctx, project.ID, "app")
	if err != nil {
		t.Fatalf("查仓库: %v", err)
	}
	if _, err := db.AddGrant(ctx, repo.ID, user.ID, nil); err != nil {
		t.Fatalf("授权私有仓库: %v", err)
	}

	pats := NewPersonalTokenService(db, PersonalTokenOptions{})
	raw, _, err := pats.Create(ctx, user.ID, "ci", ExpiryPolicy{})
	if err != nil {
		t.Fatalf("生成令牌: %v", err)
	}

	up.mu.Lock()
	up.status = http.StatusAccepted
	up.body = `{"uuid":"x"}`
	up.mu.Unlock()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v2/team-a/app/blobs/uploads/", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	svc.Forward(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("状态码 = %d，期望 202（PAT 应通过鉴权并转发上游）", rec.Code)
	}
	up.mu.Lock()
	gotAuth := up.authHdrs[0]
	up.mu.Unlock()
	// 上游只认 EdDSA，PAT 必须换发成实例级令牌。
	upstreamTok := strings.TrimPrefix(gotAuth, "Bearer ")
	if gotAuth == "Bearer "+raw {
		t.Fatal("上游收到了 PAT 本身——上游只认 EdDSA")
	}
	if alg := tokenAlg(t, upstreamTok); alg != "EdDSA" {
		t.Fatalf("上游令牌 alg = %q，期望 EdDSA", alg)
	}
	claims, err := svc.issuer.Verify(ctx, upstreamTok)
	if err != nil {
		t.Fatalf("上游令牌验签失败: %v", err)
	}
	if claims.Subject != "dev" {
		t.Fatalf("上游令牌 subject = %q，期望 dev", claims.Subject)
	}
	if !crypto.AllowsAccess(claims, "team-a/app", "push") {
		t.Fatalf("上游令牌没有 team-a/app 的 push 权限")
	}
}

// TestPersonalTokenWithoutPermissionDenied 覆盖"权限不够 → 403"。
//
// PAT 是身份钥匙，不是万能钥匙：用户没有的权限，PAT 也变不出来。
func TestPersonalTokenWithoutPermissionDenied(t *testing.T) {
	svc, up, db := testRegistry(t, &fakeLogger{})
	ctx := context.Background()

	seedProject(t, db, "team-a", domain.VisibilityPrivate)
	// outsider 不是任何项目的成员。
	user, err := db.CreateUser(ctx, "outsider", "hash", "local", "", false)
	if err != nil {
		t.Fatalf("创建用户: %v", err)
	}
	pats := NewPersonalTokenService(db, PersonalTokenOptions{})
	raw, _, err := pats.Create(ctx, user.ID, "ci", ExpiryPolicy{})
	if err != nil {
		t.Fatalf("生成令牌: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v2/team-a/app/blobs/uploads/", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	svc.Forward(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("状态码 = %d，期望 403（无权限的用户不该能推）", rec.Code)
	}
	if up.count() != 0 {
		t.Fatalf("上游收到 %d 次请求，期望 0 次（不该转发）", up.count())
	}
}

// TestRevokedPersonalTokenRejected 覆盖"吊销后立刻 401"。
func TestRevokedPersonalTokenRejected(t *testing.T) {
	svc, up, db := testRegistry(t, &fakeLogger{})
	ctx := context.Background()

	seedProject(t, db, "team-a", domain.VisibilityPrivate)
	user, err := db.CreateUser(ctx, "dev", "hash", "local", "", false)
	if err != nil {
		t.Fatalf("创建用户: %v", err)
	}
	project, err := db.ProjectByName(ctx, "team-a")
	if err != nil {
		t.Fatalf("查项目: %v", err)
	}
	if err := db.AddMember(ctx, project.ID, user.ID, domain.RoleDeveloper, nil); err != nil {
		t.Fatalf("加成员: %v", err)
	}
	pats := NewPersonalTokenService(db, PersonalTokenOptions{})
	raw, tok, err := pats.Create(ctx, user.ID, "ci", ExpiryPolicy{})
	if err != nil {
		t.Fatalf("生成令牌: %v", err)
	}
	if err := pats.Revoke(ctx, tok.ID, user.ID); err != nil {
		t.Fatalf("吊销: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v2/team-a/app/blobs/uploads/", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	svc.Forward(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d，期望 401（吊销后应当重新认证）", rec.Code)
	}
	if up.count() != 0 {
		t.Fatalf("上游收到 %d 次请求，期望 0 次", up.count())
	}
}
