package crypto

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/lau6code/lau6-cr/apps/api/internal/cache"
)

func TestProjectSecretStoreEnsureGetRevoke(t *testing.T) {
	dir := t.TempDir()
	s := NewProjectSecretStore(ProjectSecretOptions{Dir: dir})
	ctx := context.Background()

	rec, err := s.Ensure(ctx, 42)
	if err != nil {
		t.Fatalf("创建项目密钥: %v", err)
	}
	if rec.Secret == "" {
		t.Fatal("密钥不能为空")
	}

	got, ok, err := s.Get(ctx, 42)
	if err != nil || !ok {
		t.Fatalf("读取项目密钥: ok=%v err=%v", ok, err)
	}
	if got.Secret != rec.Secret {
		t.Fatal("两次读取的密钥不一致")
	}

	// 幂等：已存在时 Ensure 返回同一把。
	again, err := s.Ensure(ctx, 42)
	if err != nil {
		t.Fatalf("重复创建: %v", err)
	}
	if again.Secret != rec.Secret {
		t.Fatal("重复创建应返回同一把密钥")
	}

	// 吊销后立即读不到。
	if err := s.Revoke(ctx, 42); err != nil {
		t.Fatalf("吊销项目密钥: %v", err)
	}
	if _, ok, err := s.Get(ctx, 42); err != nil || ok {
		t.Fatalf("吊销后应读不到: ok=%v err=%v", ok, err)
	}
}

func TestProjectSecretStoreDifferentTokensDifferentKeys(t *testing.T) {
	s := NewProjectSecretStore(ProjectSecretOptions{Dir: t.TempDir()})
	ctx := context.Background()
	a, err := s.Ensure(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Ensure(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if a.Secret == b.Secret {
		t.Fatal("不同令牌的密钥不能相同")
	}
}

func TestProjectSecretStoreRejectsInvalidTokenID(t *testing.T) {
	s := NewProjectSecretStore(ProjectSecretOptions{Dir: t.TempDir()})
	ctx := context.Background()
	if _, err := s.Ensure(ctx, 0); err == nil {
		t.Fatal("令牌 ID 为 0 应报错")
	}
	if _, _, err := s.Get(ctx, -1); err == nil {
		t.Fatal("令牌 ID 为负应报错")
	}
	if err := s.Revoke(ctx, 0); err == nil {
		t.Fatal("吊销时令牌 ID 为 0 应报错")
	}
}

func TestProjectSecretFileLayout(t *testing.T) {
	dir := t.TempDir()
	s := NewProjectSecretStore(ProjectSecretOptions{Dir: dir})
	ctx := context.Background()
	if _, err := s.Ensure(ctx, 7); err != nil {
		t.Fatal(err)
	}
	// 文件名是 sha256(token_id)，不是 token_id 本身。
	files, err := filepath.Glob(filepath.Join(dir, "*.key"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("密钥文件数 = %d，期望 1", len(files))
	}
	if filepath.Base(files[0]) == "project-7.key" {
		t.Fatalf("文件名不能直接用令牌 ID：%s", files[0])
	}
	info, err := os.Stat(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("密钥文件权限 = %o，期望 600", perm)
	}
}

func TestProjectSecretRedisBackend(t *testing.T) {
	ctx := context.Background()
	c := newFakeRedis()
	holder := cache.NewHolder(c)
	s := NewProjectSecretStore(ProjectSecretOptions{
		Cache: holder,
		Dir:   t.TempDir(),
	})

	rec, err := s.Ensure(ctx, 9)
	if err != nil {
		t.Fatalf("生成密钥: %v", err)
	}
	raw, ok, err := c.Get(ctx, CacheKeyProjectSecretPrefix+"9")
	if err != nil || !ok {
		t.Fatalf("缓存里没有项目密钥: ok=%v err=%v", ok, err)
	}
	var stored ProjectSecret
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("解析缓存里的项目密钥: %v", err)
	}
	if stored.Secret != rec.Secret {
		t.Fatal("缓存里的密钥与返回的不一致")
	}

	if err := s.Revoke(ctx, 9); err != nil {
		t.Fatalf("吊销: %v", err)
	}
	if _, ok, err := c.Get(ctx, CacheKeyProjectSecretPrefix+"9"); err != nil || ok {
		t.Fatalf("吊销后缓存里还有密钥: ok=%v err=%v", ok, err)
	}
}

func TestProjectSecretMigrateKeepsSecret(t *testing.T) {
	ctx := context.Background()
	from := newFakeRedis()
	to := newFakeRedis()
	s := NewProjectSecretStore(ProjectSecretOptions{
		Cache: cache.NewHolder(from),
		Dir:   t.TempDir(),
	})
	rec, err := s.Ensure(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	moved, err := s.Migrate(ctx, from, to, []int64{3})
	if err != nil {
		t.Fatalf("搬迁: %v", err)
	}
	if moved != 1 {
		t.Fatalf("搬迁数量 = %d，期望 1", moved)
	}
	raw, ok, err := to.Get(ctx, CacheKeyProjectSecretPrefix+"3")
	if err != nil || !ok {
		t.Fatalf("新后端里没有密钥: ok=%v err=%v", ok, err)
	}
	var stored ProjectSecret
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Secret != rec.Secret {
		t.Fatal("搬迁后密钥变了")
	}
}
