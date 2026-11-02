package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/lau6code/lau6-cr/apps/api/internal/config"
	"github.com/lau6code/lau6-cr/apps/api/internal/service"
)

// runSetupCLI 在终端里初始化实例。
//
// 管理员凭据从终端读（交互式或 stdin 管道），不读环境变量。
func runSetupCLI() {
	rt := openRuntime(nil)
	defer rt.Close()
	cfg := rt.cfg
	logger := rt.logger
	db := rt.db
	settings, err := service.NewSettingsManager(db, cfg, cfg.ApplicationYAMLPath(),
		service.WithLogger(logger.Logger),
		// ⚠️ 形态必须告诉设置管理器：external 形态下 storage.* 是上游
		//    distribution 的事，lau6-cr 不渲染也不重启它——
		//    写进设置表就是"看着生效、实际没人读"。
		service.WithDistributionEmbedded(cfg.Registry.Mode == config.ModeEmbedded))
	if err != nil {
		logger.Logger.Error("初始化设置管理器失败", "err", err)
		os.Exit(1)
	}
	// 管理员凭据只从**界面**来：终端交互输入（或 stdin 管道）。
	// 不读环境变量——明文口令进 compose 等于进 git、进配置中心、进 CI 变量。
	user, pass, err := readAdminCredentials(os.Stdin, os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取管理员凭据失败: %v\n", err)
		os.Exit(1)
	}
	svc := service.NewSetupService(service.SetupDeps{
		DB:       db,
		Settings: settings,
		YAMLPath: cfg.ApplicationYAMLPath(),
	})
	// 引导令牌与网页端同一个契约：**客户端提供**，后端收下并落库。
	// CLI 这里自己生成（网页端由浏览器生成），打印出来供后续重新初始化复用。
	tok := strings.TrimSpace(os.Getenv("CR_SETUP_TOKEN"))
	generated := false
	if tok == "" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			logger.Logger.Error("生成引导令牌失败", "err", err)
			os.Exit(1)
		}
		tok = hex.EncodeToString(b)
		generated = true
	}
	in := service.SetupInput{
		BootstrapToken: tok,
		PublicURL:      cfg.Core.PublicURL,
		AdminUsername:  user,
		AdminPassword:  pass,
		// ⚠️ **不传 LDAP 段**：CLI 路径不问 LDAP，
		//    传零值会被当成"显式关掉 LDAP"，把 CR_LDAP_ENABLED=true 也覆盖掉。
	}
	// ⚠️ CLI 不问缓存 / 存储 / LDAP，向导提交的值都是**当前生效值**——
	//    被 env 钉死的项不会与生效值冲突，因此没有"值被忽略"可报。
	if _, err := svc.Submit(context.Background(), in); err != nil {
		if errors.Is(err, service.ErrSetupAlreadyDone) {
			logger.Logger.Info("实例已完成初始化")
			return
		}
		if errors.Is(err, service.ErrSetupConfigReloadFailed) {
			// 数据库里的初始化**已经提交**，失败的是运行期配置重载——
			// 退出码非 0，让部署流程知道"没完全成功"，不能当成成功放过。
			logger.Logger.Error("初始化已提交，但运行期配置重载失败，请重启实例使新配置生效", "err", err)
			os.Exit(1)
		}
		logger.Logger.Error("初始化失败", "err", err)
		os.Exit(1)
	}
	logger.Logger.Info("初始化完成", "admin", user)
	if generated {
		// 令牌只在这里出现一次（终端，不是日志）——
		// 之后重新初始化（清掉初始化标记）时需要它。
		fmt.Println()
		fmt.Println("引导令牌（请妥善保存，重新初始化时需要）：")
		fmt.Println("  " + tok)
	}
}
