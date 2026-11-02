package server

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Routes 装配全部路由。
func (a *App) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(requestID)
	// 访问日志夹在 requestID 之后——它要读 context 里的请求 ID。
	r.Use(a.accessLog)
	r.Use(securityHeaders)
	r.Use(a.withSession)

	// 公开
	r.Get("/healthz", a.handleHealthz)
	r.Get("/readyz", a.handleReadyz)

	// JWKS（仅公钥）。远程 distribution 验签 lau6-cr 签的 JWT 时要拉它，
	// 所以必须是**公开端点**——不能挂在 requireSetupDone 后面
	// （distribution 可能在 setup 完成之前就要拉）。
	r.Get("/jwks.json", a.handleJWKS)

	// 引导相关（自身不做"未初始化"拦截）
	r.Get("/setup/status", a.handleSetupStatus)
	r.Post("/setup", a.handleSetupSubmit)
	// 连通性测试：数据库 / LDAP / 缓存 / 存储。
	//
	// ⚠️ 这些端点只在**未初始化**时可用（handler → service 双重校验）：
	// 它们能连任意目标，留着就是长期可用的探测面。
	r.Post("/setup/test/database", a.handleSetupTestDatabase)
	r.Post("/setup/test/ldap", a.handleSetupTestLDAP)
	r.Post("/setup/test/cache", a.handleSetupTestCache)
	r.Post("/setup/test/storage", a.handleSetupTestStorage)
	// 数据库连接信息（预填用，**不含口令**）。同样只在未初始化时可用。
	r.Get("/setup/database", a.handleSetupDatabase)
	// 存储配置（预填用，**不含凭据**）。同样只在未初始化时可用。
	r.Get("/setup/storage", a.handleSetupStorage)

	// Docker CLI token realm
	r.Get("/token", a.handleToken)

	// distribution 事件回调：Bearer 共享密钥，非 session
	r.Post("/api/internal/registry-events", a.handleWebhook)

	// 业务 API

	// 口令加密公钥（公开端点）。
	//
	// ⚠️ **不能**挂在 /api/v1 分组里：那个分组带 requireSetupDone，
	//    而 setup 阶段前端就要用公钥加密管理员口令——
	//    挂在里面会导致"配置成 required 的实例永远初始化不了"。
	//
	// 契约见 docs/design/13-password-transport.md §4.2。
	r.Get("/api/v1/auth/pubkey", a.handlePasswordPubKey)

	r.Route("/api/v1", func(r chi.Router) {
		r.Use(a.requireSetupDone)

		r.Post("/auth/login", a.handleLogin)
		r.Post("/auth/logout", a.handleLogout)
		r.Get("/auth/me", a.handleMe)
		// 自助改密：改的是**自己**的口令，不需要管理员权限。
		r.With(a.requireLogin).Post("/auth/password", a.handleChangePassword)

		// 个人访问令牌（PAT）：用户自己生成、用来在 CI/脚本里推拉镜像。
		//
		// 原始令牌只在创建时返回一次（库里只有散列）。
		r.With(a.requireLogin).Get("/auth/tokens", a.handleListPersonalTokens)
		r.With(a.requireLogin).Post("/auth/tokens", a.handleCreatePersonalToken)
		// 改失效时间（永久 ↔ 定时）：令牌本体不变，散列不动。
		r.With(a.requireLogin).Patch("/auth/tokens/{id}", a.handlePatchPersonalToken)
		r.With(a.requireLogin).Delete("/auth/tokens/{id}", a.handleDeletePersonalToken)

		r.Get("/registry/catalog", a.handleCatalog)
		r.Get("/registry/overview", a.handleOverview)
		// ⚠️ 仓库名固定是 <project>/<repo>，<repo> 段可以带 "/"（如 team-a/sub/app）。
		//    chi 的路由树**按段匹配**：{name} 只吃一段，跨不过 "/"——
		//    用 {name} 的话 "team-a/app" 这类真实仓库名根本匹配不上（404），
		//    代理与可见性过滤全部失效。
		//    所以这里挂通配，由 handler 自己按后缀形态解析仓库名
		//    （service.ParseRegistryPath 就是干这个的）。
		r.Method(http.MethodGet, "/registry/*", http.HandlerFunc(a.handleRegistryRead))
		r.Method(http.MethodDelete, "/registry/*", http.HandlerFunc(a.handleRegistryDelete))

		r.Method(http.MethodGet, "/repositories/*", http.HandlerFunc(a.handleRepoTraffic))

		// 项目列表要求登录：匿名不该能枚举项目名（含私有项目）。
		r.With(a.requireLogin).Get("/projects", a.handleListProjects)
		// 跨项目的成员关系：一张表横看所有项目的成员分布。
		// 同样要求登录，可见范围与项目列表一致。
		r.With(a.requireLogin).Get("/project-members", a.handleListProjectMembers)
		// 创建项目是**系统级**操作：项目名是 Docker 顶层命名空间的一段，
		// 允许任意登录用户创建等于允许任何人抢注名字。
		// 见 04-api-data.md §2。
		r.With(a.requireAdmin).Post("/projects", a.handleCreateProject)
		// 项目详情：**登录即可**（04-api-data.md §3 的契约是 session）。
		//
		// 公开项目的详情会连**成员名单**一起返回，
		// 匿名用户据此就能枚举出项目成员（用户名 / 昵称 / 角色），
		// 且与 /projects/{name}/members（项目管理员）口径不一致——
		// 所以必须挂 requireLogin。
		r.With(a.requireLogin).Get("/projects/{name}", a.handleGetProject)
		r.With(a.requireProjectAdmin).Patch("/projects/{name}", a.handleUpdateProject)
		r.With(a.requireAdmin).Delete("/projects/{name}", a.handleDeleteProject)
		r.With(a.requireProjectAdmin).Get("/projects/{name}/members", a.handleListMembers)
		// 候选用户：加成员时从已有账户里挑，而不是手打用户名。
		// 字面量段 candidates 要排在 {userId} 参数段前面（chi 按段匹配）。
		r.With(a.requireProjectAdmin).Get("/projects/{name}/members/candidates", a.handleListMemberCandidates)
		r.With(a.requireProjectAdmin).Post("/projects/{name}/members", a.handleAddMember)
		r.With(a.requireProjectAdmin).Delete("/projects/{name}/members/{userId}", a.handleRemoveMember)
		r.With(a.requireLogin).Get("/projects/{name}/repositories", a.handleListProjectRepos)
		r.With(a.requireProjectAdmin).Post("/projects/{name}/repositories", a.handleCreateRepo)
		r.With(a.requireProjectAdmin).Patch("/projects/{name}/repositories/{repo}", a.handleUpdateRepo)
		// 私有仓库的显式授权：项目内的非管理员成员靠它拿到访问权。
		r.With(a.requireProjectAdmin).Get("/projects/{name}/repositories/{repo}/grants", a.handleListRepoGrants)
		r.With(a.requireProjectAdmin).Post("/projects/{name}/repositories/{repo}/grants", a.handleAddRepoGrant)
		r.With(a.requireProjectAdmin).Delete("/projects/{name}/repositories/{repo}/grants/{userId}", a.handleRemoveRepoGrant)

		// 保留策略（项目级）：覆盖全局默认。
		// 契约见 docs/design/03-registry.md §5.2。
		r.With(a.requireProjectAdmin).Get("/projects/{name}/retention", a.handleGetProjectRetention)
		r.With(a.requireProjectAdmin).Put("/projects/{name}/retention", a.handleUpdateProjectRetention)
		r.With(a.requireProjectAdmin).Delete("/projects/{name}/retention", a.handleDeleteProjectRetention)

		// 项目令牌：项目管理员为自己项目生成的长期凭据（02-auth.md §2.6.2）。
		r.With(a.requireProjectAdmin).Get("/projects/{name}/tokens", a.handleListProjectTokens)
		r.With(a.requireProjectAdmin).Post("/projects/{name}/tokens", a.handleCreateProjectToken)
		r.With(a.requireProjectAdmin).Patch("/projects/{name}/tokens/{id}", a.handlePatchProjectToken)
		r.With(a.requireProjectAdmin).Delete("/projects/{name}/tokens/{id}", a.handleDeleteProjectToken)

		r.With(a.requireLogin).Get("/settings", a.handleGetSettings)
		r.With(a.requireLogin).Get("/settings/{key}", a.handleGetSetting)
		r.With(a.requireAdmin).Patch("/settings", a.handlePatchSettings)
		r.With(a.requireAdmin).Post("/settings/{key}/reset", a.handleResetSetting)

		// 配置文件：导出、备份、回滚（14-settings.md §5.3、§6.1）。
		//
		// ⚠️ 全部要求系统管理员：回滚会覆盖配置文件，
		//    等于把运行期配置换回旧值——不是普通用户该有的权限。
		r.With(a.requireAdmin).Post("/config/export", a.handleConfigExport)
		r.With(a.requireAdmin).Get("/config/backups", a.handleConfigBackups)
		r.With(a.requireAdmin).Post("/config/rollback", a.handleConfigRollback)

		// 规则模板清单：**登录即可读**。
		//
		// 它是静态的规则元数据（模板名 / 参数 / 说明），不含任何用户数据，
		// 但**项目管理员必须能读到**——否则他们配不了规则
		// （项目级保留策略的编辑权限在 requireProjectAdmin 上）。
		// 放在 /admin 分组里会让项目管理员必然 403。
		r.With(a.requireLogin).Get("/retention/templates", a.handleListRetentionTemplates)

		// 用户管理需要系统管理员权限，但路径**不带 /admin 前缀**——
		// 与前端契约一致（前端调 /api/v1/users）。
		r.With(a.requireAdmin).Get("/users", a.handleListUsers)
		r.With(a.requireAdmin).Post("/users", a.handleCreateUser)
		r.With(a.requireAdmin).Patch("/users/{id}", a.handleUpdateUser)
		r.With(a.requireAdmin).Delete("/users/{id}", a.handleDeleteUser)
		// 管理员替用户重置口令（不验旧口令）。边界见 02-auth.md §1.2：
		// 只支持本地非管理员账号。
		r.With(a.requireAdmin).Put("/users/{id}/password", a.handleAdminResetPassword)

		// 吊销令牌：系统管理员可吊销任何人，用户也可吊销自己。
		//
		// 权限在 handler 里判（管理员 or 本人），所以这里只要求登录。
		r.With(a.requireLogin).Post("/users/{id}/revoke-tokens", a.handleRevokeUserTokens)

		r.Route("/admin", func(r chi.Router) {
			r.Use(a.requireAdmin)
			r.Get("/audit", a.handleAudit)
			r.Get("/system/storage", a.handleStorageStatus)
			r.Get("/system/traffic", a.handleSystemTraffic)
			r.Get("/system/traffic/top", a.handleSystemTrafficTop)

			// 镜像清理
			// 全局默认保留策略 + 手动执行 + 执行历史。
			// 契约见 docs/design/03-registry.md §5.2 / §5.4。
			r.Get("/cleanup/policy", a.handleGetGlobalRetentionPolicy)
			r.Put("/cleanup/policy", a.handleUpdateGlobalRetentionPolicy)
			// 全局默认 + 各项目级覆盖一览：谁压过了全局默认，这里一眼看到。
			r.Get("/cleanup/policies", a.handleListRetentionPolicies)
			r.Post("/cleanup/run", a.handleRunCleanup)
			r.Get("/cleanup/runs", a.handleListCleanupRuns)
			r.Get("/cleanup/runs/{id}", a.handleGetCleanupRun)

			// blob 回收（第 ② 层）
			// 把"已解引用但仍占盘"的 blob 清掉，磁盘空间才真正回来。
			// 契约见 docs/design/03-registry.md §5.5。
			r.Get("/cleanup/gc/status", a.handleGCStatus)
			r.Post("/cleanup/gc", a.handleRunGC)
			r.Get("/cleanup/gc/runs", a.handleListGCRuns)
			r.Get("/cleanup/gc/runs/{id}", a.handleGetGCRun)
		})
	})

	// registry 协议代理
	//
	// ⚠️ manifest 路径必须**逐方法注册**，绝不能用 r.Any：
	// chi v5 的 setEndpoint 在 mALL 分支会遍历 methodMap 覆写所有方法的
	// handler（含 mPUT），导致 manifest 守卫永不执行。
	//
	// ⚠️ 仓库名固定是 <project>/<repo>，<repo> 段可以带 "/"（如 team-a/sub/app）。
	//    chi 的路由树**按段匹配**：{name:.+} 的正则只作用于"下一个 / 之前"的
	//    那一段，跨不过 "/" —— 用 {name:.+} 的话 "team-a/app" 这类真实仓库名
	//    根本匹配不上（404），代理与 manifest 守卫全部失效。
	//    所以这里统一挂到 "/v2/*"，由 handler 自己解析仓库名
	//    （service.ParseRegistryPath 按后缀形态消歧）。
	//
	// ⚠️ 写方法（PUT/POST/PATCH/DELETE）额外挂**写屏障**：
	//    blob 回收期间挂起写操作，等在途上传排空后再扫盘。
	//    只读方法（GET/HEAD/OPTIONS）不受影响，回收期间照常 pull。
	r.Method(http.MethodPut, "/v2/*", a.withWriteBarrier(http.HandlerFunc(a.handleV2Put)))
	r.Method(http.MethodPost, "/v2/*", a.withWriteBarrier(http.HandlerFunc(a.handleProxyRegistry)))
	r.Method(http.MethodGet, "/v2/*", http.HandlerFunc(a.handleProxyRegistry))
	r.Method(http.MethodHead, "/v2/*", http.HandlerFunc(a.handleProxyRegistry))
	r.Method(http.MethodDelete, "/v2/*", a.withWriteBarrier(http.HandlerFunc(a.handleProxyRegistry)))
	r.Method(http.MethodPatch, "/v2/*", a.withWriteBarrier(http.HandlerFunc(a.handleProxyRegistry)))
	r.Method(http.MethodOptions, "/v2/*", http.HandlerFunc(a.handleProxyRegistry))

	// 静态资源（SPA fallback）
	r.Get("/*", a.handleStatic)

	return r
}

// isAPIPath 判断路径是否属于 API 命名空间。
//
// SPA fallback 必须排除这些前缀，否则前端会拿到 HTML 却按 JSON 解析。
//
// ⚠️ "/setup" 必须和 "/setup/" 分开：
// "/setup" 是**引导页**（浏览器要拿到 index.html 才能跑 SPA，前端路由里也有这条），
// "/setup/status" 与 "POST /setup" 才是 API。
// 把 "/setup" 整个当 API 前缀，浏览器直接开 /setup 会拿到 404 JSON，
// 引导向导根本进不去（未初始化实例访问 / 也是死路）。
func isAPIPath(p string) bool {
	for _, prefix := range []string{"/api/", "/v2/", "/healthz", "/readyz", "/jwks.json"} {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	// realm 端点就是 "/token" **本身**。
	//
	// ⚠️ 不能用前缀匹配：它会把 "/tokens"（前端路由「访问令牌」）
	//    一起当成 API，浏览器直接开该页面拿到的是 404 JSON 而不是 SPA，
	//    刷新即 404——页面在应用内跳转正常、刷新就断。
	if p == "/token" {
		return true
	}
	// 引导页本体（带不带尾斜杠都算）走 SPA。
	if p == "/setup" || p == "/setup/" {
		return false
	}
	return strings.HasPrefix(p, "/setup/")
}
