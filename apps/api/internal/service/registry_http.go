package service

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
)

// RegistryError 是 Docker registry 协议的错误信封。
type RegistryError struct {
	Errors []RegistryErrorItem `json:"errors"`
}

// RegistryErrorItem 是单条错误。
type RegistryErrorItem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Detail  any    `json:"detail,omitempty"`
}

func (s *RegistryService) writeRegistryError(w http.ResponseWriter, status int, code, msg string, detail any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	w.WriteHeader(status)
	// ⚠️ 状态码已经写出去了，**改不了**——客户端拿到的是正确的状态码 +
	//    **截断的错误信封**（JSON 解析失败，只剩半个错误对象）。
	//    丢掉这个 error 就是"无声的失败"：调用方只看到一个解析不了的响应体，
	//    而日志里什么都没有。
	//
	// 这个 helper 没有 *http.Request，拿不到 method/path——
	// 靠 status + code + err 定位（code 就是错误分类，够用）。
	if err := json.NewEncoder(w).Encode(RegistryError{Errors: []RegistryErrorItem{{
		Code: code, Message: msg, Detail: detail,
	}}}); err != nil {
		if s.logger != nil {
			s.logger.Warn("registry 错误响应体写出失败：客户端收到的是截断的错误信封",
				"status", status, "code", code, "err", err)
		}
	}
}

// hopByHop 是逐跳头，代理时必须剥掉。
var hopByHop = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Te", "Trailer",
	"Transfer-Encoding", "Upgrade", "Proxy-Authenticate", "Proxy-Authorization",
}

func copyRequestHeaders(dst, src http.Header) {
	copyHeaders(dst, src)
}

func copyResponseHeaders(dst, src http.Header) {
	copyHeaders(dst, src)
}

// copyHeaders 把 src 的头复制到 dst，逐跳头跳过。
func copyHeaders(dst, src http.Header) {
	for k, vv := range src {
		if isHopByHop(k) {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func isHopByHop(k string) bool {
	for _, h := range hopByHop {
		if strings.EqualFold(k, h) {
			return true
		}
	}
	return false
}

// keyLocker 是按 (repo, tag) 粒度的互斥锁。
//
// 多 tag 并发写入时按字典序取锁，避免互相等待。
type keyLocker struct {
	mu    sync.Mutex
	locks map[string]*lockEntry
}

type lockEntry struct {
	mu   sync.Mutex
	refs int
}

func newKeyLocker() *keyLocker {
	return &keyLocker{locks: map[string]*lockEntry{}}
}

// lock 依次获取给定键的锁（调用方必须调用返回的 unlock）。
func (k *keyLocker) lock(keys []string) func() {
	k.mu.Lock()
	entries := make([]*lockEntry, 0, len(keys))
	for _, key := range keys {
		e, ok := k.locks[key]
		if !ok {
			e = &lockEntry{}
			k.locks[key] = e
		}
		e.refs++
		entries = append(entries, e)
	}
	k.mu.Unlock()

	for _, e := range entries {
		e.mu.Lock()
	}
	return func() {
		for i := len(entries) - 1; i >= 0; i-- {
			entries[i].mu.Unlock()
		}
		k.mu.Lock()
		for _, e := range entries {
			e.refs--
			if e.refs <= 0 {
				for key, ent := range k.locks {
					if ent == e {
						delete(k.locks, key)
						break
					}
				}
			}
		}
		k.mu.Unlock()
	}
}
