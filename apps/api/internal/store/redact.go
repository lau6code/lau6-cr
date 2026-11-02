package store

import (
	"net/url"
	"strings"
)

// RedactDSN 把连接串里的口令抹成 ***。
//
// 连接串里通常含数据库口令，出现在**响应体、日志、错误信息**里都是泄漏。
// 实现见 docs/design/07-database.md §9.3。
//
// 解析不了就退回到"把 user:pass@ 里的口令段整段替换"——
// 宁可显示得糙一点，也不能把口令原样吐出去。
func RedactDSN(driver, dsn string) string {
	if dsn == "" {
		return ""
	}
	switch driver {
	case "postgres":
		// URL 形式（向导拼出来的就是这种）：直接改 userinfo。
		if strings.Contains(dsn, "://") {
			u, err := url.Parse(dsn)
			if err == nil && u.User != nil {
				if pw, ok := u.User.Password(); ok && pw != "" {
					// ⚠️ 手工拼，不走 url.UserPassword：
					//    它会把 * 转义成 %2A，读起来不像样
					//    （文档里的脱敏形式是 postgres://user:***@host/db）。
					name := u.User.Username()
					out := dsn[:strings.Index(dsn, "://")+3] + name + ":***@" + u.Host + u.Path
					if u.RawQuery != "" {
						out += "?" + u.RawQuery
					}
					return out
				}
				return dsn
			}
			return redactUserinfo(dsn)
		}
		// key=value 形式：口令在 "password=" 后面。
		return redactKeyValue(dsn, "password")
	case "mysql":
		// mysql 的 DSN 是 `user:pass@tcp(host:port)/db`——
		// 口令是 @ 之前的最后一段。
		return redactUserinfo(dsn)
	default:
		// sqlite 的连接串里没有口令。
		return dsn
	}
}

// redactUserinfo 把 "user:pass@" 里的口令段替换成 ***。
//
// ⚠️ 它是"显示得糙一点"的兜底，不是"假装成功"——
// 调用方拿到的仍然是一条不可用的连接串（口令被抹了），
// 不会有人拿它去连库。
func redactUserinfo(dsn string) string {
	i := strings.Index(dsn, "://")
	rest := dsn
	if i >= 0 {
		rest = dsn[i+3:]
	}
	at := strings.LastIndex(rest, "@")
	if at < 0 {
		return dsn
	}
	colon := strings.Index(rest[:at], ":")
	if colon < 0 {
		return dsn
	}
	prefix := ""
	if i >= 0 {
		prefix = dsn[:i+3]
	}
	return prefix + rest[:colon+1] + "***" + rest[at:]
}

func redactKeyValue(dsn, key string) string {
	lower := strings.ToLower(dsn)
	idx := strings.Index(lower, key+"=")
	if idx < 0 {
		return dsn
	}
	start := idx + len(key) + 1
	end := strings.IndexAny(dsn[start:], " \t\n")
	if end < 0 {
		end = len(dsn)
	} else {
		end += start
	}
	return dsn[:start] + "***" + dsn[end:]
}
