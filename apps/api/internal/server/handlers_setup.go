package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/lau6code/lau6-cr/apps/api/internal/service"
)

func (a *App) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	st, err := a.SetupSvc.Status(r.Context())
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "读取引导状态失败")
		return
	}
	OK(w, st)
}

func (a *App) handleSetupSubmit(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	// setup 的请求体 = service.SetupInput + 可选的应用层加密信封
	//（password.mode != plaintext 时，管理员口令在信封里）。
	var in struct {
		service.SetupInput
		Kid   string `json:"kid"`
		Epk   string `json:"epk"`
		Nonce string `json:"nonce"`
		Ct    string `json:"ct"`
	}
	if !decodeJSONBody(w, r, &in) {
		return
	}
	// 口令来源（明文 / 密文）：管理员口令也要过模式闸门。
	secret, err := a.resolvePassword(r.Context(),
		passwordEnvelope{Kid: in.Kid, Epk: in.Epk, Nonce: in.Nonce, Ct: in.Ct},
		in.AdminUsername, in.AdminPassword, "", false)
	if err != nil {
		a.failPassword(w, err)
		return
	}
	in.AdminPassword = secret.Password
	res, err := a.SetupSvc.Submit(r.Context(), in.SetupInput)
	if err != nil {
		switch {
		case err == service.ErrSetupAlreadyDone:
			Fail(w, http.StatusConflict, CodeConflict, "实例已完成初始化")
		case err == service.ErrBadBootstrapToken:
			Fail(w, http.StatusUnauthorized, CodeUnauthorized, "引导令牌不正确")
		case errors.Is(err, service.ErrCurrentDatabaseUnreadable):
			// 服务端自己的配置坏了，不是请求写错了 → 5xx。
			Fail(w, http.StatusInternalServerError, CodeInternal, err.Error())
		case errors.Is(err, service.ErrSetupConfigReloadFailed):
			// ⚠️ 数据库里的初始化**已经提交**，失败的是运行期配置重载。
			//    这是服务端故障，不是请求写错了——回 400 会让运维以为
			//    "改一下参数重试"就行，实际必须重启实例。
			Fail(w, http.StatusInternalServerError, CodeInternal,
				"初始化已完成，但运行期配置重载失败，请重启实例使新配置生效")
		default:
			Fail(w, http.StatusBadRequest, CodeInvalidParam, err.Error())
		}
		return
	}
	// 初始化后重建配置快照，并渲染 distribution 配置。
	//
	// ⚠️ 重载失败必须报出来：数据库里的初始化**已经提交**，
	// 但运行期配置还停在旧值上——实例处在
	// "数据库已初始化、内存配置是旧的"的分裂状态。
	// 这时候回一句"初始化完成"是假成功（AGENTS.md 规则 1 / 3）。
	if err := a.reloadAfterSetup(); err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal,
			"初始化已完成，但运行期配置重载失败，请重启实例使新配置生效")
		return
	}
	// ⚠️ 被环境变量钉死、因而被忽略的项**必须回报**：
	//    向导里填的值没生效，用户看不见就会以为"填了没用"。
	//    env 优先级最高本身不是错误，但"没生效"必须让人知道。
	OK(w, map[string]any{
		"initialized":        true,
		"ignored_env_pinned": res.IgnoredEnvPinned,
	})
}

// 重载失败时返回错误，由调用方决定怎么报——
// 这里**不能**只记日志了事：日志记了、行为没变，
// 等于把"配置没生效"藏起来（AGENTS.md 规则 3）。
func (a *App) reloadAfterSetup() error {
	if err := a.ReloadConfig(); err != nil {
		a.Logger.Error("初始化后重载配置失败", "err", err)
		return err
	}
	// 同步运行期配置里的缓存段：向导里换过缓存后端，
	// a.Cfg 还停在启动时的值上——设置页拿它做"变了没有"的判定，
	// 不同步会让它一直以为缓存没换过。
	if a.SettingsSvc != nil {
		a.Cfg.Cache = a.SettingsSvc.Snapshot().Cache
	}
	return nil
}

// setupProbe 收敛四个 setup 连通性测试端点（数据库 / 缓存 / LDAP / 存储）：
// 除了输入类型与 service 方法名，它们完全同构。
//
// label 是失败时写进日志的文案——四个端点各有一句，逐字保留。
func setupProbe[I any, R any](a *App, w http.ResponseWriter, r *http.Request, label string,
	probe func(ctx context.Context, in I) (*R, error)) {
	w.Header().Set("Cache-Control", "no-store")
	var in I
	if !decodeJSONBody(w, r, &in) {
		return
	}
	res, err := probe(r.Context(), in)
	if err != nil {
		a.Logger.Warn(label, "ip", clientIP(r), "err", err)
		writeProbeError(w, err)
		return
	}
	OK(w, res)
}

// ⚠️ 只在**未初始化**时可用（service 层校验）——
// 初始化完成后这个"能连任意目标"的探测接口必须关闭。
func (a *App) handleSetupTestDatabase(w http.ResponseWriter, r *http.Request) {
	setupProbe(a, w, r, "setup 数据库连通性测试失败",
		func(ctx context.Context, in service.DatabaseTestInput) (*service.ConnectivityResult, error) {
			return a.SetupSvc.TestDatabase(ctx, in)
		})
}

// ⚠️ **不含口令**：未初始化时这个端点是开放面（那时还没有任何账号），
// 口令绝不能回显。
//
// ⚠️ 只在**未初始化**时可用：它会暴露主机 / 用户名等拓扑信息，
// 已初始化的实例不该留着这个口。
func (a *App) handleSetupDatabase(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	initialized, err := a.isInitialized(r.Context())
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "读取初始化状态失败")
		return
	}
	if initialized {
		Fail(w, http.StatusConflict, CodeConflict, "实例已完成初始化")
		return
	}
	info, err := a.SetupSvc.DatabaseInfo(r.Context())
	if err != nil {
		a.Logger.Warn("读取当前数据库配置失败", "ip", clientIP(r), "err", err)
		Fail(w, http.StatusInternalServerError, CodeInternal, "读取当前数据库配置失败："+err.Error())
		return
	}
	OK(w, info)
}

// ⚠️ **不含凭据**：未初始化时这个端点是开放面（那时还没有任何账号），
// 密钥绝不能回显——只给"配没配"。
//
// ⚠️ 只在**未初始化**时可用：它会暴露对象存储的地址与桶名，
// 已初始化的实例不该留着这个口。
func (a *App) handleSetupStorage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	initialized, err := a.isInitialized(r.Context())
	if err != nil {
		Fail(w, http.StatusInternalServerError, CodeInternal, "读取初始化状态失败")
		return
	}
	if initialized {
		Fail(w, http.StatusConflict, CodeConflict, "实例已完成初始化")
		return
	}
	info, err := a.SetupSvc.StorageInfo(r.Context())
	if err != nil {
		a.Logger.Warn("读取当前存储配置失败", "ip", clientIP(r), "err", err)
		Fail(w, http.StatusInternalServerError, CodeInternal, "读取当前存储配置失败："+err.Error())
		return
	}
	OK(w, info)
}

// 约束同 handleSetupTestDatabase：只在未初始化时可用。
func (a *App) handleSetupTestCache(w http.ResponseWriter, r *http.Request) {
	setupProbe(a, w, r, "setup 缓存连通性测试失败",
		func(ctx context.Context, in service.CacheTestInput) (*service.CacheTestResult, error) {
			return a.SetupSvc.TestCache(ctx, in)
		})
}

// 约束同 handleSetupTestDatabase：只在未初始化时可用。
func (a *App) handleSetupTestLDAP(w http.ResponseWriter, r *http.Request) {
	setupProbe(a, w, r, "setup LDAP 连通性测试失败",
		func(ctx context.Context, in service.LDAPTestInput) (*service.ConnectivityResult, error) {
			return a.SetupSvc.TestLDAP(ctx, in)
		})
}

// 三类分开报，别混成一句"测试失败"：
//
//   - 参数不合法   → 400（用户填错了）
//   - 令牌不对     → 401
//   - 连不上       → 503（依赖不可用），并带上原始原因
//
// ⚠️ 失败**不返回 200**：测试失败就是失败，
// 用 200 带一个 ok=false 的对象去"假装成功"是本仓库明令禁止的兜底。
func writeProbeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrSetupAlreadyDone):
		Fail(w, http.StatusConflict, CodeConflict, "实例已完成初始化")
	case errors.Is(err, service.ErrBadBootstrapToken):
		Fail(w, http.StatusUnauthorized, CodeUnauthorized, "引导令牌不正确")
	case errors.Is(err, service.ErrBadBootstrapTokenFormat):
		Fail(w, http.StatusBadRequest, CodeInvalidParam, err.Error())
	case errors.Is(err, service.ErrInvalidProbeParam):
		Fail(w, http.StatusBadRequest, CodeInvalidParam, err.Error())
	case errors.Is(err, service.ErrCurrentDatabaseUnreadable):
		// ⚠️ 服务端自己的配置坏了，不是用户填错了——回 5xx。
		//    归到 400/503 会把排查方向指错。
		Fail(w, http.StatusInternalServerError, CodeInternal, err.Error())
	default:
		Fail(w, http.StatusServiceUnavailable, CodeUnavailable, "连接失败："+err.Error())
	}
}

// ⚠️ 只在**未初始化**时可用（service 层校验）——
// 初始化完成后这个"能连任意目标的探测接口"必须关闭。
func (a *App) handleSetupTestStorage(w http.ResponseWriter, r *http.Request) {
	setupProbe(a, w, r, "setup 存储连通性测试失败",
		func(ctx context.Context, in service.StorageTestInput) (*service.StorageTestResult, error) {
			return a.SetupSvc.TestStorage(ctx, in)
		})
}
