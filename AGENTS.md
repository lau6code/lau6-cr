# AGENTS.md

lau6-cr（私有容器镜像仓库）的项目级 Agent 规则。Go 后端 + Vue 3 前端 + SQLite/Redis 缓存 + LDAP 认证。

本文件对**所有** agent（mavis / worker / explore / verifier，以及其他会读 `AGENTS.md` 的工具）在本仓库的**每次任务**生效。

设计文档在 `docs/design/`，`docs/design/README.md` 是索引。改接口、配置、数据模型、鉴权逻辑之前，先读对应章节再动手。

## 四条硬规则

### 1、禁止兜底

失败必须暴露到调用方，不能用默认值、空结果、占位值或"假成功"糊过去。

- Go：`err != nil` 必须被处理——返回、包装（`fmt.Errorf("...: %w", err)`）或重新抛出，绝不 `_ = err`。
- HTTP handler 出错必须返回对应状态码（4xx/5xx），不能返回 200 + 空 body 或 200 + 伪造数据。
- 前端：请求失败必须提示用户，不能 catch 之后静默渲染成"暂无数据"。
- 本仓库**故意**的降级设计（例如 registry 的 reconciler 自愈路径、清理执行里"审计写失败仍继续删除但把运行落成 failed"这类）是允许的——但它们必须在代码里点名、写清为什么可以降级、并且运行时可观测。除此之外不许自行发明默认值兜底。
- 反过来，**没有消费点的配置项就是假开关**：设置页上能改、后端没有任何代码读它 = 改了不起作用却显示"已保存"。要么实现它，要么删掉它（配置项、设置项、文档一并删）。本仓库不提供 `cache.redis.fail_open` 这类开关——见 `docs/design/12-cache.md` §2.1。

**判定线：如果主路径失败了，而调用方看不出失败过 → 兜底违规。**

### 2、禁止猜测

- 任何结论必须来自当前仓库的真实代码、配置或命令输出：先 `grep`、先 `read`、先跑一遍。
- 不许臆造 API 名、函数签名、配置键、文件路径、版本号、命令行参数。
- 报告里的路径、行号、报错原文、接口字段名，必须来自工具输出或真实读过的文件。
- 拿不准就明确说不确定，并写清已经查过什么、还差什么。

**判定线：结论若来自"印象"而不是工具输出 → 猜测违规。**

### 3、禁止静默异常

- 禁止吞异常：空 `catch`、忽略返回值、catch 之后当没事发生继续往下走，都不行。
- catch 之后只有三种合法处理：
  1. 转成调用方可见的失败（返回错误 / 抛到前端 / 返回 5xx）；
  2. 记日志**并且改变行为**（重试、跳过并上报、显式降级），日志级别要反映真实严重性；
  3. 确实处理不了 → 重新抛出。
- **记了日志却仍然返回成功** = 静默失败，同样违规。
- 确实要容忍的异常，必须在注释里写清"为什么可以忽略"，并留下可观测手段（计数、指标或日志）。

**判定线：执行到一半失败，最终却返回成功/空结果 → 静默异常违规。**

### 4、只留现状，不留历史

- 注释只解释"为什么这么做"，不复述代码在做什么。删掉描述旧实现、记录改动过程、标注"以前/原来/已废弃"的注释。
- 代码同样只留现在还在用的：死函数、注释掉的代码块、无人消费的开关、过期 TODO/FIXME，该删就删。确实要留的 TODO，必须写清为什么现在还不能做。
- 不为"以后也许要回滚"保留旧实现、旧配置项、旧文档章节——历史在 Git 里，不在仓库里。
- 以现状为准：文档、注释、代码描述同一件事时，只描述当前实现，不写"曾经是 X，现在改成 Y"。
- 例外：规则 1、3 要求保留的说明（为什么允许降级、为什么可以忽略某个异常）不在清理范围内。

**判定线：删掉某段注释/代码后当前实现不受影响 → 它就是历史包袱，删掉。**

## 验证要求

- 后端改动：`make test`（= `cd apps/api && go test ./...`）、`make vet`、必要时 `make fmt`。
- 前端改动：`make build-web`（= `pnpm install --frozen-lockfile && pnpm run build`，内含 `vue-tsc` 类型检查）。前端**没有单测框架**，只能靠类型检查 + 手动走查，交付时要说明走查了什么。
- 改了界面文案（`apps/web/src/locales/*.ts`）还要跑 `make check-i18n`：它校验三种语言包的键结构一致、代码里引用的键都存在、同一条文案的占位符一致。漏键时 vue-i18n 会把**键名本身**当文案显示出来（界面上出现 `settings.groupBasic` 这种鬼东西），而这在生产构建里是静默的。
- 改了前端加密（`apps/web/src/api/passwordCrypto.ts`）还要跑 `make check-crypto`：它用前端真实实现复现共享协议向量，Go 侧用同一份向量验解密。前后端互操作错了只表现为"解密失败"，没有别的自动化守门。
- 增删设置项（`apps/api/internal/service/settings_defs.go` 里的 `AllSettingDefs`）还要跑 `make check-settings-keys`：它比对后端定义、前端设置页分组（`apps/web/src/views/admin/SettingsView.vue` 的 `GROUPS`）、设计文档 `14-settings.md` 三处清单。漏一处 = 设置页上少一个入口或文档与实现对不上，而两边都不会报错。
- 完整产物构建必须走 `make build`，顺序固定为 **前端 → 拷贝进 embed 目录 → 编译 Go**：`go:embed` 在编译期固化内容，顺序反了会把旧产物嵌进二进制。
- 没跑成的检查必须如实报告，不许用"应该没问题"代替实测结果。

## 仓库速查

| 位置 | 内容 |
|---|---|
| `apps/api` | Go 后端，模块名 `github.com/lau6code/lau6-cr/apps/api`（Go 1.26）；`cmd/lau6-cr` 是入口，`internal/` 是实现 |
| `apps/web` | Vue 3.5 + TypeScript + Element Plus + Pinia + Vite 6（pnpm）；界面文案在 `apps/web/src/locales/`（zh-CN / zh-TW / en），`scripts/check-i18n.mjs` 校验三者一致性；口令加密用 `@noble/ciphers` / `@noble/hashes`（**不依赖 WebCrypto**——`crypto.subtle` 只在安全上下文可用） |
| `apps/api/internal/web/dist` | 前端产物嵌入目录，真实产物与 `placeholder/`、`.gitkeep` 并存；**占位文件不能删**，否则干净 checkout 编译不过 |
| `docs/design/` | 设计文档（中文），接口与数据契约见 `04-api-data.md`，鉴权见 `02-auth.md`，配置项见 `14-settings.md` |
| `docker/`、`docker-compose.yaml` | 部署与运行时编排 |

## 其他约定

- 临时脚本、构建产物、验证数据放 `.dev-scratch/`（已 gitignore），不要污染仓库根目录；交付前清掉自己造的临时件。
- **别在仓库工作区里放持久数据**。容器的数据卷是 Docker **命名卷**（`lau6-cr_lau6-cr-data`），不在仓库里——工作区的任何清理动作都碰不到它。若改成绑定挂载到仓库目录（如 `./data/...`），`git clean -xfd` 之类操作会把数据库、配置和镜像 blob 一起删掉，宿主目录一消失容器里的 `/data` 就成了死挂载（`mkdir` 报 ENOENT）。见 `docs/design/05-build-deploy.md`。
- **前端表单带 `rules` 时要设 `validate-on-rule-change="false"`**。规则文案来自语言包，切语言会换掉 rules 对象；Element Plus 默认会在规则变化时顺手校验一次，于是空表单上凭空冒出必填错误。已有 5 处：`LoginView` / `ProjectsView` / `admin/UsersView` / `ProjectEditDialog` / `AppHeader`（改密弹窗）。
- 设计文档与实现冲突时，以**文档为准**并顺手修实现；文档本身过期就在同一轮里修掉，不要留下两套说法。
