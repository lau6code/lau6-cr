package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/config"
	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// 保留策略的执行器。
//
// 设计依据见 docs/design/03-registry.md §5。

// resolveSettings 取当前生效配置。
//
// ⚠️ 取配置函数可能为 nil（测试里不注入）——此时用内置默认值，不能因为"没注入配置"就 panic。
//
// 这是**已声明的降级**：拿不到配置时用一份合法的默认配置继续跑，而不是让清理/回收整轮失败（那等于什么都不做）。
func resolveSettings(fn func() *config.Config) *config.Config {
	if fn == nil {
		return config.Default()
	}
	return fn()
}

// ErrCleanupBusy 表示已有一次清理在跑。
//
// 清理之间**不排队**：两次清理叠在一起没有意义，
// 后一次直接告诉调用方"已有任务在跑"。
var ErrCleanupBusy = errors.New("已有清理任务在运行")

// CleanupRunOptions 是一次清理的参数。
type CleanupRunOptions struct {
	// Trigger 是触发方式：schedule / manual / push。
	Trigger string
	// ProjectName 非空表示只清理该项目；空表示全部项目。
	ProjectName string
	// DryRun 为 true 时只评估不删除。
	DryRun bool
	// Actor 是审计里的操作者。
	Actor string
	// ClientIP 是审计里的来源地址。
	ClientIP string
}

// CleanupDeps 是 CleanupService 的依赖。
type CleanupDeps struct {
	DB           *store.DB
	Refresher    *Refresher
	Logger       Logger
	SettingsFn   func() *config.Config
	RefreshQueue *RefreshQueue
	// GC 是第 ② 层 blob 回收的执行器。
	//
	// 为 nil 时不做自动回收（保留策略照常工作，只是空间不回收）。
	GC *GCService
}

// CleanupService 执行保留策略。
type CleanupService struct {
	db           *store.DB
	refresher    *Refresher
	logger       Logger
	settingsFn   func() *config.Config
	refreshQueue *RefreshQueue
	gc           *GCService

	// guard 保证同一时刻只允许一次清理。
	guard runGuard

	// pushMu 保护 pushAt（push 触发的冷却记录）。
	pushMu sync.Mutex
	pushAt map[int64]time.Time
}

// NewCleanupService 构造清理执行器。
func NewCleanupService(d CleanupDeps) *CleanupService {
	return &CleanupService{
		db:           d.DB,
		refresher:    d.Refresher,
		logger:       d.Logger,
		settingsFn:   d.SettingsFn,
		refreshQueue: d.RefreshQueue,
		gc:           d.GC,
		pushAt:       map[int64]time.Time{},
	}
}

// Run 执行一次清理。
//
// 返回的 run 里带完整的统计与明细。执行失败时返回 error，
// 但 run 仍然带回（里面有失败明细）——**部分成功不伪装成成功**。
func (s *CleanupService) Run(ctx context.Context, opts CleanupRunOptions) (*domain.CleanupRun, error) {
	if !s.guard.acquire() {
		return nil, ErrCleanupBusy
	}
	defer s.guard.release()

	run := &domain.CleanupRun{
		Trigger:     opts.Trigger,
		Scope:       "all",
		ProjectName: opts.ProjectName,
		DryRun:      opts.DryRun,
		Status:      domain.CleanupStatusRunning,
		StartedAt:   time.Now().Unix(),
	}
	if opts.ProjectName != "" {
		run.Scope = "project:" + opts.ProjectName
	}
	id, err := s.db.CreateCleanupRun(ctx, run)
	if err != nil {
		return nil, fmt.Errorf("创建清理执行记录失败: %w", err)
	}
	run.ID = id

	// 审计写失败**不能只记日志**：这次运行在审计链上就有洞了，
	// 而清理动作已经发生了、回滚不了。错误先记下来，
	// 收尾时落进 run.Status / run.Error——执行历史里必须看得见。
	auditErr := s.db.WriteAudit(ctx, auditActor(opts.Actor), "cleanup.run.started", run.Scope, opts.ProjectName,
		opts.ClientIP, true, map[string]any{"dry_run": opts.DryRun, "trigger": opts.Trigger})
	if auditErr != nil {
		s.logger.Error("写审计失败", "action", "cleanup.run.started", "err", auditErr)
	}

	detail := &domain.CleanupDetail{}
	runErr := s.execute(ctx, opts, run, detail)
	if runErr == nil && auditErr != nil {
		runErr = fmt.Errorf("写审计记录失败: %w", auditErr)
	}

	// ② blob 回收（第 ② 层）：tag 删了之后把磁盘空间收回来。
	//
	// ⚠️ 回收失败会让整次执行落成 failed——
	//    "tag 删了但空间没回来"必须让人看见，不能装作成功（§5.7）。
	//    试运行的清理什么都没删，不跟着扫一遍全盘。
	if runErr == nil && !opts.DryRun && s.gc != nil {
		gcRun, gcErr := s.gc.TriggerAfterCleanup(ctx, auditActor(opts.Actor))
		if gcRun != nil {
			detail.GC = &domain.CleanupGC{
				RunID:          gcRun.ID,
				Status:         gcRun.Status,
				DryRun:         gcRun.DryRun,
				BlobsDeleted:   gcRun.BlobsDeleted,
				BytesReclaimed: gcRun.BytesReclaimed,
				Error:          gcRun.Error,
			}
		}
		if gcErr != nil {
			runErr = fmt.Errorf("tag 清理已完成，但 blob 回收失败: %w", gcErr)
		}
	}

	if b, err := json.Marshal(detail); err == nil {
		run.Detail = string(b)
	} else {
		// 明细序列化失败不该让整次清理"看起来成功"——
		// 记进 error，让状态落到 failed。
		if runErr == nil {
			runErr = fmt.Errorf("序列化清理明细失败: %w", err)
		}
	}

	if runErr != nil {
		run.Status = domain.CleanupStatusFailed
		run.Error = runErr.Error()
	} else if run.FailedCount > 0 {
		// 有条目删除失败 → 整体 failed（§5.7）。
		run.Status = domain.CleanupStatusFailed
		run.Error = fmt.Sprintf("%d 个条目删除失败", run.FailedCount)
	} else {
		run.Status = domain.CleanupStatusSuccess
	}
	// 审计写失败也算失败：tag 删了但审计链上没有痕迹，
	// 这次执行不能报成功——单列一栏在 detail.audit_failures 里。
	if len(detail.AuditFailures) > 0 {
		run.Status = domain.CleanupStatusFailed
		msg := fmt.Sprintf("%d 个条目已删除但审计记录写入失败", len(detail.AuditFailures))
		if run.Error == "" {
			run.Error = msg
		} else {
			run.Error = run.Error + "；" + msg
		}
	}

	action := "cleanup.run.completed"
	if run.Status == domain.CleanupStatusFailed {
		action = "cleanup.run.failed"
	}
	// ⚠️ 完成审计写在**回写执行记录之前**：审计写失败时还能把这次运行
	// 落成 failed，让"审计链有洞"在执行历史里看得见，
	// 而不是只躺在日志里没人知道。
	if err := s.db.WriteAudit(ctx, auditActor(opts.Actor), action, run.Scope, opts.ProjectName,
		opts.ClientIP, run.Status == domain.CleanupStatusSuccess, map[string]any{
			"tags_deleted":      run.TagsDeleted,
			"manifests_deleted": run.ManifestsDeleted,
			"tags_kept":         run.TagsKept,
			"failed_count":      run.FailedCount,
			"dry_run":           run.DryRun,
			"error":             run.Error,
		}); err != nil {
		s.logger.Error("写审计失败", "action", action, "err", err)
		if runErr == nil {
			runErr = fmt.Errorf("写审计记录失败: %w", err)
		}
		run.Status = domain.CleanupStatusFailed
		if run.Error == "" {
			run.Error = fmt.Sprintf("写审计记录失败: %v", err)
		}
	}

	if err := s.db.FinishCleanupRun(ctx, run); err != nil {
		// 收尾写库失败是**真失败**，必须让调用方知道：
		// 否则执行历史里会永远留着一条 running 的记录。
		if runErr == nil {
			runErr = fmt.Errorf("回写清理执行记录失败: %w", err)
		}
		run.Status = domain.CleanupStatusFailed
	}

	s.logger.Info("镜像清理执行完成",
		"scope", run.Scope, "dry_run", run.DryRun, "status", run.Status,
		"tags_deleted", run.TagsDeleted, "manifests_deleted", run.ManifestsDeleted,
		"tags_kept", run.TagsKept, "failed", run.FailedCount, "err", run.Error)

	return run, runErr
}

// Settings 返回当前生效配置（供 handler 取热改后的运行参数）。
func (s *CleanupService) Settings() *config.Config {
	return s.settings()
}

func (s *CleanupService) settings() *config.Config {
	return resolveSettings(s.settingsFn)
}

func (s *CleanupService) execute(ctx context.Context, opts CleanupRunOptions,
	run *domain.CleanupRun, detail *domain.CleanupDetail) error {

	cfg := s.settings()
	now := time.Now()

	projects, err := s.db.ListProjects(ctx, nil, nil)
	if err != nil {
		return fmt.Errorf("读取项目列表失败: %w", err)
	}

	globalPolicy, err := s.db.RetentionPolicyByScope(ctx, store.GlobalRetentionScope)
	if err != nil {
		return fmt.Errorf("读取全局保留策略失败: %w", err)
	}

	for i := range projects {
		project := &projects[i]
		if opts.ProjectName != "" && project.Name != opts.ProjectName {
			continue
		}

		policy, err := s.policyFor(ctx, project, globalPolicy)
		if err != nil {
			return err
		}
		// 没策略 / 没启用 → 这个项目不动（§5.2.4 ④）。
		if policy == nil || !policy.Enabled || len(policy.Rules) == 0 {
			continue
		}

		repos, err := s.db.ListRepositoriesInProject(ctx, project.ID)
		if err != nil {
			// 项目级读仓库失败 → 记为整体失败，不能当作"这个项目没仓库"。
			return fmt.Errorf("读取项目 %s 的仓库失败: %w", project.Name, err)
		}
		for j := range repos {
			repo := &repos[j]
			if err := s.cleanRepository(ctx, opts, policy, repo, cfg, now, run, detail); err != nil {
				return err
			}
		}
	}
	return nil
}

// policyFor：项目级策略优先，其次全局默认。
func (s *CleanupService) policyFor(ctx context.Context, project *domain.Project,
	global *domain.RetentionPolicy) (*domain.RetentionPolicy, error) {

	policy, err := s.db.RetentionPolicyByProject(ctx, project.ID)
	if err != nil {
		return nil, fmt.Errorf("读取项目 %s 的保留策略失败: %w", project.Name, err)
	}
	if policy != nil {
		policy.ProjectName = project.Name
		return policy, nil
	}
	return global, nil
}

func (s *CleanupService) cleanRepository(ctx context.Context, opts CleanupRunOptions,
	policy *domain.RetentionPolicy, repo *domain.Repository, cfg *config.Config, now time.Time,
	run *domain.CleanupRun, detail *domain.CleanupDetail) error {

	full := repo.FullName

	// ① 以上游为准取当前 tag 与 digest（本地快照可能过期）。
	upstreamTags, err := s.refresher.TagDigests(ctx, full)
	if err != nil {
		// 拉不到 tag 列表 → 这个仓库整体跳过，**一个都不删**。
		run.FailedCount++
		detail.Failures = append(detail.Failures, domain.CleanupFailure{
			Repo: full, Error: "读取上游 tag 列表失败: " + err.Error(),
		})
		s.logger.Error("清理跳过仓库：读取上游 tag 列表失败", "repo", full, "err", err)
		return nil
	}

	// ② 合并本地快照里的推送时间。
	snapshot, err := s.db.RetentionTagsOf(ctx, repo.ID)
	if err != nil {
		return fmt.Errorf("读取仓库 %s 的 tag 快照失败: %w", full, err)
	}
	snapByTag := make(map[string]domain.RetentionTag, len(snapshot))
	for _, t := range snapshot {
		snapByTag[t.Tag] = t
	}

	// 把**上游已没有**的快照行清掉。
	//
	// 残留快照会让"删掉又用同一个 digest 重建"的 tag 带着过期的推送时间，
	// 于是它显得更老、更容易被删——危险的方向（§5.2.4 ②）。
	upstreamNames := make([]string, 0, len(upstreamTags))
	for _, ut := range upstreamTags {
		upstreamNames = append(upstreamNames, ut.Tag)
	}
	if _, err := s.db.PruneRetentionTagsNotIn(ctx, repo.ID, upstreamNames); err != nil {
		// ⚠️ **这次失败可以忽略**：
		//    残留行对应的 tag 已经不在上游列表里，而规则求值只看
		//    **上游当前真实**的 tag 集合（§5.2.5 第 1 步），
		//    所以残留**不影响本次清理的判定**。
		//
		//    残留本身是**自愈**的：下一轮清理开头的 prune 会把它清掉。
		//    唯一要留意的是"删掉又用同一个 digest 重建"的 tag——
		//    残留会让它带着过期的推送时间、显得更老（危险的方向，
		//    见 store 侧 PruneRetentionTagsNotIn 的注释）。
		//    经 lau6-cr 重新推送时，代理层观测到的 push 会用新的 pushed_at
		//    覆盖这一行（UpsertRetentionTag），正常路径下不会出事。
		//
		//    显式容忍，但**不能悄悄吞掉**：记 Error 级日志留痕。
		s.logger.Error("清理 tag 快照残留失败", "repo", full, "err", err)
	}

	tags := make([]domain.RetentionTag, 0, len(upstreamTags))
	for _, ut := range upstreamTags {
		t := ut
		if st, ok := snapByTag[ut.Tag]; ok {
			// ⚠️ digest 变了说明这个 tag 被重新推过，而我们不知道什么时候推的。
			//    这时推送时间**必须当未知**——沿用旧时间会让它看起来更老，
			//    于是更容易被删，方向是危险的（§5.2.4 ②）。
			if st.Digest == ut.Digest {
				t.PushedAt = st.PushedAt
			}
			t.Size = st.Size
		}
		tags = append(tags, t)
	}

	// ③ 规则求值。
	decision, err := EvaluateRetention(policy, full, tags, now, cfg.Cleanup.ProtectLatest)
	if err != nil {
		return fmt.Errorf("评估仓库 %s 的保留策略失败: %w", full, err)
	}
	run.TagsKept += len(decision.Keep)
	for _, sk := range decision.Skipped {
		switch sk.Reason {
		case SkipReasonUnknownTime:
			run.SkippedUnknownTime++
		case SkipReasonSharedDigest:
			run.SkippedSharedDigest++
		}
	}
	detail.Skipped = append(detail.Skipped, decision.Skipped...)

	// ④ 执行删除。
	for _, cand := range decision.Delete {
		if opts.DryRun {
			detail.Deleted = append(detail.Deleted, domain.CleanupDeleted{
				Repo: full, Tags: cand.Tags, Digest: cand.Digest, Size: cand.Size,
			})
			run.TagsDeleted += len(cand.Tags)
			run.ManifestsDeleted++
			continue
		}
		if err := s.refresher.DeleteTag(ctx, full, cand.Digest); err != nil {
			// 单个删除失败 → 记明细，继续处理其余。
			// 整体状态会在 Run 里按 FailedCount 落成 failed。
			run.FailedCount++
			detail.Failures = append(detail.Failures, domain.CleanupFailure{
				Repo: full, Tag: cand.Tags[0], Digest: cand.Digest, Error: err.Error(),
			})
			s.logger.Error("清理删除 manifest 失败", "repo", full, "digest", cand.Digest, "err", err)
			continue
		}
		run.TagsDeleted += len(cand.Tags)
		run.ManifestsDeleted++
		detail.Deleted = append(detail.Deleted, domain.CleanupDeleted{
			Repo: full, Tags: cand.Tags, Digest: cand.Digest, Size: cand.Size,
		})
		// 快照同步删除，避免下次评估还看到已经不存在的 tag。
		for _, tag := range cand.Tags {
			if err := s.db.DeleteRetentionTag(ctx, repo.ID, tag); err != nil {
				// ⚠️ **这次失败可以忽略**：manifest 已经删掉了，
				//    残留的快照行**不影响本次清理的判定**
				//    （判定只看上游当前真实的 tag 集合，§5.2.5 第 1 步），
				//    而且它是**自愈**的——下一轮清理开头的 prune
				//    （PruneRetentionTagsNotIn）会把这个 tag 的残留行清掉：
				//    它已经不在上游列表里了。
				//    期间若该 tag 被重新推送，代理层观测到的 push 会用新的
				//    pushed_at 覆盖这一行（UpsertRetentionTag）。
				//
				//    显式容忍，但**不能悄悄吞掉**：记 Error 级日志留痕。
				s.logger.Error("清理后删除 tag 快照失败", "repo", full, "tag", tag, "err", err)
			}
		}
		// 审计写失败 → 这次删除在审计链上是空的。
		// tag 已经删掉了（回滚不了），所以**不中断整次清理**——
		// 但必须单列一条明细、并让本次执行落成 failed，
		// 否则它跟"删干净了"在执行历史里长得一模一样。
		if err := s.db.WriteAudit(ctx, "system", "cleanup.tag.deleted", full+":"+cand.Tags[0], "",
			opts.ClientIP, true, map[string]any{"digest": cand.Digest, "tags": cand.Tags}); err != nil {
			s.logger.Error("写审计失败", "action", "cleanup.tag.deleted", "repo", full, "err", err)
			detail.AuditFailures = append(detail.AuditFailures, domain.CleanupFailure{
				Repo: full, Tag: cand.Tags[0], Digest: cand.Digest,
				Error: "写审计记录失败: " + err.Error(),
			})
		}
		// 目录缓存里的 tag 数/大小已经过期，排一次刷新。
		if s.refreshQueue != nil {
			s.refreshQueue.Enqueue(repo)
		}
	}
	return nil
}
