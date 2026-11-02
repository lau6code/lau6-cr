<script setup lang="ts">
/**
 * 镜像清理（系统管理员）。
 *
 * 四块内容：
 *   1. 全局默认保留策略——没单独配策略的项目都用它。
 *   2. 立即执行——试运行 / 正式执行。正式执行不可逆，必须二次确认。
 *   3. 磁盘回收（blob GC）——把"已解引用但仍占盘"的 blob 清掉，磁盘空间才回来。
 *   4. 执行历史——每次执行的结果与明细（为什么没删干净要看这里）。
 */
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { useI18n } from 'vue-i18n'

import { errorMessage } from '@/api/client'
import {
  getCleanupRun,
  getGCStatus,
  listCleanupRuns,
  listGCRuns,
  listRetentionPolicies,
  listRetentionTemplates,
  runCleanup,
  runGC,
} from '@/api/cleanup'
import { listProjects } from '@/api/projects'
import PageHeader from '@/components/PageHeader.vue'
import RetentionPolicyEditor from '@/components/RetentionPolicyEditor.vue'
import { formatBytes, formatUnixSeconds } from '@/utils/format'
import {
  SKIP_REASON_LABELS,
  STATUS_LABELS,
  TRIGGER_LABELS,
} from '@/utils/cleanupLabels'
import { parseCleanupDetail } from '@/utils/cleanupDetail'
import { policyRuleSummaries } from '@/utils/retention'
import type {
  CleanupDeleted,
  CleanupFailure,
  CleanupRun,
  CleanupSkipped,
  GCRun,
  GCStatus,
  Project,
  RetentionPolicy,
  RetentionTemplate,
} from '@/types'

const { t } = useI18n()

// ── 策略一览（全局默认 + 各项目级覆盖）───────────────────────

const policiesLoading = ref(false)
/** 全部保留策略；全局默认 + 每个配了项目级策略的项目各一条。 */
const policies = ref<RetentionPolicy[]>([])
/** 读取失败的原因。非空时必须显示出来，不能当成"没有策略"。 */
const policiesError = ref('')
/** 规则模板清单：用来把规则翻译成中文。 */
const templates = ref<RetentionTemplate[]>([])
/** 模板读取失败的原因。非空时必须显示出来，不能当成"没有模板"。 */
const templateError = ref('')

/** 项目级策略（全局默认单独在上面的编辑器里，不进这张表）。 */
const projectPolicies = computed(() => policies.value.filter((p) => p.project_id))

function policySummaries(policy: RetentionPolicy): string[] {
  return policyRuleSummaries(policy, templates.value)
}

async function loadPolicies(): Promise<void> {
  policiesLoading.value = true
  policiesError.value = ''
  try {
    const data = await listRetentionPolicies()
    policies.value = data.policies ?? []
  } catch (err) {
    policiesError.value = errorMessage(err, t('cleanup.policiesFailed'))
    policies.value = []
  } finally {
    policiesLoading.value = false
  }
}

/** 读规则模板（只为了把规则翻译成中文；读不到就退回模板原名）。 */
async function loadTemplates(): Promise<void> {
  templateError.value = ''
  try {
    const data = await listRetentionTemplates()
    templates.value = data.templates ?? []
  } catch (err) {
    // 失败必须看得见：摘要会退回模板原名，用户得知道那不是模板该有的样子。
    templateError.value = errorMessage(err, t('retention.templatesFailed'))
    templates.value = []
  }
}

// ── 立即执行 ───────────────────────────────────────────────

const running = ref(false)
/**
 * 选中的项目；ALL_PROJECTS = 全部项目。
 *
 * ⚠️ 不能用空串表示"全部项目"：el-select 把空串当"空值"，
 * 选中后显示的是 placeholder 而不是选项文本。
 */
const ALL_PROJECTS = '__all__'
const runProject = ref(ALL_PROJECTS)
/** true = 试运行（只评估不删除）。默认试运行：清理不可逆。 */
const runDryRun = ref(true)

const projectsLoading = ref(false)
const projects = ref<Project[]>([])

// ── 执行历史 ───────────────────────────────────────────────

const runsLoading = ref(false)
const runs = ref<CleanupRun[]>([])

// ── 执行详情抽屉 ───────────────────────────────────────────

const drawerVisible = ref(false)
const detailLoading = ref(false)
const detailError = ref('')
const currentRun = ref<CleanupRun | null>(null)

// ── 磁盘回收（blob GC）─────────────────────────────────────

const gcStatusLoading = ref(false)
/** 回收的当前状态与配置；null = 还没读到。 */
const gcStatus = ref<GCStatus | null>(null)
const gcRunsLoading = ref(false)
const gcRuns = ref<GCRun[]>([])
/** true = 试运行（只评估不删除）。默认试运行：GC 直接删文件。 */
const gcDryRun = ref(true)
/** 回收执行中。 */
const gcRunning = ref(false)

function triggerLabel(trigger: string): string {
  const key = TRIGGER_LABELS[trigger]
  return key ? t(key) : trigger
}

function statusLabel(status: string): string {
  const key = STATUS_LABELS[status]
  return key ? t(key) : status
}

function statusTagType(status: string): 'success' | 'danger' | 'info' {
  if (status === 'success') return 'success'
  if (status === 'failed') return 'danger'
  return 'info'
}

function scopeLabel(run: CleanupRun): string {
  if (run.scope === 'all') return t('cleanup.scopeAll')
  if (run.scope.startsWith('project:')) {
    return t('cleanup.scopeProject', { name: run.scope.slice('project:'.length) })
  }
  return run.scope
}

function skipReasonLabel(reason: string): string {
  const key = SKIP_REASON_LABELS[reason]
  return key ? t(key) : reason
}

/**
 * 解析执行明细。
 *
 * 没有明细不是错误（后端把它当可选的增强信息存）；
 * **解析失败**是错误——明细是"为什么没删干净"的唯一来源，
 * 静默退化成空表会让用户以为"这次什么都没删"。
 */
const parsedDetail = computed(() =>
  parseCleanupDetail(currentRun.value, t('cleanup.detailFailed')),
)

const detail = computed(() => parsedDetail.value.detail)
/** 明细解析失败的原因；非空时必须在页面上显示出来。 */
const detailParseError = computed(() => parsedDetail.value.error)

const deletedRows = computed(() => detail.value.deleted ?? [])
const skippedRows = computed(() => detail.value.skipped ?? [])
const failureRows = computed(() => detail.value.failures ?? [])
/** 审计写失败的条目：tag 删了但审计链上没有痕迹——要单独一栏，不能混进"失败明细"。 */
const auditFailureRows = computed(() => detail.value.audit_failures ?? [])

async function loadProjects(): Promise<void> {
  projectsLoading.value = true
  try {
    const data = await listProjects()
    projects.value = data.projects ?? []
  } catch (err) {
    ElMessage.error(errorMessage(err, t('projects.loadFailed')))
    projects.value = []
  } finally {
    projectsLoading.value = false
  }
}

async function loadRuns(): Promise<void> {
  runsLoading.value = true
  try {
    const data = await listCleanupRuns({ limit: 50, offset: 0 })
    runs.value = data.runs ?? []
  } catch (err) {
    ElMessage.error(errorMessage(err, t('cleanup.runsFailed')))
    runs.value = []
  } finally {
    runsLoading.value = false
  }
}

/** 打开详情抽屉：按 id 再取一次完整记录。 */
async function openDetail(run: CleanupRun): Promise<void> {
  drawerVisible.value = true
  detailLoading.value = true
  detailError.value = ''
  currentRun.value = run
  try {
    const data = await getCleanupRun(run.id)
    currentRun.value = data.run
  } catch (err) {
    detailError.value = errorMessage(err, t('cleanup.detailFailed'))
  } finally {
    detailLoading.value = false
  }
}

async function execute(): Promise<void> {
  if (!runDryRun.value) {
    // 正式执行会真删：不可逆，必须二次确认。
    try {
      await ElMessageBox.confirm(
        t('cleanup.executeConfirm'),
        t('cleanup.executeTitle'),
        {
          type: 'warning',
          confirmButtonText: t('cleanup.executeConfirmButton'),
          cancelButtonText: t('common.cancel'),
        },
      )
    } catch {
      // 用户取消，不是失败。
      return
    }
  }

  running.value = true
  try {
    const data = await runCleanup({
      project: runProject.value === ALL_PROJECTS ? '' : runProject.value,
      dry_run: runDryRun.value,
    })
    const run = data.run
    if (run.status === 'failed') {
      ElMessage.error(
        run.error ? t('cleanup.runFailedWith', { reason: run.error }) : t('cleanup.runFailed'),
      )
    } else if (run.dry_run) {
      ElMessage.success(
        t('cleanup.dryRunDone', { deleted: run.tags_deleted, kept: run.tags_kept }),
      )
    } else {
      ElMessage.success(
        t('cleanup.runDone', {
          tags: run.tags_deleted,
          manifests: run.manifests_deleted,
          kept: run.tags_kept,
        }),
      )
    }
    await loadRuns()
    // 保留策略执行后可能自动跑了一次磁盘回收，历史与状态一起刷新。
    await loadGCRuns()
    await loadGCStatus()
    await openDetail(run)
  } catch (err) {
    ElMessage.error(errorMessage(err, t('cleanup.executeFailed')))
    // 失败也会落一条执行记录：刷新历史，让用户能看到失败明细。
    await loadRuns()
  } finally {
    running.value = false
  }
}

async function loadGCStatus(): Promise<void> {
  gcStatusLoading.value = true
  try {
    gcStatus.value = await getGCStatus()
  } catch (err) {
    ElMessage.error(errorMessage(err, t('cleanup.gcStatusFailed')))
    gcStatus.value = null
  } finally {
    gcStatusLoading.value = false
  }
}

async function loadGCRuns(): Promise<void> {
  gcRunsLoading.value = true
  try {
    const data = await listGCRuns({ limit: 20, offset: 0 })
    gcRuns.value = data.runs ?? []
  } catch (err) {
    ElMessage.error(errorMessage(err, t('cleanup.gcRunsFailed')))
    gcRuns.value = []
  } finally {
    gcRunsLoading.value = false
  }
}

/**
 * 轮询间隔（毫秒）。
 *
 * ⚠️ 别太短：回收在对象存储下可能跑几十分钟到几小时，
 * 每次都拉全量历史只是浪费。
 */
const GC_POLL_INTERVAL_MS = 5000

/** 回收轮询的定时器；null = 没在轮询。 */
let gcPollTimer: ReturnType<typeof setInterval> | null = null

/**
 * 启动轮询：回收在后台跑，结果要靠轮询执行记录才看得到。
 *
 * 停下来的条件：最近一条记录不再处于 running。
 */
function startGCPoll(): void {
  stopGCPoll()
  gcPollTimer = setInterval(() => {
    void tickGCPoll()
  }, GC_POLL_INTERVAL_MS)
}

function stopGCPoll(): void {
  if (gcPollTimer !== null) {
    clearInterval(gcPollTimer)
    gcPollTimer = null
  }
}

/** 轮询一次：刷新记录与状态；回收结束后报结果并停轮询。 */
async function tickGCPoll(): Promise<void> {
  await loadGCRuns()
  await loadGCStatus()
  const latest = gcRuns.value[0]
  if (!latest || latest.status !== 'running') {
    stopGCPoll()
    if (latest) {
      reportGCDone(latest)
    }
  }
}

function reportGCDone(run: GCRun): void {
  if (run.status === 'failed') {
    ElMessage.error(
      run.error ? t('cleanup.gcFailedWith', { reason: run.error }) : t('cleanup.gcFailed'),
    )
    return
  }
  if (run.dry_run) {
    ElMessage.success(
      t('cleanup.gcDryRunDone', {
        scanned: run.blobs_scanned,
        deleted: run.blobs_deleted,
        bytes: formatBytes(run.bytes_reclaimed),
      }),
    )
    return
  }
  ElMessage.success(
    t('cleanup.gcDone', {
      deleted: run.blobs_deleted,
      bytes: formatBytes(run.bytes_reclaimed),
    }),
  )
}

/**
 * 触发一次 blob 回收。
 *
 * ⚠️ 回收在**后台**执行：接口立即返回，结局看执行记录。
 * 同步等会把 HTTP 请求拖到超时（对象存储下可能跑几小时）。
 */
async function executeGC(): Promise<void> {
  if (!gcDryRun.value) {
    // 正式回收会真删 blob：不可逆，必须二次确认。
    try {
      await ElMessageBox.confirm(
        t('cleanup.gcConfirm'),
        t('cleanup.gcTitle'),
        {
          type: 'warning',
          confirmButtonText: t('cleanup.executeConfirmButton'),
          cancelButtonText: t('common.cancel'),
        },
      )
    } catch {
      // 用户取消，不是失败。
      return
    }
  }

  gcRunning.value = true
  try {
    await runGC({ dry_run: gcDryRun.value })
    ElMessage.info(t('cleanup.gcStarted'))
    await loadGCRuns()
    await loadGCStatus()
    startGCPoll()
  } catch (err) {
    ElMessage.error(errorMessage(err, t('cleanup.gcStartFailed')))
    await loadGCRuns()
  } finally {
    gcRunning.value = false
  }
}

onMounted(() => {
  void loadProjects()
  void loadRuns()
  void loadGCStatus()
  void loadGCRuns()
  void loadPolicies()
  void loadTemplates()
})

// 离开页面时停掉轮询：后台任务还在跑，页面不该继续发请求。
onUnmounted(() => {
  stopGCPoll()
})
</script>

<template>
  <div class="page-panel">
    <page-header :title="t('route.adminCleanup')">
      <template #subtitle>
        <span class="page-meta">
          <span>{{ t('cleanup.subtitle') }}</span>
        </span>
      </template>
      <template #actions>
        <el-button :loading="runsLoading" @click="loadRuns">{{ t('cleanup.refreshRuns') }}</el-button>
      </template>
    </page-header>

    <el-alert
      type="info"
      show-icon
      :closable="false"
      :title="t('cleanup.twoLayersTitle')"
      style="margin-bottom: var(--space-4)"
    >
      <div class="form-hint" style="margin-top: var(--space-2)">
        {{ t('cleanup.twoLayersHint') }}
      </div>
    </el-alert>

    <div class="section-title" style="margin: 0 0 var(--space-3)">
      <span class="page-title" style="font-size: var(--fs-md)">{{ t('retention.globalPolicy') }}</span>
    </div>

    <retention-policy-editor scope="global" />

    <!-- ── 项目级策略（一览）─────────────────────────────
         项目级策略**覆盖**全局默认：配了就不再用全局默认。
         没有这张表就看不出全局默认对哪些项目根本不生效。 -->
    <div class="section-title" style="margin: var(--space-6) 0 var(--space-3)">
      <span class="page-title" style="font-size: var(--fs-md)">
        {{ t('retention.projectPolicy') }}
      </span>
    </div>

    <el-alert
      v-if="policiesError"
      type="error"
      :title="policiesError"
      show-icon
      :closable="false"
      style="margin-bottom: var(--space-4)"
    />

    <!-- 规则模板读不到：摘要会退回模板原名，用户得知道那不是模板该有的样子。 -->
    <el-alert
      v-if="templateError"
      type="error"
      show-icon
      :closable="false"
      :title="t('retention.templatesFailedTitle', { reason: templateError })"
      style="margin-bottom: var(--space-4)"
    >
      <div class="form-hint" style="margin-top: var(--space-2)">
        {{ t('retention.templatesFailedHint') }}
      </div>
    </el-alert>

    <el-table
      v-loading="policiesLoading"
      :data="projectPolicies"
      style="width: 100%"
      :empty-text="t('cleanup.noProjectPolicies')"
    >
      <el-table-column prop="project_name" :label="t('nav.projects')" width="180">
        <template #default="{ row }">
          <RouterLink
            class="project-link"
            :to="{ name: 'project-detail', params: { name: row.project_name } }"
          >
            <span class="mono">{{ row.project_name }}</span>
          </RouterLink>
        </template>
      </el-table-column>

      <el-table-column :label="t('common.status')" width="110">
        <template #default="{ row }">
          <span
            class="status-pill"
            :class="row.enabled ? 'status-ok' : 'status-muted'"
          >
            {{ row.enabled ? t('retention.enabled') : t('retention.stopped') }}
          </span>
        </template>
      </el-table-column>

      <el-table-column :label="t('retention.rules')" min-width="280">
        <template #default="{ row }">
          <div v-for="(item, index) in policySummaries(row as RetentionPolicy)" :key="index">
            <span class="cell-mono">{{ item }}</span>
          </div>
        </template>
      </el-table-column>

      <el-table-column :label="t('cleanup.updatedAt')" width="160">
        <template #default="{ row }">
          <span class="cell-mono">{{ formatUnixSeconds(row.updated_at) }}</span>
        </template>
      </el-table-column>

      <el-table-column :label="t('common.actions')" width="110" align="right">
        <template #default="{ row }">
          <!-- 进策略编辑页，不是项目详情页——那边不承载策略配置。 -->
          <RouterLink
            class="text-link"
            :to="{
              name: 'project-retention-detail',
              params: { name: row.project_name },
            }"
          >
            {{ t('cleanup.configure') }}
          </RouterLink>
        </template>
      </el-table-column>
    </el-table>

    <div class="section-title" style="margin: var(--space-6) 0 var(--space-3)">
      <span class="page-title" style="font-size: var(--fs-md)">{{ t('cleanup.runNow') }}</span>
    </div>

    <div class="config-group">
      <div class="config-group__body">
        <div class="setting-field">
          <div class="setting-field__label">
            <span class="setting-field__key">{{ t('cleanup.scope') }}</span>
          </div>
          <div class="setting-field__control">
            <el-select
              v-model="runProject"
              :loading="projectsLoading"
              class="toolbar__select"
              :placeholder="t('cleanup.scopeAll')"
            >
              <el-option :label="t('cleanup.scopeAll')" :value="ALL_PROJECTS" />
              <el-option
                v-for="project in projects"
                :key="project.name"
                :label="t('common.parenthesized', { label: project.name, value: project.display_name })"
                :value="project.name"
              />
            </el-select>
          </div>
          <div class="setting-field__desc">
            <!-- 文案里带静态 <strong> 标记，来自语言包常量，不含用户输入。 -->
            <span v-html="t('cleanup.scopeHint')" />
            <br />
            {{ t('cleanup.scopeHintTail') }}
          </div>
        </div>

        <div class="setting-field">
          <div class="setting-field__label">
            <span class="setting-field__key">{{ t('cleanup.mode') }}</span>
          </div>
          <div class="setting-field__control">
            <el-radio-group v-model="runDryRun">
              <el-radio :value="true">{{ t('cleanup.dryRun') }}</el-radio>
              <el-radio :value="false">{{ t('cleanup.formal') }}</el-radio>
            </el-radio-group>
          </div>
          <div class="setting-field__desc">
            <template v-if="runDryRun">
              {{ t('cleanup.dryRunHint') }}
            </template>
            <template v-else>
              <strong>{{ t('cleanup.formalBold') }}</strong>{{ t('cleanup.formalHint') }}
            </template>
          </div>
        </div>

        <div class="setting-field">
          <div class="setting-field__label">
            <span class="setting-field__key">{{ t('cleanup.execute') }}</span>
          </div>
          <div class="setting-field__control">
            <el-button
              :type="runDryRun ? 'primary' : 'danger'"
              :loading="running"
              @click="execute"
            >
              {{ runDryRun ? t('cleanup.startDryRun') : t('cleanup.startFormal') }}
            </el-button>
          </div>
        </div>
      </div>
    </div>

    <div class="section-title" style="margin: var(--space-6) 0 var(--space-3)">
      <span class="page-title" style="font-size: var(--fs-md)">{{ t('cleanup.gcTitle') }}</span>
      <span class="toolbar__count">{{ t('cleanup.gcSubtitle') }}</span>
    </div>

    <el-alert
      v-if="gcStatus && !gcStatus.enabled"
      type="warning"
      show-icon
      :closable="false"
      :title="t('cleanup.gcDisabled')"
      style="margin-bottom: var(--space-3)"
    >
      <div class="form-hint" style="margin-top: var(--space-2)">
        {{ t('cleanup.gcDisabledHint') }}
      </div>
    </el-alert>

    <el-alert
      v-if="gcStatus?.schedule_enabled && gcStatus.dry_run"
      type="warning"
      show-icon
      :closable="false"
      :title="t('cleanup.gcScheduleDryRun')"
      style="margin-bottom: var(--space-3)"
    >
      <div class="form-hint" style="margin-top: var(--space-2)">
        {{ t('cleanup.gcScheduleDryRunHint', { time: gcStatus.schedule_at }) }}
      </div>
    </el-alert>

    <div class="config-group">
      <div class="config-group__body">
        <div class="setting-field">
          <div class="setting-field__label">
            <span class="setting-field__key">{{ t('cleanup.mode') }}</span>
          </div>
          <div class="setting-field__control">
            <el-radio-group v-model="gcDryRun">
              <el-radio :value="true">{{ t('cleanup.dryRun') }}</el-radio>
              <el-radio :value="false">{{ t('cleanup.formalGc') }}</el-radio>
            </el-radio-group>
          </div>
          <div class="setting-field__desc">
            <template v-if="gcDryRun">
              {{ t('cleanup.gcDryRunHint') }}
            </template>
            <template v-else>
              <strong>{{ t('cleanup.formalBold') }}</strong>{{ t('cleanup.gcFormalHint') }}
            </template>
          </div>
        </div>

        <div class="setting-field">
          <div class="setting-field__label">
            <span class="setting-field__key">{{ t('cleanup.execute') }}</span>
          </div>
          <div class="setting-field__control">
            <el-button
              :type="gcDryRun ? 'primary' : 'danger'"
              :loading="gcRunning"
              :disabled="!gcStatus?.enabled"
              @click="executeGC"
            >
              {{ gcDryRun ? t('cleanup.startDryRun') : t('cleanup.startFormalGc') }}
            </el-button>
          </div>
          <div class="setting-field__desc">
            <template v-if="gcStatus?.enabled">
              {{ t('cleanup.gcEnabledHint') }}
            </template>
            <template v-else>
              {{ t('cleanup.gcNotEnabled') }}
            </template>
          </div>
        </div>

        <div class="setting-field">
          <div class="setting-field__label">
            <span class="setting-field__key">{{ t('cleanup.schedule') }}</span>
          </div>
          <div class="setting-field__control">
            <span class="setting-field__value">
              {{
                gcStatus?.schedule_enabled
                  ? t('cleanup.scheduleEnabled', { time: gcStatus.schedule_at })
                  : t('cleanup.scheduleDisabled')
              }}
            </span>
          </div>
          <div class="setting-field__desc">{{ t('cleanup.scheduleHint') }}</div>
        </div>
      </div>
    </div>

    <div class="section-title" style="margin: var(--space-5) 0 var(--space-3)">
      <span class="page-title" style="font-size: var(--fs-md)">{{ t('cleanup.gcHistory') }}</span>
      <span class="toolbar__count">{{ t('cleanup.recentCount', { count: gcRuns.length }) }}</span>
    </div>

    <el-table
      v-loading="gcRunsLoading"
      :data="gcRuns"
      style="width: 100%"
      :empty-text="t('cleanup.noGcRuns')"
    >
      <el-table-column :label="t('cleanup.startedAt')" width="160">
        <template #default="{ row }">
          <span class="cell-mono">{{ formatUnixSeconds((row as GCRun).started_at) }}</span>
        </template>
      </el-table-column>

      <el-table-column :label="t('cleanup.trigger')" width="110">
        <template #default="{ row }">
          <span class="status-pill status-muted">
            {{ triggerLabel((row as GCRun).trigger) }}
          </span>
        </template>
      </el-table-column>

      <el-table-column :label="t('cleanup.mode')" width="96">
        <template #default="{ row }">
          <span
            class="status-pill"
            :class="(row as GCRun).dry_run ? 'status-info' : 'status-primary'"
          >
            {{ (row as GCRun).dry_run ? t('cleanup.dryRun') : t('cleanup.formal') }}
          </span>
        </template>
      </el-table-column>

      <el-table-column :label="t('common.status')" width="96">
        <template #default="{ row }">
          <el-tag
            size="small"
            :type="statusTagType((row as GCRun).status)"
            effect="light"
          >
            {{ statusLabel((row as GCRun).status) }}
          </el-tag>
        </template>
      </el-table-column>

      <el-table-column :label="t('cleanup.blobsScanned')" width="100" align="right">
        <template #default="{ row }">
          <span class="cell-num">{{ (row as GCRun).blobs_scanned }}</span>
        </template>
      </el-table-column>

      <el-table-column :label="t('cleanup.blobsReclaimed')" width="100" align="right">
        <template #default="{ row }">
          <span class="cell-num">{{ (row as GCRun).blobs_deleted }}</span>
        </template>
      </el-table-column>

      <el-table-column :label="t('cleanup.spaceReclaimed')" width="120" align="right">
        <template #default="{ row }">
          <span class="cell-num">{{ formatBytes((row as GCRun).bytes_reclaimed) }}</span>
        </template>
      </el-table-column>

      <el-table-column :label="t('common.description')" min-width="180" show-overflow-tooltip>
        <template #default="{ row }">
          <span
            v-if="(row as GCRun).error"
            class="form-hint"
            style="margin-top: 0; color: var(--danger)"
          >
            {{ (row as GCRun).error }}
          </span>
          <span v-else class="cell-mono">
            {{ t('cleanup.repoManifestCount', { repos: (row as GCRun).repositories, manifests: (row as GCRun).manifests_found }) }}
          </span>
        </template>
      </el-table-column>
    </el-table>

    <div class="section-title" style="margin: var(--space-6) 0 var(--space-3)">
      <span class="page-title" style="font-size: var(--fs-md)">{{ t('cleanup.runHistory') }}</span>
      <span class="toolbar__count">{{ t('cleanup.recentCount', { count: runs.length }) }}</span>
    </div>

    <el-table
      v-loading="runsLoading"
      :data="runs"
      style="width: 100%"
      :empty-text="t('cleanup.noRuns')"
      @row-click="(row: CleanupRun) => openDetail(row)"
    >
      <el-table-column :label="t('cleanup.startedAt')" width="160">
        <template #default="{ row }">
          <span class="cell-mono">{{ formatUnixSeconds((row as CleanupRun).started_at) }}</span>
        </template>
      </el-table-column>

      <el-table-column :label="t('cleanup.trigger')" width="100">
        <template #default="{ row }">
          <span class="status-pill status-muted">
            {{ triggerLabel((row as CleanupRun).trigger) }}
          </span>
        </template>
      </el-table-column>

      <el-table-column :label="t('cleanup.scope')" min-width="140" show-overflow-tooltip>
        <template #default="{ row }">
          <span class="cell-mono">{{ scopeLabel(row as CleanupRun) }}</span>
        </template>
      </el-table-column>

      <el-table-column :label="t('cleanup.mode')" width="96">
        <template #default="{ row }">
          <span
            class="status-pill"
            :class="(row as CleanupRun).dry_run ? 'status-info' : 'status-primary'"
          >
            {{ (row as CleanupRun).dry_run ? t('cleanup.dryRun') : t('cleanup.formal') }}
          </span>
        </template>
      </el-table-column>

      <el-table-column :label="t('common.status')" width="96">
        <template #default="{ row }">
          <el-tag
            size="small"
            :type="statusTagType((row as CleanupRun).status)"
            effect="light"
          >
            {{ statusLabel((row as CleanupRun).status) }}
          </el-tag>
        </template>
      </el-table-column>

      <el-table-column :label="t('cleanup.tagsDeleted')" width="96" align="right">
        <template #default="{ row }">
          <span class="cell-num">{{ (row as CleanupRun).tags_deleted }}</span>
        </template>
      </el-table-column>

      <el-table-column :label="t('cleanup.manifestsDeleted')" width="120" align="right">
        <template #default="{ row }">
          <span class="cell-num">{{ (row as CleanupRun).manifests_deleted }}</span>
        </template>
      </el-table-column>

      <el-table-column :label="t('cleanup.tagsKept')" width="88" align="right">
        <template #default="{ row }">
          <span class="cell-num">{{ (row as CleanupRun).tags_kept }}</span>
        </template>
      </el-table-column>

      <!-- 跳过数：说明"为什么没删干净" -->
      <el-table-column :label="t('cleanup.skipped')" width="140" align="right">
        <template #default="{ row }">
          <span class="cell-num">
            {{ (row as CleanupRun).skipped_unknown_time + (row as CleanupRun).skipped_shared_digest }}
          </span>
          <div class="form-hint" style="margin-top: 0">
            {{ t('cleanup.unknownTime', { count: (row as CleanupRun).skipped_unknown_time }) }} ·
            {{ t('cleanup.sharedDigest', { count: (row as CleanupRun).skipped_shared_digest }) }}
          </div>
        </template>
      </el-table-column>

      <el-table-column :label="t('cleanup.failed')" width="88" align="right">
        <template #default="{ row }">
          <span v-if="(row as CleanupRun).failed_count > 0" class="cell-num" style="color: var(--danger)">
            {{ (row as CleanupRun).failed_count }}
          </span>
          <span v-else class="cell-mono">0</span>
        </template>
      </el-table-column>

      <el-table-column :label="t('common.actions')" width="88" align="right">
        <template #default="{ row }">
          <el-button link type="primary" @click.stop="openDetail(row as CleanupRun)">
            {{ t('cleanup.detail') }}
          </el-button>
        </template>
      </el-table-column>
    </el-table>

    <el-drawer v-model="drawerVisible" :title="t('cleanup.detailTitle')" size="min(860px, 92%)">
      <div v-loading="detailLoading">
        <el-alert
          v-if="detailError"
          type="error"
          show-icon
          :closable="false"
          :title="detailError"
          style="margin-bottom: var(--space-4)"
        />

        <!-- 明细解析失败：下面的表是空的，但那不是"没删任何东西"，是读不出来。 -->
        <el-alert
          v-if="detailParseError"
          type="error"
          show-icon
          :closable="false"
          :title="detailParseError"
          style="margin-bottom: var(--space-4)"
        />

        <template v-if="currentRun">
          <div class="run-summary">
            <div class="run-summary__item">
              <span class="run-summary__label">{{ t('cleanup.startedAt') }}</span>
              <span class="cell-mono">{{ formatUnixSeconds(currentRun.started_at) }}</span>
            </div>
            <div class="run-summary__item">
              <span class="run-summary__label">{{ t('cleanup.finishedAt') }}</span>
              <span class="cell-mono">{{ formatUnixSeconds(currentRun.finished_at) }}</span>
            </div>
            <div class="run-summary__item">
              <span class="run-summary__label">{{ t('cleanup.trigger') }}</span>
              <span>{{ triggerLabel(currentRun.trigger) }}</span>
            </div>
            <div class="run-summary__item">
              <span class="run-summary__label">{{ t('cleanup.scope') }}</span>
              <span class="cell-mono">{{ scopeLabel(currentRun) }}</span>
            </div>
            <div class="run-summary__item">
              <span class="run-summary__label">{{ t('cleanup.mode') }}</span>
              <span>{{ currentRun.dry_run ? t('cleanup.dryRunNotDeleted') : t('cleanup.formal') }}</span>
            </div>
            <div class="run-summary__item">
              <span class="run-summary__label">{{ t('common.status') }}</span>
              <el-tag size="small" :type="statusTagType(currentRun.status)" effect="light">
                {{ statusLabel(currentRun.status) }}
              </el-tag>
            </div>
            <div class="run-summary__item">
              <span class="run-summary__label">{{ t('cleanup.deletedKept') }}</span>
              <span class="cell-num">
                {{ currentRun.tags_deleted }} / {{ currentRun.tags_kept }}
              </span>
            </div>
            <div class="run-summary__item">
              <span class="run-summary__label">{{ t('cleanup.skipped') }}</span>
              <span class="cell-num">
                {{ t('cleanup.unknownTime', { count: currentRun.skipped_unknown_time }) }} ·
                {{ t('cleanup.sharedDigest', { count: currentRun.skipped_shared_digest }) }}
              </span>
            </div>
          </div>

          <el-alert
            v-if="currentRun.error"
            type="error"
            show-icon
            :closable="false"
            :title="currentRun.error"
            style="margin-bottom: var(--space-4)"
          />

          <el-alert
            v-if="detail.gc"
            :type="detail.gc.status === 'failed' ? 'error' : 'success'"
            show-icon
            :closable="false"
            :title="
              detail.gc.status === 'failed'
                ? t('cleanup.gcFailedWith', { reason: detail.gc.error ?? t('common.unknown') })
                : t('cleanup.gcSummary', {
                    dryRun: detail.gc.dry_run ? t('cleanup.dryRunTag') : '',
                    deleted: detail.gc.blobs_deleted,
                    bytes: formatBytes(detail.gc.bytes_reclaimed),
                  })
            "
            style="margin-bottom: var(--space-4)"
          />

          <div class="section-title" style="margin-bottom: var(--space-2)">
            <span class="page-title" style="font-size: var(--fs-md)">
              {{ t('cleanup.deletedDetail', { count: deletedRows.length }) }}
            </span>
          </div>
          <el-table :data="deletedRows" style="width: 100%" :empty-text="t('cleanup.noDeleted')">
            <el-table-column prop="repo" :label="t('catalog.columnRepo')" min-width="180" show-overflow-tooltip>
              <template #default="{ row }">
                <span class="cell-mono">{{ (row as CleanupDeleted).repo }}</span>
              </template>
            </el-table-column>
            <el-table-column :label="t('repository.columnTag')" min-width="180" show-overflow-tooltip>
              <template #default="{ row }">
                <span class="cell-mono">
                  {{ ((row as CleanupDeleted).tags ?? []).join(t('common.listSeparator')) }}
                </span>
              </template>
            </el-table-column>
            <el-table-column :label="t('cleanup.digest')" min-width="200" show-overflow-tooltip>
              <template #default="{ row }">
                <span class="cell-mono">{{ (row as CleanupDeleted).digest }}</span>
              </template>
            </el-table-column>
            <el-table-column :label="t('projects.columnSize')" width="110" align="right">
              <template #default="{ row }">
                <span class="cell-num">{{ formatBytes((row as CleanupDeleted).size) }}</span>
              </template>
            </el-table-column>
          </el-table>

          <div class="section-title" style="margin: var(--space-5) 0 var(--space-2)">
            <span class="page-title" style="font-size: var(--fs-md)">
              {{ t('cleanup.skippedDetail', { count: skippedRows.length }) }}
            </span>
          </div>
          <el-table :data="skippedRows" style="width: 100%" :empty-text="t('cleanup.noSkipped')">
            <el-table-column prop="repo" :label="t('catalog.columnRepo')" min-width="180" show-overflow-tooltip>
              <template #default="{ row }">
                <span class="cell-mono">{{ (row as CleanupSkipped).repo }}</span>
              </template>
            </el-table-column>
            <el-table-column prop="tag" :label="t('repository.columnTag')" width="160">
              <template #default="{ row }">
                <span class="cell-mono">{{ (row as CleanupSkipped).tag }}</span>
              </template>
            </el-table-column>
            <el-table-column :label="t('cleanup.reason')" min-width="220">
              <template #default="{ row }">
                <span class="status-pill status-warn">
                  {{ skipReasonLabel((row as CleanupSkipped).reason) }}
                </span>
              </template>
            </el-table-column>
          </el-table>

          <div class="section-title" style="margin: var(--space-5) 0 var(--space-2)">
            <span class="page-title" style="font-size: var(--fs-md)">
              {{ t('cleanup.failedDetail', { count: failureRows.length }) }}
            </span>
          </div>
          <el-table :data="failureRows" style="width: 100%" :empty-text="t('cleanup.noFailures')">
            <el-table-column prop="repo" :label="t('catalog.columnRepo')" min-width="180" show-overflow-tooltip>
              <template #default="{ row }">
                <span class="cell-mono">{{ (row as CleanupFailure).repo }}</span>
              </template>
            </el-table-column>
            <el-table-column prop="tag" :label="t('repository.columnTag')" width="160">
              <template #default="{ row }">
                <span class="cell-mono">{{ (row as CleanupFailure).tag || '-' }}</span>
              </template>
            </el-table-column>
            <el-table-column prop="error" :label="t('cleanup.error')" min-width="240" show-overflow-tooltip />
          </el-table>

          <div class="section-title" style="margin: var(--space-5) 0 var(--space-2)">
            <span class="page-title" style="font-size: var(--fs-md)">
              {{ t('cleanup.auditFailures', { count: auditFailureRows.length }) }}
            </span>
          </div>
          <el-alert
            v-if="auditFailureRows.length"
            type="warning"
            :title="t('cleanup.auditFailuresHint')"
            show-icon
            :closable="false"
            style="margin-bottom: var(--space-3)"
          />
          <el-table :data="auditFailureRows" style="width: 100%" :empty-text="t('cleanup.noAuditFailures')">
            <el-table-column prop="repo" :label="t('catalog.columnRepo')" min-width="180" show-overflow-tooltip>
              <template #default="{ row }">
                <span class="cell-mono">{{ (row as CleanupFailure).repo }}</span>
              </template>
            </el-table-column>
            <el-table-column prop="tag" :label="t('repository.columnTag')" width="160">
              <template #default="{ row }">
                <span class="cell-mono">{{ (row as CleanupFailure).tag || '-' }}</span>
              </template>
            </el-table-column>
            <el-table-column prop="error" :label="t('cleanup.error')" min-width="240" show-overflow-tooltip />
          </el-table>
        </template>
      </div>
    </el-drawer>
  </div>
</template>

<style scoped>
.run-summary {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(220px, 1fr));
  gap: var(--space-3);
  margin-bottom: var(--space-4);
  padding: var(--space-4);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  background: var(--content-bg);
}

.run-summary__item {
  display: flex;
  flex-direction: column;
  gap: var(--space-1);
  min-width: 0;
}

.run-summary__label {
  font-size: var(--fs-xs);
  letter-spacing: 0.5px;
  text-transform: uppercase;
  color: var(--text-tertiary);
}
</style>
