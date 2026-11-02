package server

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/web"
)

// 返回 JWKS 文档（**只有公钥**）。
//
// 远程 distribution 要验 lau6-cr 签的 JWT，就必须拿到这里的公钥。
// 两个要点：
//
//   - **公开端点**：公钥本来就是公开的，不在 requireSetupDone 后面——
//     distribution 可能在 setup 完成之前就要拉它。
//   - **不缓存**：密钥轮换后要尽快拿到新的，所以 Cache-Control: no-store。
func (a *App) handleJWKS(w http.ResponseWriter, r *http.Request) {
	if a.Keys == nil {
		Fail(w, http.StatusNotFound, CodeNotFound, "公钥尚未就绪")
		return
	}
	// ⚠️ 序列化失败必须回 5xx：空 body + 200 会让上游 distribution
	//    验签直接失败，而调用方看不出出了什么事（AGENTS.md 规则 1）。
	doc, err := a.Keys.JWKSBytes()
	if err != nil {
		a.Logger.Error("生成 JWKS 文档失败", "err", err)
		Fail(w, http.StatusInternalServerError, CodeInternal, "生成公钥文档失败")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(doc)
}

// 提供前端静态资源（SPA fallback）。
//
// 取用顺序是**磁盘优先、embed 兜底**：
//
//	CR_STATIC_DIR 指向的目录里有 index.html → 用磁盘那份
//	（本地开发、运维临时替换前端）
//	否则 → 用编译进二进制的副本（容器 / 生产环境）
func (a *App) handleStatic(w http.ResponseWriter, r *http.Request) {
	// API 前缀必须 404，不能回落到 index.html——
	// 否则前端会拿到 HTML 却按 JSON 解析，错误信息完全失真。
	if isAPIPath(r.URL.Path) {
		Fail(w, http.StatusNotFound, CodeNotFound, "not found")
		return
	}
	dir := a.Cfg.Server.StaticDir
	if web.HasDiskIndex(dir) {
		a.serveStaticDisk(w, r, dir)
		return
	}
	a.serveStaticEmbed(w, r)
}

// 从磁盘目录取前端资源（磁盘优先路径）。
func (a *App) serveStaticDisk(w http.ResponseWriter, r *http.Request, dir string) {
	clean := filepath.Clean("/" + strings.TrimPrefix(r.URL.Path, "/"))
	target := filepath.Join(dir, clean)

	info, err := os.Stat(target)
	if err != nil || info.IsDir() {
		// SPA fallback：未知的非资源路径交给前端路由。
		target = filepath.Join(dir, "index.html")
		if _, err := os.Stat(target); err != nil {
			Fail(w, http.StatusNotFound, CodeNotFound, "前端资源未部署")
			return
		}
		clean = "/index.html"
	}
	setStaticCache(w, clean)
	http.ServeFile(w, r, target)
}

// 从嵌入副本取前端资源（embed 兜底路径）。
func (a *App) serveStaticEmbed(w http.ResponseWriter, r *http.Request) {
	name, data, err := web.Resolve(r.URL.Path)
	if err != nil {
		Fail(w, http.StatusNotFound, CodeNotFound, "前端资源未部署")
		return
	}
	setStaticCache(w, "/"+name)
	// ServeContent 按扩展名给 Content-Type，并支持 Range。
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
}

// 设置缓存头：index.html 不缓存，带 hash 的资源长缓存。
func setStaticCache(w http.ResponseWriter, servedPath string) {
	if strings.HasSuffix(servedPath, "index.html") {
		// index.html 不缓存，保证发版后立刻生效。
		w.Header().Set("Cache-Control", "no-cache")
		return
	}
	// 带 hash 的资源可以长缓存。
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
}
