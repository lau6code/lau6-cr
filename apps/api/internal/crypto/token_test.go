package crypto

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
)

// newDynamicIssuer 造一个 TTL 可变的签发器。
func newDynamicIssuer(t *testing.T, ttl func() time.Duration) *TokenIssuer {
	t.Helper()
	dir := t.TempDir()
	ks, err := NewKeystore(Options{
		PrivatePath: filepath.Join(dir, "priv.pem"),
		JWKSPath:    filepath.Join(dir, "jwks.json"),
	})
	if err != nil {
		t.Fatalf("构造密钥库: %v", err)
	}
	secrets := NewUserSecretStore(UserSecretOptions{Dir: filepath.Join(dir, "user-secrets")})
	issuer, err := NewTokenIssuer(ks, secrets, nil, "lau6-cr", "lau6-cr", ttl)
	if err != nil {
		t.Fatalf("构造签发器: %v", err)
	}
	return issuer
}

// TestTokenIssuerTTLIsDynamic 覆盖"令牌 TTL 跟着设置走"。
//
// core.token_ttl_seconds 是 T0 热生效设置：
// 启动时把窗口冻住的话，设置页能改、能显示"已保存"，运行期却还是旧窗口。
func TestTokenIssuerTTLIsDynamic(t *testing.T) {
	ttl := 5 * time.Minute
	issuer := newDynamicIssuer(t, func() time.Duration { return ttl })

	if got := issuer.TTL(); got != 5*time.Minute {
		t.Fatalf("初始 TTL = %s，期望 5m", got)
	}
	if got := issuer.ExpiresIn(); got != 300 {
		t.Fatalf("初始 ExpiresIn = %d，期望 300", got)
	}

	// 运行期改设置 → 立刻生效。
	ttl = 10 * time.Minute
	if got := issuer.TTL(); got != 10*time.Minute {
		t.Errorf("改设置后的 TTL = %s，期望 10m（必须跟着当前设置走）", got)
	}
	if got := issuer.ExpiresIn(); got != 600 {
		t.Errorf("改设置后的 ExpiresIn = %d，期望 600", got)
	}
}

// TestTokenIssuerRejectsTooShortTTL 覆盖"窗口过短要当场报错"。
//
// distribution 会拒绝过短窗口。与其发一枚必然被拒的令牌，
// 不如让签发失败——那才是"失败暴露到调用方"。
func TestTokenIssuerRejectsTooShortTTL(t *testing.T) {
	ttl := 5 * time.Minute
	issuer := newDynamicIssuer(t, func() time.Duration { return ttl })

	// 运行期把窗口改到 1 分钟以下。
	ttl = 30 * time.Second
	if _, err := issuer.Issue("alice", nil); err == nil {
		t.Fatal("窗口小于 1 分钟时必须拒绝签发，不能发出必然被上游拒绝的令牌")
	}
}

// TestTokenIssuerRejectsBadTTLAtStartup 覆盖"构造时就拒掉非法窗口"。
//
// 配置写错要在启动时暴露，而不是等第一次签发才发现。
func TestTokenIssuerRejectsBadTTLAtStartup(t *testing.T) {
	dir := t.TempDir()
	ks, err := NewKeystore(Options{
		PrivatePath: filepath.Join(dir, "priv.pem"),
		JWKSPath:    filepath.Join(dir, "jwks.json"),
	})
	if err != nil {
		t.Fatalf("构造密钥库: %v", err)
	}
	secrets := NewUserSecretStore(UserSecretOptions{Dir: filepath.Join(dir, "user-secrets")})

	if _, err := NewTokenIssuer(ks, secrets, nil, "lau6-cr", "lau6-cr", func() time.Duration {
		return 10 * time.Second
	}); err == nil {
		t.Fatal("构造时窗口不合法必须报错")
	}
	if _, err := NewTokenIssuer(ks, secrets, nil, "lau6-cr", "lau6-cr", nil); err == nil {
		t.Fatal("TTL 未配置必须报错，不能用默认值兜底")
	}
}

// newProjectIssuer 造一个带项目令牌密钥库的签发器。
func newProjectIssuer(t *testing.T) *TokenIssuer {
	t.Helper()
	dir := t.TempDir()
	ks, err := NewKeystore(Options{
		PrivatePath: filepath.Join(dir, "priv.pem"),
		JWKSPath:    filepath.Join(dir, "jwks.json"),
	})
	if err != nil {
		t.Fatalf("构造密钥库: %v", err)
	}
	secrets := NewUserSecretStore(UserSecretOptions{Dir: filepath.Join(dir, "user-secrets")})
	projectSecrets := NewProjectSecretStore(ProjectSecretOptions{Dir: filepath.Join(dir, "project-secrets")})
	issuer, err := NewTokenIssuer(ks, secrets, projectSecrets, "lau6-cr", "lau6-cr", func() time.Duration {
		return 5 * time.Minute
	})
	if err != nil {
		t.Fatalf("构造签发器: %v", err)
	}
	return issuer
}

// TestIssueProjectTokenUsesItsOwnKey 覆盖"项目令牌用自己的密钥签"。
func TestIssueProjectTokenUsesItsOwnKey(t *testing.T) {
	issuer := newProjectIssuer(t)
	ctx := context.Background()

	tok, err := issuer.IssueProject(ctx, 7, "project:team-a", []domain.Access{{
		Type: "repository", Name: "team-a/app", Actions: []string{"pull"},
	}})
	if err != nil {
		t.Fatalf("签发项目令牌: %v", err)
	}
	claims, err := issuer.Verify(ctx, tok)
	if err != nil {
		t.Fatalf("校验项目令牌: %v", err)
	}
	if claims.TokenID != 7 {
		t.Fatalf("claims.TokenID = %d，期望 7", claims.TokenID)
	}
	if claims.Subject != "project:team-a" {
		t.Fatalf("claims.Subject = %q，期望 project:team-a", claims.Subject)
	}
	if len(claims.Access) != 1 || claims.Access[0].Name != "team-a/app" {
		t.Fatalf("claims.Access = %+v", claims.Access)
	}
}

// TestProjectTokenCannotBeVerifiedWithAnotherTokensKey 覆盖"密钥不通用"。
//
// 把 token_id 改成别的令牌的 ID、但仍用**原来那把**密钥重签，必须验不过——
// 否则一枚令牌就能冒充另一枚令牌。
func TestProjectTokenCannotBeVerifiedWithAnotherTokensKey(t *testing.T) {
	issuer := newProjectIssuer(t)
	ctx := context.Background()

	tok, err := issuer.IssueProject(ctx, 1, "project:team-a", nil)
	if err != nil {
		t.Fatalf("签发项目令牌: %v", err)
	}
	claims, err := issuer.Verify(ctx, tok)
	if err != nil {
		t.Fatalf("校验项目令牌: %v", err)
	}

	// 保留令牌 1 的密钥，把 token_id 改成 2 后重签。
	rec1, err := issuer.projectSecrets.Ensure(ctx, 1)
	if err != nil {
		t.Fatalf("取令牌 1 的密钥: %v", err)
	}
	claims.TokenID = 2
	tampered := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tampered.SignedString([]byte(rec1.Secret))
	if err != nil {
		t.Fatalf("重签: %v", err)
	}
	if _, err := issuer.Verify(ctx, signed); err == nil {
		t.Fatal("用令牌 1 的密钥冒充令牌 2 的令牌竟然验过了")
	}
}

// TestProjectTokenRejectedAfterKeyRevoke 覆盖"吊销立即生效"。
func TestProjectTokenRejectedAfterKeyRevoke(t *testing.T) {
	issuer := newProjectIssuer(t)
	ctx := context.Background()

	tok, err := issuer.IssueProject(ctx, 3, "project:team-a", nil)
	if err != nil {
		t.Fatalf("签发项目令牌: %v", err)
	}
	if _, err := issuer.Verify(ctx, tok); err != nil {
		t.Fatalf("吊销前应能验过: %v", err)
	}
	if err := issuer.projectSecrets.Revoke(ctx, 3); err != nil {
		t.Fatalf("吊销密钥: %v", err)
	}
	if _, err := issuer.Verify(ctx, tok); err == nil {
		t.Fatal("吊销后竟然还能验过")
	}
}

// TestIssueProjectTokenWithoutKeyStoreFails 覆盖"密钥库没接线就报错"。
//
// 不给默认值：缺依赖要在调用点暴露，而不是悄悄发一枚验不过的令牌。
func TestIssueProjectTokenWithoutKeyStoreFails(t *testing.T) {
	dir := t.TempDir()
	ks, err := NewKeystore(Options{
		PrivatePath: filepath.Join(dir, "priv.pem"),
		JWKSPath:    filepath.Join(dir, "jwks.json"),
	})
	if err != nil {
		t.Fatalf("构造密钥库: %v", err)
	}
	secrets := NewUserSecretStore(UserSecretOptions{Dir: filepath.Join(dir, "user-secrets")})
	issuer, err := NewTokenIssuer(ks, secrets, nil, "lau6-cr", "lau6-cr", func() time.Duration {
		return 5 * time.Minute
	})
	if err != nil {
		t.Fatalf("构造签发器: %v", err)
	}
	if _, err := issuer.IssueProject(context.Background(), 1, "project:team-a", nil); err == nil {
		t.Fatal("密钥库未接线时签发项目令牌应报错")
	}
}
