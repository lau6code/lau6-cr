package crypto

import (
	"context"
	"fmt"
	"os"

	"github.com/lau6code/lau6-cr/apps/api/internal/cache"
	"github.com/lau6code/lau6-cr/apps/api/internal/fsatomic"
)

// 私钥的双源装载策略。
//
// 签名私钥（Ed25519）与口令加密私钥（X25519）走的是同一套流程：
// 缓存是权威源还是文件是权威源，取决于缓存后端类型。
// 两种私钥只有"生成/编码/解析"这��步不同，其余流程收敛在这里。

// keySource 描述一把私钥的装载参数。
type keySource[T any] struct {
	// label 是私钥的用途名，进错误信息（如"签名密钥"）。
	label string
	// cacheKey 是缓存键名。
	cacheKey string
	// filePath 是私钥文件路径（0600）。
	filePath string
	// parse 把 PEM 字节解析成私钥；source 是错误信息里的来源描述。
	parse func(data []byte, source string) (T, error)
	// generate 生成一把新私钥。
	generate func() (T, error)
	// encode 把私钥编码成 PEM。
	encode func(T) ([]byte, error)
}

// loadKey 按双源策略装载一把私钥。
//
// redis 模式下**缓存是权威源**（docs/design/12-cache.md §3.3）：
//
//	缓存里有        → 载入内存（**不读文件**）
//	缓存里没有      → 生成 → 写回缓存 + 写一份文件（审计副本）
//	缓存连不上      → **启动失败**（fail-closed）
//
// fail-closed 是硬要求：降级成"连不上就生成新密钥"，
// 每次缓存抖动都会让全体用户重新登录——
// 一次缓存抖动被放大成可用性事故。
//
// local 模式（进程内缓存，或没配缓存）下**文件是权威源**：
// 文件不存在才生成。
func loadKey[T any](ctx context.Context, c cache.Cache, s keySource[T]) (T, error) {
	var zero T
	if !cacheIsAuthoritative(c) {
		return loadKeyFromFile(s)
	}

	raw, ok, err := c.Get(ctx, s.cacheKey)
	if err != nil {
		// ❌ fail-closed：连不上就启动失败，由容器编排重试。
		return zero, fmt.Errorf("从缓存读%s（Redis 不可达）: %w", s.label, err)
	}
	if ok {
		return s.parse(raw, "缓存 "+s.cacheKey)
	}

	// 缓存里没有 → 生成 → 写回缓存 + 写一份文件。
	key, err := s.generate()
	if err != nil {
		return zero, err
	}
	pemBytes, err := s.encode(key)
	if err != nil {
		return zero, err
	}
	// ⚠️ 不设 TTL：私钥是启动源，不是热点数据。
	if err := c.Set(ctx, s.cacheKey, pemBytes, 0); err != nil {
		return zero, fmt.Errorf("写%s到缓存: %w", s.label, err)
	}
	// 文件在 redis 模式下**不是启动源**，只是灾备与审计副本。
	if err := fsatomic.Write(s.filePath, pemBytes, 0o600); err != nil {
		return zero, err
	}
	return key, nil
}

// loadKeyFromFile 从文件读私钥；文件不存在则生成。
//
// 这是 **local 模式**的路径：文件是权威源。
func loadKeyFromFile[T any](s keySource[T]) (T, error) {
	var zero T
	b, err := os.ReadFile(s.filePath)
	if err == nil {
		return s.parse(b, s.filePath)
	}
	if !os.IsNotExist(err) {
		return zero, fmt.Errorf("读取%s %s: %w", s.label, s.filePath, err)
	}
	key, err := s.generate()
	if err != nil {
		return zero, err
	}
	pemBytes, err := s.encode(key)
	if err != nil {
		return zero, err
	}
	if err := fsatomic.Write(s.filePath, pemBytes, 0o600); err != nil {
		return zero, err
	}
	return key, nil
}

// cacheIsAuthoritative 判断缓存是不是权威源。
//
// 只有 redis 才算：local 是进程内的，进程一没就没了，
// 不能当"启动源"——那等于每次重启都换密钥。
func cacheIsAuthoritative(c cache.Cache) bool {
	return c != nil && c.Driver() == "redis"
}
