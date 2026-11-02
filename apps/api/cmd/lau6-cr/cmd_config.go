package main

import (
	"context"
	"fmt"
	"os"

	"github.com/lau6code/lau6-cr/apps/api/internal/service"
	"github.com/lau6code/lau6-cr/apps/api/internal/store"
)

// runSetupCLI 非交互式初始化。
// runConfig 处理 config 子命令（导出 / 回滚 application.yaml）。
//
// 它与 HTTP 端点走**同一个** SettingsManager 方法，
// 两边的备份与回滚语义必须一致——
// 页面上的"回滚"和命令行上的"回滚"要是两套逻辑，那就有一边是错的。
func runConfig(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "用法: lau6-cr config export | lau6-cr config rollback [--to ID]")
		os.Exit(2)
	}
	switch args[0] {
	case "export":
		runConfigExport()
	case "backups":
		runConfigBackups()
	case "rollback":
		runConfigRollback(args[1:])
	case "help", "--help", "-h":
		fmt.Println("用法: lau6-cr config export | lau6-cr config backups | lau6-cr config rollback [--to ID]")
	default:
		fmt.Fprintf(os.Stderr, "未知子命令: config %s\n\n", args[0])
		fmt.Fprintln(os.Stderr, "用法: lau6-cr config export | lau6-cr config backups | lau6-cr config rollback [--to ID]")
		os.Exit(2)
	}
}

// configSettings 打开 DB 并构造设置管理器（config 子命令共用）。
func configSettings() *service.SettingsManager {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载配置失败: %v\n", err)
		os.Exit(1)
	}
	if err := cfg.EnsureDirs(); err != nil {
		fmt.Fprintf(os.Stderr, "创建运行目录失败: %v\n", err)
		os.Exit(1)
	}
	db, err := store.Open(cfg.Database.Driver, dsnFor(cfg), 20, 10)
	if err != nil {
		fmt.Fprintf(os.Stderr, "打开数据库失败: %v\n", err)
		os.Exit(1)
	}
	if err := db.Migrate(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "执行迁移失败: %v\n", err)
		os.Exit(1)
	}
	settings, err := service.NewSettingsManager(db, cfg, cfg.ApplicationYAMLPath())
	if err != nil {
		fmt.Fprintf(os.Stderr, "初始化设置管理器失败: %v\n", err)
		os.Exit(1)
	}
	return settings
}
func runConfigExport() {
	settings := configSettings()
	if err := settings.Export(); err != nil {
		fmt.Fprintf(os.Stderr, "导出配置失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("已导出到 %s\n", settings.YAMLPath())
}

// listConfigBackups 列出可用备份（最新的在前）。
//
// config backups 与 config rollback 不带 --to 时列的是同一份清单，
// 各写一遍必然漂移。
func listConfigBackups(settings *service.SettingsManager) {
	backups, err := settings.ListBackups()
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取备份失败: %v\n", err)
		os.Exit(1)
	}
	if len(backups) == 0 {
		fmt.Println("没有可用的备份")
		return
	}
	fmt.Println("可用备份（最新的在前）：")
	for _, b := range backups {
		fmt.Printf("  %s  %d 字节\n", b.Name, b.Size)
	}
}

// runConfigBackups 列出可用的配置备份。
func runConfigBackups() {
	listConfigBackups(configSettings())
}

// runConfigRollback 回滚配置。
//
// 不带 --to 时只列备份；带 --to 才真的回滚。
func runConfigRollback(args []string) {
	to := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--to":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--to 后面要跟备份 ID")
				os.Exit(2)
			}
			to = args[i+1]
			i++
		default:
			fmt.Fprintf(os.Stderr, "未知参数: %s\n", args[i])
			os.Exit(2)
		}
	}
	settings := configSettings()
	if to == "" {
		listConfigBackups(settings)
		fmt.Println()
		fmt.Println("回滚: lau6-cr config rollback --to <上面某个 ID>")
		return
	}
	path, err := settings.Rollback(context.Background(), to)
	if err != nil {
		fmt.Fprintf(os.Stderr, "回滚失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("已回滚到备份 %s（文件: %s）\n", to, path)
	fmt.Println("⚠️ 只改了配置文件。运行期仍以 system_settings 为准——重启容器后才生效。")
}
