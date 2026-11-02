<script setup lang="ts">
/**
 * "生成令牌"对话框。
 *
 * 个人令牌与项目令牌共用：名字、失效方式、生成后展示令牌本体这几段完全一样，
 * 差别只有项目令牌多一个"角色"表单项（用插槽插在名字之后）与各自的文案键。
 */
import { useI18n } from 'vue-i18n'

import TokenCreatedPanel from '@/components/TokenCreatedPanel.vue'
import TokenExpiryField from '@/components/TokenExpiryField.vue'
import type { ExpiryMode } from '@/composables/useTokenExpiry'
import { formLabelWidth } from '@/i18n'

const visible = defineModel<boolean>({ required: true })
const tokenName = defineModel<string>('tokenName', { required: true })
const mode = defineModel<ExpiryMode>('mode', { required: true })
const custom = defineModel<Date | null>('custom', { required: true })

defineProps<{
  /** 对话框标题的语言包键。 */
  titleKey: string
  /** 生成按钮文案的语言包键。 */
  generateKey: string
  /** "系统默认"那一档的文案（如 "30 天"）。 */
  defaultLabel: string
  /** 禁用今天之前的日期。 */
  disabledDate: (date: Date) => boolean
  /** 生成中。 */
  creating: boolean
  /** 刚生成出来的令牌本体；null = 还没生成。 */
  createdToken: string | null
}>()

const emit = defineEmits<{ submit: [] }>()

const { t } = useI18n()
</script>

<template>
  <el-dialog v-model="visible" :title="t(titleKey)" width="560px">
    <template v-if="!createdToken">
      <el-form :label-width="formLabelWidth(80)" @submit.prevent>
        <el-form-item :label="t('common.name')" required>
          <el-input
            v-model="tokenName"
            :placeholder="t('tokens.namePlaceholder')"
            maxlength="64"
            show-word-limit
          />
        </el-form-item>

        <slot name="extra-fields" />

        <token-expiry-field
          v-model="mode"
          v-model:custom="custom"
          :default-label="defaultLabel"
          :disabled-date="disabledDate"
        />
      </el-form>

      <el-alert v-if="mode === 'never'" type="warning" show-icon :closable="false">
        <div>{{ t('tokens.neverWarning') }}</div>
      </el-alert>

      <div class="form-hint">{{ t('tokens.oneTimeHint') }}</div>
    </template>

    <template v-else>
      <token-created-panel :token="createdToken" />
    </template>

    <template #footer>
      <el-button @click="visible = false">{{ t('common.close') }}</el-button>
      <el-button v-if="!createdToken" type="primary" :loading="creating" @click="emit('submit')">
        {{ t(generateKey) }}
      </el-button>
    </template>
  </el-dialog>
</template>
