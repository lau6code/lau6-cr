package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/service"
)

func (a *App) handleRepoTraffic(w http.ResponseWriter, r *http.Request) {
	full, ok := parseRepoTrafficPath(r.URL.Path)
	if !ok {
		Fail(w, http.StatusBadRequest, CodeInvalidParam, "仓库名不合法")
		return
	}
	ref, ok := service.ParseRepoName(full)
	if !ok {
		Fail(w, http.StatusBadRequest, CodeInvalidParam, "仓库名不合法")
		return
	}
	project, err := a.DB.ProjectByName(r.Context(), ref.ProjectName)
	if err != nil {
		Fail(w, http.StatusNotFound, CodeNotVisible, "仓库不存在或无权访问")
		return
	}
	repo, err := a.DB.RepositoryByPath(r.Context(), project.ID, ref.RepoPath)
	if err != nil {
		Fail(w, http.StatusNotFound, CodeNotVisible, "仓库不存在或无权访问")
		return
	}
	p := principalFrom(r.Context())
	if p.IsAnonymous() {
		// 流量是运维数据，匿名看不到。
		Fail(w, http.StatusUnauthorized, CodeUnauthorized, "请先登录")
		return
	}
	d, err := a.Perm.Decide(r.Context(), p, full, []string{"pull"})
	if err != nil || len(d.Granted) == 0 {
		Fail(w, http.StatusNotFound, CodeNotVisible, "仓库不存在或无权访问")
		return
	}
	from, to, gran := parseTimeRange(r)
	series, err := a.TrafficSvc.RepositoryTraffic(r.Context(), repo.ID, from, to, gran)
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "读取流量失败")
		return
	}
	series.Repository = full
	OK(w, series)
}

// 解析 /api/v1/repositories/<repo>/traffic。
//
// 仓库名 <project>/<repo> 的 <repo> 段可以带 "/"，chi 的路由树按段匹配，
// 拿不到跨段的 {name} —— 所以按后缀形态自己切分。
func parseRepoTrafficPath(p string) (string, bool) {
	const prefix = "/api/v1/repositories/"
	if !strings.HasPrefix(p, prefix) {
		return "", false
	}
	rest := p[len(prefix):]
	if !strings.HasSuffix(rest, "/traffic") {
		return "", false
	}
	repo := strings.TrimSuffix(rest, "/traffic")
	if repo == "" {
		return "", false
	}
	return repo, true
}

func (a *App) handleSystemTraffic(w http.ResponseWriter, r *http.Request) {
	from, to, _ := parseTimeRange(r)
	series, err := a.TrafficSvc.SystemTraffic(r.Context(), from, to)
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "读取流量失败")
		return
	}
	OK(w, series)
}

func (a *App) handleSystemTrafficTop(w http.ResponseWriter, r *http.Request) {
	from, to, _ := parseTimeRange(r)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	metric := r.URL.Query().Get("metric")
	top, err := a.TrafficSvc.TopRepositories(r.Context(), metric, limit, from, to)
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "读取流量失败")
		return
	}
	OK(w, map[string]any{"top": top})
}

func (a *App) handleStorageStatus(w http.ResponseWriter, r *http.Request) {
	cfg := a.SettingsSvc.Snapshot()
	OK(w, map[string]any{
		"driver": cfg.Storage.Driver,
		"local":  map[string]any{"root": cfg.Storage.Local.Root},
		"s3": map[string]any{
			"endpoint":       cfg.Storage.S3.Endpoint,
			"region":         cfg.Storage.S3.Region,
			"bucket":         cfg.Storage.S3.Bucket,
			"path_style":     cfg.Storage.S3.PathStyle,
			"access_key_set": cfg.Storage.S3.AccessKey != "",
			"secret_key_set": cfg.Storage.S3.SecretKey != "",
		},
	})
}

func (a *App) handleAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	offset, _ := strconv.Atoi(q.Get("offset"))
	logs, err := a.DB.ListAudit(r.Context(), q.Get("actor"), q.Get("action"), limit, offset)
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "读取审计失败")
		return
	}
	// audit_write_failures：审计写失败的累计次数。
	// 审计链上有洞是必须被看见的事——它挂在同一个响应里，
	// 运维查审计时顺手就能知道"这里少了记录"。
	OK(w, map[string]any{"logs": logs, "audit_write_failures": auditFailures()})
}

// 解析时间范围与粒度。
func parseTimeRange(r *http.Request) (from, to int64, granularity string) {
	q := r.URL.Query()
	now := time.Now()
	to = now.Unix()
	from = to - 7*24*3600
	if v := q.Get("from"); v != "" {
		if t, err := time.Parse("2006-01-02", v); err == nil {
			from = t.Unix()
		}
	}
	if v := q.Get("to"); v != "" {
		if t, err := time.Parse("2006-01-02", v); err == nil {
			to = t.Add(24 * time.Hour).Unix()
		}
	}
	granularity = q.Get("granularity")
	if granularity != "day" {
		granularity = "hour"
	}
	return from, to, granularity
}
