package cache

import "testing"

// TestRedisStatusWarnings 覆盖缓存配置的风险提示。
//
// 这三条是 docs/design/12-cache.md §5 的运维清单：
// 淘汰策略、AOF/RDB 持久化、requirepass。
// 漏报任何一条，用户都会以为"地址密码填对了就万事大吉"。
func TestRedisStatusWarnings(t *testing.T) {
	yes := true
	no := false

	cases := []struct {
		name    string
		status  RedisStatus
		want    []string
		notWant []string
	}{
		{
			name:   "淘汰策略不对",
			status: RedisStatus{MaxmemoryPolicy: "allkeys-lru", AppendOnly: "yes", RequirePass: &yes},
			want:   []string{"maxmemory-policy"},
		},
		{
			name:   "没开持久化",
			status: RedisStatus{MaxmemoryPolicy: "noeviction", AppendOnly: "no", RequirePass: &yes},
			want:   []string{"appendonly"},
		},
		{
			name:   "没设 requirepass",
			status: RedisStatus{MaxmemoryPolicy: "noeviction", AppendOnly: "yes", RequirePass: &no},
			want:   []string{"requirepass"},
		},
		{
			name:    "全对",
			status:  RedisStatus{MaxmemoryPolicy: "noeviction", AppendOnly: "yes", RequirePass: &yes},
			notWant: []string{"maxmemory-policy", "appendonly", "requirepass"},
		},
		{
			name:    "读不到配置时不乱报",
			status:  RedisStatus{},
			notWant: []string{"maxmemory-policy", "appendonly", "requirepass"},
		},
	}

	for _, c := range cases {
		got := c.status.Warnings()
		joined := ""
		for _, w := range got {
			joined += w + "\n"
		}
		for _, want := range c.want {
			if !contains(joined, want) {
				t.Errorf("%s: 提示里没有 %q，实际：%s", c.name, want, joined)
			}
		}
		for _, notWant := range c.notWant {
			if contains(joined, notWant) {
				t.Errorf("%s: 不该提示 %q，实际：%s", c.name, notWant, joined)
			}
		}
	}
}

// contains 是 strings.Contains 的小封装（避免多 import）。
func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestInfoField 覆盖 INFO 文本的字段解析。
func TestInfoField(t *testing.T) {
	info := "# Server\r\nredis_version:7.2.5\r\nredis_git_sha1:abc\r\n\r\n# Keyspace\r\n"
	if got := infoField(info, "redis_version"); got != "7.2.5" {
		t.Errorf("redis_version = %q, 期望 7.2.5", got)
	}
	if got := infoField(info, "not_exist"); got != "" {
		t.Errorf("不存在的字段应返回空串，实际 %q", got)
	}
}
