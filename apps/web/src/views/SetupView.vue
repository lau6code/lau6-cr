<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { useI18n } from 'vue-i18n'

import { errorMessage } from '@/api/client'
import { takePendingNotice } from '@/api/passwordTransport'
import {
  fetchSetupDatabase,
  fetchSetupStatus,
  fetchSetupStorage,
  submitSetup,
  testSetupCache,
  testSetupDatabase,
  testSetupLDAP,
  testSetupStorage,
} from '@/api/setup'
import LocaleSwitcher from '@/components/LocaleSwitcher.vue'
import ThemeSwitcher from '@/components/ThemeSwitcher.vue'
import { useSetupPinned } from '@/composables/useSetupPinned'
import { useSetupProbes } from '@/composables/useSetupProbes'
import { useSetupWizard } from '@/composables/useSetupWizard'
import { useAuthStore } from '@/stores/auth'
import type { CacheTestResult, SetupStatus } from '@/types'
import { formLabelWidth } from '@/i18n'

const router = useRouter()
const auth = useAuthStore()
const { t } = useI18n()

const checking = ref(true)
const submitting = ref(false)
const status = ref<SetupStatus | null>(null)

/**
 * distribution 是否与本进程同容器。
 *
 * ⚠️ external 形态下**不显示存储步骤**：blob 存不存对象存储是上游
 * distribution 自己的事，lau6-cr 这边没有 blob 可放。
 * 显示一个"选了也没人读"的开关就是假开关。
 */
const distributionEmbedded = ref(true)

/** 是否需要引导令牌；需要时先停在令牌步骤 */
const tokenRequired = ref(false)
const tokenPhase = ref(false)

/** 连通性测试的进行中状态与结果。 */
const dbTesting = ref(false)
const ldapTesting = ref(false)
const cacheTesting = ref(false)
const storageTesting = ref(false)
const dbProbeResult = ref<{ ok: boolean; message: string } | null>(null)
const ldapProbeResult = ref<{ ok: boolean; message: string } | null>(null)
const cacheProbeResult = ref<{ ok: boolean; message: string } | null>(null)
const storageProbeResult = ref<{ ok: boolean; message: string } | null>(null)
/** 缓存配置的风险提示（淘汰策略、持久化、requirepass）。 */
const cacheProbeWarnings = ref<string[]>([])

const { isPinned, pinnedNotice } = useSetupPinned(status)

/**
 * 数据库连接被环境变量钉死？
 *
 * env 的优先级最高：被钉死时向导里改了**重启后就没了**——
 * 所以这一段要显示成只读，不能让人以为改了会生效。
 */
const dbEnvLocked = ref(false)
const dbEnvVar = ref('')

/**
 * LDAP 连通性测试用的账号（可选）。
 *
 * 单独放，**不混进 form.ldap** —— form.ldap 会被原样提交给后端，
 * 多带字段会被 DisallowUnknownFields 拒掉。
 */
const ldapProbeAccount = reactive({ username: '', password: '' })

const form = reactive({
  bootstrap_token: '',
  public_url: '',
  admin_username: 'admin',
  admin_password: '',
  admin_password_confirm: '',
  anonymous_read: true,
  database: {
    driver: 'sqlite',
    sqlite_path: '/data/db/lau6-cr.db',
    host: '',
    port: '',
    username: '',
    password: '',
    name: '',
    schema: '',
    ssl_mode: 'disable',
  },
  cache: {
    driver: 'local',
    local: { max_entries: 10000 },
    // ⚠️ db 起步是 **null**（空），不是 0：
    //    db 0 在共享 Redis 上常常是别人的应用在用，
    //    预填 0 等于替用户做了这个决定（12-cache.md §2）。
    redis: { addr: '', password: '', prefix: 'lau6-cr:', db: null as number | null },
  },
  ldap: {
    enabled: false,
    url: '',
    dc: '',
    user_segment: '',
    start_tls: false,
    allow_create: true,
    timeout_sec: 5,
  },
  storage: {
    driver: 'local',
    s3: {
      endpoint: '',
      region: 'us-east-1',
      bucket: '',
      path_style: true,
      access_key: '',
      secret_key: '',
    },
  },
})

const { cachePayload, storagePayload } = useSetupProbes(form)

const { STEPS, activeStep, activeKey, isLastStep, next, prev } = useSetupWizard({
  form,
  distributionEmbedded,
  isPinned,
})

const databaseSummary = computed(() => {
  const db = form.database
  if (db.driver === 'sqlite') return db.sqlite_path
  const base = `${db.username}@${db.host}:${db.port}/${db.name}`
  return db.driver === 'postgres' && db.schema
    ? t('setup.dbSummarySchema', { base, schema: db.schema })
    : base
})

/**
 * 切换驱动时跟着调整端口与 schema。
 *
 * 端口默认：pg 5432 / mysql 3306。
 * mysql 没有独立 schema（库名即 schema）——切过去时清掉，免得留下个
 * mysql 不认的字段。
 */
function onDriverChange(driver: string): void {
  form.database.port = driver === 'mysql' ? '3306' : driver === 'postgres' ? '5432' : ''
  if (driver !== 'postgres') {
    form.database.schema = ''
  }
  form.database.ssl_mode = driver === 'postgres' ? 'disable' : ''
  dbProbeResult.value = null
}

/**
 * 预填当前生效的数据库连接信息。
 *
 * 后端**不回显口令**——口令要在表单里自己填（留空表示沿用当前口令）。
 */
async function loadDatabaseInfo(): Promise<void> {
  try {
    const info = await fetchSetupDatabase()
    dbEnvLocked.value = info.env_var !== ''
    dbEnvVar.value = info.env_var
    if (!info.driver) return

    form.database.driver = info.driver
    if (info.driver === 'sqlite') {
      form.database.sqlite_path = info.sqlite_path || form.database.sqlite_path
      return
    }
    // 只有驱动对得上才填字段：驱动都换了，旧的 host / 库名没有意义。
    form.database.host = info.host
    form.database.port = info.port
    form.database.username = info.username
    form.database.name = info.name
    form.database.schema = info.schema
    form.database.ssl_mode = info.ssl_mode || 'disable'
  } catch (err) {
    // 读不到就让人手填——不能因此不让初始化继续。
    ElMessage.warning(errorMessage(err, t('setup.dbInfoFailed')))
  }
}

/**
 * 预填当前生效的存储配置。
 *
 * 后端**不回显凭据**——只给 access_key_set / secret_key_set，
 * 凭据要自己在表单里填（留空表示沿用环境变量里的那份）。
 */
async function loadStorageInfo(): Promise<void> {
  if (!distributionEmbedded.value) return
  try {
    const info = await fetchSetupStorage()
    if (info.driver) {
      form.storage.driver = info.driver
    }
    form.storage.s3.endpoint = info.s3.endpoint
    form.storage.s3.region = info.s3.region || 'us-east-1'
    form.storage.s3.bucket = info.s3.bucket
    form.storage.s3.path_style = info.s3.path_style
  } catch (err) {
    // 读不到就让人手填——不能因此不让初始化继续。
    ElMessage.warning(errorMessage(err, t('setup.storageInfoFailed')))
  }
}

/**
 * 令牌提示语。
 *
 * 令牌由**前端生成**、后端收下并落库——setup 阶段还没有任何配置文件，
 * 服务端没有可预发的令牌，所以不需要去服务器上取。
 */
const tokenHint = computed(() => t('setup.tokenHint'))

async function checkStatus(): Promise<void> {
  checking.value = true
  try {
    const data = await fetchSetupStatus()
    status.value = data
    if (data.initialized) {
      // 实例已完成初始化 → 去登录页。
      //
      // ⚠️ 必须先 markInitialized()：进引导页时 /auth/me 返回的是
      //    503 setup_required，auth.setupRequired 被置成 true，
      //    路由守卫会把 /login 弹回 /setup（见 stores/auth.ts）。
      auth.markInitialized()
      ElMessage.info(t('setup.alreadyInitialized'))
      await router.replace('/login')
      return
    }
    tokenRequired.value = data.require_bootstrap_token === true
    tokenPhase.value = tokenRequired.value
    if (data.database_driver) {
      form.database.driver = data.database_driver
    }
    // external 形态下不显示存储步骤（blob 是上游 distribution 的事）。
    distributionEmbedded.value = data.distribution_embedded !== false
    // 存储后端被 env 钉死时，这一栏是只读的：
    // 预填成生效值，用户能看到它是什么。
    if (data.storage_driver && isPinned('storage.driver')) {
      form.storage.driver = data.storage_driver
    }
    // 缓存后端按**生效值**显示。
    //
    // ⚠️ 不预填的后果：生效的是 redis（CR_CACHE_DRIVER 或配置文件）、
    //    向导却显示 local ——「选 redis 要填数据库编号」这条要求
    //    会被整段跳过，实例默默用上 db 0。
    if (data.cache_driver) {
      form.cache.driver = data.cache_driver
    }
    // Redis 数据库编号被 env 钉死时，这一栏是**只读**的：
    // 预填成生效值，用户能看到它是什么（空着的只读框会被当成"没填"）。
    if (isPinned('cache.redis.db')) {
      form.cache.redis.db = data.cache_redis_db
    }
    // 公共访问 URL 被环境变量钉死时，这一栏是**只读**的：
    // 预填成生效值，用户能看到它是什么（也才走得完这一步）。
    //
    // ⚠️ 只在被钉死时预填：没被钉死时这一栏留空，
    //    让用户填自己的对外地址——默认的 localhost:5001 不是他要的东西。
    if (data.public_url && isPinned('core.public_url')) {
      form.public_url = data.public_url
    }
    // 存储配置要在知道形态之后才能预填（external 形态没有这一步）。
    await loadStorageInfo()
  } catch (err) {
    ElMessage.error(errorMessage(err, t('setup.statusFailed')))
  } finally {
    checking.value = false
  }
}

function confirmToken(): void {
  const tok = form.bootstrap_token.trim()
  if (!tok) {
    ElMessage.warning(t('setup.tokenRequired'))
    return
  }
  // 与后端一致：至少 16 位（后端会再校验一次）。
  if (tok.length < 16) {
    ElMessage.warning(t('setup.tokenTooShort'))
    return
  }
  form.bootstrap_token = tok
  tokenPhase.value = false
  activeStep.value = 0
}

/**
 * 生成随机引导令牌。
 *
 * 令牌由**前端**生成、后端收下并落库——setup 阶段还没有任何配置文件，
 * 服务端没有可预发的令牌可比对。
 *
 * 用 crypto.getRandomValues 而不是 Math.random：这是能当凭证用的随机数。
 */
function generateToken(): void {
  const bytes = new Uint8Array(32)
  crypto.getRandomValues(bytes)
  form.bootstrap_token = Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('')
  ElMessage.success(t('setup.tokenGenerated'))
}

/**
 * 切换缓存后端时清掉上一次的测试结果。
 *
 * 结果是**针对某一组参数**的——参数变了还留着旧结果，
 * 用户会以为新参数也测过了。
 */
function onCacheDriverChange(): void {
  cacheProbeResult.value = null
  cacheProbeWarnings.value = []
}

/**
 * Redis 数据库编号是不是"已经有了"。
 *
 * ⚠️ 被环境变量钉死时（CR_REDIS_DB）算有——值由环境变量说了算，
 *    向导填什么都不作数，后端也不校验（08-setup.md §6.3）。
 *
 * ⚠️ 用 typeof 判而不是 `=== null`：输入框被清空时组件可能给出
 *    undefined，两种都得当成"没填"。
 */
function redisDBFilled(): boolean {
  return isPinned('cache.redis.db') || typeof form.cache.redis.db === 'number'
}

/**
 * 测试缓存后端连通性。
 *
 * 不只测"连不连得上"，还读回淘汰策略 / 持久化 / requirepass——
 * 地址密码填对了不代表 Redis 配对了
 * （见 docs/design/12-cache.md §5）。
 */
async function testCache(): Promise<void> {
  if (!form.cache.redis.addr.trim()) {
    ElMessage.warning(t('setup.cacheRequired'))
    return
  }
  if (!redisDBFilled()) {
    ElMessage.warning(t('setup.cacheDbRequired'))
    return
  }
  cacheTesting.value = true
  cacheProbeResult.value = null
  cacheProbeWarnings.value = []
  try {
    const res = await testSetupCache(cachePayload())
    cacheProbeResult.value = {
      ok: true,
      message: `${res.detail}${t('setup.latencySuffix', { ms: res.latency_ms })}`,
    }
    cacheProbeWarnings.value = res.warnings ?? []
    ElMessage.success(t('setup.cacheTestOk', { detail: res.detail }))
  } catch (err) {
    const message = errorMessage(err, t('setup.cacheTestFailed'))
    cacheProbeResult.value = { ok: false, message }
    ElMessage.error(message)
  } finally {
    cacheTesting.value = false
  }
}

/**
 * 提交前先探一次缓存。
 *
 * 连不上就别提交——后端也会再验一次，但这里先探能给出**更准**的错误，
 * 也避免把实例配成一个连不上的缓存。
 *
 * 有风险提示时问一句再继续（对应 TUI 的"仍要继续？ [y/N]"）。
 */
async function confirmCacheBeforeSubmit(): Promise<boolean> {
  if (form.cache.driver !== 'redis') {
    return true
  }
  if (!form.cache.redis.addr.trim()) {
    ElMessage.warning(t('setup.cacheRequired'))
    return false
  }
  if (!redisDBFilled()) {
    ElMessage.warning(t('setup.cacheDbRequired'))
    return false
  }
  let res: CacheTestResult
  try {
    res = await testSetupCache(cachePayload())
  } catch (err) {
    ElMessage.error(errorMessage(err, t('setup.cacheTestFailed')))
    return false
  }
  cacheProbeWarnings.value = res.warnings ?? []
  if (!res.warnings || res.warnings.length === 0) {
    return true
  }
  try {
    await ElMessageBox.confirm(
      t(
        'setup.cacheRiskyMessage',
        { warnings: res.warnings.map((w) => `· ${w}`).join('\n') },
      ),
      t('setup.cacheRiskyTitle'),
      {
        type: 'warning',
        confirmButtonText: t('setup.cacheContinue'),
        cancelButtonText: t('setup.cacheBackToEdit'),
      },
    )
    return true
  } catch {
    // 用户点了"返回修改"——不是错误，是选择。
    return false
  }
}

/**
 * 测试存储后端能不能真的用。
 *
 * s3 会真写一个探针对象 → 读回 → 删掉（不只是格式校验）：
 * 只读探针证明不了写权限，而"只给了读权限"是最常见的坑。
 */
async function testStorage(): Promise<void> {
  if (form.storage.driver === 's3') {
    if (!form.storage.s3.endpoint.trim()) {
      ElMessage.warning(t('setup.storageRequired'))
      return
    }
    if (!form.storage.s3.bucket.trim()) {
      ElMessage.warning(t('setup.storageBucketRequired'))
      return
    }
  }
  storageTesting.value = true
  storageProbeResult.value = null
  try {
    const res = await testSetupStorage(storagePayload())
    storageProbeResult.value = {
      ok: true,
      message: `${res.detail}${t('setup.latencySuffix', { ms: res.latency_ms })}`,
    }
    ElMessage.success(t('setup.storageTestOk', { detail: res.detail }))
  } catch (err) {
    const message = errorMessage(err, t('setup.storageTestFailed'))
    storageProbeResult.value = { ok: false, message }
    ElMessage.error(message)
  } finally {
    storageTesting.value = false
  }
}

/**
 * 提交前先探一次存储。
 *
 * ⚠️ 探测失败**不直接拦下**：setup 所在的管理网可能访问不到对象存储，
 *    而运行期网络是通的（docs/design/11-storage.md §4.4）。
 *    所以问一句"仍要继续吗"，默认选项是"返回修改"。
 */
async function confirmStorageBeforeSubmit(): Promise<boolean> {
  if (!distributionEmbedded.value) return true
  if (form.storage.driver === 's3') {
    if (!form.storage.s3.endpoint.trim() || !form.storage.s3.bucket.trim()) {
      ElMessage.warning(t('setup.storageRequiredBoth'))
      return false
    }
  }
  try {
    await testSetupStorage(storagePayload())
    return true
  } catch (err) {
    const message = errorMessage(err, t('setup.storageTestFailed'))
    storageProbeResult.value = { ok: false, message }
    try {
      await ElMessageBox.confirm(
        t('setup.storageTestFailed') +
          t('common.colon') +
          `\n\n${message}\n\n` +
          t('setup.storageContinueMessage') +
          '\n\n' +
          '⚠️ ' +
          t('setup.storageImmutableDescription'),
        t('setup.storageTestFailed'),
        {
          type: 'warning',
          confirmButtonText: t('setup.storageContinueAnyway'),
          cancelButtonText: t('setup.cacheBackToEdit'),
        },
      )
      return true
    } catch {
      // 用户点了"返回修改"——不是错误，是选择。
      return false
    }
  }
}

/**
 * 切换存储后端时清掉上一次的测试结果。
 *
 * 结果是**针对某一组参数**的——参数变了还留着旧结果，
 * 用户会以为新参数也测过了。
 */
function onStorageDriverChange(): void {
  storageProbeResult.value = null
}

/**
 * 测试数据库连通性。
 *
 * 只测"填的东西连不连得上"，**不改任何配置**。
 * 失败必须让用户看见——不吞异常、不静默成"成功"。
 */
async function testDatabase(): Promise<void> {
  const db = form.database
  if (db.driver === 'sqlite') {
    if (!db.sqlite_path.trim()) {
      ElMessage.warning(t('setup.sqlitePathRequired'))
      return
    }
  } else {
    if (!db.host.trim()) {
      ElMessage.warning(t('setup.dbHostRequired'))
      return
    }
    if (!db.username.trim()) {
      ElMessage.warning(t('setup.dbUsernameRequired'))
      return
    }
    if (!db.name.trim()) {
      ElMessage.warning(t('setup.dbNameRequired'))
      return
    }
  }
  dbTesting.value = true
  dbProbeResult.value = null
  try {
    const res = await testSetupDatabase({
      bootstrap_token: form.bootstrap_token.trim(),
      database: { ...db },
    })
    const schema = res.schema ? t('setup.dbTestSchema', { schema: res.schema }) : ''
    dbProbeResult.value = {
      ok: true,
      message: `${res.detail}${t('setup.latencySuffix', { ms: res.latency_ms })}${schema}`,
    }
    ElMessage.success(t('setup.dbTestOk', { detail: res.detail }))
  } catch (err) {
    const message = errorMessage(err, t('setup.dbTestFailed'))
    dbProbeResult.value = { ok: false, message }
    ElMessage.error(message)
  } finally {
    dbTesting.value = false
  }
}

/**
 * 测试 LDAP 连通性。
 *
 * 填了测试账号 → 按上面的用户过滤器拼出 DN 做一次真实绑定；
 * 不填 → 只测连接与 TLS 握手。
 */
async function testLDAP(): Promise<void> {
  if (!form.ldap.url.trim()) {
    ElMessage.warning(t('setup.ldapUrlRequired'))
    return
  }
  ldapTesting.value = true
  ldapProbeResult.value = null
  try {
    const res = await testSetupLDAP({
      bootstrap_token: form.bootstrap_token.trim(),
      url: form.ldap.url.trim(),
      dc: form.ldap.dc.trim(),
      user_segment: form.ldap.user_segment.trim(),
      start_tls: form.ldap.start_tls,
      timeout_sec: form.ldap.timeout_sec,
      username: ldapProbeAccount.username.trim(),
      password: ldapProbeAccount.password,
    })
    ldapProbeResult.value = {
      ok: true,
      message: `${res.detail}${t('setup.latencySuffix', { ms: res.latency_ms })}`,
    }
    ElMessage.success(t('setup.ldapTestOk', { detail: res.detail }))
  } catch (err) {
    const message = errorMessage(err, t('setup.ldapTestFailed'))
    ldapProbeResult.value = { ok: false, message }
    ElMessage.error(message)
  } finally {
    ldapTesting.value = false
  }
}

async function submit(): Promise<void> {
  if (!(await confirmCacheBeforeSubmit())) {
    return
  }
  if (!(await confirmStorageBeforeSubmit())) {
    return
  }
  submitting.value = true
  try {
    const outcome = await submitSetup({
      bootstrap_token: form.bootstrap_token,
      public_url: form.public_url.trim(),
      admin_username: form.admin_username.trim(),
      admin_password: form.admin_password,
      anonymous_read: form.anonymous_read,
      database: { ...form.database },
      cache: {
        driver: form.cache.driver,
        local: { ...form.cache.local },
        redis: { ...form.cache.redis },
      },
      ldap: { ...form.ldap },
      // ⚠️ external 形态**不传** storage：那时 blob 是上游 distribution 的事，
      //    lau6-cr 这边没有 blob 可放（传了就是"改了没人读"）。
      ...(distributionEmbedded.value ? { storage: { ...form.storage } } : {}),
    })
    if (outcome.notice) ElMessage.warning(outcome.notice)
    // ⚠️ 被 env 钉死而忽略的项要说出来：
    //    用户填了值却没生效，不说的话他会以为"填了没用"。
    const ignored = outcome.data.ignored_env_pinned ?? []
    if (ignored.length > 0) {
      ElMessage.warning(
        t('setup.ignoredPinned', {
          items: ignored
            .map((p) => t('common.parenthesized', { label: p.key, value: p.env_var }))
            .join(t('common.listSeparator')),
        }),
      )
    }
    // 实例已初始化 → 登录页。
    //
    // ⚠️ 必须先 markInitialized()：否则 auth.setupRequired 仍是 true，
    //    路由守卫会把 /login 弹回 /setup，页面卡在引导页上。
    auth.markInitialized()
    ElMessage.success(t('setup.submitted'))
    await router.replace('/login')
  } catch (err) {
    const notice = takePendingNotice()
    if (notice) ElMessage.warning(notice)
    // ⚠️ 后端有两种失败，必须分开处理：
    //   ① 初始化**没成功**（数据库里没写标记）→ 留在本页继续填；
    //   ② 初始化**已经提交**、但运行期配置重载失败 → 后端回 500，
    //      这时实例其实已经初始化完成。
    //
    // 不区分的话，② 里 auth.setupRequired 还是 true，
    // 路由守卫会把用户弹回 /setup（checkStatus 只在 onMounted 跑一次，
    // ensureLoaded 因为 loaded=true 也不会重新拉）——
    // 页面卡在引导页上，而后端明明已经初始化好了。
    //
    // 所以失败后**回查一次状态**：initialized 为真 = 初始化其实成功了。
    const status = await fetchSetupStatus().catch(() => null)
    if (status?.initialized) {
      auth.markInitialized()
      ElMessage.warning(errorMessage(err, t('setup.submittedButReloadFailed')))
      await router.replace('/login')
      return
    }
    ElMessage.error(errorMessage(err, t('setup.submitFailed')))
  } finally {
    submitting.value = false
  }
}

onMounted(() => {
  void checkStatus()
  void loadDatabaseInfo()
})
</script>

<template>
  <div class="blank-layout">
    <div class="blank-layout__card blank-layout__card--wide">
      <div class="blank-layout__head">
        <span class="blank-layout__title">{{ t('setup.title') }}</span>
        <div class="blank-layout__head-right">
          <span v-if="status" class="status-pill status-muted">
            {{ t('setup.summaryDatabase') }}{{ t('common.colon') }}{{ status.database_driver }}
          </span>
          <!-- 引导页没有顶栏，语言与主题的切换器得放在这里。 -->
          <LocaleSwitcher />
          <ThemeSwitcher />
        </div>
      </div>

      <el-skeleton v-if="checking" :rows="6" animated />

      <!-- 令牌步骤：先填令牌，再显示后续步骤 -->
      <template v-else-if="tokenPhase">
        <el-alert
          type="info"
          show-icon
          :closable="false"
          :title="t('setup.tokenTitle')"
          :description="tokenHint"
          style="margin-bottom: var(--space-4)"
        />
        <el-form :label-width="formLabelWidth(90)" @submit.prevent>
          <el-form-item :label="t('setup.tokenLabel')" required>
            <el-input
              v-model="form.bootstrap_token"
              type="password"
              show-password
              :placeholder="t('setup.tokenPlaceholder')"
            >
              <template #append>
                <el-button @click="generateToken">{{ t('setup.tokenGenerate') }}</el-button>
              </template>
            </el-input>
          </el-form-item>
          <el-button type="primary" @click="confirmToken">{{ t('setup.next') }}</el-button>
        </el-form>
      </template>

      <template v-else>
        <el-steps :active="activeStep" align-center finish-status="success" style="margin-bottom: var(--space-6)">
          <el-step v-for="title in STEPS" :key="title" :title="title" />
        </el-steps>

        <!--
          env 钉死的项：这里显示成只读。

          ⚠️ 不能只靠后端拦（后端会拒绝覆盖并回报）——
             前端也必须让人**事先知道**这一项改了不生效，
             否则用户会在只读框里填半天、提交后发现没变化。
        -->
        <el-alert
          v-if="pinnedNotice.length > 0"
          type="warning"
          show-icon
          :closable="false"
          :title="t('setup.pinnedTitle')"
          :description="
            t('setup.pinnedDescription', { items: pinnedNotice.join(t('common.listSeparator')) })
          "
          style="margin-bottom: var(--space-4)"
        />

        <div v-show="activeKey === 'database'">
          <el-alert
            v-if="dbEnvLocked"
            type="warning"
            show-icon
            :closable="false"
            :title="t('setup.dbEnvLockedTitle', { envVar: dbEnvVar })"
            :description="t('setup.dbEnvLockedDescription', { envVar: dbEnvVar })"
            style="margin-bottom: var(--space-4)"
          />
          <el-alert
            v-else
            type="info"
            show-icon
            :closable="false"
            :title="t('setup.dbSwitchTitle')"
            :description="t('setup.dbSwitchDescription')"
            style="margin-bottom: var(--space-4)"
          />
          <el-form :label-width="formLabelWidth(112)" :disabled="dbEnvLocked">
            <el-form-item :label="t('setup.dbDriver')">
              <el-select
                v-model="form.database.driver"
                class="form-control"
                @change="onDriverChange"
              >
                <el-option label="sqlite" value="sqlite" />
                <el-option label="postgres" value="postgres" />
                <el-option label="mysql" value="mysql" />
              </el-select>
            </el-form-item>

            <el-form-item v-if="form.database.driver === 'sqlite'" :label="t('setup.dbSqlitePath')">
              <el-input v-model="form.database.sqlite_path" class="form-control" />
            </el-form-item>

            <template v-else>
              <el-form-item :label="t('setup.dbHost')">
                <el-input
                  v-model="form.database.host"
                  class="form-control"
                  :placeholder="t('setup.dbHostPlaceholder')"
                />
              </el-form-item>
              <el-form-item :label="t('setup.dbPort')">
                <el-input
                  v-model="form.database.port"
                  class="form-control form-control--xs"
                  :placeholder="form.database.driver === 'mysql' ? '3306' : '5432'"
                />
              </el-form-item>
              <el-form-item :label="t('setup.dbUsername')">
                <el-input v-model="form.database.username" class="form-control" />
              </el-form-item>
              <el-form-item :label="t('setup.dbPassword')">
                <el-input
                  v-model="form.database.password"
                  type="password"
                  show-password
                  class="form-control"
                  :placeholder="t('setup.dbPasswordPlaceholder')"
                />
              </el-form-item>
              <el-form-item :label="t('setup.dbName')">
                <el-input v-model="form.database.name" class="form-control" />
              </el-form-item>
              <el-form-item v-if="form.database.driver === 'postgres'" :label="t('setup.dbSchema')">
                <el-input
                  v-model="form.database.schema"
                  class="form-control"
                  :placeholder="t('setup.dbSchemaPlaceholder')"
                />
              </el-form-item>
              <el-form-item v-else :label="t('setup.dbSchema')">
                <span class="text-muted">{{ t('setup.dbSchemaMysqlHint') }}</span>
              </el-form-item>
            </template>

            <el-form-item :label="t('setup.connectivityTest')">
              <el-button :loading="dbTesting" @click="testDatabase">
                {{ t('setup.testConnection') }}
              </el-button>
              <span class="form-hint" style="margin-left: var(--space-3)">
                {{ t('setup.connectivityHint') }}
              </span>
            </el-form-item>
          </el-form>
          <el-alert
            v-if="dbProbeResult"
            :type="dbProbeResult.ok ? 'success' : 'error'"
            show-icon
            :closable="false"
            :title="dbProbeResult.message"
            style="margin-top: var(--space-2)"
          />
        </div>

        <div v-show="activeKey === 'cache'">
          <el-alert
            type="info"
            show-icon
            :closable="false"
            :title="t('setup.cacheSwitchTitle')"
            style="margin-bottom: var(--space-4)"
          />
          <el-form :label-width="formLabelWidth(112)">
            <el-form-item :label="t('setup.cacheBackend')">
              <el-radio-group
                v-model="form.cache.driver"
                :disabled="isPinned('cache.driver')"
                @change="onCacheDriverChange"
              >
                <el-radio value="local">{{ t('setup.cacheLocal') }}</el-radio>
                <el-radio value="redis">{{ t('setup.cacheRedis') }}</el-radio>
              </el-radio-group>
            </el-form-item>
            <el-form-item v-if="form.cache.driver === 'local'" :label="t('setup.cacheMaxEntries')">
              <el-input-number v-model="form.cache.local.max_entries" :min="1" :step="100" />
            </el-form-item>
            <template v-else>
              <el-form-item :label="t('setup.cacheRedisAddr')">
                <el-input
                  v-model="form.cache.redis.addr"
                  :disabled="isPinned('cache.redis.addr')"
                  class="form-control"
                  placeholder="redis:6379"
                />
              </el-form-item>
              <el-form-item :label="t('setup.cacheRedisPassword')">
                <el-input
                  v-model="form.cache.redis.password"
                  :disabled="isPinned('cache.redis.password')"
                  type="password"
                  show-password
                  class="form-control"
                />
              </el-form-item>
              <!--
                ⚠️ 上限 1023 与后端 redisDBMax 同一口径
                （apps/api/internal/service/settings_defs.go）——
                这里只是输入框的闸门，真正的判定在后端。
              -->
              <el-form-item :label="t('setup.cacheRedisDb')">
                <el-input-number
                  v-model="form.cache.redis.db"
                  :min="0"
                  :max="1023"
                  :step="1"
                  :disabled="isPinned('cache.redis.db')"
                  controls-position="right"
                />
                <span class="form-hint" style="margin-left: var(--space-3)">
                  {{ t('setup.cacheRedisDbHint') }}
                </span>
              </el-form-item>
              <el-form-item :label="t('setup.cacheRedisPrefix')">
                <el-input
                  v-model="form.cache.redis.prefix"
                  :disabled="isPinned('cache.redis.prefix')"
                  class="form-control"
                />
              </el-form-item>
              <el-form-item :label="t('setup.connectivityTest')">
                <el-button :loading="cacheTesting" @click="testCache">
                  {{ t('setup.testConnection') }}
                </el-button>
                <span class="form-hint" style="margin-left: var(--space-3)">
                  {{ t('setup.cacheConnectivityHint') }}
                </span>
              </el-form-item>
            </template>
          </el-form>
          <el-alert
            v-if="cacheProbeResult"
            :type="cacheProbeResult.ok ? 'success' : 'error'"
            show-icon
            :closable="false"
            :title="cacheProbeResult.message"
            style="margin-top: var(--space-2)"
          />
          <el-alert
            v-for="(w, i) in cacheProbeWarnings"
            :key="i"
            type="warning"
            show-icon
            :closable="false"
            :title="w"
            style="margin-top: var(--space-2)"
          />
        </div>

        <div v-show="activeKey === 'storage'">
          <el-alert
            type="warning"
            show-icon
            :closable="false"
            :title="t('setup.storageImmutableTitle')"
            :description="t('setup.storageImmutableDescription')"
            style="margin-bottom: var(--space-4)"
          />
          <el-form :label-width="formLabelWidth(112)">
            <el-form-item :label="t('setup.storageBackend')">
              <el-radio-group
                v-model="form.storage.driver"
                :disabled="isPinned('storage.driver')"
                @change="onStorageDriverChange"
              >
                <el-radio value="local">{{ t('setup.storageLocal') }}</el-radio>
                <el-radio value="s3">{{ t('setup.storageS3') }}</el-radio>
              </el-radio-group>
            </el-form-item>
            <template v-if="form.storage.driver === 's3'">
              <el-form-item :label="t('setup.storageEndpoint')">
                <el-input
                  v-model="form.storage.s3.endpoint"
                  :disabled="isPinned('storage.s3.endpoint')"
                  class="form-control"
                  placeholder="https://minio.example.com"
                />
              </el-form-item>
              <el-form-item :label="t('setup.storageBucket')">
                <el-input
                  v-model="form.storage.s3.bucket"
                  :disabled="isPinned('storage.s3.bucket')"
                  class="form-control"
                  placeholder="lau6-cr"
                />
              </el-form-item>
              <el-form-item :label="t('setup.storageRegion')">
                <el-input
                  v-model="form.storage.s3.region"
                  :disabled="isPinned('storage.s3.region')"
                  class="form-control"
                  placeholder="us-east-1"
                />
              </el-form-item>
              <el-form-item :label="t('setup.storagePathStyle')">
                <div class="form-switch-row">
                  <el-switch
                    v-model="form.storage.s3.path_style"
                    :disabled="isPinned('storage.s3.path_style')"
                  />
                  <span class="form-switch-row__label">
                    {{ t('setup.storagePathStyleHint') }}
                  </span>
                </div>
              </el-form-item>
              <el-form-item :label="t('setup.storageAccessKey')">
                <el-input
                  v-model="form.storage.s3.access_key"
                  class="form-control"
                  :placeholder="t('setup.storageAccessKeyPlaceholder')"
                />
              </el-form-item>
              <el-form-item :label="t('setup.storageSecretKey')">
                <el-input
                  v-model="form.storage.s3.secret_key"
                  type="password"
                  show-password
                  class="form-control"
                  :placeholder="t('setup.storageSecretKeyPlaceholder')"
                />
              </el-form-item>
              <el-form-item :label="t('setup.connectivityTest')">
                <el-button :loading="storageTesting" @click="testStorage">
                  {{ t('setup.testConnection') }}
                </el-button>
                <span class="form-hint" style="margin-left: var(--space-3)">
                  {{ t('setup.storageConnectivityHint') }}
                </span>
              </el-form-item>
            </template>
            <el-form-item v-else :label="t('setup.storageLocal')">
              <span class="form-hint">
                {{ t('setup.storageLocalHint') }}
              </span>
            </el-form-item>
          </el-form>
          <el-alert
            v-if="storageProbeResult"
            :type="storageProbeResult.ok ? 'success' : 'error'"
            show-icon
            :closable="false"
            :title="storageProbeResult.message"
            style="margin-top: var(--space-2)"
          />
        </div>

        <div v-show="activeKey === 'public_url'">
          <el-form :label-width="formLabelWidth(112)">
            <el-form-item :label="t('setup.publicUrl')">
              <el-input
                v-model="form.public_url"
                :disabled="isPinned('core.public_url')"
                class="form-control"
                placeholder="https://cr.example.com"
              />
            </el-form-item>
          </el-form>
          <el-alert
            type="info"
            show-icon
            :closable="false"
            :title="t('setup.publicUrlHint')"
          />
        </div>

        <div v-show="activeKey === 'admin'">
          <el-form :label-width="formLabelWidth(112)">
            <el-form-item :label="t('setup.adminUsername')">
              <el-input v-model="form.admin_username" class="form-control" />
            </el-form-item>
            <el-form-item :label="t('setup.adminPassword')">
              <el-input
                v-model="form.admin_password"
                type="password"
                show-password
                class="form-control"
                :placeholder="t('setup.adminPasswordPlaceholder')"
              />
            </el-form-item>
            <el-form-item :label="t('setup.adminPasswordConfirm')">
              <el-input
                v-model="form.admin_password_confirm"
                type="password"
                show-password
                class="form-control"
              />
            </el-form-item>
            <el-form-item :label="t('setup.anonymousRead')">
              <div class="form-switch-row">
                <el-switch v-model="form.anonymous_read" :disabled="isPinned('access.anonymous_read_enabled')" />
                <span class="form-switch-row__label">
                  {{ t('setup.anonymousReadHint') }}
                </span>
              </div>
            </el-form-item>
            <el-form-item :label="t('setup.ldapEnabled')">
              <el-switch v-model="form.ldap.enabled" :disabled="isPinned('ldap.enabled')" />
            </el-form-item>
            <template v-if="form.ldap.enabled">
              <el-form-item :label="t('setup.ldapUrl')">
                <el-input
                  v-model="form.ldap.url"
                  :disabled="isPinned('ldap.url')"
                  class="form-control"
                  placeholder="ldap://ldap:389"
                />
              </el-form-item>
              <el-form-item :label="t('setup.ldapDc')">
                <el-input
                  v-model="form.ldap.dc"
                  :disabled="isPinned('ldap.dc')"
                  class="form-control"
                  placeholder="dc=example,dc=com"
                />
              </el-form-item>
              <el-form-item :label="t('setup.ldapUserFilter')">
                <el-input
                  v-model="form.ldap.user_segment"
                  :disabled="isPinned('ldap.user_segment')"
                  class="form-control"
                  placeholder="uid={0},ou=people"
                />
              </el-form-item>
              <el-form-item :label="t('setup.ldapStartTls')">
                <el-switch v-model="form.ldap.start_tls" :disabled="isPinned('ldap.start_tls')" />
              </el-form-item>
              <el-form-item :label="t('setup.ldapAllowCreate')">
                <el-switch v-model="form.ldap.allow_create" :disabled="isPinned('ldap.allow_create')" />
              </el-form-item>
              <el-form-item :label="t('setup.ldapTimeout')">
                <el-input-number
                  v-model="form.ldap.timeout_sec"
                  :disabled="isPinned('ldap.timeout_sec')"
                  :min="1"
                  :max="60"
                />
              </el-form-item>
              <el-form-item :label="t('setup.connectivityTest')">
                <div class="setup-probe-row">
                  <el-input
                    v-model="ldapProbeAccount.username"
                    class="form-control--sm"
                    :placeholder="t('setup.ldapTestAccountPlaceholder')"
                  />
                  <el-input
                    v-model="ldapProbeAccount.password"
                    type="password"
                    show-password
                    class="form-control--sm"
                    :placeholder="t('setup.ldapTestPasswordPlaceholder')"
                  />
                  <el-button :loading="ldapTesting" @click="testLDAP">
                    {{ t('setup.testConnection') }}
                  </el-button>
                </div>
                <div class="form-hint">
                  {{ t('setup.ldapTestHint') }}
                </div>
              </el-form-item>
            </template>
          </el-form>
          <el-alert
            v-if="ldapProbeResult"
            :type="ldapProbeResult.ok ? 'success' : 'error'"
            show-icon
            :closable="false"
            :title="ldapProbeResult.message"
            style="margin-top: var(--space-2)"
          />
        </div>

        <div v-show="activeKey === 'confirm'">
          <el-descriptions :column="1" border class="setup-summary">
            <el-descriptions-item :label="t('setup.summaryDatabase')">
              {{ form.database.driver }}
              <span class="text-muted">{{ databaseSummary }}</span>
            </el-descriptions-item>
            <el-descriptions-item :label="t('setup.summaryCache')">
              {{ form.cache.driver }}
              <span v-if="form.cache.driver === 'redis'" class="text-muted">
                {{ form.cache.redis.addr }}
                <template v-if="typeof form.cache.redis.db === 'number'">
                  · {{ t('setup.cacheRedisDb') }} {{ form.cache.redis.db }}
                </template>
              </span>
            </el-descriptions-item>
            <el-descriptions-item v-if="distributionEmbedded" :label="t('setup.summaryStorage')">
              {{ form.storage.driver }}
              <span v-if="form.storage.driver === 's3'" class="text-muted">
                {{ form.storage.s3.bucket }} @ {{ form.storage.s3.endpoint }}
              </span>
            </el-descriptions-item>
            <el-descriptions-item :label="t('setup.summaryPublicUrl')">{{ form.public_url }}</el-descriptions-item>
            <el-descriptions-item :label="t('setup.summaryAdmin')">{{ form.admin_username }}</el-descriptions-item>
            <el-descriptions-item :label="t('setup.summaryAnonymousRead')">
              {{ form.anonymous_read ? t('setup.enabled') : t('setup.disabled') }}
            </el-descriptions-item>
            <el-descriptions-item :label="t('setup.summaryLdap')">
              {{ form.ldap.enabled ? form.ldap.url : t('setup.notEnabled') }}
            </el-descriptions-item>
          </el-descriptions>
          <el-alert
            type="warning"
            show-icon
            :closable="false"
            :title="
              distributionEmbedded
                ? t('setup.confirmCloseTitle')
                : t('setup.confirmCloseTitleNoStorage')
            "
            style="margin-top: var(--space-4)"
          />
        </div>

        <div class="setup-actions">
          <el-button :disabled="activeStep === 0" @click="prev">{{ t('setup.prev') }}</el-button>
          <el-button v-if="!isLastStep" type="primary" @click="next">{{ t('setup.next') }}</el-button>
          <el-button v-else type="primary" :loading="submitting" @click="submit">
            {{ t('setup.submit') }}
          </el-button>
        </div>
      </template>
    </div>
  </div>
</template>
