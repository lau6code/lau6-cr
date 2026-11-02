package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// mysqlReservedWords 是 MySQL 8.0 的**保留字**清单。
//
// 取自 INFORMATION_SCHEMA.KEYWORDS（RESERVED = 1），不是手抄的——
// 手抄会漏，而漏一个就是"代码看着对、连上 MySQL 直接 1064"。
//
// 它是下面几个静态审计测试的依据：
// MySQL 的保留字**不能**直接当列名/表名用，必须加反引号；
// sqlite 与 postgres 里大部分是"非保留字"，所以同一份 SQL 在那边能跑。
var mysqlReservedWords = map[string]bool{
	"accessible": true, "add": true, "all": true, "alter": true, "analyze": true, "and": true,
	"as": true, "asc": true, "asensitive": true, "before": true, "between": true, "bigint": true,
	"binary": true, "blob": true, "both": true, "by": true, "call": true, "cascade": true, "case": true,
	"change": true, "char": true, "character": true, "check": true, "collate": true, "column": true,
	"condition": true, "constraint": true, "continue": true, "convert": true, "create": true,
	"cross": true, "cube": true, "cume_dist": true, "current_date": true, "current_time": true,
	"current_timestamp": true, "current_user": true, "cursor": true, "database": true,
	"databases": true, "day_hour": true, "day_microsecond": true, "day_minute": true,
	"day_second": true, "dec": true, "decimal": true, "declare": true, "default": true, "delayed": true,
	"delete": true, "dense_rank": true, "desc": true, "describe": true, "deterministic": true,
	"distinct": true, "distinctrow": true, "div": true, "double": true, "drop": true, "dual": true,
	"each": true, "else": true, "elseif": true, "empty": true, "enclosed": true, "escaped": true,
	"except": true, "exists": true, "exit": true, "explain": true, "false": true, "fetch": true,
	"first_value": true, "float": true, "float4": true, "float8": true, "for": true, "force": true,
	"foreign": true, "from": true, "fulltext": true, "function": true, "generated": true, "get": true,
	"grant": true, "group": true, "grouping": true, "groups": true, "having": true,
	"high_priority": true, "hour_microsecond": true, "hour_minute": true, "hour_second": true,
	"if": true, "ignore": true, "in": true, "index": true, "infile": true, "inner": true, "inout": true,
	"insensitive": true, "insert": true, "int": true, "int1": true, "int2": true, "int3": true,
	"int4": true, "int8": true, "integer": true, "intersect": true, "interval": true, "into": true,
	"io_after_gtids": true, "io_before_gtids": true, "is": true, "iterate": true, "join": true,
	"json_table": true, "key": true, "keys": true, "kill": true, "lag": true, "last_value": true,
	"lateral": true, "lead": true, "leading": true, "leave": true, "left": true, "like": true,
	"limit": true, "linear": true, "lines": true, "load": true, "localtime": true,
	"localtimestamp": true, "lock": true, "long": true, "longblob": true, "longtext": true,
	"loop": true, "low_priority": true, "master_bind": true, "master_ssl_verify_server_cert": true,
	"match": true, "maxvalue": true, "mediumblob": true, "mediumint": true, "mediumtext": true,
	"middleint": true, "minute_microsecond": true, "minute_second": true, "mod": true,
	"modifies": true, "natural": true, "not": true, "no_write_to_binlog": true, "nth_value": true,
	"ntile": true, "null": true, "numeric": true, "of": true, "on": true, "optimize": true,
	"optimizer_costs": true, "option": true, "optionally": true, "or": true, "order": true, "out": true,
	"outer": true, "outfile": true, "over": true, "partition": true, "percent_rank": true,
	"precision": true, "primary": true, "procedure": true, "purge": true, "range": true, "rank": true,
	"read": true, "reads": true, "read_write": true, "real": true, "recursive": true,
	"references": true, "regexp": true, "release": true, "rename": true, "repeat": true,
	"replace": true, "require": true, "resignal": true, "restrict": true, "return": true,
	"revoke": true, "right": true, "rlike": true, "row": true, "row_number": true, "rows": true,
	"schema": true, "schemas": true, "second_microsecond": true, "select": true, "sensitive": true,
	"separator": true, "set": true, "show": true, "signal": true, "smallint": true, "spatial": true,
	"specific": true, "sql": true, "sql_big_result": true, "sql_calc_found_rows": true,
	"sqlexception": true, "sql_small_result": true, "sqlstate": true, "sqlwarning": true, "ssl": true,
	"starting": true, "stored": true, "straight_join": true, "system": true, "table": true,
	"terminated": true, "then": true, "tinyblob": true, "tinyint": true, "tinytext": true, "to": true,
	"trailing": true, "trigger": true, "true": true, "undo": true, "union": true, "unique": true,
	"unlock": true, "unsigned": true, "update": true, "usage": true, "use": true, "using": true,
	"utc_date": true, "utc_time": true, "utc_timestamp": true, "values": true, "varbinary": true,
	"varchar": true, "varcharacter": true, "varying": true, "virtual": true, "when": true,
	"where": true, "while": true, "window": true, "with": true, "write": true, "xor": true,
	"year_month": true, "zerofill": true}

// sqlKeywords 是 SQL 语法关键字——它们出现在 SQL 里是正常的，
// 不该被当成"列名用了保留字"。
//
// 之所以要单独列：MySQL 的保留字表里混着 SELECT / KEY / VALUES 这类
// 本身就既是关键字又是保留字的词。
var sqlKeywords = map[string]bool{
	"select": true, "insert": true, "update": true, "delete": true,
	"create": true, "alter": true, "drop": true, "table": true,
	"from": true, "where": true, "set": true, "values": true,
	"and": true, "or": true, "not": true, "null": true,
	"order": true, "by": true, "group": true, "limit": true, "offset": true,
	"join": true, "left": true, "right": true, "inner": true, "outer": true,
	"on": true, "as": true, "in": true, "is": true, "like": true,
	"between": true, "exists": true, "case": true, "when": true,
	"then": true, "else": true, "end": true, "asc": true, "desc": true,
	"distinct": true, "count": true, "max": true, "min": true,
	"coalesce": true, "sum": true, "avg": true, "if": true, "ifnull": true,
	"conflict": true, "duplicate": true, "key": true, "primary": true,
	"unique": true, "index": true, "default": true, "check": true,
	"references": true, "foreign": true, "constraint": true, "cascade": true,
	"auto_increment": true, "engine": true, "charset": true, "collate": true,
	"int": true, "integer": true, "bigint": true, "smallint": true,
	"tinyint": true, "varchar": true, "text": true, "blob": true,
	"boolean": true, "bool": true, "timestamp": true, "datetime": true,
	"double": true, "real": true, "json": true, "returning": true,
	"with": true, "union": true, "all_rows": true, "using": true,
	"force": true, "ignore": true, "replace": true, "into": true,
	"values_list": true, "row": true, "rows": true, "recursive": true,
	"over": true, "partition": true, "window": true, "lateral": true,
}

// 标识符：字母、数字、下划线。
var identRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

// quotedIdentRe 匹配 MySQL 的反引号标识符。
var quotedIdentRe = regexp.MustCompile("`[^`]*`")

// TestMySQLMigrationsAvoidReservedWords 扫描 MySQL 迁移脚本里的保留字列名/表名。
//
// 保留字做列名在 MySQL 上是**语法错误**（1064）——
// 而这类错误只有真连上 MySQL 才会暴露，CI 里跑 sqlite 完全看不见。
func TestMySQLMigrationsAvoidReservedWords(t *testing.T) {
	entries, err := fs.ReadDir(migrationFS, "migrations/mysql")
	if err != nil {
		t.Fatalf("读取 MySQL 迁移目录: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		b, err := migrationFS.ReadFile("migrations/mysql/" + e.Name())
		if err != nil {
			t.Fatalf("读取 %s: %v", e.Name(), err)
		}
		for _, ident := range reservedIdentsInDDL(string(b)) {
			t.Errorf("%s: 列名/表名 %q 是 MySQL 保留字，必须加反引号（如 `%s`）",
				e.Name(), ident, ident)
		}
	}
}

// reservedIdentsInDDL 找出 DDL 里未加引号的保留字标识符。
func reservedIdentsInDDL(sql string) []string {
	var hits []string
	seen := map[string]bool{}
	// 去掉注释行，避免注释里的词误报。
	var lines []string
	for _, line := range strings.Split(sql, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		lines = append(lines, line)
	}
	body := strings.Join(lines, "\n")

	for _, block := range regexp.MustCompile(`(?is)CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?(\S+)\s*\((.*?)\)\s*ENGINE`).FindAllStringSubmatch(body, -1) {
		table := strings.Trim(block[1], "`")
		if mysqlReservedWords[strings.ToLower(table)] && !seen[table] {
			seen[table] = true
			hits = append(hits, table)
		}
		for _, line := range strings.Split(block[2], "\n") {
			s := strings.TrimSpace(line)
			if s == "" || strings.HasPrefix(s, "KEY ") || strings.HasPrefix(s, "CONSTRAINT ") ||
				strings.HasPrefix(s, "PRIMARY KEY") || strings.HasPrefix(s, "UNIQUE ") ||
				strings.HasPrefix(s, "INDEX ") || strings.HasPrefix(s, "CHECK ") ||
				strings.HasPrefix(s, "FOREIGN KEY") {
				continue
			}
			m := regexp.MustCompile("^(`?)([A-Za-z_][A-Za-z0-9_]*)`?").FindStringSubmatch(s)
			if m == nil {
				continue
			}
			col := m[2]
			if m[1] == "`" {
				// 已经加了反引号——处理过了。
				continue
			}
			low := strings.ToLower(col)
			if mysqlReservedWords[low] && !seen[col] {
				seen[col] = true
				hits = append(hits, col)
			}
		}
	}
	return hits
}

// TestMySQLMigrationsAvoidIndexedText 扫描"被索引的 TEXT/BLOB 列"。
//
// MySQL 不许 TEXT/BLOB 直接做索引（除非指定前缀长度）：
//
//	Error 1170 (42000): BLOB/TEXT column 'version' used in key
//	                    specification without a key length
func TestMySQLMigrationsAvoidIndexedText(t *testing.T) {
	entries, err := fs.ReadDir(migrationFS, "migrations/mysql")
	if err != nil {
		t.Fatalf("读取 MySQL 迁移目录: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		b, err := migrationFS.ReadFile("migrations/mysql/" + e.Name())
		if err != nil {
			t.Fatalf("读取 %s: %v", e.Name(), err)
		}
		sql := string(b)
		for _, m := range regexp.MustCompile(`(?is)PRIMARY\s+KEY\s*\(([^)]*)\)`).FindAllStringSubmatch(sql, -1) {
			for _, col := range strings.Split(m[1], ",") {
				name := strings.TrimSpace(strings.Trim(strings.TrimSpace(col), "`"))
				if !isTextColumn(sql, name) {
					continue
				}
				t.Errorf("%s: 主键列 %q 是 TEXT/BLOB——MySQL 会报 Error 1170，改用 VARCHAR",
					e.Name(), name)
			}
		}
		for _, m := range regexp.MustCompile(`(?is)UNIQUE\s*(?:KEY|INDEX)?\s*\(([^)]*)\)`).FindAllStringSubmatch(sql, -1) {
			for _, col := range strings.Split(m[1], ",") {
				name := strings.TrimSpace(strings.Trim(strings.TrimSpace(col), "`"))
				if isTextColumn(sql, name) {
					t.Errorf("%s: 唯一索引列 %q 是 TEXT/BLOB——MySQL 会报 Error 1170，改用 VARCHAR",
						e.Name(), name)
				}
			}
		}
	}
}

// isTextColumn 判断列名在脚本里是不是 TEXT/BLOB 类型。
func isTextColumn(sql, col string) bool {
	re := regexp.MustCompile("(?i)(?:`" + regexp.QuoteMeta(col) + "`|" + regexp.QuoteMeta(col) + `)\\s+(TEXT|BLOB|LONGTEXT|MEDIUMTEXT|TINYTEXT)`)
	return re.MatchString(sql)
}

// TestSQLStringsAvoidMySQLReservedWords 扫描 Go 代码里的 SQL 字符串。
//
// 覆盖 store 与 service 两个包——它们是唯一写裸 SQL 的地方。
//
// 规则：SQL 里出现的 MySQL 保留字，必须用反引号包起来
// （方言感知就绪的走 Dialect.QuoteIdent）。
func TestSQLStringsAvoidMySQLReservedWords(t *testing.T) {
	for _, dir := range []string{".", "../service"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatalf("列出 %s: %v", dir, err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			for _, sql := range sqlLiteralsInFile(t, f) {
				for _, ident := range reservedIdentsInSQL(sql) {
					t.Errorf("%s: SQL 里的 %q 是 MySQL 保留字，必须加引号：%s",
						filepath.Base(f), ident, firstLine(sql))
				}
			}
		}
	}
}

// TestSQLStringsAvoidConcatOperator 扫描 `||` 字符串拼接。
//
// postgres / sqlite 里 `||` 是拼接，**MySQL 里 `||` 是逻辑 OR**——
// 拼出来的是 0/1，不是字符串。要用 CONCAT()（三个方言都有）。
func TestSQLStringsAvoidConcatOperator(t *testing.T) {
	for _, dir := range []string{".", "../service"} {
		files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			for _, sql := range sqlLiteralsInFile(t, f) {
				if strings.Contains(sql, "||") {
					t.Errorf("%s: SQL 里用了 || 拼接（MySQL 里是逻辑 OR），改用 CONCAT()：%s",
						filepath.Base(f), firstLine(sql))
				}
			}
		}
	}
}

// sqlLiteralsInFile 抽出文件里像 SQL 的字符串字面量。
func sqlLiteralsInFile(t *testing.T, path string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("解析 %s: %v", path, err)
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		s, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		if looksLikeSQL(s) {
			out = append(out, s)
		}
		return true
	})
	return out
}

// looksLikeSQL 判断字符串是不是 SQL 语句。
func looksLikeSQL(s string) bool {
	up := strings.ToUpper(strings.TrimSpace(s))
	for _, kw := range []string{"SELECT ", "INSERT ", "UPDATE ", "DELETE ", "CREATE ", "ALTER "} {
		if strings.HasPrefix(up, kw) {
			return true
		}
	}
	return false
}

// reservedIdentsInSQL 找出 SQL 里未加引号的保留字标识符。
func reservedIdentsInSQL(sql string) []string {
	// 先把反引号标识符摘掉（它们是"已经处理过"的）。
	stripped := quotedIdentRe.ReplaceAllString(sql, " ")
	var hits []string
	seen := map[string]bool{}
	for _, loc := range identRe.FindAllStringIndex(stripped, -1) {
		m := stripped[loc[0]:loc[1]]
		low := strings.ToLower(m)
		if sqlKeywords[low] || !mysqlReservedWords[low] || seen[low] {
			continue
		}
		// 函数调用（DATABASE() 这类）不是标识符。
		if rest := strings.TrimSpace(stripped[loc[1]:]); strings.HasPrefix(rest, "(") {
			continue
		}
		seen[low] = true
		hits = append(hits, m)
	}
	return hits
}

// firstLine 取 SQL 的第一行（报错信息里够用）。
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// 确认 os 包被用到（避免 import 漂移）。
var _ = os.Getenv
