package service

import (
	"context"
	"testing"

	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// projectTokenPrincipal 造一个项目令牌主体。
func projectTokenPrincipal(projectID int64, role domain.Role) Principal {
	return Principal{Project: &domain.ProjectToken{
		ID:        1,
		ProjectID: projectID,
		Role:      role,
	}}
}

// TestDecideProjectTokenDeveloper 覆盖"developer 角色的项目令牌"。
func TestDecideProjectTokenDeveloper(t *testing.T) {
	ctx := context.Background()
	eng, db := newTestPermEngine(t)

	project, err := db.CreateProject(ctx, "team-a", "项目 A", "", domain.VisibilityPrivate, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateRepository(ctx, project.ID, "app", domain.VisibilityPublic, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateRepository(ctx, project.ID, "secret", domain.VisibilityPrivate, ""); err != nil {
		t.Fatal(err)
	}

	p := projectTokenPrincipal(project.ID, domain.RoleDeveloper)

	// 公开仓库
	d, err := eng.Decide(ctx, p, "team-a/app", []string{"pull", "push"})
	if err != nil {
		t.Fatal(err)
	}
	if !d.Allow(ActionPull) || !d.Allow(ActionPush) {
		t.Fatalf("developer 应拿到 pull+push，实际 %v", d.Granted)
	}

	// 私有仓库：项目令牌**无需** repository_grants 授权。
	d, err = eng.Decide(ctx, p, "team-a/secret", []string{"pull", "push", "delete"})
	if err != nil {
		t.Fatal(err)
	}
	if !d.Allow(ActionPull) || !d.Allow(ActionPush) || !d.Allow(ActionDelete) {
		t.Fatalf("developer 对私有仓库应拿到 pull+push+delete，实际 %v", d.Granted)
	}
}

// TestDecideProjectTokenReader 覆盖"reader 角色的项目令牌"。
func TestDecideProjectTokenReader(t *testing.T) {
	ctx := context.Background()
	eng, db := newTestPermEngine(t)

	project, err := db.CreateProject(ctx, "team-a", "项目 A", "", domain.VisibilityPrivate, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateRepository(ctx, project.ID, "app", domain.VisibilityPublic, ""); err != nil {
		t.Fatal(err)
	}

	p := projectTokenPrincipal(project.ID, domain.RoleReader)
	d, err := eng.Decide(ctx, p, "team-a/app", []string{"pull", "push", "delete"})
	if err != nil {
		t.Fatal(err)
	}
	if !d.Allow(ActionPull) {
		t.Fatalf("reader 应拿到 pull，实际 %v", d.Granted)
	}
	if d.Allow(ActionPush) || d.Allow(ActionDelete) {
		t.Fatalf("reader 不该拿到 push/delete，实际 %v", d.Granted)
	}
}

// TestDecideProjectTokenCrossProject 覆盖"项目令牌跨项目一律拒绝"。
func TestDecideProjectTokenCrossProject(t *testing.T) {
	ctx := context.Background()
	eng, db := newTestPermEngine(t)

	projectA, err := db.CreateProject(ctx, "team-a", "项目 A", "", domain.VisibilityPublic, nil)
	if err != nil {
		t.Fatal(err)
	}
	projectB, err := db.CreateProject(ctx, "team-b", "项目 B", "", domain.VisibilityPublic, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateRepository(ctx, projectB.ID, "app", domain.VisibilityPublic, ""); err != nil {
		t.Fatal(err)
	}

	p := projectTokenPrincipal(projectA.ID, domain.RoleDeveloper)
	d, err := eng.Decide(ctx, p, "team-b/app", []string{"pull", "push"})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Granted) != 0 {
		t.Fatalf("跨项目不该授予任何动作，实际 %v", d.Granted)
	}
}

// TestDecideProjectTokenNotShortCircuitedByPublicFace 覆盖"公开面不短路项目令牌"。
//
// 项目令牌不是 project_members 的一行，member 查出来是 nil——
// 如果判定顺序没把它放在公开面短路之前，它会被降级成"只读 pull"。
func TestDecideProjectTokenNotShortCircuitedByPublicFace(t *testing.T) {
	ctx := context.Background()
	eng, db := newTestPermEngine(t)

	project, err := db.CreateProject(ctx, "team-a", "项目 A", "", domain.VisibilityPublic, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateRepository(ctx, project.ID, "app", domain.VisibilityPublic, ""); err != nil {
		t.Fatal(err)
	}

	p := projectTokenPrincipal(project.ID, domain.RoleDeveloper)
	d, err := eng.Decide(ctx, p, "team-a/app", []string{"pull", "push"})
	if err != nil {
		t.Fatal(err)
	}
	if !d.Allow(ActionPush) {
		t.Fatalf("公开项目里的 developer 项目令牌应能 push，实际 %v", d.Granted)
	}
}

// TestDecideProjectTokenMissingProject 覆盖"项目令牌对不存在的项目"。
func TestDecideProjectTokenMissingProject(t *testing.T) {
	ctx := context.Background()
	eng, _ := newTestPermEngine(t)

	p := projectTokenPrincipal(999, domain.RoleDeveloper)
	d, err := eng.Decide(ctx, p, "ghost/app", []string{"pull"})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Granted) != 0 {
		t.Fatalf("项目不存在时不该授予动作，实际 %v", d.Granted)
	}
	if d.Reason != ReasonProjectNotFound {
		t.Fatalf("Reason = %q，期望 %q", d.Reason, ReasonProjectNotFound)
	}
}

// TestDecideProjectTokenAutoCreate 覆盖"项目令牌自动建仓"。
//
// delete 是不可逆动作，不授予——与成员路径同口径。
func TestDecideProjectTokenAutoCreate(t *testing.T) {
	ctx := context.Background()
	eng, db := newTestPermEngine(t)

	project, err := db.CreateProject(ctx, "team-a", "项目 A", "", domain.VisibilityPrivate, nil)
	if err != nil {
		t.Fatal(err)
	}

	p := projectTokenPrincipal(project.ID, domain.RoleDeveloper)
	d, err := eng.Decide(ctx, p, "team-a/newrepo", []string{"pull", "push", "delete"})
	if err != nil {
		t.Fatal(err)
	}
	if !d.Allow(ActionPull) || !d.Allow(ActionPush) {
		t.Fatalf("自动建仓应授予 pull+push，实际 %v", d.Granted)
	}
	if d.Allow(ActionDelete) {
		t.Fatalf("自动建仓不该授予 delete，实际 %v", d.Granted)
	}
}

// TestDecideProjectTokenAutoCreateCrossProject 覆盖"跨项目不能建仓"。
func TestDecideProjectTokenAutoCreateCrossProject(t *testing.T) {
	ctx := context.Background()
	eng, db := newTestPermEngine(t)

	projectA, err := db.CreateProject(ctx, "team-a", "项目 A", "", domain.VisibilityPrivate, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateProject(ctx, "team-b", "项目 B", "", domain.VisibilityPrivate, nil); err != nil {
		t.Fatal(err)
	}

	p := projectTokenPrincipal(projectA.ID, domain.RoleDeveloper)
	d, err := eng.Decide(ctx, p, "team-b/newrepo", []string{"pull", "push"})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Granted) != 0 {
		t.Fatalf("跨项目建仓不该被授予，实际 %v", d.Granted)
	}
}

// TestDecideAnonymousStillWorks 覆盖"匿名主体没有被项目主体改坏"。
func TestDecideAnonymousStillWorks(t *testing.T) {
	ctx := context.Background()
	eng, db := newTestPermEngine(t)

	project, err := db.CreateProject(ctx, "team-a", "项目 A", "", domain.VisibilityPublic, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateRepository(ctx, project.ID, "app", domain.VisibilityPublic, ""); err != nil {
		t.Fatal(err)
	}

	d, err := eng.Decide(ctx, Principal{}, "team-a/app", []string{"pull", "push"})
	if err != nil {
		t.Fatal(err)
	}
	if !d.Allow(ActionPull) {
		t.Fatalf("匿名应拿到 pull，实际 %v", d.Granted)
	}
	if d.Allow(ActionPush) {
		t.Fatalf("匿名不该拿到 push，实际 %v", d.Granted)
	}
}

// TestProjectPrincipalIsAnonymous 覆盖"匿名判定"。
func TestProjectPrincipalIsAnonymous(t *testing.T) {
	var anon Principal
	if !anon.IsAnonymous() {
		t.Fatal("空主体应是匿名")
	}
	proj := projectTokenPrincipal(1, domain.RoleReader)
	if proj.IsAnonymous() {
		t.Fatal("项目令牌主体不是匿名")
	}
	user := Principal{User: &domain.User{}}
	if user.IsAnonymous() {
		t.Fatal("用户主体不是匿名")
	}
}

// TestVisibleRepositoriesWithProjectPrincipal 覆盖"项目令牌看得到本项目私有仓库"。
func TestVisibleRepositoriesWithProjectPrincipal(t *testing.T) {
	ctx := context.Background()
	eng, db := newTestPermEngine(t)

	project, err := db.CreateProject(ctx, "team-a", "项目 A", "", domain.VisibilityPrivate, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateRepository(ctx, project.ID, "app", domain.VisibilityPublic, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateRepository(ctx, project.ID, "secret", domain.VisibilityPrivate, ""); err != nil {
		t.Fatal(err)
	}

	p := projectTokenPrincipal(project.ID, domain.RoleDeveloper)
	repos, err := eng.VisibleRepositories(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 2 {
		t.Fatalf("可见仓库数 = %d，期望 2", len(repos))
	}
}

// TestStoreProjectTokenRoundTrip 覆盖"存储层与主体配合"。
func TestStoreProjectTokenRoundTrip(t *testing.T) {
	ctx := context.Background()
	_, db := newTestPermEngine(t)

	project, err := db.CreateProject(ctx, "team-a", "项目 A", "", domain.VisibilityPrivate, nil)
	if err != nil {
		t.Fatal(err)
	}
	tok := &domain.ProjectToken{
		ProjectID:   project.ID,
		Name:        "CI",
		Role:        domain.RoleDeveloper,
		TokenHash:   "h",
		TokenPrefix: "lau6_p_xxxxxx",
	}
	if err := db.CreateProjectToken(ctx, tok); err != nil {
		t.Fatal(err)
	}
	got, err := db.ProjectTokenByID(ctx, tok.ID, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Role != domain.RoleDeveloper {
		t.Fatalf("角色 = %q", got.Role)
	}
	if _, err := db.ProjectTokenByID(ctx, tok.ID, project.ID+1); err != store.ErrNotFound {
		t.Fatalf("跨项目按 ID 查应返回 ErrNotFound，实际 %v", err)
	}
}
