<script setup lang="ts">
/**
 * 令牌失效时间选择（系统默认 / 永久 / 指定时刻）。
 *
 * 个人令牌与项目令牌的失效语义完全一样，这个控件在两个页面里
 * 各出现两次（创建与改期对话框）——所以只留一份，
 * 免得"改了控件行为只改到一处"。
 */
import { useI18n } from 'vue-i18n'

import type { ExpiryMode } from '@/composables/useTokenExpiry'

const props = defineProps<{
  /** 当前选中的失效方式。 */
  modelValue: ExpiryMode
  /** "指定时刻"选中的时间。 */
  custom: Date | null
  /** "系统默认"那一档的文案（如 "30 天"）。 */
  defaultLabel: string
  /** 禁用今天之前的日期（选过去的时间没有意义）。 */
  disabledDate: (date: Date) => boolean
}>()

const emit = defineEmits<{
  (e: 'update:modelValue', value: ExpiryMode): void
  (e: 'update:custom', value: Date | null): void
}>()

const { t } = useI18n()
</script>

<template>
  <el-form-item :label="t('common.expiresAt')">
    <el-radio-group
      :model-value="props.modelValue"
      @update:model-value="emit('update:modelValue', $event as ExpiryMode)"
    >
      <el-radio value="default">
        {{ t('tokens.expiryDefault', { duration: props.defaultLabel }) }}
      </el-radio>
      <el-radio value="never">{{ t('tokens.expiryNever') }}</el-radio>
      <el-radio value="custom">{{ t('tokens.expiryCustom') }}</el-radio>
    </el-radio-group>
  </el-form-item>

  <el-form-item v-if="props.modelValue === 'custom'" label=" ">
    <el-date-picker
      :model-value="props.custom"
      type="datetime"
      :placeholder="t('tokens.expiryPickerPlaceholder')"
      :disabled-date="props.disabledDate"
      style="width: 100%"
      @update:model-value="emit('update:custom', $event as Date | null)"
    />
  </el-form-item>
</template>
