package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/config"
	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// 清理执行器的端到端测试：假上游 registry + 真实 SQLite + 真实执行链路。

// fakeUpstream 是一个最小的 registry 上游。
//
// 只实现清理需要的三个端点：
//
//	GET    /v2/<repo>/tags/list
//	GET    /v2/<repo>/manifests/<tag|digest>
//	DELETE /v2/<repo>/manifests/<digest>
type fakeUpstream struct {
	mu sync.Mutex
	// tags 是 仓库 → (tag → digest)。
	tags map[string]map[string]string
	// deleted 记录被删除的 manifest（repo:digest）。
	deleted []string
	// failDelete 让删除一律返回 500，用于验证"部分失败 → 整体 failed"。
	failDelete bool
}

func newFakeUpstream() *fakeUpstream {
	return &fakeUpstream{tags: map[string]map[string]string{}}
}

// handler 返回上游的 HTTP handler。
func (f *fakeUpstream) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/v2/")
		f.mu.Lock()
		defer f.mu.Unlock()

		if i := strings.LastIndex(p, "/manifests/"); i >= 0 {
			repo := p[:i]
			ref := p[i+len("/manifests/"):]
			m := f.tags[repo]
			if m == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if r.Method == http.MethodDelete {
				if f.failDelete {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				for tag, dg := range m {
					if dg == ref {
						delete(m, tag)
						break
					}
				}
				f.deleted = append(f.deleted, repo+":"+ref)
				w.WriteHeader(http.StatusAccepted)
				return
			}
			dgst, ok := m[ref]
			if !ok {
				for _, d := range m {
					if d == ref {
						dgst, ok = d, true
						break
					}
				}
			}
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Docker-Content-Digest", dgst)
			w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"schemaVersion": 2,
				"mediaType":     "application/vnd.docker.distribution.manifest.v2+json",
				"config": map[string]any{
					"digest": "sha256:cfg", "size": 10,
					"mediaType": "application/vnd.docker.container.image.v1+json",
				},
				"layers": []any{},
			})
			return
		}

		if strings.HasSuffix(p, "/tags/list") {
			repo := strings.TrimSuffix(p, "/tags/list")
			m := f.tags[repo]
			names := make([]string, 0, len(m))
			for k := range m {
				names = append(names, k)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"name": repo, "tags": names})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}
}

// deletedList 返回被删除的 manifest 列表。
func (f *fakeUpstream) deletedList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.deleted))
	copy(out, f.deleted)
	return out
}

// remainingTags 返回某仓库剩下的 tag。
func (f *fakeUpstream) remainingTags(repo string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := f.tags[repo]
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// cleanupFixture 搭一套完整的清理环境。
type cleanupFixture struct {
	db       *store.DB
	svc      *CleanupService
	up       *fakeUpstream
	repoID   int64
	projectN string
}

// newCleanupFixture 搭好库、策略、假上游。
//
// tags 是 仓库 → (tag → digest)；pushedAt 是 tag → 推送时间（unix 秒）。
func newCleanupFixture(t *testing.T, rules []domain.RetentionRule,
	tags map[string]map[string]string, pushedAt map[string]int64) *cleanupFixture {
	t.Helper()
	ctx := context.Background()
	db := newTestDB(t)

	project, err := db.CreateProject(ctx, "team-a", "项目 A", "", domain.VisibilityPrivate, nil)
	if err != nil {
		t.Fatalf("创建项目: %v", err)
	}
	repo, err := db.CreateRepository(ctx, project.ID, "app", domain.VisibilityPrivate, "")
	if err != nil {
		t.Fatalf("创建仓库: %v", err)
	}

	// 全局默认策略。
	policy := &domain.RetentionPolicy{
		ScopeKey:    store.GlobalRetentionScope,
		Enabled:     true,
		Algorithm:   RetentionAlgorithmOR,
		TagSelector: "**",
		Rules:       rules,
	}
	if err := db.UpsertRetentionPolicy(ctx, policy); err != nil {
		t.Fatalf("写入策略: %v", err)
	}

	// tag 快照（推送时间）。
	//
	// ⚠️ 只给**pushedAt 里有的** tag 建快照行——
	//    没建行 = "推送时间未知"，那正是要验的场景。
	//    建了行却把时间写成 0 会变成"1970 年推的"，是另一回事。
	for tag, dg := range tags["team-a/app"] {
		at, ok := pushedAt[tag]
		if !ok {
			continue
		}
		if err := db.UpsertRetentionTag(ctx, &domain.RetentionTag{
			RepositoryID: repo.ID, Tag: tag, Digest: dg, PushedAt: &at,
		}); err != nil {
			t.Fatalf("写入 tag 快照: %v", err)
		}
	}

	up := newFakeUpstream()
	up.tags = tags
	srv := httptest.NewServer(up.handler())
	t.Cleanup(srv.Close)

	queue := NewRefreshQueue()
	t.Cleanup(queue.Close)
	refresher := NewRefresher(db, srv.Client(), srv.URL, newTestIssuer(t), queue, &fakeLogger{})

	cfg := config.Default()
	cfg.Cleanup.DryRun = false
	cfg.Cleanup.ProtectLatest = true

	svc := NewCleanupService(CleanupDeps{
		DB:           db,
		Refresher:    refresher,
		Logger:       &fakeLogger{},
		SettingsFn:   func() *config.Config { return cfg },
		RefreshQueue: queue,
	})

	return &cleanupFixture{db: db, svc: svc, up: up, repoID: repo.ID, projectN: "team-a"}
}

// TestCleanupRunDeletesOldTags 覆盖完整链路：评估 → 删 manifest → 更新快照。
func TestCleanupRunDeletesOldTags(t *testing.T) {
	now := time.Now().Unix()
	tags := map[string]map[string]string{
		"team-a/app": {
			"v1": "sha256:d1",
			"v2": "sha256:d2",
			"v3": "sha256:d3",
		},
	}
	pushedAt := map[string]int64{
		"v1": now - 7200,
		"v2": now - 3600,
		"v3": now,
	}
	fx := newCleanupFixture(t, []domain.RetentionRule{
		{Template: domain.RuleKeepLatestK, Params: map[string]int{"k": 1}},
	}, tags, pushedAt)

	run, err := fx.svc.Run(context.Background(), CleanupRunOptions{
		Trigger: domain.TriggerManual,
		DryRun:  false,
		Actor:   "admin",
	})
	if err != nil {
		t.Fatalf("执行清理失败: %v", err)
	}
	if run.Status != domain.CleanupStatusSuccess {
		t.Fatalf("状态 = %q（error=%q），期望 success", run.Status, run.Error)
	}
	if run.ManifestsDeleted != 2 {
		t.Fatalf("删掉的 manifest 数 = %d，期望 2", run.ManifestsDeleted)
	}
	if run.TagsDeleted != 2 {
		t.Fatalf("删掉的 tag 数 = %d，期望 2", run.TagsDeleted)
	}
	if run.TagsKept != 1 {
		t.Fatalf("保留的 tag 数 = %d，期望 1", run.TagsKept)
	}

	// 上游只该剩最新的那个 tag。
	left := fx.up.remainingTags("team-a/app")
	if len(left) != 1 || left[0] != "v3" {
		t.Fatalf("上游剩下的 tag = %v，期望只有 v3", left)
	}

	// 被删 tag 的快照也该清掉。
	snap, err := fx.db.RetentionTagsOf(context.Background(), fx.repoID)
	if err != nil {
		t.Fatalf("读取快照: %v", err)
	}
	if len(snap) != 1 || snap[0].Tag != "v3" {
		t.Fatalf("快照 = %+v，期望只剩 v3", snap)
	}

	// 执行记录要落库。
	stored, err := fx.db.CleanupRunByID(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("读取执行记录: %v", err)
	}
	if stored == nil || stored.Status != domain.CleanupStatusSuccess {
		t.Fatalf("执行记录 = %+v", stored)
	}
}

// TestCleanupRunDryRunDeletesNothing 覆盖试运行：只评估不删。
func TestCleanupRunDryRunDeletesNothing(t *testing.T) {
	now := time.Now().Unix()
	tags := map[string]map[string]string{
		"team-a/app": {"v1": "sha256:d1", "v2": "sha256:d2"},
	}
	pushedAt := map[string]int64{"v1": now - 7200, "v2": now}
	fx := newCleanupFixture(t, []domain.RetentionRule{
		{Template: domain.RuleKeepLatestK, Params: map[string]int{"k": 1}},
	}, tags, pushedAt)

	run, err := fx.svc.Run(context.Background(), CleanupRunOptions{
		Trigger: domain.TriggerManual,
		DryRun:  true,
		Actor:   "admin",
	})
	if err != nil {
		t.Fatalf("执行清理失败: %v", err)
	}
	if !run.DryRun {
		t.Fatal("dry_run 标记没生效")
	}
	if run.ManifestsDeleted != 1 {
		t.Fatalf("试运行报告的待删数 = %d，期望 1", run.ManifestsDeleted)
	}
	// 关键：**上游一个都不能少**。
	if left := fx.up.remainingTags("team-a/app"); len(left) != 2 {
		t.Fatalf("上游剩下 %v，试运行不该删任何东西", left)
	}
	if del := fx.up.deletedList(); len(del) != 0 {
		t.Fatalf("试运行却真的删了: %v", del)
	}
}

// TestCleanupRunFailsWhenDeleteFails 覆盖"部分失败 → 整体 failed"。
//
// 失败不许伪装成成功（`03-registry.md` §5.7）。
func TestCleanupRunFailsWhenDeleteFails(t *testing.T) {
	now := time.Now().Unix()
	tags := map[string]map[string]string{
		"team-a/app": {"v1": "sha256:d1", "v2": "sha256:d2"},
	}
	pushedAt := map[string]int64{"v1": now - 7200, "v2": now}
	fx := newCleanupFixture(t, []domain.RetentionRule{
		{Template: domain.RuleKeepLatestK, Params: map[string]int{"k": 1}},
	}, tags, pushedAt)
	fx.up.failDelete = true

	run, err := fx.svc.Run(context.Background(), CleanupRunOptions{
		Trigger: domain.TriggerManual, DryRun: false, Actor: "admin",
	})
	if err != nil {
		t.Fatalf("执行应当返回错误: %v", err)
	}
	if run == nil {
		t.Fatal("执行失败时也要返回执行记录")
	}
	if run.Status != domain.CleanupStatusFailed {
		t.Fatalf("状态 = %q，期望 failed（删除失败不许伪装成成功）", run.Status)
	}
	if run.FailedCount == 0 {
		t.Fatal("失败计数为 0")
	}
}

// TestCleanupRunKeepsUnknownTimeTags 覆盖"推送时间未知 → 保留"。
//
// 端到端地验证：上游有一个 tag，而快照里没有它（推送时间未知），
// 清理**不能**删它。
func TestCleanupRunKeepsUnknownTimeTags(t *testing.T) {
	now := time.Now().Unix()
	tags := map[string]map[string]string{
		"team-a/app": {"known": "sha256:d1", "unknown": "sha256:d2"},
	}
	// 快照里只有 known。
	pushedAt := map[string]int64{"known": now}
	fx := newCleanupFixture(t, []domain.RetentionRule{
		{Template: domain.RuleKeepLatestK, Params: map[string]int{"k": 1}},
	}, tags, pushedAt)

	run, err := fx.svc.Run(context.Background(), CleanupRunOptions{
		Trigger: domain.TriggerManual, DryRun: false, Actor: "admin",
	})
	if err != nil {
		t.Fatalf("执行清理失败: %v", err)
	}
	if run.SkippedUnknownTime != 1 {
		t.Fatalf("跳过（时间未知）计数 = %d，期望 1", run.SkippedUnknownTime)
	}
	// known 保留（最新），unknown 也保留（时间未知）。
	if left := fx.up.remainingTags("team-a/app"); len(left) != 2 {
		t.Fatalf("上游剩下 %v，两个 tag 都该保留", left)
	}
	if run.ManifestsDeleted != 0 {
		t.Fatalf("删掉了 %d 个 manifest，期望 0", run.ManifestsDeleted)
	}
}

// TestCleanupRunSkipsWhenPolicyDisabled 覆盖"策略没启用 → 什么都不删"。
func TestCleanupRunSkipsWhenPolicyDisabled(t *testing.T) {
	now := time.Now().Unix()
	tags := map[string]map[string]string{
		"team-a/app": {"v1": "sha256:d1"},
	}
	pushedAt := map[string]int64{"v1": now - 7200}
	fx := newCleanupFixture(t, []domain.RetentionRule{
		{Template: domain.RuleKeepLatestK, Params: map[string]int{"k": 0}},
	}, tags, pushedAt)
	// 关掉策略。
	policy, err := fx.db.RetentionPolicyByScope(context.Background(), store.GlobalRetentionScope)
	if err != nil {
		t.Fatalf("读策略: %v", err)
	}
	policy.Enabled = false
	if err := fx.db.UpsertRetentionPolicy(context.Background(), policy); err != nil {
		t.Fatalf("写策略: %v", err)
	}

	run, err := fx.svc.Run(context.Background(), CleanupRunOptions{
		Trigger: domain.TriggerManual, DryRun: false, Actor: "admin",
	})
	if err != nil {
		t.Fatalf("执行清理失败: %v", err)
	}
	if run.ManifestsDeleted != 0 {
		t.Fatalf("删掉了 %d 个 manifest，期望 0", run.ManifestsDeleted)
	}
	if left := fx.up.remainingTags("team-a/app"); len(left) != 1 {
		t.Fatalf("上游剩下 %v，不该删", left)
	}
}

// TestCleanupRunRejectsConcurrentRun 覆盖"同一时刻只允许一次清理"。
func TestCleanupRunRejectsConcurrentRun(t *testing.T) {
	now := time.Now().Unix()
	tags := map[string]map[string]string{"team-a/app": {"v1": "sha256:d1"}}
	pushedAt := map[string]int64{"v1": now}
	fx := newCleanupFixture(t, []domain.RetentionRule{
		{Template: domain.RuleKeepLatestK, Params: map[string]int{"k": 1}},
	}, tags, pushedAt)

	// 直接占住执行权，模拟"另一次清理正在跑"。
	if !fx.svc.guard.acquire() {
		t.Fatal("第一次抢占应当成功")
	}
	defer fx.svc.guard.release()

	_, err := fx.svc.Run(context.Background(), CleanupRunOptions{
		Trigger: domain.TriggerManual, DryRun: false, Actor: "admin",
	})
	if err == nil {
		t.Fatal("已有清理在跑时应当报错")
	}
	if err != ErrCleanupBusy {
		t.Fatalf("错误 = %v，期望 ErrCleanupBusy", err)
	}
}
