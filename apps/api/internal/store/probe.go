package store

import (
	"context"
	"database/sql"
	"fmt"
)

// sqlDriverName 把配置里的驱动名映射到 database/sql 里注册的驱动名。
//
// 配置层用的是**方言名**（sqlite / postgres / mysql，见 DialectFor），
// 但 pgx 注册的名字是 "pgx"——直接 sql.Open("postgres", …) 会报
//
//	sql: unknown driver "postgres" (forgotten import?)
//
// 表现是"配了 postgres 的实例根本起不来"。
// 这里做一次显式映射，把方言名与驱动名分开。
func sqlDriverName(driver string) string {
	switch driver {
	case "postgres":
		return "pgx"
	default:
		return driver
	}
}

// Probe 按给定的驱动与 DSN 建立一次连接并 ping 一下，
// 返回**实际生效的 schema**。
//
// 它**只测连通性**：不跑迁移、不建表、不写数据。
// 供配置向导的「测试连接」按钮使用（见 docs/design/08-setup.md §7.4）。
//
// wantSchema 是"应当生效的 schema"：
//
//   - postgres：search_path 里指定的那个 schema；
//   - mysql：库名（mysql 里库名即 schema）；
//   - sqlite：空串。
//
// ⚠️ 为什么要校验它：postgres 的 `SET search_path = '不存在的schema'`
// **不会报错**——连接照样成功，但之后每条查询都找不到表。
// 这里把它变成一次明确的失败，别把这种坑留到运行时。
//
// 失败时返回原始驱动错误（连接被拒、认证失败、库不存在……），
// 由调用方翻译成对外文案——这里不做任何包装或兜底。
func Probe(ctx context.Context, driver, dsn, wantSchema string) (string, error) {
	dialect, err := DialectFor(driver)
	if err != nil {
		return "", err
	}
	sqlDB, err := sql.Open(sqlDriverName(driver), dsn)
	if err != nil {
		return "", fmt.Errorf("打开数据库: %w", err)
	}
	defer func() { _ = sqlDB.Close() }()
	if err := sqlDB.PingContext(ctx); err != nil {
		return "", fmt.Errorf("连接数据库: %w", err)
	}

	schema, err := currentSchema(ctx, dialect, sqlDB)
	if err != nil {
		return "", err
	}
	if wantSchema != "" && schema != wantSchema {
		if schema == "" {
			return schema, fmt.Errorf("schema %q 不存在（连接上之后没有任何生效的 schema）", wantSchema)
		}
		return schema, fmt.Errorf("schema %q 不存在（当前生效的是 %q）", wantSchema, schema)
	}
	return schema, nil
}

// currentSchema 查当前生效的 schema。
func currentSchema(ctx context.Context, dialect Dialect, sqlDB *sql.DB) (string, error) {
	var q string
	switch dialect {
	case DialectPostgres:
		q = `SELECT current_schema()`
	case DialectMySQL:
		q = `SELECT DATABASE()`
	default:
		return "", nil
	}
	var schema sql.NullString
	if err := sqlDB.QueryRowContext(ctx, q).Scan(&schema); err != nil {
		return "", fmt.Errorf("查询当前 schema: %w", err)
	}
	if !schema.Valid {
		return "", nil
	}
	return schema.String, nil
}
