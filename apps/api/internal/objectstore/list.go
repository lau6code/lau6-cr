package objectstore

import (
	"context"

	"github.com/minio/minio-go/v7"
)

// ListObjects 遍历 bucket 里某个前缀下的所有对象，逐个回调键与字节数。
//
// ⚠️ 用的是**流式列举**：回调里不要攒全量键值对——
// 生产环境的桶里可能有上百万个对象，一次性攒进内存会把进程拖垮。
//
// 前缀按普通字符串前缀匹配（不是目录语义）：
// 列举 "docker/registry/v2/blobs/" 会把该前缀下的所有对象都列出来，
// 包括 "…/blobs/sha256/ab/abc…/data" 这种更深的对象。
func ListObjects(ctx context.Context, cfg S3Config, prefix string, fn func(key string, size int64) error) error {
	client, err := newClient(cfg)
	if err != nil {
		return err
	}
	for obj := range client.ListObjects(ctx, cfg.Bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	}) {
		if obj.Err != nil {
			return scrubError(obj.Err, cfg)
		}
		if err := fn(obj.Key, obj.Size); err != nil {
			return err
		}
	}
	return nil
}
