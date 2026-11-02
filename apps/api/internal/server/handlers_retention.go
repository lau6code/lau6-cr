package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
	"github.com/lau6code/lau6-cr/apps/api/internal/service"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// 镜像清理策略的 HTTP 接口。
//
// 契约见 docs/design/03-registry.md §5 与 04-api-data.md。
//
// ⚠️ 这里只管**保留策略**（删 tag/manifest，只解除引用）；
//    blob 回收（第 ② 层，把磁盘空间还给操作系统）是另一件事，见 handlers_gc.go。

type retentionPolicyRequest struct {
	// Enabled 是否启用这条策略。
	Enabled bool `json:"enabled"`
	// Algorithm 是组合算法，只支持 "or"。
	Algorithm string `json:"algorithm"`
	// TagSelector 是策略级默认 tag 选择器。
	TagSelector string `json:"tag_selector"`
	// Rules 是规则列表。
	Rules []domain.RetentionRule `json:"rules"`
}

// emptyRetentionPolicy 造一份"关闭且无规则"的保留策略骨架。
//
// 没配过策略时用它顶替：前端不必为"没有"和"有但空"写两套渲染逻辑。
func emptyRetentionPolicy(scopeKey string) *domain.RetentionPolicy {
	return &domain.RetentionPolicy{
		ScopeKey:    scopeKey,
		Enabled:     false,
		Algorithm:   service.RetentionAlgorithmOR,
		TagSelector: "**",
		Rules:       []domain.RetentionRule{},
	}
}

func (a *App) handleGetGlobalRetentionPolicy(w http.ResponseWriter, r *http.Request) {
	policy, err := a.DB.RetentionPolicyByScope(r.Context(), store.GlobalRetentionScope)
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "读取保留策略失败")
		return
	}
	if policy == nil {
		policy = emptyRetentionPolicy(store.GlobalRetentionScope)
	}
	OK(w, map[string]any{"policy": policy})
}

func (a *App) handleUpdateGlobalRetentionPolicy(w http.ResponseWriter, r *http.Request) {
	var req retentionPolicyRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	policy := &domain.RetentionPolicy{
		ScopeKey:    store.GlobalRetentionScope,
		Enabled:     req.Enabled,
		Algorithm:   req.Algorithm,
		TagSelector: req.TagSelector,
		Rules:       req.Rules,
	}
	// ⚠️ 校验在**保存时**做——留到执行期才发现规则非法，那次清理已经开始了。
	if err := service.ValidateRetentionPolicy(policy); err != nil {
		Fail(w, http.StatusBadRequest, CodeInvalidParam, err.Error())
		return
	}
	if err := a.DB.UpsertRetentionPolicy(r.Context(), policy); err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "保存保留策略失败")
		return
	}
	if !a.mustAudit(w, r, actorName(r.Context()), "retention.policy.updated", "global", "",
		clientIP(r), true, map[string]any{"enabled": policy.Enabled, "rules": len(policy.Rules)}) {
		return
	}
	saved, err := a.DB.RetentionPolicyByScope(r.Context(), store.GlobalRetentionScope)
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "读取保留策略失败")
		return
	}
	OK(w, map[string]any{"policy": saved})
}

// 列出全部保留策略：全局默认 + 各项目级覆盖。
//
// 给系统管理员看"谁压过了全局默认"——项目级策略是**逐项目覆盖**全局默认的
// （03-registry.md §5.2.3），没有这个列表就看不出全局默认对哪些项目根本不生效。
//
// 权限：系统管理员（挂在 /admin 分组下，requireAdmin 强制）。
func (a *App) handleListRetentionPolicies(w http.ResponseWriter, r *http.Request) {
	policies, err := a.DB.ListRetentionPolicies(r.Context())
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "读取保留策略列表失败")
		return
	}
	OK(w, map[string]any{"policies": policies})
}

// 读取项目级保留策略。
//
// 响应里除了项目级策略本身，还给出**实际生效的策略**：
//
//	policy   —— 项目级策略（没配过时是"关闭且无规则"的骨架）
//	effective —— 实际会被执行的策略（项目级优先，没有则用全局默认）
//	source   —— "project" | "global"
//
// 为什么要给 effective：项目管理员可能**没配**项目级策略，
// 那时生效的是全局默认。页面若只显示骨架，用户会以为"这个项目不受清理影响"，
// 而实际上全局默认正在删它的镜像。
func (a *App) handleGetProjectRetention(w http.ResponseWriter, r *http.Request) {
	project, ok := a.resolveProject(w, r, CodeNotVisible, "项目不存在或无权访问")
	if !ok {
		return
	}
	policy, err := a.DB.RetentionPolicyByProject(r.Context(), project.ID)
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "读取保留策略失败")
		return
	}
	global, err := a.DB.RetentionPolicyByScope(r.Context(), store.GlobalRetentionScope)
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "读取保留策略失败")
		return
	}

	source := "global"
	effective := global
	if policy != nil {
		source = "project"
		effective = policy
	}
	if policy == nil {
		policy = emptyRetentionPolicy(store.ProjectRetentionScope(project.ID))
		policy.ProjectID = &project.ID
		policy.ProjectName = project.Name
	}
	policy.ProjectName = project.Name
	if effective == nil {
		// 全局默认也没配 —— 给一个同样的骨架，别让前端处理 null。
		effective = emptyRetentionPolicy(store.GlobalRetentionScope)
	}
	OK(w, map[string]any{
		"policy":    policy,
		"effective": effective,
		"source":    source,
	})
}

func (a *App) handleUpdateProjectRetention(w http.ResponseWriter, r *http.Request) {
	project, ok := a.resolveProject(w, r, CodeNotVisible, "项目不存在或无权访问")
	if !ok {
		return
	}
	var req retentionPolicyRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	policy := &domain.RetentionPolicy{
		ScopeKey:    store.ProjectRetentionScope(project.ID),
		ProjectID:   &project.ID,
		ProjectName: project.Name,
		Enabled:     req.Enabled,
		Algorithm:   req.Algorithm,
		TagSelector: req.TagSelector,
		Rules:       req.Rules,
	}
	if err := service.ValidateRetentionPolicy(policy); err != nil {
		Fail(w, http.StatusBadRequest, CodeInvalidParam, err.Error())
		return
	}
	if err := a.DB.UpsertRetentionPolicy(r.Context(), policy); err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "保存保留策略失败")
		return
	}
	if !a.mustAudit(w, r, actorName(r.Context()), "retention.policy.updated", project.Name, "",
		clientIP(r), true, map[string]any{"enabled": policy.Enabled, "rules": len(policy.Rules)}) {
		return
	}
	OK(w, map[string]any{"policy": policy})
}

// 删除项目级策略（回落到全局默认）。
func (a *App) handleDeleteProjectRetention(w http.ResponseWriter, r *http.Request) {
	project, ok := a.resolveProject(w, r, CodeNotVisible, "项目不存在或无权访问")
	if !ok {
		return
	}
	if err := a.DB.DeleteRetentionPolicy(r.Context(), store.ProjectRetentionScope(project.ID)); err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "删除保留策略失败")
		return
	}
	if !a.mustAudit(w, r, actorName(r.Context()), "retention.policy.deleted", project.Name, "",
		clientIP(r), true, nil) {
		return
	}
	OK(w, map[string]any{})
}

// 手动执行一次清理。
func (a *App) handleRunCleanup(w http.ResponseWriter, r *http.Request) {
	if a.CleanupSvc == nil {
		Fail(w, http.StatusServiceUnavailable, CodeUnavailable, "清理服务未就绪")
		return
	}
	var req struct {
		// Project 为空表示全部项目。
		Project string `json:"project"`
		// DryRun 为 true 时只评估不删除。
		//
		// ⚠️ 默认取全局设置 cleanup.dry_run（默认 true = 试运行）。
		// 要真删必须显式传 dry_run=false。
		DryRun *bool `json:"dry_run"`
	}
	// ⚠️ 用 `!= 0` 而不是 `> 0`：分块传输的请求 ContentLength 是 -1，
	// 用 `> 0` 会把它的请求体整段忽略掉（dry_run 之类的字段全丢）。
	if r.ContentLength != 0 {
		if !decodeJSONBody(w, r, &req) {
			return
		}
	}
	// ⚠️ 取**实时快照**而不是 a.Cfg：
	//    a.Cfg 是启动时捕获的配置，设置页改了 cleanup.dry_run 之后它就过期了，
	//    用它会导致"UI 上关了试运行，手动执行还在试运行"。
	dryRun := a.CleanupSvc.Settings().Cleanup.DryRun
	if req.DryRun != nil {
		dryRun = *req.DryRun
	}
	if req.Project != "" {
		if _, err := a.DB.ProjectByName(r.Context(), req.Project); err == store.ErrNotFound {
			Fail(w, http.StatusNotFound, CodeNotFound, "项目不存在")
			return
		} else if err != nil {
			Fail(w, http.StatusInternalServerError, CodeInternal, "读取项目失败")
			return
		}
	}
	run, err := a.CleanupSvc.Run(r.Context(), service.CleanupRunOptions{
		Trigger:     domain.TriggerManual,
		ProjectName: req.Project,
		DryRun:      dryRun,
		Actor:       actorName(r.Context()),
		ClientIP:    clientIP(r),
	})
	if err != nil {
		if errors.Is(err, service.ErrCleanupBusy) {
			Fail(w, http.StatusConflict, CodeConflict, "已有清理任务在运行")
			return
		}
		// 执行本身失败时**仍然把执行记录带上**——
		// 前端要能看到失败明细（哪个仓库、哪个 digest、报了什么错），
		// 而不是只拿到一句"失败了"再去翻历史。
		//
		// ⚠️ HTTP 状态码仍是非 2xx：失败就是失败，
		// 不能用 200 带一个 status=failed 的对象去假装成功。
		if run != nil {
			FailWithData(w, http.StatusInternalServerError, CodeInternal,
				"清理执行失败: "+run.Error, map[string]any{"run": run})
			return
		}
		Fail(w, http.StatusInternalServerError, CodeInternal, "清理执行失败: "+err.Error())
		return
	}
	OK(w, map[string]any{"run": run})
}

// listRunHistory 是执行历史列表的通用 handler。
//
// 清理执行与 blob 回收各有一份，两边只有表名与文案不同。
func listRunHistory[T any](w http.ResponseWriter, r *http.Request,
	list func(ctx context.Context, limit, offset int) ([]T, error), errMsg string) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	runs, err := list(r.Context(), limit, offset)
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, errMsg)
		return
	}
	OK(w, map[string]any{"runs": runs})
}

// getRunHistory 是单次执行记录详情的通用 handler。
func getRunHistory[T any](w http.ResponseWriter, r *http.Request,
	get func(ctx context.Context, id int64) (*T, error), errMsg string) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		Fail(w, http.StatusBadRequest, CodeInvalidParam, "执行记录 ID 不合法")
		return
	}
	run, err := get(r.Context(), id)
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, errMsg)
		return
	}
	if run == nil {
		Fail(w, http.StatusNotFound, CodeNotFound, "执行记录不存在")
		return
	}
	OK(w, map[string]any{"run": run})
}

func (a *App) handleListCleanupRuns(w http.ResponseWriter, r *http.Request) {
	listRunHistory(w, r, a.DB.ListCleanupRuns, "读取清理历史失败")
}

func (a *App) handleGetCleanupRun(w http.ResponseWriter, r *http.Request) {
	getRunHistory(w, r, a.DB.CleanupRunByID, "读取清理记录失败")
}

// 列出可用的规则模板。
//
// 前端用它渲染规则编辑器，避免把模板清单硬编码在前端。
func (a *App) handleListRetentionTemplates(w http.ResponseWriter, r *http.Request) {
	OK(w, map[string]any{
		"templates": []map[string]any{
			{
				"template": domain.RuleKeepLatestK,
				"label":    "保留最近推送的 K 个",
				"params":   []map[string]any{{"name": "k", "type": "int", "unit": "count", "required": true}},
				"descr":    "每个仓库保留最近推送的 K 个 tag，其余删除",
				"harbor":   "latestPushedK",
			},
			{
				"template": domain.RuleKeepWithinDays,
				"label":    "保留最近 N 天推送的",
				"params":   []map[string]any{{"name": "days", "type": "int", "unit": "days", "required": true}},
				"descr":    "保留最近 N 天内推送过的 tag，更早的删除",
				"harbor":   "nDaysSinceLastPush",
			},
			{
				"template": domain.RuleKeepAlways,
				"label":    "永不自动删除",
				"params":   []map[string]any{},
				"descr":    "该选择器命中的 tag 永不自动删除。适合做局部豁免",
				"harbor":   "always",
			},
		},
		"selector_help": retentionSelectorHelp(),
	})
}

// 返回选择器语法说明（前端提示用）。
func retentionSelectorHelp() string {
	return strings.TrimSpace(`
支持 * ? **：
  *  匹配任意字符（不跨 /）
  ?  匹配单个字符
  ** 匹配任意字符（跨 /）
例：
  **            全部 tag
  release-*     release- 前缀
  v[0-9]*       版本号前缀`)
}
