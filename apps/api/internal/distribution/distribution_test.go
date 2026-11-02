package distribution

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/lau6code/lau6-cr/apps/api/internal/config"
)

// TestRender 覆盖渲染出来的 distribution 配置。
func TestRender(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.Local.Root = "/data/registry"
	cfg.Core.PublicURL = "http://registry.example.com"
	cfg.Token.Audience = "lau6-cr"

	b, err := Render(RenderInputs{
		Cfg:             *cfg,
		JWKSPath:        "/data/config/jwt-jwks.json",
		RegistrySecret:  "s3cret",
		WebhookSecret:   "hook",
		CallbackBaseURL: "http://127.0.0.1:5000",
		ListenAddr:      "127.0.0.1:5001",
		DebugAddr:       "127.0.0.1:5002",
		LogLevel:        "info",
	})
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("解析渲染结果失败: %v\n%s", err, b)
	}

	httpCfg, ok := doc["http"].(map[string]any)
	if !ok {
		t.Fatalf("渲染结果缺少 http 段:\n%s", b)
	}
	if httpCfg["addr"] != "127.0.0.1:5001" {
		t.Errorf("http.addr 期望 127.0.0.1:5001，实际 %v", httpCfg["addr"])
	}
	// debug 端口必须与监听端口错开，否则两个端口撞车起不来。
	debug, ok := httpCfg["debug"].(map[string]any)
	if !ok {
		t.Fatalf("渲染结果缺少 http.debug 段:\n%s", b)
	}
	if debug["addr"] == httpCfg["addr"] {
		t.Errorf("debug 端口不能与监听端口相同（实际 %v）", debug["addr"])
	}
	if debug["addr"] != "127.0.0.1:5002" {
		t.Errorf("http.debug.addr 期望 127.0.0.1:5002，实际 %v", debug["addr"])
	}

	storage, ok := doc["storage"].(map[string]any)
	if !ok {
		t.Fatalf("渲染结果缺少 storage 段:\n%s", b)
	}
	fs, ok := storage["filesystem"].(map[string]any)
	if !ok {
		t.Fatalf("渲染结果缺少 storage.filesystem 段:\n%s", b)
	}
	if fs["rootdirectory"] != "/data/registry" {
		t.Errorf("storage 根目录期望 /data/registry，实际 %v", fs["rootdirectory"])
	}

	notif, ok := doc["notifications"].(map[string]any)
	if !ok {
		t.Fatalf("渲染结果缺少 notifications 段:\n%s", b)
	}
	endpoints, ok := notif["endpoints"].([]any)
	if !ok || len(endpoints) == 0 {
		t.Fatalf("渲染结果缺少事件回调端点:\n%s", b)
	}
	ep, ok := endpoints[0].(map[string]any)
	if !ok {
		t.Fatalf("事件回调端点格式异常:\n%s", b)
	}
	if ep["url"] != "http://127.0.0.1:5000/api/internal/registry-events" {
		t.Errorf("事件回调地址期望 http://127.0.0.1:5000/api/internal/registry-events，实际 %v", ep["url"])
	}
}

// TestRender_DeleteEnabled 覆盖 storage 级别的 delete 开关。
//
// ⚠️ 这个键是 **storage 级别**（与 filesystem / s3 平级），不是驱动块里的。
// distribution 3.x 在 registry/handlers/app.go 里读 config.Storage["delete"]["enabled"]，
// 没开 → app.deleteEnabled 为 false → manifest / tag 的 DELETE
// 一律回 **405 unsupported**（registry/handlers/manifests.go）。
//
// 症状是"删仓库永远失败"，而且日志里只有一条正常的 405 响应——
// 没有任何报错线索，所以必须用测试把它钉住。
//
// ⚠️ 两种驱动都要覆盖：这个开关是 **storage 级别**的，
// 与用哪个驱动无关——只测 local 会漏掉"只有 S3 分支没设"的情况。
func TestRender_DeleteEnabled(t *testing.T) {
	for _, driver := range []string{"local", "s3"} {
		t.Run(driver, func(t *testing.T) {
			cfg := config.Default()
			cfg.Storage.Driver = driver
			b, err := Render(RenderInputs{
				Cfg:             *cfg,
				JWKSPath:        "/data/config/jwt-jwks.json",
				RegistrySecret:  "s3cret",
				WebhookSecret:   "hook",
				CallbackBaseURL: "http://127.0.0.1:5000",
				ListenAddr:      "127.0.0.1:5001",
				LogLevel:        "info",
			})
			if err != nil {
				t.Fatalf("渲染失败: %v", err)
			}
			var doc map[string]any
			if err := yaml.Unmarshal(b, &doc); err != nil {
				t.Fatalf("解析渲染结果失败: %v\n%s", err, b)
			}
			storage, ok := doc["storage"].(map[string]any)
			if !ok {
				t.Fatalf("渲染结果缺少 storage 段:\n%s", b)
			}
			del, ok := storage["delete"].(map[string]any)
			if !ok {
				t.Fatalf("渲染结果缺少 storage.delete 段（删镜像会被 gate 成 405）:\n%s", b)
			}
			if del["enabled"] != true {
				t.Errorf("storage.delete.enabled = %v，期望 true（否则 DELETE manifest 一律 405）", del["enabled"])
			}
		})
	}
}

// TestRender_S3 覆盖渲染出来的 S3 存储配置。
//
// ⚠️ 键名必须与 distribution 3.x 的 s3 驱动一致
//
//	（registry/storage/driver/s3-aws/s3.go 读的是
//	`regionendpoint` / `forcepathstyle` / `rootdirectory`；
//	`rootdirectory` 是可选的，本项目**不写**——写了就多一层前缀，见 TestRender_S3KeyLayout）。
//
//	驱动**不校验未知键**——键名写错会被静默忽略：
//	endpoint 退回 AWS 默认地址、blob 落到桶根、
//	path style 失效（DNS 寻址打到别的主机）。
//	症状是"配置看着对、blob 却存错地方"，极难排查。
func TestRender_S3(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.Driver = "s3"
	cfg.Storage.S3.Endpoint = "https://minio.example.com"
	cfg.Storage.S3.Bucket = "lau6-cr"
	cfg.Storage.S3.Region = "us-east-1"
	cfg.Storage.S3.AccessKey = "AKIAEXAMPLE"
	cfg.Storage.S3.SecretKey = "s3cr3t"
	cfg.Storage.S3.PathStyle = true
	cfg.Storage.S3.Secure = true

	b, err := Render(RenderInputs{
		Cfg:             *cfg,
		JWKSPath:        "/data/config/jwt-jwks.json",
		RegistrySecret:  "s3cret",
		WebhookSecret:   "hook",
		CallbackBaseURL: "http://127.0.0.1:5000",
		ListenAddr:      "127.0.0.1:5001",
		LogLevel:        "info",
	})
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("解析渲染结果失败: %v\n%s", err, b)
	}
	storage, ok := doc["storage"].(map[string]any)
	if !ok {
		t.Fatalf("渲染结果缺少 storage 段:\n%s", b)
	}
	s3, ok := storage["s3"].(map[string]any)
	if !ok {
		t.Fatalf("渲染结果缺少 storage.s3 段:\n%s", b)
	}
	if s3["regionendpoint"] != "https://minio.example.com" {
		t.Errorf("storage.s3.regionendpoint 期望 https://minio.example.com，实际 %v", s3["regionendpoint"])
	}
	if s3["forcepathstyle"] != true {
		t.Errorf("storage.s3.forcepathstyle 期望 true，实际 %v", s3["forcepathstyle"])
	}
	// ⚠️ rootdirectory 必须留空（桶根）。
	//
	// distribution 自己的内部路径已经带 /docker/registry/ 这一层
	// （registry/storage/paths.go 的 storagePathRoot），
	// s3 驱动的对象键 = RootDirectory + 内部路径（s3-aws/s3.go 的 s3Path）。
	// 这里再填一遍就成了 docker/registry/v2/docker/registry/v2/...（多一层），
	// 键布局与本地形态对不上，也会让换桶/迁移脚本按错前缀去找对象。
	if v, ok := s3["rootdirectory"]; ok {
		t.Errorf("storage.s3 不该有 rootdirectory 这个键（应留空 = 桶根），实际 %v", v)
	}
	if s3["bucket"] != "lau6-cr" {
		t.Errorf("storage.s3.bucket 期望 lau6-cr，实际 %v", s3["bucket"])
	}
	// 旧键名不能残留：它们会被驱动静默忽略，看起来"配了"其实没生效。
	for _, stale := range []string{"endpoint", "pathstyle", "rootpath", "path_style"} {
		if _, ok := s3[stale]; ok {
			t.Errorf("storage.s3 不该有 %q 这个键（distribution 3.x 不认它）", stale)
		}
	}
}

// TestRender_S3KeyLayout 锁住 S3 的**对象键布局**。
//
// 键 = RootDirectory + distribution 内部路径（s3-aws/s3.go 的 s3Path），
// 而内部路径自带 /docker/registry/v2/ 这一层。
// 所以桶内对象必须是 docker/registry/v2/... ——
// 与本地形态的 <root>/docker/registry/v2/... 一一对应。
//
// ⚠️ 这是"多一层前缀"的回归防线：RootDirectory 一旦被填成
//
//	/docker/registry/v2，键就变成 docker/registry/v2/docker/registry/v2/...，
//	旧对象全部失联（拉取报 blob unknown），且没有任何报错线索。
func TestRender_S3KeyLayout(t *testing.T) {
	cfg := config.Default()
	cfg.Storage.Driver = "s3"
	cfg.Storage.S3.Endpoint = "http://minio.example.com:9000"
	cfg.Storage.S3.Bucket = "cr-test"
	cfg.Storage.S3.Region = "us-east-1"
	cfg.Storage.S3.PathStyle = true
	cfg.Storage.S3.Secure = false

	b, err := Render(RenderInputs{
		Cfg:             *cfg,
		JWKSPath:        "/data/config/jwt-jwks.json",
		RegistrySecret:  "s3cret",
		WebhookSecret:   "hook",
		CallbackBaseURL: "http://127.0.0.1:5000",
		ListenAddr:      "127.0.0.1:5001",
		LogLevel:        "info",
	})
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("解析渲染结果失败: %v\n%s", err, b)
	}
	s3, ok := doc["storage"].(map[string]any)["s3"].(map[string]any)
	if !ok {
		t.Fatalf("渲染结果缺少 storage.s3 段:\n%s", b)
	}
	root, _ := s3["rootdirectory"].(string)

	// 复刻驱动的拼键方式：s3Path(path) = TrimLeft(TrimRight(root,"/")+path, "/")
	const internalPath = "/docker/registry/v2/repositories/test/postgres/_manifests/tags/18/current/link"
	key := strings.TrimLeft(strings.TrimRight(root, "/")+internalPath, "/")
	const want = "docker/registry/v2/repositories/test/postgres/_manifests/tags/18/current/link"
	if key != want {
		t.Errorf("S3 对象键 = %q，期望 %q（根目录前缀重复了）", key, want)
	}
}

// TestRender_MissingInputs 覆盖必填项缺失时**明确报错**（不渲染半成品）。
func TestRender_MissingInputs(t *testing.T) {
	cfg := config.Default()
	if _, err := Render(RenderInputs{
		Cfg:        *cfg,
		ListenAddr: "127.0.0.1:5001",
	}); err == nil {
		t.Fatal("缺回调地址期望报错")
	}
	if _, err := Render(RenderInputs{
		Cfg:             *cfg,
		CallbackBaseURL: "http://127.0.0.1:5000",
	}); err == nil {
		t.Fatal("缺监听地址期望报错")
	}
}

// TestRender_NoDebugAddr 覆盖"不配 debug 地址 → 渲染里就没有 debug 段"。
//
// 没给地址时**不能**自己推导一个（"监听端口 + 1" 推导出来的端口
// 可能正好撞上 lau6-cr 自己的监听端口 → bind 失败 → 服务起不来）。
func TestRender_NoDebugAddr(t *testing.T) {
	cfg := config.Default()
	b, err := Render(RenderInputs{
		Cfg:             *cfg,
		JWKSPath:        "/data/config/jwt-jwks.json",
		RegistrySecret:  "s3cret",
		WebhookSecret:   "hook",
		CallbackBaseURL: "http://127.0.0.1:5000",
		ListenAddr:      "127.0.0.1:5001",
		LogLevel:        "info",
	})
	if err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("解析渲染结果失败: %v\n%s", err, b)
	}
	httpCfg, ok := doc["http"].(map[string]any)
	if !ok {
		t.Fatalf("渲染结果缺少 http 段:\n%s", b)
	}
	if debug, ok := httpCfg["debug"]; ok {
		t.Errorf("未配 debug 地址时不应渲染 debug 段，实际 %v", debug)
	}
}

// TestSupervisor_MissingBinary 覆盖"distribution 二进制不存在 → 明确报错"。
//
// 这是最常见的部署事故，不能只报一句 exec 的 "no such file or directory"。
func TestSupervisor_MissingBinary(t *testing.T) {
	s := NewSupervisor(SupervisorOptions{
		Bin:        filepath.Join(t.TempDir(), "distribution"),
		ConfigPath: filepath.Join(t.TempDir(), "config.yml"),
		ListenAddr: "127.0.0.1:5011",
	})
	err := s.Start(context.Background())
	if err == nil {
		t.Fatal("二进制不存在时启动期望失败")
	}
	if !strings.Contains(err.Error(), "找不到 distribution 可执行文件") {
		t.Errorf("错误信息应指名找不到可执行文件，实际: %v", err)
	}
}

// TestSupervisor_NotExecutable 覆盖"文件在但不可执行 → 报错"。
func TestSupervisor_NotExecutable(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "distribution")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewSupervisor(SupervisorOptions{
		Bin:        bin,
		ConfigPath: filepath.Join(t.TempDir(), "config.yml"),
		ListenAddr: "127.0.0.1:5011",
	})
	err := s.Start(context.Background())
	if err == nil {
		t.Fatal("文件不可执行时启动期望失败")
	}
	if !strings.Contains(err.Error(), "不可执行") {
		t.Errorf("错误信息应说明不可执行，实际: %v", err)
	}
}

// TestChildEnv 覆盖"默认关掉 OTLP 导出，但不覆盖运维显式设置"。
func TestChildEnv(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "otlp")
	env := childEnv()
	if !hasEnvValue(env, "OTEL_TRACES_EXPORTER", "otlp") {
		t.Error("运维显式设置的 OTEL_TRACES_EXPORTER 不应被覆盖")
	}

	os.Unsetenv("OTEL_TRACES_EXPORTER")
	env = childEnv()
	if !hasEnvValue(env, "OTEL_TRACES_EXPORTER", "none") {
		t.Error("未显式设置时应当默认关掉 OTLP 导出（否则日志被连接错误刷满）")
	}
}

// hasEnvValue 判断环境变量列表里有没有指定键值。
func hasEnvValue(env []string, key, val string) bool {
	for _, e := range env {
		if e == key+"="+val {
			return true
		}
	}
	return false
}
