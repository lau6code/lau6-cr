// Package web 承载前端静态资源的嵌入副本。
//
// 前端构建产物（apps/web/dist）在构建期被拷进 internal/web/dist，
// 再由 go:embed 固化进 Go 二进制 —— 最终交付物是**单一二进制**。
//
// 运行时的取用顺序是**磁盘优先、embed 兜底**：
//
//	CR_STATIC_DIR 指向的目录里有 index.html → 用磁盘那份
//	（本地开发、运维临时替换前端）
//	否则 → 用编译进二进制的副本（容器 / 生产环境）
//
// ⚠️ go:embed 只能嵌入**当前包目录内**的文件，所以嵌入目录是
//
//	internal/web/dist，而不是 apps/web/dist。
package web

import (
	"embed"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// embeddedFS 是编译期内嵌的前端产物。
//
// all: 前缀让 .gitkeep 这类点开头的文件也进包；嵌入目录必须存在且非空，
// 所以 dist/ 里永远留有占位文件（见 dist/placeholder/index.html）。
//
//go:embed all:dist
var embeddedFS embed.FS

// distFS 是以 dist/ 为根的嵌入文件系统。
var distFS = mustSub(embeddedFS, "dist")

// placeholderIndex 是占位 SPA 入口：干净 checkout（没跑过前端构建）时用。
const placeholderIndex = "placeholder/index.html"

// mustSub 把嵌入文件系统裁到指定子目录。
//
// 子目录不存在说明打包出了问题（嵌入目录被删了），此时直接 panic ——
// 总比运行时才发现前端全是 404 好。
func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic("web: 嵌入目录不可用: " + err.Error())
	}
	return sub
}

// ReadFile 读嵌入资源；name 形如 "index.html"、"assets/app-abc123.js"。
func ReadFile(name string) ([]byte, error) {
	return fs.ReadFile(distFS, clean(name))
}

// Index 返回 SPA 入口文件：优先真实构建产物的 index.html，
// 没有则回落到占位 placeholder/index.html。
//
// 返回的 name 用于决定 Content-Type 与缓存策略。
func Index() (string, []byte, error) {
	if data, err := ReadFile("index.html"); err == nil {
		return "index.html", data, nil
	}
	data, err := ReadFile(placeholderIndex)
	if err != nil {
		return "", nil, err
	}
	return placeholderIndex, data, nil
}

// Resolve 按 URL 路径取前端资源；取不到就回落到 SPA 入口（index.html）。
//
// 返回的 name 是**实际服务的文件名**——决定了 Content-Type 与缓存头。
func Resolve(urlPath string) (string, []byte, error) {
	name := clean(urlPath)
	if name == "" {
		name = "index.html"
	}
	if info, err := fs.Stat(distFS, name); err == nil && !info.IsDir() {
		if data, err := fs.ReadFile(distFS, name); err == nil {
			return name, data, nil
		}
	}
	// SPA fallback：未知的非资源路径交给前端路由。
	idxName, idxData, err := Index()
	if err != nil {
		return "", nil, err
	}
	return idxName, idxData, nil
}

// HasDiskIndex 判断磁盘目录能不能当前端根目录用。
//
// 判据是**目录里有 index.html**——只有 index.html 的目录才算可用的前端。
// 目录不存在或没有 index.html，一律回落到嵌入副本。
func HasDiskIndex(dir string) bool {
	if dir == "" {
		return false
	}
	info, err := os.Stat(filepath.Join(dir, "index.html"))
	return err == nil && !info.IsDir()
}

// clean 把 URL 路径规整成嵌入文件系统里的相对路径，
// 同时挡掉 ../ 越界。
func clean(name string) string {
	p := path.Clean("/" + strings.TrimPrefix(name, "/"))
	return strings.TrimPrefix(p, "/")
}
