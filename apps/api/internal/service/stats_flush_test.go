package service

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// newStatsTestDB 起一个带迁移的 sqlite 库。
func newStatsTestDB(t *testing.T) *store.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "stats.db")
	db, err := store.Open("sqlite", dsn, 1, 1)
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("执行迁移: %v", err)
	}
	return db
}

// TestFlushKeepsAllBucketsOnError 覆盖「刷盘失败时本轮其余桶不能丢」。
//
// snapshot 里其余仓库的计数（已经从 a.buckets 换出来了）不能因为
// 一个键出错就丢掉——
// 恢复函数注释写的是"下轮重试"，实现必须是"丢掉其余部分"的反面。
//
// 判定线：执行到一半失败，最终却看起来成功 = 静默异常（AGENTS.md 规则 3）。
func TestFlushKeepsAllBucketsOnError(t *testing.T) {
	ctx := context.Background()
	db := newStatsTestDB(t)

	project, err := db.CreateProject(ctx, "team-a", "项目 A", "", domain.VisibilityPrivate, nil)
	if err != nil {
		t.Fatalf("建项目: %v", err)
	}
	for _, name := range []string{"app-one", "app-two"} {
		if _, err := db.CreateRepository(ctx, project.ID, name, domain.VisibilityPrivate, ""); err != nil {
			t.Fatalf("建仓库 %s: %v", name, err)
		}
	}

	a := NewStatsAggregator(StatsOptions{DB: db, Enabled: true})
	t.Cleanup(func() { _ = a.Close() })

	for _, name := range []string{"team-a/app-one", "team-a/app-two"} {
		a.Note(RegistryPath{Repo: name, Kind: "manifests"}, "GET")
		a.Note(RegistryPath{Repo: name, Kind: "manifests"}, "GET")
	}

	// 让落库必然失败：把 hourly_stats 表删掉。
	if _, err := db.Exec(ctx, `DROP TABLE traffic_stats_hourly`); err != nil {
		t.Fatalf("删 hourly_stats: %v", err)
	}

	if err := a.Flush(ctx); err == nil {
		t.Fatal("落库失败时 Flush 应当返回错误")
	}

	// 两个仓库的桶都必须还在——下一轮要重试。
	for _, name := range []string{"team-a/app-one", "team-a/app-two"} {
		b := statsBuckets(a, name)
		if b == nil {
			t.Fatalf("仓库 %s 的计数桶丢了：刷盘失败后必须保留待重试", name)
		}
		if got := b.pulls.Load(); got != 2 {
			t.Fatalf("仓库 %s 的 pulls = %d, 期望 2", name, got)
		}
	}
	// 还要能说出丢了多少、丢了哪些仓库。
	if got := a.restoredBuckets.Load(); got != 2 {
		t.Fatalf("restoredBuckets = %d, 期望 2", got)
	}
	repos := a.restoredRepos()
	if len(repos) != 2 {
		t.Fatalf("restoredRepos = %v, 期望两个仓库名", repos)
	}
}

// TestFlushKeepsIPBucketsOnError 覆盖「仓库桶落库失败时 IP 桶也不能丢」。
func TestFlushKeepsIPBucketsOnError(t *testing.T) {
	ctx := context.Background()
	db := newStatsTestDB(t)

	project, err := db.CreateProject(ctx, "team-a", "项目 A", "", domain.VisibilityPrivate, nil)
	if err != nil {
		t.Fatalf("建项目: %v", err)
	}
	if _, err := db.CreateRepository(ctx, project.ID, "app", domain.VisibilityPrivate, ""); err != nil {
		t.Fatalf("建仓库: %v", err)
	}

	a := NewStatsAggregator(StatsOptions{DB: db, Enabled: true, RecordIP: true})
	t.Cleanup(func() { _ = a.Close() })

	a.NoteIP("team-a/app", "10.1.2.3")
	if _, err := db.Exec(ctx, `DROP TABLE traffic_stats_ip`); err != nil {
		t.Fatalf("删 traffic_stats_ip: %v", err)
	}
	if err := a.Flush(ctx); err == nil {
		t.Fatal("落库失败时 Flush 应当返回错误")
	}

	a.mu.RLock()
	n := len(a.ipBuckets)
	a.mu.RUnlock()
	if n == 0 {
		t.Fatal("IP 桶丢了：刷盘失败后必须保留待重试")
	}
}
