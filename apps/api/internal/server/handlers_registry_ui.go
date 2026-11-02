package server

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
	"github.com/lau6code/lau6-cr/apps/api/internal/service"
)

// 处理 UI 代理的读请求（tags / manifest / blob）。
func (a *App) handleRegistryRead(w http.ResponseWriter, r *http.Request) {
	// 匿名读限流：公开目录是匿名可达的读路径。
	if !a.allowAnonymousRead(w, r, false) {
		return
	}
	repo, _, kind, ok := parseRegistryProxyPath(r.URL.Path)
	if ok && kind == "tags" {
		a.handleRegistryTags(w, r, repo)
		return
	}
	a.proxyRepo(w, r, []string{"pull"})
}

// 返回仓库的 tag 列表。
//
// 与 manifest / blob 不同，它**不是**纯代理：每个 tag 还要带
// 创建时间、大小、平台与 manifest 摘要（删除只能按摘要删）。
// 逐 tag 度量意味着逐 tag 拉上游，所以它只在下钻时调用，
// **不能**放进列表页（那是 N+1 的重灾区）。
func (a *App) handleRegistryTags(w http.ResponseWriter, r *http.Request, repo string) {
	if !a.requireRepoAction(w, r, repo, "pull") {
		return
	}
	tags, err := a.Refresher.TagsOf(r.Context(), repo)
	if err != nil {
		a.Logger.Error("读取标签列表失败", "repo", repo, "err", err)
		Fail(w, http.StatusBadGateway, CodeUnavailable, "读取标签列表失败")
		return
	}
	OK(w, map[string]any{
		"name":        tags.Name,
		"tags":        tags.Tags,
		"pull_prefix": a.pullPrefix(),
	})
}

// 处理删除：按 digest 删单个 tag，或删掉整个镜像。
func (a *App) handleRegistryDelete(w http.ResponseWriter, r *http.Request) {
	if principalFrom(r.Context()).IsAnonymous() {
		// 匿名调用返回 **401** 而不是 404：删除是明确的写操作，
		// 未登录时应该提示"去登录"而不是"资源不存在"。
		Fail(w, http.StatusUnauthorized, CodeUnauthorized, "请先登录")
		return
	}
	repo, suffix, kind, ok := parseRegistryProxyPath(r.URL.Path)
	if !ok {
		// 路径形如 /api/v1/registry/<repo>：删除**整个镜像**（它下面所有 tag）。
		name, nameOK := parseRegistryRepoPath(r.URL.Path)
		if !nameOK {
			Fail(w, http.StatusNotFound, CodeNotVisible, "仓库不存在或无权访问")
			return
		}
		if !a.requireRepoAction(w, r, name, "delete") {
			return
		}
		a.deleteRepository(w, r, name)
		return
	}
	if !a.requireRepoAction(w, r, repo, "delete") {
		return
	}

	if kind != "manifests" {
		// 删除整个镜像 = 把它下面所有 tag 删掉。
		a.deleteRepository(w, r, repo)
		return
	}

	// 单个 tag：registry 规范只允许**按 digest 删**。
	digest := strings.TrimPrefix(suffix, "/manifests/")
	if digest == "" {
		Fail(w, http.StatusBadRequest, CodeInvalidParam, "缺少 manifest 摘要")
		return
	}
	if err := a.Refresher.DeleteTag(r.Context(), repo, digest); err != nil {
		a.Logger.Error("删除 tag 失败", "repo", repo, "digest", digest, "err", err)
		Fail(w, http.StatusBadGateway, CodeUnavailable, "删除失败")
		return
	}
	a.Logger.Info("已删除 tag", "repo", repo, "digest", digest)
	// 收尾失败要告诉用户：删除本身成功了，但目录可能残留空条目。
	if cerr := a.afterDelete(r, repo); cerr != nil {
		OK(w, map[string]any{"deleted": digest, "cleanup_warning": cerr.Error()})
		return
	}
	OK(w, map[string]any{"deleted": digest})
}

// 删除整个镜像（该仓库下的所有 tag）。
func (a *App) deleteRepository(w http.ResponseWriter, r *http.Request, repo string) {
	tags, err := a.Refresher.TagsOf(r.Context(), repo)
	if err != nil {
		a.Logger.Error("读取标签列表失败", "repo", repo, "err", err)
		Fail(w, http.StatusBadGateway, CodeUnavailable, "读取标签列表失败")
		return
	}
	deleted := 0
	var failed []string
	for _, t := range tags.Tags {
		if t.Digest == "" {
			// 拿不到摘要就没法删（registry 规范只认 digest）——记下来，不假装成功。
			// 原因一并带出去：只报一个 tag 名，调用方无从判断是拉取失败还是本来就没有。
			reason := t.Error
			if reason == "" {
				reason = "拿不到摘要"
			}
			failed = append(failed, t.Tag+"（"+reason+"）")
			continue
		}
		if err := a.Refresher.DeleteTag(r.Context(), repo, t.Digest); err != nil {
			a.Logger.Error("删除 tag 失败", "repo", repo, "tag", t.Tag, "err", err)
			failed = append(failed, t.Tag)
			continue
		}
		deleted++
	}
	a.Logger.Info("已删除镜像", "repo", repo, "deleted", deleted, "failed", len(failed))
	cleanupWarning := ""
	if cerr := a.afterDelete(r, repo); cerr != nil {
		cleanupWarning = cerr.Error()
	}
	if len(failed) > 0 {
		// 部分失败要说清楚：不能报"删除成功"却其实没删掉。
		msg := "部分标签删除失败：" + strings.Join(failed, "、")
		if cleanupWarning != "" {
			msg += "；收尾失败：" + cleanupWarning
		}
		Fail(w, http.StatusBadGateway, CodeUnavailable, msg)
		return
	}
	if cleanupWarning != "" {
		OK(w, map[string]any{"deleted": deleted, "cleanup_warning": cleanupWarning})
		return
	}
	OK(w, map[string]any{"deleted": deleted})
}

// 删除后的收尾：刷新目录缓存；一个 tag 都不剩时把仓库行删掉。
//
// 仓库行是"这个命名空间下有这个镜像"的记录；
// 镜像被删光后它就该从目录里消失，否则目录里会留一条空条目。
//
// ⚠️ 收尾失败**必须报出来**：删除本身已经成功（回滚不了），
// 但目录可能因此残留空条目、缓存可能过期。
// 静默 return 会让用户看到的目录和真实状态对不上，且事后无从排查——
// 所以这里返回错误，由调用方放进响应里告诉用户。
func (a *App) afterDelete(r *http.Request, repo string) error {
	ctx := r.Context()
	ref, ok := service.ParseRepoName(repo)
	if !ok {
		err := fmt.Errorf("仓库名不合法: %q", repo)
		a.Logger.Error("删除后收尾失败", "repo", repo, "err", err)
		return err
	}
	project, err := a.DB.ProjectByName(ctx, ref.ProjectName)
	if err != nil {
		a.Logger.Error("删除后读取项目失败", "repo", repo, "project", ref.ProjectName, "err", err)
		return fmt.Errorf("读取项目 %s 失败: %w", ref.ProjectName, err)
	}
	row, err := a.DB.RepositoryByPath(ctx, project.ID, ref.RepoPath)
	if err != nil {
		a.Logger.Error("删除后读取仓库记录失败", "repo", repo, "err", err)
		return fmt.Errorf("读取仓库记录失败: %w", err)
	}
	tags, err := a.Refresher.TagsOf(ctx, repo)
	if err != nil {
		// 查不到就不动：宁可留一条空条目，也不要误删仓库记录。
		a.Logger.Error("删除后统计标签失败", "repo", repo, "err", err)
		return fmt.Errorf("统计标签失败: %w", err)
	}
	if len(tags.Tags) > 0 {
		a.RefreshQ.Enqueue(row)
		return nil
	}
	if err := a.DB.DeleteRepository(ctx, row.ID); err != nil {
		a.Logger.Error("删除空仓库记录失败", "repo", repo, "err", err)
		return fmt.Errorf("删除空仓库记录失败: %w", err)
	}
	a.Logger.Info("镜像已无标签，删除仓库记录", "repo", repo)
	return nil
}

// 判定当前主体对仓库是否有指定动作。
//
// 无权与不存在返回**完全一致**的响应（404）——
// 不能让"没权限"和"没这个仓库"看起来不一样。
func (a *App) requireRepoAction(w http.ResponseWriter, r *http.Request, repo, action string) bool {
	d, err := a.Perm.Decide(r.Context(), principalFrom(r.Context()), repo, []string{action})
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "权限判定失败")
		return false
	}
	if len(d.Granted) == 0 {
		Fail(w, http.StatusNotFound, CodeNotVisible, "仓库不存在或无权访问")
		return false
	}
	return true
}

// 解析 /api/v1/registry/<repo>（后面没有子路径）。
//
// 用于「删除整个镜像」——它针对的是仓库本身，而不是仓库下的某个对象。
// 仓库名里可以带 "/"（形如 team-a/sub/app），所以这里不切分路径。
func parseRegistryRepoPath(p string) (string, bool) {
	const prefix = "/api/v1/registry/"
	if !strings.HasPrefix(p, prefix) {
		return "", false
	}
	rest := strings.TrimPrefix(p, prefix)
	if rest == "" {
		return "", false
	}
	return rest, true
}

// 解析 /api/v1/registry/<repo>/<kind>[/<ref>]。
//
// 仓库名 <project>/<repo> 的 <repo> 段可以带 "/"，chi 的路由树按段匹配，
// 拿不到跨段的 {name} —— 所以按后缀形态自己切分，
// manifest / blob 复用 registry 协议那套解析（含 blob 摘要形态的消歧）。
func parseRegistryProxyPath(p string) (repo, suffix, kind string, ok bool) {
	const prefix = "/api/v1/registry/"
	if !strings.HasPrefix(p, prefix) {
		return "", "", "", false
	}
	rest := p[len(prefix):]
	if strings.HasSuffix(rest, "/tags") {
		repo = strings.TrimSuffix(rest, "/tags")
		if repo == "" {
			return "", "", "", false
		}
		// ⚠️ 上游路径是 `/v2/<name>/tags/list`（registry 规范），
		//    不是 `/v2/<name>/tags`——后者上游会 404。
		return repo, "/tags/list", "tags", true
	}
	rp, good := service.ParseRegistryPath("/v2/" + rest)
	if !good || rp.Repo == "" {
		return "", "", "", false
	}
	if rp.Kind != "manifests" && rp.Kind != "blob" {
		return "", "", "", false
	}
	return rp.Repo, rest[len(rp.Repo):], rp.Kind, true
}

// 代理一个仓库级的 registry 请求。
//
// 走"UI 代理"路径：先按当前主体算权限，
// 再用**现签的短命 token**去访问上游。
func (a *App) proxyRepo(w http.ResponseWriter, r *http.Request, actions []string) {
	repo, suffix, _, ok := parseRegistryProxyPath(r.URL.Path)
	if !ok {
		Fail(w, http.StatusBadRequest, CodeInvalidParam, "仓库名不合法")
		return
	}
	a.proxyRepoWith(w, r, repo, suffix, actions)
}

// proxyRepoWith 是 proxyRepo 的核心：权限判定 + 带上游令牌转发。
func (a *App) proxyRepoWith(w http.ResponseWriter, r *http.Request, repo, suffix string, actions []string) {
	if repo == "" {
		Fail(w, http.StatusBadRequest, CodeInvalidParam, "缺少仓库名")
		return
	}
	principal := principalFrom(r.Context())

	d, err := a.Perm.Decide(r.Context(), principal, repo, actions)
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "权限判定失败")
		return
	}
	if len(d.Granted) == 0 {
		// 无权与不存在返回一致，不泄漏仓库是否存在。
		Fail(w, http.StatusNotFound, CodeNotVisible, "仓库不存在或无权访问")
		return
	}

	token, err := a.Tokens.Issue(subjectOf(principal), []domain.Access{{
		Type: "repository", Name: repo, Actions: d.Granted,
	}})
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "签发上游令牌失败")
		return
	}

	upstreamURL := a.UpstreamAddr + "/v2/" + repo + suffix
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}

	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL, r.Body)
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "构造上游请求失败")
		return
	}
	outReq.Header.Set("Authorization", "Bearer "+token)
	if accept := r.Header.Get("Accept"); accept != "" {
		outReq.Header.Set("Accept", accept)
	}
	outReq.ContentLength = r.ContentLength

	resp, err := a.Upstream.Do(outReq)
	if err != nil {
		Fail(w, http.StatusBadGateway, CodeUnavailable, "上游 registry 不可用")
		return
	}
	defer resp.Body.Close()
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	w.WriteHeader(resp.StatusCode)
	// ⚠️ **必须接住 error**：上游中途断连时，状态码与响应头在上面的
	// w.WriteHeader 已经写出去了——**改不了状态码**（想改成 5xx 就得
	// 先把整个 body 读完再决定，而 blob 可以是几 GiB，缓冲不了）。
	//
	// 客户端看到的是：**连接提前结束 + 被截断的响应体**（半个镜像），
	// 状态码仍是上游给的 200。这是**数据损坏级别**的问题——
	// 客户端会把半个镜像当成完整的存下来。
	//
	// 把 error 丢掉就是"无声的数据损坏"：没有日志，也没有计数能对不上。
	written, copyErr := io.Copy(w, resp.Body)
	if copyErr != nil {
		// ⚠️ a.Logger 可能为 nil（测试里不注入）——判空后再调，
		//    否则这条"上游出问题时才走"的分支会把请求打成 panic。
		if a.Logger != nil {
			a.Logger.Error("上游响应体转发中断：客户端收到的是截断的响应体",
				"repo", repo, "path", r.URL.Path, "method", r.Method,
				"upstream_status", resp.StatusCode, "written", written, "err", copyErr)
		}
	}
}

// 返回 token 的 subject，匿名为 "anonymous"。
func subjectOf(p service.Principal) string {
	if p.IsAnonymous() {
		return "anonymous"
	}
	return p.User.Username
}
