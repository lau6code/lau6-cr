package service

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/lau6code/lau6-cr/apps/api/internal/config"
	"github.com/lau6code/lau6-cr/apps/api/internal/fsatomic"
)

// 把生效配置写回 application.yaml。
//
// 这一组只依赖 YAML 文件与配置副本，不碰 SettingsManager 的
// mu / snapshot / pending——与设置快照、校验逻辑零耦合。

// exportYAML 把生效配置写回 application.yaml。
//
// 用 yaml.Node 读写以**保留注释与键序**；map 往返会把文件重排、丢掉全部注释。
func (m *SettingsManager) exportYAML(cfg *config.Config) error {
	return m.exportYAMLDoc(cfg, nil, nil)
}

// exportYAMLDoc 把生效配置写回 application.yaml。
//
// database 不为 nil 时**额外**写 database 段（driver / sqlite_path / dsn）。
//
// ⚠️ 它只在向导里换过数据库时才传进来——
// database 段不是托管设置，无条件写会把 env 提供的 DSN
// （**含口令**）抄进文件里。
//
// storage 不为 nil 时**额外**写 storage 段里**不是托管设置**的键
// （本地根目录、S3 凭据、secure）。
//
// ⚠️ 凭据只在向导里填了的时候写：env 提供的 AK/SK 不进文件
//
//	（密钥不该留在备份里）。
func (m *SettingsManager) exportYAMLDoc(
	cfg *config.Config, database *config.DatabaseConfig, storage *storageFileExtra,
) error {
	if m.yamlPath == "" {
		return nil
	}
	// ⚠️ **写之前先备份旧文件**（14-settings.md §5.3）：
	//    这次写入会让旧配置消失，没有备份就再也找不回来。
	//    备份失败必须中止写入——继续写等于让配置**无法回滚**。
	if err := m.backupNow(); err != nil {
		return fmt.Errorf("备份当前配置失败: %w", err)
	}
	doc := &yaml.Node{}
	b, err := os.ReadFile(m.yamlPath)
	if err == nil {
		if err := yaml.Unmarshal(b, doc); err != nil {
			return fmt.Errorf("解析 %s: %w", m.yamlPath, err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if len(doc.Content) == 0 {
		doc = &yaml.Node{Kind: yaml.DocumentNode}
	}

	for _, def := range AllSettingDefs() {
		val := readSetting(cfg, def.Key)
		if def.Secret {
			// 敏感项不导出明文。
			if s, _ := val.(string); s == "" {
				continue
			}
		}
		if err := setYAMLPath(doc, def.YAMLPath, val); err != nil {
			return err
		}
	}

	if database != nil {
		for _, kv := range []struct {
			key string
			val string
		}{
			{"driver", database.Driver},
			{"sqlite_path", database.SQLitePath},
			{"dsn", database.DSN},
		} {
			if err := setYAMLPath(doc, []string{"database", kv.key}, kv.val); err != nil {
				return err
			}
		}
	}

	if storage != nil {
		// secure：由 endpoint 的 scheme 推导（见 applyStorage）。
		if err := setYAMLPath(doc, []string{"storage", "s3", "secure"}, storage.S3Secure); err != nil {
			return err
		}
		// 凭据：只在向导里填了的时候写。
		// 空串**不写**——那会把文件里已有的凭据抹成空，
		// 而"没填"的意思是"用 env 里的那份"，不是"清空"。
		if storage.S3AccessKey != "" {
			if err := setYAMLPath(doc, []string{"storage", "s3", "access_key"}, storage.S3AccessKey); err != nil {
				return err
			}
		}
		if storage.S3SecretKey != "" {
			if err := setYAMLPath(doc, []string{"storage", "s3", "secret_key"}, storage.S3SecretKey); err != nil {
				return err
			}
		}
	}

	out, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	// ⚠️ 原子写：**不先删旧文件**。
	//    POSIX 的 rename 本身就能覆盖，"先删再改名"会在
	//    "删完、rename 失败"之间留一个窗口——配置文件就此丢失。
	//    实现见 internal/fsatomic。
	return fsatomic.Write(m.yamlPath, out, 0o600)
}

func setYAMLPath(doc *yaml.Node, path []string, val any) error {
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		doc.Kind = yaml.DocumentNode
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
	}
	root := doc.Content[0]
	cur := root
	for i, seg := range path {
		if cur.Kind != yaml.MappingNode {
			cur.Kind = yaml.MappingNode
			cur.Tag = "!!map"
		}
		next := findMapValue(cur, seg)
		if next == nil {
			next = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str"}
			cur.Content = append(cur.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: seg}, next)
		}
		if i == len(path)-1 {
			nv := &yaml.Node{}
			if err := nv.Encode(val); err != nil {
				return err
			}
			next.Kind, next.Tag, next.Value, next.Content = nv.Kind, nv.Tag, nv.Value, nv.Content
		}
		cur = next
	}
	return nil
}

func findMapValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// ErrVersionConflict 表示乐观锁冲突。
var ErrVersionConflict = fmt.Errorf("设置已被他人修改")
