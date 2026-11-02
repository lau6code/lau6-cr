package server

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
	"github.com/lau6code/lau6-cr/apps/api/internal/service"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// resolveProject 按 URL 里的项目名取项目。
//
// 返回 ok=false 表示错误响应已经写出去了，调用方必须立刻 return。
//
// code / msg 是"项目不存在"时的响应口径：
//
//   - CodeNotFound + "项目不存在"：调用方本来就该看得到这个项目；
//   - CodeNotVisible + "项目不存在或无权访问"：不该暴露项目是否存在
//     （不区分"没有"与"没权限"，避免被拿来探测项目名）。
//
// 两档的 500 分支完全一致：读库失败不是"项目不存在"，必须说清是服务端的问题。
func (a *App) resolveProject(w http.ResponseWriter, r *http.Request, code int, msg string) (*domain.Project, bool) {
	name := chi.URLParam(r, "name")
	project, err := a.DB.ProjectByName(r.Context(), name)
	if err == store.ErrNotFound {
		Fail(w, http.StatusNotFound, code, msg)
		return nil, false
	}
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "读取项目失败")
		return nil, false
	}
	return project, true
}

// 普通用户只看到自己是成员的项目；系统管理员看到全部。
//
// 每个项目都带上 my_role —— **当前查看者**在本项目里的角色。
// 前端用它决定"编辑项目"入口是否出现（项目管理员或系统管理员才能编辑）。
func (a *App) handleListProjects(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	var memberOf *int64
	var viewerID *int64
	if !p.IsAnonymous() {
		id := p.User.ID
		viewerID = &id
		if !p.User.IsSystemAdmin {
			memberOf = &id
		}
	}
	projects, err := a.DB.ListProjects(r.Context(), memberOf, viewerID)
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "读取项目列表失败")
		return
	}
	if projects == nil {
		projects = []domain.Project{}
	}
	OK(w, map[string]any{"projects": projects})
}

// 可见范围与 handleListProjects 一致：
// 普通用户只看自己是成员的项目；系统管理员看全部项目。
func (a *App) handleListProjectMembers(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	var memberOf *int64
	if !p.User.IsSystemAdmin {
		id := p.User.ID
		memberOf = &id
	}
	members, err := a.DB.ListMemberships(r.Context(), memberOf)
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "读取成员关系失败")
		return
	}
	if members == nil {
		members = []domain.ProjectMembership{}
	}
	OK(w, map[string]any{"members": members})
}

func (a *App) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
		Description string `json:"description"`
		Visibility  string `json:"visibility"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if !validProjectName(req.Name) {
		Fail(w, http.StatusBadRequest, CodeInvalidParam, "项目名只能包含小写字母、数字、. _ -，且不能以分隔符开头")
		return
	}
	// 可见性只认 public / private：非法值必须报错，不能静默改成 private——
	// 调用方以为设成了公开、实际是私有，这种"顺手兜底"就是假成功。
	// 与 handleCreateRepo 对同一概念的处理保持一致。
	vis := domain.Visibility(strings.TrimSpace(req.Visibility))
	if vis != domain.VisibilityPublic && vis != domain.VisibilityPrivate {
		Fail(w, http.StatusBadRequest, CodeInvalidParam, "可见性只能是 public 或 private")
		return
	}
	p := principalFrom(r.Context())
	var createdBy *int64
	if !p.IsAnonymous() {
		id := p.User.ID
		createdBy = &id
	}

	// 项目 + 创建者的管理员关系 + 审计记录，同一事务
	//
	// ⚠️ 为什么要放一个事务里：这三件事必须**要么都成、要么都不成**。
	// 分开写的话，"项目建好了但创建者不是管理员"会留下一个谁也管不了的
	// 孤儿项目——创建者看到的却只有"创建成功"。
	// 同理，审计写失败也不能把项目留在库里。
	tx, err := a.DB.Begin(r.Context())
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "创建项目失败")
		return
	}
	defer tx.Rollback()

	now := store.Now()
	id, err := tx.InsertAndGetID(r.Context(), `INSERT INTO projects
		(name, display_name, description, visibility, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		req.Name, req.DisplayName, req.Description, string(vis), createdBy, now, now)
	if err != nil {
		if store.IsDuplicate(err) {
			Fail(w, http.StatusConflict, CodeConflict, "项目名已存在")
			return
		}
		Fail(w, http.StatusInternalServerError, CodeInternal, "创建项目失败")
		return
	}
	if !p.IsAnonymous() {
		if err := tx.AddMember(r.Context(), id, p.User.ID, domain.RoleAdmin, nil); err != nil {
			// 失败就整体回滚：项目建出来而创建者不是管理员，
			// 等于留一个谁都管不了的项目——那比创建失败更糟。
			a.Logger.Error("创建者加入项目失败", "project", req.Name, "user", p.User.ID, "err", err)
			Fail(w, http.StatusInternalServerError, CodeInternal, "创建项目失败")
			return
		}
	}
	if err := tx.WriteAudit(r.Context(), actorName(r.Context()), "project.created", req.Name, "",
		clientIP(r), true, nil); err != nil {
		a.Logger.Error("写审计失败", "action", "project.created", "project", req.Name, "err", err)
		Fail(w, http.StatusInternalServerError, CodeInternal, "写审计日志失败，项目未创建")
		return
	}
	if err := tx.Commit(); err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "创建项目失败")
		return
	}

	// 回读项目对象（含成员数 / 仓库数 / my_role）——响应里给的是数据库里的真实状态。
	project, err := a.DB.ProjectByName(r.Context(), req.Name)
	if err != nil {
		a.Logger.Error("读取新建项目失败", "project", req.Name, "err", err)
		Fail(w, http.StatusInternalServerError, CodeInternal, "项目已创建，但读取项目信息失败，请刷新确认")
		return
	}
	a.fillMyRole(r, project)
	OK(w, map[string]any{"project": project})
}

func (a *App) handleGetProject(w http.ResponseWriter, r *http.Request) {
	project, ok := a.resolveProject(w, r, CodeNotFound, "项目不存在")
	if !ok {
		return
	}
	p := principalFrom(r.Context())
	if !a.canViewProject(r, project, p) {
		Fail(w, http.StatusNotFound, CodeNotVisible, "项目不存在或无权访问")
		return
	}
	members, err := a.DB.ListMembers(r.Context(), project.ID)
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "读取成员失败")
		return
	}
	if members == nil {
		members = []domain.Member{}
	}
	// 回填查看者在本项目里的角色：详情页据此决定"编辑项目 / 管理成员"入口是否可见。
	a.fillMyRole(r, project)
	OK(w, map[string]any{"project": project, "members": members})
}

// 回填"当前查看者在本项目里的角色"。
//
// 项目对象在列表 / 详情 / 修改响应里都会经过它，保证同一个接口族返回的
// my_role 语义一致——前端不必在拿到新数据后自己去猜权限。
func (a *App) fillMyRole(r *http.Request, project *domain.Project) {
	p := principalFrom(r.Context())
	if p.IsAnonymous() {
		return
	}
	m, err := a.DB.Membership(r.Context(), project.ID, p.User.ID)
	if err != nil {
		// 数据坏了（成员关系的 repo_patterns 解析失败等）不是"不是成员"——
		// 但这里只是回填展示字段，不能因此让整个请求失败。
		// 至少要留痕：否则 my_role 会莫名其妙是空的。
		a.Logger.Error("读取成员关系失败，my_role 无法回填", "err", err, "project", project.Name)
		return
	}
	if m == nil {
		return
	}
	role := m.Role
	project.MyRole = &role
}

// canViewProject 判断主体能否查看项目。
func (a *App) canViewProject(r *http.Request, project *domain.Project, p service.Principal) bool {
	if !p.IsAnonymous() && p.User.IsSystemAdmin {
		return true
	}
	if project.Visibility == domain.VisibilityPublic {
		return true
	}
	if p.IsAnonymous() {
		return false
	}
	m, err := a.DB.Membership(r.Context(), project.ID, p.User.ID)
	if err != nil {
		// 读失败 → 保守判为"不可见"（fail-closed），但必须留痕：
		// 否则数据损坏会被伪装成"没有权限"。
		a.Logger.Error("读取成员关系失败，按不可见处理", "err", err, "project", project.Name)
		return false
	}
	return m != nil
}

func (a *App) handleUpdateProject(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	project, ok := a.resolveProject(w, r, CodeNotFound, "项目不存在")
	if !ok {
		return
	}
	var req struct {
		DisplayName *string `json:"display_name"`
		Description *string `json:"description"`
		Visibility  *string `json:"visibility"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	// 编辑项目的层级（与 docs/design/04-api-data.md §3.4 一致）：
	//
	//	· display_name / description → 项目管理员 or 系统管理员
	//	· visibility                 → **只有**系统管理员（改的是公开面，不是项目内属性）
	//
	// 先判"能不能编辑这个项目"，再判"能不能改可见性"，
	// 这样普通成员收到的是"需要项目管理员权限"而不是更让人误解的可见性提示。
	if !a.isProjectAdmin(r, project) {
		Fail(w, http.StatusForbidden, CodeForbidden, "需要项目管理员权限")
		return
	}
	if req.Visibility != nil && !a.isSystemAdmin(r) {
		Fail(w, http.StatusForbidden, CodeForbidden, "修改项目可见性需要系统管理员权限")
		return
	}
	var vis *domain.Visibility
	if req.Visibility != nil {
		v := domain.Visibility(*req.Visibility)
		if v != domain.VisibilityPublic && v != domain.VisibilityPrivate {
			Fail(w, http.StatusBadRequest, CodeInvalidParam, "可见性只能是 public 或 private")
			return
		}
		vis = &v
	}
	if err := a.DB.UpdateProject(r.Context(), project.ID, req.DisplayName, req.Description, vis); err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "更新项目失败")
		return
	}
	if !a.mustAudit(w, r, actorName(r.Context()), "project.updated", project.Name, "", clientIP(r), true, nil) {
		return
	}
	updated, err := a.DB.ProjectByName(r.Context(), name)
	if err != nil {
		// 项目已经改完了，但回读失败：不能说"更新失败"（那是假的），
		// 也不能把旧对象当新对象返回（那是拿陈旧值兜底）。
		a.Logger.Error("回读项目失败", "project", name, "err", err)
		Fail(w, http.StatusInternalServerError, CodeInternal, "更新已完成，但读取项目信息失败，请刷新确认")
		return
	}
	// 回填 my_role：前端拿到响应就能直接替换列表里的那一行，
	// 不会因为少了这个字段把"编辑"入口判没了。
	a.fillMyRole(r, updated)
	OK(w, map[string]any{"project": updated})
}

func (a *App) handleDeleteProject(w http.ResponseWriter, r *http.Request) {
	project, ok := a.resolveProject(w, r, CodeNotFound, "项目不存在")
	if !ok {
		return
	}
	if !a.isSystemAdmin(r) {
		Fail(w, http.StatusForbidden, CodeForbidden, "删除项目需要系统管理员权限")
		return
	}
	if err := a.DB.DeleteProject(r.Context(), project.ID); err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "删除项目失败")
		return
	}
	if !a.mustAudit(w, r, actorName(r.Context()), "project.deleted", project.Name, "", clientIP(r), true, nil) {
		return
	}
	OK(w, map[string]any{})
}

// 与 lf-db 同族产品一致：返回**所有**启用账户，并带上其在该项目里的
// 当前角色（current_role 为空 = 尚未加入）。前端据此分两组展示。
func (a *App) handleListMemberCandidates(w http.ResponseWriter, r *http.Request) {
	project, ok := a.resolveProject(w, r, CodeNotVisible, "项目不存在或无权访问")
	if !ok {
		return
	}
	candidates, err := a.DB.ListMemberCandidates(r.Context(), project.ID)
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "读取候选用户失败")
		return
	}
	if candidates == nil {
		candidates = []domain.MemberCandidate{}
	}
	OK(w, map[string]any{"candidates": candidates})
}

func (a *App) handleListMembers(w http.ResponseWriter, r *http.Request) {
	project, ok := a.resolveProject(w, r, CodeNotFound, "项目不存在")
	if !ok {
		return
	}
	members, err := a.DB.ListMembers(r.Context(), project.ID)
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "读取成员失败")
		return
	}
	if members == nil {
		members = []domain.Member{}
	}
	OK(w, map[string]any{"members": members})
}

func (a *App) handleAddMember(w http.ResponseWriter, r *http.Request) {
	project, ok := a.resolveProject(w, r, CodeNotFound, "项目不存在")
	if !ok {
		return
	}
	if !a.isProjectAdmin(r, project) {
		Fail(w, http.StatusForbidden, CodeForbidden, "需要项目管理员权限")
		return
	}
	var req struct {
		Username     string   `json:"username"`
		Role         string   `json:"role"`
		RepoPatterns []string `json:"repo_patterns"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	role := domain.Role(req.Role)
	if !role.Valid() {
		Fail(w, http.StatusBadRequest, CodeInvalidParam, "角色只能是 admin / developer / reader")
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
	if err := a.DB.AddMember(r.Context(), project.ID, user.ID, role, req.RepoPatterns); err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "添加成员失败")
		return
	}
	if !a.mustAudit(w, r, actorName(r.Context()), "member.added", project.Name, "", clientIP(r), true,
		map[string]any{"username": user.Username, "role": string(role)}) {
		return
	}
	OK(w, map[string]any{})
}

func (a *App) handleRemoveMember(w http.ResponseWriter, r *http.Request) {
	project, ok := a.resolveProject(w, r, CodeNotFound, "项目不存在")
	if !ok {
		return
	}
	if !a.isProjectAdmin(r, project) {
		Fail(w, http.StatusForbidden, CodeForbidden, "需要项目管理员权限")
		return
	}
	userID, err := strconv.ParseInt(chi.URLParam(r, "userId"), 10, 64)
	if err != nil {
		Fail(w, http.StatusBadRequest, CodeInvalidParam, "用户 ID 不合法")
		return
	}
	if err := a.DB.RemoveMember(r.Context(), project.ID, userID); err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "移除成员失败")
		return
	}
	if !a.mustAudit(w, r, actorName(r.Context()), "member.removed", project.Name, "", clientIP(r), true,
		map[string]any{"user_id": userID}) {
		return
	}
	OK(w, map[string]any{})
}

// 仓库默认**跟随项目**（02-auth.md §6.1）：项目公开时它跟着公开，
// 项目改公开时它自动跟着公开——不用挨个改仓库。
// 显式给 visibility 就是"这个仓库我要单独定"。
//
// ⚠️ 预建只是把行写出来；真正的镜像内容仍然由 push 事件回填。
func (a *App) handleCreateRepo(w http.ResponseWriter, r *http.Request) {
	project, ok := a.resolveProject(w, r, CodeNotFound, "项目不存在")
	if !ok {
		return
	}
	if !a.isProjectAdmin(r, project) {
		Fail(w, http.StatusForbidden, CodeForbidden, "需要项目管理员权限")
		return
	}
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Visibility  string `json:"visibility"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	repoName := strings.TrimSpace(req.Name)
	if !validRepoPath(repoName) {
		Fail(w, http.StatusBadRequest, CodeInvalidParam,
			"仓库名只能是项目内的相对路径（小写字母、数字与 . _ -，可用 / 分层）")
		return
	}
	repo, err := a.DB.CreateRepository(r.Context(), project.ID, repoName, project.Visibility, req.Description)
	if err == store.ErrDuplicate {
		Fail(w, http.StatusConflict, CodeConflict, "仓库已存在")
		return
	}
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "创建仓库失败")
		return
	}
	// 显式可见性 → 覆盖"跟随项目"。inherit（空串）= 保持跟随。
	if v := domain.Visibility(strings.TrimSpace(req.Visibility)); v != "" {
		if v != domain.VisibilityPublic && v != domain.VisibilityPrivate &&
			v != domain.VisibilityInherit {
			Fail(w, http.StatusBadRequest, CodeInvalidParam, "可见性只能是 inherit、public 或 private")
			return
		}
		if v != domain.VisibilityInherit {
			if err := a.DB.UpdateRepository(r.Context(), repo.ID, &v, nil); err != nil {
				Fail(w, http.StatusInternalServerError, CodeInternal, "设置仓库可见性失败")
				return
			}
			updated, err := a.DB.RepositoryByID(r.Context(), repo.ID)
			if err != nil {
				// 可见性已经改进库里了，但回读失败：
				// 不能拿旧对象当新对象返回——那会让调用方拿到
				// 与数据库真实状态不一致的可见性（AGENTS.md 规则 1）。
				a.Logger.Error("回读仓库失败", "repo", repo.FullName, "err", err)
				Fail(w, http.StatusInternalServerError, CodeInternal, "仓库已创建，但读取仓库信息失败，请刷新确认")
				return
			}
			repo = updated
		}
	}
	if !a.mustAudit(w, r, actorName(r.Context()), "repository.created", repo.FullName, "",
		clientIP(r), true, map[string]any{"visibility": repo.Visibility, "source": repo.VisibilitySource}) {
		return
	}
	OK(w, map[string]any{"repository": repo})
}

// 规则与 Docker 仓库名一致（[a-z0-9] 开头，段内可用 . _ -），
// 但允许 "/" 分层（team-a/sub/app 这种）。
func validRepoPath(path string) bool {
	if path == "" || len(path) > 255 {
		return false
	}
	for _, seg := range strings.Split(path, "/") {
		if seg == "" || len(seg) > 64 {
			return false
		}
		for i, r := range seg {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			case (r == '.' || r == '_' || r == '-') && i > 0:
			default:
				return false
			}
		}
	}
	return true
}

// 只返回当前主体**可见**的仓库——可见性判定一律走权限引擎
// （与 /api/v1/registry/catalog 同一条路径）。
func (a *App) handleListProjectRepos(w http.ResponseWriter, r *http.Request) {
	project, ok := a.resolveProject(w, r, CodeNotFound, "项目不存在")
	if !ok {
		return
	}
	repos, err := a.DB.ListRepositoriesInProject(r.Context(), project.ID)
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "读取仓库失败")
		return
	}
	p := principalFrom(r.Context())
	out := make([]domain.Repository, 0, len(repos))
	for _, repo := range repos {
		d, err := a.Perm.Decide(r.Context(), p, repo.FullName, []string{"pull"})
		if err != nil {
			Fail(w, http.StatusInternalServerError, CodeInternal, "权限判定失败")
			return
		}
		if len(d.Granted) == 0 {
			continue
		}
		out = append(out, repo)
	}
	OK(w, map[string]any{"repositories": out})
}

func (a *App) handleUpdateRepo(w http.ResponseWriter, r *http.Request) {
	repoName := chi.URLParam(r, "repo")
	project, ok := a.resolveProject(w, r, CodeNotFound, "项目不存在")
	if !ok {
		return
	}
	repo, err := a.DB.RepositoryByPath(r.Context(), project.ID, repoName)
	if err == store.ErrNotFound {
		Fail(w, http.StatusNotFound, CodeNotFound, "仓库不存在")
		return
	}
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "读取仓库失败")
		return
	}
	if !a.isProjectAdmin(r, project) {
		Fail(w, http.StatusForbidden, CodeForbidden, "需要项目管理员权限")
		return
	}
	var req struct {
		Visibility  *string `json:"visibility"`
		Description *string `json:"description"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	var vis *domain.Visibility
	if req.Visibility != nil {
		v := domain.Visibility(*req.Visibility)
		// 三态：跟随项目 / 公开 / 私有（02-auth.md §6）。
		if v != domain.VisibilityPublic && v != domain.VisibilityPrivate &&
			v != domain.VisibilityInherit {
			Fail(w, http.StatusBadRequest, CodeInvalidParam, "可见性只能是 inherit、public 或 private")
			return
		}
		vis = &v
	}
	if err := a.DB.UpdateRepository(r.Context(), repo.ID, vis, req.Description); err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "更新仓库失败")
		return
	}
	if !a.mustAudit(w, r, actorName(r.Context()), "repository.updated", repo.FullName, "", clientIP(r), true, nil) {
		return
	}
	OK(w, map[string]any{})
}

// 工具

// isSystemAdmin 判断当前主体是否系统管理员。
func (a *App) isSystemAdmin(r *http.Request) bool {
	p := principalFrom(r.Context())
	return !p.IsAnonymous() && p.User.IsSystemAdmin
}

func (a *App) isProjectAdmin(r *http.Request, project *domain.Project) bool {
	p := principalFrom(r.Context())
	if p.IsAnonymous() {
		return false
	}
	if p.User.IsSystemAdmin {
		return true
	}
	m, err := a.DB.Membership(r.Context(), project.ID, p.User.ID)
	if err != nil {
		// 读失败 → 保守判为"不是管理员"（fail-closed），但必须留痕：
		// 否则数据损坏会被伪装成"权限不够"。
		a.Logger.Error("读取成员关系失败，按非项目管理员处理", "err", err, "project", project.Name)
		return false
	}
	return m != nil && m.Role == domain.RoleAdmin
}

func validProjectName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case (r == '.' || r == '_' || r == '-') && i > 0:
		default:
			return false
		}
	}
	return !strings.HasSuffix(name, ".")
}
