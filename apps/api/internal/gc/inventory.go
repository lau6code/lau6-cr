package gc

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/lau6code/lau6-cr/apps/api/internal/distribution"
	"github.com/lau6code/lau6-cr/apps/api/internal/objectstore"
)

// distribution 存储布局里的两个前缀。
//
// ⚠️ 逐字对齐 docker/distribution v3 的 registry/storage/paths.go
// （storagePathRoot="/docker/registry/" + storagePathVersion="v2"）。
// 布局对不上就是"统计到 0 个 blob"——不报错，只是数字不对。
const (
	// blobPrefix 是 blob 数据对象的前缀。
	//
	// 上游布局：<prefix>/blobs/<算法>/<前两位>/<完整摘要>/data
	// （digestPathComponents(multilevel=true) 把摘要前两位拆成一层目录）。
	blobPrefix = "docker/registry/v2/blobs/"
	// repositoriesPrefix 是仓库元数据的前缀。
	//
	// manifest 修订链接在 <prefix>/repositories/<仓库>/_manifests/revisions/<算法>/<摘要>/link。
	repositoriesPrefix = "docker/registry/v2/repositories/"
)

// storage 是回收面对的后端（从 distribution 配置的 storage 段读出来的）。
//
// ⚠️ 它**必须**来自 registry 自己在用的那份配置：
// GC 按这个后端去删对象，配置不是同一份就会删到别的地方去。
type storage struct {
	fs *distribution.FilesystemStorage
	s3 *distribution.S3Storage
}

// storageFrom 从 distribution 配置里取出 storage 段。
func storageFrom(cfg *distribution.DistributionConfig) storage {
	return storage{fs: cfg.Storage.Filesystem, s3: cfg.Storage.S3}
}

// blobTotals 统计后端里所有 blob 的数量与总字节。
//
// 用的是**流式遍历**：桶里可能有上百万个 blob，
// 一次性攒进内存会把进程拖垮，所以只累加、不留存。
func blobTotals(ctx context.Context, st storage) (int, int64, error) {
	var count int
	var total int64
	err := forEachBlob(ctx, st, func(_ string, size int64) error {
		count++
		total += size
		return nil
	})
	return count, total, err
}

// blobSizesOf 量出指定摘要集合占用的字节。
//
// 试运行时用它算"预计能释放多少"——
// 正式回收跑完那些 blob 已经没了，只能靠回收前后的总量差。
func blobSizesOf(ctx context.Context, st storage, digests []string) (int64, error) {
	want := make(map[string]struct{}, len(digests))
	for _, d := range digests {
		want[d] = struct{}{}
	}
	var total int64
	err := forEachBlob(ctx, st, func(dgst string, size int64) error {
		if _, ok := want[dgst]; ok {
			total += size
		}
		return nil
	})
	return total, err
}

// manifestCount 数出仓库里的 manifest 修订数。
func manifestCount(ctx context.Context, st storage) (int, error) {
	switch {
	case st.fs != nil:
		return localManifestCount(st.fs.Rootdirectory)
	case st.s3 != nil:
		return s3ManifestCount(ctx, st.s3)
	default:
		return 0, fmt.Errorf("distribution 配置里没有可识别的 storage 段（filesystem / s3）")
	}
}

// forEachBlob 遍历后端里所有 blob 数据对象。
//
// 回调收到 blob 的摘要（"sha256:…"）与字节数。
func forEachBlob(ctx context.Context, st storage, fn func(digest string, size int64) error) error {
	switch {
	case st.fs != nil:
		return localForEachBlob(st.fs.Rootdirectory, fn)
	case st.s3 != nil:
		return s3ForEachBlob(ctx, st.s3, fn)
	default:
		return fmt.Errorf("distribution 配置里没有可识别的 storage 段（filesystem / s3）")
	}
}

// localForEachBlob 遍历本地存储里的 blob 数据文件。
func localForEachBlob(root string, fn func(string, int64) error) error {
	dir := filepath.Join(root, filepath.FromSlash(blobPrefix))
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			// 目录都不存在 = 一个 blob 都没有（仓库是空的）。
			return nil
		}
		return fmt.Errorf("读取 blob 目录 %s 失败: %w", dir, err)
	}
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		dgst, ok := digestFromBlobPath(path)
		if !ok {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return fn(dgst, info.Size())
	})
}

// s3ForEachBlob 遍历对象存储里的 blob 数据对象。
func s3ForEachBlob(ctx context.Context, s3 *distribution.S3Storage, fn func(string, int64) error) error {
	return objectstore.ListObjects(ctx, s3Config(s3), s3Prefix(s3, blobPrefix),
		func(key string, size int64) error {
			dgst, ok := digestFromBlobPath(key)
			if !ok {
				return nil
			}
			return fn(dgst, size)
		})
}

// localManifestCount 数本地存储里的 manifest 修订链接数。
func localManifestCount(root string) (int, error) {
	dir := filepath.Join(root, filepath.FromSlash(repositoriesPrefix))
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("读取仓库目录 %s 失败: %w", dir, err)
	}
	count := 0
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Base(path) != "link" {
			return nil
		}
		if !strings.Contains(filepath.ToSlash(path), "/_manifests/revisions/") {
			return nil
		}
		count++
		return nil
	})
	return count, err
}

// s3ManifestCount 数对象存储里的 manifest 修订链接数。
func s3ManifestCount(ctx context.Context, s3 *distribution.S3Storage) (int, error) {
	count := 0
	err := objectstore.ListObjects(ctx, s3Config(s3), s3Prefix(s3, repositoriesPrefix),
		func(key string, _ int64) error {
			if strings.HasSuffix(key, "/link") && strings.Contains(key, "/_manifests/revisions/") {
				count++
			}
			return nil
		})
	return count, err
}

// s3Config 把 distribution 的 s3 段换成本包的连接参数。
func s3Config(s3 *distribution.S3Storage) objectstore.S3Config {
	return objectstore.S3Config{
		Endpoint:  s3.Endpoint,
		Region:    s3.Region,
		Bucket:    s3.Bucket,
		AccessKey: s3.AccessKey,
		SecretKey: s3.SecretKey,
		PathStyle: s3.PathStyle,
		Secure:    s3.Secure,
	}
}

// s3Prefix 拼出桶内对象前缀。
//
// rootdirectory 是**桶内**的一段前缀（默认空 = 桶根），
// distribution 的内部路径接在它后面——
// 所以桶内对象是 <rootdirectory>/docker/registry/v2/...。
func s3Prefix(s3 *distribution.S3Storage, inner string) string {
	root := strings.Trim(s3.RootPath, "/")
	if root == "" {
		return inner
	}
	return root + "/" + inner
}

// digestFromBlobPath 从 blob 数据路径反推摘要。
//
// 上游布局：blobs/<算法>/<前两位>/<完整摘要>/data，
// 其中 <前两位> 只是把摘要前两位拆出来的一层目录（打散单目录文件数），
// **摘要本身是叶子目录名**（上游 digestFromPath 也是这么还原的）。
//
// ⚠️ 认不出来就返回 false（跳过这个文件）——
//
//	宁可少算一个 blob，也不能把别的路径当成 blob 报进统计里。
func digestFromBlobPath(path string) (string, bool) {
	norm := filepath.ToSlash(path)
	// blob 的数据文件名固定是 "data"——别的文件（link 等）不是 blob。
	if !strings.HasSuffix(norm, "/data") {
		return "", false
	}
	norm = strings.TrimSuffix(norm, "/data")
	parts := strings.Split(norm, "/")
	// 算法名在 "blobs" 后面那一段（布局固定：…/blobs/<算法>/…/<摘要>）。
	idx := -1
	for i, p := range parts {
		if p == "blobs" {
			idx = i
			break
		}
	}
	if idx < 0 || idx+1 >= len(parts) {
		return "", false
	}
	algo := parts[idx+1]
	hex := parts[len(parts)-1]
	if algo == "" || !isHexDigest(hex) {
		return "", false
	}
	return algo + ":" + hex, true
}

// isHexDigest 判断字符串是不是十六进制摘要。
//
// 长度下限取 32 位十六进制（128 bit）——真实摘要是 64 位，
// 但"至少像摘要"就够挡住目录名、link 这类噪声。
func isHexDigest(s string) bool {
	if len(s) < 32 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
