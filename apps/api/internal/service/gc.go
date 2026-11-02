package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/config"
	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
	"github.com/lau6code/lau6-cr/apps/api/internal/gc"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// blob 回收（第 ② 层）的执行器。
//
// 设计依据见 docs/design/03-registry.md §5.5。
//
// 回收动作由 **distribution 自己**执行（`distribution garbage-collect`），
// lau6-cr 只负责调度、关闸与记账——
// 对象存储下没有本地目录可扫，只有上游那份实现知道
// "哪些 blob 还被引用"（11-storage.md §7）。

// ErrGCBusy 表示已有一次 blob 回收在跑。
//
// 回收之间**不排队**：两次扫盘叠在一起没有意义，
// 后一次直接告诉调用方"已有任务在跑"。
var ErrGCBusy = errors.New("已有 blob 回收任务在运行")

// ErrGCDisabled 表示 blob 回收总开关未打开。
var ErrGCDisabled = errors.New("blob 回收未启用")

// GCDeps 是 GCService 的依赖。
type GCDeps struct {
	DB         *store.DB
	Logger     Logger
	SettingsFn func() *config.Config
	// Barrier 是仓库写屏障：GC 期间挂起写操作。
	//
	// 它同时被 HTTP 层的 /v2/* 写路径使用——
	// 屏障必须与"判断某个请求是不是写请求"的地方共享同一个实例。
	Barrier *gc.Barrier
	// Executor 调度 distribution 的 garbage-collect。
	//
	// 为 nil 时回收不可用（没有 distribution 可调度）。
	Executor *gc.Executor
	// Ctx 是**进程生命周期**的上下文：后台回收挂在它下面。
	//
	// ⚠️ 不能用请求的 ctx：请求一返回就被取消，
	//    跟着取消的回收会在中途夭折。
	//
	// ⚠️ 也不能用 Background：那样 lau6-cr 退出后，
	//    回收子进程会**继续删 blob**（孤儿进程）。
	//    进程退出时 ctx 被取消 → 子进程随之被杀。
	//
	// 为 nil 时退化成 Background（测试里用）。
	Ctx context.Context
}

// GCService 执行 blob 回收。
type GCService struct {
	db         *store.DB
	logger     Logger
	settingsFn func() *config.Config
	barrier    *gc.Barrier
	executor   *gc.Executor
	// ctx 是后台回收用的进程生命周期上下文。
	ctx context.Context

	// guard 保证同一时刻只允许一次回收。
	guard runGuard
}

// NewGCService 构造 blob 回收执行器。
func NewGCService(d GCDeps) *GCService {
	barrier := d.Barrier
	if barrier == nil {
		barrier = gc.NewBarrier()
	}
	ctx := d.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return &GCService{
		db:         d.DB,
		logger:     d.Logger,
		settingsFn: d.SettingsFn,
		barrier:    barrier,
		executor:   d.Executor,
		ctx:        ctx,
	}
}

// GCRunOptions 是一次 blob 回收的参数。
type GCRunOptions struct {
	// Trigger 是触发方式：manual / after_cleanup。
	Trigger string
	// DryRun 为 true 时只评估不删除。
	// nil 表示取配置里的默认值（cleanup.gc.dry_run）。
	DryRun *bool
	// Actor 是审计里的操作者。
	Actor string
	// ClientIP 是审计里的来源地址。
	ClientIP string
}

// Settings 返回当前生效配置（供 handler 取热改后的运行参数）。
func (s *GCService) Settings() *config.Config {
	return s.settings()
}

func (s *GCService) settings() *config.Config {
	return resolveSettings(s.settingsFn)
}

// Run 启动一次 blob 回收，**立即返回**执行记录。
//
// ⚠️ 回收在后台跑。
//
// 理由：对象存储下 mark 阶段可能跑几十分钟到几小时（11-storage.md §7.1），
// 同步等会把 HTTP 请求拖到超时；而请求一断，请求的 ctx 就被取消——
// 跟着取消的回收会在中途夭折，执行记录永远停在 running。
//
// 动手前就能发现的问题（二进制/配置不可用）在这里**同步报错**；
// 跑起来之后的失败落在执行记录里（status=failed + error），
// 调用方从执行记录看得到——不是"假装成功"。
func (s *GCService) Run(ctx context.Context, opts GCRunOptions) (*domain.GCRun, error) {
	if !s.guard.acquire() {
		return nil, ErrGCBusy
	}

	cfg := s.settings()
	gcCfg := cfg.Cleanup.GC

	if !gcCfg.Enabled {
		s.guard.release()
		return nil, ErrGCDisabled
	}
	if s.executor == nil {
		s.guard.release()
		return nil, fmt.Errorf("blob 回收不可用：本进程没有可调度的 distribution。" +
			"独立部署 registry 的形态需要用 CR_DISTRIBUTION_CONFIG 指向 registry 自己的配置文件")
	}
	// 预检：distribution 二进制与配置必须在**动手前**就位，
	// 不能让它变成一个跑起来才失败的后台任务。
	if err := s.executor.Precheck(); err != nil {
		s.guard.release()
		return nil, err
	}

	dryRun := gcCfg.DryRun
	if opts.DryRun != nil {
		dryRun = *opts.DryRun
	}

	// 回收目标标识：本地存储是存储根目录，对象存储是 s3://<bucket>。
	//
	// ⚠️ 读不出来就留空——真正的失败原因由回收本身报出来，
	//    这里只是给执行记录留一个可读的标识。
	target := s.target()

	run := &domain.GCRun{
		Trigger:     opts.Trigger,
		DryRun:      dryRun,
		Status:      domain.GCStatusRunning,
		StorageRoot: target,
		StartedAt:   time.Now().Unix(),
	}
	id, err := s.db.CreateGCRun(ctx, run)
	if err != nil {
		s.guard.release()
		return nil, fmt.Errorf("创建 blob 回收执行记录失败: %w", err)
	}
	run.ID = id

	// 审计写失败**不能只记日志**：这次回收在审计链上就有洞了，
	// 而 blob 已经回收掉了、回滚不了。错误先记下来，
	// 收尾时落进 run.Status / run.Error——执行历史里必须看得见。
	auditErr := s.db.WriteAudit(ctx, auditActor(opts.Actor), "gc.run.started", target, "",
		opts.ClientIP, true, map[string]any{"dry_run": dryRun, "trigger": opts.Trigger})
	if auditErr != nil {
		s.logger.Error("写审计失败", "action", "gc.run.started", "err", auditErr)
	}

	// 后台执行。
	//
	// ⚠️ 用进程生命周期的 ctx（不是请求的 ctx）：请求一返回就被取消，
	//    跟着取消的回收会在中途夭折，执行记录永远停在 running。
	go s.run(run, target, dryRun, gcCfg, opts, auditErr)
	return run, nil
}

// ⚠️ 失败**不会**回到最初的调用方（那时它已经返回了），
//
//	所以失败必须落在执行记录与审计里——
//	否则就是"执行到一半失败，最终却显示成功"。
func (s *GCService) run(run *domain.GCRun, target string, dryRun bool, gcCfg config.GCConfig,
	opts GCRunOptions, startAuditErr error) {
	defer s.guard.release()
	ctx := s.ctx

	report, runErr := s.execute(ctx, dryRun, gcCfg)
	if report != nil {
		run.Repositories = report.Repositories
		run.ManifestsFound = report.ManifestsFound
		run.BlobsMarked = report.BlobsMarked
		run.BlobsScanned = report.BlobsScanned
		run.BlobsDeleted = report.BlobsDeleted
		run.BytesReclaimed = report.BytesReclaimed
		run.LayerLinksDeleted = report.LayerLinksDeleted
		run.UntaggedDeleted = report.UntaggedDeleted
	}
	switch {
	case runErr != nil:
		run.Status = domain.GCStatusFailed
		run.Error = runErr.Error()
	case startAuditErr != nil:
		run.Status = domain.GCStatusFailed
		run.Error = fmt.Sprintf("写审计记录失败: %v", startAuditErr)
	default:
		run.Status = domain.GCStatusSuccess
	}

	action := "gc.run.completed"
	if run.Status == domain.GCStatusFailed {
		action = "gc.run.failed"
	}
	// ⚠️ 完成审计写在**回写执行记录之前**：审计写失败时还能把这次回收
	// 落成 failed，让"审计链有洞"在执行历史里看得见，
	// 而不是只躺在日志里没人知道。
	if err := s.db.WriteAudit(ctx, auditActor(opts.Actor), action, target, "",
		opts.ClientIP, run.Status == domain.GCStatusSuccess, map[string]any{
			"blobs_deleted":       run.BlobsDeleted,
			"bytes_reclaimed":     run.BytesReclaimed,
			"layer_links_deleted": run.LayerLinksDeleted,
			"untagged_deleted":    run.UntaggedDeleted,
			"dry_run":             run.DryRun,
			"error":               run.Error,
		}); err != nil {
		s.logger.Error("写审计失败", "action", action, "err", err)
		run.Status = domain.GCStatusFailed
		if run.Error == "" {
			run.Error = fmt.Sprintf("写审计记录失败: %v", err)
		}
	}

	if err := s.db.FinishGCRun(ctx, run); err != nil {
		// 收尾写库失败是**真失败**，必须让调用方知道：
		// 否则执行历史里会永远留着一条 running 的记录。
		s.logger.Error("回写 blob 回收执行记录失败", "err", err)
		run.Status = domain.GCStatusFailed
		if run.Error == "" {
			run.Error = fmt.Sprintf("回写 blob 回收执行记录失败: %v", err)
		}
	}

	s.logger.Info("blob 回收执行完成",
		"target", target, "dry_run", dryRun, "status", run.Status,
		"blobs_deleted", run.BlobsDeleted, "bytes_reclaimed", run.BytesReclaimed,
		"layer_links_deleted", run.LayerLinksDeleted, "err", run.Error)
}

// 读不出回收目标就返回空串——真正的失败原因会由回收本身报出来，
// 这里只是给记录留个可读的标识。
func (s *GCService) target() string {
	if s.executor == nil {
		return ""
	}
	target, err := s.executor.Target()
	if err != nil {
		s.logger.Warn("读不出回收目标标识", "err", err)
		return ""
	}
	return target
}

func (s *GCService) execute(ctx context.Context, dryRun bool, cfg config.GCConfig) (*gc.Report, error) {
	// ① 关闸：GC 期间挂起仓库写操作，并等在途上传排空。
	//
	// 官方 GC 要求 registry 处于只读或停止状态，否则"上传中的镜像层可能被误删"。
	// lau6-cr 是唯一入口，所以停写这件事由它自己做（03-registry.md §5.5）。
	release, err := s.barrier.Block(ctx, cfg.DrainTimeout)
	if err != nil {
		return nil, fmt.Errorf("关闭仓库写入口失败: %w", err)
	}
	defer release()

	s.logger.Info("blob 回收开始",
		"target", s.target(), "dry_run", dryRun, "delete_untagged", cfg.DeleteUntagged)

	report, err := s.executor.Collect(ctx, gc.CollectOptions{
		DryRun:         dryRun,
		DeleteUntagged: cfg.DeleteUntagged,
	})
	if err != nil {
		return nil, err
	}
	return report, nil
}

// TriggerAfterCleanup 是保留策略执行完后的自动回收。
//
// ⚠️ 只在**真正删过东西**的清理之后触发：
//
//	试运行的清理什么都没删，跟着跑一次回收是白扫一遍全盘。
//
// ⚠️ 回收是**异步**的：这里返回时回收刚开始，结局落在回收自己的
//
//	执行记录里（status / 统计 / 失败原因）。
//	清理本身不因为回收失败而失败——两者是**两件事**
//	（第 ① 层删 tag、第 ② 层回收 blob），
//	但回收的失败**一定**看得见：回收执行记录 + 审计 + 日志。
func (s *GCService) TriggerAfterCleanup(ctx context.Context, actor string) (*domain.GCRun, error) {
	cfg := s.settings()
	if !cfg.Cleanup.GC.Enabled || !cfg.Cleanup.GC.TriggerAfterCleanup {
		return nil, nil
	}
	return s.Run(ctx, GCRunOptions{
		Trigger: domain.GCTriggerAfterCleanup,
		Actor:   actor,
	})
}
