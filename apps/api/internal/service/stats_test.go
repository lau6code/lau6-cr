package service

import (
	"net/http"
	"testing"
)

// statsBuckets 取出聚合器当前的计数桶。
func statsBuckets(a *StatsAggregator, repo string) *counterBucket {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for k, b := range a.buckets {
		if k.repo == repo {
			return b
		}
	}
	return nil
}

// TestNoteFollowsCollectionTable 锁定采集点对照表
// （docs/design/10-traffic-stats.md §1.3）。
//
// 这张表是流量统计的**唯一口径**：
//
//   - HEAD 一律不计（守卫探测与客户端探活会虚高）。
//   - manifest：GET 计 pulls、PUT 计 pushes、DELETE 计 deletes。
//   - blob：GET 计 pulls。
//   - 上传会话：POST 只建会话不计；PUT 计一次推送；PATCH 不计次数。
//   - tags/list 与 _catalog 不计。
func TestNoteFollowsCollectionTable(t *testing.T) {
	cases := []struct {
		name   string
		rp     RegistryPath
		method string
		pulls  int64
		pushes int64
		del    int64
	}{
		{
			name:   "GET manifest 计一次拉取",
			rp:     RegistryPath{Repo: "team-a/app", Kind: "manifests"},
			method: http.MethodGet, pulls: 1,
		},
		{
			name:   "PUT manifest 计一次推送",
			rp:     RegistryPath{Repo: "team-a/app", Kind: "manifests"},
			method: http.MethodPut, pushes: 1,
		},
		{
			name:   "DELETE manifest 计一次删除",
			rp:     RegistryPath{Repo: "team-a/app", Kind: "manifests"},
			method: http.MethodDelete, del: 1,
		},
		{
			name:   "HEAD manifest 不计",
			rp:     RegistryPath{Repo: "team-a/app", Kind: "manifests"},
			method: http.MethodHead,
		},
		{
			name:   "GET blob 计一次拉取",
			rp:     RegistryPath{Repo: "team-a/app", Kind: "blob"},
			method: http.MethodGet, pulls: 1,
		},
		{
			name:   "POST 上传会话不计",
			rp:     RegistryPath{Repo: "team-a/app", Kind: "upload"},
			method: http.MethodPost,
		},
		{
			name:   "PATCH 分块不计次数",
			rp:     RegistryPath{Repo: "team-a/app", Kind: "upload"},
			method: http.MethodPatch,
		},
		{
			name:   "PUT 上传分块计一次推送",
			rp:     RegistryPath{Repo: "team-a/app", Kind: "upload"},
			method: http.MethodPut, pushes: 1,
		},
		{
			name:   "tags 列表不计",
			rp:     RegistryPath{Repo: "team-a/app", Kind: "tags"},
			method: http.MethodGet,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := NewStatsAggregator(StatsOptions{Enabled: true})
			defer a.Close()

			a.Note(tc.rp, tc.method)
			b := statsBuckets(a, "team-a/app")
			if b == nil {
				if tc.pulls == 0 && tc.pushes == 0 && tc.del == 0 {
					return // 本来就不该建桶
				}
				t.Fatal("计数桶没建起来")
			}
			if b.pulls.Load() != tc.pulls {
				t.Fatalf("pulls = %d，期望 %d", b.pulls.Load(), tc.pulls)
			}
			if b.pushes.Load() != tc.pushes {
				t.Fatalf("pushes = %d，期望 %d", b.pushes.Load(), tc.pushes)
			}
			if b.deletes.Load() != tc.del {
				t.Fatalf("deletes = %d，期望 %d", b.deletes.Load(), tc.del)
			}
		})
	}
}

// TestNoteIgnoresEmptyRepo 覆盖"没有仓库上下文就不计数"。
func TestNoteIgnoresEmptyRepo(t *testing.T) {
	a := NewStatsAggregator(StatsOptions{Enabled: true})
	defer a.Close()

	a.Note(RegistryPath{Kind: "catalog"}, http.MethodGet)
	if len(a.buckets) != 0 {
		t.Fatalf("建了 %d 个桶，期望 0（catalog 不该计数）", len(a.buckets))
	}
}

// TestAddBytesLandsInBucket 覆盖字节累加真的落进桶里。
//
// ⚠️ 这条锁的是"计数器与桶之间那一环"：
//
//	每次请求都新建一个计数器、抄完字节就丢掉，
//	桶里永远是 0——页面上传输量恒为 0，且没有任何报错。
func TestAddBytesLandsInBucket(t *testing.T) {
	a := NewStatsAggregator(StatsOptions{Enabled: true})
	defer a.Close()

	a.AddOut("team-a/app", 1024)
	a.AddIn("team-a/app", 2048)
	// 0 与负数不算。
	a.AddOut("team-a/app", 0)
	a.AddIn("team-a/app", -5)

	b := statsBuckets(a, "team-a/app")
	if b == nil {
		t.Fatal("计数桶没建起来")
	}
	if b.bytesOut.Load() != 1024 {
		t.Fatalf("bytesOut = %d，期望 1024", b.bytesOut.Load())
	}
	if b.bytesIn.Load() != 2048 {
		t.Fatalf("bytesIn = %d，期望 2048", b.bytesIn.Load())
	}
}
