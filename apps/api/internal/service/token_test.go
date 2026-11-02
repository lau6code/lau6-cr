package service

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/crypto"
	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
	"golang.org/x/crypto/bcrypt"
)

// newTestProjectTokenIssuer 造一个带项目令牌密钥库的签发器。
//
// 返回的密钥库必须与 ProjectTokenService 用的是**同一个实例**——
// 签发时写进去、吊销时删掉的是同一把钥匙，两个实例就等于吊销失效。
func newTestProjectTokenIssuer(t *testing.T) (*crypto.TokenIssuer, *crypto.ProjectSecretStore) {
	t.Helper()
	dir := t.TempDir()
	ks, err := crypto.NewKeystore(crypto.Options{
		PrivatePath: filepath.Join(dir, "priv.pem"),
		JWKSPath:    filepath.Join(dir, "jwks.json"),
	})
	if err != nil {
		t.Fatalf("构造密钥库: %v", err)
	}
	secrets := crypto.NewUserSecretStore(crypto.UserSecretOptions{Dir: filepath.Join(dir, "user-secrets")})
	projectSecrets := crypto.NewProjectSecretStore(crypto.ProjectSecretOptions{Dir: filepath.Join(dir, "project-secrets")})
	issuer, err := crypto.NewTokenIssuer(ks, secrets, projectSecrets, "lau6-cr", "lau6-cr", func() time.Duration {
		return 5 * time.Minute
	})
	if err != nil {
		t.Fatalf("构造签发器: %v", err)
	}
	return issuer, projectSecrets
}

// newTestTokenService 造一个 token 服务（需要一个能校验口令的 AuthService）。
func newTestTokenService(t *testing.T) (*TokenService, *store.DB) {
	t.Helper()
	db := newTestDB(t)
	ctx := context.Background()
	// 造一个账号，bcrypt 散列用固定值（校验在 AuthService 里做）。
	if _, err := db.CreateUser(ctx, "admin", mustHash(t, "VerifyPass123!"), "local", "", true); err != nil {
		t.Fatalf("创建用户: %v", err)
	}
	auth := NewAuthService(db, AuthOptions{})
	pats := NewPersonalTokenService(db, PersonalTokenOptions{})
	issuer, projectSecrets := newTestProjectTokenIssuer(t)
	projTokens := NewProjectTokenService(db, ProjectTokenOptions{Secrets: projectSecrets})
	perm := NewPermissionEngine(db, PermissionOptions{AnonymousReadEnabled: func() bool { return true }})
	return NewTokenService(perm, issuer, auth, pats, projTokens), db
}

// mustHash 生成 bcrypt 散列。
func mustHash(t *testing.T, pw string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("散列口令: %v", err)
	}
	return string(h)
}

// seedPublicRepo 建一个公开项目 + 公开仓库 <project>/app。
func seedPublicRepo(t *testing.T, db *store.DB, projectName string) {
	t.Helper()
	ctx := context.Background()
	p, err := db.CreateProject(ctx, projectName, projectName, "", domain.VisibilityPublic, nil)
	if err != nil {
		t.Fatalf("创建项目: %v", err)
	}
	if _, err := db.CreateRepository(ctx, p.ID, "app", domain.VisibilityPublic, ""); err != nil {
		t.Fatalf("创建仓库: %v", err)
	}
}

// TestTokenIssueRejectsMissingProject 覆盖"认证请求遇到不存在的项目 → 明确错误"。
//
// ⚠️ 为什么不能只签空 access 就算了：
// 客户端拿到空 access 后会在 /v2/ 上**反复重试**（每次 403），
// 运维看到的是一串 403 日志，看不出真正原因。
// 在换 token 这一步就拒掉，客户端立刻带着明确信息停下。
func TestTokenIssueRejectsMissingProject(t *testing.T) {
	svc, _ := newTestTokenService(t)

	_, err := svc.Issue(context.Background(), "Basic YWRtaW46VmVyaWZ5UGFzczEyMyE=",
		[]string{"repository:rustdesk/rustdesk-server:pull,push"})
	if err == nil {
		t.Fatalf("项目不存在时应当返回错误")
	}
	var pnf *ProjectNotFoundError
	if !errors.As(err, &pnf) {
		t.Fatalf("错误类型 = %T，期望 *ProjectNotFoundError", err)
	}
	if pnf.Project != "rustdesk" {
		t.Fatalf("项目名 = %q，期望 rustdesk", pnf.Project)
	}
	if pnf.Error() != "未经授权: 项目 rustdesk 不存在" {
		t.Fatalf("错误文案 = %q", pnf.Error())
	}
}

// TestTokenIssueAnonymousDoesNotRevealMissingProject 覆盖"匿名请求不暴露项目是否存在"。
//
// 匿名是**未认证攻击面**：区分"项目不存在"与"无权限"
// 就能用来探测私有项目名。
//
// ⚠️ 匿名申请**写权限**（push/delete）时直接 401（错误文案里**不含仓库名**）：
//
//	匿名能不能 push 与仓库存不存在无关，两种情况都是同一个 401，不泄露。
//
// 匿名申请**只读权限**时签空 access（不报错）。
func TestTokenIssueAnonymousDoesNotRevealMissingProject(t *testing.T) {
	svc, _ := newTestTokenService(t)

	// 匿名申请写权限 → 401，且错误文案不含仓库/项目名。
	_, err := svc.Issue(context.Background(), "",
		[]string{"repository:rustdesk/rustdesk-server:pull,push"})
	var aws *AnonymousWriteScopeError
	if !errors.As(err, &aws) {
		t.Fatalf("匿名申请写权限应当被拒，实际错误 = %v", err)
	}
	if strings.Contains(err.Error(), "rustdesk") {
		t.Fatalf("错误文案泄露了仓库/项目名：%q", err.Error())
	}

	// 匿名申请只读权限 → 仍签发（空 access），不暴露项目是否存在。
	res, err := svc.Issue(context.Background(), "",
		[]string{"repository:rustdesk/rustdesk-server:pull"})
	if err != nil {
		t.Fatalf("匿名只读请求不该报错: %v", err)
	}
	if len(res.Granted) != 0 {
		t.Fatalf("匿名不该拿到任何授权，实际 %v", res.Granted)
	}
}

// TestTokenIssueAnonymousPullOnPublicRepo 锁定"公开仓库允许未登录用户拉取"。
//
// 这是"公开"的字面含义（02-auth.md §4.5）：匿名 token 必须拿到 pull。
// 写权限被拒**不能**连累读权限——否则公开面就形同虚设。
func TestTokenIssueAnonymousPullOnPublicRepo(t *testing.T) {
	svc, db := newTestTokenService(t)
	ctx := context.Background()

	seedPublicRepo(t, db, "team-a")

	res, err := svc.Issue(ctx, "", []string{"repository:team-a/app:pull"})
	if err != nil {
		t.Fatalf("匿名拉取公开仓库不该报错: %v", err)
	}
	if len(res.Granted) != 1 {
		t.Fatalf("匿名该拿到 1 条 pull 授权，实际 %v", res.Granted)
	}
	if res.Granted[0].Name != "team-a/app" {
		t.Fatalf("授予的仓库 = %q", res.Granted[0].Name)
	}
	for _, act := range res.Granted[0].Actions {
		if act != string(ActionPull) {
			t.Fatalf("匿名只该拿到 pull，实际动作 %q", act)
		}
	}
}

// TestTokenIssueAnonymousWriteScopeRejected 覆盖"匿名申请写权限 → 拒"。
//
// 这条是**止住重试循环**的关键（见 token.go 的说明）：
// 签一枚降级 token 会让客户端拿着它反复推 → 401 → 再换 token → 死循环。
func TestTokenIssueAnonymousWriteScopeRejected(t *testing.T) {
	svc, db := newTestTokenService(t)
	seedPublicRepo(t, db, "team-a")

	cases := []string{
		"repository:team-a/app:push",
		"repository:team-a/app:delete",
		"repository:team-a/app:pull,push",
	}
	for _, scope := range cases {
		_, err := svc.Issue(context.Background(), "", []string{scope})
		var aws *AnonymousWriteScopeError
		if !errors.As(err, &aws) {
			t.Fatalf("匿名申请 %q 应当被拒，实际错误 = %v", scope, err)
		}
	}
}

// TestTokenIssueAllowsExistingProject 覆盖"项目存在时正常签发"。
func TestTokenIssueAllowsExistingProject(t *testing.T) {
	svc, db := newTestTokenService(t)
	seedProjectOnly(t, db, "team-a")

	res, err := svc.Issue(context.Background(), "Basic YWRtaW46VmVyaWZ5UGFzczEyMyE=",
		[]string{"repository:team-a/app:pull,push"})
	if err != nil {
		t.Fatalf("项目存在时不该报错: %v", err)
	}
	if len(res.Granted) != 1 {
		t.Fatalf("授予 = %v，期望 1 条", res.Granted)
	}
	if res.Granted[0].Name != "team-a/app" {
		t.Fatalf("授予的仓库 = %q", res.Granted[0].Name)
	}
}

// TestTokenIssueUsesUserSecretForLoggedIn 覆盖"已登录 → 用户级令牌（HS256）"。
//
// 已登录用户的令牌用**他自己**的密钥签——
// 这样吊销一个人的密钥就能只作废他一个人的令牌。
func TestTokenIssueUsesUserSecretForLoggedIn(t *testing.T) {
	svc, db := newTestTokenService(t)
	seedProjectOnly(t, db, "team-a")

	res, err := svc.Issue(context.Background(), "Basic YWRtaW46VmVyaWZ5UGFzczEyMyE=",
		[]string{"repository:team-a/app:pull,push"})
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if alg := tokenAlg(t, res.Token); alg != "HS256" {
		t.Fatalf("已登录用户的令牌 alg = %q，期望 HS256", alg)
	}
	if res.Subject != "admin" {
		t.Fatalf("subject = %q，期望 admin", res.Subject)
	}
	// 有效期是 30 天（用户级），不是上游转发用的 5 分钟。
	if res.ExpiresIn != int(crypto.DefaultUserSecretTTL/time.Second) {
		t.Fatalf("有效期 = %d 秒，期望 %d 秒（30 天）",
			res.ExpiresIn, int(crypto.DefaultUserSecretTTL/time.Second))
	}
}

// TestTokenIssueAnonymousUsesInstanceKey 覆盖"匿名 → 实例级令牌（EdDSA）"。
//
// 匿名没有用户身份，没有"每用户一把密钥"可言；
// 用实例密钥签还能少一次缓存往返。
func TestTokenIssueAnonymousUsesInstanceKey(t *testing.T) {
	svc, db := newTestTokenService(t)
	seedProjectOnly(t, db, "team-a")

	res, err := svc.Issue(context.Background(), "", []string{"repository:team-a/app:pull"})
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if alg := tokenAlg(t, res.Token); alg != "EdDSA" {
		t.Fatalf("匿名令牌的 alg = %q，期望 EdDSA", alg)
	}
	if res.Subject != "anonymous" {
		t.Fatalf("subject = %q，期望 anonymous", res.Subject)
	}
}
