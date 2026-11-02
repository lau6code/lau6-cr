<script setup lang="ts">
/**
 * 删除确认框。
 *
 * 参照 Harbor 的删除确认：标题 + 说明 + 取消/删除（危险色）。
 *
 * 删除不可逆，所以主按钮必须写清楚"删什么"，
 * 让人一眼知道自己点下去会发生什么——而不是笼统的"确定"。
 */
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'

const { t } = useI18n()

const props = defineProps<{
  modelValue: boolean
  title: string
  target: string
  detail?: string
  loading?: boolean
}>()

const emit = defineEmits<{
  'update:modelValue': [boolean]
  confirm: []
}>()

const visible = computed({
  get: () => props.modelValue,
  set: (v: boolean) => emit('update:modelValue', v),
})

function cancel(): void {
  visible.value = false
}

function confirm(): void {
  emit('confirm')
}
</script>

<template>
  <el-dialog
    v-model="visible"
    :title="title"
    width="440px"
    :close-on-click-modal="!loading"
    :close-on-press-escape="!loading"
  >
    <el-alert type="warning" show-icon :closable="false">
      <div class="confirm-target">{{ target }}</div>
      <div v-if="detail" class="form-hint" style="margin-top: var(--space-2)">
        {{ detail }}
      </div>
      <div class="form-hint" style="margin-top: var(--space-2)">
        {{ t('deleteDialog.irreversible') }}
      </div>
    </el-alert>

    <template #footer>
      <el-button :disabled="loading" @click="cancel">{{ t('deleteDialog.cancel') }}</el-button>
      <el-button type="danger" :loading="loading" @click="confirm">
        {{ t('deleteDialog.delete') }}
      </el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.confirm-target {
  font-family: var(--font-mono);
  font-size: 13px;
  color: var(--text);
  word-break: break-all;
}
</style>
