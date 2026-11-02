package server

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
	"github.com/lau6code/lau6-cr/apps/api/internal/service"
)

// 项目访问令牌（project token）
//
// 项目令牌是**项目身份**的长期凭据：项目管理员生成后交给 CI / 流水线，
// 让自动化以项目身份推拉镜像，不必占用某个人的个人账号。
//
// 与个人令牌的**根本区别**：个人令牌不带 scope，权限按用户的当前角色实时算；
// 项目令牌带一个创建时选定、此后不变的角色（docs/design/02-auth.md §2.6.2）。

// projectTokenActor 拼出项目令牌在审计里的 actor。
//
// 项目令牌没有"属主用户"，不记清楚的话审计里就分不清这枚凭据代表谁。
func projectTokenActor(projectName string) string {
	return service.ProjectSubject(projectName)
}

// handleListProjectTokens 列出某个项目的项目令牌。
//
// 列表里**没有令牌本体**——只有前缀、角色、创建时间、过期时间、最近使用时间。
func (a *App) handleListProjectTokens(w http.ResponseWriter, r *http.Request) {
	project, ok := a.resolveProject(w, r, CodeNotFound, "项目不存在")
	if !ok {
		return
	}
	toks, err := a.ProjectTokenSvc.List(r.Context(), project.ID)
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "读取项目令牌失败")
		return
	}
	OK(w, map[string]any{
		"tokens":              toks,
		"default_ttl_seconds": int64(a.ProjectTokenSvc.TTL() / time.Second),
	})
}

// handleCreateProjectToken 生成一枚项目令牌。
//
// ⚠️ **原始令牌只在这里返回一次。**
//
// 之后既取不回来，也没法从库里还原（库里只有 SHA-256 散列）。
// 前端必须让用户当场复制走——错过就只能重新生成。
func (a *App) handleCreateProjectToken(w http.ResponseWriter, r *http.Request) {
	project, ok := a.resolveProject(w, r, CodeNotFound, "项目不存在")
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
		Role string `json:"role"`
		personalTokenExpiryRequest
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	p := principalFrom(r.Context())
	raw, tok, err := a.ProjectTokenSvc.Create(r.Context(), project.ID, p.User.ID,
		req.Name, domain.Role(req.Role), req.expiryPolicy())
	if err != nil {
		Fail(w, http.StatusBadRequest, CodeInvalidParam, err.Error())
		return
	}
	// 审计写失败 → 这枚令牌**作废**：原始令牌只在这一刻可见，
	// 库里还原不出来，留着就是一条"谁都用不了"的孤儿凭据。
	actor := projectTokenActor(project.Name)
	if err := a.auditErr(r, actor, "project_token.created",
		tok.TokenPrefix, project.Name, clientIP(r), true,
		map[string]any{
			"never_expires": tok.ExpiresAt == nil,
			"role":          string(tok.Role),
		}); err != nil {
		if rerr := a.ProjectTokenSvc.Revoke(r.Context(), tok.ID, project.ID); rerr != nil {
			a.Logger.Error("审计写失败后吊销项目令牌失败", "project", project.Name, "err", rerr)
		}
		Fail(w, http.StatusInternalServerError, CodeInternal, "写审计日志失败，令牌未创建")
		return
	}
	OK(w, map[string]any{
		"token":        raw,
		"id":           tok.ID,
		"name":         tok.Name,
		"role":         tok.Role,
		"token_prefix": tok.TokenPrefix,
		"created_at":   tok.CreatedAt,
		"expires_at":   tok.ExpiresAt,
	})
}

// handlePatchProjectToken 改一枚项目令牌的失效时间（限本项目）。
//
// 令牌本体不变（散列不动）——CI 里已经配好的凭据不用重配。
//
// ⚠️ **角色不可改**：角色决定这枚令牌能干什么，改角色等于换了一枚凭据。
// 要换角色就吊销它、重新生成一枚新的。
func (a *App) handlePatchProjectToken(w http.ResponseWriter, r *http.Request) {
	project, ok := a.resolveProject(w, r, CodeNotFound, "项目不存在")
	if !ok {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		Fail(w, http.StatusBadRequest, CodeInvalidParam, "令牌 ID 不合法")
		return
	}
	var req struct {
		Role string `json:"role"`
		personalTokenExpiryRequest
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Role != "" {
		Fail(w, http.StatusBadRequest, CodeInvalidParam,
			"角色不可修改：角色决定这枚令牌能干什么，请吊销后重新生成")
		return
	}
	if err := a.ProjectTokenSvc.SetExpiry(r.Context(), id, project.ID, req.expiryPolicy()); err != nil {
		if errors.Is(err, service.ErrInvalidProjectToken) {
			Fail(w, http.StatusNotFound, CodeNotFound, "令牌不存在")
			return
		}
		// 矛盾请求 / 失效时间不合法 → 参数错误。
		Fail(w, http.StatusBadRequest, CodeInvalidParam, err.Error())
		return
	}
	if !a.mustAudit(w, r, projectTokenActor(project.Name), "project_token.expiry_updated",
		strconv.FormatInt(id, 10), project.Name, clientIP(r), true, nil) {
		return
	}
	OK(w, map[string]any{})
}

// handleDeleteProjectToken 吊销一枚项目令牌。
//
// 只能吊销**本项目**的（service 层按 project_id 限定）。
//
// ⚠️ 吊销**立即生效**：该令牌换发出去的 JWT 一并作废
// （签名密钥被删掉，验签必然失败）。
func (a *App) handleDeleteProjectToken(w http.ResponseWriter, r *http.Request) {
	project, ok := a.resolveProject(w, r, CodeNotFound, "项目不存在")
	if !ok {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		Fail(w, http.StatusBadRequest, CodeInvalidParam, "令牌 ID 不合法")
		return
	}
	if err := a.ProjectTokenSvc.Revoke(r.Context(), id, project.ID); err != nil {
		if errors.Is(err, service.ErrInvalidProjectToken) {
			Fail(w, http.StatusNotFound, CodeNotFound, "令牌不存在")
			return
		}
		Fail(w, http.StatusInternalServerError, CodeInternal, "吊销令牌失败")
		return
	}
	if !a.mustAudit(w, r, projectTokenActor(project.Name), "project_token.revoked",
		strconv.FormatInt(id, 10), project.Name, clientIP(r), true, nil) {
		return
	}
	OK(w, map[string]any{})
}
