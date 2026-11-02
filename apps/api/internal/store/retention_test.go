package store

import (
	"context"
	"testing"

	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
)

// 保留策略与清理执行记录的持久化测试。

// TestRetentionPolicyUpsertAndRead 覆盖策略的写入与读取。
func TestRetentionPolicyUpsertAndRead(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	project, err := db.CreateProject(ctx, "team-a", "项目 A", "", domain.VisibilityPrivate, nil)
	if err != nil {
		t.Fatalf("创建项目: %v", err)
	}

	// 全局默认。
	global := &domain.RetentionPolicy{
		ScopeKey:    GlobalRetentionScope,
		Enabled:     true,
		Algorithm:   "or",
		TagSelector: "**",
		Rules: []domain.RetentionRule{
			{Template: domain.RuleKeepLatestK, Params: map[string]int{"k": 5}},
		},
	}
	if err := db.UpsertRetentionPolicy(ctx, global); err != nil {
		t.Fatalf("写入全局策略: %v", err)
	}
	got, err := db.RetentionPolicyByScope(ctx, GlobalRetentionScope)
	if err != nil {
		t.Fatalf("读取全局策略: %v", err)
	}
	if got == nil {
		t.Fatal("全局策略读不到")
	}
	if !got.Enabled || len(got.Rules) != 1 {
		t.Fatalf("全局策略 = %+v，期望启用且 1 条规则", got)
	}
	if got.Rules[0].Template != domain.RuleKeepLatestK || got.Rules[0].Params["k"] != 5 {
		t.Fatalf("规则 = %+v，参数没存回来", got.Rules[0])
	}

	// 项目级策略。
	projPolicy := &domain.RetentionPolicy{
		ScopeKey:    ProjectRetentionScope(project.ID),
		ProjectID:   &project.ID,
		Enabled:     true,
		Algorithm:   "or",
		TagSelector: "release-*",
		Rules: []domain.RetentionRule{
			{Template: domain.RuleKeepAlways, Params: map[string]int{}},
		},
	}
	if err := db.UpsertRetentionPolicy(ctx, projPolicy); err != nil {
		t.Fatalf("写入项目策略: %v", err)
	}
	p, err := db.RetentionPolicyByProject(ctx, project.ID)
	if err != nil {
		t.Fatalf("读取项目策略: %v", err)
	}
	if p == nil {
		t.Fatal("项目策略读不到")
	}
	if p.TagSelector != "release-*" || len(p.Rules) != 1 {
		t.Fatalf("项目策略 = %+v", p)
	}

	// 重复写入同一 scope 是覆盖而不是插新行。
	if err := db.UpsertRetentionPolicy(ctx, projPolicy); err != nil {
		t.Fatalf("覆盖项目策略: %v", err)
	}
	all, err := db.ListRetentionPolicies(ctx)
	if err != nil {
		t.Fatalf("列出策略: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("策略数 = %d，期望 2", len(all))
	}

	// 删除项目策略后回落到"没有项目级策略"。
	if err := db.DeleteRetentionPolicy(ctx, ProjectRetentionScope(project.ID)); err != nil {
		t.Fatalf("删除项目策略: %v", err)
	}
	p, err = db.RetentionPolicyByProject(ctx, project.ID)
	if err != nil {
		t.Fatalf("读取项目策略: %v", err)
	}
	if p != nil {
		t.Fatalf("项目策略已删除却还读得到: %+v", p)
	}
}

// TestRetentionTagPushedAtDirection 覆盖推送时间的**方向性**。
//
// 推送时间未知时：
//
//	· digest 没变 → 保留旧时间（还是同一个镜像）；
//	· digest 变了 → **清空**（我们不知道它什么时候被重新推的，
//	  沿用旧时间会让它看起来更老，于是更容易被删——危险的方向）。
func TestRetentionTagPushedAtDirection(t *testing.T) {
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

	// 已知推送时间。
	known := int64(1700000000)
	if err := db.UpsertRetentionTag(ctx, &domain.RetentionTag{
		RepositoryID: repo.ID, Tag: "v1", Digest: "sha256:a", PushedAt: &known,
	}); err != nil {
		t.Fatalf("写入 tag 快照: %v", err)
	}

	// digest 未变、时间未知 → 保留旧时间。
	if err := db.UpsertRetentionTag(ctx, &domain.RetentionTag{
		RepositoryID: repo.ID, Tag: "v1", Digest: "sha256:a",
	}); err != nil {
		t.Fatalf("更新 tag 快照: %v", err)
	}
	got, err := db.RetentionTag(ctx, repo.ID, "v1")
	if err != nil {
		t.Fatalf("读取 tag 快照: %v", err)
	}
	if got.PushedAt == nil || *got.PushedAt != known {
		t.Fatalf("digest 未变时推送时间应保留，得到 %+v", got.PushedAt)
	}

	// digest 变了、时间未知 → 清空。
	if err := db.UpsertRetentionTag(ctx, &domain.RetentionTag{
		RepositoryID: repo.ID, Tag: "v1", Digest: "sha256:b",
	}); err != nil {
		t.Fatalf("更新 tag 快照: %v", err)
	}
	got, err = db.RetentionTag(ctx, repo.ID, "v1")
	if err != nil {
		t.Fatalf("读取 tag 快照: %v", err)
	}
	if got.PushedAt != nil {
		t.Fatalf("digest 变了却保留了旧推送时间 %d——它会显得更老，更容易被删", *got.PushedAt)
	}
	if got.Digest != "sha256:b" {
		t.Fatalf("digest = %q，期望 sha256:b", got.Digest)
	}
}

// TestCleanupRunLifecycle 覆盖执行记录的创建与收尾。
func TestCleanupRunLifecycle(t *testing.T) {
	ctx := context.Background()
	db := newTestDB(t)

	run := &domain.CleanupRun{
		Trigger: domain.TriggerManual,
		Scope:   "all",
		DryRun:  true,
		Status:  domain.CleanupStatusRunning,
	}
	id, err := db.CreateCleanupRun(ctx, run)
	if err != nil {
		t.Fatalf("创建执行记录: %v", err)
	}
	if id <= 0 {
		t.Fatalf("执行记录 ID = %d", id)
	}

	run.ID = id
	run.Status = domain.CleanupStatusFailed
	run.TagsDeleted = 3
	run.ManifestsDeleted = 2
	run.TagsKept = 7
	run.FailedCount = 1
	run.Error = "1 个条目删除失败"
	run.Detail = `{"failures":[{"repo":"team-a/app","error":"boom"}]}`
	if err := db.FinishCleanupRun(ctx, run); err != nil {
		t.Fatalf("收尾执行记录: %v", err)
	}

	got, err := db.CleanupRunByID(ctx, id)
	if err != nil {
		t.Fatalf("读取执行记录: %v", err)
	}
	if got == nil {
		t.Fatal("执行记录读不到")
	}
	if got.Status != domain.CleanupStatusFailed {
		t.Fatalf("status = %q，期望 failed", got.Status)
	}
	if got.TagsDeleted != 3 || got.ManifestsDeleted != 2 || got.TagsKept != 7 {
		t.Fatalf("计数不对: %+v", got)
	}
	if !got.DryRun {
		t.Fatal("dry_run 没存回来")
	}
	if got.FinishedAt == nil {
		t.Fatal("finished_at 为空")
	}

	list, err := db.ListCleanupRuns(ctx, 10, 0)
	if err != nil {
		t.Fatalf("列出执行记录: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("执行记录数 = %d，期望 1", len(list))
	}
}
