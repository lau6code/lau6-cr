// Package distribution 负责内嵌 distribution（仓库内核）的配置渲染与进程管理。
//
// 两种部署形态（config.RegistryConfig.Mode）：
//
//   - embedded（本包的职责）：distribution 与 lau6-cr **同容器**，
//     lau6-cr 自己把 distribution 拉起来。两者共享同一个数据目录，
//     所以 blob 回收（GC）能直接扫盘。
//   - external：distribution 独立部署，lau6-cr 只通过
//     registry.upstream_addr 调用它——本包不参与。
//
// 配置**不烘焙在镜像里**——它含明文密钥，且 storage 块要在运行时按
// local / S3 二选一。渲染产物放 /run（tmpfs），每次启动重新生成。
package distribution

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/lau6code/lau6-cr/apps/api/internal/config"
)

// FilesystemStorage 是 distribution 配置里的 storage.filesystem 段。
type FilesystemStorage struct {
	Rootdirectory string `yaml:"rootdirectory"`
}

// S3Storage 是 distribution 配置里的 storage.s3 段。
//
// ⚠️ 键名必须与 distribution 3.x 的 s3 驱动一致
//
//	（registry/storage/driver/s3-aws/s3.go 读的是
//	`regionendpoint` / `forcepathstyle` / `rootdirectory`）。
//
// 驱动**不校验未知键**——键名写错会被静默忽略，
// 于是 endpoint 退回 AWS 默认地址、bucket 落到桶根，
// 现象是"配置看着对、blob 却存错地方"。
type S3Storage struct {
	AccessKey string `yaml:"accesskey"`
	SecretKey string `yaml:"secretkey"`
	Bucket    string `yaml:"bucket"`
	Region    string `yaml:"region"`
	// Endpoint 写的是 distribution 的 `regionendpoint`。
	Endpoint string `yaml:"regionendpoint"`
	// PathStyle 写的是 `forcepathstyle`。
	PathStyle bool `yaml:"forcepathstyle"`
	Secure    bool `yaml:"secure"`
	// RootPath 留空 = 桶根。
	//
	// ⚠️ 别在这里填 "docker/registry/v2"：distribution 自己的内部路径
	//    已经带了这一层（registry/storage/paths.go 的 storagePathRoot），
	//    而 s3 驱动的对象键 = RootDirectory + 内部路径
	//    （s3-aws/s3.go 的 s3Path）——
	//    填了会变成 docker/registry/v2/docker/registry/v2/...，**多一层**。
	//
	//    留空时对象键就是 docker/registry/v2/...，
	//    与本地形态的 <root>/docker/registry/v2/... 一一对应，
	//    也是 distribution 3.x 文档里的默认值（3.x docs: rootdirectory 默认空串 = 桶根）。
	RootPath string `yaml:"rootdirectory,omitempty"`
}

// DistributionConfig 是 distribution 的配置结构。
//
// 只声明需要动态生成的部分，其余走 distribution 自己的默认值。
type DistributionConfig struct {
	Version string `yaml:"version"`
	Log     struct {
		Level     string `yaml:"level"`
		Formatter string `yaml:"formatter"`
	} `yaml:"log"`
	Storage struct {
		Filesystem *FilesystemStorage `yaml:"filesystem,omitempty"`
		S3         *S3Storage         `yaml:"s3,omitempty"`
		// Delete 段是 **storage 级别**的开关（与 filesystem / s3 平级），
		// distribution 3.x 读它决定"能不能删"：
		// registry/handlers/app.go 里
		// `if d, ok := config.Storage["delete"]; ok { ... }`
		// 读到 enabled=true 才给 app 挂上 storage.EnableDelete；
		// 没挂上时 manifest / tag 的 DELETE 一律回 **405 unsupported**。
		//
		// ⚠️ 别删这个键：症状是"删仓库永远失败、报上游 405"，
		//    而且**不报错日志**（405 是正常响应）——极难定位。
		//    键名要写在 storage 下，**不能**写进 s3 里（那个没人读）。
		Delete struct {
			Enabled bool `yaml:"enabled"`
		} `yaml:"delete"`
		Cache struct {
			BlobDescriptor string `yaml:"blobdescriptor"`
		} `yaml:"cache"`
		Maintenance struct {
			UploadPurging struct {
				Enabled  bool   `yaml:"enabled"`
				Age      string `yaml:"age"`
				Interval string `yaml:"interval"`
				DryRun   bool   `yaml:"dryrun"`
			} `yaml:"uploadpurging"`
		} `yaml:"maintenance"`
	} `yaml:"storage"`
	HTTP struct {
		Addr    string `yaml:"addr"`
		Secret  string `yaml:"secret"`
		Headers struct {
			XContentTypeOptions []string `yaml:"X-Content-Type-Options"`
		} `yaml:"headers"`
		Debug struct {
			Addr       string `yaml:"addr"`
			Prometheus struct {
				Enabled bool   `yaml:"enabled"`
				Path    string `yaml:"path"`
			} `yaml:"prometheus,omitempty"`
		} `yaml:"debug,omitempty"`
	} `yaml:"http"`
	Auth struct {
		Token struct {
			Realm             string   `yaml:"realm"`
			Service           string   `yaml:"service"`
			Issuer            string   `yaml:"issuer"`
			JWKS              string   `yaml:"jwks"`
			SigningAlgorithms []string `yaml:"signingalgorithms"`
		} `yaml:"token"`
	} `yaml:"auth"`
	Notifications struct {
		Endpoints []NotificationEndpoint `yaml:"endpoints"`
	} `yaml:"notifications"`
}

// NotificationEndpoint 是 distribution 的事件回调端点。
type NotificationEndpoint struct {
	Name      string              `yaml:"name"`
	URL       string              `yaml:"url"`
	Headers   map[string][]string `yaml:"headers"`
	Timeout   string              `yaml:"timeout"`
	Threshold int                 `yaml:"threshold"`
	Backoff   string              `yaml:"backoff"`
	Events    []string            `yaml:"events"`
}

// RenderInputs 是渲染所需的运行时输入。
type RenderInputs struct {
	Cfg config.Config
	// JWKSPath 是 JWKS 文件在容器内的路径。
	JWKSPath string
	// RegistrySecret 是 distribution 的 http.secret。
	RegistrySecret string
	// WebhookSecret 是事件回调的共享密钥。
	WebhookSecret string
	// CallbackBaseURL 是 lau6-cr 自身的基地址
	//（事件回调的目标，distribution 主动打过来）。
	//
	// 内嵌模式下它是回环地址（同容器）。
	CallbackBaseURL string
	// ListenAddr 是 distribution 自己的监听地址（host:port，不带 scheme）。
	//
	// ⚠️ 它必须与 config.RegistryConfig.DistributionAddr 一致——
	//    lau6-cr 调用上游用的就是那个地址，两边不一致 = 连不上。
	ListenAddr string
	// DebugAddr 是 distribution 的 debug/metrics 监听地址（host:port）。
	//
	// 留空 = 不开 debug server（也就没有 /metrics）——
	// 没有消费方时不该多开一个端口（还要防它跟别的监听撞端口）。
	DebugAddr string
	// LogLevel 是 distribution 的日志级别。
	LogLevel string
}

// Render 生成 distribution 的配置文件内容。
func Render(in RenderInputs) ([]byte, error) {
	if strings.TrimSpace(in.ListenAddr) == "" {
		return nil, fmt.Errorf("distribution 监听地址不能为空")
	}
	if strings.TrimSpace(in.CallbackBaseURL) == "" {
		return nil, fmt.Errorf("distribution 事件回调地址不能为空")
	}
	var c DistributionConfig
	c.Version = "0.1"
	c.Log.Level = in.LogLevel
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
	c.Log.Formatter = "text"

	switch in.Cfg.Storage.Driver {
	case "local":
		c.Storage.Filesystem = &FilesystemStorage{Rootdirectory: in.Cfg.Storage.Local.Root}
	case "s3":
		c.Storage.S3 = &S3Storage{
			AccessKey: in.Cfg.Storage.S3.AccessKey,
			SecretKey: in.Cfg.Storage.S3.SecretKey,
			Bucket:    in.Cfg.Storage.S3.Bucket,
			Region:    in.Cfg.Storage.S3.Region,
			Endpoint:  in.Cfg.Storage.S3.Endpoint,
			PathStyle: in.Cfg.Storage.S3.PathStyle,
			Secure:    in.Cfg.Storage.S3.Secure,
			// RootPath 不设 = 桶根（见结构体里的说明）。
		}
	default:
		return nil, fmt.Errorf("未知的 storage.driver: %q", in.Cfg.Storage.Driver)
	}

	// ⚠️ 必须开：distribution 3.x 用它决定"能不能删"。
	//    没开的话 manifest / tag 的 DELETE 一律 405 unsupported
	//    （registry/handlers/app.go → app.deleteEnabled）。
	c.Storage.Delete.Enabled = true
	c.Storage.Cache.BlobDescriptor = "inmemory"
	c.Storage.Maintenance.UploadPurging.Enabled = true
	c.Storage.Maintenance.UploadPurging.Age = "168h"
	c.Storage.Maintenance.UploadPurging.Interval = "24h"
	c.Storage.Maintenance.UploadPurging.DryRun = false

	c.HTTP.Addr = in.ListenAddr
	c.HTTP.Secret = in.RegistrySecret
	c.HTTP.Headers.XContentTypeOptions = []string{"nosniff"}
	// debug server 是**另一个**监听端口。
	//
	// ⚠️ 地址由配置给（config.RegistryConfig.DistributionDebugAddr），
	//    **不**在这里推导（比如"监听端口 + 1"）——
	//    推导出来的端口可能正好撞上 lau6-cr 自己的监听端口，
	//    症状是 lau6-cr 报 "address already in use" 然后退出，
	//    而日志里 distribution 明明"已就绪"。
	//    端口冲突由 config.ValidateRegistry 在启动前挡住。
	if strings.TrimSpace(in.DebugAddr) != "" {
		c.HTTP.Debug.Addr = in.DebugAddr
		c.HTTP.Debug.Prometheus.Enabled = true
		c.HTTP.Debug.Prometheus.Path = "/metrics"
	}

	c.Auth.Token.Realm = strings.TrimRight(in.Cfg.Core.PublicURL, "/") + "/token"
	c.Auth.Token.Service = in.Cfg.Token.Audience
	c.Auth.Token.Issuer = in.Cfg.Token.Issuer
	c.Auth.Token.JWKS = in.JWKSPath
	c.Auth.Token.SigningAlgorithms = []string{"EdDSA"}

	c.Notifications.Endpoints = []NotificationEndpoint{{
		Name:      "lau6-cr-cache",
		URL:       strings.TrimRight(in.CallbackBaseURL, "/") + "/api/internal/registry-events",
		Headers:   map[string][]string{"Authorization": {"Bearer " + in.WebhookSecret}},
		Timeout:   "5s",
		Threshold: 10,
		Backoff:   "1s",
		Events:    []string{"push", "pull", "delete", "mount"},
	}}

	out, err := yaml.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("序列化 distribution 配置: %w", err)
	}
	return out, nil
}
