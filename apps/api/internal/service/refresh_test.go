package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lau6code/lau6-cr/apps/api/internal/domain"
)

// registryStub 造一个按路径返回固定 JSON 的假上游。
func registryStub(t *testing.T, routes map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := routes[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"errors":[{"code":"NOT_FOUND"}]}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// imageConfig 是镜像 config blob 的样子（created 与平台在这里）。
func imageConfig(created, os, arch string) string {
	b, _ := json.Marshal(map[string]any{
		"created":      created,
		"os":           os,
		"architecture": arch,
	})
	return string(b)
}

// TestMeasureSingleManifestFetchesConfigBlob 覆盖单架构镜像的度量。
//
// ⚠️ manifest 里的 config 只是**描述符**（size/digest），
// created 与平台在**镜像 config blob** 里——
// 不拉 blob 的话 created 与平台**永远是空的**。
func TestMeasureSingleManifestFetchesConfigBlob(t *testing.T) {
	digest := "sha256:" + strings.Repeat("c", 64)
	manifest := `{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json",
		"config":{"mediaType":"application/vnd.docker.container.image.v1+json","size":100,"digest":"` + digest + `"},
		"layers":[{"mediaType":"application/vnd.docker.image.rootfs.diff.tar.gzip","size":200}]}`

	up := registryStub(t, map[string]string{
		"/v2/team-a/app/manifests/v1":    manifest,
		"/v2/team-a/app/blobs/" + digest: imageConfig("2026-10-27T10:00:00Z", "linux", "amd64"),
	})
	r := NewRefresher(nil, up.Client(), up.URL, newTestIssuer(t), nil, &fakeLogger{})

	m, err := r.measureManifest(context.Background(), "team-a/app", map[string]any{
		"schemaVersion": 2,
		"config":        map[string]any{"size": float64(100), "digest": digest},
		"layers":        []any{map[string]any{"size": float64(200)}},
	})
	if err != nil {
		t.Fatalf("度量失败: %v", err)
	}
	if m.size != 300 {
		t.Fatalf("size = %d，期望 300（config 100 + layer 200）", m.size)
	}
	if m.created != "2026-10-27T10:00:00Z" {
		t.Fatalf("created = %q，期望来自镜像 config blob", m.created)
	}
	if len(m.platforms) != 1 || m.platforms[0] != "linux/amd64" {
		t.Fatalf("platforms = %v，期望 [linux/amd64]", m.platforms)
	}
}

// TestMeasureManifestListAggregates 覆盖多架构镜像（manifest list）。
//
// size 按子 manifest 累加、created 取最大、平台取并集。
func TestMeasureManifestListAggregates(t *testing.T) {
	d1 := "sha256:" + strings.Repeat("1", 64)
	d2 := "sha256:" + strings.Repeat("2", 64)
	list := `{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.list.v2+json",
		"manifests":[{"digest":"` + d1 + `","platform":{"os":"linux","architecture":"amd64"}},
		             {"digest":"` + d2 + `","platform":{"os":"linux","architecture":"arm64"}}]}`
	m1 := `{"schemaVersion":2,"config":{"size":10,"digest":"sha256:` + strings.Repeat("a", 64) + `"},"layers":[{"size":100}]}`
	m2 := `{"schemaVersion":2,"config":{"size":20,"digest":"sha256:` + strings.Repeat("b", 64) + `"},"layers":[{"size":200}]}`

	up := registryStub(t, map[string]string{
		"/v2/team-a/app/manifests/v1":                            list,
		"/v2/team-a/app/manifests/" + d1:                         m1,
		"/v2/team-a/app/manifests/" + d2:                         m2,
		"/v2/team-a/app/blobs/sha256:" + strings.Repeat("a", 64): imageConfig("2026-10-27T10:00:00Z", "linux", "amd64"),
		"/v2/team-a/app/blobs/sha256:" + strings.Repeat("b", 64): imageConfig("2026-10-28T10:00:00Z", "linux", "arm64"),
	})
	r := NewRefresher(nil, up.Client(), up.URL, newTestIssuer(t), nil, &fakeLogger{})

	raw, err := r.getManifest(context.Background(), "team-a/app", "v1")
	if err != nil {
		t.Fatalf("拉 manifest 失败: %v", err)
	}
	m, err := r.measureManifest(context.Background(), "team-a/app", raw)
	if err != nil {
		t.Fatalf("度量失败: %v", err)
	}
	if m.size != 330 {
		t.Fatalf("size = %d，期望 330（(10+100)+(20+200)）", m.size)
	}
	if m.created != "2026-10-28T10:00:00Z" {
		t.Fatalf("created = %q，期望取最大的那个", m.created)
	}
	if len(m.platforms) != 2 {
		t.Fatalf("platforms = %v，期望两个平台", m.platforms)
	}
}

// TestRefreshOneFailsWhenManifestUnavailable 覆盖"拉不到 manifest 就不写半成品"。
//
// 吞掉错误会写出一行 size=0 / created="" / platforms=[] 的缓存，
// 目录页显示"有镜像但大小为 0"，而且**没有任何日志**。
func TestRefreshOneFailsWhenManifestUnavailable(t *testing.T) {
	db := newTestDB(t)
	seedProjectOnly(t, db, "team-a")
	ctx := context.Background()
	project, _ := db.ProjectByName(ctx, "team-a")
	if _, err := db.CreateRepository(ctx, project.ID, "app", domain.VisibilityPrivate, ""); err != nil {
		t.Fatalf("建仓: %v", err)
	}

	// tags/list 正常，manifest 404。
	up := registryStub(t, map[string]string{
		"/v2/team-a/app/tags/list": `{"name":"team-a/app","tags":["v1"]}`,
	})
	logger := &fakeLogger{}
	r := NewRefresher(db, up.Client(), up.URL, newTestIssuer(t), NewRefreshQueue(), logger)

	if err := r.refreshOne(ctx, "team-a/app"); err == nil {
		t.Fatalf("manifest 拉不到时应当返回错误")
	}
	repo, _ := db.RepositoryByPath(ctx, project.ID, "app")
	meta, err := db.RepoMeta(ctx, repo.ID)
	if err != nil {
		t.Fatalf("读缓存行: %v", err)
	}
	if meta != nil {
		t.Fatalf("失败时不该写缓存行，实际写了 %+v", meta)
	}
}

// TestRefreshOneWritesMeta 覆盖完整成功路径。
func TestRefreshOneWritesMeta(t *testing.T) {
	db := newTestDB(t)
	seedProjectOnly(t, db, "team-a")
	ctx := context.Background()
	project, _ := db.ProjectByName(ctx, "team-a")
	repo, err := db.CreateRepository(ctx, project.ID, "app", domain.VisibilityPrivate, "")
	if err != nil {
		t.Fatalf("建仓: %v", err)
	}

	digest := "sha256:" + strings.Repeat("c", 64)
	manifest := `{"schemaVersion":2,"config":{"size":100,"digest":"` + digest + `"},
		"layers":[{"size":200}]}`
	up := registryStub(t, map[string]string{
		"/v2/team-a/app/tags/list":       `{"name":"team-a/app","tags":["v1","v2"]}`,
		"/v2/team-a/app/manifests/v2":    manifest,
		"/v2/team-a/app/blobs/" + digest: imageConfig("2026-10-27T10:00:00Z", "linux", "amd64"),
	})
	r := NewRefresher(db, up.Client(), up.URL, newTestIssuer(t), NewRefreshQueue(), &fakeLogger{})

	if err := r.refreshOne(ctx, "team-a/app"); err != nil {
		t.Fatalf("刷新失败: %v", err)
	}
	meta, err := db.RepoMeta(ctx, repo.ID)
	if err != nil || meta == nil {
		t.Fatalf("读缓存行: %v（meta=%v）", err, meta)
	}
	if meta.LatestTag != "v2" {
		t.Fatalf("latest_tag = %q，期望 v2", meta.LatestTag)
	}
	if meta.TagCount != 2 {
		t.Fatalf("tag_count = %d，期望 2", meta.TagCount)
	}
	if meta.Size != 300 {
		t.Fatalf("size = %d，期望 300", meta.Size)
	}
	if meta.Created != "2026-10-27T10:00:00Z" {
		t.Fatalf("created = %q", meta.Created)
	}
	if len(meta.Platforms) != 1 || !strings.Contains(meta.Platforms[0], "linux") {
		t.Fatalf("platforms = %v", meta.Platforms)
	}
}

// TestMeasureOCIIndexFetchesChildManifest 锁定「拉 manifest 必须带 Accept」。
//
// 上游 distribution 对 manifest 的 GET 会检查 Accept 头：缺省时它只认
// Docker v2 那几种媒体类型，拉 **OCI image manifest** 直接 404
// MANIFEST_UNKNOWN（"OCI manifest found, but accept header does not support
// OCI manifests"）。
//
// docker 现在默认推的正是 OCI index + OCI image manifest，
// 少了 Accept 头 → 多架构镜像量不出尺寸与平台 → 仓库缓存刷新失败。
func TestMeasureOCIIndexFetchesChildManifest(t *testing.T) {
	childDigest := "sha256:" + strings.Repeat("d", 64)
	configDigest := "sha256:" + strings.Repeat("e", 64)
	index := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json",
		"manifests":[{"digest":"` + childDigest + `","mediaType":"application/vnd.oci.image.manifest.v1+json",
		"platform":{"os":"linux","architecture":"amd64"}}]}`
	child := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json",
		"config":{"mediaType":"application/vnd.oci.image.config.v1+json","size":100,"digest":"` + configDigest + `"},
		"layers":[{"mediaType":"application/vnd.oci.image.layer.v1.tar+gzip","size":200}]}`

	// 复刻上游行为：Accept 里没有 OCI image manifest 就 404。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/team-a/app/manifests/v1":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(index))
		case "/v2/team-a/app/manifests/" + childDigest:
			if !strings.Contains(r.Header.Get("Accept"), "application/vnd.oci.image.manifest.v1+json") {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"errors":[{"code":"MANIFEST_UNKNOWN",` +
					`"message":"OCI manifest found, but accept header does not support OCI manifests"}]}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(child))
		case "/v2/team-a/app/blobs/" + configDigest:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(imageConfig("2026-10-29T10:00:00Z", "linux", "amd64")))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	r := NewRefresher(nil, srv.Client(), srv.URL, newTestIssuer(t), nil, &fakeLogger{})
	raw, err := r.getManifest(context.Background(), "team-a/app", "v1")
	if err != nil {
		t.Fatalf("拉 index 失败: %v", err)
	}
	m, err := r.measureManifest(context.Background(), "team-a/app", raw)
	if err != nil {
		t.Fatalf("度量 OCI index 失败: %v", err)
	}
	if m.size != 300 {
		t.Fatalf("size = %d，期望 300（config 100 + layer 200）", m.size)
	}
	if len(m.platforms) != 1 || m.platforms[0] != "linux/amd64" {
		t.Fatalf("platforms = %v，期望 [linux/amd64]", m.platforms)
	}
}
