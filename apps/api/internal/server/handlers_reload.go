package server

import (
	"github.com/lau6code/lau6-cr/apps/api/internal/config"
	"github.com/lau6code/lau6-cr/apps/api/internal/service"
)

// onSettingsChanged 把新配置应用到各组件。
//
// 它不会失败：能热切换的项都在这里直接生效；
// 热切换本身失败的两项（缓存后端、日志级别）各自记 Error 日志并沿用旧值继续跑
// （见 reloadCacheIfChanged / reloadLoggingIfChanged 的注释），
// 并把对应的设置项标成"重启后生效"，让用户从设置页看得出来。
//
// ⚠️ 存储参数**不在这里**处理：storage.* 在设置页上全部只读
// （11-storage.md §6），Apply 根本收不到它们。
// 存储后端只在 setup 阶段选定，那边会重渲染 + 重启 distribution。
func (a *App) onSettingsChanged() {
	// 旧配置留一份：热切换失败时要拿它和新配置比，
	// 才能说出"具体哪几项没换过来"。
	old := *a.Cfg
	cfg := a.SettingsSvc.Snapshot()
	a.Stats.SetEnabled(cfg.Stats.Enabled)
	// 匿名 IP 记录同样要立刻生效（RecordIPFn 只在每轮刷盘时刷新，
	// 那最多有一个刷盘周期的延迟；这里显式推一次，管理员改完即生效）。
	a.Stats.SetRecordIP(cfg.Stats.RecordIP)
	// 刷盘周期与桶数上限也是 T0：改完立刻生效，不必等重启。
	// 周期是"崩溃时的统计丢失窗口"——晚一个周期才生效就是多丢一个周期的数据。
	a.Stats.SetInterval(cfg.Stats.FlushInterval)
	a.Stats.SetMaxBuckets(cfg.Stats.MaxBuckets)
	a.Cfg.Core.PublicURL = cfg.Core.PublicURL
	a.Cfg.Core.TokenTTLSeconds = cfg.Core.TokenTTLSeconds
	a.Cfg.Core.SessionTTLSeconds = cfg.Core.SessionTTLSeconds
	a.Cfg.Core.TrustedProxies = cfg.Core.TrustedProxies
	a.Cfg.Access.AnonymousReadEnabled = cfg.Access.AnonymousReadEnabled
	// 可信反代列表变了要立刻生效——它决定 X-Forwarded-Proto 是否可信。
	a.SetTrustedProxies(cfg.Core.TrustedProxies)
	// 缓存后端热切换：driver 或 Redis 连接参数变了就重建后端，
	// 新后端立即接管，不等重启。
	a.reloadCacheIfChanged(cfg, &old)
	// 日志级别热生效：排障时在设置页调级别要立刻见效，不用重启容器。
	a.reloadLoggingIfChanged(cfg, &old)
}

// 只处理级别：它由 slog.LevelVar 承载，改一下就原子生效。
//
// 文件相关参数（目录、文件名、大小上限、滚动方式、保留天数）不在这里处理——
// 改它们要重建文件写入器，涉及正在写的文件句柄，
// 走重启更稳妥（对应设置项的 Tier 是 T2）。
func (a *App) reloadLoggingIfChanged(cfg *config.Config, old *config.Config) {
	if a.ReloadLogging == nil {
		return
	}
	if a.Cfg.Logging.Level == cfg.Logging.Level {
		return
	}
	if err := a.ReloadLogging(cfg); err != nil {
		// 级别改不动就保持原样——宁可日志级别不对，
		// 也不能因为一次设置保存让整个服务出问题。
		//
		// ⚠️ 这一项**设置已保存**，只是运行期没换过来：
		//    所以要把它标成"重启后生效"（设置页会显示出来），
		//    否则用户只会看到"已保存"，以为改完就管用了。
		a.Logger.Error("切换日志级别失败：设置已保存，运行期沿用旧级别，重启后生效", "err", err)
		a.markPendingRestart(old, cfg, "logging.")
		return
	}
	a.Cfg.Logging.Level = cfg.Logging.Level
	a.clearPendingRestart(old, cfg, "logging.")
}

// 只看连接参数：不影响"连到哪个后端"的项不该为它重建客户端。
func (a *App) reloadCacheIfChanged(cfg *config.Config, old *config.Config) {
	if a.ReloadCache == nil {
		return
	}
	if !cacheConnChanged(a.Cfg.Cache, cfg.Cache) {
		return
	}
	if err := a.ReloadCache(cfg); err != nil {
		// 重建失败就保持旧后端继续跑——总比整个服务起不来强。
		//
		// ⚠️ 与日志级别同理：这一项**设置已保存**，只是运行期没换过来，
		//    所以要标成"重启后生效"，让设置页显示出来。
		a.Logger.Error("切换缓存后端失败：设置已保存，运行期沿用旧后端，重启后生效", "err", err)
		a.markPendingRestart(old, cfg, "cache.")
		return
	}
	a.Cfg.Cache = cfg.Cache
	a.clearPendingRestart(old, cfg, "cache.")
}

// markPendingRestart 把"已保存但运行期没换过来"的设置项标成待重启。
//
// 这些项必须让用户看得见（AGENTS.md 规则 1：调用方要能看出失败过）：
// 设置页会把它们列进"重启后生效"，而不是让用户以为改完就当场管用了。
func (a *App) markPendingRestart(old, new *config.Config, prefix string) {
	keys := service.ChangedSettingKeys(old, new, prefix)
	if len(keys) == 0 {
		return
	}
	a.SettingsSvc.MarkPendingRestart(keys...)
}

// clearPendingRestart 撤掉已经生效的设置项的待重启标记——
// 留着就是假提示。
func (a *App) clearPendingRestart(old, new *config.Config, prefix string) {
	keys := service.ChangedSettingKeys(old, new, prefix)
	if len(keys) == 0 {
		return
	}
	a.SettingsSvc.ClearPendingRestart(keys...)
}

// 判断缓存连接参数是否变了。
//
// 实现放在 config.CacheConfig.ConnChanged —— 设置页与初始化向导
// 两处都要用同一份判定，免得两边的"变了"标准不一致。
func cacheConnChanged(a, b config.CacheConfig) bool {
	return a.ConnChanged(b)
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
