<script setup lang="ts">
/**
 * 令牌列表表格。
 *
 * 个人令牌与项目令牌的公共列完全一样；项目令牌多一列"角色"，
 * 由调用方通过 extra-columns 插槽插进去（插槽在 el-table 的默认插槽里，
 * 表格列才能正常注册）。
 */
import { useI18n } from 'vue-i18n'

import { tokenIsExpired } from '@/composables/useTokenExpiry'
import type { TokenRow } from '@/types'
import { formatUnixSeconds } from '@/utils/format'

const props = defineProps<{
  /** 令牌列表。 */
  tokens: TokenRow[]
  /** 加载中。 */
  loading: boolean
  /** 空列表时的提示。 */
  emptyText: string
  /** "吊销"按钮的文案。 */
  revokeLabel: string
}>()

const emit = defineEmits<{
  (e: 'edit', token: TokenRow): void
  (e: 'revoke', token: TokenRow): void
}>()

const { t } = useI18n()
</script>

<template>
  <el-table
    v-loading="props.loading"
    :data="props.tokens"
    style="width: 100%"
    :empty-text="props.emptyText"
  >
    <el-table-column prop="name" :label="t('common.name')" min-width="180" />

    <!-- 额外列：项目令牌在这里插"角色"列 -->
    <slot name="extra-columns" />

    <el-table-column :label="t('common.prefix')" width="180">
      <template #default="{ row }">
        <span class="cell-mono">{{ (row as TokenRow).token_prefix }}…</span>
      </template>
    </el-table-column>

    <el-table-column :label="t('common.createdAt')" width="170">
      <template #default="{ row }">
        <span class="cell-mono">{{ formatUnixSeconds((row as TokenRow).created_at) }}</span>
      </template>
    </el-table-column>

    <el-table-column :label="t('common.expiresAt')" width="170">
      <template #default="{ row }">
        <span
          v-if="(row as TokenRow).expires_at === null"
          class="status-pill status-warn"
        >{{ t('tokens.never') }}</span>
        <span v-else class="cell-mono">
          {{ formatUnixSeconds((row as TokenRow).expires_at) }}
        </span>
      </template>
    </el-table-column>

    <el-table-column :label="t('common.lastUsedAt')" width="170">
      <template #default="{ row }">
        <span class="cell-mono">
          {{ formatUnixSeconds((row as TokenRow).last_used_at) }}
        </span>
      </template>
    </el-table-column>

    <el-table-column :label="t('common.status')" width="110" align="center">
      <template #default="{ row }">
        <span
          v-if="tokenIsExpired(row as TokenRow)"
          class="status-pill status-muted"
        >{{ t('tokens.statusExpired') }}</span>
        <span v-else class="status-pill status-ok">{{ t('tokens.statusValid') }}</span>
      </template>
    </el-table-column>

    <el-table-column :label="t('common.actions')" width="170" align="right">
      <template #default="{ row }">
        <el-button link type="primary" @click="emit('edit', row as TokenRow)">
          {{ t('tokens.actionExpiry') }}
        </el-button>
        <el-button link type="danger" @click="emit('revoke', row as TokenRow)">
          {{ props.revokeLabel }}
        </el-button>
      </template>
    </el-table-column>
  </el-table>
</template>
