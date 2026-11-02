package service

import (
	"bytes"
	"context"
	"io"
	"net/http"

	"github.com/lau6code/lau6-cr/apps/api/internal/crypto"
	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
)

// countingReader 统计从底层 Reader 读走的字节数（入向流量）。
//
// ⚠️ 与出向计数是两回事：io.TeeReader 记的是**写出去**的字节，
// 拿它数请求体会把方向记反。
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// upstreamToken 换发一枚给上游 distribution 用的令牌。
//
// claims 为 nil（客户端没带令牌）→ 返回空串：不带 Authorization 转发。
//
// 有令牌 → 用**实例级** EdDSA 签一枚新的，载荷与客户端令牌一致
// （subject 与 access 原样带过去）。
//
// 为什么 access 要原样带过去：上游 distribution 会拿令牌的 access
// 自己对仓库与动作做判定（03-registry.md §3）。
// lau6-cr 已经判过一次，两边判的必须是同一件事——
// 把 access 裁剪过的结果原样透传，上游看到的授权面才和客户端一样。
//
// ⚠️ 上游令牌用**实例级 TTL**（core.token_ttl_seconds，默认 5 分钟）：
//
//	它只是一次转发的一次性通行证，不是客户端长期持有的凭据。
//	客户端那枚 30 天的用户级令牌才是长期凭据。
func (s *RegistryService) upstreamToken(claims *crypto.RegistryClaims) (string, error) {
	if claims == nil {
		return "", nil
	}
	access := claims.Access
	if access == nil {
		access = []domain.Access{}
	}
	return s.issuer.Issue(claims.Subject, access)
}

// stream 把请求转发到上游 distribution。
//
// body 为 nil 表示直接透传（流式，不缓冲）；非 nil 表示已读入内存的请求体。
//
// claims 是客户端令牌的已验签载荷（没有令牌时为 nil）。
// 它决定转发给上游时带哪枚令牌——见 upstreamToken 的注释。
func (s *RegistryService) stream(w http.ResponseWriter, r *http.Request, rp RegistryPath, body []byte, claims *crypto.RegistryClaims) {
	// 单次流式转发的兜底超时
	//
	// ⚠️ 必须挂在**转发请求自己的 context** 上：
	//    http.Client 的整体 Timeout 会对整个响应体（含下载）生效，
	//    大镜像会中途被掐断（unexpected EOF）。
	//    per-request context 只限制这次转发的总时长，
	//    且随客户端断开立即取消。
	//
	// 见 01-architecture.md §3.3 与 09-entrypoint.md §4.3。
	ctx, cancel := context.WithTimeout(r.Context(), streamTimeout)
	defer cancel()

	outReq, err := http.NewRequestWithContext(ctx, r.Method,
		s.upstreamURL+r.URL.RequestURI(), nil)
	if err != nil {
		s.writeRegistryError(w, http.StatusInternalServerError, "DENIED", "cannot build upstream request", nil)
		return
	}
	if body != nil {
		outReq.Body = io.NopCloser(bytes.NewReader(body))
		outReq.ContentLength = int64(len(body))
	} else if r.Body != nil {
		outReq.Body = r.Body
		outReq.ContentLength = r.ContentLength
	}
	// 入向字节：上传分块（PATCH/PUT /blobs/uploads/）计入。
	// ⚠️ 计数器必须在**发请求之前**包上——请求体是流式转发的，
	//    http.Client 在 Do() 里就把它读走了，事后包就永远数不到字节。
	var inCounter *countingReader
	if rp.Kind == "upload" &&
		(r.Method == http.MethodPatch || r.Method == http.MethodPut) &&
		s.stats != nil && body == nil {
		inCounter = &countingReader{r: r.Body}
		outReq.Body = io.NopCloser(inCounter)
	}

	copyRequestHeaders(outReq.Header, r.Header)
	outReq.Host = r.Host

	// ⚠️ **上游令牌必须换发**。
	//
	// 客户端带的是**用户级令牌**（HS256，用用户自己的密钥签），
	// 而 distribution 只认 JWKS 里的 EdDSA 公钥——直接转发过去必然 401。
	// 所以这里用客户端令牌的载荷换发一枚**实例级**令牌（EdDSA）给上游。
	//
	// 换发而不是透传还有一个副作用好处：上游永远只看到 lau6-cr 自己签的令牌，
	// 客户端的凭据（哪怕是 Basic 口令）不会漏给上游。
	upstreamToken, err := s.upstreamToken(claims)
	if err != nil {
		s.logError("签发上游令牌失败", r, "err", err)
		s.writeRegistryError(w, http.StatusInternalServerError, "DENIED", "cannot issue upstream token", nil)
		return
	}
	if upstreamToken == "" {
		outReq.Header.Del("Authorization")
	} else {
		outReq.Header.Set("Authorization", "Bearer "+upstreamToken)
	}

	resp, err := s.upstream.Do(outReq)
	if err != nil {
		// 上游不可达时记一条 error——这是"docker pull 全部失败"的根因。
		// 这里必须自己记：转发层的错误对用户只表现为 502，
		// 没有日志就完全看不出是上游挂了还是权限问题。
		s.logError("上游 registry 请求失败", r, "err", err)
		s.writeRegistryError(w, http.StatusBadGateway, "DENIED", "upstream registry unavailable", nil)
		return
	}
	defer resp.Body.Close()

	// 转发结果（方法/仓库/上游状态码）是排障 pull 失败的第一手信息：
	// 404 是仓库不存在，401 是权限，500 是上游自己坏了。
	//
	// ⚠️ 4xx/5xx 必须落在 **warn 及以上**：默认日志级别是 info，
	// 只记 debug 的话线上只剩"一片安静"——排障时完全看不出上游返回了什么。
	if s.logger != nil {
		attrs := []any{
			"method", r.Method, "repo", rp.Repo, "path", r.URL.Path,
			"status", resp.StatusCode,
		}
		if resp.StatusCode >= http.StatusBadRequest {
			s.logger.Warn("上游 registry 返回错误状态", attrs...)
		} else {
			s.logger.Debug("转发 registry 请求", attrs...)
		}
	}

	// 流量统计
	//
	// 口径见 docs/design/10-traffic-stats.md §1.3 的采集点对照表。
	//
	// ⚠️ 只有**上游成功**的请求才计数：守卫拒绝（403/409/503）与上游报错
	// （4xx/5xx）根本没有发生镜像传输，计进去会让"推送次数"和仓库实际内容对不上。
	ok := resp.StatusCode >= 200 && resp.StatusCode < 300

	copyResponseHeaders(w.Header(), resp.Header)
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	w.WriteHeader(resp.StatusCode)

	// 检测到一次成功的 manifest 写入 → 立刻排队刷新目录
	//
	// lau6-cr **就在 push 的数据路径上**：每一次 manifest 写入都经过这里。
	// 所以"有人推了新镜像"这件事它本来就知道，不必等上游的回调
	// （远程 distribution 常常压根没配 notifications）或等 30 分钟一轮的对账。
	//
	// 触发条件收紧为「manifest PUT + 上游回 2xx」：
	// 守卫拦下的（409）与上游报错（4xx/5xx）都不触发。
	if rp.Kind == "manifests" && r.Method == http.MethodPut &&
		resp.StatusCode >= 200 && resp.StatusCode < 300 {
		s.onManifestPushed(r.Context(), r, rp, resp.Header.Get("Docker-Content-Digest"))
	}

	// 出向字节：io.Copy 的返回值就是真正写给客户端的字节数。
	//
	// ⚠️ **必须接住 error**：上游中途断连时，状态码与响应头在上面的
	//    w.WriteHeader(resp.StatusCode) 就已经写出去了——**改不了状态码**。
	//    想改成 5xx 就得先把整个 body 读完再决定，而 blob 可以是几 GiB，
	//    根本缓冲不了，所以这条路走不通，**不是"忘了处理"**。
	//
	//    客户端实际看到的是：**连接提前结束 + 被截断的响应体**
	//    （半个 blob / 半个 manifest），状态码仍是上游给的 200。
	//    这是**数据损坏级别**的问题——客户端会把半个镜像当成完整的存下来。
	//    把 error 丢掉就是"无声的数据损坏"：既没有日志，也没有计数能对不上，
	//    运维完全看不出来。所以这里记 **Error** 级日志。
	//
	//    统计口径保持诚实：AddOut 记的仍然是**实际写给客户端的字节数**
	//    （io.Copy 的返回值本来就是这个），少记的那部分由这条日志兜底说明。
	written, copyErr := io.Copy(w, resp.Body)
	if copyErr != nil {
		s.logError("上游响应体转发中断：客户端收到的是截断的响应体", r,
			"repo", rp.Repo, "upstream_status", resp.StatusCode,
			"written", written, "err", copyErr)
	}

	// 把这次请求的次数与字节记进当前小时的桶
	//
	// ⚠️ stats 可能为 nil（各处都有 nil 判断，这里也必须有），
	//    否则 Note 里的 a.enabled.Load() 会空指针 panic，
	//    把一次正常的转发打成 500。
	if s.stats != nil {
		if ok {
			s.stats.Note(rp, r.Method)
			// 匿名请求的来源 IP：只在 stats.record_ip 打开时才记
			// （默认关闭 = 热路径上连哈希都不算，见
			// docs/design/10-traffic-stats.md §6 / ADR 14）。
			//
			// ⚠️ HEAD 一律不记：探活请求会把命中次数刷虚
			//    （与流量计数的口径一致，ADR 2）。
			if r.Method != http.MethodHead && isAnonymousClaims(claims) {
				s.stats.NoteIP(rp.Repo, statsClientIP(r))
			}
		}
		// 出向字节：只有 GET blob 计入（manifest 太小，不计字节）。
		if ok && rp.Kind == "blob" && r.Method == http.MethodGet {
			s.stats.AddOut(rp.Repo, written)
		}
		switch {
		case inCounter != nil:
			s.stats.AddIn(rp.Repo, inCounter.n)
		case ok && rp.Kind == "upload" && body != nil &&
			(r.Method == http.MethodPatch || r.Method == http.MethodPut):
			s.stats.AddIn(rp.Repo, int64(len(body)))
		}
	}
}
