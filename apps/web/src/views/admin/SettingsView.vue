<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { useI18n } from 'vue-i18n'

import { errorMessage } from '@/api/client'
import {
  exportConfig,
  fetchSettings,
  listConfigBackups,
  patchSettings,
  rollbackConfig,
} from '@/api/settings'
import type { ConfigBackup } from '@/api/settings'
import PageHeader from '@/components/PageHeader.vue'
import SettingField from '@/components/SettingField.vue'
import type { SettingItem, SettingsPayload } from '@/types'
import { formatBytes, formatUnixSeconds } from '@/utils/format'

const { t } = useI18n()

interface SettingGroup {
  /** 分组标题的语言包键。 */
  titleKey: string
  keys: string[]
}

/** 按 key 分组渲染；后端没返回的 key 直接跳过。 */
const GROUPS: SettingGroup[] = [
  {
    titleKey: 'settings.groupBasic',
    keys: [
      'core.public_url',
      'core.session_ttl_seconds',
      'core.token_ttl_seconds',
      'core.user_token_ttl_seconds',
      'core.pat_ttl_seconds',
      'core.project_token_ttl_seconds',
      'core.trusted_proxies',
    ],
  },
  {
    titleKey: 'settings.groupAccess',
    keys: [
      'access.anonymous_read_enabled',
      'access.anonymous_token_rpm',
      'access.anonymous_read_rpm',
      'access.login_fail_limit',
      'access.login_lock_seconds',
    ],
  },
  {
    titleKey: 'settings.groupRegistry',
    keys: ['registry.prevent_tag_overwrite', 'registry.tag_overwrite_exclusions'],
  },
  {
    // 镜像清理的开关与节奏。真正的保留策略（规则）不在这里配，
    // 在「镜像清理」页与项目详情页配——那两处是结构化的编辑器。
    titleKey: 'settings.groupCleanup',
    keys: [
      'cleanup.enabled',
      'cleanup.interval_minutes',
      'cleanup.trigger_on_push',
      'cleanup.push_trigger_cooldown',
      'cleanup.protect_latest',
      'cleanup.dry_run',
    ],
  },
  {
    // blob 回收（镜像清理的第 ② 层）：把"已解引用但仍占盘"的 blob 清掉。
    // 回收由 distribution 自己执行（lau6-cr 只调度），
    // 所以 local 与对象存储是同一条路，不需要填存储根目录。
    titleKey: 'settings.groupGc',
    keys: [
      'cleanup.gc.enabled',
      'cleanup.gc.delete_untagged',
      'cleanup.gc.dry_run',
      'cleanup.gc.trigger_after_cleanup',
      'cleanup.gc.schedule_enabled',
      'cleanup.gc.schedule_at',
      'cleanup.gc.drain_timeout',
    ],
  },
  {
    titleKey: 'settings.groupStats',
    keys: [
      'stats.enabled',
      'stats.flush_interval',
      'stats.hourly_retention',
      'stats.daily_retention',
      'stats.record_ip',
      'stats.prune_hour',
      'stats.max_buckets',
    ],
  },
  {
    titleKey: 'settings.groupAuth',
    keys: [
      'ldap.enabled',
      'ldap.url',
      'ldap.dc',
      'ldap.user_segment',
      'ldap.start_tls',
      'ldap.allow_create',
      'ldap.timeout_sec',
      'password.mode',
    ],
  },
  {
    // 存储后端：embedded 形态下由 lau6-cr 渲染进 distribution 的配置，
    // 所以 S3 参数改完能自动重渲染 + 重启 distribution 子进程（T1）。
    // external 形态下这几项后端不返回——那时 distribution 用它自己的配置文件。
    titleKey: 'settings.groupStorage',
    keys: [
      'storage.driver',
      'storage.s3.endpoint',
      'storage.s3.bucket',
      'storage.s3.region',
      'storage.s3.path_style',
    ],
  },
  {
    // 缓存是 lau6-cr 自己的运行时缓存，与上面那个"存储后端"是两回事。
    titleKey: 'settings.groupCache',
    keys: [
      'cache.driver',
      'cache.redis.addr',
      'cache.redis.password',
      'cache.redis.db',
      'cache.redis.prefix',
    ],
  },
  {
    // 日志：级别 T0 热生效（改完立刻见效），
    // 文件相关项 T2 需重启（要重建文件写入器）。
    titleKey: 'settings.groupLogging',
    keys: [
      'logging.level',
      'logging.output',
      'logging.file.enabled',
      'logging.file.dir',
      'logging.file.filename',
      'logging.file.max_size_mb',
      'logging.file.rotate',
      'logging.file.retain_days',
    ],
  },
]

const loading = ref(false)
const saving = ref(false)
const exporting = ref(false)
const payload = ref<SettingsPayload | null>(null)
const form = reactive<Record<string, unknown>>({})
const baseline = ref<Record<string, unknown>>({})
const pendingRestart = ref<string[]>([])
const backups = ref<ConfigBackup[]>([])
const backupsLoading = ref(false)
/** 正在回滚的备份名；空串 = 没有正在进行的回滚。 */
const rollingBack = ref('')

/**
 * 页面有两种状态：
 *
 * - **只读**（默认）：值以纯文本展示，页头给一个「编辑」入口
 * - **编辑**：值变成控件，底部粘性条给「取消 / 保存」
 *
 * 只读态不摆控件——一屏输入框让人以为随时能改，实际改一个要跳三十个坑。
 */
const editing = ref(false)

const groups = computed(() => {
  const items = payload.value?.settings ?? []
  return GROUPS.map((group) => ({
    title: t(group.titleKey),
    items: group.keys
      .map((key) => items.find((item) => item.key === key))
      .filter((item): item is SettingItem => Boolean(item)),
  })).filter((group) => group.items.length > 0)
})

const changedKeys = computed(() => {
  const result: string[] = []
  for (const item of payload.value?.settings ?? []) {
    if (!item.editable) continue
    if (!sameValue(form[item.key], baseline.value[item.key])) result.push(item.key)
  }
  return result
})

/**
 * 口令传输模式是不是 required。
 *
 * 读的是**表单里的当前值**（含未保存的改动）——
 * 编辑态切到 required 时就要立刻警告，不能等保存完才说。
 */
const passwordModeRequired = computed(() => form['password.mode'] === 'required')

/** 这次保存会不会把口令传输模式切到 required。 */
const savingPasswordModeRequired = computed(
  () =>
    changedKeys.value.includes('password.mode') &&
    form['password.mode'] === 'required' &&
    baseline.value['password.mode'] !== 'required',
)

function sameValue(a: unknown, b: unknown): boolean {
  return JSON.stringify(a ?? null) === JSON.stringify(b ?? null)
}

/** 一项可编辑的设置都没有时，「编辑」按钮没意义。 */
const hasEditable = computed(() =>
  (payload.value?.settings ?? []).some((item) => item.editable),
)

/** 把表单恢复成基线值（丢弃本地改动）。 */
function resetFormToBaseline(): void {
  for (const key of Object.keys(form)) delete form[key]
  for (const item of payload.value?.settings ?? []) {
    form[item.key] = baseline.value[item.key]
  }
}

function startEdit(): void {
  editing.value = true
}

function cancelEdit(): void {
  resetFormToBaseline()
  editing.value = false
}

function applyPayload(data: SettingsPayload): void {
  payload.value = data
  pendingRestart.value = data.pending_restart ?? []
  for (const key of Object.keys(form)) delete form[key]
  for (const item of data.settings ?? []) {
    form[item.key] = item.value
    baseline.value[item.key] = item.value
  }
}

async function load(): Promise<void> {
  loading.value = true
  try {
    const data = await fetchSettings()
    applyPayload(data)
  } catch (err) {
    ElMessage.error(errorMessage(err, t('settings.loadFailed')))
  } finally {
    loading.value = false
  }
}

/** 拉配置备份列表（最新的在前）。 */
async function loadBackups(): Promise<void> {
  backupsLoading.value = true
  try {
    const data = await listConfigBackups()
    backups.value = data.backups ?? []
  } catch (err) {
    ElMessage.error(errorMessage(err, t('settings.backupsFailed')))
  } finally {
    backupsLoading.value = false
  }
}

/**
 * 回滚到指定备份。
 *
 * ⚠️ 回滚只改配置文件，运行期仍以 system_settings 为准——必须重启容器才生效。
 *    所以确认框与成功提示都要把这句话讲清楚，不能让人以为"点完就好了"。
 */
async function doRollback(backup: ConfigBackup): Promise<void> {
  try {
    await ElMessageBox.confirm(
      t('settings.rollbackConfirm', {
        time: formatUnixSeconds(backup.created_at),
      }),
      t('settings.rollbackTitle'),
      {
        type: 'warning',
        confirmButtonText: t('settings.rollbackConfirmButton'),
        cancelButtonText: t('common.cancel'),
      },
    )
  } catch {
    // 用户取消，不是失败。
    return
  }

  rollingBack.value = backup.name
  try {
    const result = await rollbackConfig(backup.name)
    ElMessage.warning(result.note)
    await Promise.all([loadBackups(), load()])
  } catch (err) {
    ElMessage.error(errorMessage(err, t('settings.rollbackFailed')))
  } finally {
    rollingBack.value = ''
  }
}

/**
 * 强制重新导出 application.yaml。
 *
 * 出现"YAML 导出失败"时用：DB 与内存快照是对的，只是文件是旧的。
 * 导出完再刷新页面，"配置文件未同步"的提示就该消失。
 */
async function doExport(): Promise<void> {
  exporting.value = true
  try {
    await exportConfig()
    ElMessage.success(t('settings.exported'))
    await load()
  } catch (err) {
    ElMessage.error(errorMessage(err, t('settings.exportFailed')))
  } finally {
    exporting.value = false
  }
}

async function save(): Promise<void> {
  if (changedKeys.value.length === 0) {
    ElMessage.info(t('settings.noChanges'))
    return
  }

  const changes: Record<string, unknown> = {}
  for (const key of changedKeys.value) changes[key] = form[key]

  // ⚠️ 切到 required 之前必须明确警告：
  //    未升级的旧前端没有加密能力，切过去之后**登不上**。
  if (savingPasswordModeRequired.value) {
    try {
      await ElMessageBox.confirm(
        t('settings.requiredConfirm'),
        t('settings.requiredTitle'),
        {
          type: 'warning',
          confirmButtonText: t('settings.requiredConfirmButton'),
          cancelButtonText: t('common.cancel'),
        },
      )
    } catch {
      // 用户取消，不是失败。
      return
    }
  }

  saving.value = true
  try {
    const data = await patchSettings({
      version: payload.value?.version ?? 0,
      changes,
    })
    applyPayload(data)
    // 保存成功 → 回到只读态。失败时留在编辑态，改动还在表单里。
    editing.value = false
    if ((data.pending_restart ?? []).length > 0) {
      ElMessage.warning(
        t('settings.pendingRestart', { keys: (data.pending_restart ?? []).join(t('common.listSeparator')) }),
      )
    } else {
      ElMessage.success(t('settings.saved'))
    }
  } catch (err) {
    ElMessage.error(errorMessage(err, t('settings.saveFailed')))
  } finally {
    saving.value = false
  }
}

onMounted(() => {
  void load()
  void loadBackups()
})
</script>

<template>
  <div class="page-panel">
    <page-header :title="t('route.adminSettings')">
      <template #subtitle>
        <span class="page-meta">
          <span class="mono">{{ t('settings.version', { version: payload?.version ?? '-' }) }}</span>
          <template v-if="editing">
            <span class="page-meta__sep">·</span>
            <el-tag size="small" type="warning" effect="plain">{{ t('settings.editing') }}</el-tag>
          </template>
          <template v-if="payload?.export_stale">
            <span class="page-meta__sep">·</span>
            <span style="color: var(--amber-fg)">{{ t('settings.exportStale') }}</span>
          </template>
        </span>
      </template>

      <template #actions v-if="!editing">
        <el-button :loading="loading" @click="load">{{ t('settings.reload') }}</el-button>
        <el-button
          v-if="payload?.export_stale"
          :loading="exporting"
          @click="doExport"
        >
          {{ t('settings.exportNow') }}
        </el-button>
        <el-button type="primary" :disabled="!hasEditable" @click="startEdit">
          {{ t('common.edit') }}
        </el-button>
      </template>
    </page-header>

    <el-alert
      v-if="passwordModeRequired"
      type="warning"
      show-icon
      :closable="false"
      style="margin-bottom: var(--space-4)"
      :title="t('settings.requiredWarning')"
    />

    <el-alert
      v-if="pendingRestart.length > 0"
      type="warning"
      show-icon
      :closable="false"
      style="margin-bottom: var(--space-4)"
      :title="t('settings.pendingRestart', { keys: pendingRestart.join(t('common.listSeparator')) })"
    />

    <el-alert
      v-if="payload?.export_stale"
      type="warning"
      show-icon
      :closable="false"
      style="margin-bottom: var(--space-4)"
      :title="t('settings.exportStaleDetail')"
    />

    <div v-for="group in groups" :key="group.title" class="config-group">
      <div class="config-group__header">{{ group.title }}</div>
      <div class="config-group__body">
        <setting-field
          v-for="item in group.items"
          :key="item.key"
          :item="item"
          v-model="form[item.key]"
          :readonly="!editing"
        />
      </div>
    </div>

    <div class="section-title" style="margin: var(--space-5) 0 var(--space-3)">
      <span class="page-title" style="font-size: var(--fs-md)">{{ t('settings.backupsTitle') }}</span>
      <span class="toolbar__count">{{ t('settings.backupsCount', { count: backups.length }) }}</span>
    </div>

    <el-alert
      type="info"
      show-icon
      :closable="false"
      style="margin-bottom: var(--space-3)"
      :title="t('settings.rollbackNotice')"
    />

    <el-table
      v-loading="backupsLoading"
      :data="backups"
      style="width: 100%"
      :empty-text="t('settings.noBackups')"
    >
      <el-table-column :label="t('settings.backupTime')" width="180">
        <template #default="{ row }">
          <span class="cell-mono">{{ formatUnixSeconds((row as ConfigBackup).created_at) }}</span>
        </template>
      </el-table-column>

      <el-table-column :label="t('settings.backupFile')" min-width="240">
        <template #default="{ row }">
          <span class="cell-mono">{{ (row as ConfigBackup).name }}</span>
        </template>
      </el-table-column>

      <el-table-column :label="t('projects.columnSize')" width="100">
        <template #default="{ row }">
          <span class="cell-num">{{ formatBytes((row as ConfigBackup).size) }}</span>
        </template>
      </el-table-column>

      <el-table-column :label="t('common.actions')" width="110" align="right">
        <template #default="{ row }">
          <el-button
            link
            type="warning"
            :loading="rollingBack === (row as ConfigBackup).name"
            @click="doRollback(row as ConfigBackup)"
          >
            {{ t('settings.rollback') }}
          </el-button>
        </template>
      </el-table-column>
    </el-table>

    <div v-if="editing" class="settings-actions">
      <span class="settings-actions__count">
        {{
          changedKeys.length > 0
            ? t('settings.pendingChanges', { count: changedKeys.length })
            : t('settings.noPendingChanges')
        }}
      </span>
      <el-button @click="cancelEdit">{{ t('common.cancel') }}</el-button>
      <el-button type="primary" :loading="saving" @click="save">{{ t('common.save') }}</el-button>
    </div>
  </div>
</template>
