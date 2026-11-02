package server

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/lau6code/lau6-cr/apps/api/internal/service"
)

// 系统管理员看全部；普通已登录用户**只看得到公开项**
// （S3 endpoint / Redis 地址 / LDAP URL 这类会暴露拓扑的不给看）。
func (a *App) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	view, err := a.SettingsSvc.View(r.Context())
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "读取设置失败")
		return
	}
	if !a.isSystemAdmin(r) {
		filtered := make([]service.SettingView, 0, len(view.Settings))
		for _, s := range view.Settings {
			if service.IsPublicSetting(s.Key) {
				filtered = append(filtered, s)
			}
		}
		view.Settings = filtered
	}
	OK(w, view)
}

func (a *App) handlePatchSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Version int64          `json:"version"`
		Changes map[string]any `json:"changes"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	_, err := a.SettingsSvc.Apply(r.Context(), actorName(r.Context()), req.Version, req.Changes)
	if errors.Is(err, service.ErrVersionConflict) {
		Fail(w, http.StatusConflict, CodeVersionConflict, "设置已被他人修改，请刷新后重试")
		return
	}
	if err != nil {
		Fail(w, http.StatusBadRequest, CodeInvalidParam, err.Error())
		return
	}
	if !a.mustAudit(w, r, actorName(r.Context()), "settings.updated", "", "", clientIP(r), true,
		map[string]any{"keys": keysOf(req.Changes)}) {
		return
	}
	a.onSettingsChanged()
	// 重新取一次视图：刚落地的值与"待重启"清单都要重新读
	//（logging.* 是 TierRestart，改完要重启才生效）。
	view, err := a.SettingsSvc.View(r.Context())
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "读取设置失败")
		return
	}
	OK(w, view)
}

// 可见性与 handleGetSettings 一致：
// 普通已登录用户只看得到公开项（否则就成了一键探测基础设施拓扑的接口）。
func (a *App) handleGetSetting(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	if key == "" {
		Fail(w, http.StatusBadRequest, CodeInvalidParam, "缺少设置项 key")
		return
	}
	view, err := a.SettingsSvc.View(r.Context())
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "读取设置失败")
		return
	}
	for _, s := range view.Settings {
		if s.Key != key {
			continue
		}
		if !a.isSystemAdmin(r) && !service.IsPublicSetting(s.Key) {
			// ⚠️ 与列表端点保持一致：不可见的项就当它不存在，
			//    而不是回 403——403 会告诉调用方"这项存在"，
			//    那正是公开项过滤要防的信息泄露。
			Fail(w, http.StatusNotFound, CodeNotFound, "设置项不存在")
			return
		}
		OK(w, s)
		return
	}
	Fail(w, http.StatusNotFound, CodeNotFound, "设置项不存在")
}

func (a *App) handleResetSetting(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	if key == "" {
		Fail(w, http.StatusBadRequest, CodeInvalidParam, "缺少设置项 key")
		return
	}
	_, err := a.SettingsSvc.Reset(r.Context(), actorName(r.Context()), key)
	if err != nil {
		Fail(w, http.StatusBadRequest, CodeInvalidParam, err.Error())
		return
	}
	if !a.mustAudit(w, r, actorName(r.Context()), "settings.reset", key, "", clientIP(r), true,
		map[string]any{"key": key}) {
		return
	}
	a.onSettingsChanged()
	// 同上：重新取一次视图，让调用方看到回落后的值与"待重启"清单。
	view, err := a.SettingsSvc.View(r.Context())
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "读取设置失败")
		return
	}
	OK(w, view)
}

// 用于"YAML 导出失败"之后的补救：DB 与内存快照是对的，只是文件是旧的。
func (a *App) handleConfigExport(w http.ResponseWriter, r *http.Request) {
	if err := a.SettingsSvc.Export(); err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "导出配置文件失败: "+err.Error())
		return
	}
	if !a.mustAudit(w, r, actorName(r.Context()), "config.exported", "", "", clientIP(r), true, nil) {
		return
	}
	OK(w, map[string]any{"exported_at": a.SettingsSvc.ExportedAt()})
}

func (a *App) handleConfigBackups(w http.ResponseWriter, r *http.Request) {
	list, err := a.SettingsSvc.ListBackups()
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "读取配置备份失败")
		return
	}
	OK(w, map[string]any{"backups": list})
}

// ⚠️ 回滚只改**文件**，不改运行期的内存快照与 DB：
// 要真正生效必须重启容器。响应里明确带出这一点，
// 不能让用户以为"点一下就回滚好了"。
func (a *App) handleConfigRollback(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	path, err := a.SettingsSvc.Rollback(r.Context(), req.Name)
	if err != nil {
		Fail(w, http.StatusBadRequest, CodeInvalidParam, err.Error())
		return
	}
	if !a.mustAudit(w, r, actorName(r.Context()), "config.rolled_back", req.Name, "", clientIP(r), true,
		map[string]any{"name": req.Name, "path": path}) {
		return
	}
	OK(w, map[string]any{
		"path":            path,
		"pending_restart": true,
		"note":            "配置文件已回滚，但运行期仍以 system_settings 为准；重启容器后生效",
	})
}
