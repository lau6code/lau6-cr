/** 后端 ID 可能是数字或字符串，统一放宽。 */
export type Id = number | string

export interface User {
  id: Id
  username: string
  display_name: string
  is_system_admin: boolean
  /** 账号来源：local | ldap。LDAP 账号的口令由目录维护，不能自助改密。 */
  source: string
  disabled: boolean
}

/** GET /api/v1/auth/me 的响应。匿名时 anonymous=true 且没有 user。 */
export interface MeResponse {
  anonymous: boolean
  user?: User
}

export interface LoginResult {
  user: User
}

export interface Repository {
  /** 完整 Docker 仓库名，形如 team-a/myapp */
  name: string
  project: string
  latest_tag: string
  tag_count: number
  /** 字节 */
  size: number
  created: string
  platforms: string[]
}

export interface RegistryOverview {
  repositories: Repository[]
  /**
   * 拼 `docker pull` 命令用的前缀（host[:port]，不含协议）。
   *
   * 后端从 core.public_url 取；取不到时为空串。
   */
  pull_prefix?: string
}

/** 单个 tag 的度量结果。 */
export interface TagInfo {
  /** 标签名 */
  tag: string
  /** 镜像创建时间（ISO 8601）；上游没给时为空串 */
  created: string
  /** 镜像大小（字节） */
  size: number
  /** 覆盖的平台，如 linux/amd64 */
  platforms: string[]
  /** manifest 摘要——删除只能按它来 */
  digest: string
  /**
   * 这一条 tag 的失败原因（拉不到 manifest / 度量失败）。
   *
   * 空 = 正常。**非空说明这一条没取到完整信息**，
   * 不能当成"这个 tag 没有这些信息"——两者在字段上长得一样，
   * 页面必须区分开，否则用户只知道少了一个 tag 却不知道为什么。
   */
  error?: string
}

/** 删除操作的响应：deleted 是删掉的数量 / 摘要。 */
export interface RegistryDeleteResult {
  /** 删掉的 tag 数（删整个镜像时）或 manifest 摘要（删单个 tag 时） */
  deleted: number | string
  /**
   * 删除后的收尾失败原因（目录刷新 / 清理空仓库记录失败）。
   *
   * 删除本身成功了，但目录可能残留空条目——必须让用户看见，
   * 不能悄悄咽下去。
   */
  cleanup_warning?: string
}

/** GET /api/v1/registry/{name}/tags 的响应。 */
export interface RepoTags {
  /** 完整仓库名，形如 team-a/myapp */
  name: string
  /** 各 tag 的度量 */
  tags: TagInfo[]
  /**
   * 拼 docker pull 命令用的前缀（host[:port]）。
   *
   * 后端从 core.public_url 取；取不到时为空串，
   * 前端退回浏览器当前地址（与 Harbor 的做法一致）。
   */
  pull_prefix?: string
}

export type ProjectVisibility = 'public' | 'private'
export type ProjectRole = 'admin' | 'developer' | 'reader'

export interface Project {
  name: string
  display_name: string
  description: string
  visibility: ProjectVisibility
  member_count: number
  repo_count: number
  /**
   * 项目内所有仓库的镜像大小之和（字节）。
   *
   * 后端汇总的是 repo_meta 缓存里的 size，反映的是**最近一次刷新**的统计值，
   * 与镜像目录页每个仓库的"大小"列同源。
   */
  total_size: number
  /**
   * 当前查看者在本项目里的角色；null / 缺省 = 不是成员。
   *
   * 后端按请求主体计算，同一个项目对不同人返回不同的值。
   * 编辑项目要项目管理员（my_role === 'admin'）或系统管理员。
   */
  my_role?: ProjectRole | null
}

export interface ProjectMember {
  user_id: Id
  username: string
  display_name: string
  role: ProjectRole
}

export interface ProjectDetail {
  project: Project
  members: ProjectMember[]
}

/** 跨项目的成员关系（"项目 × 用户"），GET /api/v1/project-members。 */
export interface ProjectMembership {
  project: string
  user_id: Id
  username: string
  display_name: string
  role: ProjectRole
  created_at: number
}

/**
 * 可加入某项目的候选用户，GET /api/v1/projects/{name}/members/candidates。
 * current_role 为空 = 尚未加入该项目。
 */
export interface MemberCandidate {
  user_id: Id
  username: string
  display_name: string
  current_role: ProjectRole | null
  source: string
  is_system_admin: boolean
}

export interface RegistryUser {
  id: Id
  username: string
  display_name: string
  is_system_admin: boolean
  disabled: boolean
  /** local | ldap */
  source: string
}

type SettingSource = 'env' | 'db' | 'yaml' | 'default'
type SettingTier = 'T0' | 'T1' | 'T2'

interface SettingConstraints {
  min?: number
  max?: number
  max_items?: number
  pattern?: string
}

export interface SettingItem {
  key: string
  value: unknown
  default: unknown
  source: SettingSource
  tier: SettingTier
  editable: boolean
  /** 设置项类型：bool | int | string | duration | strings | time。 */
  type?: string
  description?: string
  constraints?: SettingConstraints
  enum?: string[]
  env_var?: string
  hint?: string
  pending_restart?: boolean
}

export interface SettingsPayload {
  version: number
  exported_at: string
  export_stale: boolean
  pending_restart: string[]
  settings: SettingItem[]
}

export interface SetupStatus {
  initialized: boolean
  require_bootstrap_token: boolean
  database_driver: string
  /** 被环境变量钉死、向导改不了的设置项。 */
  env_pinned: SetupEnvPinned[]
  /**
   * 当前**生效**的公共访问 URL。
   *
   * ⚠️ 被环境变量钉死时向导里的那一栏是只读的，
   *    必须把生效值显示出来——否则用户看到的是一个空的只读输入框，
   *    又过不了"必须是 http(s) 开头"的校验，初始化根本走不下去。
   */
  public_url: string
  /**
   * distribution 是否与本进程同容器。
   *
   * ⚠️ external 形态下向导**不显示存储步骤**：blob 存不存对象存储
   * 是上游 distribution 自己的事，lau6-cr 这边没有 blob 可放。
   */
  distribution_embedded: boolean
  /**
   * 当前**生效**的存储后端。
   *
   * 被 env 钉死时向导里的那一栏是只读的，得让用户看见生效值。
   */
  storage_driver: string
  /**
   * 当前**生效**的缓存后端。
   *
   * ⚠️ 向导必须按它显示：生效的是 redis 而向导显示 local 时，
   *    「选 redis 要填数据库编号」这条要求会被整段跳过。
   */
  cache_driver: string
  /**
   * 当前**生效**的 Redis 数据库编号。
   *
   * ⚠️ 被 env 钉死时向导里的那一栏是只读的，
   *    空着的只读输入框会被当成"没填"——必须把生效值显示出来。
   */
  cache_redis_db: number
}

/**
 * 数据库连接信息（向导里的那几个输入框）。
 *
 * sqlite 用 sqlite_path；postgres / mysql 用 host + port + username +
 * password + name（+ postgres 的 schema）。
 */
interface SetupDatabase {
  driver: string
  sqlite_path: string
  host: string
  port: string
  username: string
  password: string
  name: string
  schema: string
  ssl_mode: string
}

/**
 * 当前生效的数据库连接信息（预填用）。
 *
 * ⚠️ **不含口令**——未初始化时这是开放面，口令不能回显。
 * env_var 非空 = 数据库配置被环境变量钉死，向导里改不动。
 */
export interface SetupDatabaseInfo {
  driver: string
  host: string
  port: string
  username: string
  name: string
  schema: string
  ssl_mode: string
  sqlite_path: string
  env_var: string
}

export interface SetupCache {
  driver: string
  local: { max_entries: number }
  redis: {
    addr: string
    password: string
    prefix: string
    /**
     * Redis 数据库编号。
     *
     * ⚠️ null = 没填。选 redis 时**必填**——db 0 常被别的应用占着，
     *    默默用 0 等于把签名密钥写进别人的库。
     */
    db: number | null
  }
}

/**
 * 被环境变量钉死的设置项。
 *
 * env 的优先级最高（env > system_settings > application.yaml > 默认值），
 * 被钉死的项向导改不了——改了也是白改：运行期短暂按向导的值走，
 * 重启后 env 又盖回去。向导据此把这些字段显示成只读。
 */
export interface SetupEnvPinned {
  key: string
  env_var: string
}

interface SetupLdap {
  enabled: boolean
  url: string
  dc: string
  user_segment: string
  start_tls: boolean
  allow_create: boolean
  timeout_sec: number
}

/**
 * 镜像存储配置（blob 放哪）。
 *
 * ⚠️ **初始化后不可改**（docs/design/11-storage.md §6）：
 * 改后端不会让已存在的 blob 出现在新位置。
 */
export interface SetupStorage {
  driver: string
  s3: {
    endpoint: string
    region: string
    bucket: string
    path_style: boolean
    /** 留空表示走环境变量 CR_S3_ACCESS_KEY / CR_S3_SECRET_KEY。 */
    access_key: string
    secret_key: string
  }
}

/**
 * 当前生效的存储配置（预填用，**不含凭据**）。
 *
 * ⚠️ 未初始化时这是开放面，密钥不能回显——
 * 只给 access_key_set / secret_key_set 表示"配没配"。
 */
export interface SetupStorageInfo {
  driver: string
  local: { root: string }
  s3: {
    endpoint: string
    region: string
    bucket: string
    path_style: boolean
    access_key_set: boolean
    secret_key_set: boolean
  }
}

export interface SetupRequest {
  bootstrap_token: string
  public_url: string
  admin_username: string
  admin_password: string
  anonymous_read: boolean
  database: SetupDatabase
  cache: SetupCache
  ldap: SetupLdap
  /**
   * 镜像存储配置。
   *
   * ⚠️ external 形态（distribution 独立部署）**不传这一段**：
   * 那时 blob 存不存对象存储是上游自己的事，lau6-cr 这边没有 blob 可放。
   */
  storage?: SetupStorage
}

/** 连通性测试的结果（后端 /setup/test/* 返回）。 */
export interface SetupTestResult {
  /** 被测目标（DSN / URL），回显给用户确认测的是哪个。 */
  target: string
  /** 握手 + ping 往返耗时（毫秒）。 */
  latency_ms: number
  /** 人类可读的结果说明。 */
  detail: string
  /** 实际生效的 schema（postgres 的 search_path / mysql 的当前库）。 */
  schema?: string
}

/** 缓存连通性测试的结果（后端 /setup/test/cache 返回）。 */
export interface CacheTestResult {
  driver: string
  /** 被测目标（Redis 地址），不含口令。 */
  target: string
  latency_ms: number
  detail: string
  /**
   * 配置风险提示（淘汰策略、持久化、requirepass）。
   *
   * 它们不阻断初始化——Redis 怎么配是运维的判断，
   * 但不提示用户会以为"地址密码填对了就万事大吉"。
   */
  warnings: string[]
  /** 读到的 Redis 关键配置（读不到时字段为空）。 */
  info?: {
    version: string
    maxmemory_policy: string
    append_only: string
    require_pass: boolean | null
  }
}

/** 数据库连通性测试的入参。 */
export interface SetupDatabaseTestRequest {
  bootstrap_token: string
  database: SetupDatabase
}

/**
 * LDAP 连通性测试的入参。
 *
 * username / password 可选：填了就按 user_segment + dc 拼 DN 做一次
 * simple bind（验证完整登录路径）；不填只测连接。
 */
export interface SetupLdapTestRequest {
  bootstrap_token: string
  url: string
  dc: string
  user_segment: string
  start_tls: boolean
  timeout_sec: number
  username: string
  password: string
}

/** 缓存连通性测试的入参。 */
export interface SetupCacheTestRequest {
  bootstrap_token: string
  cache: SetupCache
}

/** 存储连通性测试的入参。 */
export interface SetupStorageTestRequest {
  bootstrap_token: string
  storage: SetupStorage
}

/**
 * 存储连通性测试的结果（后端 /setup/test/storage 返回）。
 *
 * ⚠️ 探测失败**不阻断**向导：setup 所在的管理网可能访问不到对象存储，
 * 而运行期网络是通的（docs/design/11-storage.md §4.4）。
 * 所以失败时向导会问一句"仍要继续吗"，而不是硬拦。
 */
export interface StorageTestResult {
  /** 人类可读的结果说明。 */
  detail: string
  /** 探测耗时（毫秒）。 */
  latency_ms: number
  /** "能连上但有风险"的提示。 */
  warnings?: string[]
}

// ── 镜像清理（保留策略）──────────────────────────────────────

/** 保留策略里的一条规则。 */
export interface RetentionRule {
  /** 规则模板，见 RetentionTemplate.template。 */
  template: string
  /** 模板参数，如 {"k": 5} / {"days": 7}；keep_always 为空对象。 */
  params: Record<string, number>
  /** 该规则自己的 tag 选择器；空串 = 用策略级的 tag_selector。 */
  tag_selector?: string
}

/**
 * 保留策略。
 *
 * 全局默认与项目级覆盖是同一套结构，靠 project_id 区分：
 * 没有 project_id = 全局默认，有值 = 项目级。
 */
export interface RetentionPolicy {
  /** 0 表示这条策略还没落库（后端返回的是骨架策略）。 */
  id: number
  project_id?: number
  project_name?: string
  enabled: boolean
  /** 组合算法，恒为 "or"（命中任一规则即保留）。 */
  algorithm: string
  /** 策略级默认 tag 选择器。 */
  tag_selector: string
  rules: RetentionRule[]
  created_at: number
  updated_at: number
}

/** 规则模板的参数定义。 */
interface RetentionTemplateParam {
  /** 参数名，如 k / days。 */
  name: string
  /** 参数类型，目前只有 int。 */
  type: string
  /** 单位：count（个）/ days（天）。 */
  unit?: string
  required: boolean
}

/** 一条可用的规则模板。 */
export interface RetentionTemplate {
  template: string
  label: string
  params: RetentionTemplateParam[]
  descr?: string
}

/** GET /api/v1/retention/templates 的响应。 */
export interface RetentionTemplates {
  templates: RetentionTemplate[]
  /** tag 选择器语法说明（多行文本）。 */
  selector_help: string
}

/** 清理的触发方式。 */
type CleanupTrigger = 'schedule' | 'manual' | 'push'

/** 清理执行的状态——只有三态，没有"部分成功"。 */
type CleanupRunStatus = 'running' | 'success' | 'failed'

/** 一次清理执行记录。 */
export interface CleanupRun {
  id: number
  trigger: CleanupTrigger
  /** "all" = 全部项目；"project:<name>" = 指定项目。 */
  scope: string
  project_name?: string
  /** true = 试运行（只评估不删除）。 */
  dry_run: boolean
  status: CleanupRunStatus
  /** 删掉的 tag 数（一个 digest 可能对应多个 tag）。 */
  tags_deleted: number
  /** 删掉的 manifest 数（按 digest 计）。 */
  manifests_deleted: number
  /** 保留的 tag 数。 */
  tags_kept: number
  /** 推送时间未知而保留的 tag 数。 */
  skipped_unknown_time: number
  /** 同 digest 下有别的 tag 要保留、整组跳过的 tag 数。 */
  skipped_shared_digest: number
  /** 删除失败的条目数。 */
  failed_count: number
  /** 整体错误原文；单条失败在 detail 里。 */
  error?: string
  /** 明细的 JSON 字符串（可选；解析失败会显式报错，不当成"没有明细"）。 */
  detail?: string
  /** 起止时间（unix 秒）。 */
  started_at: number
  finished_at?: number
}

/** 跳过原因。 */
type CleanupSkipReason =
  | 'unknown_time'
  | 'shared_digest'
  | 'protected_latest'
  | 'no_digest'

/** 一条删除失败记录。 */
export interface CleanupFailure {
  repo: string
  tag: string
  digest: string
  error: string
}

/** 一条删除记录（按 digest 聚合，可能对应多个 tag）。 */
export interface CleanupDeleted {
  repo: string
  tags: string[]
  digest: string
  /** 字节。 */
  size: number
}

/** 一条跳过记录。 */
export interface CleanupSkipped {
  repo: string
  tag: string
  digest?: string
  reason: CleanupSkipReason
}

/** 执行明细（run.detail 解析后的结构）。 */
export interface CleanupDetail {
  failures?: CleanupFailure[]
  /**
   * 审计写失败的条目：tag 已经删掉了，但审计链上没留下痕迹。
   *
   * 有这一栏说明这次执行**不是**干净的——页面要显式提示，
   * 不能让它跟"删干净了"长得一样。
   */
  audit_failures?: CleanupFailure[]
  deleted?: CleanupDeleted[]
  skipped?: CleanupSkipped[]
  /** 附带的 blob 回收结果（没跑回收时没有这个字段）。 */
  gc?: CleanupGC
}

/** 清理执行里附带的 blob 回收结果。 */
interface CleanupGC {
  run_id: number
  status: GCRunStatus
  dry_run: boolean
  /** 回收掉的 blob 数。 */
  blobs_deleted: number
  /** 回收的字节数。 */
  bytes_reclaimed: number
  error?: string
}

/** blob 回收的触发方式。 */
type GCTrigger = 'manual' | 'after_cleanup' | 'schedule'

/** blob 回收的执行状态——与清理执行一样只有三态。 */
type GCRunStatus = 'running' | 'success' | 'failed'

/** 一次 blob 回收的执行记录。 */
export interface GCRun {
  id: number
  trigger: GCTrigger
  /** true = 试运行（只评估不删除）。 */
  dry_run: boolean
  status: GCRunStatus
  /** 扫到的仓库数。 */
  repositories: number
  /** 扫到的 manifest 修订数。 */
  manifests_found: number
  /** 扫到的 blob 总数。 */
  blobs_scanned: number
  /** 删掉的 blob 数。 */
  blobs_deleted: number
  /** 回收的字节数。 */
  bytes_reclaimed: number
  /** 整体错误原文。 */
  error?: string
  /** 起止时间（unix 秒）。 */
  started_at: number
  finished_at?: number
}

/** blob 回收的当前状态与配置。 */
export interface GCStatus {
  /** 总开关。 */
  enabled: boolean
  /** 是否删除无 tag 的 manifest。 */
  delete_untagged: boolean
  /** 默认试运行。 */
  dry_run: boolean
  /** 保留策略执行后是否自动回收。 */
  trigger_after_cleanup: boolean
  /** 是否启用每日定时回收。 */
  schedule_enabled: boolean
  /** 每日定时回收的执行时间（HH:MM，24 小时制）。 */
  schedule_at: string
}

/**
 * 令牌列表的公共列。
 *
 * 个人令牌与项目令牌的表格长得一样——项目令牌只多一列"角色"。
 * 公共列抽成这个接口，具体类型在满足它的同时各自多带字段。
 */
export interface TokenRow {
  id: Id
  name: string
  token_prefix: string
  created_at: number
  /** 失效时间（unix 秒）；null = 永久有效。 */
  expires_at: number | null
  /** 最近一次使用时间；没用过为 null。 */
  last_used_at: number | null
}

/**
 * 个人访问令牌（PAT）：用户自己生成的长期凭据。
 *
 * 用来在 CI / 脚本里推拉需要权限的镜像，不必把账户口令交出去。
 *
 * ⚠️ 令牌本体**不在这里**——后端只返回展示前缀。
 *    原始令牌只在创建时返回一次（见 PersonalTokenCreated）。
 */
export interface PersonalToken {
  id: Id
  /** 用户给令牌起的名字（便于区分用途）。 */
  name: string
  /** 展示用前缀，如 lau6_u_a1b2c3。 */
  token_prefix: string
  created_at: number
  /**
   * 失效时间（unix 秒）；**null = 永久有效**。
   *
   * 永久是用户显式选的，不是默认值——默认永远是"系统默认 TTL"。
   */
  expires_at: number | null
  /** 最近一次使用时间；没用过为 null。 */
  last_used_at: number | null
}

/** 创建令牌时的响应：**令牌本体只在这里出现一次**。 */
export interface PersonalTokenCreated {
  /** 原始令牌。离开这个响应就再也拿不到了。 */
  token: string
  id: Id
  name: string
  token_prefix: string
  created_at: number
  /** 失效时间（unix 秒）；null = 永久有效。 */
  expires_at: number | null
}

/**
 * 个人令牌的失效策略。
 *
 * 三种合法组合（后端会拒绝矛盾组合）：
 * - `{ never_expires: true }` → 永久有效
 * - `{ never_expires: false, expires_at: 1799000000 }` → 该时刻失效
 * - `{ never_expires: false }` → 走系统默认（core.pat_ttl_seconds）
 */
export interface PersonalTokenExpiry {
  never_expires: boolean
  expires_at?: number | null
}

/**
 * 项目令牌的角色。
 *
 * 取值与 ProjectRole 相同，但**含义不同**：
 * - ProjectRole 是用户在项目里的成员角色，会随成员关系变化；
 * - ProjectTokenRole 是令牌创建时**固化**的角色，此后不可修改。
 */
export type ProjectTokenRole = 'admin' | 'developer' | 'reader'

/**
 * 项目访问令牌：代表**项目**身份的长期凭据。
 *
 * 项目管理员生成后交给 CI / 流水线，让自动化以项目身份推拉镜像，
 * 不必占用某个人的个人账号。
 *
 * ⚠️ 令牌本体**不在这里**——后端只返回展示前缀。
 *    原始令牌只在创建时返回一次（见 ProjectTokenCreated）。
 */
export interface ProjectToken {
  id: Id
  /** 给令牌起的名字（便于在列表里区分用途）。 */
  name: string
  /** 创建时选定的项目内角色，此后不变。 */
  role: ProjectTokenRole
  /** 展示用前缀，如 lau6_p_a1b2c3。 */
  token_prefix: string
  created_at: number
  /** 失效时间（unix 秒）；**null = 永久有效**。 */
  expires_at: number | null
  /** 最近一次使用时间；没用过为 null。 */
  last_used_at: number | null
}

/** 创建项目令牌时的响应：**令牌本体只在这里出现一次**。 */
export interface ProjectTokenCreated {
  /** 原始令牌。离开这个响应就再也拿不到了。 */
  token: string
  id: Id
  name: string
  role: ProjectTokenRole
  token_prefix: string
  created_at: number
  /** 失效时间（unix 秒）；null = 永久有效。 */
  expires_at: number | null
}

/**
 * 项目令牌的失效策略。
 *
 * 三种合法组合（后端会拒绝矛盾组合）：
 * - `{ never_expires: true }` → 永久有效
 * - `{ never_expires: false, expires_at: 1799000000 }` → 该时刻失效
 * - `{ never_expires: false }` → 走系统默认（core.project_token_ttl_seconds）
 */
export interface ProjectTokenExpiry {
  never_expires: boolean
  expires_at?: number | null
}
