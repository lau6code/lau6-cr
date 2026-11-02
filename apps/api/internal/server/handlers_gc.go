package server

import (
	"errors"
	"net/http"

	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
	"github.com/lau6code/lau6-cr/apps/api/internal/service"
)

// blob 回收（第 ② 层）。
//
// ⚠️ 它与保留策略是**两件独立的事**（config/config.go §GCConfig）：
//
//	保留策略删 tag/manifest（只解除引用），
//	GC 才真正把磁盘空间还给操作系统。
//
// 契约见 docs/design/03-registry.md §5.5。

// 返回 blob 回收的当前状态与配置。
func (a *App) handleGCStatus(w http.ResponseWriter, r *http.Request) {
	if a.GCSvc == nil {
		Fail(w, http.StatusServiceUnavailable, CodeUnavailable, "blob 回收服务未就绪")
		return
	}
	cfg := a.GCSvc.Settings()
	last, err := a.DB.LastGCRun(r.Context())
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "读取最近一次 blob 回收记录失败")
		return
	}
	busy := a.WriteBarrier != nil && a.WriteBarrier.Blocked()
	OK(w, map[string]any{
		"enabled":               cfg.Cleanup.GC.Enabled,
		"delete_untagged":       cfg.Cleanup.GC.DeleteUntagged,
		"dry_run":               cfg.Cleanup.GC.DryRun,
		"trigger_after_cleanup": cfg.Cleanup.GC.TriggerAfterCleanup,
		"schedule_enabled":      cfg.Cleanup.GC.ScheduleEnabled,
		"schedule_at":           cfg.Cleanup.GC.ScheduleAt,
		"busy":                  busy,
		"last_run":              last,
	})
}

// 手动触发一次 blob 回收。
//
// ⚠️ **立即返回**：回收在后台跑（对象存储下可能跑几十分钟到几小时，
// 同步等会把请求拖到超时）。
//
// 返回的 run 里 status=running；结局在执行记录里
// （status / 统计 / 失败原因），前端轮询执行记录即可。
//
// 动手前就能发现的问题（回收未启用、已有任务在跑、distribution 不可用）
// 仍然是**同步报错**——那不是"回收失败"，是"没能开始回收"。
func (a *App) handleRunGC(w http.ResponseWriter, r *http.Request) {
	if a.GCSvc == nil {
		Fail(w, http.StatusServiceUnavailable, CodeUnavailable, "blob 回收服务未就绪")
		return
	}
	var req struct {
		// DryRun 为 true 时只评估不删除。
		//
		// ⚠️ 不传时取全局设置 cleanup.gc.dry_run（默认 true = 试运行）。
		DryRun *bool `json:"dry_run"`
	}
	if r.ContentLength != 0 {
		if !decodeJSONBody(w, r, &req) {
			return
		}
	}
	run, err := a.GCSvc.Run(r.Context(), service.GCRunOptions{
		Trigger:  domain.GCTriggerManual,
		DryRun:   req.DryRun,
		Actor:    actorName(r.Context()),
		ClientIP: clientIP(r),
	})
	if err != nil {
		if errors.Is(err, service.ErrGCBusy) {
			Fail(w, http.StatusConflict, CodeConflict, "已有 blob 回收任务在运行")
			return
		}
		if errors.Is(err, service.ErrGCDisabled) {
			Fail(w, http.StatusBadRequest, CodeInvalidParam,
				"blob 回收未启用：请先在「系统设置」里开启 cleanup.gc.enabled")
			return
		}
		Fail(w, http.StatusInternalServerError, CodeInternal, "blob 回收启动失败: "+err.Error())
		return
	}
	OK(w, map[string]any{"run": run})
}

func (a *App) handleListGCRuns(w http.ResponseWriter, r *http.Request) {
	listRunHistory(w, r, a.DB.ListGCRuns, "读取 blob 回收历史失败")
}

func (a *App) handleGetGCRun(w http.ResponseWriter, r *http.Request) {
	getRunHistory(w, r, a.DB.GCRunByID, "读取 blob 回收记录失败")
}
