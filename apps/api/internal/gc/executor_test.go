package gc

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lau6code/lau6-cr/apps/api/internal/distribution"
)

// fakeDistributionEnv 让测试二进制**假扮** distribution。
//
// 用"重新执行自己"这套手法（而不是写 shell 脚本）是为了跨平台：
// 脚本在 Windows 上跑不了，而这里只需要一个能接收参数、
// 打印上游格式输出的可执行文件。
const fakeDistributionEnv = "GC_FAKE_DISTRIBUTION"

// TestMain 在特定环境变量下把本测试二进制变成"假 distribution"。
func TestMain(m *testing.M) {
	if os.Getenv(fakeDistributionEnv) != "" {
		fakeDistributionMain()
		return
	}
	os.Exit(m.Run())
}

// fakeDistributionMain 模拟上游 `distribution garbage-collect`。
//
// 行为按测试用环境变量驱动：
//
//   - GC_FAKE_ARGS_FILE：把收到的参数写进去（验证命令行拼装）；
//   - GC_FAKE_BLOB_PATH / GC_FAKE_BLOB_DIGEST：正式回收时删掉这个 blob；
//   - GC_FAKE_FAIL：非空则以非 0 退出（模拟上游失败）。
func fakeDistributionMain() {
	if argsFile := os.Getenv("GC_FAKE_ARGS_FILE"); argsFile != "" {
		_ = os.WriteFile(argsFile, []byte(strings.Join(os.Args[1:], " ")), 0o600)
	}
	if os.Getenv("GC_FAKE_FAIL") != "" {
		fmt.Fprintln(os.Stderr, "failed to garbage collect: fake failure")
		os.Exit(1)
	}

	dryRun := false
	for _, a := range os.Args[1:] {
		if a == "--dry-run" {
			dryRun = true
		}
	}

	blobPath := os.Getenv("GC_FAKE_BLOB_PATH")
	blobDigest := os.Getenv("GC_FAKE_BLOB_DIGEST")
	if !dryRun && blobPath != "" {
		if err := os.Remove(blobPath); err != nil {
			fmt.Fprintf(os.Stderr, "fake delete failed: %v\n", err)
			os.Exit(1)
		}
	}

	// 上游的输出格式（registry/storage/garbagecollect.go）：
	// 标记阶段每个仓库一行仓库名，随后是统计行与待删清单。
	fmt.Println("test/postgres")
	fmt.Println()
	if os.Getenv("GC_FAKE_NO_SUMMARY") == "" {
		fmt.Println("1 blobs marked, 1 blobs and 0 manifests eligible for deletion")
	}
	if blobDigest != "" {
		fmt.Printf("blob eligible for deletion: %s\n", blobDigest)
	}
}

// fixture 是一个"本地存储 + distribution 配置"的测试夹具。
type fixture struct {
	root       string
	configPath string
	// blobPath / blobDigest 指向一个用来验证删除的 blob。
	blobPath   string
	blobDigest string
	// otherBlobPath 是第二个 blob（不被删），用来验证"扫到几个"。
	otherBlobPath string
}

// newFixture 造一个夹具：两个 blob + 一份指向它的 distribution 配置。
func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()

	f := &fixture{root: root}

	h1 := strings.Repeat("a", 64)
	h2 := strings.Repeat("b", 64)
	f.blobDigest = "sha256:" + h1
	f.blobPath = filepath.Join(root, filepath.FromSlash(blobPrefix), "sha256", h1[:2], h1, "data")
	f.otherBlobPath = filepath.Join(root, filepath.FromSlash(blobPrefix), "sha256", h2[:2], h2, "data")
	for _, p := range []string{f.blobPath, f.otherBlobPath} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("建 blob 目录失败: %v", err)
		}
		if err := os.WriteFile(p, []byte("0123456789"), 0o644); err != nil {
			t.Fatalf("写 blob 失败: %v", err)
		}
	}

	// 一个 manifest 修订链接（统计 manifest 数用）。
	revDir := filepath.Join(root, filepath.FromSlash(repositoriesPrefix), "test", "postgres",
		"_manifests", "revisions", "sha256", strings.Repeat("c", 64))
	if err := os.MkdirAll(revDir, 0o755); err != nil {
		t.Fatalf("建修订目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(revDir, "link"), []byte("sha256:"+strings.Repeat("c", 64)), 0o644); err != nil {
		t.Fatalf("写修订链接失败: %v", err)
	}

	f.configPath = filepath.Join(root, "distribution-config.yml")
	body := fmt.Sprintf("version: 0.1\nstorage:\n  filesystem:\n    rootdirectory: %s\n  delete:\n    enabled: true\n", root)
	if err := os.WriteFile(f.configPath, []byte(body), 0o600); err != nil {
		t.Fatalf("写 distribution 配置失败: %v", err)
	}
	return f
}

// fakeExecutor 造一个指向"假 distribution"的执行器。
func fakeExecutor(t *testing.T, f *fixture) (*Executor, string) {
	t.Helper()
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	t.Setenv(fakeDistributionEnv, "1")
	t.Setenv("GC_FAKE_ARGS_FILE", argsFile)
	t.Setenv("GC_FAKE_BLOB_PATH", f.blobPath)
	t.Setenv("GC_FAKE_BLOB_DIGEST", f.blobDigest)
	return NewExecutor(os.Args[0], f.configPath, nil), argsFile
}

// TestExecutorPrecheck 覆盖"动手前就能发现的问题"。
func TestExecutorPrecheck(t *testing.T) {
	f := newFixture(t)

	if err := NewExecutor("", f.configPath, nil).Precheck(); err == nil {
		t.Error("缺二进制路径时期望报错")
	}
	if err := NewExecutor(os.Args[0], "", nil).Precheck(); err == nil {
		t.Error("缺配置路径时期望报错")
	}
	if err := NewExecutor("/nonexistent/distribution", f.configPath, nil).Precheck(); err == nil {
		t.Error("二进制不存在时期望报错")
	}
	if err := NewExecutor(os.Args[0], "/nonexistent/config.yml", nil).Precheck(); err == nil {
		t.Error("配置不存在时期望报错")
	}
	if err := NewExecutor(os.Args[0], f.configPath, nil).Precheck(); err != nil {
		t.Errorf("条件齐备时期望通过，实际 %v", err)
	}
}

// TestExecutorCollect 覆盖一次完整回收：调度上游、解析统计、算释放字节。
func TestExecutorCollect(t *testing.T) {
	f := newFixture(t)
	ex, argsFile := fakeExecutor(t, f)

	report, err := ex.Collect(context.Background(), CollectOptions{})
	if err != nil {
		t.Fatalf("回收失败: %v", err)
	}

	// 命令行拼装：garbage-collect <配置>，正式回收不带 --dry-run。
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("读假 distribution 的参数失败: %v", err)
	}
	got := strings.TrimSpace(string(args))
	if !strings.HasPrefix(got, "garbage-collect ") || !strings.Contains(got, f.configPath) {
		t.Errorf("命令行期望含 garbage-collect 与配置路径，实际 %q", got)
	}
	if strings.Contains(got, "--dry-run") {
		t.Errorf("正式回收不该带 --dry-run，实际 %q", got)
	}

	if report.Repositories != 1 {
		t.Errorf("仓库数期望 1，实际 %d", report.Repositories)
	}
	if report.BlobsMarked != 1 {
		t.Errorf("标记 blob 数期望 1，实际 %d", report.BlobsMarked)
	}
	if report.BlobsScanned != 2 {
		t.Errorf("扫到 blob 数期望 2，实际 %d", report.BlobsScanned)
	}
	if report.BlobsDeleted != 1 {
		t.Errorf("删除 blob 数期望 1，实际 %d", report.BlobsDeleted)
	}
	if report.ManifestsFound != 1 {
		t.Errorf("manifest 修订数期望 1，实际 %d", report.ManifestsFound)
	}
	if report.BytesReclaimed != 10 {
		t.Errorf("释放字节期望 10，实际 %d", report.BytesReclaimed)
	}
	// 上游真的把那个 blob 删掉了。
	if _, err := os.Stat(f.blobPath); !os.IsNotExist(err) {
		t.Errorf("blob 应该已被上游删除，stat 错误 = %v", err)
	}
}

// TestExecutorCollectDryRun 覆盖试运行：什么都不删，但给出"预计释放"。
func TestExecutorCollectDryRun(t *testing.T) {
	f := newFixture(t)
	ex, argsFile := fakeExecutor(t, f)

	report, err := ex.Collect(context.Background(), CollectOptions{DryRun: true})
	if err != nil {
		t.Fatalf("试运行失败: %v", err)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("读参数失败: %v", err)
	}
	if !strings.Contains(string(args), "--dry-run") {
		t.Errorf("试运行该带 --dry-run，实际 %q", string(args))
	}
	if report.BlobsDeleted != 1 {
		t.Errorf("可回收 blob 数期望 1，实际 %d", report.BlobsDeleted)
	}
	if report.BytesReclaimed != 10 {
		t.Errorf("预计释放字节期望 10，实际 %d", report.BytesReclaimed)
	}
	// 试运行不许删东西。
	if _, err := os.Stat(f.blobPath); err != nil {
		t.Errorf("试运行不该删 blob，stat 错误 = %v", err)
	}
}

// TestExecutorCollectUpstreamFailure 覆盖上游失败：必须报错，不装作成功。
func TestExecutorCollectUpstreamFailure(t *testing.T) {
	f := newFixture(t)
	ex, _ := fakeExecutor(t, f)
	t.Setenv("GC_FAKE_FAIL", "1")

	if _, err := ex.Collect(context.Background(), CollectOptions{}); err == nil {
		t.Fatal("上游失败时期望报错")
	}
}

// TestExecutorCollectUnparsableOutput 覆盖"上游输出变了"的情况。
//
// 统计解析不出来时必须报错——
// 报成"回收了 0 个 blob"就是把"统计不出来"说成"没有垃圾"。
func TestExecutorCollectUnparsableOutput(t *testing.T) {
	f := newFixture(t)
	ex, _ := fakeExecutor(t, f)
	t.Setenv("GC_FAKE_ARGS_FILE", "")
	// 让假 distribution 不打印统计行：直接用空输出模式。
	t.Setenv("GC_FAKE_NO_SUMMARY", "1")

	if _, err := ex.Collect(context.Background(), CollectOptions{}); err == nil {
		t.Fatal("统计行缺失时期望报错")
	}
}

// TestParseGCOutput 覆盖上游输出的解析。
func TestParseGCOutput(t *testing.T) {
	out := `test/postgres
team-a/app

3 blobs marked, 2 blobs and 1 manifests eligible for deletion
blob eligible for deletion: sha256:aaa
blob eligible for deletion: sha256:bbb
test/postgres: layer link eligible for deletion: sha256:ccc
manifest eligible for deletion: {Name:test/postgres Digest:sha256:ddd Tags:[]}
`
	st, err := parseGCOutput(out)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if st.Repositories != 2 {
		t.Errorf("仓库数期望 2，实际 %d", st.Repositories)
	}
	if st.BlobsMarked != 3 {
		t.Errorf("标记数期望 3，实际 %d", st.BlobsMarked)
	}
	if len(st.DeletedBlobs) != 2 {
		t.Errorf("待删 blob 期望 2 个，实际 %d", len(st.DeletedBlobs))
	}
	if st.DeletedBlobs[0] != "sha256:aaa" {
		t.Errorf("待删 blob 期望 sha256:aaa，实际 %q", st.DeletedBlobs[0])
	}
	if st.LayerLinks != 1 {
		t.Errorf("层链接期望 1，实际 %d", st.LayerLinks)
	}
	if st.Untagged != 1 {
		t.Errorf("无 tag manifest 期望 1，实际 %d", st.Untagged)
	}
}

// TestParseGCOutputRequiresSummary 覆盖"统计行必须存在"。
func TestParseGCOutputRequiresSummary(t *testing.T) {
	if _, err := parseGCOutput("test/postgres\n"); err == nil {
		t.Fatal("缺统计行时期望报错")
	}
}

// TestDigestFromBlobPath 覆盖"从 blob 路径反推摘要"。
func TestDigestFromBlobPath(t *testing.T) {
	hex := strings.Repeat("a", 64)
	cases := []struct {
		path string
		want string
		ok   bool
	}{
		{"/data/registry/docker/registry/v2/blobs/sha256/aa/" + hex + "/data", "sha256:" + hex, true},
		{"docker/registry/v2/blobs/sha256/aa/" + hex + "/data", "sha256:" + hex, true},
		{"/data/registry/docker/registry/v2/blobs/sha256/aa/" + hex + "/link", "", false},
		{"/data/registry/docker/registry/v2/blobs/sha256/aa/data", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := digestFromBlobPath(c.path)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("digestFromBlobPath(%q) = %q,%v；期望 %q,%v", c.path, got, ok, c.want, c.ok)
		}
	}
}

// TestS3Prefix 覆盖"桶内对象前缀"的拼法。
//
// ⚠️ 桶内对象键 = rootdirectory + distribution 的内部路径。
// rootdirectory 留空（桶根）时是 docker/registry/v2/...；
// 填了别的值就是 <rootdirectory>/docker/registry/v2/...。
//
// 这条拼错的后果是"列举不到任何 blob"——
// 统计变成 0，而回收本身（上游按自己的配置走）仍然是对的：
// 两边对不上，数字就会骗人。
func TestS3Prefix(t *testing.T) {
	cases := []struct {
		root string
		want string
	}{
		{"", "docker/registry/v2/blobs/"},
		{"/", "docker/registry/v2/blobs/"},
		{"/docker/registry/v2", "docker/registry/v2/docker/registry/v2/blobs/"},
		{"prefix", "prefix/docker/registry/v2/blobs/"},
	}
	for _, c := range cases {
		got := s3Prefix(&distribution.S3Storage{RootPath: c.root}, blobPrefix)
		if got != c.want {
			t.Errorf("s3Prefix(root=%q) = %q，期望 %q", c.root, got, c.want)
		}
	}
}

// TestS3KeyDigest 覆盖"从桶内对象键反推摘要"。
func TestS3KeyDigest(t *testing.T) {
	hex := strings.Repeat("d", 64)
	key := "docker/registry/v2/blobs/sha256/dd/" + hex + "/data"
	got, ok := digestFromBlobPath(key)
	if !ok || got != "sha256:"+hex {
		t.Errorf("digestFromBlobPath(%q) = %q,%v；期望 sha256:%s,true", key, got, ok, hex)
	}
	// 上传残留（uploads/ 前缀）不是 blob，不能算进统计。
	if _, ok := digestFromBlobPath("docker/registry/v2/blobs/uploads/sha256/" + hex); ok {
		t.Error("uploads 前缀不该被当成 blob")
	}
}
