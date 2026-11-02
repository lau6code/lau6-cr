package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/lau6code/lau6-cr/apps/api/internal/crypto"
)

// adminResetSecret 是管理员重置时写入的新口令。
const adminResetSecret = "ResetSecret12345!"

// newAdminResetApp 造一个带三类账号的 App：
//
//	alice —— 本地非管理员（可被重置）
//	bob   —— LDAP 账号（口令在目录里，不可重置）
//	root  —— 本地系统管理员（不可被重置）
func newAdminResetApp(t *testing.T, mode string) (*App, *crypto.PasswordKeyStore) {
	t.Helper()
	app, ks := newPasswordTestApp(t, mode)
	hash, err := app.hashPassword(passwordTestSecret)
	if err != nil {
		t.Fatalf("散列口令: %v", err)
	}
	if _, err := app.DB.CreateUser(context.Background(), "bob", hash, "ldap", "", false); err != nil {
		t.Fatalf("创建 LDAP 账号: %v", err)
	}
	if _, err := app.DB.CreateUser(context.Background(), "root", hash, "local", "", true); err != nil {
		t.Fatalf("创建系统管理员账号: %v", err)
	}
	return app, ks
}

// doAdminReset 发一次管理员重置口令的请求（直接调 handler，绕开路由树）。
func doAdminReset(t *testing.T, app *App, userID int64, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/users/1/password", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = withRouteParams(req, map[string]string{"id": strconv.FormatInt(userID, 10)})
	req = req.WithContext(withPrincipal(req.Context(), adminPrincipal()))
	rec := httptest.NewRecorder()
	app.handleAdminResetPassword(rec, req)
	return rec
}

// userIDByName 查账号的主键。
func userIDByName(t *testing.T, app *App, username string) int64 {
	t.Helper()
	u, err := app.DB.UserByUsername(context.Background(), username)
	if err != nil {
		t.Fatalf("读取用户 %q: %v", username, err)
	}
	return u.ID
}

// passwordMatches 判断库里存的散列是不是这个口令的。
func passwordMatches(t *testing.T, app *App, username, password string) bool {
	t.Helper()
	u, err := app.DB.UserByUsername(context.Background(), username)
	if err != nil {
		t.Fatalf("读取用户 %q: %v", username, err)
	}
	return bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) == nil
}

// TestAdminResetPassword 覆盖"管理员替本地非管理员重置口令"。
//
// 语义（02-auth.md §1.2）：不验旧口令，只改口令散列。
func TestAdminResetPassword(t *testing.T) {
	app, _ := newAdminResetApp(t, "optional")
	id := userIDByName(t, app, "alice")

	rec := doAdminReset(t, app, id, fmt.Sprintf(`{"new_password":%q}`, adminResetSecret))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（body：%s）", rec.Code, rec.Body.String())
	}
	if !passwordMatches(t, app, "alice", adminResetSecret) {
		t.Fatalf("新口令不生效")
	}
	if passwordMatches(t, app, "alice", passwordTestSecret) {
		t.Fatalf("旧口令仍然有效")
	}
}

// TestAdminResetPasswordWritesAudit 覆盖重置口令落审计事件。
//
// 管理员替别人改口令是"高敏感"操作，没有审计等于没发生过。
func TestAdminResetPasswordWritesAudit(t *testing.T) {
	app, _ := newAdminResetApp(t, "optional")
	id := userIDByName(t, app, "alice")

	rec := doAdminReset(t, app, id, fmt.Sprintf(`{"new_password":%q}`, adminResetSecret))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（body：%s）", rec.Code, rec.Body.String())
	}
	logs, err := app.DB.ListAudit(context.Background(), "", "user.password.reset", 10, 0)
	if err != nil {
		t.Fatalf("读取审计: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("审计条数 = %d，期望 1", len(logs))
	}
	if logs[0].Target != "alice" {
		t.Fatalf("审计 target = %q，期望 alice", logs[0].Target)
	}
}

// TestAdminResetPasswordRejectsLDAP 覆盖 LDAP 账号不可重置。
//
// LDAP 的口令在目录里；改本地散列会造出一个与目录不一致的影子账号。
func TestAdminResetPasswordRejectsLDAP(t *testing.T) {
	app, _ := newAdminResetApp(t, "optional")
	id := userIDByName(t, app, "bob")

	rec := doAdminReset(t, app, id, fmt.Sprintf(`{"new_password":%q}`, adminResetSecret))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400（body：%s）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "目录") {
		t.Fatalf("body = %q，期望说明口令由目录维护", rec.Body.String())
	}
	if passwordMatches(t, app, "bob", adminResetSecret) {
		t.Fatalf("LDAP 账号的口令被改了")
	}
}

// TestAdminResetPasswordRejectsSystemAdmin 覆盖系统管理员账号不可被重置。
//
// 管理员能重置管理员的口令 = 能登录别人的管理员账号（横向接管）。
func TestAdminResetPasswordRejectsSystemAdmin(t *testing.T) {
	app, _ := newAdminResetApp(t, "optional")
	id := userIDByName(t, app, "root")

	rec := doAdminReset(t, app, id, fmt.Sprintf(`{"new_password":%q}`, adminResetSecret))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("状态码 = %d，期望 403（body：%s）", rec.Code, rec.Body.String())
	}
	if passwordMatches(t, app, "root", adminResetSecret) {
		t.Fatalf("系统管理员的口令被改了")
	}
}

// TestAdminResetPasswordRejectsShortPassword 覆盖强度下限（≥12 位）。
func TestAdminResetPasswordRejectsShortPassword(t *testing.T) {
	app, _ := newAdminResetApp(t, "optional")
	id := userIDByName(t, app, "alice")

	rec := doAdminReset(t, app, id, `{"new_password":"short1"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400（body：%s）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "12 位") {
		t.Fatalf("body = %q，期望说明长度下限", rec.Body.String())
	}
}

// TestAdminResetPasswordEncrypted 覆盖加密模式下重置口令。
//
// 密文用**目标用户的用户名**参与 HKDF 派生——服务端从 URL 的 id 查库得到它。
func TestAdminResetPasswordEncrypted(t *testing.T) {
	app, ks := newAdminResetApp(t, "required")
	id := userIDByName(t, app, "alice")

	env := encryptSecret(t, ks, "alice", adminResetSecret, "", time.Now())
	body := fmt.Sprintf(`{"kid":%q,"epk":%q,"nonce":%q,"ct":%q}`,
		env.Kid, env.Epk, env.Nonce, env.Ct)

	rec := doAdminReset(t, app, id, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（body：%s）", rec.Code, rec.Body.String())
	}
	if !passwordMatches(t, app, "alice", adminResetSecret) {
		t.Fatalf("新口令不生效")
	}
}

// TestAdminResetPasswordRequiredModeRejectsPlaintext 覆盖 required 模式拒明文。
//
// 口令来源要过 password.mode 闸门，不能因为是管理员就绕过。
func TestAdminResetPasswordRequiredModeRejectsPlaintext(t *testing.T) {
	app, _ := newAdminResetApp(t, "required")
	id := userIDByName(t, app, "alice")

	rec := doAdminReset(t, app, id, fmt.Sprintf(`{"new_password":%q}`, adminResetSecret))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400（body：%s）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "加密登录") {
		t.Fatalf("body = %q，期望说明当前要求加密登录", rec.Body.String())
	}
	if passwordMatches(t, app, "alice", adminResetSecret) {
		t.Fatalf("明文口令被接受了")
	}
}

// TestAdminResetPasswordUnknownUser 覆盖目标不存在。
func TestAdminResetPasswordUnknownUser(t *testing.T) {
	app, _ := newAdminResetApp(t, "optional")

	rec := doAdminReset(t, app, 999999, fmt.Sprintf(`{"new_password":%q}`, adminResetSecret))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404（body：%s）", rec.Code, rec.Body.String())
	}
}
