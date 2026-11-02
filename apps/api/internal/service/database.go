package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/lau6code/lau6-cr/apps/api/internal/config"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// DatabaseInfo 是数据库步骤要展示与预填的信息。
//
// ⚠️ **不含口令**：预填端点在未初始化时是开放面（那时还没有任何账号），
// 口令绝不能回显。用户自己填的口令只用来拼 DSN 与试连。
type DatabaseInfo struct {
	Driver     string `json:"driver"`
	Host       string `json:"host"`
	Port       string `json:"port"`
	Username   string `json:"username"`
	Name       string `json:"name"`
	Schema     string `json:"schema"`
	SSLMode    string `json:"ssl_mode"`
	SQLitePath string `json:"sqlite_path"`
	// EnvVar 非空 = 数据库配置被环境变量钉死。
	//
	// env 的优先级最高：被钉死时向导里改了也留不住（重启就没了），
	// 所以 UI 要把它显示成只读，不能让人以为改了会生效。
	EnvVar string `json:"env_var"`
}

// databaseResolution 是向导里填的数据库目标解析结果。
type databaseResolution struct {
	target store.DatabaseTarget
	dsn    string
	// PasswordSource 说明口令的来源：
	//   - "input"   —— 用了输入框里填的口令；
	//   - "current" —— 输入框留空，沿用了当前生效配置里的口令。
	//
	// 它会被回显在测试结果里——**沿用**是替用户做的决定，必须说清楚。
	PasswordSource string
}

// probeSchema 返回"应当生效的 schema"，用于探测时校验它真的存在。
//
//   - postgres：search_path 里的那个 schema；
//   - mysql：库名（mysql 里库名即 schema）；
//   - sqlite：空串。
func (r databaseResolution) probeSchema() string {
	switch r.target.Driver {
	case "postgres":
		return r.target.Schema
	case "mysql":
		return r.target.Name
	default:
		return ""
	}
}

// DatabaseInfo 返回当前生效的数据库连接信息（**不含口令**）。
func (s *SetupService) DatabaseInfo(ctx context.Context) (*DatabaseInfo, error) {
	cfg := s.settings.Snapshot()
	info := &DatabaseInfo{
		Driver: cfg.Database.Driver,
		EnvVar: cfg.DatabaseEnvVar(),
	}
	if cfg.Database.Driver == "sqlite" {
		info.SQLitePath = cfg.Database.SQLitePath
		return info, nil
	}
	target, err := store.ParseDatabaseTarget(cfg.Database.Driver, cfg.Database.DSN, "")
	if err != nil {
		// 反解不出来就**报错**，不返回半截信息——
		// 半截的预填比没有预填更糟：用户会以为那就是真实配置。
		return nil, err
	}
	info.Host = target.Host
	info.Port = target.Port
	info.Username = target.Username
	info.Name = target.Name
	info.Schema = target.Schema
	info.SSLMode = target.SSLMode
	return info, nil
}

// currentDatabaseTarget 反解当前生效的数据库目标（含口令）。
func (s *SetupService) currentDatabaseTarget() (store.DatabaseTarget, error) {
	cfg := s.settings.Snapshot()
	return store.ParseDatabaseTarget(cfg.Database.Driver, cfg.Database.DSN, cfg.Database.SQLitePath)
}

func (s *SetupService) sqliteDSN(path string) string {
	cfg := *s.settings.Snapshot()
	cfg.Database.SQLitePath = path
	return cfg.SQLiteDSN()
}

// currentDSN 返回当前生效的连接串。
func (s *SetupService) currentDSN() string {
	cfg := s.settings.Snapshot()
	if cfg.Database.Driver == "sqlite" {
		return s.sqliteDSN(cfg.Database.SQLitePath)
	}
	return cfg.Database.DSN
}

// resolveDatabaseTarget 解析向导里填的数据库目标。
//
// 返回 ok=false 表示"向导没填数据库信息" → 保持当前配置不动。
//
// 口令留空的语义：**沿用当前生效配置里的口令**（仅当指向同一个库时）。
// 这不是兜底——沿用的事实会写进测试结果与提示语，用户看得见。
func (s *SetupService) resolveDatabaseTarget(in DatabaseInput) (databaseResolution, bool, error) {
	driver := strings.ToLower(strings.TrimSpace(in.Driver))
	switch driver {
	case "sqlite":
		path := strings.TrimSpace(in.SQLitePath)
		if path == "" {
			return databaseResolution{}, false, nil
		}
		return databaseResolution{
			target:         store.DatabaseTarget{Driver: "sqlite", SQLitePath: path},
			dsn:            s.sqliteDSN(path),
			PasswordSource: "input",
		}, true, nil

	case "postgres", "mysql":
		target := store.DatabaseTarget{
			Driver:   driver,
			Host:     in.Host,
			Port:     in.Port,
			Username: in.Username,
			Password: in.Password,
			Name:     in.Name,
			Schema:   in.Schema,
			SSLMode:  in.SSLMode,
		}
		if err := target.Normalize(); err != nil {
			return databaseResolution{}, false, fmt.Errorf("%w: %s", ErrInvalidProbeParam, err.Error())
		}
		source := "input"
		if target.Password == "" {
			// 口令留空 → 沿用当前生效配置里的口令（仅限同一个库）。
			//
			// ⚠️ 反解失败**必须报出来**：
			//    吞掉它的话口令会留空，后续探测只会呈现为"认证失败"，
			//    真实原因（当前生效的 DSN 解析不了）被彻底掩盖——
			//    运维会去查口令，而问题在连接串本身（AGENTS.md 规则 1）。
			//
			//    这不是用户填错了参数，是**服务端自己的配置坏了**，
			//    所以用独立哨兵跟 ErrInvalidProbeParam 分开（见 ErrCurrentDatabaseUnreadable）。
			cur, err := s.currentDatabaseTarget()
			if err != nil {
				return databaseResolution{}, false, fmt.Errorf("%w: %w", ErrCurrentDatabaseUnreadable, err)
			}
			if sameDatabaseEndpoint(cur, target) {
				target.Password = cur.Password
				source = "current"
			}
		}
		dsn, err := target.DSN()
		if err != nil {
			return databaseResolution{}, false, fmt.Errorf("%w: %s", ErrInvalidProbeParam, err.Error())
		}
		return databaseResolution{target: target, dsn: dsn, PasswordSource: source}, true, nil

	default:
		// 没选驱动 → 视为"不改数据库"。
		if strings.TrimSpace(in.Driver) == "" {
			return databaseResolution{}, false, nil
		}
		return databaseResolution{}, false, fmt.Errorf("%w: 不支持的数据库驱动 %q", ErrInvalidProbeParam, driver)
	}
}

// sameDatabaseEndpoint 判断两个目标指向同一个库（忽略口令）。
func sameDatabaseEndpoint(a, b store.DatabaseTarget) bool {
	if a.Driver != b.Driver {
		return false
	}
	if a.Driver == "sqlite" {
		return a.SQLitePath == b.SQLitePath
	}
	return a.Host == b.Host && a.Port == b.Port &&
		a.Username == b.Username && a.Name == b.Name && a.Schema == b.Schema
}

// switchDatabase 把运行期数据库切到目标。
//
// 返回 switched=false 表示"目标与当前一致"，不需要切。
//
// ⚠️ 顺序：**先切库，再建管理员**——
// 管理员、初始化标记、引导令牌摘要必须落在**新库**里。
// 反过来（先建后切）会留下"标记在旧库、业务在新库"的分裂状态：
// 新库里没有 system.initialized → 实例变回未初始化 → /setup 重新开放。
func (s *SetupService) switchDatabase(ctx context.Context, res databaseResolution) (bool, error) {
	cfg := s.settings.Snapshot()
	if res.target.Driver == cfg.Database.Driver && res.dsn == s.currentDSN() {
		// 目标就是当前库：不换连接，但**必须补跑迁移**。
		//
		// ⚠️ 为什么不能直接跳过：当前库可能是**半成品**——
		//    上一次初始化中途失败（表建了一半 / 迁移没记版本），
		//    用户再点一次初始化时目标与当前一致，
		//    这里若直接返回，向导就永远补不上那些表，
		//    用户会被"relation does not exist"永久堵死。
		//
		// 迁移脚本是幂等的，补跑安全；补不上的情况会明确报错。
		if err := s.db.Migrate(ctx); err != nil {
			return false, fmt.Errorf("迁移当前数据库: %w", err)
		}
		return false, nil
	}
	if err := s.db.SwitchTo(ctx, res.target.Driver, res.dsn); err != nil {
		return false, fmt.Errorf("切换数据库: %w", err)
	}
	return true, nil
}

// databaseSection 返回要写进 application.yaml 的 database 段。
//
// nil = 不写（数据库没换，或被 env 钉死——env 优先级最高，
// 写进去也只是看着像生效了，重启就没了）。
func (s *SetupService) databaseSection(res databaseResolution, switched bool) *config.DatabaseConfig {
	if !switched {
		return nil
	}
	section := &config.DatabaseConfig{
		Driver:     res.target.Driver,
		SQLitePath: res.target.SQLitePath,
	}
	if res.target.Driver != "sqlite" {
		// sqlite 的连接串由 sqlite_path + PRAGMA 拼出来（config.SQLiteDSN），
		// dsn 字段对 sqlite 没有意义——写进去只会让人以为配置是那样用的。
		section.DSN = res.dsn
	}
	return section
}
