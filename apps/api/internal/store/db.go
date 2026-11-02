package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

//go:embed migrations/sqlite/*.sql migrations/postgres/*.sql migrations/mysql/*.sql
var migrationFS embed.FS

// handle 是一个数据库连接池 + 它的方言。
type handle struct {
	sqlDB   *sql.DB
	dialect Dialect
}

// DB 是带方言信息的数据库句柄，支持**运行期换库**。
//
// 换库是**指针级**的：所有持有 *DB 的服务（鉴权、权限、设置、注册表……）
// 都通过它访问数据库，换库后自动改用新连接——不需要给每个服务发通知。
//
// 换库入口是 SwitchTo：开新连接 → 跑迁移 → 原子替换 → 旧连接延迟关闭。
type DB struct {
	cur atomic.Pointer[handle]

	// mu 保护 retired（被换下的旧连接池）。
	mu      sync.Mutex
	retired []*sql.DB
}

// retireGrace 是旧连接池的宽限期。
//
// ⚠️ 不能立刻关：换库瞬间可能有在途查询/事务还在用旧连接，
// 立刻 Close 会让它们报 "sql: database is closed"。
const retireGrace = 30 * time.Second

// Tx 是带方言信息的事务句柄。
//
// ⚠️ 用它而不是裸 *sql.Tx：sql.Tx **不会**自动 Rebind——
//
//	PostgreSQL 需要 $1 形式的占位符，裸 ? 会直接语法报错。
type Tx struct {
	*sql.Tx
	Dialect Dialect
}

// current 返回当前生效的连接池。
func (d *DB) current() *handle { return d.cur.Load() }

// rowQuerier 取一行结果。
type rowQuerier func(ctx context.Context, query string, args ...any) *sql.Row

// rowsQuerier 取结果集。
type rowsQuerier func(ctx context.Context, query string, args ...any) (*sql.Rows, error)

// execer 执行写语句。
type execer func(ctx context.Context, query string, args ...any) (sql.Result, error)

// Dialect 返回当前生效的方言。
func (d *DB) Dialect() Dialect {
	h := d.current()
	if h == nil {
		return ""
	}
	return h.dialect
}

// Begin 开启一个带方言信息的事务。
func (d *DB) Begin(ctx context.Context) (*Tx, error) {
	h := d.current()
	if h == nil {
		return nil, fmt.Errorf("数据库连接已关闭")
	}
	tx, err := h.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	return &Tx{Tx: tx, Dialect: h.dialect}, nil
}

// Exec 在事务里执行写语句（按方言改写占位符）。
func (t *Tx) Exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return t.Tx.ExecContext(ctx, t.Dialect.Rebind(q), args...)
}

// QueryRow 在事务里查一行（按方言改写占位符）。
func (t *Tx) QueryRow(ctx context.Context, q string, args ...any) *sql.Row {
	return t.Tx.QueryRowContext(ctx, t.Dialect.Rebind(q), args...)
}

// Open 按配置打开数据库并执行迁移。
func Open(driver, dsn string, maxOpen, maxIdle int) (*DB, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	h, err := openHandle(ctx, driver, dsn, maxOpen, maxIdle)
	if err != nil {
		return nil, err
	}
	db := &DB{}
	db.cur.Store(h)
	if err := db.migrateHandle(ctx, h); err != nil {
		_ = h.sqlDB.Close()
		return nil, err
	}
	return db, nil
}

// openHandle 打开一个连接池（不迁移）。
func openHandle(ctx context.Context, driver, dsn string, maxOpen, maxIdle int) (*handle, error) {
	dialect, err := DialectFor(driver)
	if err != nil {
		return nil, err
	}
	// ⚠️ 用 sqlDriverName(driver) 而不是 driver：
	//    配置层写的是方言名 "postgres"，但 pgx 注册的名字是 "pgx"，
	//    直接 sql.Open("postgres") 会报 "unknown driver"。
	sqlDB, err := sql.Open(sqlDriverName(driver), dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库: %w", err)
	}
	// SQLite 只能单写者：多连接会随机 SQLITE_BUSY，且 busy_timeout 对写锁竞争不生效。
	if dialect == DialectSQLite {
		maxOpen = 1
		maxIdle = 1
	}
	sqlDB.SetMaxOpenConns(maxOpen)
	sqlDB.SetMaxIdleConns(maxIdle)
	sqlDB.SetConnMaxLifetime(30 * time.Minute)

	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("连接数据库: %w", err)
	}
	return &handle{sqlDB: sqlDB, dialect: dialect}, nil
}

// SwitchTo 换到另一个数据库：开新连接 → 跑迁移 → 原子替换 → 旧连接延迟关闭。
//
// 顺序很重要：**迁移在替换之前**——新库没建好表就不该被切上去。
// 失败时旧连接原封不动，调用方拿到的是错误而不是"半切换"的状态。
//
// 换库后所有持有 *DB 的服务自动改用新连接。
func (d *DB) SwitchTo(ctx context.Context, driver, dsn string) error {
	h, err := openHandle(ctx, driver, dsn, 20, 10)
	if err != nil {
		return err
	}
	if err := d.migrateHandle(ctx, h); err != nil {
		_ = h.sqlDB.Close()
		return fmt.Errorf("迁移新库: %w", err)
	}
	old := d.cur.Swap(h)
	d.retire(old)
	return nil
}

// retire 把旧连接池退役，延迟关闭。
//
// 宽限期见 retireGrace 的说明。
func (d *DB) retire(old *handle) {
	if old == nil {
		return
	}
	d.mu.Lock()
	d.retired = append(d.retired, old.sqlDB)
	d.mu.Unlock()
	time.AfterFunc(retireGrace, func() { _ = old.sqlDB.Close() })
}

// Ping 检查当前连接是否可用。
func (d *DB) Ping(ctx context.Context) error {
	h := d.current()
	if h == nil {
		return fmt.Errorf("数据库连接已关闭")
	}
	return h.sqlDB.PingContext(ctx)
}

// Close 关闭当前连接池与所有已退役的连接池。
func (d *DB) Close() error {
	var errs []error
	if h := d.current(); h != nil {
		errs = append(errs, h.sqlDB.Close())
	}
	d.mu.Lock()
	for _, s := range d.retired {
		errs = append(errs, s.Close())
	}
	d.retired = nil
	d.mu.Unlock()
	return errors.Join(errs...)
}

// Query 执行查询并按方言重绑占位符。
func (d *DB) Query(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	h := d.current()
	if h == nil {
		return nil, fmt.Errorf("数据库连接已关闭")
	}
	return h.sqlDB.QueryContext(ctx, h.dialect.Rebind(q), args...)
}

// QueryRow 执行单行查询。
func (d *DB) QueryRow(ctx context.Context, q string, args ...any) *sql.Row {
	h := d.current()
	if h == nil {
		// QueryRow 的签名不允许返回 error——只能让错误从 Scan 里冒出来。
		// 用一个已关闭的连接池产出"必然失败的 Row"。
		return closedDB.QueryRowContext(ctx, q)
	}
	return h.sqlDB.QueryRowContext(ctx, h.dialect.Rebind(q), args...)
}

// closedDB 是一个已关闭的连接池，只用于在"没有连接池"时产出必然失败的 Row。
var closedDB = func() *sql.DB {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		panic(fmt.Sprintf("初始化 closedDB: %v", err))
	}
	_ = db.Close()
	return db
}()

// Exec 执行写语句。
func (d *DB) Exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	h := d.current()
	if h == nil {
		return nil, fmt.Errorf("数据库连接已关闭")
	}
	return h.sqlDB.ExecContext(ctx, h.dialect.Rebind(q), args...)
}

// insertAndGetID 插入一行并取回它的自增主键。
//
// insert 必须是完整的 INSERT 语句（不含结尾分号）。
//
// ⚠️ **不能一律用 LastInsertId**：pgx 驱动不实现它，
//
//	调用会返回 "LastInsertId is not supported by this driver"——
//	行插进去了、调用方却拿到错误。postgres 与 sqlite 走 `RETURNING id`，
//	MySQL 走驱动的 LastInsertId。
//
// ⚠️ **不能一律用 RETURNING**：MySQL 不支持它。
func (d *DB) insertAndGetID(ctx context.Context, insert string, args ...any) (int64, error) {
	h := d.current()
	if h == nil {
		return 0, fmt.Errorf("数据库连接已关闭")
	}
	return insertAndGetID(ctx, h.dialect, d.QueryRow, d.Exec, insert, args...)
}

// insertAndGetID 插入一行并取回它的自增主键（DB 与 Tx 共用）。
//
// queryRow / exec 由调用方给出：DB 走连接池、Tx 走事务，
// 但占位符重绑都由它们自己完成——这里只决定"用 RETURNING 还是 LastInsertId"。
func insertAndGetID(ctx context.Context, dialect Dialect, queryRow rowQuerier, exec execer, insert string, args ...any) (int64, error) {
	if dialect.SupportsReturning() {
		var id int64
		if err := queryRow(ctx, insert+" RETURNING id", args...).Scan(&id); err != nil {
			return 0, err
		}
		return id, nil
	}
	res, err := exec(ctx, insert, args...)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// Upsert 执行累加式 upsert。
//
// insertCols 是 INSERT 的完整列清单（顺序与 values 一致），
// setCols 是冲突时累加的列，conflictCols 是唯一键列。
func (d *DB) Upsert(ctx context.Context, table, conflictCols string, insertCols, setCols []string, values []any) error {
	ph := Placeholders(len(values))
	q := d.Dialect().Upsert(table, conflictCols, insertCols, setCols, ph)
	_, err := d.Exec(ctx, q, values...)
	return err
}

// Migrate 执行当前方言的迁移脚本。
func (d *DB) Migrate(ctx context.Context) error {
	h := d.current()
	if h == nil {
		return fmt.Errorf("数据库连接已关闭")
	}
	return d.migrateHandle(ctx, h)
}

func (d *DB) migrateHandle(ctx context.Context, h *handle) error {
	exec := func(q string, args ...any) (sql.Result, error) {
		return h.sqlDB.ExecContext(ctx, h.dialect.Rebind(q), args...)
	}
	if _, err := exec(schemaMigrationsDDL(h.dialect)); err != nil {
		return fmt.Errorf("创建 schema_migrations: %w", err)
	}
	applied, err := d.appliedVersionsHandle(ctx, h)
	if err != nil {
		return err
	}
	dir := "migrations/" + string(h.dialect)
	entries, err := fs.ReadDir(migrationFS, dir)
	if err != nil {
		return fmt.Errorf("读取迁移目录: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		if applied[name] {
			continue
		}
		b, err := migrationFS.ReadFile(path.Join(dir, name))
		if err != nil {
			return fmt.Errorf("读取迁移 %s: %w", name, err)
		}
		// 迁移语句必须幂等（MySQL 无事务性 DDL，重跑要安全）。
		for _, stmt := range splitStatements(string(b)) {
			if _, err := exec(stmt); err != nil {
				// ⚠️ 「补列」语句遇到"列已存在"→ 当作已完成。
				//
				// 为什么允许：ALTER TABLE ... ADD COLUMN 没法写成幂等形式
				//（sqlite / mysql 都不支持 ADD COLUMN IF NOT EXISTS），
				// 而"列已存在、内容正确"正是这条语句要的终态。
				//
				// 为什么**只**放宽这一种：其它语句报"已存在"是真问题
				//（表重复、索引重复……），必须暴露出来。
				//
				// 这条豁免救的是**半成品库**：迁移跑到一半失败
				// （比如 0004 加完列、版本还没记上就挂了），
				// 重跑时不能因为"列已存在"永远卡住。
				if isAddColumnStatement(stmt) && IsDuplicateColumn(err) {
					continue
				}
				return fmt.Errorf("执行迁移 %s 失败: %w\n语句: %s", name, err, stmt)
			}
		}
		if _, err := exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			name, Now()); err != nil {
			return fmt.Errorf("记录迁移 %s: %w", name, err)
		}
	}
	return nil
}

// schemaMigrationsDDL 返回当前方言的 schema_migrations 建表语句。
//
// ⚠️ MySQL **不许 TEXT/BLOB 列做主键**——直接建会报
//
//	Error 1170 (42000): BLOB/TEXT column 'version' used in key
//	                specification without a key length
//
// 所以 MySQL 用 VARCHAR(191)：utf8mb4 下 191 字符 = 764 字节，
// 在 InnoDB 索引前缀上限（767/3072 字节）里都装得下。
//
// sqlite / postgres 的 TEXT 主键没问题，保持原样。
func schemaMigrationsDDL(dialect Dialect) string {
	if dialect == DialectMySQL {
		return `CREATE TABLE IF NOT EXISTS schema_migrations (
			version VARCHAR(191) NOT NULL PRIMARY KEY,
			applied_at BIGINT NOT NULL
		)`
	}
	return `CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY,
		applied_at BIGINT NOT NULL
	)`
}

// appliedVersionsHandle 读指定连接池已应用的迁移版本。
func (d *DB) appliedVersionsHandle(ctx context.Context, h *handle) (map[string]bool, error) {
	rows, err := h.sqlDB.QueryContext(ctx, h.dialect.Rebind(`SELECT version FROM schema_migrations`))
	if err != nil {
		return nil, fmt.Errorf("查询 schema_migrations: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = true
	}
	return out, rows.Err()
}

// splitStatements 按分号切分 SQL，去掉空语句与注释行。
func splitStatements(s string) []string {
	var out []string
	var cur strings.Builder
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		cur.WriteString(line)
		cur.WriteByte('\n')
	}
	for _, stmt := range strings.Split(cur.String(), ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		out = append(out, strings.TrimSpace(stmt))
	}
	return out
}
