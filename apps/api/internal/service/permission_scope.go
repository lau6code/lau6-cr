package service

import (
	"context"
	"strings"

	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
)

// VisibleRepositories 返回主体可见的仓库列表（用于 UI 目录与过滤）。
//
// 可见性 = 公开面 ∪ 成员可见 ∪ 被授权；再叠加项目内仓库通配收窄。
func (e *PermissionEngine) VisibleRepositories(ctx context.Context, p Principal) ([]domain.Repository, error) {
	all, err := e.db.AllRepositories(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Repository, 0, len(all))
	for _, repo := range all {
		d, err := e.Decide(ctx, p, repo.FullName, []string{"pull"})
		if err != nil {
			return nil, err
		}
		if len(d.Granted) == 0 {
			continue
		}
		// 成员被 repo_patterns 排除的仓库不应出现在列表里。
		//
		// ⚠️ 只对**用户主体**做这件事：项目令牌没有成员关系，
		//    它的收窄由角色决定（角色不收窄仓库范围）。
		if !p.IsAnonymous() && p.User != nil {
			project, err := e.db.ProjectByName(ctx, repo.ProjectName)
			if err != nil {
				return nil, err
			}
			member, err := e.db.Membership(ctx, project.ID, p.User.ID)
			if err != nil {
				return nil, err
			}
			if member != nil && len(member.RepoPatterns) > 0 &&
				!MatchAnyPattern(member.RepoPatterns, repo.Name) {
				continue
			}
		}
		out = append(out, repo)
	}
	return out, nil
}

// Scope 是 Docker registry 的 scope 请求。
type Scope struct {
	Type    string
	Name    string
	Actions []string
}

// ParseScope 解析 "repository:team-a/myapp:pull,push"。
func ParseScope(s string) (Scope, bool) {
	parts := strings.SplitN(s, ":", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
		return Scope{}, false
	}
	acts := []string{}
	for _, a := range strings.Split(parts[2], ",") {
		a = strings.TrimSpace(a)
		if a != "" {
			acts = append(acts, a)
		}
	}
	return Scope{Type: parts[0], Name: parts[1], Actions: acts}, true
}

// MatchAnyPattern 判断 name 是否命中任一 glob 模式。
//
// 模式里 "*" 只匹配**一段**路径，不跨 "/"：
//
//	"myapp"     → 精确匹配
//	"sub/*"     → 匹配 sub/xxx
//	"*"         → 匹配单段
func MatchAnyPattern(patterns []string, name string) bool {
	for _, p := range patterns {
		if MatchPattern(p, name) {
			return true
		}
	}
	return false
}

// MatchPattern 判断 name 是否命中单个 glob 模式。
func MatchPattern(pattern, name string) bool {
	if pattern == "" {
		return false
	}
	if pattern == "*" {
		// "*" 只匹配单段，不跨 "/"
		return !strings.Contains(name, "/")
	}
	pp := strings.Split(pattern, "/")
	np := strings.Split(name, "/")
	if len(pp) != len(np) {
		return false
	}
	for i := range pp {
		if pp[i] == "*" {
			continue
		}
		if pp[i] != np[i] {
			return false
		}
	}
	return true
}

// intersect 求两个动作集合的交集，保留 a 的顺序。
//
// 任一侧含 "*" 时展开为全量动作集。
func intersect(a, b []string) []string {
	full := map[string]bool{"pull": true, "push": true, "delete": true}
	expand := func(xs []string) map[string]bool {
		m := map[string]bool{}
		for _, x := range xs {
			if x == "*" {
				for k := range full {
					m[k] = true
				}
				continue
			}
			m[x] = true
		}
		return m
	}
	bm := expand(b)
	out := []string{}
	seen := map[string]bool{}
	for _, x := range a {
		if x == "*" {
			for _, k := range []string{"pull", "push", "delete"} {
				if bm[k] && !seen[k] {
					seen[k] = true
					out = append(out, k)
				}
			}
			continue
		}
		if bm[x] && !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}
