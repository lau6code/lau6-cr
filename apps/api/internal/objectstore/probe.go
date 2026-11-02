// Package objectstore 负责对象存储（S3 兼容）的**连通性探测**。
//
// ⚠️ lau6-cr 自己**不存 blob**——blob 由 distribution 写进对象存储
//
//	（11-storage.md §1）。本包存在的唯一理由是 setup 阶段：
//	在把"对象存储"这个后端写进配置之前，先真连一次，
//	把"bucket 名写错""region 填错""path_style 不对"这类问题
//	挡在初始化那一刻，而不是等到第一次 push 才 500。
//
// 探测方式是**写 → 读 → 删**：只读探针（比如 ListBucket）证明不了
// 写权限——CI 里最常见的"只给了读权限"照样能在 List 时通过，
// 却在 push 时失败。
package objectstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Config 是探测用的连接参数。
//
// 字段与 config.S3Storage 对齐，但**不共用类型**：
// 探测发生在 setup 阶段，那时配置还没定型，
// 传进来的是向导表单里的值，不一定有值（凭据可能来自 env）。
type S3Config struct {
	// Endpoint 是对象存储地址，形如 https://minio.example.com。
	// 也接受不带 scheme 的 host[:port]，此时按 Secure 决定协议。
	Endpoint string
	Region   string
	Bucket   string
	// AccessKey / SecretKey 留空表示"没有静态凭据"——
	// 此时探测会明确报错，而不是拿空凭据去连出一堆看不懂的错误。
	AccessKey string
	SecretKey string
	// PathStyle 为 true 时用 path style 寻址（bucket 放在路径里）。
	// MinIO / Ceph 必为 true。
	PathStyle bool
	// Secure 在 endpoint **不带 scheme** 时决定用 https 还是 http。
	// endpoint 带 scheme 时以 scheme 为准。
	Secure bool
}

// probePrefix 是探针对象的前缀。
//
// 放在桶根而不是 blob 目录里：探针不是镜像，
// 不该混进 distribution 的存储布局里。
const probePrefix = "lau6-cr-probe/"

// probeBody 是探针对象的内容：固定字节，便于读回比对。
var probeBody = []byte("lau6-cr storage probe")

// ProbeS3 真连一次对象存储：写入探针对象 → 读回比对 → 删除。
//
// 返回耗时与错误。错误信息里的凭据会被清洗掉——
// SDK 的错误可能带上请求 URL，甚至在某些实现里回显 Access Key。
func ProbeS3(ctx context.Context, cfg S3Config) (elapsed time.Duration, err error) {
	client, err := newClient(cfg)
	if err != nil {
		return 0, err
	}
	key := fmt.Sprintf("%s%d", probePrefix, time.Now().UnixNano())
	start := time.Now()

	// 无论成败都要删掉探针对象：残留的探针会一直占着桶里的空间，
	// 而用户不会知道那是什么。
	//
	// ⚠️ 清理失败**不能吞掉**：主流程成功但探针删不掉，
	//    桶里会留下垃圾对象，用户得知道（所以它在主错误为空时才顶上来）。
	defer func() {
		if rmErr := client.RemoveObject(ctx, cfg.Bucket, key, minio.RemoveObjectOptions{}); rmErr != nil && err == nil {
			err = fmt.Errorf("探测通过，但清理探针对象失败（桶里会留下 %s）：%w", key, scrubError(rmErr, cfg))
		}
	}()

	if _, err = client.PutObject(ctx, cfg.Bucket, key,
		bytes.NewReader(probeBody), int64(len(probeBody)), minio.PutObjectOptions{}); err != nil {
		return time.Since(start), scrubError(err, cfg)
	}

	obj, getErr := client.GetObject(ctx, cfg.Bucket, key, minio.GetObjectOptions{})
	if getErr != nil {
		return time.Since(start), scrubError(getErr, cfg)
	}
	defer obj.Close()

	got, readErr := io.ReadAll(obj)
	if readErr != nil {
		return time.Since(start), scrubError(readErr, cfg)
	}
	if !bytes.Equal(got, probeBody) {
		return time.Since(start), errors.New("读回内容不一致：对象存储返回的不是刚写入的数据")
	}
	return time.Since(start), nil
}

func newClient(cfg S3Config) (*minio.Client, error) {
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, errors.New("endpoint 不能为空")
	}
	if strings.TrimSpace(cfg.Bucket) == "" {
		return nil, errors.New("bucket 不能为空")
	}
	host, secure, err := splitEndpoint(cfg.Endpoint, cfg.Secure)
	if err != nil {
		return nil, err
	}
	opts := &minio.Options{
		Secure:       secure,
		Region:       cfg.Region,
		BucketLookup: bucketLookup(cfg.PathStyle),
	}
	// 静态凭据：AWS 默认凭证链不启用（11-storage.md §2.2）——
	// 容器里的实例角色不可控，出问题时排查成本极高。
	if cfg.AccessKey != "" || cfg.SecretKey != "" {
		opts.Creds = credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, "")
	}
	client, err := minio.New(host, opts)
	if err != nil {
		return nil, scrubError(err, cfg)
	}
	return client, nil
}

// SecureFromEndpoint 从 endpoint 的 scheme 推导"用不用 TLS"。
//
// endpoint 带 scheme 时以 scheme 为准（https → true，http → false）；
// 不带 scheme 时返回 fallback（由配置里的 secure 决定）。
//
// ⚠️ 别让用户把"协议"和"secure 开关"填成两处可能打架的值：
//
//	"http:// 的地址 + secure: true" 这种组合，
//	探测用 http、distribution 用 https，症状是"探测通过但推送全失败"。
func SecureFromEndpoint(endpoint string, fallback bool) bool {
	ep := strings.TrimSpace(endpoint)
	if !strings.Contains(ep, "://") {
		return fallback
	}
	u, err := url.Parse(ep)
	if err != nil {
		return fallback
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		return false
	default:
		return fallback
	}
}

// bucketLookup 把"path style 寻址"翻成 minio-go 的寻址方式。
//
// ⚠️ 口径必须与 distribution 的 `forcepathstyle` 一致，
//
//	否则探测通过、实际写入却落到另一个位置（11-storage.md §2.3）：
//	- path style：bucket 放在路径里（MinIO / Ceph 必用）
//	- DNS style：bucket 放在主机名里（AWS S3 常用）
func bucketLookup(pathStyle bool) minio.BucketLookupType {
	if pathStyle {
		return minio.BucketLookupPath
	}
	return minio.BucketLookupDNS
}

// splitEndpoint 把 endpoint 拆成 minio-go 要的 host[:port] 与 TLS 开关。
//
// minio-go 的 endpoint **不带 scheme**，而 distribution 的
// `regionendpoint` 必须带（AWS SDK 直接拿它做 url.Parse，
// 没 scheme 就没有 Host，请求根本发不出去）。
// 两边的要求正好相反，所以这里统一成"带 scheme 的 endpoint"，
// 由本函数负责拆。
func splitEndpoint(endpoint string, secure bool) (host string, useTLS bool, err error) {
	ep := strings.TrimSpace(endpoint)
	if !strings.Contains(ep, "://") {
		if strings.ContainsAny(ep, "/?#") {
			return "", false, fmt.Errorf("endpoint 格式不正确：%q", endpoint)
		}
		return ep, secure, nil
	}
	u, perr := url.Parse(ep)
	if perr != nil {
		return "", false, fmt.Errorf("endpoint 格式不正确：%w", perr)
	}
	if u.Host == "" {
		return "", false, fmt.Errorf("endpoint 缺少主机名：%q", endpoint)
	}
	if u.Path != "" && u.Path != "/" {
		return "", false, fmt.Errorf("endpoint 不能带路径：%q", endpoint)
	}
	switch u.Scheme {
	case "https":
		return u.Host, true, nil
	case "http":
		return u.Host, false, nil
	default:
		return "", false, fmt.Errorf("endpoint 的协议只能是 http 或 https：%q", endpoint)
	}
}

// scrubError 把错误里的凭据换成 ***。
//
// SDK 错误会带上请求 URL，某些实现还会回显 Access Key——
// 错误信息会进日志、进向导的提示框、进 setup 的响应体，
// 密钥不能从那里漏出去。
func scrubError(err error, cfg S3Config) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	for _, secret := range []string{cfg.SecretKey, cfg.AccessKey} {
		if secret == "" {
			continue
		}
		msg = strings.ReplaceAll(msg, secret, "***")
	}
	return errors.New(msg)
}
