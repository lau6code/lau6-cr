<script setup lang="ts">
/**
 * 改令牌失效时间的对话框。
 *
 * 只改时间，不动令牌本体——令牌本体不变，CI 里已经配好的凭据不用重配。
 */
import { useI18n } from 'vue-i18n'

import TokenExpiryField from '@/components/TokenExpiryField.vue'
import type { ExpiryMode } from '@/composables/useTokenExpiry'
import { formLabelWidth } from '@/i18n'

const props = defineProps<{
  /** 对话框是否打开。 */
  visible: boolean
  /** 当前选中的失效方式。 */
  mode: ExpiryMode
  /** "指定时刻"选中的时间。 */
  custom: Date | null
  /** "系统默认"那一档的文案（如 "30 天"）。 */
  defaultLabel: string
  /** 禁用今天之前的日期。 */
  disabledDate: (date: Date) => boolean
  /** 保存中。 */
  saving: boolean
}>()

const emit = defineEmits<{
  (e: 'update:visible', value: boolean): void
  (e: 'update:mode', value: ExpiryMode): void
  (e: 'update:custom', value: Date | null): void
  (e: 'save'): void
}>()

const { t } = useI18n()
</script>

<template>
  <el-dialog
    :model-value="props.visible"
    :title="t('tokens.editTitle')"
    width="520px"
    @update:model-value="emit('update:visible', $event)"
  >
    <div class="form-hint" style="margin-bottom: var(--space-3)">{{ t('tokens.editHint') }}</div>

    <el-form :label-width="formLabelWidth(80)" @submit.prevent>
      <token-expiry-field
        :model-value="props.mode"
        :custom="props.custom"
        :default-label="props.defaultLabel"
        :disabled-date="props.disabledDate"
        @update:model-value="emit('update:mode', $event)"
        @update:custom="emit('update:custom', $event)"
      />
    </el-form>

    <el-alert v-if="props.mode === 'never'" type="warning" show-icon :closable="false">
      <div>{{ t('tokens.neverEditWarning') }}</div>
    </el-alert>

    <template #footer>
      <el-button @click="emit('update:visible', false)">{{ t('common.cancel') }}</el-button>
      <el-button type="primary" :loading="props.saving" @click="emit('save')">
        {{ t('common.save') }}
      </el-button>
    </template>
  </el-dialog>
</template>
