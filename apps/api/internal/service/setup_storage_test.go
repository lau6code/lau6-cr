package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/lau6code/lau6-cr/apps/api/internal/config"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// newStorageSetupService 造一个带配置文件的 setup 服务。
//
// local 根目录指向临时目录：默认的 /data/registry 在测试机上不该被创建。
func newStorageSetupService(t *testing.T, embedded bool) (*SetupService, string) {
	t.Helper()
	dir := t.TempDir()
	yamlPath := filepath.Join(dir, "application.yaml")
	db, err := store.Open("sqlite", "file:"+filepath.Join(dir, "test.db"), 1, 1)
	if err != nil {
		t.Fatalf("打开测试数据库: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("执行迁移: %v", err)
	}
	base := config.Default()
	base.Storage.Local.Root = filepath.Join(dir, "registry")
	settings, err := NewSettingsManager(db, base, yamlPath, WithDistributionEmbedded(embedded))
	if err != nil {
		t.Fatalf("构造设置管理器: %v", err)
	}
	return &SetupService{db: db, settings: settings, yamlPath: yamlPath}, yamlPath
}

// storageSubmitInput 造一份能过校验的提交参数。
func storageSubmitInput() SetupInput {
	return SetupInput{
		BootstrapToken: strings.Repeat("a", 32),
		PublicURL:      "http://localhost:5001",
		AdminUsername:  "admin",
		AdminPassword:  "VerifyPass123!",
	}
}

// readYAML 读回写好的配置文件。
func readYAML(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取配置文件: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("解析配置文件: %v", err)
	}
	return doc
}

// TestSubmitPersistsStorageChoice 覆盖"存储后端在 setup 阶段选定并落盘"。
//
// 契约（docs/design/11-storage.md §4）：
//   - 向导里的 storage 段写进 application.yaml（文件基线，供备份与迁移）
//   - 同一份值写进 system_settings（运行时真源）
//   - 跑起来的快照就是选中的那套
func TestSubmitPersistsStorageChoice(t *testing.T) {
	ctx := context.Background()
	s, yamlPath := newStorageSetupService(t, true)

	in := storageSubmitInput()
	in.Storage = &StorageInput{Driver: "s3"}
	in.Storage.S3.Endpoint = "https://minio.example.com"
	in.Storage.S3.Bucket = "lau6-cr"
	in.Storage.S3.Region = "us-east-1"
	in.Storage.S3.PathStyle = true
	in.Storage.S3.AccessKey = "AKIAEXAMPLE"
	in.Storage.S3.SecretKey = "s3cr3t"

	if _, err := s.Submit(ctx, in); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}

	// 文件基线
	doc := readYAML(t, yamlPath)
	storage, ok := doc["storage"].(map[string]any)
	if !ok {
		t.Fatalf("配置文件里没有 storage 段: %v", doc)
	}
	if storage["driver"] != "s3" {
		t.Errorf("storage.driver = %v，期望 s3", storage["driver"])
	}
	s3, ok := storage["s3"].(map[string]any)
	if !ok {
		t.Fatalf("配置文件里没有 storage.s3 段: %v", storage)
	}
	if s3["endpoint"] != "https://minio.example.com" {
		t.Errorf("storage.s3.endpoint = %v", s3["endpoint"])
	}
	if s3["bucket"] != "lau6-cr" {
		t.Errorf("storage.s3.bucket = %v", s3["bucket"])
	}
	if s3["access_key"] != "AKIAEXAMPLE" {
		t.Errorf("storage.s3.access_key = %v，期望写入向导填的值", s3["access_key"])
	}
	if s3["secure"] != true {
		t.Errorf("storage.s3.secure = %v，期望由 https 推导为 true", s3["secure"])
	}

	// 运行时真源
	snap := s.settings.Snapshot()
	if snap.Storage.Driver != "s3" {
		t.Errorf("快照里的 driver = %q，期望 s3", snap.Storage.Driver)
	}
	if snap.Storage.S3.Endpoint != "https://minio.example.com" {
		t.Errorf("快照里的 endpoint = %q", snap.Storage.S3.Endpoint)
	}
	if snap.Storage.S3.Bucket != "lau6-cr" {
		t.Errorf("快照里的 bucket = %q", snap.Storage.S3.Bucket)
	}
	if !snap.Storage.S3.PathStyle {
		t.Error("快照里的 path_style 期望为 true")
	}
	// secure 由 endpoint 的 scheme 推导，不接受"地址是 http 但 secure 写着 true"。
	if !snap.Storage.S3.Secure {
		t.Error("https 的 endpoint 应当推出 secure=true")
	}
}

// TestSubmitStorageEnvPinned 覆盖"存储后端被环境变量钉死"。
//
// env 的优先级最高，向导改了也没用——但必须**说出来**，
// 否则用户填了一堆参数却发现没生效（AGENTS.md 规则 1）。
func TestSubmitStorageEnvPinned(t *testing.T) {
	t.Setenv("CR_STORAGE_DRIVER", "local")
	ctx := context.Background()
	s, yamlPath := newStorageSetupService(t, true)

	in := storageSubmitInput()
	in.Storage = &StorageInput{Driver: "s3"}
	in.Storage.S3.Endpoint = "https://minio.example.com"
	in.Storage.S3.Bucket = "lau6-cr"

	res, err := s.Submit(ctx, in)
	if err != nil {
		t.Fatalf("初始化失败: %v", err)
	}
	if got := s.settings.Snapshot().Storage.Driver; got != "local" {
		t.Errorf("快照里的 driver = %q，期望仍是 env 给的 local", got)
	}
	ignored := false
	for _, p := range res.IgnoredEnvPinned {
		if p.Key == "storage.driver" {
			ignored = true
		}
	}
	if !ignored {
		t.Errorf("被 env 钉死的 storage.driver 应当回报给用户，实际: %+v", res.IgnoredEnvPinned)
	}
	doc := readYAML(t, yamlPath)
	storage, _ := doc["storage"].(map[string]any)
	if storage["driver"] != "local" {
		t.Errorf("配置文件里的 driver = %v，期望 local", storage["driver"])
	}
}

// TestSubmitWithoutStorageKeepsLocal 覆盖"没传 storage 段"。
//
// CLI 路径与 external 形态都不问存储：这时不能把向导的零值当成
// "用户选了 local"写进去——那会把已有的 s3 配置抹掉。
func TestSubmitWithoutStorageKeepsLocal(t *testing.T) {
	ctx := context.Background()
	s, yamlPath := newStorageSetupService(t, true)

	if _, err := s.Submit(ctx, storageSubmitInput()); err != nil {
		t.Fatalf("初始化失败: %v", err)
	}
	doc := readYAML(t, yamlPath)
	storage, _ := doc["storage"].(map[string]any)
	if storage["driver"] != "local" {
		t.Errorf("配置文件里的 driver = %v，期望 local", storage["driver"])
	}
	// s3 参数是托管设置，导出时总会有一份（值为空）。
	// 关键是**值**为空，而不是"这段不存在"。
	s3, _ := storage["s3"].(map[string]any)
	if v, _ := s3["endpoint"].(string); v != "" {
		t.Errorf("storage.s3.endpoint = %v，没选 s3 时应当为空", v)
	}
}

// TestSubmitStorageLocalRootWritable 覆盖 local 后端的目录检查。
//
// 目录不可写却让初始化通过 = 第一次 push 才炸，而那时用户已经初始化完了。
func TestSubmitStorageLocalRootWritable(t *testing.T) {
	ctx := context.Background()
	s, yamlPath := newStorageSetupService(t, true)

	// 把根目录指向一个**文件**——MkdirAll 会失败。
	base := *s.settings.Snapshot()
	base.Storage.Local.Root = filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(base.Storage.Local.Root, []byte("x"), 0o600); err != nil {
		t.Fatalf("准备不可用目录: %v", err)
	}
	if err := s.settings.SetBase(&base); err != nil {
		t.Fatalf("更新基线: %v", err)
	}

	in := storageSubmitInput()
	in.Storage = &StorageInput{Driver: "local"}
	if _, err := s.Submit(ctx, in); err == nil {
		t.Fatal("本地存储目录不可用时初始化应当失败")
	}
	if _, err := os.Stat(yamlPath); err == nil {
		t.Error("初始化失败时不该写配置文件")
	}
}

// TestTestStorageLocal 覆盖本地目录的探测。
func TestTestStorageLocal(t *testing.T) {
	ctx := context.Background()
	s, _ := newStorageSetupService(t, true)

	res, err := s.TestStorage(ctx, StorageTestInput{
		BootstrapToken: strings.Repeat("a", 32),
		Storage:        StorageInput{Driver: "local"},
	})
	if err != nil {
		t.Fatalf("本地存储探测失败: %v", err)
	}
	if !strings.Contains(res.Detail, "可写") {
		t.Errorf("detail = %q，期望说明目录可写", res.Detail)
	}
}

// TestTestStorageS3MissingParams 覆盖参数不全时的报错。
//
// 拿空凭据去连对象存储，报出来的错没人看得懂——
// 必须直接说"缺凭据，去填或用环境变量"。
func TestTestStorageS3MissingParams(t *testing.T) {
	ctx := context.Background()
	s, _ := newStorageSetupService(t, true)

	in := StorageTestInput{
		BootstrapToken: strings.Repeat("a", 32),
		Storage:        StorageInput{Driver: "s3"},
	}
	in.Storage.S3.Endpoint = "https://minio.example.com"
	in.Storage.S3.Bucket = "lau6-cr"

	// 凭据没填：先报凭据。
	_, err := s.TestStorage(ctx, in)
	if err == nil {
		t.Fatal("缺凭据时应当报错")
	}
	if !strings.Contains(err.Error(), "凭据") {
		t.Errorf("err = %v，期望说明缺凭据", err)
	}

	// 凭据给了、bucket 没填：报 bucket。
	in.Storage.S3.AccessKey = "AKIAEXAMPLE"
	in.Storage.S3.SecretKey = "s3cr3t"
	in.Storage.S3.Bucket = ""
	_, err = s.TestStorage(ctx, in)
	if err == nil {
		t.Fatal("缺 bucket 时应当报错")
	}
	if !strings.Contains(err.Error(), "bucket") {
		t.Errorf("err = %v，期望说明缺 bucket", err)
	}
}

// TestStorageInfoOmitsCredentials 覆盖预填信息**不含凭据**。
//
// 预填端点在未初始化时是开放面，密钥绝不能回显。
func TestStorageInfoOmitsCredentials(t *testing.T) {
	ctx := context.Background()
	s, _ := newStorageSetupService(t, true)

	base := *s.settings.Snapshot()
	base.Storage.S3.AccessKey = "AKIAEXAMPLE"
	base.Storage.S3.SecretKey = "s3cr3t"
	if err := s.settings.SetBase(&base); err != nil {
		t.Fatalf("更新基线: %v", err)
	}

	info, err := s.StorageInfo(ctx)
	if err != nil {
		t.Fatalf("StorageInfo: %v", err)
	}
	if !info.S3.AccessKeySet || !info.S3.SecretKeySet {
		t.Error("预填信息应当标出凭据配没配")
	}
	blob, err := yaml.Marshal(info)
	if err != nil {
		t.Fatalf("序列化预填信息: %v", err)
	}
	if strings.Contains(string(blob), "AKIAEXAMPLE") || strings.Contains(string(blob), "s3cr3t") {
		t.Errorf("预填信息里漏出了凭据: %s", blob)
	}
}

// TestMountGoneHint 覆盖"挂载源被删了"的自我诊断。
//
// 症状：父目录还在，但建子目录报 ENOENT——那是死挂载，
// MkdirAll 救不回来，只能补回宿主目录并重建容器。
// 认错方向比不给提示更糟（会把人引到错的排查方向），所以认不出就返回空串。
func TestMountGoneHint(t *testing.T) {
	parent := t.TempDir()

	t.Run("父目录还在且报 ENOENT 时给提示", func(t *testing.T) {
		err := &os.PathError{Op: "mkdir", Path: filepath.Join(parent, "registry"), Err: syscall.ENOENT}
		hint := mountGoneHint(err, parent)
		if hint == "" {
			t.Fatalf("死挂载场景没有给提示")
		}
		if !strings.Contains(hint, parent) {
			t.Fatalf("提示里没提父目录 %s：%s", parent, hint)
		}
	})

	t.Run("别的错误不给提示", func(t *testing.T) {
		err := &os.PathError{Op: "mkdir", Path: filepath.Join(parent, "registry"), Err: syscall.EACCES}
		if hint := mountGoneHint(err, parent); hint != "" {
			t.Fatalf("不是 ENOENT 却给了提示：%s", hint)
		}
	})

	t.Run("父目录本身不存在时不给提示", func(t *testing.T) {
		err := &os.PathError{Op: "mkdir", Path: "/nonexistent/registry", Err: syscall.ENOENT}
		if hint := mountGoneHint(err, "/nonexistent"); hint != "" {
			t.Fatalf("父目录不存在却给了提示：%s", hint)
		}
	})
}
