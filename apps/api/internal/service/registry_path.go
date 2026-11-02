package service

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

const registryPrefix = "/v2/"

// registryServiceName 是 token 的 service 名。
//
// 它出现在 WWW-Authenticate 与 /token 请求里，作用是告诉客户端
// "这枚 token 是给哪个 registry 用的"。客户端会把它原样回传，
// 服务端据此确认请求来自同一个 registry。
const registryServiceName = "lau6-cr"

// 用摘要的固定形态（sha256:64 位十六进制）锚定结尾，
// 避免仓库名里含 "blobs" 时解析错位。
var reBlobDigest = regexp.MustCompile(`/blobs/(sha256:[0-9a-fA-F]{64})$`)

// RegistryPath 是一次 registry 请求的解析结果。
type RegistryPath struct {
	// Repo 是完整仓库名（<project>/<repo>）；_catalog 等为空。
	Repo string
	// Kind 是 manifest / blob / upload / tags / catalog / other。
	Kind string
	// Reference 是 manifest 的 reference（tag 或 digest）。
	Reference string
}

// ParseRegistryPath 从请求路径解析出仓库名。
//
// 仓库名本身可以含 "/"，所以靠**后缀形态**消歧：
//   - manifest：取最后一个 "/manifests/"
//   - blob：摘要形态唯一，锚定结尾
//   - upload：取最后一个 "/blobs/uploads/"
func ParseRegistryPath(p string) (RegistryPath, bool) {
	if !strings.HasPrefix(p, registryPrefix) {
		return RegistryPath{}, false
	}
	rest := p[len(registryPrefix):]
	if rest == "_catalog" {
		return RegistryPath{Kind: "catalog"}, true
	}
	if i := strings.LastIndex(rest, "/manifests/"); i > 0 {
		return RegistryPath{Repo: rest[:i], Kind: "manifests", Reference: rest[i+len("/manifests/"):]}, true
	}
	if m := reBlobDigest.FindStringSubmatch(rest); m != nil {
		idx := strings.LastIndex(rest, "/blobs/")
		return RegistryPath{Repo: rest[:idx], Kind: "blob", Reference: m[1]}, true
	}
	if i := strings.LastIndex(rest, "/blobs/uploads/"); i > 0 {
		return RegistryPath{Repo: rest[:i], Kind: "upload", Reference: rest[i+len("/blobs/uploads/"):]}, true
	}
	if i := strings.LastIndex(rest, "/tags/list"); i > 0 {
		return RegistryPath{Repo: rest[:i], Kind: "tags"}, true
	}
	if i := strings.LastIndex(rest, "/blobs/"); i > 0 {
		return RegistryPath{Repo: rest[:i], Kind: "blob", Reference: rest[i+len("/blobs/"):]}, true
	}
	return RegistryPath{Kind: "other"}, true
}

// 路径末尾的斜杠可有可无（两个客户端都这么发）。
func isVersionCheck(p string) bool {
	return p == registryPrefix || p == strings.TrimSuffix(registryPrefix, "/")
}

// requiredAction 返回该请求在仓库上需要的动作。
//
//	POST/PATCH/PUT /blobs/uploads/…  → push（写 blob）
//	PUT/POST/PATCH /manifests/…       → push（写 tag）
//	DELETE …                          → delete
//	GET/HEAD …                        → pull（读）
//
// 写操作必须要求 push：blob 上传不涉及 tag 覆盖策略，
// 但它是在往仓库里写数据，用 pull 放行等于把写权限白送。
func requiredAction(rp RegistryPath, method string) Action {
	switch rp.Kind {
	case "upload":
		return ActionPush
	case "manifests", "blob":
		switch method {
		case http.MethodPut, http.MethodPost, http.MethodPatch:
			return ActionPush
		case http.MethodDelete:
			return ActionDelete
		default:
			return ActionPull
		}
	default:
		return ActionPull
	}
}

// challengeScope 返回该操作在 WWW-Authenticate 里声明的 scope。
//
// 写路径要声明 pull,push：部分客户端以挑战里的 scope 为准去申请 token，
// 只声明 pull 会让它们拿到只读 token，直到最后一步 manifest PUT 才 403——
// 报错点离原因很远。
func challengeScope(repo string, action Action) string {
	switch action {
	case ActionPush:
		return fmt.Sprintf("repository:%s:pull,push", repo)
	case ActionDelete:
		return fmt.Sprintf("repository:%s:pull,delete", repo)
	default:
		return fmt.Sprintf("repository:%s:pull", repo)
	}
}

var reDigest = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*:[0-9a-fA-F]{32,}$`)

func isDigest(s string) bool { return reDigest.MatchString(s) }
