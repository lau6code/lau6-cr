// Package service 实现业务编排。
package service

import (
	"context"
	"log/slog"
	"strings"
	"sync"

	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// Action 是 Docker registry 的动作。
type Action string

const (
	ActionPull   Action = "pull"
	ActionPush   Action = "push"
	ActionDelete Action = "delete"
)

// Principal 是请求主体。
//
// 三种形态：
//
//   - User != nil：已认证用户
//   - Project != nil：**项目令牌**（项目身份，02-auth.md §2.6.2）
//   - 两者都为 nil：匿名请求
type Principal struct {
	User *domain.User
	// Project 是项目令牌主体。
	//
	// 它**不是**用户：不是 project_members 的一行，没有用户名，
	// 权限来自令牌自带的角色（创建时选定、此后不变）。
	Project *domain.ProjectToken
}

// anonymousSubject 是匿名主体在 registry token 里的 subject。
//
// 它同时是"这枚 token 不是某个真实用户签的"的标记：
// 凭它回查账户查不到人，于是按匿名处理。
const anonymousSubject = "anonymous"

// projectSubjectPrefix 是项目令牌在 registry token 里的 subject 前缀。
//
// 完整形态是 `project:<项目名>`——人类可读，审计与转发给上游都用它。
const projectSubjectPrefix = "project:"

// ProjectSubject 造项目令牌在 registry token 里的 subject。
func ProjectSubject(projectName string) string {
	return projectSubjectPrefix + projectName
}

func (p Principal) IsAnonymous() bool { return p.User == nil && p.Project == nil }

// PermissionEngine 负责所有仓库访问判定。
//
// **所有鉴权都必须走这里**，UI 代理、token realm、manifest 守卫共用同一份实现，
// 避免出现两套逻辑导致的实现漂移。
type PermissionEngine struct {
	db *store.DB
	// anonRead 返回"是否允许匿名主体免鉴权拉取公开仓库"。
	//
	// ⚠️ 接线是**必须的**：它就是公开面的紧急止血阀
	// （access.anonymous_read_enabled），不接等于永远放行。
	anonRead func() bool
	logger   *slog.Logger
	// anonReadWarn 保证"开关没接线"只报一次，不刷屏。
	anonReadWarn sync.Once
}

// PermissionOptions 是权限引擎的依赖。
type PermissionOptions struct {
	// AnonymousReadEnabled 返回"匿名读总开关"当前的值。
	//
	// 必须接上设置快照（settings.Snapshot().Access.AnonymousReadEnabled），
	// 否则这个开关改了不起作用——那正是它作为紧急开关最不能有的毛病。
	AnonymousReadEnabled func() bool
	// Logger 可选：用于报告"开关没接线"这类配置错误。
	Logger *slog.Logger
}

// NewPermissionEngine 构造权限引擎。
func NewPermissionEngine(db *store.DB, o PermissionOptions) *PermissionEngine {
	return &PermissionEngine{db: db, anonRead: o.AnonymousReadEnabled, logger: o.Logger}
}

// ⚠️ 没接线（nil）时按**关闭**处理：这个开关的用途是
// "公开面出问题时一键止血"，读不到它的状态就不能默认放行——
// 那正是它要防的那件事。接线漏了属于配置错误，会记 error 日志。
func (e *PermissionEngine) anonymousReadAllowed() bool {
	if e.anonRead == nil {
		e.anonReadWarn.Do(func() {
			if e.logger != nil {
				e.logger.Error("匿名读开关未接线：匿名拉取一律拒绝（access.anonymous_read_enabled 不生效）")
			}
		})
		return false
	}
	return e.anonRead()
}

// RepoRef 是解析后的仓库引用。
type RepoRef struct {
	ProjectName string
	RepoPath    string
	FullName    string
}

// ParseRepoName 把 Docker 仓库名拆成项目名与项目内路径。
//
// 仓库名固定为 <project>/<repo>，项目是 Docker 名称空间的第一段。
// 无 "/" 的一律拒绝。
func ParseRepoName(full string) (RepoRef, bool) {
	idx := strings.Index(full, "/")
	if idx <= 0 || idx == len(full)-1 {
		return RepoRef{}, false
	}
	return RepoRef{
		ProjectName: full[:idx],
		RepoPath:    full[idx+1:],
		FullName:    full,
	}, true
}

// Decision 是一次判定的结果。
type Decision struct {
	// Granted 是授予的动作集合（可能为空）。
	Granted []string
	// Reason 是拒绝原因（供上层给操作者一句人话）。
	//
	// 默认空串 = "没有更多信息"（上层按不泄露处理）。
	// 唯一的例外是 ReasonProjectNotFound：项目是 Docker 命名空间，
	// "项目没建"是**操作前提**而不是权限问题，直说更省事。
	Reason string
	// ReasonProject 是 ReasonProjectNotFound 时的项目名。
	ReasonProject string
}

// ReasonProjectNotFound 表示"项目（Docker 命名空间）不存在"。
const ReasonProjectNotFound = "project_not_found"

func (d Decision) Allow(action Action) bool {
	for _, g := range d.Granted {
		if g == string(action) || g == "*" {
			return true
		}
	}
	return false
}

// Decide 是**唯一的**仓库访问判定入口。
//
// 判定顺序（顺序本身是安全属性，不可随意调整）：
//
//  0. 解析仓库名 → 项目名 + 项目内路径
//  1. 定位项目；**项目不存在 → 一律 DENY（系统管理员也不例外）**
//  2. 系统管理员 → 全局绕过（**项目内**的绕过）
//  3. 定位仓库；仓库不存在 → 自动建仓授权（带三重闸门）
//  4. 项目令牌主体 → 只在本项目内生效
//  5. 成员关系
//  6. 公开面：项目 public 且仓库 public → 只对匿名/非成员授予 pull
//     （匿名还要过"匿名读总开关"——公开面的一键止血阀）
//  7. 必须已认证
//  8. 非成员且不在公开面 → 拒绝
//  9. 项目管理员 → 项目内全权
//  10. 私有仓库 → 需要显式授权
//  11. 项目内仓库通配收窄
//  12. 角色 → 动作集
func (e *PermissionEngine) Decide(ctx context.Context, p Principal, fullRepo string, requested []string) (Decision, error) {
	// 0. 解析仓库名
	ref, ok := ParseRepoName(fullRepo)
	if !ok {
		return Decision{}, nil
	}

	// 1. 定位项目
	project, err := e.db.ProjectByName(ctx, ref.ProjectName)
	if err == store.ErrNotFound {
		// ⚠️ **项目不存在 → 一律 DENY，系统管理员也不例外。**
		//
		//    镜像名的第一段就是项目名，也就是 Docker 的**命名空间**。
		//    往不存在的命名空间推，会在上游留下一个**没人管得了的孤儿镜像**：
		//    目录里列不出来（没有项目行 → 没有可见性与授权可言），
		//    也无法按项目做权限管控。
		//
		//    管理员的"全局绕过"是**项目内**的绕过，
		//    不是"能凭空造出一个命名空间"。
		//    见 `02-auth.md` §6："若项目本身不存在 → DENY"。
		//
		//    ⚠️ 这里带上 Reason：上层要能对**已认证**的调用方说清
		//    "项目 xxx 不存在"（否则操作者只看到一句"无权限"，
		//    客户端还会反复重试）。未认证路径不暴露（见 token.go）。
		return Decision{
			Reason:        ReasonProjectNotFound,
			ReasonProject: ref.ProjectName,
		}, nil
	}
	if err != nil {
		return Decision{}, err
	}

	// 2. 系统管理员：全局绕过（项目存在的前提下）
	if !p.IsAnonymous() && p.User != nil && p.User.IsSystemAdmin {
		return Decision{
			Granted: intersect(requested, []string{"pull", "push", "delete"}),
		}, nil
	}

	// 3. 定位仓库；仓库不存在 → 自动建仓授权
	repo, err := e.db.RepositoryByPath(ctx, project.ID, ref.RepoPath)
	if err == store.ErrNotFound {
		return e.decideAutoCreate(ctx, p, project, requested)
	}
	if err != nil {
		return Decision{}, err
	}

	// 4. 项目令牌主体：只在本项目内生效，权限 = 令牌自带的角色
	//
	// ⚠️ 这一步必须在**公开面短路（第 5 步）之前**：项目令牌不是
	//    project_members 的一行，member 查出来是 nil，
	//    落到公开面会被降级成"只读 pull"。
	//
	// ⚠️ 也必须在**系统管理员绕过之后**：is_system_admin 是用户主体的标记，
	//    对项目令牌没有意义。
	//
	// ⚠️ 还要在**定位仓库之后**：仓库还不存在时走的是自动建仓授权，
	//    那条路径只授予 pull/push（delete 不可逆，见 decideAutoCreate）。
	if p.Project != nil {
		if p.Project.ProjectID != project.ID {
			// 跨项目一律拒绝：项目令牌是项目身份，别的项目与它无关。
			return Decision{}, nil
		}
		return Decision{
			Granted: intersect(requested, p.Project.Role.Actions()),
		}, nil
	}

	// 5. 成员关系（可能为 nil）
	var member *domain.Member
	if !p.IsAnonymous() && p.User != nil {
		member, err = e.db.Membership(ctx, project.ID, p.User.ID)
		if err != nil {
			return Decision{}, err
		}
	}

	// 6. 公开面：项目 public 且仓库 public
	//
	// ⚠️ 短路**只对匿名与非成员生效**。项目成员必须继续走角色判定，
	// 否则公开项目里的开发者永远拿不到 push/delete。
	if project.Visibility == domain.VisibilityPublic && repo.Visibility == domain.VisibilityPublic {
		if p.IsAnonymous() || member == nil {
			// 匿名读总开关（access.anonymous_read_enabled）：
			// 公开面出问题时的一键止血阀。关掉它 = 公开仓库也必须登录才能拉。
			//
			// 只挡**匿名**主体：已登录但不是成员的访问仍然放行
			// （那不是"免鉴权"，用户本来就带着身份来的）。
			if p.IsAnonymous() && !e.anonymousReadAllowed() {
				return Decision{}, nil
			}
			return Decision{
				Granted: intersect(requested, []string{"pull"}),
			}, nil
		}
		// 成员：继续往下走
	}

	// 7. 必须已认证
	if p.IsAnonymous() {
		return Decision{}, nil
	}

	// 8. 非成员且不在公开面 → 拒绝
	if member == nil {
		return Decision{}, nil
	}

	// 9. 项目管理员：项目内全权（含未授权的私有仓库）
	if member.Role == domain.RoleAdmin {
		return Decision{
			Granted: intersect(requested, []string{"pull", "push", "delete"}),
		}, nil
	}

	// 10. 私有仓库需要显式授权
	if repo.Visibility == domain.VisibilityPrivate {
		granted, err := e.db.HasGrant(ctx, repo.ID, p.User.ID)
		if err != nil {
			return Decision{}, err
		}
		if !granted {
			return Decision{}, nil
		}
	}

	// 11. 项目内仓库通配收窄
	if len(member.RepoPatterns) > 0 && !MatchAnyPattern(member.RepoPatterns, ref.RepoPath) {
		return Decision{}, nil
	}

	// 12. 角色 → 动作集
	return Decision{
		Granted: intersect(requested, member.Role.Actions()),
	}, nil
}

// decideAutoCreate 处理"仓库尚不存在"的情况。
//
// 三重闸门缺一不可：
//
//	① 主体已认证 —— 匿名绝不能建仓
//	② 主体是项目成员 —— 非成员不能在自己不存在的项目里建仓
//	③ 角色含 push —— 只读成员不能建仓
//
// 项目"公开"只意味着**可读**，不意味着谁都能建仓。
func (e *PermissionEngine) decideAutoCreate(ctx context.Context, p Principal, project *domain.Project, requested []string) (Decision, error) {
	if p.IsAnonymous() {
		return Decision{}, nil
	}
	// 项目令牌：与第 3b 步同口径——只在本项目内，且只授予 pull/push。
	if p.Project != nil {
		if p.Project.ProjectID != project.ID {
			return Decision{}, nil
		}
		return Decision{Granted: intersect(requested, []string{"pull", "push"})}, nil
	}
	member, err := e.db.Membership(ctx, project.ID, p.User.ID)
	if err != nil {
		return Decision{}, err
	}
	if member == nil {
		return Decision{}, nil
	}
	hasPush := false
	for _, a := range member.Role.Actions() {
		if a == string(ActionPush) {
			hasPush = true
			break
		}
	}
	if !hasPush {
		return Decision{}, nil
	}
	// 只授予 pull / push —— delete 是不可逆动作，必须显式建仓或由管理员授权。
	granted := intersect(requested, []string{"pull", "push"})
	return Decision{Granted: granted}, nil
}
