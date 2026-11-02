package server

import (
	"net/http"

	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// 审计写入策略
//
// 审计（audit_logs）是"谁在什么时候做了什么"的唯一凭据。
// 它写不进去时怎么办，按"写失败会掩盖什么"分三类：
//
//  ① mustAudit —— 管理写操作（项目 / 成员 / 仓库 / 用户 / 设置 / 保留策略）。
//     写不进去就让请求失败（5xx）：审计链是这类动作的唯一追溯凭据，
//     报 200 却没留下记录 = 告诉调用方"记下了"，那是假成功（AGENTS.md 规则 1）。
//
//  ② auditFailure —— 失败路径（登录失败等）。
//     响应本来就是失败，不能把它改写成 5xx——
//     那会把"口令不对"说成"服务器炸了"，是另一种撒谎。
//     但绝不静默：记 error 日志；失败次数由 store 层累计并对外可见。
//
//  ③ 后台任务（清理 / 回收 / 对账）—— 见 service 层同类注释：
//     动作已经发生，回滚不了，但失败必须留痕并计数。
//
// 失败计数统一由 store.AuditWriteFailures() 暴露，
// 管理端在 /api/v1/admin/audit 的响应里带出来。

// mustAudit 写一条审计；写不进去就返回 5xx。
//
// 返回 false 表示已经写过响应，调用方必须立刻 return。
//
// ⚠️ 消息里写"操作可能已生效"：审计写发生在业务写**之后**，
// 业务写已经提交、回滚不了——撒谎说"操作未完成"同样是兜底。
// 真正需要原子性的场景（创建项目）走事务，见 handleCreateProject。
func (a *App) mustAudit(w http.ResponseWriter, r *http.Request,
	actor, action, target, scope, ip string, success bool, detail map[string]any) bool {
	if err := a.auditErr(r, actor, action, target, scope, ip, success, detail); err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal,
			"写审计日志失败，本次操作可能已生效，请刷新确认状态")
		return false
	}
	return true
}

// auditErr 写一条审计；只返回错误，不写响应。
//
// 给"失败后需要先撤销副作用、再回错误"的场景用（如登录：
// 审计写不进去就把刚签发的会话销毁，不能一边说登录失败、
// 一边留着一个能用的会话）。
func (a *App) auditErr(r *http.Request,
	actor, action, target, scope, ip string, success bool, detail map[string]any) error {
	err := a.DB.WriteAudit(r.Context(), actor, action, target, scope, ip, success, detail)
	if err != nil {
		a.Logger.Error("写审计失败", "action", action, "actor", actor, "target", target, "err", err)
	}
	return err
}

// auditFailure 写一条"失败"审计（失败路径专用）。
//
// ⚠️ 只在**响应本来就是失败**的路径上用（登录失败等）。
// 审计写失败不能改写响应的失败原因，但必须留痕：
// error 日志 + store 层的失败计数。
func (a *App) auditFailure(r *http.Request, actor, action, target, scope, ip string, detail map[string]any) {
	if err := a.auditErr(r, actor, action, target, scope, ip, false, detail); err != nil {
		a.Logger.Error("写失败审计失败", "action", action, "actor", actor, "target", target, "err", err)
	}
}

// auditFailures 返回审计写失败的累计次数（供管理端展示）。
func auditFailures() int64 { return store.AuditWriteFailures() }
