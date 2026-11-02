package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
)

// manifestMediaTypes 是本仓库支持的 manifest 媒体类型（**有序**）。
//
// 顺序固定：Accept 头按这个顺序拼，便于比对与排查。
var manifestMediaTypes = []string{
	"application/vnd.docker.distribution.manifest.v2+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.oci.image.index.v1+json",
}

// allowedManifestMediaTypes 是允许写入的 manifest 媒体类型白名单。
//
// 白名单里的类型是 registry 能正常处理的；其余类型写进去会生成
// **拉不下来的脏数据**——比写失败更难排查。
var allowedManifestMediaTypes = func() map[string]bool {
	m := make(map[string]bool, len(manifestMediaTypes))
	for _, t := range manifestMediaTypes {
		m[t] = true
	}
	return m
}()

// manifestAcceptHeader 是拉取 manifest 时必须带的 Accept 头。
//
// ⚠️ **不能省**：上游 distribution 对 manifest 的 GET/HEAD 会检查 Accept，
// 缺省时只认 Docker v2 那几种媒体类型。缺 Accept（只带 Docker v2 默认值）
// 去拉 **OCI image manifest** 会得到 404 MANIFEST_UNKNOWN
// （"OCI manifest found, but accept header does not support OCI manifests"）——
// 多架构镜像的子 manifest 就是这种，拉不到就量不出尺寸与平台。
var manifestAcceptHeader = strings.Join(manifestMediaTypes, ", ")

func normalizeMediaType(ct string) string {
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.ToLower(strings.TrimSpace(ct))
}

// PutManifest 处理 manifest 写入，带 tag 覆盖保护。
func (s *RegistryService) PutManifest(w http.ResponseWriter, r *http.Request) {
	rp, ok := ParseRegistryPath(r.URL.Path)
	if !ok || rp.Repo == "" {
		s.writeRegistryError(w, http.StatusBadRequest, "NAME_INVALID",
			"invalid repository or manifest reference", nil)
		return
	}

	policy := s.currentPolicy()

	// ⚠️ 鉴权**必须先于**策略判断。
	//
	// tag 覆盖保护是**附加**检查，不是鉴权的替代品。
	// "策略关闭 → 直接透传"等于**完全不做鉴权**：
	// 匿名 `PUT /v2/<repo>/manifests/<tag>` 会被原样转发给上游，
	// 上游没配鉴权时就真写进去了 —— 任何人都能覆盖任意仓库的 tag
	// （而 blob 上传是有鉴权的，所以这个洞特别隐蔽）。
	//
	// 无论策略开关如何，**写 manifest 必须有 push 权限**。
	claims, ok := s.authorize(w, r, rp.Repo, ActionPush)
	if !ok {
		return
	}

	if !policy.PreventTagOverwrite {
		// 策略关闭 → 透明转发（鉴权已过，零额外开销）。
		s.stream(w, r, rp, nil, claims)
		return
	}
	if s.logger != nil {
		s.logger.Debug("manifest PUT 进入覆盖保护检查",
			"repo", rp.Repo, "reference", rp.Reference)
	}

	// 收集本次要写入的 tag：?tag= 参数 + reference（非 digest 时）。
	tags, err := collectTags(r, rp.Reference)
	if err != nil {
		s.writeRegistryError(w, http.StatusBadRequest, "TAG_INVALID", "invalid manifest tag", nil)
		return
	}
	if len(tags) == 0 {
		s.writeRegistryError(w, http.StatusBadRequest, "TAG_INVALID", "invalid manifest tag", nil)
		return
	}
	protected := []string{}
	for _, t := range tags {
		if !policy.Allows(t) {
			protected = append(protected, t)
		}
	}
	if len(protected) == 0 {
		s.stream(w, r, rp, nil, claims)
		return
	}

	// ① 鉴权已在函数开头完成（没有 push 权限的调用方在进媒体类型校验前
	//    就已经被拒了，不会先撞上 400）。
	//    这里**不再验一遍**：用户级令牌每验一次都要回缓存取一次密钥，
	//    重复验签等于把一次 push 变成两次缓存往返。

	// ② 媒体类型白名单：不在白名单的类型写进去会变成拉不下来的脏数据，
	//    比写失败更难排查。
	if mt := normalizeMediaType(r.Header.Get("Content-Type")); mt != "" &&
		!allowedManifestMediaTypes[mt] {
		s.writeRegistryError(w, http.StatusBadRequest, "MANIFEST_INVALID",
			"unsupported manifest media type", map[string]any{"media_type": mt})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, s.maxManifestBytes+1))
	if err != nil {
		s.writeRegistryError(w, http.StatusBadRequest, "MANIFEST_INVALID", "cannot read manifest", nil)
		return
	}
	if int64(len(body)) > s.maxManifestBytes {
		s.writeRegistryError(w, http.StatusRequestEntityTooLarge, "MANIFEST_INVALID",
			fmt.Sprintf("manifest exceeds %d MiB limit", s.maxManifestBytes>>20), nil)
		return
	}
	sum := sha256.Sum256(body)
	attempted := "sha256:" + hex.EncodeToString(sum[:])

	// 多 tag 时按字典序取锁，避免死锁。
	sort.Strings(protected)
	keys := make([]string, 0, len(protected))
	for _, t := range protected {
		keys = append(keys, rp.Repo+"\x00"+t)
	}
	unlock := s.locks.lock(keys)
	defer unlock()

	for _, tag := range protected {
		current, err := s.currentTagDigest(r.Context(), rp.Repo, tag)
		if err != nil {
			// **fail closed**：无法确认状态时拒绝写入。
			s.writeRegistryError(w, http.StatusServiceUnavailable, "DENIED",
				"cannot verify current tag state; overwrite protection failed closed", nil)
			return
		}
		if current == "" {
			continue // tag 不存在 → 首次发布，允许
		}
		if current != attempted {
			if s.logger != nil {
				s.logger.Debug("tag 覆盖被拒",
					"repo", rp.Repo, "tag", tag,
					"current_digest", current, "attempted_digest", attempted)
			}
			// 失败路径审计：这次写入本来就被拒了（响应是 409 DENIED），
			// 审计写失败不能改写拒绝的原因，但必须留痕：
			// error 日志 + store 层的失败计数。
			if err := s.db.WriteAudit(r.Context(), "system", "image.tag.overwrite_denied",
				rp.Repo+":"+tag, "", clientIPOf(r), false, map[string]any{
					"current_digest":   current,
					"attempted_digest": attempted,
				}); err != nil && s.logger != nil {
				s.logger.Error("写审计失败", "action", "image.tag.overwrite_denied",
					"repo", rp.Repo, "tag", tag, "err", err)
			}
			s.writeRegistryError(w, http.StatusConflict, "DENIED",
				"tag is immutable and cannot be overwritten", map[string]any{
					"name": rp.Repo, "tag": tag,
					"current_digest": current, "attempted_digest": attempted,
				})
			return
		}
	}

	s.stream(w, r, rp, body, claims)
}

// currentTagDigest 读当前 tag 的 digest。
//
// 返回空字符串表示 tag 不存在（允许写）；返回错误表示**无法确认**（fail closed）。
func (s *RegistryService) currentTagDigest(ctx context.Context, repo, tag string) (string, error) {
	raw, err := s.issuer.Issue("lau6-cr-tag-guard", []domain.Access{{
		Type: "repository", Name: repo, Actions: []string{"pull"},
	}})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead,
		s.upstreamURL+registryPrefix+repo+"/manifests/"+tag, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+raw)
	req.Header.Set("Accept", manifestAcceptHeader)

	resp, err := s.upstream.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	// 把响应体读完（丢弃）：HEAD 响应没有实体体，这里只是为了让连接能被复用。
	//
	// ⚠️ **这里的 error 可以忽略**（与上面的转发路径不同）：
	//    判定结果只来自**状态码与响应头**（Docker-Content-Digest），
	//    响应体不参与任何判断；drain 失败最多让这次连接不被复用，
	//    不影响"tag 是否已存在"的结论。
	//    把它当成错误抛出去反而会误伤正常的 tag 覆盖保护，
	//    所以这里显式容忍——但**不能悄悄吞掉**，故留注释说明。
	_, _ = io.Copy(io.Discard, resp.Body)

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return "", nil // 不存在，允许
	case resp.StatusCode != http.StatusOK:
		return "", fmt.Errorf("上游返回 %d", resp.StatusCode)
	}
	digest := resp.Header.Get("Docker-Content-Digest")
	if digest == "" {
		return "", fmt.Errorf("上游未返回 Docker-Content-Digest")
	}
	return digest, nil
}

func (s *RegistryService) currentPolicy() Policy {
	if s.policyFn == nil {
		return Policy{}
	}
	return s.policyFn()
}

// collectTags 收集本次写入涉及的 tag。
//
// tag 名里**不允许出现 "/"**（Docker tag 的字符集不含路径分隔符）。
// 遇到非法 tag 返回错误而不是静默跳过——静默跳过会让调用方以为
// 那个 tag 被写入了，实际并没有。
func collectTags(r *http.Request, reference string) ([]string, error) {
	set := map[string]bool{}
	for _, t := range r.URL.Query()["tag"] {
		if t == "" {
			continue
		}
		if strings.Contains(t, "/") {
			return nil, fmt.Errorf("tag %q 含有非法字符 '/'", t)
		}
		set[t] = true
	}
	// reference 不是 digest 时，它本身就是一个 tag。
	if reference != "" && !isDigest(reference) {
		if strings.Contains(reference, "/") {
			return nil, fmt.Errorf("tag %q 含有非法字符 '/'", reference)
		}
		set[reference] = true
	}
	out := make([]string, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Strings(out)
	return out, nil
}

// onManifestPushed 处理"检测到一次 manifest 写入"。
//
// 做三件事：
//  1. 确保仓库行存在（上游第一次出现的仓库，DB 里还没有行）；
//  2. 把仓库排进刷新队列——刷新 worker 会去上游算 tag/大小/平台，写进 repo_meta；
//  3. 记下这个 tag 的推送时间（保留策略求值要用，见 03-registry.md §5.3），
//     并触发该项目的保留策略。
//
// 这条路径让目录在 **push 后立刻**更新，不依赖上游的事件通知配置，
// 也不必等 30 分钟一轮的对账（对账仍是兜底）。
func (s *RegistryService) onManifestPushed(ctx context.Context, r *http.Request, rp RegistryPath, digest string) {
	full := rp.Repo
	if s.refreshQueue == nil || full == "" {
		return
	}
	ref, ok := ParseRepoName(full)
	if !ok {
		return
	}
	project, err := s.db.ProjectByName(ctx, ref.ProjectName)
	if err != nil {
		// 项目不存在 → 不是我们的仓库（镜像名第一段就是项目名）。
		s.logWarn("manifest 写入后建仓失败：项目不存在", r, "repo", full, "err", err)
		return
	}
	repo, err := s.db.RepositoryByPath(ctx, project.ID, ref.RepoPath)
	if err != nil {
		// 新仓库的可见性**跟随项目**（`02-auth.md` §6）：
		// 公开项目里推上来的镜像就该能匿名拉，
		// 私有项目里的则是私有的。写死 private 会让公开项目里的镜像
		// 匿名拉不到（公开面要求"项目 public 且仓库 public"）。
		repo, err = s.db.CreateRepository(ctx, project.ID, ref.RepoPath, project.Visibility, "")
		if err != nil {
			s.logWarn("manifest 写入后建仓失败", r, "repo", full, "err", err)
			return
		}
	}
	s.refreshQueue.Enqueue(repo)

	// 记 tag 快照（推送时间）
	//
	// ⚠️ 只有**按 tag 名** PUT 才有 tag 快照可言：
	//    PUT /manifests/<digest> 是"按摘要写"，不改变任何 tag 的指向，
	//    把它记成某个 tag 的推送时间会是假数据。
	tag := rp.Reference
	if digest != "" && reTagName.MatchString(tag) {
		now := time.Now().Unix()
		if err := s.db.UpsertRetentionTag(ctx, &domain.RetentionTag{
			RepositoryID: repo.ID,
			Tag:          tag,
			Digest:       digest,
			PushedAt:     &now,
		}); err != nil {
			// 快照写失败不影响 push 本身，但必须留痕——
			// 后果是"这个 tag 的推送时间未知"，策略会因此保留它（安全方向）。
			s.logError("记录 tag 推送快照失败", r, "repo", full, "tag", tag, "err", err)
		}
	}

	if s.cleanupTrigger != nil {
		s.cleanupTrigger(ctx, project.ID, project.Name)
	}
}
