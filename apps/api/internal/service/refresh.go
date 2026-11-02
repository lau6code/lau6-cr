package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// RefreshQueue 是 Catalog 缓存的刷新队列。
//
// 单 goroutine 消费，pending set 去重保证同一仓库同时只有一次刷新。
type RefreshQueue struct {
	pending map[string]bool
	signal  chan struct{}
	mu      sync.Mutex
	stopCh  chan struct{}
	once    sync.Once
}

// NewRefreshQueue 创建刷新队列。
func NewRefreshQueue() *RefreshQueue {
	return &RefreshQueue{
		pending: map[string]bool{},
		signal:  make(chan struct{}, 1),
		stopCh:  make(chan struct{}),
	}
}

// Enqueue 把仓库放进刷新队列（重复入队会被去重）。
func (q *RefreshQueue) Enqueue(repo *domain.Repository) {
	if repo == nil {
		return
	}
	key := repo.FullName
	q.mu.Lock()
	if q.pending[key] {
		q.mu.Unlock()
		return
	}
	q.pending[key] = true
	q.mu.Unlock()

	select {
	case q.signal <- struct{}{}:
	default: // 已有信号在队列里，不用重复唤醒
	}
}

// Take 取出下一个待刷新的仓库。
func (q *RefreshQueue) Take() (string, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for k := range q.pending {
		delete(q.pending, k)
		return k, true
	}
	return "", false
}

// Close 停止队列。
func (q *RefreshQueue) Close() {
	q.once.Do(func() { close(q.stopCh) })
}

// StopCh 返回停止信号。
func (q *RefreshQueue) StopCh() <-chan struct{} { return q.stopCh }

// Signal 返回唤醒信号。
func (q *RefreshQueue) Signal() <-chan struct{} { return q.signal }

// Refresher 负责重算 repo_meta。
type Refresher struct {
	db          *store.DB
	upstream    *http.Client
	upstreamURL string
	issuer      cryptoIssuer
	queue       *RefreshQueue
	logger      Logger
}

// cryptoIssuer 是签发内部 token 的最小接口。
type cryptoIssuer interface {
	Issue(subject string, access []domain.Access) (string, error)
}

// Logger 是日志接口（避免与 slog 强耦合）。
type Logger interface {
	Error(msg string, args ...any)
	// Warn 是"出问题了但还能继续"的级别。
	//
	// 鉴权拒绝、上游返回 4xx 这类**调用方能看见的失败**必须落在 warn 及以上——
	// 默认级别是 info，记在 debug 里等于没记：
	// "客户端报 unauthorized 但后端一片安静"就是这么来的。
	Warn(msg string, args ...any)
	// Info 是常规运行信息（请求完成、token 签发等）。
	Info(msg string, args ...any)
	// Debug 是排障用的细粒度日志。
	//
	// 默认级别（info）下它不产出任何内容——
	// 只在设置页把日志级别调成 debug 时才可见。
	Debug(msg string, args ...any)
}

// NewRefresher 构造刷新器。
func NewRefresher(db *store.DB, upstream *http.Client, upstreamURL string, issuer cryptoIssuer, queue *RefreshQueue, logger Logger) *Refresher {
	return &Refresher{
		db: db, upstream: upstream, upstreamURL: strings.TrimRight(upstreamURL, "/"),
		issuer: issuer, queue: queue, logger: logger,
	}
}

// Run 启动刷新协程。
func (r *Refresher) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.queue.StopCh():
			return
		case <-r.queue.Signal():
			for {
				full, ok := r.queue.Take()
				if !ok {
					break
				}
				if err := r.refreshOne(ctx, full); err != nil {
					r.logger.Error("刷新仓库缓存失败", "repo", full, "err", err)
				}
			}
		}
	}
}

// refreshOne 重算某仓库的 repo_meta。
func (r *Refresher) refreshOne(ctx context.Context, full string) error {
	ref, ok := ParseRepoName(full)
	if !ok {
		return nil
	}
	project, err := r.db.ProjectByName(ctx, ref.ProjectName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// 项目没了（连带仓库行也没了）→ 跳过，不是错误。
			r.logger.Debug("刷新跳过：项目不存在", "repo", full)
			return nil
		}
		return err
	}
	repo, err := r.db.RepositoryByPath(ctx, project.ID, ref.RepoPath)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			r.logger.Debug("刷新跳过：仓库行不存在", "repo", full)
			return nil
		}
		return err
	}

	tags, err := r.listTags(ctx, full)
	if err != nil {
		return err
	}
	if len(tags) == 0 {
		// 仓库已空：删缓存行（仓库记录本身保留）。
		return r.db.DeleteRepoMeta(ctx, repo.ID)
	}

	latest := pickRepresentativeTag(tags)
	manifest, err := r.getManifest(ctx, full, latest)
	if err != nil {
		// ⚠️ 拉不到 manifest 就**报错**，不写半成品 meta。
		//    吞掉错误会写出一行 size=0 / created="" / platforms=[] 的缓存，
		//    目录页显示"有镜像但大小为 0"，而且**没有任何日志**。
		return err
	}
	m, err := r.measureManifest(ctx, full, manifest)
	if err != nil {
		return err
	}
	meta := &domain.RepoMeta{
		RepositoryID: repo.ID,
		LatestTag:    latest,
		TagCount:     len(tags),
		Size:         m.size,
		Created:      m.created,
		Platforms:    m.platforms,
	}
	return r.db.UpsertRepoMeta(ctx, meta)
}

// TagInfo 是单个 tag 的度量结果。
type TagInfo struct {
	// Tag 是标签名。
	Tag string `json:"tag"`
	// Created 是镜像的创建时间（ISO 8601，来自镜像 config blob）。
	Created string `json:"created"`
	// Size 是镜像大小（字节）。
	Size int64 `json:"size"`
	// Platforms 是该 tag 覆盖的平台（os/architecture[/variant]）。
	Platforms []string `json:"platforms"`
	// Digest 是该 tag 的 manifest 摘要——**删除只能按它来**
	// （registry 规范：只能按 digest 删 manifest）。
	Digest string `json:"digest"`
	// Error 是这一条 tag 的失败原因（拉不到 manifest / 度量失败）。
	//
	// 空串 = 这一条是好的。
	//
	// ⚠️ 为什么必须显式带上失败原因：光看空字段分不出
	// "这个 tag 真的没有 digest"和"拉取失败了"——
	// 两者在返回结构里长得一模一样，页面就少了一个 tag 却没人知道为什么。
	Error string `json:"error,omitempty"`
}

// RepoTags 是某仓库的 tag 列表与逐 tag 度量。
type RepoTags struct {
	// Name 是完整仓库名（<project>/<repo>）。
	Name string `json:"name"`
	// Tags 是各 tag 的度量。
	Tags []TagInfo `json:"tags"`
}

// TagsOf 返回仓库的 tag 列表及每个 tag 的创建时间/大小/平台/摘要。
//
// 逐 tag 度量意味着逐 tag 拉 manifest（多架构还要再拉子 manifest 与 config blob），
// 所以**别在列表页调它**——那是 N+1 的重灾区。
// 它是给"点开某个镜像看它的全部 tag"这种**下钻**用的。
func (r *Refresher) TagsOf(ctx context.Context, full string) (*RepoTags, error) {
	names, err := r.listTags(ctx, full)
	if err != nil {
		return nil, err
	}
	out := &RepoTags{Name: full, Tags: make([]TagInfo, 0, len(names))}
	for _, name := range names {
		info := TagInfo{Tag: name, Platforms: []string{}}
		manifest, digest, err := r.getManifestWithDigest(ctx, full, name)
		if err != nil {
			// 单个 tag 拉不到**不影响其它 tag**：记下来继续。
			// 但必须留痕——否则页面上少了一个 tag 却没人知道为什么。
			// 失败原因还要**放进返回值**：只写日志的话，
			// 调用方（页面 / 删除流程）拿到的还是一条"空"的 tag。
			r.logger.Error("读取 tag 的 manifest 失败", "repo", full, "tag", name, "err", err)
			info.Error = "读取 manifest 失败: " + err.Error()
			out.Tags = append(out.Tags, info)
			continue
		}
		info.Digest = digest
		m, err := r.measureManifest(ctx, full, manifest)
		if err != nil {
			r.logger.Error("度量 tag 失败", "repo", full, "tag", name, "err", err)
			info.Error = "度量失败: " + err.Error()
		} else {
			info.Created = m.created
			info.Size = m.size
			info.Platforms = m.platforms
		}
		out.Tags = append(out.Tags, info)
	}
	return out, nil
}

// TagDigests 拉取仓库的 tag 列表，以及每个 tag 指向的 manifest digest。
//
// 与 TagsOf 的区别：**只取 digest，不拉 config blob**。
// 清理只需要知道"哪个 tag 指向哪个 manifest"，
// 尺寸/平台/创建时间对它没用——省掉这些请求能把开销从
// "每 tag 一次 manifest + 一次 blob"降到"每 tag 一次 manifest"。
//
// ⚠️ 单个 tag 拉不到 manifest 时**不中断整个列表**：
//
//	该 tag 以空 digest 返回，策略求值会因"拿不到 digest"而保留它。
func (r *Refresher) TagDigests(ctx context.Context, full string) ([]domain.RetentionTag, error) {
	names, err := r.listTags(ctx, full)
	if err != nil {
		return nil, err
	}
	out := make([]domain.RetentionTag, 0, len(names))
	for _, name := range names {
		_, digest, err := r.getManifestWithDigest(ctx, full, name)
		if err != nil {
			// 记下来但不让整个列表失败——少删一个 tag 比整个仓库都不清理好。
			r.logger.Error("读取 tag 的 manifest 失败", "repo", full, "tag", name, "err", err)
			out = append(out, domain.RetentionTag{Tag: name})
			continue
		}
		out = append(out, domain.RetentionTag{Tag: name, Digest: digest})
	}
	return out, nil
}

// DeleteTag 按 digest 删掉一个 tag（manifest）。
//
// registry 规范只允许按 digest 删——按 tag 名删要先解析成 digest，
// 而且同一个 digest 可能有多个 tag 指向它。
func (r *Refresher) DeleteTag(ctx context.Context, full, digest string) error {
	token, err := r.issuer.Issue("lau6-cr-delete", []domain.Access{{
		Type: "repository", Name: full, Actions: []string{"pull", "push", "delete"},
	}})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		r.upstreamURL+registryPrefix+full+"/manifests/"+digest, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := r.upstream.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// 把响应体读完（丢弃）：DELETE 的响应体不参与任何判断，
	// drain 一下只是为了让这条连接能被复用。
	//
	// ⚠️ **这里的 error 可以忽略**：
	//    判定结果只来自**状态码**（manifest 删没删掉），
	//    响应体不参与任何判断；drain 失败最多让这次连接不被复用，
	//    不影响"manifest 是否已删除"的结论。
	//    把它当成错误抛出去反而会误伤已经删成功的删除动作，
	//    所以这里显式容忍——但**不能悄悄吞掉**，故留注释说明。
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("上游返回 %d", resp.StatusCode)
	}
	return nil
}

// 优先用上游给的 Docker-Content-Digest 头；没有就退回自己算 sha256(body)。
func (r *Refresher) getManifestWithDigest(ctx context.Context, full, reference string) (map[string]any, string, error) {
	token, err := r.issuer.Issue("lau6-cr-refresher", []domain.Access{{
		Type: "repository", Name: full, Actions: []string{"pull"},
	}})
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		r.upstreamURL+registryPrefix+full+"/manifests/"+reference, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	// ⚠️ 必须带 Accept：缺省时上游不认 OCI image manifest（404 MANIFEST_UNKNOWN）。
	req.Header.Set("Accept", manifestAcceptHeader)
	resp, err := r.upstream.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("上游返回 %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, "", err
	}
	digest := resp.Header.Get("Docker-Content-Digest")
	if digest == "" {
		sum := sha256.Sum256(body)
		digest = "sha256:" + hex.EncodeToString(sum[:])
	}
	return out, digest, nil
}

func (r *Refresher) listTags(ctx context.Context, full string) ([]string, error) {
	body, err := r.get(ctx, full, "/tags/list")
	if err != nil {
		return nil, err
	}
	var out struct {
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return out.Tags, nil
}

func (r *Refresher) getManifest(ctx context.Context, full, reference string) (map[string]any, error) {
	body, err := r.get(ctx, full, "/manifests/"+reference)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *Refresher) getBlob(ctx context.Context, full, digest string) ([]byte, error) {
	return r.get(ctx, full, "/blobs/"+digest)
}

func (r *Refresher) get(ctx context.Context, full, suffix string) ([]byte, error) {
	token, err := r.issuer.Issue("lau6-cr-refresher", []domain.Access{{
		Type: "repository", Name: full, Actions: []string{"pull"},
	}})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		r.upstreamURL+registryPrefix+full+suffix, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	// ⚠️ 同上：manifest 的 Accept 头不能省，否则 OCI image manifest 拉不到。
	req.Header.Set("Accept", manifestAcceptHeader)
	resp, err := r.upstream.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("上游返回 %d", resp.StatusCode)
	}
	return b, nil
}
