package store

import (
	"fmt"
	"strings"
	"time"
)

// Dialect 表示数据库方言的差异点。
type Dialect string

const (
	DialectSQLite   Dialect = "sqlite"
	DialectPostgres Dialect = "postgres"
	DialectMySQL    Dialect = "mysql"
)

func DialectFor(driver string) (Dialect, error) {
	switch driver {
	case "sqlite":
		return DialectSQLite, nil
	case "postgres":
		return DialectPostgres, nil
	case "mysql":
		return DialectMySQL, nil
	}
	return "", fmt.Errorf("不支持的数据库驱动: %q", driver)
}

// QuoteIdent 给标识符加上方言对应的引号。
//
// 目前只有 MySQL 需要：`key` 是它的**保留字**，
// 不加反引号会直接语法报错——
//
//	Error 1064 (42000): You have an error in your SQL syntax;
//	                     check the manual that corresponds to your MySQL server version
//
// sqlite 与 postgres 里 `key` 是非保留字，保持原样即可。
func (d Dialect) QuoteIdent(name string) string {
	if d == DialectMySQL {
		return "`" + name + "`"
	}
	return name
}

// Rebind 把 ? 占位符改写成目标方言的形式。
//
// SQLite 与 MySQL 原生用 ?；PostgreSQL 需要 $1 $2 …。
// 字符串字面量里的问号必须跳过，否则 LIKE '%?%' 会被改坏。
func (d Dialect) Rebind(query string) string {
	if d != DialectPostgres {
		return query
	}
	var b strings.Builder
	b.Grow(len(query) + 8)
	n := 0
	inSingle, inDouble, inBacktick := false, false, false
	for i := 0; i < len(query); i++ {
		c := query[i]
		switch {
		case c == '\'' && !inDouble && !inBacktick:
			inSingle = !inSingle
		case c == '"' && !inSingle && !inBacktick:
			inDouble = !inDouble
		case c == '`' && !inSingle && !inDouble:
			inBacktick = !inBacktick
		case c == '?' && !inSingle && !inDouble && !inBacktick:
			n++
			b.WriteByte('$')
			b.WriteString(fmt.Sprint(n))
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// Upsert 生成"插入或累加"的语句。
//
// 统计表的写入是**累加**而非覆盖——同一小时会刷盘多次，
// 覆盖会把先前的数据抹掉。
//
// ⚠️ 三组列各司其职，少一组就出错：
//
//   - insertCols：INSERT 的**完整**列清单，顺序必须与 values 一致。
//     只列 setCols 会让值数与列数对不上——SQLite 报 "N values for M columns"。
//   - setCols：冲突时**累加**的列（col = col + EXCLUDED.col）。
//   - conflictCols：唯一键列（SQLite/Postgres 的 ON CONFLICT 目标）。
//
// updated_at 这类"覆盖写"的列属于 insertCols，但**不在** setCols 里
// ——累加时间戳没有意义。
func (d Dialect) Upsert(table, conflictCols string, insertCols, setCols []string, placeholder string) string {
	if d == DialectMySQL {
		// MySQL: ON DUPLICATE KEY UPDATE col = col + VALUES(col)
		assigns := make([]string, 0, len(setCols))
		for _, c := range setCols {
			assigns = append(assigns, fmt.Sprintf("%s = %s + VALUES(%s)", c, c, c))
		}
		return fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) ON DUPLICATE KEY UPDATE %s",
			table, strings.Join(insertCols, ", "), placeholder, strings.Join(assigns, ", "))
	}
	// SQLite / PostgreSQL: ON CONFLICT (...) DO UPDATE SET col = table.col + EXCLUDED.col
	assigns := make([]string, 0, len(setCols))
	for _, c := range setCols {
		assigns = append(assigns, fmt.Sprintf("%s = %s.%s + EXCLUDED.%s", c, table, c, c))
	}
	return fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) ON CONFLICT (%s) DO UPDATE SET %s",
		table, strings.Join(insertCols, ", "), placeholder, conflictCols, strings.Join(assigns, ", "))
}

// UpsertOverwrite 生成"插入或覆盖"的语句。
//
// 与 Upsert 的区别只在冲突时的赋值：这里是**覆盖**
// （col = EXCLUDED.col / col = VALUES(col)），不是累加。
//
// ⚠️ 三组列各司其职，少一组就出错：
//
//   - insertCols：INSERT 的**完整**列清单，顺序必须与 values 一致。
//   - setCols：冲突时被**覆盖**的列。
//   - conflictCols：唯一键列（SQLite/Postgres 的 ON CONFLICT 目标）。
//
// created_at 这类"只在插入时写"的列属于 insertCols，但**不在** setCols 里——
// 冲突时保留旧值。
func (d Dialect) UpsertOverwrite(table, conflictCols string, insertCols, setCols []string, placeholder string) string {
	if d == DialectMySQL {
		// MySQL: ON DUPLICATE KEY UPDATE col = VALUES(col)
		assigns := make([]string, 0, len(setCols))
		for _, c := range setCols {
			assigns = append(assigns, fmt.Sprintf("%s = VALUES(%s)", c, c))
		}
		return fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) ON DUPLICATE KEY UPDATE %s",
			table, strings.Join(insertCols, ", "), placeholder, strings.Join(assigns, ", "))
	}
	// SQLite / PostgreSQL: ON CONFLICT (...) DO UPDATE SET col = EXCLUDED.col
	assigns := make([]string, 0, len(setCols))
	for _, c := range setCols {
		assigns = append(assigns, fmt.Sprintf("%s = EXCLUDED.%s", c, c))
	}
	return fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) ON CONFLICT (%s) DO UPDATE SET %s",
		table, strings.Join(insertCols, ", "), placeholder, conflictCols, strings.Join(assigns, ", "))
}

// SupportsReturning 报告该方言是否支持 `INSERT ... RETURNING id`。
//
// postgres 与 sqlite 支持（SQLite 3.35+）；MySQL 不支持，
// 只能靠驱动的 LastInsertId。
//
// ⚠️ 这不是"偏好"问题：**pgx 驱动不实现 LastInsertId**，
//
//	调用它会返回 "LastInsertId is not supported by this driver"——
//	行插进去了，调用方却拿到错误。
//	表现是"创建用户接口报 500，但用户其实已经建好了"。
func (d Dialect) SupportsReturning() bool {
	return d != DialectMySQL
}

func Placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	parts := make([]string, n)
	for i := range parts {
		parts[i] = "?"
	}
	return strings.Join(parts, ", ")
}

func Now() int64 { return time.Now().Unix() }
