package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveRootServesSPA 断言根路径能拿到 SPA 外壳。
//
// 干净 checkout（没跑过前端构建）时拿到的是占位页；
// 跑过前端构建时拿到的是真实 index.html —— 两者都必须含 SPA 挂载点。
func TestResolveRootServesSPA(t *testing.T) {
	name, data, err := Resolve("/")
	if err != nil {
		t.Fatalf("Resolve(/) 失败: %v", err)
	}
	if !strings.HasSuffix(name, "index.html") {
		t.Errorf("期望以 index.html 结尾，实际 %q", name)
	}
	if !strings.Contains(string(data), `<div id="app"></div>`) {
		t.Errorf("返回的不是 SPA 外壳，缺少 <div id=\"app\"></div>：\n%s", data)
	}
}

// TestResolveUnknownPathFallsBackToIndex 断言未知路径回落到 index.html。
func TestResolveUnknownPathFallsBackToIndex(t *testing.T) {
	name, data, err := Resolve("/some/vue/route/that/does/not/exist")
	if err != nil {
		t.Fatalf("Resolve 失败: %v", err)
	}
	if !strings.HasSuffix(name, "index.html") {
		t.Errorf("期望回落到 index.html，实际 %q", name)
	}
	if !strings.Contains(string(data), `<div id="app"></div>`) {
		t.Errorf("回落结果不是 SPA 外壳：\n%s", data)
	}
}

// TestResolveRejectsTraversal 断言 ../ 越界被挡住。
func TestResolveRejectsTraversal(t *testing.T) {
	name, _, err := Resolve("/../../../../etc/passwd")
	if err != nil {
		t.Fatalf("Resolve 失败: %v", err)
	}
	if !strings.HasSuffix(name, "index.html") {
		t.Errorf("越界路径应回落到 index.html，实际 %q", name)
	}
}

// TestHasDiskIndex 断言磁盘目录的可用性判据是「有 index.html」。
func TestHasDiskIndex(t *testing.T) {
	dir := t.TempDir()

	if HasDiskIndex(dir) {
		t.Error("空目录不应被判为可用前端目录")
	}
	if HasDiskIndex("") {
		t.Error("空路径不应被判为可用前端目录")
	}
	if HasDiskIndex(filepath.Join(dir, "nope")) {
		t.Error("不存在的目录不应被判为可用前端目录")
	}

	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html></html>"), 0o600); err != nil {
		t.Fatalf("写 index.html 失败: %v", err)
	}
	if !HasDiskIndex(dir) {
		t.Error("含 index.html 的目录应被判为可用前端目录")
	}
}
