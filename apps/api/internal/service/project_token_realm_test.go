package service

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/lau6code/lau6-cr/apps/api/internal/crypto"
	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// seedProjectToken 建一个项目 + 一枚项目令牌，返回原始令牌。
func seedProjectToken(t *testing.T, db *store.DB, projectName string, role domain.Role) string {
	t.Helper()
	p, err := db.CreateProject(context.Background(), projectName, projectName, "", domain.VisibilityPrivate, nil)
	if err != nil {
		t.Fatalf("创建项目: %v", err)
	}
	if _, err := db.CreateRepository(context.Background(), p.ID, "app", domain.VisibilityPublic, ""); err != nil {
		t.Fatalf("创建仓库: %v", err)
	}
	raw, hash, prefix, err := crypto.GenerateProjectToken()
	if err != nil {
		t.Fatalf("生成项目令牌: %v", err)
	}
	tok := &domain.ProjectToken{
		ProjectID:   p.ID,
		Name:        "CI",
		Role:        role,
		TokenHash:   hash,
		TokenPrefix: prefix,
	}
	if err := db.CreateProjectToken(context.Background(), tok); err != nil {
		t.Fatalf("落库项目令牌: %v", err)
	}
	return raw
}

// TestTokenRealmWithProjectToken 覆盖"docker login 用项目令牌换 JWT"。
func TestTokenRealmWithProjectToken(t *testing.T) {
	svc, db := newTestTokenService(t)
	raw := seedProjectToken(t, db, "team-a", domain.RoleDeveloper)

	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("任意用户名:"+raw))
	res, err := svc.Issue(context.Background(), basic, []string{"repository:team-a/app:pull,push"})
	if err != nil {
		t.Fatalf("用项目令牌换令牌: %v", err)
	}
	if !strings.HasPrefix(res.Subject, "project:team-a") {
		t.Fatalf("subject = %q，期望 project:team-a", res.Subject)
	}
	if len(res.Granted) != 1 {
		t.Fatalf("授予的访问 = %+v", res.Granted)
	}
	if res.Granted[0].Name != "team-a/app" {
		t.Fatalf("授予的仓库 = %q", res.Granted[0].Name)
	}
	// developer 角色 → push 必须在授予列表里。
	if !containsAction(res.Granted[0].Actions, "push") {
		t.Fatalf("动作 = %v，应含 push", res.Granted[0].Actions)
	}
}

// TestTokenRealmWithLegacyTokenPrefix 覆盖"旧前缀报明确原因"。
//
// 不能笼统地说"令牌无效"——那会让用户以为令牌被吊销了，
// 去查权限、翻审计，越查越找不到原因。
func TestTokenRealmWithLegacyTokenPrefix(t *testing.T) {
	svc, _ := newTestTokenService(t)

	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("admin:cr_pat_旧令牌"))
	_, err := svc.Issue(context.Background(), basic, []string{"repository:team-a/app:pull"})
	if !errors.Is(err, ErrLegacyTokenPrefix) {
		t.Fatalf("错误 = %v，期望 ErrLegacyTokenPrefix", err)
	}
	if !strings.Contains(err.Error(), "前缀已变更") {
		t.Fatalf("错误文案 = %q，应说清是前缀变更", err.Error())
	}
}

// TestTokenRealmProjectTokenCrossProjectDenied 覆盖"项目令牌换不到别的项目的令牌"。
func TestTokenRealmProjectTokenCrossProjectDenied(t *testing.T) {
	svc, db := newTestTokenService(t)
	raw := seedProjectToken(t, db, "team-a", domain.RoleDeveloper)
	if _, err := db.CreateProject(context.Background(), "team-b", "team-b", "", domain.VisibilityPublic, nil); err != nil {
		t.Fatal(err)
	}

	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("x:"+raw))
	res, err := svc.Issue(context.Background(), basic, []string{"repository:team-b/app:pull"})
	if err != nil {
		t.Fatalf("换令牌: %v", err)
	}
	if len(res.Granted) != 0 {
		t.Fatalf("跨项目不该授予访问，实际 %+v", res.Granted)
	}
}

// TestTokenRealmProjectTokenJWTVerifies 覆盖"换到的 JWT 能被验签"。
func TestTokenRealmProjectTokenJWTVerifies(t *testing.T) {
	svc, db := newTestTokenService(t)
	raw := seedProjectToken(t, db, "team-a", domain.RoleDeveloper)

	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("x:"+raw))
	res, err := svc.Issue(context.Background(), basic, []string{"repository:team-a/app:pull"})
	if err != nil {
		t.Fatal(err)
	}
	claims, err := svc.issuer.Verify(context.Background(), res.Token)
	if err != nil {
		t.Fatalf("校验换到的 JWT: %v", err)
	}
	if claims.TokenID == 0 {
		t.Fatal("JWT 应带 token_id")
	}
	if claims.Subject != res.Subject {
		t.Fatalf("JWT subject = %q，期望 %q", claims.Subject, res.Subject)
	}
}

// containsAction 判断动作列表里有没有某个动作。
func containsAction(actions []string, want string) bool {
	for _, a := range actions {
		if a == want {
			return true
		}
	}
	return false
}
