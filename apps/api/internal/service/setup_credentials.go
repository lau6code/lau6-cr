package service

import "fmt"

// adminPasswordMinLen 是管理员口令的最短长度。
//
// ⚠️ 只有这一处定义它：CLI 的终端输入与 Web 向导都走这里。
// 两处各写一份必然漂移——有一处放宽了，另一处照旧，
// 于是"页面上能过、命令行被拒"这种怪事就出现了。
const adminPasswordMinLen = 8

// ValidateAdminCredentials 校验管理员凭据。
//
// ⚠️ 凭据**只能**从界面来（Web 向导的表单 / CLI 的终端输入）：
// 不从环境变量读，也没有"请求里没传就退回配置"的兜底。
//
// 用户名为空或口令为空 → 拒；口令短于 adminPasswordMinLen → 拒。
// 建出一个用户自己不知道口令的管理员等于把实例锁死
// （`lau6-cr admin` 目前没有口令重置命令）。
func ValidateAdminCredentials(username, password string) error {
	if username == "" || password == "" {
		return fmt.Errorf("管理员用户名与密码不能为空")
	}
	if len(password) < adminPasswordMinLen {
		return fmt.Errorf("管理员密码至少 %d 位", adminPasswordMinLen)
	}
	return nil
}
