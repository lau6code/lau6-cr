package service

import (
	"context"
	"errors"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/config"
	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
)

// blob 回收的**每日定时调度**。
//
// 执行主体在 gc.go——这里只负责"什么时候跑"。

// scheduleDateLayout 是"今天"的日期格式，用来判断一天只跑一次。
const scheduleDateLayout = "2006-01-02"

// gcTickInterval 是定时回收调度器的醒来间隔。
//
// ⚠️ **必须是 10 秒，不能是 1 分钟**。
//
// 原因：ticker 的唤醒时刻 = 启动时刻 + k × 间隔，**间隔是整分钟时，
// 唤醒时刻的"分"永远等于启动时的分偏移**——配置成"02:00"的定时回收
// 在 18:22:30 启动的进程里**永远不会触发**（它只会在 :30 分醒）。
//
// 10 秒的间隔会让秒值在每个分钟内遍历 {0,10,…,50}，因此每个
// "小时:分钟"都必然被扫到一次，与启动时刻无关。
//
// 做成变量而不是常量：测试要把间隔缩到毫秒级。
var gcTickInterval = 10 * time.Second

// RunScheduler 启动每日定时回收。
//
// 调度方式是"每分钟醒一次，看当前时刻是否落在配置的时间点上"，
// 而不是把间隔写死进 ticker——时间点是**热改设置项**，改了要立刻生效
// （与保留策略的调度器同一套思路，03-registry.md §5.4）。
//
// ⚠️ 进程在时间点那一刻没跑着 → **那天不跑**（不做补跑）。
//
//	补跑意味着"重启一次就删一堆文件"，不是这里想要的语义。
//
// ⚠️ 定时回收**同样受 cleanup.gc.dry_run 约束**：
//
//	试运行模式下它只统计不删除，磁盘空间不会真的释放。
//	这一点会在日志与执行记录里写明，不让它变成"看起来在跑其实没删"。
func (s *GCService) RunScheduler(ctx context.Context) {
	ticker := time.NewTicker(gcTickInterval)
	defer ticker.Stop()
	// lastDate 是"最近一次**尝试**的日期"，保证一天只触发一次。
	lastDate := ""
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			cfg := s.settings()
			if !cfg.Cleanup.GC.Enabled || !cfg.Cleanup.GC.ScheduleEnabled {
				continue
			}
			hour, minute, err := config.ParseClock(cfg.Cleanup.GC.ScheduleAt)
			if err != nil {
				// 启动时的校验会拦住非法值；这里兜的是**热改**进来的坏值。
				s.logger.Error("磁盘回收时间点配置非法，已跳过本次调度", "err", err)
				continue
			}
			if !scheduleDue(now, hour, minute) {
				continue
			}
			today := now.Format(scheduleDateLayout)
			if lastDate == today {
				continue
			}
			lastDate = today
			s.runScheduled(ctx)
		}
	}
}

// 只看"小时:分钟"，不看秒——调度器的醒来间隔（10 秒）与分钟边界不对齐，
// 真正的防重复靠的是"当天只跑一次"的闸门（lastDate）。
func scheduleDue(now time.Time, hour, minute int) bool {
	return now.Hour() == hour && now.Minute() == minute
}

func (s *GCService) runScheduled(ctx context.Context) {
	cfg := s.settings()
	if cfg.Cleanup.GC.DryRun {
		// 试运行模式下定时任务不释放磁盘——**必须说清楚**，
		// 否则运维会以为"每天凌晨都在回收"，其实一个字节都没删。
		s.logger.Warn("定时磁盘回收运行在试运行模式：只统计不删除，磁盘空间不会释放",
			"dry_run", true)
	}
	run, err := s.Run(ctx, GCRunOptions{
		Trigger: domain.GCTriggerSchedule,
		Actor:   "system",
	})
	if err != nil {
		if errors.Is(err, ErrGCBusy) {
			s.logger.Warn("定时磁盘回收被跳过：已有回收任务在运行")
			return
		}
		s.logger.Error("定时磁盘回收失败", "err", err)
		return
	}
	s.logger.Info("定时磁盘回收完成",
		"blobs_deleted", run.BlobsDeleted,
		"bytes_reclaimed", run.BytesReclaimed,
		"dry_run", run.DryRun)
}
