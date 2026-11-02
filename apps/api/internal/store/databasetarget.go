package store

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
)

// 各驱动的默认端口。
const (
	DefaultPostgresPort = "5432"
	DefaultMySQLPort    = "3306"
)

// DatabaseTarget 是一次数据库连接的目标——初始化向导里那几个输入框。
//
// 它是"结构化"的连接描述：host / port / 用户名 / 口令 / 库名 / schema。
// 拼成 DSN 的活儿交给 DSN()，反解成字段的活儿交给 ParseDatabaseTarget。
//
// 为什么不让前端直接拼 DSN：
//
//   - postgres 走 URL（口令要百分号转义），mysql 走 `user:pass@tcp(host:port)/db`
//     （口令**不能**转义）——两套规则写在前端就是两处坑；
//   - schema 在 postgres 里是 search_path，在 mysql 里根本不存在（库名即 schema）。
//
// 拼与解都放在有驱动依赖的 store 包里，用驱动自己的解析器保证往返一致。
type DatabaseTarget struct {
	Driver   string `json:"driver"`
	Host     string `json:"host"`
	Port     string `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
	// Name 是数据库名（postgres 的 dbname / mysql 的 database）。
	Name string `json:"name"`
	// Schema 只对 postgres 有意义（search_path）；mysql 里数据库名即 schema。
	Schema string `json:"schema"`
	// SSLMode 是 postgres 的 sslmode，默认 disable。
	SSLMode string `json:"ssl_mode"`
	// SQLitePath 只对 sqlite 有意义（数据文件路径）。
	SQLitePath string `json:"sqlite_path"`
}

// Normalize 归一化并校验。
//
// 校验的是"能不能拼出一条像样的连接串"，不是"能不能连上"——
// 连通性由 Probe 负责（见 store.Probe）。
func (t *DatabaseTarget) Normalize() error {
	t.Driver = strings.ToLower(strings.TrimSpace(t.Driver))
	t.Host = strings.TrimSpace(t.Host)
	t.Port = strings.TrimSpace(t.Port)
	t.Username = strings.TrimSpace(t.Username)
	t.Name = strings.TrimSpace(t.Name)
	t.Schema = strings.TrimSpace(t.Schema)
	t.SSLMode = strings.TrimSpace(t.SSLMode)
	t.SQLitePath = strings.TrimSpace(t.SQLitePath)

	switch t.Driver {
	case "sqlite":
		if t.SQLitePath == "" {
			return fmt.Errorf("SQLite 数据文件路径不能为空")
		}
	case "postgres":
		if t.Host == "" {
			return fmt.Errorf("数据库主机不能为空")
		}
		if t.Port == "" {
			t.Port = DefaultPostgresPort
		}
		if t.Username == "" {
			return fmt.Errorf("数据库用户名不能为空")
		}
		if t.Name == "" {
			return fmt.Errorf("数据库名不能为空")
		}
		if t.SSLMode == "" {
			t.SSLMode = "disable"
		}
	case "mysql":
		if t.Host == "" {
			return fmt.Errorf("数据库主机不能为空")
		}
		if t.Port == "" {
			t.Port = DefaultMySQLPort
		}
		if t.Username == "" {
			return fmt.Errorf("数据库用户名不能为空")
		}
		if t.Name == "" {
			return fmt.Errorf("数据库名不能为空")
		}
	default:
		return fmt.Errorf("不支持的数据库驱动: %q", t.Driver)
	}
	return nil
}

// DSN 拼出连接串。
//
// ⚠️ sqlite 不在这里拼：它的 DSN 带 PRAGMA（见 config.SQLiteDSN），
// 由 config 层负责——两处各拼一半最容易出现"测的时候好好的、起来就报错"。
func (t DatabaseTarget) DSN() (string, error) {
	switch t.Driver {
	case "postgres":
		u := &url.URL{
			Scheme: "postgres",
			Host:   net.JoinHostPort(t.Host, t.Port),
			Path:   "/" + t.Name,
		}
		// url.UserPassword 会把口令里的 @ : / 等字符百分号转义——
		// pgx 用 URL 解析，**必须**转义，否则含 @ 的口令会被截断。
		if t.Password == "" {
			u.User = url.User(t.Username)
		} else {
			u.User = url.UserPassword(t.Username, t.Password)
		}
		q := url.Values{}
		if t.SSLMode != "" {
			q.Set("sslmode", t.SSLMode)
		}
		if t.Schema != "" {
			// pgx 会把不认识的 query 参数原样交给服务端当 runtime param，
			// search_path 正是其中之一。
			q.Set("search_path", t.Schema)
		}
		u.RawQuery = q.Encode()
		return u.String(), nil
	case "mysql":
		// ⚠️ 口令**不转义**：go-sql-driver 的 DSN 语法是
		//    user:pass@tcp(host:port)/dbname
		//    它按"最后一个 @"和"最后一个 /"切分，口令里的 @ 与 / 都能正确还原；
		//    但它**不会**解转义序列——转义反而会把口令改坏。
		//    库名要转义（驱动会对它做 PathUnescape）。
		return fmt.Sprintf("%s:%s@tcp(%s)/%s?parseTime=true&charset=utf8mb4",
			t.Username, t.Password, net.JoinHostPort(t.Host, t.Port),
			url.PathEscape(t.Name)), nil
	case "sqlite":
		return "", fmt.Errorf("sqlite 的 DSN 由 config.SQLiteDSN 拼，不走这里")
	default:
		return "", fmt.Errorf("不支持的数据库驱动: %q", t.Driver)
	}
}

// ParseDatabaseTarget 从生效的 DSN 反解出各个字段——用于向导预填。
//
// ⚠️ 口令会原样解出来，但**只应留在服务端**：
// 返回给前端前必须抹掉（见 server 层的数据库预填端点）。
//
// 反解不出来时返回错误，不返回"半截"结果：
// 半截的预填比没有预填更糟——用户会以为那就是真实配置。
func ParseDatabaseTarget(driver, dsn, sqlitePath string) (DatabaseTarget, error) {
	t := DatabaseTarget{Driver: driver, SQLitePath: sqlitePath}
	switch driver {
	case "sqlite":
		if sqlitePath == "" {
			return t, fmt.Errorf("SQLite 数据文件路径为空")
		}
		return t, nil
	case "postgres":
		cfg, err := pgconn.ParseConfig(dsn)
		if err != nil {
			return t, fmt.Errorf("解析 postgres DSN: %w", err)
		}
		host := cfg.Host
		if host == "" {
			return t, fmt.Errorf("postgres DSN 里没有主机")
		}
		// 多主机 DSN（host1,host2）只取第一个——表单只能填一个。
		if i := strings.Index(host, ","); i >= 0 {
			host = host[:i]
		}
		t.Host = host
		t.Port = strconv.Itoa(int(cfg.Port))
		t.Username = cfg.User
		t.Password = cfg.Password
		t.Name = cfg.Database
		t.Schema = cfg.RuntimeParams["search_path"]
		t.SSLMode = "disable"
		return t, nil
	case "mysql":
		cfg, err := mysql.ParseDSN(dsn)
		if err != nil {
			return t, fmt.Errorf("解析 mysql DSN: %w", err)
		}
		host, port, err := net.SplitHostPort(cfg.Addr)
		if err != nil {
			// 没写端口时驱动默认 3306，Addr 可能就是 "host"。
			host = cfg.Addr
			port = DefaultMySQLPort
		}
		t.Host = host
		t.Port = port
		t.Username = cfg.User
		t.Password = cfg.Passwd
		t.Name = cfg.DBName
		return t, nil
	default:
		return t, fmt.Errorf("不支持的数据库驱动: %q", driver)
	}
}
