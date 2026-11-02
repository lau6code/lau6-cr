package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/cache"
	"github.com/lau6code/lau6-cr/apps/api/internal/config"
	"github.com/lau6code/lau6-cr/apps/api/internal/crypto"
	"github.com/lau6code/lau6-cr/apps/api/internal/service"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// passwordTestSecret 是测试用户的口令。
const passwordTestSecret = "CorrectHorse123!"

// newPasswordTestApp 造一个能处理登录的 App（含口令加密密钥库与缓存）。
func newPasswordTestApp(t *testing.T, mode string) (*App, *crypto.PasswordKeyStore) {
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

	cfg := config.Default()
	cfg.Password.Mode = mode
	// 运行期的真源是 DB（0008 迁移给 password.mode 写了行），
	// 配置基线只是启动基线 —— 所以这里必须写库，写配置不生效。
	if err := db.UpsertSetting(context.Background(), "password.mode", mode, "test"); err != nil {
		t.Fatalf("写口令传输模式: %v", err)
	}
	settings, err := service.NewSettingsManager(db, cfg, "")
	if err != nil {
		t.Fatalf("构造设置管理器: %v", err)
	}

	ks, err := crypto.NewPasswordKeyStore(crypto.PasswordKeyOptions{
		PrivatePath: filepath.Join(t.TempDir(), "password-key.pem"),
	})
	if err != nil {
		t.Fatalf("构造口令加密密钥库: %v", err)
	}
	logger, _ := newTestLogger()
	app := &App{
		Logger:       logger,
		Cfg:          cfg,
		DB:           db,
		Cache:        cache.NewHolder(cache.NewLocal(1024)),
		AuthSvc:      service.NewAuthService(db, service.AuthOptions{}),
		SettingsSvc:  settings,
		PasswordKeys: ks,
	}
	hash, err := app.hashPassword(passwordTestSecret)
	if err != nil {
		t.Fatalf("散列口令: %v", err)
	}
	if _, err := db.CreateUser(context.Background(), "alice", hash, "local", "", false); err != nil {
		t.Fatalf("创建用户: %v", err)
	}
	return app, ks
}

// encryptSecret 用服务端公钥加密口令（模拟前端）。
func encryptSecret(
	t *testing.T,
	ks *crypto.PasswordKeyStore,
	username, password, oldPassword string,
	ts time.Time,
) crypto.PasswordEnvelope {
	t.Helper()
	pub, err := base64.RawURLEncoding.DecodeString(ks.PublicBase64())
	if err != nil {
		t.Fatalf("解码公钥: %v", err)
	}
	env, err := crypto.EncryptPassword(pub, ks.KID(), username, password, oldPassword, ts)
	if err != nil {
		t.Fatalf("加密口令: %v", err)
	}
	return env
}

// loginBody 造登录请求体（env 为 nil 时是明文体）。
func loginBody(t *testing.T, env *crypto.PasswordEnvelope, username, password string) string {
	t.Helper()
	if env == nil {
		return fmt.Sprintf(`{"username":%q,"password":%q}`, username, password)
	}
	return fmt.Sprintf(`{"username":%q,"kid":%q,"epk":%q,"nonce":%q,"ct":%q}`,
		username, env.Kid, env.Epk, env.Nonce, env.Ct)
}

// doLogin 发一次登录请求（直接调 handler，绕开路由树）。
func doLogin(t *testing.T, app *App, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	app.handleLogin(rec, req)
	return rec
}

// envelopeMessage 取响应信封里的 message。
func envelopeMessage(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("解析响应 %q: %v", rec.Body.String(), err)
	}
	return env.Message
}

// TestEncryptedLoginRoundTrip 覆盖"加密登录往返成功"。
//
// 加密模式（required）下，明文口令根本到不了 bcrypt——
// 走的是"信封 → 解密 → VerifyCredentials"。
func TestEncryptedLoginRoundTrip(t *testing.T) {
	app, ks := newPasswordTestApp(t, "required")
	env := encryptSecret(t, ks, "alice", passwordTestSecret, "", time.Now())

	rec := doLogin(t, app, loginBody(t, &env, "alice", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（body：%s）", rec.Code, rec.Body.String())
	}
}

// TestEncryptedLoginWrongPasswordStillUnauthorized 覆盖"解密成功但口令错 → 仍然 401"。
//
// 响应契约不能被加密模式改掉：文案与状态码和明文模式完全一致。
func TestEncryptedLoginWrongPasswordStillUnauthorized(t *testing.T) {
	app, ks := newPasswordTestApp(t, "required")
	env := encryptSecret(t, ks, "alice", "WrongPassword123!", "", time.Now())

	rec := doLogin(t, app, loginBody(t, &env, "alice", ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d，期望 401（body：%s）", rec.Code, rec.Body.String())
	}
	if msg := envelopeMessage(t, rec); msg != "用户名或密码错误" {
		t.Fatalf("message = %q，期望固定的失败文案", msg)
	}
}

// TestEncryptedLoginRejectsStaleTimestamp 覆盖 ts 超出 ±60s 被拒。
//
// 时间窗是重放防护的第一层（13-password-transport.md §4.3）。
func TestEncryptedLoginRejectsStaleTimestamp(t *testing.T) {
	app, ks := newPasswordTestApp(t, "required")
	env := encryptSecret(t, ks, "alice", passwordTestSecret, "", time.Now().Add(-2*time.Minute))

	rec := doLogin(t, app, loginBody(t, &env, "alice", ""))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400（body：%s）", rec.Code, rec.Body.String())
	}
	if msg := envelopeMessage(t, rec); !strings.Contains(msg, "过期") {
		t.Fatalf("message = %q，期望提示请求过期", msg)
	}
}

// TestEncryptedLoginRejectsReplay 覆盖同一个 nonce 重放被拒。
//
// 纯靠时间窗不够：60 秒窗口内可以无限重放（§4.3）。
func TestEncryptedLoginRejectsReplay(t *testing.T) {
	app, ks := newPasswordTestApp(t, "required")
	env := encryptSecret(t, ks, "alice", passwordTestSecret, "", time.Now())

	first := doLogin(t, app, loginBody(t, &env, "alice", ""))
	if first.Code != http.StatusOK {
		t.Fatalf("第一次登录状态码 = %d，期望 200（body：%s）", first.Code, first.Body.String())
	}
	second := doLogin(t, app, loginBody(t, &env, "alice", ""))
	if second.Code != http.StatusBadRequest {
		t.Fatalf("重放的状态码 = %d，期望 400（body：%s）", second.Code, second.Body.String())
	}
	if msg := envelopeMessage(t, second); !strings.Contains(msg, "重复") {
		t.Fatalf("message = %q，期望提示请求重复提交", msg)
	}
}

// TestRequiredModeRejectsPlaintext 覆盖 required 模式拒明文。
func TestRequiredModeRejectsPlaintext(t *testing.T) {
	app, _ := newPasswordTestApp(t, "required")

	rec := doLogin(t, app, loginBody(t, nil, "alice", passwordTestSecret))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400（body：%s）", rec.Code, rec.Body.String())
	}
	if msg := envelopeMessage(t, rec); !strings.Contains(msg, "加密登录") {
		t.Fatalf("message = %q，期望说明当前要求加密登录", msg)
	}
}

// TestPlaintextModeRejectsCiphertext 覆盖 plaintext 模式拒密文。
func TestPlaintextModeRejectsCiphertext(t *testing.T) {
	app, ks := newPasswordTestApp(t, "plaintext")
	env := encryptSecret(t, ks, "alice", passwordTestSecret, "", time.Now())

	rec := doLogin(t, app, loginBody(t, &env, "alice", ""))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400（body：%s）", rec.Code, rec.Body.String())
	}
	if msg := envelopeMessage(t, rec); !strings.Contains(msg, "未启用加密") {
		t.Fatalf("message = %q，期望说明当前未启用加密登录", msg)
	}
}

// TestOptionalModeAcceptsBoth 覆盖 optional 模式两者都收。
func TestOptionalModeAcceptsBoth(t *testing.T) {
	app, ks := newPasswordTestApp(t, "optional")

	plain := doLogin(t, app, loginBody(t, nil, "alice", passwordTestSecret))
	if plain.Code != http.StatusOK {
		t.Fatalf("明文登录状态码 = %d，期望 200（body：%s）", plain.Code, plain.Body.String())
	}

	env := encryptSecret(t, ks, "alice", passwordTestSecret, "", time.Now())
	cipher := doLogin(t, app, loginBody(t, &env, "alice", ""))
	if cipher.Code != http.StatusOK {
		t.Fatalf("加密登录状态码 = %d，期望 200（body：%s）", cipher.Code, cipher.Body.String())
	}
}

// TestLoginAcceptsPlainHTTP 覆盖明文 HTTP 上的登录不被拒。
//
// 没有 HTTPS 反代的部署必须能登录。
func TestLoginAcceptsPlainHTTP(t *testing.T) {
	app, _ := newPasswordTestApp(t, "plaintext")

	rec := doLogin(t, app, loginBody(t, nil, "alice", passwordTestSecret))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（body：%s）", rec.Code, rec.Body.String())
	}
}

// TestSessionCookieSecureTrustsOnlyConfiguredProxies 覆盖 X-Forwarded-Proto 的信任边界。
//
// 会话 Cookie 的 Secure 属性在 `session.secure=auto` 时由 requestIsHTTPS 决定——
// 可信反代 → 采信 X-Forwarded-Proto；不可信来源 → 忽略它
// （否则攻击者自己加一个头就能让浏览器把会话 Cookie 走明文）。
func TestSessionCookieSecureTrustsOnlyConfiguredProxies(t *testing.T) {
	app, _ := newPasswordTestApp(t, "plaintext")
	app.SetTrustedProxies([]string{"127.0.0.0/8"})

	newReq := func(remoteAddr string) *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
			strings.NewReader(loginBody(t, nil, "alice", passwordTestSecret)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-Proto", "https")
		req.RemoteAddr = remoteAddr
		return req
	}

	cookieSecureOf := func(rec *httptest.ResponseRecorder) bool {
		for _, c := range rec.Result().Cookies() {
			if c.Name == app.Cfg.Session.CookieName {
				return c.Secure
			}
		}
		t.Fatalf("响应里没有会话 Cookie：%v", rec.Result().Cookies())
		return false
	}

	trusted := httptest.NewRecorder()
	app.handleLogin(trusted, newReq("127.0.0.1:34567"))
	if trusted.Code != http.StatusOK {
		t.Fatalf("可信反代的状态码 = %d，期望 200（body：%s）", trusted.Code, trusted.Body.String())
	}
	if !cookieSecureOf(trusted) {
		t.Fatal("可信反代报了 https，期望会话 Cookie 带 Secure")
	}

	untrusted := httptest.NewRecorder()
	app.handleLogin(untrusted, newReq("203.0.113.9:34567"))
	if untrusted.Code != http.StatusOK {
		t.Fatalf("不可信来源的状态码 = %d，期望 200（body：%s）", untrusted.Code, untrusted.Body.String())
	}
	if cookieSecureOf(untrusted) {
		t.Fatal("不可信来源的头不该被采信，期望会话 Cookie 不带 Secure")
	}
}

// TestPubKeyEndpoint 覆盖公钥端点的契约（§4.2）。
func TestPubKeyEndpoint(t *testing.T) {
	app, ks := newPasswordTestApp(t, "optional")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/pubkey", nil)
	rec := httptest.NewRecorder()
	app.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（body：%s）", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q，期望 no-store", cc)
	}
	var env struct {
		Data struct {
			Keys []struct {
				Kid       string `json:"kid"`
				Alg       string `json:"alg"`
				X25519Pub string `json:"x25519_pub"`
			} `json:"keys"`
			Enabled bool `json:"enabled"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("解析响应 %q: %v", rec.Body.String(), err)
	}
	if !env.Data.Enabled {
		t.Fatalf("enabled = false，期望 true")
	}
	if len(env.Data.Keys) != 1 {
		t.Fatalf("keys 数量 = %d，期望 1", len(env.Data.Keys))
	}
	key := env.Data.Keys[0]
	if key.Kid != ks.KID() {
		t.Fatalf("kid = %q，期望 %q", key.Kid, ks.KID())
	}
	if key.Alg != crypto.PasswordAlg {
		t.Fatalf("alg = %q，期望 %q", key.Alg, crypto.PasswordAlg)
	}
	if key.X25519Pub != ks.PublicBase64() {
		t.Fatalf("x25519_pub 与服务端公钥不一致")
	}
}

// TestPubKeyEndpointPlaintextMode 覆盖 plaintext 模式下 enabled=false。
func TestPubKeyEndpointPlaintextMode(t *testing.T) {
	app, _ := newPasswordTestApp(t, "plaintext")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/pubkey", nil)
	rec := httptest.NewRecorder()
	app.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（body：%s）", rec.Code, rec.Body.String())
	}
	var env struct {
		Data struct {
			Keys    []any `json:"keys"`
			Enabled bool  `json:"enabled"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("解析响应 %q: %v", rec.Body.String(), err)
	}
	if env.Data.Enabled {
		t.Fatalf("enabled = true，plaintext 模式下期望 false")
	}
	if len(env.Data.Keys) != 0 {
		t.Fatalf("keys 数量 = %d，期望 0", len(env.Data.Keys))
	}
}

// TestPubKeyEndpointMissingKeyStore 覆盖密钥库缺失时的公钥接口行为。
//
// ⚠️ 必须是 500 + 明确文案，不能是 200 + enabled=false：
//
//	后者会让前端降级成明文提交，然后被 required 拒掉，
//	用户看到的是一个莫名其妙的 400。
func TestPubKeyEndpointMissingKeyStore(t *testing.T) {
	app, _ := newPasswordTestApp(t, "optional")
	app.PasswordKeys = nil

	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/pubkey", nil)
	rec := httptest.NewRecorder()
	app.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500（body：%s）", rec.Code, rec.Body.String())
	}
	if msg := envelopeMessage(t, rec); !strings.Contains(msg, "口令加密未正确初始化") {
		t.Fatalf("message = %q，期望说明口令加密未正确初始化", msg)
	}
}

// TestCheckPasswordTransport 覆盖启动自检的判定口径。
//
// 自检存在的意义：密钥库没接上时，公钥接口会**每个请求**都 500，
// 而服务照常启动、登录也照常成功——那种坏法只有上线后才发现。
// 判定口径必须与 handlePasswordPubKey 一致：
// 模式不是 plaintext 时就必须有密钥库。
func TestCheckPasswordTransport(t *testing.T) {
	cases := []struct {
		mode     string
		withKeys bool
		wantErr  bool
	}{
		{mode: "plaintext", withKeys: false, wantErr: false},
		{mode: "plaintext", withKeys: true, wantErr: false},
		{mode: "optional", withKeys: false, wantErr: true},
		{mode: "optional", withKeys: true, wantErr: false},
		{mode: "required", withKeys: false, wantErr: true},
		{mode: "required", withKeys: true, wantErr: false},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s/keys=%v", tc.mode, tc.withKeys), func(t *testing.T) {
			app, _ := newPasswordTestApp(t, tc.mode)
			if !tc.withKeys {
				app.PasswordKeys = nil
			}
			err := app.CheckPasswordTransport()
			if tc.wantErr && err == nil {
				t.Fatalf("期望报错，实际 nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("期望不报错，实际 %v", err)
			}
		})
	}
}

// TestCreateUserEncryptedPassword 覆盖"建用户的初始口令也要过模式闸门"。
func TestCreateUserEncryptedPassword(t *testing.T) {
	app, ks := newPasswordTestApp(t, "required")
	env := encryptSecret(t, ks, "bob", "BobSecret12345!", "", time.Now())

	body := fmt.Sprintf(`{"username":"bob","kid":%q,"epk":%q,"nonce":%q,"ct":%q,"display_name":"","is_system_admin":false}`,
		env.Kid, env.Epk, env.Nonce, env.Ct)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/users", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(withPrincipal(req.Context(), adminPrincipal()))
	rec := httptest.NewRecorder()
	app.handleCreateUser(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（body：%s）", rec.Code, rec.Body.String())
	}
}

// TestCreateUserRequiredModeRejectsPlaintext 覆盖 required 模式下建用户拒明文口令。
func TestCreateUserRequiredModeRejectsPlaintext(t *testing.T) {
	app, _ := newPasswordTestApp(t, "required")

	body := `{"username":"bob","password":"BobSecret12345!","display_name":"","is_system_admin":false}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/users", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(withPrincipal(req.Context(), adminPrincipal()))
	rec := httptest.NewRecorder()
	app.handleCreateUser(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400（body：%s）", rec.Code, rec.Body.String())
	}
	if msg := envelopeMessage(t, rec); !strings.Contains(msg, "加密登录") {
		t.Fatalf("message = %q，期望说明当前要求加密登录", msg)
	}
}

// TestSetupRequiredModeRejectsPlaintext 覆盖 setup 的管理员口令也要过模式闸门。
func TestSetupRequiredModeRejectsPlaintext(t *testing.T) {
	app := newPasswordSetupApp(t, "required")

	body := `{"bootstrap_token":"0123456789abcdef0123456789abcdef","public_url":"http://localhost:5000",` +
		`"admin_username":"admin","admin_password":"AdminSecret123!"}`
	req := httptest.NewRequest(http.MethodPost, "/setup", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	app.handleSetupSubmit(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400（body：%s）", rec.Code, rec.Body.String())
	}
	if msg := envelopeMessage(t, rec); !strings.Contains(msg, "加密登录") {
		t.Fatalf("message = %q，期望说明当前要求加密登录", msg)
	}
}

// TestSetupEncryptedAdminPassword 覆盖 setup 的管理员口令可以走密文。
func TestSetupEncryptedAdminPassword(t *testing.T) {
	app := newPasswordSetupApp(t, "optional")
	ks := app.PasswordKeys
	if ks == nil {
		t.Fatalf("口令加密密钥库没设置")
	}
	env := encryptSecret(t, ks, "admin", "AdminSecret123!", "", time.Now())

	body := fmt.Sprintf(`{"bootstrap_token":"0123456789abcdef0123456789abcdef","public_url":"http://localhost:5000",`+
		`"admin_username":"admin","kid":%q,"epk":%q,"nonce":%q,"ct":%q}`,
		env.Kid, env.Epk, env.Nonce, env.Ct)
	req := httptest.NewRequest(http.MethodPost, "/setup", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	app.handleSetupSubmit(rec, req)

	if rec.Code == http.StatusBadRequest {
		if msg := envelopeMessage(t, rec); strings.Contains(msg, "加密登录") {
			t.Fatalf("密文被模式闸门拒了：%s", rec.Body.String())
		}
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（body：%s）", rec.Code, rec.Body.String())
	}
}

// TestChangePasswordEncrypted 覆盖"改密走密文"（旧口令 + 新口令都在信封里）。
func TestChangePasswordEncrypted(t *testing.T) {
	app, ks := newPasswordTestApp(t, "required")
	user, err := app.DB.UserByUsername(context.Background(), "alice")
	if err != nil {
		t.Fatalf("取用户: %v", err)
	}
	env := encryptSecret(t, ks, "alice", "NewPassword12345!", passwordTestSecret, time.Now())

	body := fmt.Sprintf(`{"username":"alice","kid":%q,"epk":%q,"nonce":%q,"ct":%q}`,
		env.Kid, env.Epk, env.Nonce, env.Ct)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/password", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(withPrincipal(req.Context(), service.Principal{User: user}))
	rec := httptest.NewRecorder()
	app.handleChangePassword(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（body：%s）", rec.Code, rec.Body.String())
	}
	// 旧口令应当失效，新口令应当能登录（两次都用密文：required 模式拒明文）。
	oldEnv := encryptSecret(t, ks, "alice", passwordTestSecret, "", time.Now())
	old := doLogin(t, app, loginBody(t, &oldEnv, "alice", ""))
	if old.Code != http.StatusUnauthorized {
		t.Fatalf("旧口令登录状态码 = %d，期望 401（body：%s）", old.Code, old.Body.String())
	}
	newEnv := encryptSecret(t, ks, "alice", "NewPassword12345!", "", time.Now())
	fresh := doLogin(t, app, loginBody(t, &newEnv, "alice", ""))
	if fresh.Code != http.StatusOK {
		t.Fatalf("新口令登录状态码 = %d，期望 200（body：%s）", fresh.Code, fresh.Body.String())
	}
}

// newPasswordSetupApp 造一个能处理 /setup 的 App（含口令加密设置）。
func newPasswordSetupApp(t *testing.T, mode string) *App {
	t.Helper()
	app := newSetupTestApp(t)
	app.Cfg = setupTestBaseConfig(t)
	app.Cfg.Password.Mode = mode
	// 运行期的真源是 DB（0008 迁移给 password.mode 写了行），
	// 配置基线只是启动基线 —— 所以这里必须写库，写配置不生效。
	if err := app.DB.UpsertSetting(context.Background(), "password.mode", mode, "test"); err != nil {
		t.Fatalf("写口令传输模式: %v", err)
	}
	settings, err := service.NewSettingsManager(app.DB, app.Cfg, "")
	if err != nil {
		t.Fatalf("构造设置管理器: %v", err)
	}
	app.SettingsSvc = settings
	app.Cache = cache.NewHolder(cache.NewLocal(1024))
	// setup 成功后会重载运行期配置；测试里没有真实的重载器。
	app.ReloadConfig = func() error { return nil }

	ks, err := crypto.NewPasswordKeyStore(crypto.PasswordKeyOptions{
		PrivatePath: filepath.Join(t.TempDir(), "password-key.pem"),
	})
	if err != nil {
		t.Fatalf("构造口令加密密钥库: %v", err)
	}
	app.PasswordKeys = ks
	return app
}
