<script setup lang="ts">
/**
 * 保留策略编辑器。
 *
 * 项目级（/projects/{name}/retention）与全局默认（/admin/cleanup/policy）
 * 后端接口不同，但**数据结构和校验规则完全一样**，所以合成一个组件：
 * scope 只决定调哪几个接口，规则编辑部分逐字复用。
 *
 * 两条硬约束（对应仓库 AGENTS.md）：
 * 1. 读策略 / 读模板失败必须让人看见——分别用 error 提示条展示原始错误，
 *    不能当成"没有策略"继续渲染。
 * 2. 保存前在本地按后端的校验口径检查一遍（见 service.ValidateRetentionPolicy），
 *    但**以服务端返回的错误为准**：本地检查只是提前说清楚，不替代后端校验。
 */
import { computed, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { useI18n } from 'vue-i18n'

import { errorMessage } from '@/api/client'
import {
  deleteProjectRetention,
  getGlobalRetentionPolicy,
  getProjectRetention,
  listRetentionTemplates,
  putGlobalRetentionPolicy,
  putProjectRetention,
  type RetentionPolicyPayload,
} from '@/api/cleanup'
import { policyRuleSummaries, unitLabel } from '@/utils/retention'
import type { RetentionPolicy, RetentionRule, RetentionTemplate } from '@/types'
import { formLabelWidth } from '@/i18n'

const { t } = useI18n()

const props = defineProps<{
  /** project = 项目级策略（可重置回全局默认）；global = 全局默认策略。 */
  scope: 'project' | 'global'
  /** 项目名；scope 为 project 时必填。 */
  projectName?: string
}>()

/** params 里没填的值是 undefined（不写进请求体）。 */
interface DraftRule {
  /** 稳定 key：删掉中间一条时，后面的输入框不会串状态。 */
  uid: number
  template: string
  params: Record<string, number | undefined>
  tag_selector: string
}

/** 规则 key 计数器（只在本地用，不进请求体）。 */
let ruleSeq = 0

const loading = ref(false)
const saving = ref(false)
const resetting = ref(false)

/** 策略读取失败的原因。非空时必须显示出来，不能当成"没有策略"。 */
const loadError = ref('')
/** 模板列表读取失败的原因。 */
const templateError = ref('')

const policy = ref<RetentionPolicy | null>(null)
/**
 * 实际生效的策略与它的来源（仅项目级有）。
 *
 * 后端把这两项一起返回：项目管理员可能压根没配项目级策略，
 * 那时生效的是全局默认——只显示骨架会让他以为"这个项目不受清理影响"，
 * 而实际上全局默认正在删它的镜像。
 */
const effective = ref<RetentionPolicy | null>(null)
const effectiveSource = ref<'project' | 'global' | ''>('')
const templates = ref<RetentionTemplate[]>([])
/** 后端给的 tag 选择器语法说明。 */
const selectorHelp = ref('')

const enabled = ref(false)
const tagSelector = ref('**')
const rules = ref<DraftRule[]>([])

/**
 * 服务端返回的策略 id 为 0 = 这条策略还没落库（后端返回的是骨架）。
 *
 * 项目级出现 id=0 说明"本项目没有独立策略"，清理时用全局默认；
 * 全局级出现 id=0 说明"全局默认还没配"，等于什么都不删。
 */
const notConfigured = computed(() => policy.value !== null && policy.value.id === 0)

function templateOf(name: string): RetentionTemplate | undefined {
  return templates.value.find((item) => item.template === name)
}

/** 选中某个模板时，参数清空成该模板要求的形状。 */
function defaultParams(template: RetentionTemplate): Record<string, number | undefined> {
  const params: Record<string, number | undefined> = {}
  for (const param of template.params ?? []) params[param.name] = undefined
  return params
}

function applyDraft(source: RetentionPolicy): void {
  enabled.value = source.enabled === true
  tagSelector.value = source.tag_selector || '**'
  rules.value = (source.rules ?? []).map((rule) => ({
    uid: (ruleSeq += 1),
    template: rule.template,
    params: { ...(rule.params ?? {}) },
    tag_selector: rule.tag_selector ?? '',
  }))
}

/** 生效策略的规则摘要列表；策略没启用或没有规则时给出明确说明。 */
const effectiveSummary = computed<string[]>(() =>
  policyRuleSummaries(effective.value, templates.value),
)

/** 全局默认正在删东西时用 warning：用户必须意识到自己的镜像在被删。 */
const effectiveTone = computed<'info' | 'warning' | 'success'>(() => {
  if (effectiveSource.value === 'project') return 'success'
  const p = effective.value
  if (p && p.enabled && (p.rules ?? []).length > 0) return 'warning'
  return 'info'
})

const effectiveTitle = computed(() => {
  if (effectiveSource.value === 'project') return t('retention.effectiveProject')
  const p = effective.value
  if (p && p.enabled && (p.rules ?? []).length > 0) {
    return t('retention.effectiveGlobalFallback')
  }
  return t('retention.effectiveGlobalDisabled')
})

/** 读策略 + 读模板。两者都可能失败，分别报错，不互相掩盖。 */
async function load(): Promise<void> {
  loading.value = true
  loadError.value = ''
  templateError.value = ''
  effective.value = null
  effectiveSource.value = ''
  try {
    if (props.scope === 'project') {
      if (!props.projectName) {
        throw new Error(t('retention.missingProjectName'))
      }
      const data = await getProjectRetention(props.projectName)
      policy.value = data.policy
      effective.value = data.effective
      effectiveSource.value = data.source
      applyDraft(data.policy)
    } else {
      const data = await getGlobalRetentionPolicy()
      policy.value = data.policy
      applyDraft(data.policy)
    }
  } catch (err) {
    loadError.value = errorMessage(err, t('retention.loadFailed'))
    policy.value = null
    // 清空草稿：读不到当前策略时，页面上的值是"猜"的，
    // 保存按钮同时被禁用（见模板），不能让它盖掉服务端真正的策略。
    enabled.value = false
    tagSelector.value = '**'
    rules.value = []
  } finally {
    loading.value = false
  }

  try {
    const data = await listRetentionTemplates()
    templates.value = data.templates ?? []
    selectorHelp.value = data.selector_help ?? ''
  } catch (err) {
    templateError.value = errorMessage(err, t('retention.templatesFailed'))
    templates.value = []
    selectorHelp.value = ''
  }
}

function addRule(): void {
  const first = templates.value[0]
  rules.value.push({
    uid: (ruleSeq += 1),
    template: first?.template ?? '',
    params: first ? defaultParams(first) : {},
    tag_selector: '',
  })
}

function removeRule(index: number): void {
  rules.value.splice(index, 1)
}

/** 换模板 = 换参数形状，旧参数作废。 */
function onTemplateChange(rule: DraftRule): void {
  const template = templateOf(rule.template)
  rule.params = template ? defaultParams(template) : {}
}

/**
 * 本地校验，口径对齐后端 service.ValidateRetentionPolicy：
 * 必填参数必须存在且大于 0（k / days），模板必须认识。
 *
 * 模板列表读不到时**跳过规则校验**：这时候根本不知道每条规则要哪些参数，
 * 一边猜一边拦只会让用户连"启用开关"都保存不了。
 * 这是**显式降级**——页面上有红色提示条说明模板没读到，
 * 而且后端保存时仍会校验（不合法会返回 400 并把原因带回来）。
 */
function validate(): string | null {
  if (templates.value.length === 0) return null
  for (let i = 0; i < rules.value.length; i += 1) {
    const rule = rules.value[i]
    const template = templateOf(rule.template)
    if (!template) return t('retention.ruleNoTemplate', { index: i + 1 })
    for (const param of template.params ?? []) {
      if (!param.required) continue
      const value = rule.params[param.name]
      if (value === undefined || value === null || Number.isNaN(value)) {
        return t('retention.ruleMissingParam', { index: i + 1, param: param.name })
      }
      if (value <= 0) {
        return t('retention.ruleParamNotPositive', { index: i + 1, param: param.name })
      }
    }
  }
  return null
}

function buildPayload(): RetentionPolicyPayload {
  return {
    enabled: enabled.value,
    algorithm: 'or',
    tag_selector: tagSelector.value.trim() || '**',
    rules: rules.value.map((rule) => {
      const template = templateOf(rule.template)
      const params: Record<string, number> = {}
      // 已知模板：只写它声明的参数（没填的不写）。
      // 未知模板（模板列表没读到）：把原有参数原样带回去——
      // 不然 k / days 会被悄悄抹掉，规则就变了。
      const names = template
        ? (template.params ?? []).map((param) => param.name)
        : Object.keys(rule.params)
      for (const name of names) {
        const value = rule.params[name]
        if (value === undefined || value === null || Number.isNaN(value)) continue
        params[name] = value
      }
      const payload: RetentionRule = { template: rule.template, params }
      // 空串表示"用策略级的选择器"——不写这个字段，后端才认得出是继承。
      const selector = rule.tag_selector.trim()
      if (selector !== '') payload.tag_selector = selector
      return payload
    }),
  }
}

async function save(): Promise<void> {
  const problem = validate()
  if (problem) {
    ElMessage.warning(problem)
    return
  }
  saving.value = true
  try {
    const payload = buildPayload()
    if (props.scope === 'project') {
      await putProjectRetention(props.projectName ?? '', payload)
    } else {
      await putGlobalRetentionPolicy(payload)
    }
    ElMessage.success(t('retention.saved'))
    await load()
  } catch (err) {
    ElMessage.error(errorMessage(err, t('retention.saveFailed')))
  } finally {
    saving.value = false
  }
}

/** 重置 = 删除项目级策略，清理时回落到全局默认。 */
async function reset(): Promise<void> {
  try {
    await ElMessageBox.confirm(t('retention.resetConfirm'), t('retention.resetTitle'), {
      type: 'warning',
      confirmButtonText: t('retention.reset'),
      cancelButtonText: t('common.cancel'),
    })
  } catch {
    // 用户取消，不是失败。
    return
  }
  resetting.value = true
  try {
    await deleteProjectRetention(props.projectName ?? '')
    ElMessage.success(t('retention.resetDone'))
    await load()
  } catch (err) {
    ElMessage.error(errorMessage(err, t('retention.resetFailed')))
  } finally {
    resetting.value = false
  }
}

watch(
  () => [props.scope, props.projectName] as const,
  () => {
    void load()
  },
  { immediate: true },
)
</script>

<template>
  <div v-loading="loading">
    <div v-if="loadError" class="editor-retry">
      <el-alert
        type="error"
        show-icon
        :closable="false"
        :title="loadError"
        style="margin-bottom: var(--space-2)"
      />
      <el-button size="small" @click="load">{{ t('retention.reload') }}</el-button>
    </div>

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

    <!-- 醒目提示：清理只删 tag / manifest，磁盘空间不会因此释放。 -->
    <el-alert
      type="warning"
      show-icon
      :closable="false"
      :title="t('retention.blobWarning')"
      style="margin-bottom: var(--space-4)"
    >
      <div class="form-hint" style="margin-top: var(--space-2)">
        <span v-html="t('retention.blobHint')" />
        <code>registry garbage-collect</code>{{ t('common.period') }}
      </div>
    </el-alert>

    <!-- 项目级：说清楚"当前生效的到底是哪条策略"。
         没配项目级策略时生效的是全局默认——不说清楚，
         用户会以为这个项目不受清理影响，而全局默认正在删它的镜像。 -->
    <el-alert
      v-if="scope === 'project' && effective"
      :type="effectiveTone"
      show-icon
      :closable="false"
      :title="effectiveTitle"
      style="margin-bottom: var(--space-4)"
    >
      <ul class="effective-rules">
        <li v-for="(item, index) in effectiveSummary" :key="index">{{ item }}</li>
      </ul>
    </el-alert>

    <div class="config-group">
      <div class="config-group__header">
        <span>{{ scope === 'project' ? t('retention.projectPolicy') : t('retention.globalPolicy') }}</span>
        <!-- 读取失败时不能说"已配置"——那是在替后端撒谎。 -->
        <span v-if="policy === null" class="status-pill status-err">{{ t('retention.readFailed') }}</span>
        <span v-else-if="notConfigured" class="status-pill status-muted">{{ t('retention.notConfigured') }}</span>
        <span v-else class="status-pill status-ok">{{ t('retention.configured') }}</span>
      </div>

      <div class="config-group__body">
        <div class="setting-field">
          <div class="setting-field__label">
            <span class="setting-field__key">enabled</span>
          </div>
          <div class="setting-field__control">
            <el-switch v-model="enabled" />
            <span class="form-hint" style="margin-top: 0">
              {{ enabled ? t('retention.enabled') : t('retention.stopped') }}
            </span>
          </div>
          <div class="setting-field__desc">
            {{ t('retention.enabledHint') }}
            <template v-if="scope === 'project' && notConfigured">
              {{ t('retention.notConfiguredHint') }}
            </template>
          </div>
        </div>

        <div class="setting-field">
          <div class="setting-field__label">
            <span class="setting-field__key">tag_selector</span>
            <div class="setting-field__tags">
              <span class="status-pill status-muted">glob</span>
            </div>
          </div>
          <div class="setting-field__control">
            <el-input v-model="tagSelector" placeholder="**" class="form-control--sm" />
          </div>
          <div class="setting-field__desc">
            {{ t('retention.selectorHint') }}
            <pre v-if="selectorHelp" class="selector-help">{{ selectorHelp }}</pre>
          </div>
        </div>

        <div class="retention-rules">
          <div class="section-title" style="margin-bottom: var(--space-2)">
            <span class="page-title" style="font-size: var(--fs-md)">
              {{ t('retention.rulesTitle', { count: rules.length }) }}
            </span>
            <el-button size="small" :disabled="templates.length === 0" @click="addRule">
              {{ t('retention.addRule') }}
            </el-button>
          </div>

          <div class="form-hint" style="margin-top: 0; margin-bottom: var(--space-3)">
            {{ t('retention.rulesHint') }}
            <strong>OR</strong>
            {{ t('retention.rulesHintTail') }}
          </div>

          <div v-for="(rule, index) in rules" :key="rule.uid" class="retention-rule">
            <div class="retention-rule__head">
              <span class="retention-rule__index">
                {{ t('retention.ruleIndex', { index: index + 1 }) }}
              </span>
              <el-button link type="danger" @click="removeRule(index)">
                {{ t('common.delete') }}
              </el-button>
            </div>

            <el-form :label-width="formLabelWidth(96)" class="retention-rule__body">
              <el-form-item :label="t('retention.template')">
                <el-select
                  v-model="rule.template"
                  class="form-control--sm"
                  :placeholder="t('retention.templatePlaceholder')"
                  @change="onTemplateChange(rule)"
                >
                  <el-option
                    v-for="item in templates"
                    :key="item.template"
                    :label="item.label"
                    :value="item.template"
                  />
                </el-select>
                <div v-if="templateOf(rule.template)?.descr" class="form-hint">
                  {{ templateOf(rule.template)?.descr }}
                </div>
              </el-form-item>

              <el-form-item
                v-for="param in templateOf(rule.template)?.params ?? []"
                :key="param.name"
                :label="param.name"
              >
                <el-input-number
                  v-model="rule.params[param.name]"
                  :min="1"
                  :step="1"
                  controls-position="right"
                />
                <span class="form-hint" style="margin-top: 0; margin-left: var(--space-2)">
                  {{ unitLabel(param.unit) }}
                </span>
              </el-form-item>

              <el-form-item :label="t('retention.tagSelector')">
                <el-input
                  v-model="rule.tag_selector"
                  :placeholder="t('retention.ruleSelectorPlaceholder', { selector: tagSelector || '**' })"
                  class="form-control--sm"
                />
              </el-form-item>
            </el-form>
          </div>

          <div v-if="rules.length === 0" class="form-hint">{{ t('retention.noRules') }}</div>
        </div>
      </div>
    </div>

    <div class="settings-actions">
      <span class="settings-actions__count">
        {{ scope === 'project' ? t('retention.projectOverridesGlobal') : t('retention.globalAppliesToAll') }}
      </span>
      <el-button
        v-if="scope === 'project'"
        :loading="resetting"
        :disabled="notConfigured || loadError !== ''"
        @click="reset"
      >
        {{ t('retention.resetTitle') }}
      </el-button>
      <!-- 策略没读到时禁止保存：页面上的值是空的，存下去等于清空服务端配置。 -->
      <el-button type="primary" :loading="saving" :disabled="loadError !== ''" @click="save">
        {{ t('common.save') }}
      </el-button>
    </div>
  </div>
</template>

<style scoped>
.editor-retry {
  margin-bottom: var(--space-4);
}

/* 生效策略的规则摘要 */
.effective-rules {
  margin: var(--space-2) 0 0;
  padding-left: var(--space-4);
  font-size: var(--fs-sm);
  color: var(--text-secondary);
}

.effective-rules li {
  margin-top: var(--space-1);
}

.selector-help {
  margin: var(--space-2) 0 0;
  padding: var(--space-2) var(--space-3);
  background: var(--content-bg);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  font-family: var(--font-mono);
  font-size: var(--fs-sm);
  color: var(--text-secondary);
  white-space: pre-wrap;
}

.retention-rules {
  padding: var(--space-4) 0;
  border-bottom: 1px solid var(--border-light);
}

.retention-rule {
  margin-top: var(--space-3);
  padding: var(--space-3) var(--space-4);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  background: var(--content-bg);
}

.retention-rule__head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-3);
  margin-bottom: var(--space-2);
}

.retention-rule__index {
  font-size: var(--fs-sm);
  font-weight: 600;
  color: var(--text-secondary);
}

.retention-rule__body :deep(.el-form-item) {
  margin-bottom: var(--space-2);
}

.retention-rule__body :deep(.el-form-item:last-child) {
  margin-bottom: 0;
}
</style>
