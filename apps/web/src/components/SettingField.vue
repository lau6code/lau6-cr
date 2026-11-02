<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'

import type { SettingItem } from '@/types'

const { t } = useI18n()

const props = defineProps<{
  item: SettingItem
  modelValue: unknown
  disabled?: boolean
  /** 只读态：只展示值，不渲染控件。 */
  readonly?: boolean
}>()

const emit = defineEmits<{
  (e: 'update:modelValue', value: unknown): void
}>()

/** duration 类 key：ttl / interval / retention / cooldown */
const DURATION_PATTERN = /(ttl|interval|retention|cooldown)/i

/** duration 单位 → 秒。 */
const UNIT_SECONDS: Record<string, number> = { h: 3600, m: 60, s: 1, ms: 0.001 }

/** 单位标签（展示用）。 */
const UNIT_LABELS = computed<Record<string, string>>(() => ({
  ms: t('settings.unitMs'),
  s: t('settings.unitSeconds'),
  m: t('settings.unitMinutes'),
  h: t('settings.unitHours'),
}))

interface DurationParts {
  value: number
  /** 写回后端时的单位后缀；空串表示不写单位。 */
  suffix: string
  label: string
}

/**
 * 解析 duration 值。
 *
 * 来源有两类：
 *   - 数字（core.session_ttl_seconds = 86400）→ 按秒
 *   - Go duration 字符串（stats.flush_interval = "1m0s"、"336h0m0s"）
 *
 * 复合单位单段正则匹配不了，直接判成 0 的话：只读态显示 "1m0s"、
 * 编辑框里显示 0——运维手一抖就把刷盘间隔存成了 0。
 * 这里拆开换算成秒，再取**串里出现过的最大单位**展示与写回。
 *
 * numericLabel 是给"值是数字、单位写在 key 里"的项用的
 * （cleanup.interval_minutes = 1440 是 1440 分钟，不是 1440 秒）。
 */
function parseDuration(raw: unknown, numericLabel?: string): DurationParts | null {
  if (typeof raw === 'number') {
    return { value: raw, suffix: '', label: numericLabel ?? UNIT_LABELS.value.s }
  }
  if (typeof raw !== 'string') return null
  const text = raw.trim()
  if (text === '') return null

  // 单段形式："30" / "30s" / "5m" / "1500ms"
  const simple = /^(-?\d+(?:\.\d+)?)\s*(ms|s|m|h)?$/.exec(text)
  if (simple) {
    const suffix = simple[2] ?? ''
    return {
      value: Number(simple[1]),
      suffix,
      label: suffix ? (UNIT_LABELS.value[suffix] ?? '') : UNIT_LABELS.value.s,
    }
  }

  // 复合形式："1m0s" / "336h0m0s" / "1h30m"
  const compound = /^(-?)((?:\d+(?:\.\d+)?(?:ms|h|m|s))+)$/.exec(text)
  if (!compound) return null
  const sign = compound[1] === '-' ? -1 : 1
  const parts = compound[2].match(/(\d+(?:\.\d+)?)(ms|h|m|s)/g)
  if (!parts) return null

  let total = 0
  let largest = ''
  for (const part of parts) {
    const m = /^(\d+(?:\.\d+)?)(ms|h|m|s)$/.exec(part)
    if (!m) continue
    total += Number(m[1]) * UNIT_SECONDS[m[2]]
    if (!largest || UNIT_SECONDS[m[2]] > UNIT_SECONDS[largest]) largest = m[2]
  }
  if (!largest) return null
  return {
    value: (sign * total) / UNIT_SECONDS[largest],
    suffix: largest,
    label: UNIT_LABELS.value[largest] ?? '',
  }
}

type FieldKind = 'switch' | 'number' | 'duration' | 'select' | 'multi' | 'text' | 'time'

const enumOptions = computed<string[]>(() =>
  Array.isArray(props.item.enum) ? props.item.enum.map((v) => String(v)) : [],
)
const isDuration = computed(() => DURATION_PATTERN.test(props.item.key))
/** 值是数字、单位写在 key 里的项：xxx_seconds / xxx_minutes。 */
const isNumericUnit = computed(
  () => props.item.key.endsWith('_seconds') || props.item.key.endsWith('_minutes'),
)
/** 数字值的单位标签（_minutes = 分钟，_seconds = 秒）。 */
const numericUnitLabel = computed(() =>
  props.item.key.endsWith('_minutes') ? UNIT_LABELS.value.m : undefined,
)

const kind = computed<FieldKind>(() => {
  // type=time 的项渲染成时间选择器——手打 "02:00" 容易打错，
  // 而且格式对不对只有后端知道（严格 HH:MM）。
  if (props.item.type === 'time') return 'time'
  if (enumOptions.value.length > 0) return 'select'
  // ⚠️ 用后端声明的 type 判定时长，**不能只看 key 名**：
  //    cleanup.gc.drain_timeout 的 key 里没有 ttl/interval/retention/cooldown，
  //    只按正则判会退化成文本框，用户得手打 "2m0s"。
  //    后端的 type 是权威口径（它自己写进 YAML 的就是这个值）。
  if (props.item.type === 'duration' || isDuration.value) return 'duration'
  const value = props.modelValue
  if (typeof value === 'boolean') return 'switch'
  if (typeof value === 'number') return 'number'
  if (Array.isArray(value)) return 'multi'
  return 'text'
})

const isDisabled = computed(() => props.disabled === true || props.item.editable === false)
const isSecret = computed(() => props.item.key.endsWith('.password'))
const isReadonly = computed(() => props.readonly === true)

const boolValue = computed(() => props.modelValue === true)
const numberValue = computed<number | undefined>(() =>
  typeof props.modelValue === 'number' ? props.modelValue : undefined,
)
const stringValue = computed(() => (typeof props.modelValue === 'string' ? props.modelValue : ''))
const multiValue = computed<string[]>(() =>
  Array.isArray(props.modelValue) ? props.modelValue.map((v) => String(v)) : [],
)

const duration = computed<DurationParts>(
  () => parseDuration(props.modelValue, numericUnitLabel.value) ?? { value: 0, suffix: '', label: '' },
)

const min = computed<number | undefined>(() => props.item.constraints?.min)
const max = computed<number | undefined>(() => props.item.constraints?.max)

/** duration 值的展示文本（带单位）；不是合法 duration 时返 null。 */
const durationText = computed<string | null>(() => {
  const parts = parseDuration(props.modelValue, numericUnitLabel.value)
  if (!parts) return null
  return parts.label ? `${parts.value} ${parts.label}` : String(parts.value)
})

/**
 * 后端对敏感项只回 `{"set":true}`，不回明文（见 settings.go 的 Secret 分支）。
 *
 * 也就是说密钥**根本到不了前端**——这里只是把这个约定翻译成人话。
 */
const isSecretSet = computed(() => {
  const raw = props.modelValue
  if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) return false
  return (raw as Record<string, unknown>).set === true
})

/**
 * 只读态的展示文本。
 *
 * 密码类一律打码——万一哪天密钥以明文形式漏过来，也不能原样印到页面上。
 */
const displayText = computed(() => {
  const raw = props.modelValue

  if (isSecretSet.value) return t('settings.secretSet')
  if (isSecret.value) {
    return typeof raw === 'string' && raw.length > 0 ? '••••••' : '—'
  }
  if (raw === null || raw === undefined || raw === '') return '—'
  if (typeof raw === 'boolean') return raw ? t('settings.on') : t('settings.off')
  if (Array.isArray(raw)) {
    const list = raw.map((v) => String(v)).filter((v) => v.length > 0)
    return list.length > 0 ? list.join(t('common.listSeparator')) : '—'
  }
  if (isDuration.value) {
    const text = durationText.value
    if (text !== null) return text
  }
  return String(raw)
})

const sourceLabel = computed(() => {
  switch (props.item.source) {
    case 'env':
      return t('settings.sourceEnv')
    case 'db':
      return t('settings.sourceDb')
    case 'yaml':
      return t('settings.sourceYaml')
    default:
      return t('settings.sourceDefault')
  }
})

const sourceTone = computed(() => {
  switch (props.item.source) {
    case 'env':
      return 'status-err'
    case 'db':
      return 'status-ok'
    case 'yaml':
      return 'status-warn'
    default:
      return 'status-muted'
  }
})

function onSwitch(value: boolean | number | string): void {
  emit('update:modelValue', value === true)
}

function onNumber(value: number | undefined | null): void {
  emit('update:modelValue', value === undefined || value === null ? 0 : value)
}

function onDuration(value: number | undefined | null): void {
  const next = value === undefined || value === null ? 0 : value
  if (typeof props.modelValue === 'number' || isNumericUnit.value) {
    emit('update:modelValue', next)
    return
  }
  emit('update:modelValue', duration.value.suffix ? `${next}${duration.value.suffix}` : String(next))
}

function onSelect(value: unknown): void {
  emit('update:modelValue', typeof value === 'string' ? value : '')
}

function onMulti(value: unknown): void {
  emit('update:modelValue', Array.isArray(value) ? value.map((v) => String(v)) : [])
}

function onText(value: string | number | null | undefined): void {
  emit('update:modelValue', value === null || value === undefined ? '' : String(value))
}

/** time 类项的值是 "HH:MM" 字符串。 */
function onTime(value: unknown): void {
  emit('update:modelValue', typeof value === 'string' ? value : '')
}
</script>

<template>
  <div class="setting-field">
    <div class="setting-field__label">
      <span class="setting-field__key">{{ item.key }}</span>
      <div class="setting-field__tags">
        <span v-if="item.tier" class="status-pill status-primary">{{ item.tier }}</span>
        <span class="status-pill" :class="sourceTone">{{ sourceLabel }}</span>
      </div>
    </div>

    <div class="setting-field__control">
      <!-- 只读态：直接显示值，不摆控件 -->
      <span v-if="isReadonly" class="setting-field__value">{{ displayText }}</span>

      <el-switch
        v-else-if="kind === 'switch'"
        :model-value="boolValue"
        :disabled="isDisabled"
        @update:model-value="onSwitch"
      />

      <el-input-number
        v-else-if="kind === 'number'"
        :model-value="numberValue"
        :min="min"
        :max="max"
        :step="1"
        :disabled="isDisabled"
        @update:model-value="onNumber"
      />

      <template v-else-if="kind === 'duration'">
        <el-input-number
          :model-value="duration.value"
          :min="min"
          :max="max"
          :step="1"
          :disabled="isDisabled"
          @update:model-value="onDuration"
        />
        <span class="setting-field__unit">{{ duration.label }}</span>
      </template>

      <el-select
        v-else-if="kind === 'select'"
        :model-value="stringValue"
        :disabled="isDisabled"
        @update:model-value="onSelect"
      >
        <el-option v-for="option in enumOptions" :key="option" :label="option" :value="option" />
      </el-select>

      <el-select
        v-else-if="kind === 'multi'"
        :model-value="multiValue"
        multiple
        filterable
        allow-create
        default-first-option
        :reserve-keyword="false"
        :disabled="isDisabled"
        :placeholder="t('settings.multiPlaceholder')"
        @update:model-value="onMulti"
      >
        <el-option v-for="option in multiValue" :key="option" :label="option" :value="option" />
      </el-select>

      <el-time-picker
        v-else-if="kind === 'time'"
        :model-value="stringValue"
        format="HH:mm"
        value-format="HH:mm"
        placeholder="HH:MM"
        :clearable="false"
        :disabled="isDisabled"
        @update:model-value="onTime"
      />

      <el-input
        v-else
        :model-value="stringValue"
        :type="isSecret ? 'password' : 'text'"
        :show-password="isSecret"
        :disabled="isDisabled"
        @update:model-value="onText"
      />
    </div>

    <div v-if="item.description" class="setting-field__desc">{{ item.description }}</div>
    <div v-if="!item.editable && item.hint" class="setting-field__hint">{{ item.hint }}</div>
  </div>
</template>
