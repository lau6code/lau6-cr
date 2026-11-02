// Command lau6-cr 是私有镜像仓的服务端。
package main

import (
	"fmt"
	"os"
)

// 版本号由构建时注入。
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		runServe()
		return
	}
	switch os.Args[1] {
	case "serve":
		runServe()
	case "setup":
		runSetupCLI()
	case "config":
		runConfig(os.Args[2:])
	case "admin":
		runAdmin(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println("lau6-cr", version)
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "未知命令: %s\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}
func usage() {
	fmt.Println(`lau6-cr — 私有镜像仓

用法:
  lau6-cr serve                      启动服务（默认）
  lau6-cr setup                     交互式初始化（终端里输入管理员凭据）
  lau6-cr config export             强制重新导出 application.yaml
  lau6-cr config backups            列出可用备份
  lau6-cr config rollback [--to ID] 回滚配置到指定备份（不传 --to 则列出）
  lau6-cr admin integration-info    打印远程 distribution 接入清单
  lau6-cr version                   打印版本
  lau6-cr help                      显示本帮助

admin 子命令:
  lau6-cr admin integration-info [--show-secret]
        打印远程 distribution 需要配的 JWKS 来源与事件回调。
        --show-secret 才会打印回调密钥的**值**（否则只给路径）。

config 子命令:
  lau6-cr config export
        把当前生效配置重新写回 application.yaml（导出失败时的补救手段）。
  lau6-cr config backups
        列出可用备份。
  lau6-cr config rollback
        列出可用备份。不带 --to 时只列不滚。
  lau6-cr config rollback --to 2026-10-26T17-56-03Z
        把该备份复制回 application.yaml。
        ⚠️ 只改文件不改运行期，要生效需重启容器。

初始化:
  lau6-cr setup     在终端里输入管理员凭据（口令不回显）
                   也可以打开 /setup 走网页向导
  CR_PUBLIC_URL     外部访问根 URL（环境变量）`)
}
