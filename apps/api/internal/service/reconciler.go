package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// Reconciler 是仓库目录的**兜底同步器**（`03-registry.md` §4.3 路径三）。
//
// 为什么必须有它：
//
// 目录的主路径是 distribution 的事件通知（路径一），但那要求上游 distribution
// 配了 `notifications.endpoints`。**独立/远程部署的 distribution 常常没配**
// （用的是镜像自带的默认配置），于是事件永远不来——
// 而 push 本身是成功的（镜像确实躺在 distribution 里），
// 目录页却永远是空的，且**没有任何报错**。
//
// 兜底的做法：按固定周期把上游 catalog 与本地 `repositories` 表对一遍，
// 差异自动补齐。启动后延迟 3s 先跑一轮，之后按 interval 周期跑。
//
// 三件事：
//
//	upstream ∖ local → 建行 + EnqueueRefresh（审计 registry.reconcile.added）
//	local ∖ upstream → 删行 + 删缓存（审计 registry.reconcile.removed）
//	陈旧未刷新      → EnqueueRefresh（不写审计，避免每轮刷屏）
type Reconciler struct {
	db          *store.DB
	upstream    *http.Client
	upstreamURL string
	issuer      cryptoIssuer
	queue       *RefreshQueue
	logger      Logger
	interval    time.Duration
	staleAfter  time.Duration
}

// ReconcilerDeps 是 Reconciler 的依赖。
type ReconcilerDeps struct {
	DB          *store.DB
	Upstream    *http.Client
	UpstreamURL string
	Issuer      cryptoIssuer
	Queue       *RefreshQueue
	Logger      Logger
	// Interval 是对账周期；<= 0 时用 30 分钟。
	Interval time.Duration
	// StaleAfter 是多久没刷新算陈旧；<= 0 时用 24 小时。
	StaleAfter time.Duration
}

const (
	// defaultReconcileInterval 是对账周期默认值。
	defaultReconcileInterval = 30 * time.Minute
	// defaultReconcileStaleAfter 是"陈旧"判定默认值。
	defaultReconcileStaleAfter = 24 * time.Hour
	// reconcileStartDelay 是启动后首次对账的延迟——
	// 等密钥生成、上游连接预热、refresh worker 就绪。
	reconcileStartDelay = 3 * time.Second
	// reconcileCatalogPageSize 是拉上游 catalog 的分页大小。
	reconcileCatalogPageSize = 1000
	// reconcileMaxPages 是分页轮数上限（防御上游返回畸形数据导致死循环）。
	reconcileMaxPages = 1000
)

// NewReconciler 构造 Reconciler。
func NewReconciler(d ReconcilerDeps) *Reconciler {
	interval := d.Interval
	if interval <= 0 {
		interval = defaultReconcileInterval
	}
	staleAfter := d.StaleAfter
	if staleAfter <= 0 {
		staleAfter = defaultReconcileStaleAfter
	}
	return &Reconciler{
		db:          d.DB,
		upstream:    d.Upstream,
		upstreamURL: strings.TrimRight(d.UpstreamURL, "/"),
		issuer:      d.Issuer,
		queue:       d.Queue,
		logger:      d.Logger,
		interval:    interval,
		staleAfter:  staleAfter,
	}
}

// Run 启动对账协程。
//
// 启动后先等 3s 跑第一轮（早于第一轮周期），
// 之后每 interval 跑一次。ctx 结束即退出。
func (r *Reconciler) Run(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(reconcileStartDelay):
	}
	for {
		added, removed, refreshed, err := r.SyncOnce(ctx)
		switch {
		case err != nil:
			r.logger.Error("仓库目录对账失败", "err", err)
		default:
			r.logger.Info("仓库目录对账完成",
				"added", added, "removed", removed, "refreshed", refreshed)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.interval):
		}
	}
}

// roundFailures 统计一轮对账里发生、且**回滚不了**的失败。
//
// 对账动作已经落地（建行 / 删行 / 删缓存），回滚不了；
// 但失败必须让这一轮**报失败**——不能跟成功的轮次长得一样：
// 调用方只看到一个 error，看不出"这轮有东西没做成"。
type roundFailures struct {
	createRows  int // 建行失败
	deleteRows  int // 删行失败
	deleteCache int // 删缓存失败
	writeAudit  int // 写审计失败
	readCache   int // 读缓存失败
}

// err 把本轮的失败汇总成一个错误；没有失败时返回 nil。
func (f *roundFailures) err() error {
	parts := make([]string, 0, 5)
	if f.createRows > 0 {
		parts = append(parts, fmt.Sprintf("建行失败 %d 次", f.createRows))
	}
	if f.deleteRows > 0 {
		parts = append(parts, fmt.Sprintf("删行失败 %d 次", f.deleteRows))
	}
	if f.deleteCache > 0 {
		parts = append(parts, fmt.Sprintf("删缓存失败 %d 次", f.deleteCache))
	}
	if f.writeAudit > 0 {
		parts = append(parts, fmt.Sprintf("写审计失败 %d 次", f.writeAudit))
	}
	if f.readCache > 0 {
		parts = append(parts, fmt.Sprintf("读缓存失败 %d 次", f.readCache))
	}
	if len(parts) == 0 {
		return nil
	}
	return errors.New("对账部分失败：" + strings.Join(parts, "、"))
}

// SyncOnce 执行一轮对账。
//
// 返回新增、删除、重新刷新的仓库数。
func (r *Reconciler) SyncOnce(ctx context.Context) (added, removed, refreshed int, err error) {
	upstreamRepos, err := r.catalog(ctx)
	if err != nil {
		// ⚠️ 拉不到上游 catalog 就**什么都不做**：
		//    上游挂了就当成"全删"，本地目录会被清空。
		return 0, 0, 0, fmt.Errorf("拉取上游 catalog 失败: %w", err)
	}

	local, err := r.db.AllRepositories(ctx)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("读取本地仓库表失败: %w", err)
	}
	localNames := make(map[string]*domain.Repository, len(local))
	for i := range local {
		localNames[local[i].FullName] = &local[i]
	}
	upstreamSet := make(map[string]bool, len(upstreamRepos))
	for _, name := range upstreamRepos {
		upstreamSet[name] = true
	}

	// upstream ∖ local → 建行 + 刷新
	var failures roundFailures
	for name := range upstreamSet {
		if localNames[name] != nil {
			continue
		}
		repo, cerr := r.createRepoRow(ctx, name)
		if cerr != nil {
			// 建行失败 → 这个仓库本轮没有本地行（下一轮会重试，对账是幂等的）。
			// 但本轮**必须报失败**：不能让"建仓失败"和"目录已经齐了"长得一样。
			r.logger.Error("对账建仓失败", "repo", name, "err", cerr)
			failures.createRows++
			continue
		}
		if repo == nil {
			continue
		}
		added++
		r.queue.Enqueue(repo)
		if err := r.db.WriteAudit(ctx, "system", "registry.reconcile.added", name, "", "", true, nil); err != nil {
			r.logger.Error("写审计失败", "action", "registry.reconcile.added", "repo", name, "err", err)
			failures.writeAudit++
		}
	}

	// local ∖ upstream → 删行 + 删缓存
	//
	// ⚠️ **上游 catalog 为空时不删**：上游刚重启/被打挂时 catalog 可能是空的，
	//    那时全删等于把本地目录清空——这是不可逆的数据损失。
	if len(upstreamSet) == 0 {
		r.logger.Warn("上游 catalog 为空，跳过删除对账（防止误删本地目录）",
			"local_count", len(local))
	} else {
		for name, repo := range localNames {
			if upstreamSet[name] {
				// 上游还列着它，但它可能只剩**空壳**（manifest 被删了、目录还在）。
				// 这种仓库一个镜像都没有，留着本地行就是"标签数 0"的幽灵仓库——
				// 用户明明删过，重启后它又冒出来。查清楚了没有镜像就删行。
				hasImages, herr := r.upstreamHasImages(ctx, name)
				if herr != nil {
					// ⚠️ 查不清就**不删**：不确定时保留，误删是数据损失。
					//
					//    这是设计文档声明的安全闸门（`03-registry.md` §4.3
					//    「对账器的安全闸门」表：上游仓库**查不动** → 不删本地行并记 error），
					//    所以这里**显式降级**而不是报错：这个仓库留到下一轮再查，
					//    失败已被记成 Error 级日志（可观测），不是静默跳过。
					r.logger.Error("对账查上游仓库失败，跳过该仓库的删除对账",
						"repo", name, "err", herr)
					continue
				}
				if hasImages {
					continue
				}
				r.logger.Info("上游仓库已无镜像（空壳），删除本地仓库行", "repo", name)
			}
			if derr := r.db.DeleteRepository(ctx, repo.ID); derr != nil {
				// 删行失败 → 这个仓库本轮没删掉（下一轮会重试）。
				// 本轮**必须报失败**：本地目录与上游已经不一致了，
				// 调用方不能从"对账完成"里看不出这件事。
				r.logger.Error("对账删仓失败", "repo", name, "err", derr)
				failures.deleteRows++
				continue
			}
			if err := r.db.DeleteRepoMeta(ctx, repo.ID); err != nil {
				// ⚠️ 仓库行已经删掉、回滚不了，但**缓存行没删掉**会让目录页
				//    继续显示这个仓库的旧 tag 数/大小。
				//    这种"动作已发生、随后的写失败"必须让本轮**报失败**——
				//    不能跟成功的轮次长得一样。
				r.logger.Error("对账删缓存失败", "repo", name, "err", err)
				failures.deleteCache++
			}
			removed++
			if err := r.db.WriteAudit(ctx, "system", "registry.reconcile.removed", name, "", "", true, nil); err != nil {
				r.logger.Error("写审计失败", "action", "registry.reconcile.removed", "repo", name, "err", err)
				failures.writeAudit++
			}
		}
	}

	// 陈旧未刷新 → 重新刷新
	cutoff := time.Now().Add(-r.staleAfter).Unix()
	for name, repo := range localNames {
		meta, merr := r.db.RepoMeta(ctx, repo.ID)
		if merr != nil {
			// 读缓存失败 → 这个仓库本轮不排刷新（下一轮会重试）。
			// 本轮**必须报失败**：缓存读不出来说明本地状态没对上，
			// 不能让"对账完成"盖过去。
			r.logger.Error("读取仓库缓存失败", "repo", name, "err", merr)
			failures.readCache++
			continue
		}
		if meta == nil || meta.RefreshedAt == 0 || meta.RefreshedAt < cutoff {
			refreshed++
			r.queue.Enqueue(repo)
		}
	}
	if err := failures.err(); err != nil {
		// 本轮动作已经发生，但有东西没做成：这一轮**报失败**，
		// 让运维在日志里看见（下一轮会重新对账，幂等）。
		return added, removed, refreshed, err
	}
	return added, removed, refreshed, nil
}

// 返回 nil 表示**不该建**（仓库名不合法、项目不存在、上游只剩空壳）。
//
// ⚠️ 项目必须已存在：镜像名的第一段就是项目名，
//
//	项目不存在说明那是别的 registry 的仓库（上游被直连写过），
//	不能凭空造一个项目出来。
func (r *Reconciler) createRepoRow(ctx context.Context, full string) (*domain.Repository, error) {
	ref, ok := ParseRepoName(full)
	if !ok {
		return nil, nil
	}
	project, err := r.db.ProjectByName(ctx, ref.ProjectName)
	if err != nil {
		if err == store.ErrNotFound {
			r.logger.Warn("上游有该仓库但本地没有对应项目，已跳过建仓",
				"repo", full, "project", ref.ProjectName)
			return nil, nil
		}
		return nil, err
	}
	// ⚠️ **上游 catalog 不是"有镜像"的可靠信号**：
	//    distribution 删掉 manifest 后**仓库目录还在**（还剩层链接），
	//    catalog 照样列它 → 对账器会当成"上游有、本地没有"→ 建行 →
	//    用户删掉的仓库在**重启后复活**，还显示成"标签数 0"的幽灵仓库。
	//
	//    先问上游"真的有镜像吗"，没有就不建。
	hasImages, err := r.upstreamHasImages(ctx, full)
	if err != nil {
		return nil, fmt.Errorf("查询上游仓库镜像失败: %w", err)
	}
	if !hasImages {
		r.logger.Info("上游仓库没有镜像（空壳），跳过建仓", "repo", full)
		return nil, nil
	}
	// 可见性**跟随项目**（`02-auth.md` §6）：
	// 公开项目里的镜像匿名可拉，私有项目里的只有成员能拉。
	// 单个仓库要与项目不同，项目管理员显式改。
	return r.db.CreateRepository(ctx, project.ID, ref.RepoPath, project.Visibility, "")
}

// 判据用 **tags/list**：空 tags = 上游只剩空壳目录（一个镜像都没有）。
//
// ⚠️ 上游返回 404 时按"没有镜像"处理：那说明上游连这个仓库都没了。
//
//	其它非 200 状态**返回错误**（不猜）——调用方据此跳过，绝不误删本地行。
func (r *Reconciler) upstreamHasImages(ctx context.Context, name string) (bool, error) {
	token, err := r.issuer.Issue("lau6-cr-reconciler", []domain.Access{{
		Type: "repository", Name: name, Actions: []string{"pull"},
	}})
	if err != nil {
		return false, fmt.Errorf("签发仓库 token 失败: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		r.upstreamURL+registryPrefix+name+"/tags/list", nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := r.upstream.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("上游 tags/list 返回 %d", resp.StatusCode)
	}
	var payload struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return false, fmt.Errorf("解析上游 tags/list 失败: %w", err)
	}
	return len(payload.Tags) > 0, nil
}

func (r *Reconciler) catalog(ctx context.Context) ([]string, error) {
	// 目录端点在带鉴权的上游需要 registry 作用域的 token
	// （上游的访问控制器认的是 "registry:catalog:*"）。
	token, err := r.issuer.Issue("lau6-cr-reconciler", []domain.Access{{
		Type: "registry", Name: "catalog", Actions: []string{"*"},
	}})
	if err != nil {
		return nil, fmt.Errorf("签发目录 token 失败: %w", err)
	}

	out := []string{}
	last := ""
	for page := 0; page < reconcileMaxPages; page++ {
		q := url.Values{}
		q.Set("n", strconv.Itoa(reconcileCatalogPageSize))
		if last != "" {
			q.Set("last", last)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			r.upstreamURL+registryPrefix+"_catalog?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := r.upstream.Do(req)
		if err != nil {
			return nil, err
		}
		var payload struct {
			Repositories []string `json:"repositories"`
		}
		decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&payload)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("上游 catalog 返回 %d", resp.StatusCode)
		}
		if decodeErr != nil {
			return nil, fmt.Errorf("解析上游 catalog 失败: %w", decodeErr)
		}
		out = append(out, payload.Repositories...)
		if len(payload.Repositories) < reconcileCatalogPageSize {
			break
		}
		last = payload.Repositories[len(payload.Repositories)-1]
	}
	return out, nil
}
