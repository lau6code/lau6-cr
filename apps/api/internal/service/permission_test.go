package service

import (
	"context"
	"testing"

	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// newTestPermEngine 建一个权限引擎 + 库。
func newTestPermEngine(t *testing.T) (*PermissionEngine, *store.DB) {
	t.Helper()
	db := newTestDB(t)
	return NewPermissionEngine(db, PermissionOptions{AnonymousReadEnabled: func() bool { return true }}), db
}

// createAdmin 建一个系统管理员账户。
func createAdmin(t *testing.T, db *store.DB, name string) Principal {
	t.Helper()
	u, err := db.CreateUser(context.Background(), name, "hash", "local", "", true)
	if err != nil {
		t.Fatalf("创建管理员: %v", err)
	}
	return Principal{User: u}
}

// TestSystemAdminCannotPushToMissingProject 覆盖"不存在的项目一律拒绝"。
//
// ⚠️ 镜像名的第一段就是项目名（Docker 命名空间）。
// 往不存在的命名空间推，会在上游留下**没人管得了的孤儿镜像**：
// 目录里列不出来（没有项目行），也没有可见性与授权可言。
//
// 管理员的"全局绕过"是**项目内**的绕过，不是"能凭空造命名空间"。
func TestSystemAdminCannotPushToMissingProject(t *testing.T) {
	eng, db := newTestPermEngine(t)
	admin := createAdmin(t, db, "admin")

	d, err := eng.Decide(context.Background(), admin, "cdt/forgejo", []string{"pull", "push"})
	if err != nil {
		t.Fatalf("判定: %v", err)
	}
	if len(d.Granted) != 0 {
		t.Fatalf("项目不存在时不该授予任何动作，实际授予 %v", d.Granted)
	}

	// 连 pull 也不该给（公开面也依赖项目存在）。
	d, err = eng.Decide(context.Background(), admin, "cdt/forgejo", []string{"pull"})
	if err != nil {
		t.Fatalf("判定: %v", err)
	}
	if len(d.Granted) != 0 {
		t.Fatalf("项目不存在时不该授予 pull，实际授予 %v", d.Granted)
	}
}

// TestAnonymousCannotAccessMissingProject 覆盖匿名对不存在项目的访问。
func TestAnonymousCannotAccessMissingProject(t *testing.T) {
	eng, _ := newTestPermEngine(t)

	d, err := eng.Decide(context.Background(), Principal{}, "cdt/forgejo", []string{"pull", "push"})
	if err != nil {
		t.Fatalf("判定: %v", err)
	}
	if len(d.Granted) != 0 {
		t.Fatalf("匿名不该获得任何动作，实际 %v", d.Granted)
	}
}

// TestSystemAdminFullRightsInsideExistingProject 覆盖"管理员在已存在项目内仍然全权"。
//
// ⚠️ 这是"全局绕过"的本意：管理员不必是每个项目的成员，
// 也能在**已存在**的项目里推、拉、删。
func TestSystemAdminFullRightsInsideExistingProject(t *testing.T) {
	eng, db := newTestPermEngine(t)
	admin := createAdmin(t, db, "admin")
	seedProjectOnly(t, db, "team-a")

	// 仓库还没建（自动建仓路径），管理员也要能推。
	d, err := eng.Decide(context.Background(), admin, "team-a/app", []string{"pull", "push"})
	if err != nil {
		t.Fatalf("判定: %v", err)
	}
	if len(d.Granted) != 2 {
		t.Fatalf("管理员在已存在项目内应获得 pull+push，实际 %v", d.Granted)
	}
}

// TestAnonymousCanPullPublicRepoInPublicProject 覆盖"公开项目·公开仓库 → 匿名可拉"。
//
// 这是匿名可读能成立的唯一组合（`02-auth.md` §4.3 第 4 步）：
// **项目与仓库都要 public**。
// 任一为 private，匿名就拿不到 pull。
func TestAnonymousCanPullPublicRepoInPublicProject(t *testing.T) {
	eng, db := newTestPermEngine(t)
	ctx := context.Background()
	if _, err := db.CreateProject(ctx, "pub-team", "公开项目", "", domain.VisibilityPublic, nil); err != nil {
		t.Fatalf("创建项目: %v", err)
	}
	project, err := db.ProjectByName(ctx, "pub-team")
	if err != nil {
		t.Fatalf("查项目: %v", err)
	}
	if _, err := db.CreateRepository(ctx, project.ID, "app", domain.VisibilityPublic, ""); err != nil {
		t.Fatalf("建仓: %v", err)
	}

	d, err := eng.Decide(ctx, Principal{}, "pub-team/app", []string{"pull"})
	if err != nil {
		t.Fatalf("判定: %v", err)
	}
	if len(d.Granted) != 1 || d.Granted[0] != "pull" {
		t.Fatalf("匿名在公开项目·公开仓库上应获得 pull，实际 %v", d.Granted)
	}
}

// TestAnonymousCannotPullPrivateRepoInPublicProject 覆盖"公开项目·**显式私有**的仓库 → 匿名不可见"。
//
// ⚠️ 仓库默认**跟随项目**（02-auth.md §6），所以要造出"私有仓库"必须
//
//	**显式**设置——这正是"项目管理员可单独收紧"的那条路径。
func TestAnonymousCannotPullPrivateRepoInPublicProject(t *testing.T) {
	eng, db := newTestPermEngine(t)
	ctx := context.Background()
	if _, err := db.CreateProject(ctx, "pub-team", "公开项目", "", domain.VisibilityPublic, nil); err != nil {
		t.Fatalf("创建项目: %v", err)
	}
	project, err := db.ProjectByName(ctx, "pub-team")
	if err != nil {
		t.Fatalf("查项目: %v", err)
	}
	repo, err := db.CreateRepository(ctx, project.ID, "app", domain.VisibilityPublic, "")
	if err != nil {
		t.Fatalf("建仓: %v", err)
	}
	// 项目管理员显式收紧。
	explicit := domain.VisibilityPrivate
	if err := db.UpdateRepository(ctx, repo.ID, &explicit, nil); err != nil {
		t.Fatalf("显式设置仓库可见性: %v", err)
	}

	d, err := eng.Decide(ctx, Principal{}, "pub-team/app", []string{"pull"})
	if err != nil {
		t.Fatalf("判定: %v", err)
	}
	if len(d.Granted) != 0 {
		t.Fatalf("私有仓库不该对匿名开放，实际 %v", d.Granted)
	}
}

// TestAnonymousCanPullRepoFollowingPublicProject 覆盖"仓库跟随公开项目 → 匿名可拉"。
//
// 这是新语义的主路径：公开项目里推上来的镜像**默认**跟着公开，
// 项目改公开时它自动跟着公开（不用挨个改仓库）。
func TestAnonymousCanPullRepoFollowingPublicProject(t *testing.T) {
	eng, db := newTestPermEngine(t)
	ctx := context.Background()
	if _, err := db.CreateProject(ctx, "pub-team", "公开项目", "", domain.VisibilityPublic, nil); err != nil {
		t.Fatalf("创建项目: %v", err)
	}
	project, err := db.ProjectByName(ctx, "pub-team")
	if err != nil {
		t.Fatalf("查项目: %v", err)
	}
	// 建仓即跟随项目（这里传什么 visibility 都不重要，生效值由项目决定）。
	repo, err := db.CreateRepository(ctx, project.ID, "app", domain.VisibilityPrivate, "")
	if err != nil {
		t.Fatalf("建仓: %v", err)
	}
	if repo.Visibility != domain.VisibilityPublic {
		t.Fatalf("跟随公开项目的仓库生效可见性 = %q，期望 public", repo.Visibility)
	}
	if repo.VisibilitySource != domain.VisibilitySourceInherit {
		t.Fatalf("可见性来源 = %q，期望 inherit", repo.VisibilitySource)
	}

	d, err := eng.Decide(ctx, Principal{}, "pub-team/app", []string{"pull"})
	if err != nil {
		t.Fatalf("判定: %v", err)
	}
	if len(d.Granted) != 1 {
		t.Fatalf("跟随公开项目的仓库应给匿名 pull，实际 %v", d.Granted)
	}
}

func TestMemberCanPushNewRepoInOwnProject(t *testing.T) {
	eng, db := newTestPermEngine(t)
	ctx := context.Background()
	seedProjectOnly(t, db, "team-a")

	u, err := db.CreateUser(ctx, "dev", "hash", "local", "", false)
	if err != nil {
		t.Fatalf("创建用户: %v", err)
	}
	project, err := db.ProjectByName(ctx, "team-a")
	if err != nil {
		t.Fatalf("查项目: %v", err)
	}
	if err := db.AddMember(ctx, project.ID, u.ID, domain.RoleDeveloper, nil); err != nil {
		t.Fatalf("加成员: %v", err)
	}

	d, err := eng.Decide(ctx, Principal{User: u}, "team-a/app", []string{"pull", "push"})
	if err != nil {
		t.Fatalf("判定: %v", err)
	}
	if len(d.Granted) != 2 {
		t.Fatalf("developer 在自己项目里应获得 pull+push，实际 %v", d.Granted)
	}
}

// TestNonMemberCannotPushNewRepo 覆盖"非成员不能在自己不存在的仓库上建仓推送"。
func TestNonMemberCannotPushNewRepo(t *testing.T) {
	eng, db := newTestPermEngine(t)
	ctx := context.Background()
	seedProjectOnly(t, db, "team-a")

	u, err := db.CreateUser(ctx, "outsider", "hash", "local", "", false)
	if err != nil {
		t.Fatalf("创建用户: %v", err)
	}
	d, err := eng.Decide(ctx, Principal{User: u}, "team-a/app", []string{"pull", "push"})
	if err != nil {
		t.Fatalf("判定: %v", err)
	}
	if len(d.Granted) != 0 {
		t.Fatalf("非成员不该获得任何动作，实际 %v", d.Granted)
	}
}

// TestAnonymousReadSwitchBlocksPublicPull 覆盖"匿名读总开关真的能挡住匿名拉取"。
//
// access.anonymous_read_enabled 是公开面的一键止血阀：
// 它必须**零消费点以外**真的生效——拨了跟没拨一样等于没有。
// 这个测试锁定"拨了真的生效"。
func TestAnonymousReadSwitchBlocksPublicPull(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if _, err := db.CreateProject(ctx, "pub-team", "公开项目", "", domain.VisibilityPublic, nil); err != nil {
		t.Fatalf("创建项目: %v", err)
	}
	project, err := db.ProjectByName(ctx, "pub-team")
	if err != nil {
		t.Fatalf("查项目: %v", err)
	}
	if _, err := db.CreateRepository(ctx, project.ID, "app", domain.VisibilityPublic, ""); err != nil {
		t.Fatalf("建仓: %v", err)
	}

	// 开关关闭 → 匿名拿不到 pull。
	off := NewPermissionEngine(db, PermissionOptions{AnonymousReadEnabled: func() bool { return false }})
	d, err := off.Decide(ctx, Principal{}, "pub-team/app", []string{"pull"})
	if err != nil {
		t.Fatalf("判定: %v", err)
	}
	if len(d.Granted) != 0 {
		t.Fatalf("匿名读开关关闭时不该授予 pull，实际 %v", d.Granted)
	}

	// 开关打开 → 匿名照常可拉（开关不能把功能焊死）。
	on := NewPermissionEngine(db, PermissionOptions{AnonymousReadEnabled: func() bool { return true }})
	d, err = on.Decide(ctx, Principal{}, "pub-team/app", []string{"pull"})
	if err != nil {
		t.Fatalf("判定: %v", err)
	}
	if len(d.Granted) != 1 || d.Granted[0] != "pull" {
		t.Fatalf("匿名读开关打开时应获得 pull，实际 %v", d.Granted)
	}
}

// TestAnonymousReadSwitchDoesNotAffectMembers 覆盖"开关只挡匿名，不挡成员"。
//
// 开关的语义是"免鉴权可 pull"——已登录的成员本来就带身份，
// 不该被一起挡掉（否则项目管理员自己也拉不动）。
func TestAnonymousReadSwitchDoesNotAffectMembers(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if _, err := db.CreateProject(ctx, "pub-team", "公开项目", "", domain.VisibilityPublic, nil); err != nil {
		t.Fatalf("创建项目: %v", err)
	}
	project, err := db.ProjectByName(ctx, "pub-team")
	if err != nil {
		t.Fatalf("查项目: %v", err)
	}
	if _, err := db.CreateRepository(ctx, project.ID, "app", domain.VisibilityPublic, ""); err != nil {
		t.Fatalf("建仓: %v", err)
	}
	member := createAdmin(t, db, "dev")
	if err := db.AddMember(ctx, project.ID, member.User.ID, domain.RoleDeveloper, nil); err != nil {
		t.Fatalf("加成员: %v", err)
	}

	off := NewPermissionEngine(db, PermissionOptions{AnonymousReadEnabled: func() bool { return false }})
	d, err := off.Decide(ctx, member, "pub-team/app", []string{"pull"})
	if err != nil {
		t.Fatalf("判定: %v", err)
	}
	if len(d.Granted) == 0 {
		t.Fatal("匿名读开关关闭时，**成员**仍应能 pull")
	}
}

// TestAnonymousReadSwitchUnwiredDenies 覆盖"开关没接线 → 按关闭处理"。
//
// 这个开关读不到状态就不能默认放行——那正是它要防的那件事。
func TestAnonymousReadSwitchUnwiredDenies(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if _, err := db.CreateProject(ctx, "pub-team", "公开项目", "", domain.VisibilityPublic, nil); err != nil {
		t.Fatalf("创建项目: %v", err)
	}
	project, err := db.ProjectByName(ctx, "pub-team")
	if err != nil {
		t.Fatalf("查项目: %v", err)
	}
	if _, err := db.CreateRepository(ctx, project.ID, "app", domain.VisibilityPublic, ""); err != nil {
		t.Fatalf("建仓: %v", err)
	}

	eng := NewPermissionEngine(db, PermissionOptions{})
	d, err := eng.Decide(ctx, Principal{}, "pub-team/app", []string{"pull"})
	if err != nil {
		t.Fatalf("判定: %v", err)
	}
	if len(d.Granted) != 0 {
		t.Fatalf("开关没接线时不该放行匿名 pull，实际 %v", d.Granted)
	}
}
