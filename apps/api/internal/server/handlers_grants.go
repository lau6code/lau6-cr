package server

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// 仓库级的显式授权（repository_grants）。
//
// ⚠️ 只对**私有仓库**有意义：项目内的非管理员成员想访问私有仓库，
// 除了项目角色还得有这条显式授权（permission.go 第 9 步）。
// 项目管理员不需要它——他们在项目内本就全权。
//
// 权限一律是项目管理员（系统管理员也算，见 requireProjectAdmin）。

// handleListRepoGrants 列出仓库的显式授权。
func (a *App) handleListRepoGrants(w http.ResponseWriter, r *http.Request) {
	repo, ok := a.projectRepoForAdmin(w, r)
	if !ok {
		return
	}
	grants, err := a.DB.ListGrants(r.Context(), repo.ID)
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "读取授权列表失败")
		return
	}
	if grants == nil {
		grants = []domain.RepositoryGrant{}
	}
	OK(w, map[string]any{"grants": grants})
}

func (a *App) handleAddRepoGrant(w http.ResponseWriter, r *http.Request) {
	repo, ok := a.projectRepoForAdmin(w, r)
	if !ok {
		return
	}
	var req struct {
		Username string `json:"username"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Username == "" {
		Fail(w, http.StatusBadRequest, CodeInvalidParam, "必须指定用户名")
		return
	}
	user, err := a.DB.UserByUsername(r.Context(), req.Username)
	if err == store.ErrNotFound {
		Fail(w, http.StatusNotFound, CodeNotFound, "用户不存在")
		return
	}
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "读取用户失败")
		return
	}
	// 授权人 = 当前操作者（项目管理员）。
	grantedBy := principalFrom(r.Context()).User.ID
	added, err := a.DB.AddGrant(r.Context(), repo.ID, user.ID, &grantedBy)
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "添加授权失败")
		return
	}
	if !added {
		// 重复授权不静默成功：告诉用户"他本来就有权"，
		// 否则审计里会多出一条假的"刚刚授权"。
		Fail(w, http.StatusConflict, CodeConflict, "该用户已被授权")
		return
	}
	if !a.mustAudit(w, r, actorName(r.Context()), "grant.added", repo.FullName, "",
		clientIP(r), true, map[string]any{"username": user.Username}) {
		return
	}
	OK(w, map[string]any{})
}

// handleRemoveRepoGrant 取消某个用户的仓库授权。
func (a *App) handleRemoveRepoGrant(w http.ResponseWriter, r *http.Request) {
	repo, ok := a.projectRepoForAdmin(w, r)
	if !ok {
		return
	}
	userID, err := strconv.ParseInt(chi.URLParam(r, "userId"), 10, 64)
	if err != nil {
		Fail(w, http.StatusBadRequest, CodeInvalidParam, "用户 ID 不合法")
		return
	}
	granted, err := a.DB.HasGrant(r.Context(), repo.ID, userID)
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "读取授权失败")
		return
	}
	if !granted {
		Fail(w, http.StatusNotFound, CodeNotFound, "该用户没有这条授权")
		return
	}
	if err := a.DB.RemoveGrant(r.Context(), repo.ID, userID); err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "取消授权失败")
		return
	}
	if !a.mustAudit(w, r, actorName(r.Context()), "grant.removed", repo.FullName, "",
		clientIP(r), true, map[string]any{"user_id": userID}) {
		return
	}
	OK(w, map[string]any{})
}

// projectRepoForAdmin 取出路径上的仓库，并校验操作者是项目管理员。
//
// 返回 ok=false 时已经把错误响应写出去了。
func (a *App) projectRepoForAdmin(w http.ResponseWriter, r *http.Request) (*domain.Repository, bool) {
	project, ok := a.resolveProject(w, r, CodeNotFound, "项目不存在")
	if !ok {
		return nil, false
	}
	if !a.isProjectAdmin(r, project) {
		Fail(w, http.StatusForbidden, CodeForbidden, "需要项目管理员权限")
		return nil, false
	}
	repo, err := a.DB.RepositoryByPath(r.Context(), project.ID, chi.URLParam(r, "repo"))
	if err == store.ErrNotFound {
		Fail(w, http.StatusNotFound, CodeNotFound, "仓库不存在")
		return nil, false
	}
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "读取仓库失败")
		return nil, false
	}
	return repo, true
}
