package crypto

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/cache"
	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
)

// newTestIssuer 造一个签发器 + 用户密钥库（都落在临时目录）。
func newTestIssuer(t *testing.T) (*TokenIssuer, *UserSecretStore) {
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
	issuer, err := NewTokenIssuer(ks, secrets, nil, "lau6-cr", "lau6-cr", func() time.Duration { return 5 * time.Minute })
	if err != nil {
		t.Fatalf("构造签发器: %v", err)
	}
	return issuer, secrets
}

// TestUserSecretEnsureIsStable 覆盖"同一个用户拿到的永远是同一把密钥"。
//
// 这条是整个模型的地基：密钥变了，他名下所有存量令牌会同时作废。
func TestUserSecretEnsureIsStable(t *testing.T) {
	ctx := context.Background()
	_, secrets := newTestIssuer(t)

	first, err := secrets.Ensure(ctx, "alice")
	if err != nil {
		t.Fatalf("生成密钥: %v", err)
	}
	second, err := secrets.Ensure(ctx, "alice")
	if err != nil {
		t.Fatalf("再次生成密钥: %v", err)
	}
	if first.Secret != second.Secret {
		t.Fatal("同一用户两次拿到的密钥不同——他的存量令牌会凭空作废")
	}
	other, err := secrets.Ensure(ctx, "bob")
	if err != nil {
		t.Fatalf("生成 bob 的密钥: %v", err)
	}
	if other.Secret == first.Secret {
		t.Fatal("不同用户拿到了同一把密钥——吊销一个人会连累所有人")
	}
}

// TestUserTokenUsesOwnSecret 覆盖用户令牌用**用户自己的**密钥签。
func TestUserTokenUsesOwnSecret(t *testing.T) {
	ctx := context.Background()
	issuer, _ := newTestIssuer(t)

	tok, err := issuer.IssueUser(ctx, "alice", []domain.Access{
		{Type: "repository", Name: "team-a/app", Actions: []string{"pull"}},
	})
	if err != nil {
		t.Fatalf("签发用户令牌: %v", err)
	}
	if alg := tokenAlgOf(t, tok); alg != "HS256" {
		t.Fatalf("用户令牌 alg = %q，期望 HS256", alg)
	}
	claims, err := issuer.Verify(ctx, tok)
	if err != nil {
		t.Fatalf("验签失败: %v", err)
	}
	if claims.Subject != "alice" {
		t.Fatalf("subject = %q，期望 alice", claims.Subject)
	}
	if !AllowsAccess(claims, "team-a/app", "pull") {
		t.Fatal("载荷里没有 team-a/app 的 pull 权限")
	}
}

// TestUserTokenRejectedAfterRevoke 覆盖"吊销密钥 → 存量令牌立刻作废"。
//
// 这是每用户一把密钥换来的核心能力：实例密钥只有一把时做不到。
func TestUserTokenRejectedAfterRevoke(t *testing.T) {
	ctx := context.Background()
	issuer, secrets := newTestIssuer(t)

	tok, err := issuer.IssueUser(ctx, "alice", nil)
	if err != nil {
		t.Fatalf("签发用户令牌: %v", err)
	}
	if _, err := issuer.Verify(ctx, tok); err != nil {
		t.Fatalf("吊销前应当验签通过: %v", err)
	}
	if err := secrets.Revoke(ctx, "alice"); err != nil {
		t.Fatalf("吊销密钥: %v", err)
	}
	if _, err := issuer.Verify(ctx, tok); err == nil {
		t.Fatal("密钥吊销后令牌仍然验签通过——吊销没生效")
	}
}

// TestUserTokenSubjectTamperRejected 覆盖"改 subject 换人用"。
//
// 载荷里写别人的用户名，签名就对不上了——用谁的名字就用谁的密钥。
func TestUserTokenSubjectTamperRejected(t *testing.T) {
	ctx := context.Background()
	issuer, _ := newTestIssuer(t)

	tok, err := issuer.IssueUser(ctx, "alice", nil)
	if err != nil {
		t.Fatalf("签发用户令牌: %v", err)
	}
	// alice 的令牌，把 subject 改成 bob。
	forged := rewriteSubject(t, tok, "bob")
	if _, err := issuer.Verify(ctx, forged); err == nil {
		t.Fatal("改写 subject 的令牌验签通过——可以冒用别人身份")
	}
}

// TestUserTokenAlgConfusionRejected 覆盖算法混淆。
//
// 客户端令牌是 HS256；如果允许 alg 降级/换算法，
// 攻击者就能拿自己的密钥伪造出别的东西。
func TestUserTokenAlgConfusionRejected(t *testing.T) {
	ctx := context.Background()
	issuer, _ := newTestIssuer(t)

	tok, err := issuer.IssueUser(ctx, "alice", nil)
	if err != nil {
		t.Fatalf("签发用户令牌: %v", err)
	}
	for _, alg := range []string{"none", "EdDSA", "HS512"} {
		forged := rewriteAlg(t, tok, alg)
		if _, err := issuer.Verify(ctx, forged); err == nil {
			t.Fatalf("alg=%s 的令牌验签通过——算法被放行了", alg)
		}
	}
}

// TestInstanceTokenStillVerifies 覆盖实例级令牌不受影响。
//
// 上游转发令牌与匿名令牌走的是 EdDSA，不能被用户级改造带偏。
func TestInstanceTokenStillVerifies(t *testing.T) {
	ctx := context.Background()
	issuer, _ := newTestIssuer(t)

	tok, err := issuer.Issue("lau6-cr-internal", []domain.Access{
		{Type: "repository", Name: "team-a/app", Actions: []string{"pull", "push"}},
	})
	if err != nil {
		t.Fatalf("签发实例令牌: %v", err)
	}
	if alg := tokenAlgOf(t, tok); alg != "EdDSA" {
		t.Fatalf("实例令牌 alg = %q，期望 EdDSA", alg)
	}
	claims, err := issuer.Verify(ctx, tok)
	if err != nil {
		t.Fatalf("验签失败: %v", err)
	}
	if claims.Subject != "lau6-cr-internal" {
		t.Fatalf("subject = %q", claims.Subject)
	}
}

// TestUserTokenExpiresNoLaterThanSecret 覆盖"令牌不活得比密钥久"。
//
// 密钥 30 天到期后，令牌也必须到期——否则会出现
// "令牌还在有效期内却验不过"的怪现象。
func TestUserTokenExpiresNoLaterThanSecret(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	ks, err := NewKeystore(Options{
		PrivatePath: filepath.Join(dir, "priv.pem"),
		JWKSPath:    filepath.Join(dir, "jwks.json"),
	})
	if err != nil {
		t.Fatalf("构造密钥库: %v", err)
	}
	// TTL 故意设成 1 小时，密钥 40 分钟前就创建了——
	// 模拟"密钥先于令牌创建"的场景。
	secrets := NewUserSecretStore(UserSecretOptions{
		Dir: filepath.Join(dir, "user-secrets"),
		TTL: func() time.Duration { return time.Hour },
	})
	issuer, err := NewTokenIssuer(ks, secrets, nil, "lau6-cr", "lau6-cr", func() time.Duration { return 5 * time.Minute })
	if err != nil {
		t.Fatalf("构造签发器: %v", err)
	}

	// 先造一条 40 分钟前创建的密钥记录。
	old := UserSecret{Secret: "0123456789abcdef0123456789abcdef", CreatedAt: time.Now().Add(-40 * time.Minute)}
	b := &fileSecretBackend[string, UserSecret]{dir: filepath.Join(dir, "user-secrets"), ttl: time.Hour, spec: userSecretSpec{}}
	if _, err := b.Create(ctx, "alice", old, time.Hour); err != nil {
		t.Fatalf("预置密钥: %v", err)
	}

	tok, err := issuer.IssueUser(ctx, "alice", nil)
	if err != nil {
		t.Fatalf("签发用户令牌: %v", err)
	}
	claims, err := issuer.Verify(ctx, tok)
	if err != nil {
		t.Fatalf("验签失败: %v", err)
	}
	limit := old.CreatedAt.Add(time.Hour)
	if claims.ExpiresAt == nil || claims.ExpiresAt.Time.After(limit) {
		t.Fatalf("令牌有效期 %v 超过密钥寿命上限 %v", claims.ExpiresAt, limit)
	}
}

// TestUserSecretRedisBackend 覆盖 redis 模式：缓存是权威源。
func TestUserSecretRedisBackend(t *testing.T) {
	ctx := context.Background()
	c := newFakeRedis()
	holder := cache.NewHolder(c)
	secrets := NewUserSecretStore(UserSecretOptions{
		Cache: holder,
		Dir:   t.TempDir(),
	})

	rec, err := secrets.Ensure(ctx, "alice")
	if err != nil {
		t.Fatalf("生成密钥: %v", err)
	}
	raw, ok, err := c.Get(ctx, CacheKeyUserSecretPrefix+"alice")
	if err != nil || !ok {
		t.Fatalf("缓存里没有用户密钥: ok=%v err=%v", ok, err)
	}
	var stored UserSecret
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("解析缓存里的用户密钥: %v", err)
	}
	if stored.Secret != rec.Secret {
		t.Fatal("缓存里的密钥与返回的不一致")
	}

	// 吊销后缓存里必须没有。
	if err := secrets.Revoke(ctx, "alice"); err != nil {
		t.Fatalf("吊销: %v", err)
	}
	if _, ok, err := c.Get(ctx, CacheKeyUserSecretPrefix+"alice"); err != nil || ok {
		t.Fatalf("吊销后缓存里还有密钥: ok=%v err=%v", ok, err)
	}
}

// TestUserSecretFilePermissions 覆盖文件后端的权限。
//
// 用户密钥等同私钥：必须是 0600，目录 0700。
func TestUserSecretFilePermissions(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "user-secrets")
	secrets := NewUserSecretStore(UserSecretOptions{Dir: dir})
	if _, err := secrets.Ensure(ctx, "alice"); err != nil {
		t.Fatalf("生成密钥: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读目录: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("目录里有 %d 个文件，期望 1 个", len(entries))
	}
	info, err := entries[0].Info()
	if err != nil {
		t.Fatalf("读文件信息: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("用户密钥文件权限 = %o，期望 600", perm)
	}
	// 文件名不能是用户名本身（LDAP 用户名可能是任意字符串）。
	if strings.Contains(entries[0].Name(), "alice") {
		t.Fatalf("文件名泄露了用户名: %s", entries[0].Name())
	}
}

// TestUserSecretMigrateKeepsSecret 覆盖换缓存后端时搬迁用户密钥。
//
// 不搬的后果：切到 redis 后该用户的存量令牌全部作废。
func TestUserSecretMigrateKeepsSecret(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	secrets := NewUserSecretStore(UserSecretOptions{Dir: filepath.Join(dir, "user-secrets")})

	before, err := secrets.Ensure(ctx, "alice")
	if err != nil {
		t.Fatalf("生成密钥: %v", err)
	}

	fresh := newFakeRedis()
	moved, err := secrets.Migrate(ctx, nil, fresh, []string{"alice", "nobody"})
	if err != nil {
		t.Fatalf("搬迁: %v", err)
	}
	if moved != 1 {
		t.Fatalf("搬了 %d 个密钥，期望 1 个", moved)
	}
	raw, ok, err := fresh.Get(ctx, CacheKeyUserSecretPrefix+"alice")
	if err != nil || !ok {
		t.Fatalf("新后端里没有密钥: ok=%v err=%v", ok, err)
	}
	var stored UserSecret
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if stored.Secret != before.Secret {
		t.Fatal("搬迁后密钥变了——该用户的存量令牌会作废")
	}
}

// tokenAlgOf 取 JWT 头里的 alg。
func tokenAlgOf(t *testing.T, raw string) string {
	t.Helper()
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT 结构不对: %q", raw)
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

// rewriteSubject 改写令牌载荷里的 subject（签名保持不变——用来验证验签能抓住）。
func rewriteSubject(t *testing.T, raw, subject string) string {
	t.Helper()
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT 结构不对: %q", raw)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("解载荷: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("解载荷 JSON: %v", err)
	}
	claims["sub"] = subject
	out, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("编码载荷: %v", err)
	}
	return parts[0] + "." + base64.RawURLEncoding.EncodeToString(out) + "." + parts[2]
}

// rewriteAlg 改写令牌头里的 alg（签名保持不变）。
func rewriteAlg(t *testing.T, raw, alg string) string {
	t.Helper()
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT 结构不对: %q", raw)
	}
	head, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("解 JWT 头: %v", err)
	}
	var h map[string]any
	if err := json.Unmarshal(head, &h); err != nil {
		t.Fatalf("解 JWT 头 JSON: %v", err)
	}
	h["alg"] = alg
	out, err := json.Marshal(h)
	if err != nil {
		t.Fatalf("编码 JWT 头: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(out) + "." + parts[1] + "." + parts[2]
}
