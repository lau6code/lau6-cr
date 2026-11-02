<script setup lang="ts">
/**
 * 刚创建出来的令牌本体。
 *
 * ⚠️ 令牌**只显示一次**（后端库里只有散列）——所以这块必须
 * 明确告诉用户"关掉就再也拿不到了"，并且给一个复制按钮。
 */
import { useI18n } from 'vue-i18n'

import { copyAndNotify } from '@/utils/clipboard'

const props = defineProps<{
  /** 令牌本体。 */
  token: string
}>()

const { t } = useI18n()

async function copyToken(): Promise<void> {
  await copyAndNotify(props.token)
}
</script>

<template>
  <el-alert type="warning" show-icon :closable="false">
    <div>{{ t('tokens.createdWarning') }}</div>
  </el-alert>
  <div class="token-box">
    <span class="token-box__value">{{ props.token }}</span>
  </div>
  <el-button type="primary" style="width: 100%" @click="copyToken">
    {{ t('tokens.copyToken') }}
  </el-button>
</template>

<style scoped>
.token-box {
  margin: var(--space-3) 0;
  padding: var(--space-3);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  background: var(--surface-raised);
  word-break: break-all;
}

.token-box__value {
  font-family: var(--font-mono);
  font-size: 13px;
  color: var(--text);
}
</style>
