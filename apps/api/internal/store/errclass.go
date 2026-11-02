package store

import (
	"errors"
	"strings"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
)

// IsTableMissing 判断错误是不是「表 / 关系不存在」。
//
// ⚠️ **表不存在不等于出错**——尤其在初始化阶段：
//
//	目标库还没建表（迁移没跑过，或跑了一半）→ 这个库**一定没初始化过**，
//	向导应该继续走下去把表建出来，而不是报「连接失败」把用户堵死。
//
// 三种方言的报错形态：
//
//	postgres: ERROR: relation "system_settings" does not exist (SQLSTATE 42P01)
//	mysql:    Error 1146 (42S02): Table 'db.system_settings' doesn't exist
//	sqlite:   no such table: system_settings
func IsTableMissing(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		// 42P01 = undefined_table（表 / 视图 / 序列不存在）
		return pgErr.Code == "42P01"
	}
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) {
		// 1146 = ER_NO_SUCH_TABLE
		return myErr.Number == 1146
	}
	// SQLite（modernc 驱动）只给纯文本错误，没有可判定的错误码。
	return strings.Contains(err.Error(), "no such table")
}

// IsDuplicateColumn 判断错误是不是「列已存在」。
//
// 迁移脚本必须**幂等**（重跑要安全），但有些 DDL 没法写成幂等形式：
//
//	ALTER TABLE repositories ADD COLUMN inherit_visibility ...
//
//	sqlite 与 mysql 都不支持 ADD COLUMN IF NOT EXISTS
//	（postgres 支持，但三方言保持同一条语句更重要），
//	所以「列已存在」只能靠**运行时判定**来容忍。
//
// 只在**补列**语句上容忍：那正是这条语句想要的终态——
// 列在、内容对，等价于「已经加过了」。
// 其它场景（表重复、索引重复……）一律照常报错。
func IsDuplicateColumn(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		// 42701 = duplicate_column
		return pgErr.Code == "42701"
	}
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) {
		// 1060 = ER_DUP_FIELDNAME
		return myErr.Number == 1060
	}
	return strings.Contains(err.Error(), "duplicate column name")
}

// isAddColumnStatement 判断一条 SQL 是不是「补列」语句。
//
// 用于把「列已存在」的容忍**限定在补列**上：
// 其它语句遇到"已存在"照样报错（那是真问题）。
func isAddColumnStatement(stmt string) bool {
	up := strings.ToUpper(stmt)
	return strings.HasPrefix(up, "ALTER TABLE") && strings.Contains(up, "ADD COLUMN")
}
