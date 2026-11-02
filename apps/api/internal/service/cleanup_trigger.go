package service

import (
	"context"
	"errors"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
)

// 保留策略的**触发入口**：定时、push、手工。
//
// 执行主体在 cleanup.go——这里只负责"什么时候跑、跑哪个范围"。

// RunScheduler 启动定时清理协程。
//
// 每分钟醒一次，看是否到了清理周期——**不把周期写死成 ticker 的间隔**，
// 因为周期是热改设置项，改了要立刻生效。
func (s *CleanupService) RunScheduler(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cfg := s.settings()
			if !cfg.Cleanup.Enabled {
				continue
			}
			interval := time.Duration(cfg.Cleanup.IntervalMinutes) * time.Minute
			if interval <= 0 {
				continue
			}
			if time.Since(last) < interval {
				continue
			}
			last = time.Now()
			if _, err := s.Run(ctx, CleanupRunOptions{
				Trigger: domain.TriggerSchedule,
				DryRun:  cfg.Cleanup.DryRun,
				Actor:   "system",
			}); err != nil {
				if errors.Is(err, ErrCleanupBusy) {
					s.logger.Warn("定时清理被跳过：已有清理任务在运行")
					continue
				}
				s.logger.Error("定时清理失败", "err", err)
				continue
			}
		}
	}
}

// TriggerPush 是 push 成功后的清理触发。
//
// 只清理**被推的那个项目**，并带冷却——连续推 10 个 tag
// 不该触发 10 次全量扫描。
func (s *CleanupService) TriggerPush(ctx context.Context, projectID int64, projectName string) {
	cfg := s.settings()
	if !cfg.Cleanup.Enabled || !cfg.Cleanup.TriggerOnPush {
		return
	}
	cooldown := cfg.Cleanup.PushTriggerCooldown
	if cooldown < 0 {
		cooldown = 0
	}
	s.pushMu.Lock()
	last, ok := s.pushAt[projectID]
	if ok && time.Since(last) < cooldown {
		s.pushMu.Unlock()
		return
	}
	s.pushAt[projectID] = time.Now()
	s.pushMu.Unlock()

	// 请求的 ctx 在响应返回后就被取消了——
	// 清理要跑完，必须用不受请求生命周期约束的 ctx。
	bg := context.WithoutCancel(ctx)
	go func() {
		_, err := s.Run(bg, CleanupRunOptions{
			Trigger:     domain.TriggerPush,
			ProjectName: projectName,
			DryRun:      cfg.Cleanup.DryRun,
			Actor:       "system",
		})
		if err != nil {
			if errors.Is(err, ErrCleanupBusy) {
				s.logger.Warn("push 触发的清理被跳过：已有清理任务在运行", "project", projectName)
				return
			}
			s.logger.Error("push 触发的清理失败", "project", projectName, "err", err)
		}
	}()
}

func auditActor(actor string) string {
	if actor == "" {
		return "system"
	}
	return actor
}
