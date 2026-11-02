package store

import (
	"strings"
	"testing"
)

// TestSchemaMigrationsDDLPerDialect 锁定 schema_migrations 的建表语句按方言区分。
//
// ⚠️ MySQL 不许 TEXT/BLOB 列做主键——用 TEXT 会报
//
//	Error 1170 (42000): BLOB/TEXT column 'version' used in key
//	                    specification without a key length
//
// 它是迁移的**第一句**，所以这一个字符的差别能让整个 MySQL 部署起不来。
func TestSchemaMigrationsDDLPerDialect(t *testing.T) {
	mySQL := schemaMigrationsDDL(DialectMySQL)
	if !strings.Contains(mySQL, "VARCHAR(191)") {
		t.Errorf("MySQL 的建表语句必须用 VARCHAR：%s", mySQL)
	}
	if strings.Contains(strings.ToUpper(mySQL), "TEXT") {
		t.Errorf("MySQL 的建表语句不能用 TEXT：%s", mySQL)
	}
	for _, d := range []Dialect{DialectSQLite, DialectPostgres} {
		ddl := schemaMigrationsDDL(d)
		if !strings.Contains(ddl, "TEXT PRIMARY KEY") {
			t.Errorf("%s 的建表语句应保持 TEXT 主键：%s", d, ddl)
		}
	}
}

// TestQuoteIdent 锁定标识符引号：只有 MySQL 需要反引号。
//
// `key` / `trigger` 是 MySQL 的保留字，不加反引号直接 1064 语法错误。
func TestQuoteIdent(t *testing.T) {
	if got := DialectMySQL.QuoteIdent("key"); got != "`key`" {
		t.Errorf("MySQL: QuoteIdent(key) = %q, 期望 `key`", got)
	}
	for _, d := range []Dialect{DialectSQLite, DialectPostgres} {
		if got := d.QuoteIdent("key"); got != "key" {
			t.Errorf("%s: QuoteIdent(key) = %q, 期望原样", d, got)
		}
	}
}

// TestUpsertOverwritePerDialect 锁定"插入或覆盖"的语句形状。
//
// ⚠️ 覆盖与累加不能混：统计表要累加（Upsert），配置/缓存表要覆盖
// （UpsertOverwrite）——写成累加会把同一行反复叠加，写成覆盖会把先前的数据抹掉。
func TestUpsertOverwritePerDialect(t *testing.T) {
	insertCols := []string{"scope_key", "project_id", "created_at"}
	setCols := []string{"project_id"}
	ph := Placeholders(len(insertCols))

	mysql := DialectMySQL.UpsertOverwrite("t", "scope_key", insertCols, setCols, ph)
	if !strings.Contains(mysql, "ON DUPLICATE KEY UPDATE") {
		t.Errorf("MySQL 必须是 ON DUPLICATE KEY UPDATE：%s", mysql)
	}
	if !strings.Contains(mysql, "project_id = VALUES(project_id)") {
		t.Errorf("MySQL 覆盖赋值必须取新值：%s", mysql)
	}
	if strings.Contains(mysql, "created_at =") {
		t.Errorf("created_at 只在插入时写，冲突时必须保留旧值：%s", mysql)
	}

	for _, d := range []Dialect{DialectSQLite, DialectPostgres} {
		q := d.UpsertOverwrite("t", "scope_key", insertCols, setCols, ph)
		if !strings.Contains(q, "ON CONFLICT (scope_key) DO UPDATE SET") {
			t.Errorf("%s 必须按 scope_key 冲突：%s", d, q)
		}
		if !strings.Contains(q, "project_id = EXCLUDED.project_id") {
			t.Errorf("%s 覆盖赋值必须取新值：%s", d, q)
		}
		if strings.Contains(q, "created_at =") {
			t.Errorf("%s: created_at 只在插入时写：%s", d, q)
		}
	}
}
