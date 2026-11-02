package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// catalog 处理 /v2/_catalog。
//
// ⚠️ **不能把上游的 catalog 原样透传**：上游返回的是**全部**仓库，
// 包含私有仓库名——匿名调用方据此就能枚举出所有项目与仓库。
//
// 这里按当前主体的可见性过滤后再返回：
// 拉上游列表 → 按可见性过滤 → 返回。
// 过滤依据是权限引擎，与 UI 的 /api/v1/registry/catalog 同一套规则。
//
// 主体来自 Bearer token（token 的 subject 就是用户名）；
// 没有 token 或 token 无效 → 匿名，只能看到公开仓库。
func (s *RegistryService) catalog(w http.ResponseWriter, r *http.Request) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet,
		s.upstreamURL+registryPrefix+"_catalog", nil)
	if err != nil {
		s.logError("构造上游 catalog 请求失败", r, "err", err)
		s.writeRegistryError(w, http.StatusInternalServerError, "DENIED", "cannot build upstream request", nil)
		return
	}
	resp, err := s.upstream.Do(req)
	if err != nil {
		s.logError("上游 registry catalog 请求失败", r, "err", err)
		s.writeRegistryError(w, http.StatusBadGateway, "DENIED", "upstream registry unavailable", nil)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// 上游自己出错：状态码原样回传（不要吞成 200 空列表）。
		s.logWarn("上游 registry catalog 返回错误状态", r, "status", resp.StatusCode)
		copyResponseHeaders(w.Header(), resp.Header)
		w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
		w.WriteHeader(resp.StatusCode)
		// ⚠️ 同样要接住 error：状态码已经写出去了（改不了），
		//    客户端看到的是**截断的错误信封**（JSON 解析失败）。
		//    级别比 blob 截断低一档（warn）：这里的上游状态码本来就是错误码，
		//    客户端一定会失败，截断只让错误信息不完整，
		//    不像 blob 截断那样是"半个镜像被当成完整的"（数据损坏）。
		written, copyErr := io.Copy(w, resp.Body)
		if copyErr != nil {
			s.logWarn("上游 catalog 错误响应体转发中断：客户端收到的是截断的错误信封", r,
				"upstream_status", resp.StatusCode, "written", written, "err", copyErr)
		}
		return
	}

	var payload struct {
		Repositories []string `json:"repositories"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&payload); err != nil {
		// 解不出来就**不放行**：直接透传原始 body 等于把上游内容当目录列表发出去。
		s.logError("解析上游 catalog 失败", r, "err", err)
		s.writeRegistryError(w, http.StatusBadGateway, "DENIED", "cannot parse upstream catalog", nil)
		return
	}

	visible, err := s.perm.VisibleRepositories(r.Context(), s.catalogPrincipal(r.Context(), r))
	if err != nil {
		// fail closed：算不出可见性就不给列表。
		s.logError("计算可见仓库列表失败", r, "err", err)
		s.writeRegistryError(w, http.StatusInternalServerError, "DENIED", "authorization check failed", nil)
		return
	}
	allowed := make(map[string]bool, len(visible))
	for _, repo := range visible {
		allowed[repo.FullName] = true
	}
	out := make([]string, 0, len(payload.Repositories))
	for _, name := range payload.Repositories {
		if allowed[name] {
			out = append(out, name)
		}
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	w.WriteHeader(http.StatusOK)
	// ⚠️ 同样要接住 error：状态码已经写出去了（改不了），
	//    客户端看到的是**截断的 JSON**（解析失败，目录当成损坏的）。
	//    丢掉这个 error 就是"无声的失败"：客户端只看到一个解析不了的响应体，
	//    日志里却什么都没有。
	if err := json.NewEncoder(w).Encode(map[string]any{"repositories": out}); err != nil {
		s.logWarn("catalog 响应体写出失败：客户端收到的是截断的目录列表", r, "err", err)
	}
}

// catalogPrincipal 解析列目录时的主体。
//
// token 的 subject 就是用户名，据此回查账户；查不到（匿名 token、
// 账号已删、账号被禁用）→ 匿名主体，只能看到公开仓库。
//
// ⚠️ **降级路径必须留痕**：降级后目录里只剩公开仓库，
// 外部表现是"我的私有仓库不见了"——没有日志的话，运维无从判断
// 是令牌过期、密钥轮换、账号被删还是 DB 故障。
// 降级本身**只能靠日志被发现**。
//
// ⚠️ 本函数**不改变授权结果**：三种失败仍然按匿名主体处理（公开仓库可见），
// 本次只补可观测性。
func (s *RegistryService) catalogPrincipal(ctx context.Context, r *http.Request) Principal {
	raw := bearerOf(r)
	if raw == "" {
		// 完全没带令牌：匿名是**预期**行为，不是异常，不记日志
		//（catalog 是热路径，不该被正常流量刷屏）。
		return Principal{}
	}
	claims, err := s.issuer.Verify(r.Context(), raw)
	if err != nil {
		// 令牌验签不过（过期 / 签名不匹配 / 密钥轮换后失效）→ 降级成匿名。
		//
		// ⚠️ **为什么仍然降级而不是报错**：令牌过期是运行期常态，
		//    客户端自己会重新换令牌；直接回 401 会让它拿着同一枚坏令牌
		//    反复重试。授权结果保持不变（公开仓库可见）。
		//
		// 级别选 **warn**：令牌过期/签名不匹配是运行期常见情况，
		// 用 error 会在热路径上刷屏；但它必须落在 warn 及以上——
		// 默认日志级别是 info，记在 debug 里等于没记。
		//
		// token_subject 只是**未验签的线索**（见 tokenSubjectHint），
		// 用来回答"这枚坏令牌是谁的"，不作为任何授权依据。
		s.logWarn("catalog 令牌验签失败：按匿名主体处理", r,
			"err", err, "token_subject", tokenSubjectHint(raw))
		return Principal{}
	}
	if claims.Subject == "" || claims.Subject == anonymousSubject {
		// 匿名令牌：预期的正常路径，不记日志。
		return Principal{}
	}
	// 项目令牌主体：`sub` 是 `project:<项目名>`，不是用户名。
	//
	// ⚠️ 必须用 claims.TokenID 定位到**具体那枚**令牌——
	//    一个项目可能有多枚角色不同的令牌，按项目名找会挑错。
	if strings.HasPrefix(claims.Subject, projectSubjectPrefix) {
		if s.projTokens == nil {
			s.logWarn("catalog 未启用项目令牌：按匿名主体处理", r,
				"token_subject", claims.Subject)
			return Principal{}
		}
		pt, err := s.projTokens.ProjectTokenByID(r.Context(), claims.TokenID)
		if err != nil {
			// 令牌查不到 = 已吊销或已过期 → 按匿名主体处理。
			// 级别 warn：这是运行期常态（令牌过期），但必须留痕。
			s.logWarn("catalog 项目令牌主体不存在：按匿名主体处理", r,
				"err", err, "token_subject", claims.Subject)
			return Principal{}
		}
		return Principal{Project: pt}
	}
	u, err := s.db.UserByUsername(ctx, claims.Subject)
	if err != nil {
		// ⚠️ **必须区分两类错误**：
		//   - 查不到（store.ErrNotFound）= 账号已被删除 → warn；
		//   - 其它 err = DB/连接故障 → error（系统故障，必须有人管）。
		// 两者在外部表现上完全一样（目录里只剩公开仓库），
		// 分不开就等于没记。
		if errors.Is(err, store.ErrNotFound) {
			s.logWarn("catalog 主体对应的账号不存在：按匿名主体处理", r,
				"user", claims.Subject)
		} else {
			s.logError("catalog 查主体账户失败：按匿名主体处理", r,
				"user", claims.Subject, "err", err)
		}
		return Principal{}
	}
	if u == nil {
		// 契约防御：store 不会返回 (nil, nil)。真出现说明查库有 bug，
		// 一样不能无声降级。
		s.logWarn("catalog 查主体账户返回空记录：按匿名主体处理", r,
			"user", claims.Subject)
		return Principal{}
	}
	if u.Disabled {
		// 账号被禁用：不是故障，是运营动作 → warn 即可。
		s.logWarn("catalog 主体账号已被禁用：按匿名主体处理", r,
			"user", claims.Subject)
		return Principal{}
	}
	return Principal{User: u}
}
