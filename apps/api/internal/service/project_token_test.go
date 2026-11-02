package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/crypto"
	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// newTestProjectTokenService 造一个项目令牌服务（带签名密钥库）。
func newTestProjectTokenService(t *testing.T) (*ProjectTokenService, *store.DB) {
	t.Helper()
	db := newTestDB(t)
	secrets := crypto.NewProjectSecretStore(crypto.ProjectSecretOptions{Dir: t.TempDir()})
	return NewProjectTokenService(db, ProjectTokenOptions{Secrets: secrets}), db
}

// newTestProject 建一个项目，返回它的 ID。
func newTestProjectTokenProject(t *testing.T, db *store.DB, name string) int64 {
	t.Helper()
	p, err := db.CreateProject(context.Background(), name, "项目", "", domain.VisibilityPrivate, nil)
	if err != nil {
		t.Fatalf("建项目 %s: %v", name, err)
	}
	return p.ID
}

// TestProjectTokenCreateAndVerify 覆盖"创建 → 校验"。
func TestProjectTokenCreateAndVerify(t *testing.T) {
	ctx := context.Background()
	svc, db := newTestProjectTokenService(t)
	projectID := newTestProjectTokenProject(t, db, "team-a")

	raw, tok, err := svc.Create(ctx, projectID, 0, "CI 推送", domain.RoleDeveloper, ExpiryPolicy{})
	if err != nil {
		t.Fatalf("生成项目令牌: %v", err)
	}
	if !strings.HasPrefix(raw, crypto.ProjectTokenPrefix) {
		t.Fatalf("令牌前缀 = %q，期望 %q", raw, crypto.ProjectTokenPrefix)
	}
	if tok.Role != domain.RoleDeveloper {
		t.Fatalf("角色 = %q，期望 developer", tok.Role)
	}

	got, err := svc.Verify(ctx, raw)
	if err != nil {
		t.Fatalf("校验项目令牌: %v", err)
	}
	if got.ID != tok.ID {
		t.Fatalf("ID = %d，期望 %d", got.ID, tok.ID)
	}
	if got.Role != domain.RoleDeveloper {
		t.Fatalf("角色 = %q，期望 developer", got.Role)
	}
}

// TestProjectTokenCreateRejectsInvalidRole 覆盖"角色是权威闸门"。
//
// MySQL 上 CHECK 约束可能不被执行，光靠数据库约束等于没有约束。
func TestProjectTokenCreateRejectsInvalidRole(t *testing.T) {
	ctx := context.Background()
	svc, db := newTestProjectTokenService(t)
	projectID := newTestProjectTokenProject(t, db, "team-a")

	for _, role := range []domain.Role{"", "owner", "Admin", "admin "} {
		if _, _, err := svc.Create(ctx, projectID, 0, "x", role, ExpiryPolicy{}); err == nil {
			t.Fatalf("角色 %q 竟然被接受了", role)
		}
	}
	for _, role := range []domain.Role{domain.RoleAdmin, domain.RoleDeveloper, domain.RoleReader} {
		if _, _, err := svc.Create(ctx, projectID, 0, "x", role, ExpiryPolicy{}); err != nil {
			t.Fatalf("角色 %q 应被接受: %v", role, err)
		}
	}
}

// TestProjectTokenExpiryPolicies 覆盖"失效策略三选一"。
func TestProjectTokenExpiryPolicies(t *testing.T) {
	ctx := context.Background()
	svc, db := newTestProjectTokenService(t)
	projectID := newTestProjectTokenProject(t, db, "team-a")

	// 永久
	_, tok, err := svc.Create(ctx, projectID, 0, "永久", domain.RoleReader, ExpiryPolicy{NeverExpires: true})
	if err != nil {
		t.Fatal(err)
	}
	if tok.ExpiresAt != nil {
		t.Fatalf("永久令牌的 expires_at = %v，应为 nil", *tok.ExpiresAt)
	}

	// 指定时刻
	at := store.Now() + 86400
	_, tok, err = svc.Create(ctx, projectID, 0, "定时", domain.RoleReader, ExpiryPolicy{ExpiresAt: &at})
	if err != nil {
		t.Fatal(err)
	}
	if tok.ExpiresAt == nil || *tok.ExpiresAt != at {
		t.Fatalf("定时令牌的 expires_at = %v，期望 %d", tok.ExpiresAt, at)
	}

	// 系统默认
	_, tok, err = svc.Create(ctx, projectID, 0, "默认", domain.RoleReader, ExpiryPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if tok.ExpiresAt == nil {
		t.Fatal("默认令牌的 expires_at 不应为 nil")
	}
	want := store.Now() + int64(svc.TTL()/time.Second)
	if diff := *tok.ExpiresAt - want; diff < -5 || diff > 5 {
		t.Fatalf("默认失效时间 = %d，期望约 %d", *tok.ExpiresAt, want)
	}

	// 矛盾请求
	if _, _, err := svc.Create(ctx, projectID, 0, "矛盾", domain.RoleReader, ExpiryPolicy{
		NeverExpires: true, ExpiresAt: &at,
	}); err == nil {
		t.Fatal("永久 + 指定时刻应报错")
	}

	// 过去的时刻
	past := store.Now() - 10
	if _, _, err := svc.Create(ctx, projectID, 0, "过期", domain.RoleReader, ExpiryPolicy{ExpiresAt: &past}); err == nil {
		t.Fatal("失效时间在过去应报错")
	}
}

// TestProjectTokenRoleIsImmutable 覆盖"角色不可改"。
func TestProjectTokenRoleIsImmutable(t *testing.T) {
	ctx := context.Background()
	svc, db := newTestProjectTokenService(t)
	projectID := newTestProjectTokenProject(t, db, "team-a")

	raw, tok, err := svc.Create(ctx, projectID, 0, "CI", domain.RoleReader, ExpiryPolicy{})
	if err != nil {
		t.Fatal(err)
	}

	// 改期只动 expires_at，角色不动。
	at := store.Now() + 3600
	if err := svc.SetExpiry(ctx, tok.ID, projectID, ExpiryPolicy{ExpiresAt: &at}); err != nil {
		t.Fatalf("改期: %v", err)
	}
	got, err := svc.Verify(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.Role != domain.RoleReader {
		t.Fatalf("改期后角色变成了 %q", got.Role)
	}
	if got.ExpiresAt == nil || *got.ExpiresAt != at {
		t.Fatalf("改期后 expires_at = %v，期望 %d", got.ExpiresAt, at)
	}
}

// TestProjectTokenScopeIsolation 覆盖"令牌只属于自己那个项目"。
func TestProjectTokenScopeIsolation(t *testing.T) {
	ctx := context.Background()
	svc, db := newTestProjectTokenService(t)
	projectA := newTestProjectTokenProject(t, db, "team-a")
	projectB := newTestProjectTokenProject(t, db, "team-b")

	_, tok, err := svc.Create(ctx, projectA, 0, "CI", domain.RoleDeveloper, ExpiryPolicy{})
	if err != nil {
		t.Fatal(err)
	}

	at := store.Now() + 3600
	if err := svc.SetExpiry(ctx, tok.ID, projectB, ExpiryPolicy{ExpiresAt: &at}); !errors.Is(err, ErrInvalidProjectToken) {
		t.Fatalf("跨项目改期应返回 ErrInvalidProjectToken，实际 %v", err)
	}
	if err := svc.Revoke(ctx, tok.ID, projectB); !errors.Is(err, ErrInvalidProjectToken) {
		t.Fatalf("跨项目吊销应返回 ErrInvalidProjectToken，实际 %v", err)
	}
}

// TestProjectTokenVerifyRejectsInvalid 覆盖"无效令牌一律拒绝"。
func TestProjectTokenVerifyRejectsInvalid(t *testing.T) {
	ctx := context.Background()
	svc, db := newTestProjectTokenService(t)
	newTestProjectTokenProject(t, db, "team-a")

	for _, bad := range []string{
		"",
		"lau6_p_不是一枚真令牌",
		"lau6_u_个人令牌不是项目令牌",
		"cr_pat_旧令牌",
	} {
		if _, err := svc.Verify(ctx, bad); !errors.Is(err, ErrInvalidProjectToken) {
			t.Fatalf("令牌 %q 应被拒绝，实际 %v", bad, err)
		}
	}
}

// TestProjectTokenRevokeRemovesSigningKey 覆盖"吊销立即生效"。
//
// 删库行不够：那枚令牌换发出去的 JWT 是用密钥验签的，
// 密钥还在就照样能用。
func TestProjectTokenRevokeRemovesSigningKey(t *testing.T) {
	ctx := context.Background()
	svc, db := newTestProjectTokenService(t)
	projectID := newTestProjectTokenProject(t, db, "team-a")

	raw, tok, err := svc.Create(ctx, projectID, 0, "CI", domain.RoleDeveloper, ExpiryPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	// 吊销：删库行 + 删签名密钥。
	if err := svc.Revoke(ctx, tok.ID, projectID); err != nil {
		t.Fatalf("吊销: %v", err)
	}
	if _, err := svc.Verify(ctx, raw); !errors.Is(err, ErrInvalidProjectToken) {
		t.Fatalf("吊销后原始令牌应失效，实际 %v", err)
	}
	// 密钥必须也没了。
	if _, ok, err := svc.secrets.Get(ctx, tok.ID); err != nil || ok {
		t.Fatalf("吊销后签名密钥应被删除: ok=%v err=%v", ok, err)
	}
}

// TestProjectTokenRevokeAll 覆盖"整项目吊销"。
func TestProjectTokenRevokeAll(t *testing.T) {
	ctx := context.Background()
	svc, db := newTestProjectTokenService(t)
	projectA := newTestProjectTokenProject(t, db, "team-a")
	projectB := newTestProjectTokenProject(t, db, "team-b")

	for _, spec := range []struct {
		project int64
		hash    string
	}{{projectA, "a"}, {projectA, "b"}, {projectB, "c"}} {
		if _, _, err := svc.Create(ctx, spec.project, 0, "x", domain.RoleReader, ExpiryPolicy{}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := svc.RevokeAll(ctx, projectA)
	if err != nil {
		t.Fatalf("整项目吊销: %v", err)
	}
	if n != 2 {
		t.Fatalf("吊销数 = %d，期望 2", n)
	}
	list, err := svc.List(ctx, projectB)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("项目 B 的令牌被误删：剩余 %d 枚", len(list))
	}
}
