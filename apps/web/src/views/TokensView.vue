<script setup lang="ts">
/**
 * 个人访问令牌（PAT）。
 *
 * 用户在这里生成**自己管理**的长期凭据，用来在 CI / 脚本里推拉
 * 需要权限的镜像——不必把账户口令交出去。
 *
 * ⚠️ 令牌本体**只显示一次**（后端库里只有散列）。
 *    所以创建成功后必须让用户当场复制走，
 *    关掉对话框就再也取不回来了。
 *
 * 失效时间有三种：永久有效 / 指定时刻 / 按系统默认。
 * 默认永远是"系统默认"——永久必须由用户显式选，
 * 否则用户以为拿到的是 30 天，实际拿到的是永不过期的凭据。
 */
import { onMounted, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { useI18n } from 'vue-i18n'

import { errorMessage } from '@/api/client'
import {
  createPersonalToken,
  listPersonalTokens,
  revokePersonalToken,
  updatePersonalTokenExpiry,
} from '@/api/tokens'
import PageHeader from '@/components/PageHeader.vue'
import TokenCreateDialog from '@/components/TokenCreateDialog.vue'
import TokenEditDialog from '@/components/TokenEditDialog.vue'
import TokenTable from '@/components/TokenTable.vue'
import { useTokenExpiry } from '@/composables/useTokenExpiry'
import type { PersonalToken, PersonalTokenCreated, TokenRow } from '@/types'

const { t } = useI18n()

const loading = ref(false)
const tokens = ref<PersonalToken[]>([])
/** 系统默认有效期（秒），来自后端；改了设置页会跟着变。 */
const defaultTtl = ref(30 * 24 * 3600)

/** 创建对话框。 */
const createVisible = ref(false)
const creating = ref(false)
const tokenName = ref('')

/** 刚创建出来的令牌（含本体）——只在这个对话框里存在。 */
const created = ref<PersonalTokenCreated | null>(null)

/** 改失效时间对话框。 */
const editing = ref(false)

const {
  createMode,
  createCustom,
  editVisible,
  editTarget,
  editMode,
  editCustom,
  defaultExpiryDate,
  formatDuration,
  checkedExpiry,
  openEdit,
  disabledPastDate,
} = useTokenExpiry({
  defaultTtl,
  strings: {
    durationDays: 'tokens.durationDays',
    durationHours: 'tokens.durationHours',
    durationSeconds: 'tokens.durationSeconds',
    invalidExpiry: 'tokens.invalidExpiry',
  },
})

async function load(): Promise<void> {
  loading.value = true
  try {
    const data = await listPersonalTokens()
    tokens.value = data.tokens ?? []
    if (typeof data.default_ttl_seconds === 'number' && data.default_ttl_seconds > 0) {
      defaultTtl.value = data.default_ttl_seconds
    }
  } catch (err) {
    ElMessage.error(errorMessage(err, t('tokens.loadFailed')))
  } finally {
    loading.value = false
  }
}

function openCreate(): void {
  tokenName.value = ''
  createMode.value = 'default'
  createCustom.value = defaultExpiryDate()
  created.value = null
  createVisible.value = true
}

async function submit(): Promise<void> {
  const name = tokenName.value.trim()
  if (!name) {
    ElMessage.warning(t('tokens.nameRequired'))
    return
  }
  const expiry = checkedExpiry(createMode.value, createCustom.value)
  if (!expiry) return
  creating.value = true
  try {
    created.value = await createPersonalToken(name, expiry)
    ElMessage.success(t('tokens.generated'))
    await load()
  } catch (err) {
    ElMessage.error(errorMessage(err, t('tokens.createFailed')))
  } finally {
    creating.value = false
  }
}

async function submitEdit(): Promise<void> {
  const target = editTarget.value
  if (!target) return
  const expiry = checkedExpiry(editMode.value, editCustom.value)
  if (!expiry) return
  editing.value = true
  try {
    await updatePersonalTokenExpiry(target.id, expiry)
    ElMessage.success(t('tokens.expiryUpdated'))
    editVisible.value = false
    await load()
  } catch (err) {
    ElMessage.error(errorMessage(err, t('tokens.expiryFailed')))
  } finally {
    editing.value = false
  }
}

async function revoke(token: TokenRow): Promise<void> {
  try {
    await ElMessageBox.confirm(t('tokens.revokeMessage', { name: token.name }), t('tokens.revokeTitle'), {
      type: 'warning',
      confirmButtonText: t('tokens.actionRevoke'),
      cancelButtonText: t('common.cancel'),
    })
  } catch {
    // 用户取消。
    return
  }
  try {
    await revokePersonalToken(token.id)
    ElMessage.success(t('tokens.revoked'))
    await load()
  } catch (err) {
    ElMessage.error(errorMessage(err, t('tokens.revokeFailed')))
  }
}

onMounted(() => {
  void load()
})
</script>

<template>
  <div class="page-panel">
    <page-header :title="t('tokens.title')" :subtitle="t('tokens.subtitle')">
      <template #actions>
        <el-button :loading="loading" @click="load">{{ t('common.refresh') }}</el-button>
        <el-button type="primary" @click="openCreate">{{ t('tokens.generate') }}</el-button>
      </template>
    </page-header>

    <el-alert type="info" show-icon :closable="false" style="margin-bottom: var(--space-4)">
      <!-- 文案里带静态 HTML 标记（<span>/<strong>），来自语言包常量，不含用户输入。 -->
      <div class="form-hint" v-html="t('tokens.usageCommand')" />
      <div class="form-hint" v-html="t('tokens.usageRole')" />
    </el-alert>

    <token-table
      :tokens="tokens"
      :loading="loading"
      :empty-text="t('tokens.empty')"
      :revoke-label="t('tokens.actionRevoke')"
      @edit="openEdit"
      @revoke="revoke"
    />

    <!-- 生成对话框：先填名字与失效方式，生成后展示令牌本体 -->
    <token-create-dialog
      v-model="createVisible"
      v-model:token-name="tokenName"
      v-model:mode="createMode"
      v-model:custom="createCustom"
      title-key="tokens.createTitle"
      generate-key="tokens.generate"
      :default-label="formatDuration(defaultTtl)"
      :disabled-date="disabledPastDate"
      :creating="creating"
      :created-token="created?.token ?? null"
      @submit="submit"
    />

    <!-- 改失效时间对话框：只改时间，不动令牌本体 -->
    <token-edit-dialog
      v-model:visible="editVisible"
      v-model:mode="editMode"
      v-model:custom="editCustom"
      :default-label="formatDuration(defaultTtl)"
      :disabled-date="disabledPastDate"
      :saving="editing"
      @save="submitEdit"
    />
  </div>
</template>
