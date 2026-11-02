package server

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
)

// 返回当前主体可见的仓库名列表。
//
// 走数据库而不是上游 catalog —— 上游返回的是**所有**仓库，
// 还得再过滤一遍，白白把不可见的仓库名泄出去。
func (a *App) handleCatalog(w http.ResponseWriter, r *http.Request) {
	// 匿名读限流：公开目录是匿名可达的读路径。
	if !a.allowAnonymousRead(w, r, false) {
		return
	}
	repos, err := a.Perm.VisibleRepositories(r.Context(), principalFrom(r.Context()))
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "读取仓库列表失败")
		return
	}
	names := make([]string, 0, len(repos))
	for _, repo := range repos {
		names = append(names, repo.FullName)
	}
	OK(w, map[string]any{"repositories": names})
}

// 返回列表页需要的全部信息（一次拿全，避免 N+1）。
func (a *App) handleOverview(w http.ResponseWriter, r *http.Request) {
	// 匿名读限流：列表页是匿名可达的读路径。
	if !a.allowAnonymousRead(w, r, false) {
		return
	}
	repos, err := a.Perm.VisibleRepositories(r.Context(), principalFrom(r.Context()))
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "读取仓库列表失败")
		return
	}
	type item struct {
		domain.Repository
		LatestTag string   `json:"latest_tag"`
		TagCount  int      `json:"tag_count"`
		Size      int64    `json:"size"`
		Created   string   `json:"created"`
		Platforms []string `json:"platforms"`
	}
	out := make([]item, 0, len(repos))
	for _, repo := range repos {
		it := item{Repository: repo, Platforms: []string{}}
		meta, err := a.DB.RepoMeta(r.Context(), repo.ID)
		if err != nil {
			// 读不到缓存行时**不能**当成"这个仓库还没被刷新过"——
			// 那两种情况在列表里长得一模一样（标签数 0、大小 0），
			// 调用方分不出"没数据"和"读失败"。宁可整个列表失败，
			// 也不给一份掺了假空值的清单（AGENTS.md 规则 1）。
			a.Logger.Error("读取仓库缓存失败", "repo", repo.FullName, "err", err)
			Fail(w, http.StatusInternalServerError, CodeInternal, "读取仓库缓存失败")
			return
		}
		if meta != nil {
			it.LatestTag = meta.LatestTag
			it.TagCount = meta.TagCount
			it.Size = meta.Size
			it.Created = meta.Created
			it.Platforms = meta.Platforms
		}
		out = append(out, it)
	}
	// pull_prefix 是拼 `docker pull` 命令用的前缀（host[:port]，不含协议）。
	//
	// 它取的是**对外访问地址**：客户端就是用这个地址访问 registry 的，
	// 所以拼出来的命令才是能直接粘贴执行的那一条。
	// 放在目录接口的响应里，页面就不用再单独问一次后端要地址。
	OK(w, map[string]any{"repositories": out, "pull_prefix": a.pullPrefix()})
}

// 返回拼 docker pull 命令用的前缀（host[:port]，不含协议）。
//
// 取自 core.public_url；解析不出来就返回空串，
// 前端会退回到浏览器当前地址（与 Harbor 的做法一致）。
func (a *App) pullPrefix() string {
	if a.Cfg == nil {
		return ""
	}
	u, err := url.Parse(a.Cfg.Core.PublicURL)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host + strings.TrimSuffix(u.Path, "/")
}
