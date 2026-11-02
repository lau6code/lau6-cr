package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
	"github.com/lau6code/lau6-cr/apps/api/internal/service"
)

// 接收 distribution 的事件通知。
//
// 用共享密钥鉴权，**不是** session——distribution 没有用户身份。
func (a *App) handleWebhook(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) || a.WebhookSecret == "" ||
		!constantTimeEqual(strings.TrimSpace(auth[len(prefix):]), a.WebhookSecret) {
		// 失败时不区分"没有密钥"和"密钥错误"，避免被探测。
		Fail(w, http.StatusUnauthorized, CodeUnauthorized, "unauthorized")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		Fail(w, http.StatusBadRequest, CodeInvalidParam, "cannot read body")
		return
	}
	var env struct {
		Events []struct {
			ID     string `json:"id"`
			Action string `json:"action"`
			Target struct {
				MediaType  string `json:"mediaType"`
				Repository string `json:"repository"`
				Tag        string `json:"tag"`
				Digest     string `json:"digest"`
			} `json:"target"`
		} `json:"events"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		Fail(w, http.StatusBadRequest, CodeInvalidParam, "cannot parse body")
		return
	}
	a.handleRegistryEvents(r, env.Events)
	OK(w, map[string]any{})
}

// 处理事件：维护仓库记录、触发缓存刷新。
func (a *App) handleRegistryEvents(r *http.Request, events []struct {
	ID     string `json:"id"`
	Action string `json:"action"`
	Target struct {
		MediaType  string `json:"mediaType"`
		Repository string `json:"repository"`
		Tag        string `json:"tag"`
		Digest     string `json:"digest"`
	} `json:"target"`
}) {
	for _, ev := range events {
		if ev.Target.Repository == "" || !isManifestMediaType(ev.Target.MediaType) {
			continue
		}
		repo, err := a.ensureRepository(r, ev.Target.Repository)
		if err != nil {
			a.Logger.Error("处理 registry 事件失败", "repo", ev.Target.Repository, "err", err)
			continue
		}
		// 事件**成功**也要留痕：distribution 到底有没有把事件发过来，
		// 只有这里能回答。目录空着的时候，第一句要问的就是"事件到了吗"。
		a.Logger.Info("收到 registry 事件",
			"action", ev.Action, "repo", ev.Target.Repository,
			"tag", ev.Target.Tag, "digest", ev.Target.Digest)
		switch ev.Action {
		case "push":
			if ev.Target.Digest != "" {
				if err := a.DB.TouchRepositoryPush(r.Context(), repo.ID, ev.Target.Digest); err != nil {
					// 推送计数与首推摘要没记上：目录里的信息会偏，必须留痕。
					a.Logger.Error("记录仓库推送失败", "repo", ev.Target.Repository, "err", err)
				}
			}
			// 记 tag 的推送时间——保留策略求值要用（`03-registry.md` §5.3）。
			//
			// 这条路径覆盖**直连 distribution** 的推送（代理层看不到），
			// 与代理层的观测互补：两边都记到，推送时间才完整。
			if ev.Target.Digest != "" && ev.Target.Tag != "" {
				now := time.Now().Unix()
				if err := a.DB.UpsertRetentionTag(r.Context(), &domain.RetentionTag{
					RepositoryID: repo.ID,
					Tag:          ev.Target.Tag,
					Digest:       ev.Target.Digest,
					PushedAt:     &now,
				}); err != nil {
					a.Logger.Error("记录 tag 推送快照失败", "repo", ev.Target.Repository,
						"tag", ev.Target.Tag, "err", err)
				}
			}
			a.RefreshQ.Enqueue(repo)
		case "delete":
			a.RefreshQ.Enqueue(repo)
		case "pull":
			if err := a.DB.IncrementPull(r.Context(), repo.ID); err != nil {
				// 拉取计数没加上：流量统计会偏，必须留痕。
				a.Logger.Error("记录拉取计数失败", "repo", ev.Target.Repository, "err", err)
			}
		case "mount":
			a.RefreshQ.Enqueue(repo)
		}
	}
}

// 找到或创建仓库记录（push 事件驱动自动建仓）。
//
// 新仓库的可见性**跟随项目**（`02-auth.md` §6）：
// 公开项目里推上来的镜像就是公开的，私有项目里的是私有的。
// 公开面要求"项目 public 且仓库 public"同时成立，
// 写死 private 会让公开项目里的镜像**匿名拉不到**。
//
// 单个仓库想与项目不同，项目管理员可以在设置里显式改。
func (a *App) ensureRepository(r *http.Request, full string) (*domain.Repository, error) {
	ref, ok := service.ParseRepoName(full)
	if !ok {
		return nil, errInvalidRepoName
	}
	project, err := a.DB.ProjectByName(r.Context(), ref.ProjectName)
	if err != nil {
		return nil, err
	}
	repo, err := a.DB.RepositoryByPath(r.Context(), project.ID, ref.RepoPath)
	if err == nil {
		return repo, nil
	}
	created, err := a.DB.CreateRepository(r.Context(), project.ID, ref.RepoPath, project.Visibility, "")
	if err != nil {
		return nil, err
	}
	return created, nil
}

// errInvalidRepoName 表示仓库名不符合 <project>/<repo> 形式。
var errInvalidRepoName = errors.New("invalid repository name")

// 判断 media type 是否是 manifest 系（blob 不触发刷新）。
func isManifestMediaType(mt string) bool {
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = mt[:i]
	}
	switch strings.TrimSpace(mt) {
	case "application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.oci.image.index.v1+json":
		return true
	}
	return false
}

// 比较 webhook 共享密钥：必须常量时间，避免逐字节提前返回泄漏密钥前缀。
func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}
