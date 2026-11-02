package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
)

// newTestDB 建一个跑完迁移的 SQLite 库。
//
// 用临时文件而不是 :memory:：modernc 的 SQLite 驱动对内存库的
// 连接池行为与文件库不同，maxOpen=1 时更容易踩到"换连接就丢表"的坑。
func newTestDB(t *testing.T) *DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db")
	db, err := Open("sqlite", dsn, 1, 1)
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("执行迁移: %v", err)
	}
	return db
}

// TestRepositoryDigestColumnsMayBeNull 覆盖"仓库建了但还没推过镜像"的场景。
//
// created_digest / latest_digest 两列建仓时是 NULL（要等第一次推送才回填）。
// 扫描时必须用 NullString 兜住：直接扫进 string 会报
// "converting NULL to string is unsupported"，
// 于是目录列表接口和权限判定一起 500 —— 整个镜像目录页打不开。
func TestRepositoryDigestColumnsMayBeNull(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	project, err := db.CreateProject(ctx, "team-a", "项目 A", "", domain.VisibilityPrivate, nil)
	if err != nil {
		t.Fatalf("创建项目: %v", err)
	}
	repo, err := db.CreateRepository(ctx, project.ID, "api-gateway", domain.VisibilityPrivate, "")
	if err != nil {
		t.Fatalf("创建仓库: %v", err)
	}

	// 四条读路径都要能读出这一行，且 digest 为空。
	all, err := db.AllRepositories(ctx)
	if err != nil {
		t.Fatalf("AllRepositories: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("AllRepositories 应返回 1 行，实际 %d 行", len(all))
	}
	assertRepo(t, all[0], "team-a/api-gateway")

	byPath, err := db.RepositoryByPath(ctx, project.ID, "api-gateway")
	if err != nil {
		t.Fatalf("RepositoryByPath: %v", err)
	}
	assertRepo(t, *byPath, "team-a/api-gateway")

	byID, err := db.RepositoryByID(ctx, repo.ID)
	if err != nil {
		t.Fatalf("RepositoryByID: %v", err)
	}
	assertRepo(t, *byID, "team-a/api-gateway")

	inProject, err := db.ListRepositoriesInProject(ctx, project.ID)
	if err != nil {
		t.Fatalf("ListRepositoriesInProject: %v", err)
	}
	if len(inProject) != 1 {
		t.Fatalf("ListRepositoriesInProject 应返回 1 行，实际 %d 行", len(inProject))
	}
	assertRepo(t, inProject[0], "team-a/api-gateway")

	// 推送之后 digest 回填，仍要能正常读出。
	if err := db.TouchRepositoryPush(ctx, repo.ID, "sha256:abc123"); err != nil {
		t.Fatalf("TouchRepositoryPush: %v", err)
	}
	pushed, err := db.RepositoryByPath(ctx, project.ID, "api-gateway")
	if err != nil {
		t.Fatalf("RepositoryByPath（推送后）: %v", err)
	}
	if pushed.CreatedDigest != "sha256:abc123" || pushed.LatestDigest != "sha256:abc123" {
		t.Fatalf("推送后 digest 应回填，实际 created=%q latest=%q",
			pushed.CreatedDigest, pushed.LatestDigest)
	}
}

// assertRepo 校验仓库记录的关键字段。
func assertRepo(t *testing.T, repo domain.Repository, wantFullName string) {
	t.Helper()
	if repo.FullName != wantFullName {
		t.Errorf("FullName = %q，期望 %q", repo.FullName, wantFullName)
	}
	if repo.CreatedDigest != "" || repo.LatestDigest != "" {
		t.Errorf("未推送的仓库 digest 应为空，实际 created=%q latest=%q",
			repo.CreatedDigest, repo.LatestDigest)
	}
	if repo.PushedAt != nil {
		t.Errorf("未推送的仓库 PushedAt 应为 nil，实际 %d", *repo.PushedAt)
	}
}

// TestCreateRepositoryReturnsCompleteRecord 覆盖"建仓必须返回完整记录"。
//
// ⚠️ ProjectName 与 FullName 要 JOIN projects 才有，INSERT 拿不到。
// 如果 CreateRepository 返回半成品（FullName 为空），
// **刷新队列的键就是空串** → 刷新被静默跳过 →
// 目录页显示"有镜像但大小/标签/平台全空"，而且**一条日志都没有**。
func TestCreateRepositoryReturnsCompleteRecord(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	project, err := db.CreateProject(ctx, "team-a", "项目 A", "", domain.VisibilityPrivate, nil)
	if err != nil {
		t.Fatalf("创建项目: %v", err)
	}
	repo, err := db.CreateRepository(ctx, project.ID, "api-gateway", domain.VisibilityPrivate, "")
	if err != nil {
		t.Fatalf("创建仓库: %v", err)
	}
	if repo.FullName != "team-a/api-gateway" {
		t.Fatalf("建仓返回的 FullName = %q，期望 team-a/api-gateway（空键会让刷新被静默跳过）", repo.FullName)
	}
	if repo.ProjectName != "team-a" {
		t.Fatalf("建仓返回的 ProjectName = %q，期望 team-a", repo.ProjectName)
	}
	if repo.ID == 0 {
		t.Fatalf("建仓返回的 ID = 0")
	}
}

// TestRepositoryVisibilityFollowsProject 覆盖"仓库可见性跟随项目"。
//
// 语义见 02-auth.md §6：
//   - 新建仓库默认**跟随项目**（inherit_visibility=1）
//   - 生效可见性 = 跟随时取项目可见性，显式时取仓库自己的
//   - 项目从私有改公开，跟随的仓库**自动**跟着公开（不用挨个改）
func TestRepositoryVisibilityFollowsProject(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	project, err := db.CreateProject(ctx, "team-a", "项目 A", "", domain.VisibilityPrivate, nil)
	if err != nil {
		t.Fatalf("创建项目: %v", err)
	}
	repo, err := db.CreateRepository(ctx, project.ID, "app", domain.VisibilityPrivate, "")
	if err != nil {
		t.Fatalf("建仓: %v", err)
	}
	if repo.VisibilitySource != domain.VisibilitySourceInherit {
		t.Fatalf("新建仓库的可见性来源 = %q，期望 inherit", repo.VisibilitySource)
	}
	if repo.Visibility != domain.VisibilityPrivate {
		t.Fatalf("私有项目里的仓库生效可见性 = %q，期望 private", repo.Visibility)
	}

	// 项目改公开 → 跟随的仓库自动跟着公开。
	public := domain.VisibilityPublic
	if err := db.UpdateProject(ctx, project.ID, nil, nil, &public); err != nil {
		t.Fatalf("改项目可见性: %v", err)
	}
	after, err := db.RepositoryByID(ctx, repo.ID)
	if err != nil {
		t.Fatalf("读仓库: %v", err)
	}
	if after.Visibility != domain.VisibilityPublic {
		t.Fatalf("项目公开后仓库生效可见性 = %q，期望 public", after.Visibility)
	}

	// 项目管理员显式收紧 → 不再跟随项目。
	explicit := domain.VisibilityPrivate
	if err := db.UpdateRepository(ctx, repo.ID, &explicit, nil); err != nil {
		t.Fatalf("显式设置仓库可见性: %v", err)
	}
	after, err = db.RepositoryByID(ctx, repo.ID)
	if err != nil {
		t.Fatalf("读仓库: %v", err)
	}
	if after.VisibilitySource != domain.VisibilitySourceExplicit {
		t.Fatalf("显式设置后的来源 = %q，期望 explicit", after.VisibilitySource)
	}
	if after.Visibility != domain.VisibilityPrivate {
		t.Fatalf("显式设置后的生效可见性 = %q，期望 private", after.Visibility)
	}

	// 恢复跟随 → 又跟着项目走。
	inherit := domain.VisibilityInherit
	if err := db.UpdateRepository(ctx, repo.ID, &inherit, nil); err != nil {
		t.Fatalf("恢复跟随项目: %v", err)
	}
	after, err = db.RepositoryByID(ctx, repo.ID)
	if err != nil {
		t.Fatalf("读仓库: %v", err)
	}
	if after.VisibilitySource != domain.VisibilitySourceInherit {
		t.Fatalf("恢复后的来源 = %q，期望 inherit", after.VisibilitySource)
	}
	if after.Visibility != domain.VisibilityPublic {
		t.Fatalf("恢复后的生效可见性 = %q，期望 public（跟随项目）", after.Visibility)
	}
}

// TestProjectTotalSizeSumsRepoMetaSize 覆盖"项目大小 = 项目内所有仓库大小之和"。
//
// 数据来源是 repo_meta.size（与镜像目录页每个仓库的"大小"列同一个缓存行），
// 所以项目列表的"大小"列与目录页逐仓库相加是一致的。
func TestProjectTotalSizeSumsRepoMetaSize(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	project, err := db.CreateProject(ctx, "team-a", "项目 A", "", domain.VisibilityPrivate, nil)
	if err != nil {
		t.Fatalf("创建项目: %v", err)
	}
	other, err := db.CreateProject(ctx, "team-b", "项目 B", "", domain.VisibilityPrivate, nil)
	if err != nil {
		t.Fatalf("创建项目: %v", err)
	}

	// team-a 两个仓库：100 + 250 = 350。
	for _, tc := range []struct {
		name string
		size int64
	}{{"app", 100}, {"api", 250}} {
		repo, err := db.CreateRepository(ctx, project.ID, tc.name, domain.VisibilityPrivate, "")
		if err != nil {
			t.Fatalf("创建仓库 %s: %v", tc.name, err)
		}
		if err := db.UpsertRepoMeta(ctx, &domain.RepoMeta{RepositoryID: repo.ID, Size: tc.size}); err != nil {
			t.Fatalf("写入 repo_meta %s: %v", tc.name, err)
		}
	}
	// team-b 一个仓库：999 —— 不能串到 team-a 的和里。
	otherRepo, err := db.CreateRepository(ctx, other.ID, "solo", domain.VisibilityPrivate, "")
	if err != nil {
		t.Fatalf("创建仓库: %v", err)
	}
	if err := db.UpsertRepoMeta(ctx, &domain.RepoMeta{RepositoryID: otherRepo.ID, Size: 999}); err != nil {
		t.Fatalf("写入 repo_meta: %v", err)
	}

	projects, err := db.ListProjects(ctx, nil, nil)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	sizes := map[string]int64{}
	for _, p := range projects {
		sizes[p.Name] = p.TotalSize
	}
	if sizes["team-a"] != 350 {
		t.Fatalf("team-a 的 total_size = %d，期望 350", sizes["team-a"])
	}
	if sizes["team-b"] != 999 {
		t.Fatalf("team-b 的 total_size = %d，期望 999", sizes["team-b"])
	}

	// 详情页走的是 ProjectByName，也要带上同一个值。
	detail, err := db.ProjectByName(ctx, "team-a")
	if err != nil {
		t.Fatalf("ProjectByName: %v", err)
	}
	if detail.TotalSize != 350 {
		t.Fatalf("ProjectByName 的 total_size = %d，期望 350", detail.TotalSize)
	}
}

// TestProjectTotalSizeZeroWithoutRepoMeta 覆盖"仓库还没刷新过缓存"的情况。
//
// 没有 repo_meta 行时和必须是 0，不能报错也不能算成别的数。
func TestProjectTotalSizeZeroWithoutRepoMeta(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	project, err := db.CreateProject(ctx, "team-a", "项目 A", "", domain.VisibilityPrivate, nil)
	if err != nil {
		t.Fatalf("创建项目: %v", err)
	}
	if _, err := db.CreateRepository(ctx, project.ID, "app", domain.VisibilityPrivate, ""); err != nil {
		t.Fatalf("建仓: %v", err)
	}

	got, err := db.ProjectByName(ctx, "team-a")
	if err != nil {
		t.Fatalf("ProjectByName: %v", err)
	}
	if got.TotalSize != 0 {
		t.Fatalf("没有缓存行时 total_size = %d，期望 0", got.TotalSize)
	}
}
