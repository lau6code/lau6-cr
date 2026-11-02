package store

import (
	"strings"
	"testing"
)

// TestRedactDSNHidesPassword 覆盖脱敏：口令不能出现在结果里。
//
// 连接串会回显给前端（用户确认"测的是哪个库"），也进错误信息——
// 口令出现在里面就是泄漏。
func TestRedactDSNHidesPassword(t *testing.T) {
	cases := []struct {
		name   string
		driver string
		dsn    string
	}{
		{
			name:   "postgres",
			driver: "postgres",
			dsn:    "postgres://lau6:s3cret@db:5432/lau6cr?sslmode=disable",
		},
		{
			name:   "mysql",
			driver: "mysql",
			dsn:    "lau6:s3cret@tcp(db:3306)/lau6cr?parseTime=true",
		},
	}
	for _, c := range cases {
		got := RedactDSN(c.driver, c.dsn)
		if strings.Contains(got, "s3cret") {
			t.Errorf("%s: 脱敏后仍能看到口令：%q", c.name, got)
		}
		if !strings.Contains(got, "***") {
			t.Errorf("%s: 脱敏结果里没有 ***：%q", c.name, got)
		}
		// 主机、端口、库名要留着——用户要靠它确认测的是哪个库。
		if !strings.Contains(got, "lau6cr") {
			t.Errorf("%s: 脱敏把库名也抹掉了：%q", c.name, got)
		}
	}
}

// TestRedactDSNWithoutPassword 覆盖没有口令时原样返回。
func TestRedactDSNWithoutPassword(t *testing.T) {
	dsn := "postgres://lau6@db:5432/lau6cr"
	if got := RedactDSN("postgres", dsn); got != dsn {
		t.Fatalf("无口令时不应改动：%q", got)
	}
}

// TestRedactDSNSQLite 覆盖 sqlite：连接串里没有口令，原样返回。
func TestRedactDSNSQLite(t *testing.T) {
	dsn := "file:/data/db/lau6-cr.db?_pragma=journal_mode(WAL)"
	if got := RedactDSN("sqlite", dsn); got != dsn {
		t.Fatalf("sqlite 不该被改：%q", got)
	}
}

// TestRedactDSNUnparsable 覆盖解析失败时的兜底：宁可糙，也不能漏口令。
func TestRedactDSNUnparsable(t *testing.T) {
	dsn := "postgres://lau6:s3cret@db:notaport/lau6cr"
	got := RedactDSN("postgres", dsn)
	if strings.Contains(got, "s3cret") {
		t.Fatalf("解析失败时也不能漏口令：%q", got)
	}
}
