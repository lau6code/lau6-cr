package store

import (
	"context"
	"testing"

	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
)

// newTestProject 建一个项目，返回它的 ID。
func newTestProject(t *testing.T, db *DB, name string) int64 {
	t.Helper()
	p, err := db.CreateProject(context.Background(), name, "项目", "", domain.VisibilityPrivate, nil)
	if err != nil {
		t.Fatalf("建项目 %s: %v", name, err)
	}
	return p.ID
}

// TestProjectTokenCRUD 覆盖项目令牌的完整存取生命周期。
func TestProjectTokenCRUD(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	projectID := newTestProject(t, db, "team-a")

	tok := &domain.ProjectToken{
		ProjectID:   projectID,
		Name:        "CI 推送",
		Role:        domain.RoleDeveloper,
		TokenHash:   "hash-1",
		TokenPrefix: "lau6_p_a1b2c3",
		CreatedBy:   0,
	}
	if err := db.CreateProjectToken(ctx, tok); err != nil {
		t.Fatalf("落库项目令牌: %v", err)
	}
	if tok.ID <= 0 {
		t.Fatal("落库后应拿到 ID")
	}
	if tok.CreatedAt <= 0 {
		t.Fatal("落库后应写入 created_at")
	}

	// 列表
	list, err := db.ListProjectTokens(ctx, projectID)
	if err != nil {
		t.Fatalf("列项目令牌: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("列表长度 = %d，期望 1", len(list))
	}
	if list[0].Role != domain.RoleDeveloper {
		t.Fatalf("角色 = %q，期望 developer", list[0].Role)
	}
	if list[0].TokenHash != "hash-1" {
		t.Fatalf("散列 = %q", list[0].TokenHash)
	}

	// 按散列查（认证热路径）
	got, err := db.ProjectTokenByHash(ctx, "hash-1")
	if err != nil {
		t.Fatalf("按散列查: %v", err)
	}
	if got.ID != tok.ID {
		t.Fatalf("ID = %d，期望 %d", got.ID, tok.ID)
	}
	if got.Role != domain.RoleDeveloper {
		t.Fatalf("角色 = %q，期望 developer", got.Role)
	}

	// 按 ID 查（限定项目）
	if _, err := db.ProjectTokenByID(ctx, tok.ID, projectID); err != nil {
		t.Fatalf("按 ID 查: %v", err)
	}

	// 改失效时间
	expires := int64(1799000000)
	if err := db.SetProjectTokenExpiry(ctx, tok.ID, projectID, &expires); err != nil {
		t.Fatalf("改失效时间: %v", err)
	}
	got, err = db.ProjectTokenByID(ctx, tok.ID, projectID)
	if err != nil {
		t.Fatalf("改完再查: %v", err)
	}
	if got.ExpiresAt == nil || *got.ExpiresAt != expires {
		t.Fatalf("失效时间 = %v，期望 %d", got.ExpiresAt, expires)
	}

	// 最近使用时间
	if err := db.TouchProjectToken(ctx, tok.ID); err != nil {
		t.Fatalf("更新最近使用时间: %v", err)
	}
	got, err = db.ProjectTokenByID(ctx, tok.ID, projectID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastUsedAt == nil {
		t.Fatal("最近使用时间应被写入")
	}

	// 吊销
	if err := db.DeleteProjectToken(ctx, tok.ID, projectID); err != nil {
		t.Fatalf("吊销: %v", err)
	}
	if _, err := db.ProjectTokenByHash(ctx, "hash-1"); err != ErrNotFound {
		t.Fatalf("吊销后按散列查应返回 ErrNotFound，实际 %v", err)
	}
}

// TestProjectTokenScopeIsolation 覆盖"令牌只属于自己那个项目"。
func TestProjectTokenScopeIsolation(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	projectA := newTestProject(t, db, "team-a")
	projectB := newTestProject(t, db, "team-b")

	tok := &domain.ProjectToken{
		ProjectID:   projectA,
		Name:        "CI",
		Role:        domain.RoleReader,
		TokenHash:   "hash-a",
		TokenPrefix: "lau6_p_aaaaaa",
	}
	if err := db.CreateProjectToken(ctx, tok); err != nil {
		t.Fatal(err)
	}

	// 别的项目查不到、改不了、删不掉。
	if _, err := db.ProjectTokenByID(ctx, tok.ID, projectB); err != ErrNotFound {
		t.Fatalf("跨项目按 ID 查应返回 ErrNotFound，实际 %v", err)
	}
	expires := int64(1799000000)
	if err := db.SetProjectTokenExpiry(ctx, tok.ID, projectB, &expires); err != ErrNotFound {
		t.Fatalf("跨项目改期应返回 ErrNotFound，实际 %v", err)
	}
	if err := db.DeleteProjectToken(ctx, tok.ID, projectB); err != ErrNotFound {
		t.Fatalf("跨项目吊销应返回 ErrNotFound，实际 %v", err)
	}

	// 列表也不串项目。
	list, err := db.ListProjectTokens(ctx, projectB)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("项目 B 的列表长度 = %d，期望 0", len(list))
	}
}

// TestProjectTokenExpiredIsNotFound 覆盖"过期的令牌查不到"。
func TestProjectTokenExpiredIsNotFound(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	projectID := newTestProject(t, db, "team-a")

	past := int64(1000)
	tok := &domain.ProjectToken{
		ProjectID:   projectID,
		Name:        "已过期",
		Role:        domain.RoleReader,
		TokenHash:   "hash-expired",
		TokenPrefix: "lau6_p_expired",
		ExpiresAt:   &past,
	}
	if err := db.CreateProjectToken(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ProjectTokenByHash(ctx, "hash-expired"); err != ErrNotFound {
		t.Fatalf("过期令牌应返回 ErrNotFound，实际 %v", err)
	}
}

// TestDeleteProjectTokensOfProject 覆盖"整项目吊销"。
func TestDeleteProjectTokensOfProject(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	projectA := newTestProject(t, db, "team-a")
	projectB := newTestProject(t, db, "team-b")

	for _, spec := range []struct {
		project int64
		hash    string
	}{
		{projectA, "a1"},
		{projectA, "a2"},
		{projectB, "b1"},
	} {
		tok := &domain.ProjectToken{
			ProjectID:   spec.project,
			Name:        "x",
			Role:        domain.RoleReader,
			TokenHash:   spec.hash,
			TokenPrefix: "lau6_p_xxxxxx",
		}
		if err := db.CreateProjectToken(ctx, tok); err != nil {
			t.Fatal(err)
		}
	}

	n, err := db.DeleteProjectTokensOfProject(ctx, projectA)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("删除数 = %d，期望 2", n)
	}
	list, err := db.ListProjectTokens(ctx, projectB)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("项目 B 的令牌被误删：剩余 %d 枚", len(list))
	}
}

// TestLegacyPersonalTokensRemovedByMigration 覆盖"旧前缀的令牌被迁移清掉"。
//
// 清理动作发生在**迁移执行时**，所以测试要让 0007 重跑一遍：
// 先插旧行，再把 0007 从 schema_migrations 里摘掉，重新 Migrate。
func TestLegacyPersonalTokensRemovedByMigration(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)
	newTestProject(t, db, "team-a")

	// 直接插一行旧前缀的令牌（模拟迁移前的存量数据）。
	_, err := db.Exec(ctx, `INSERT INTO personal_tokens
		(user_id, name, token_hash, token_prefix, created_at, expires_at, last_used_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		0, "旧令牌", "legacy-hash", "cr_pat_nGDW4t", 1700000000, nil, nil)
	if err != nil {
		t.Fatalf("插旧令牌: %v", err)
	}
	// 再插一行新前缀的，确认没被误删。
	_, err = db.Exec(ctx, `INSERT INTO personal_tokens
		(user_id, name, token_hash, token_prefix, created_at, expires_at, last_used_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		0, "新令牌", "new-hash", "lau6_u_nGDW4t", 1700000000, nil, nil)
	if err != nil {
		t.Fatalf("插新令牌: %v", err)
	}

	// 让 0007 重跑：摘掉它的迁移记录。
	if _, err := db.Exec(ctx, `DELETE FROM schema_migrations WHERE version = ?`,
		"0007_project_tokens.sql"); err != nil {
		t.Fatalf("摘掉 0007 的迁移记录: %v", err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("重跑迁移: %v", err)
	}

	var n int
	err = db.QueryRow(ctx, `SELECT COUNT(*) FROM personal_tokens WHERE token_hash = ?`, "legacy-hash").Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("旧前缀令牌行数 = %d，期望 0", n)
	}
	err = db.QueryRow(ctx, `SELECT COUNT(*) FROM personal_tokens WHERE token_hash = ?`, "new-hash").Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("新前缀令牌行数 = %d，期望 1（不该被误删）", n)
	}
}
