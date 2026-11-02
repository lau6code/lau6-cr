package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lau6code/lau6-cr/apps/api/internal/config"
	"github.com/lau6code/lau6-cr/apps/api/internal/objectstore"
)

// storageProbeTimeout 是存储连通性测试的超时。
//
// 15 秒：对象存储要走一次写 + 一次读 + 一次删，
// 跨机房时 5 秒不够用。
const storageProbeTimeout = 15 * time.Second

// storageFileExtra 是要写进 application.yaml 的 storage 附加内容。
//
// ⚠️ 只包含**不是托管设置**的键：托管设置（driver / endpoint /
//
//	bucket / region / path_style）由 exportYAMLDoc 按设置定义统一写，
//	这里再写一遍会出现两套真源。
type storageFileExtra struct {
	// S3AccessKey / S3SecretKey 是向导里填的凭据。
	//
	// ⚠️ **留空就不写**：凭据可能来自环境变量，
	//    把 env 的值抄进文件等于把密钥复制一份到备份里。
	S3AccessKey string
	S3SecretKey string
	// S3Secure 是"用不用 TLS"，由 endpoint 的 scheme 推导。
	S3Secure bool
}

// 返回要写进配置文件的附加内容（凭据等非托管设置）。
//
// ⚠️ **被环境变量钉死的项一律不改**：env 的优先级最高
//
//	（env > system_settings > application.yaml > 默认值），
//	向导改了也是白改——运行期短暂按向导的值走，重启后 env 又盖回去，
//	出现"初始化后关、重启后开"的翻转。
//	被忽略的项记进 SetupResult.IgnoredEnvPinned，由调用方回报给用户。
func (s *SetupService) applyStorage(
	cfg *config.Config, in *StorageInput, guard *envGuard,
) (storageFileExtra, error) {
	extra := storageFileExtra{}

	// 向导给了 driver 才覆盖（CLI 路径与 external 形态不给这一段）。
	//
	// ⚠️ CLI 路径的存储配置来自环境变量 / 配置文件，由 config 自己合成；
	//    这里再赋一遍会把 path_style 之类的布尔值抹成零值。
	wizardGiven := in != nil && strings.TrimSpace(in.Driver) != ""
	if wizardGiven && guard.allow("storage.driver", in.Driver) {
		cfg.Storage.Driver = strings.TrimSpace(in.Driver)
	}

	switch cfg.Storage.Driver {
	case "local":
		// 本地根目录不在向导里（部署的事，11-storage.md §4.1），
		// 用的是当前生效值。这里只验它能不能写——
		// 目录不可写的话，第一次 push 才会炸，而那时候用户已经初始化完了。
		root := strings.TrimSpace(cfg.Storage.Local.Root)
		if root == "" {
			return extra, fmt.Errorf("本地存储根目录不能为空")
		}
		if err := ensureWritableDir(root); err != nil {
			return extra, fmt.Errorf("本地存储根目录 %s 不可用: %w", root, err)
		}
	case "s3":
		if wizardGiven {
			if guard.allow("storage.s3.endpoint", in.S3.Endpoint) && strings.TrimSpace(in.S3.Endpoint) != "" {
				cfg.Storage.S3.Endpoint = strings.TrimSpace(in.S3.Endpoint)
			}
			if guard.allow("storage.s3.bucket", in.S3.Bucket) && strings.TrimSpace(in.S3.Bucket) != "" {
				cfg.Storage.S3.Bucket = strings.TrimSpace(in.S3.Bucket)
			}
			if guard.allow("storage.s3.region", in.S3.Region) && strings.TrimSpace(in.S3.Region) != "" {
				cfg.Storage.S3.Region = strings.TrimSpace(in.S3.Region)
			}
			if guard.allow("storage.s3.path_style", in.S3.PathStyle) {
				cfg.Storage.S3.PathStyle = in.S3.PathStyle
			}
			// 凭据：向导里填了就用向导的；没填就用生效值（可能来自 env）。
			if in.S3.AccessKey != "" {
				cfg.Storage.S3.AccessKey = in.S3.AccessKey
			}
			if in.S3.SecretKey != "" {
				cfg.Storage.S3.SecretKey = in.S3.SecretKey
			}
			extra.S3AccessKey = in.S3.AccessKey
			extra.S3SecretKey = in.S3.SecretKey
		}
		// 校验的是**生效后**的值：被 env 钉死的项用的是 env 里的值，
		// 向导填的非法值不该把它们带偏。
		if strings.TrimSpace(cfg.Storage.S3.Endpoint) == "" ||
			strings.TrimSpace(cfg.Storage.S3.Bucket) == "" {
			return extra, fmt.Errorf("选择对象存储时 endpoint 与 bucket 必填")
		}
		if strings.TrimSpace(cfg.Storage.S3.Region) == "" {
			cfg.Storage.S3.Region = "us-east-1"
		}
		// secure 由 endpoint 的 scheme 推导：让用户再单独填一次
		// 就是两处可能打架的配置（"http:// 的地址配了 secure: true"）。
		cfg.Storage.S3.Secure = objectstore.SecureFromEndpoint(cfg.Storage.S3.Endpoint, cfg.Storage.S3.Secure)
		extra.S3Secure = cfg.Storage.S3.Secure
	default:
		return extra, fmt.Errorf("未知的存储后端: %q", cfg.Storage.Driver)
	}
	return extra, nil
}

// StorageInfo 是当前**生效**的存储配置（供向导预填）。
//
// ⚠️ **不含凭据**：setup 阶段这个端点是开放面（那时还没有任何账号），
// 密钥绝不能回显。只给"有没有配"（access_key_set / secret_key_set）。
type StorageInfo struct {
	Driver string `json:"driver"`
	Local  struct {
		Root string `json:"root"`
	} `json:"local"`
	S3 struct {
		Endpoint  string `json:"endpoint"`
		Region    string `json:"region"`
		Bucket    string `json:"bucket"`
		PathStyle bool   `json:"path_style"`
		// AccessKeySet / SecretKeySet 只表示"配没配"，不表示配的是什么。
		AccessKeySet bool `json:"access_key_set"`
		SecretKeySet bool `json:"secret_key_set"`
	} `json:"s3"`
}

// ⚠️ "只在未初始化时可用"由 handler 负责（它检查 initialized 标记）。
func (s *SetupService) StorageInfo(ctx context.Context) (*StorageInfo, error) {
	cfg := s.settings.Snapshot()
	info := &StorageInfo{Driver: cfg.Storage.Driver}
	info.Local.Root = cfg.Storage.Local.Root
	info.S3.Endpoint = cfg.Storage.S3.Endpoint
	info.S3.Region = cfg.Storage.S3.Region
	info.S3.Bucket = cfg.Storage.S3.Bucket
	info.S3.PathStyle = cfg.Storage.S3.PathStyle
	info.S3.AccessKeySet = cfg.Storage.S3.AccessKey != ""
	info.S3.SecretKeySet = cfg.Storage.S3.SecretKey != ""
	return info, nil
}

// TestStorage 测存储后端能不能真的用。
//
// local：建目录 → 写探针文件 → 读回 → 删掉。
// s3：写探针对象 → 读回比对 → 删除（见 internal/objectstore）。
//
// ⚠️ 探测失败**不阻断**向导：setup 所在的管理网可能访问不到对象存储，
//
//	而运行期网络是通的（11-storage.md §4.4）。所以 handler 把错误
//	报给前端，由前端弹确认框问用户"仍要继续吗"。
func (s *SetupService) TestStorage(ctx context.Context, in StorageTestInput) (*StorageTestResult, error) {
	if err := s.requireSetupPhase(ctx, in.BootstrapToken); err != nil {
		return nil, err
	}
	driver := strings.TrimSpace(in.Storage.Driver)
	if driver == "" {
		return nil, fmt.Errorf("%w: 没有选择存储后端", ErrInvalidProbeParam)
	}

	ctx, cancel := context.WithTimeout(ctx, storageProbeTimeout)
	defer cancel()
	start := time.Now()

	switch driver {
	case "local":
		// 本地根目录不在向导里（11-storage.md §4.1），用当前生效值。
		root := strings.TrimSpace(s.settings.Snapshot().Storage.Local.Root)
		if root == "" {
			return nil, fmt.Errorf("%w: 本地存储根目录不能为空", ErrInvalidProbeParam)
		}
		if err := probeLocalDir(root); err != nil {
			return nil, err
		}
		return &StorageTestResult{
			Detail:    "本地目录可写：" + root,
			LatencyMS: time.Since(start).Milliseconds(),
		}, nil
	case "s3":
		endpoint := strings.TrimSpace(in.Storage.S3.Endpoint)
		bucket := strings.TrimSpace(in.Storage.S3.Bucket)
		if endpoint == "" || bucket == "" {
			return nil, fmt.Errorf("%w: 对象存储的 endpoint 与 bucket 必填", ErrInvalidProbeParam)
		}
		region := strings.TrimSpace(in.Storage.S3.Region)
		if region == "" {
			region = "us-east-1"
		}
		accessKey := in.Storage.S3.AccessKey
		secretKey := in.Storage.S3.SecretKey
		if accessKey == "" || secretKey == "" {
			// 凭据留空 = 走环境变量 / 已有配置文件。
			// 这里取**当前生效**的值（env > system_settings > 文件 > 默认值），
			// 否则会拿空凭据去连，报出一堆看不懂的错误。
			current := s.settings.Snapshot().Storage.S3
			accessKey = current.AccessKey
			secretKey = current.SecretKey
		}
		if accessKey == "" || secretKey == "" {
			return nil, fmt.Errorf("%w: 缺少对象存储凭据："+
				"请填写 Access Key / Secret Key，或用环境变量 CR_S3_ACCESS_KEY / CR_S3_SECRET_KEY",
				ErrInvalidProbeParam)
		}
		elapsed, err := objectstore.ProbeS3(ctx, objectstore.S3Config{
			Endpoint:  endpoint,
			Region:    region,
			Bucket:    bucket,
			AccessKey: accessKey,
			SecretKey: secretKey,
			PathStyle: in.Storage.S3.PathStyle,
			Secure: objectstore.SecureFromEndpoint(
				endpoint, s.settings.Snapshot().Storage.S3.Secure),
		})
		if err != nil {
			return nil, err
		}
		return &StorageTestResult{
			Detail:    "对象存储可用",
			LatencyMS: elapsed.Milliseconds(),
		}, nil
	default:
		return nil, fmt.Errorf("%w: 未知的存储后端 %q", ErrInvalidProbeParam, driver)
	}
}

// probeLocalDir 验证目录能写：建目录 → 写探针 → 读回 → 删掉。
func probeLocalDir(root string) (err error) {
	if err := ensureWritableDir(root); err != nil {
		return err
	}
	probe := filepath.Join(root, ".lau6-cr-probe")
	if err := os.WriteFile(probe, []byte("probe"), 0o600); err != nil {
		return fmt.Errorf("写入探针文件失败: %w", err)
	}
	// 删不掉也要报错：残留的探针文件躺在存储根目录里，
	// 用户会以为是镜像文件，GC 也可能把它扫进来。
	defer func() {
		if rmErr := os.Remove(probe); rmErr != nil && err == nil {
			err = fmt.Errorf("清理探针文件失败: %w", rmErr)
		}
	}()

	got, readErr := os.ReadFile(probe)
	if readErr != nil {
		return fmt.Errorf("读回探针文件失败: %w", readErr)
	}
	if string(got) != "probe" {
		return fmt.Errorf("读回内容不一致")
	}
	return nil
}

// ensureWritableDir 确保目录存在且可写。
//
// 目录不存在就建——setup 阶段容器里的 /data/registry 可能还没建出来，
// 直接判"不可用"会把人逼到手动建目录。
func ensureWritableDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建目录失败: %w%s", err, mountGoneHint(err, filepath.Dir(dir)))
	}
	probe := filepath.Join(dir, ".lau6-cr-write-probe")
	if err := os.WriteFile(probe, []byte("probe"), 0o600); err != nil {
		return fmt.Errorf("目录不可写: %w%s", err, mountGoneHint(err, filepath.Dir(dir)))
	}
	// 删不掉也要报错：残留的探针文件会让后续的"目录是否干净"判断失真。
	if err := os.Remove(probe); err != nil {
		return fmt.Errorf("清理探针文件失败: %w", err)
	}
	return nil
}

// mountGoneHint 判断这次失败是不是"容器挂载源被删了"，是的话给一句能照做的提示。
//
// 症状：**父目录还在**（stat 得到目录），但在里面建东西报 ENOENT。
// 那不是路径写错，是**死挂载**——宿主的挂载源目录被删了，容器里的挂载点还在，
// 但已经写不进去（mkdir / 写文件一律 ENOENT）。这种时候 MkdirAll 救不回来，
// 只能把宿主目录补回来再重建容器。
//
// ⚠️ 认不出来就返回空串——不猜。宁可只给原始报错，也不要给一个错的结论。
func mountGoneHint(err error, parent string) string {
	if !errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	st, statErr := os.Stat(parent)
	if statErr != nil || !st.IsDir() {
		// 父目录本身就没了 → 普通的路径问题，按原始报错说。
		return ""
	}
	return fmt.Sprintf(
		"（父目录 %s 还在，但里面建不了东西——多半是容器挂载源被删了：把宿主上的目录补回来并重建容器即可）",
		parent,
	)
}
