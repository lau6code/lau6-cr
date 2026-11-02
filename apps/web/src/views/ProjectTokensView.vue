<script setup lang="ts">
/**
 * 项目访问令牌（project token）。
 *
 * 项目令牌是**项目身份**的长期凭据：项目管理员生成后交给 CI / 流水线，
 * 让自动化以项目身份推拉镜像，不必占用某个人的个人账号。
 *
 * 与个人令牌的根本区别：个人令牌不带 scope，权限按用户的当前角色实时算；
 * 项目令牌带一个**创建时选定、此后不变**的角色。
 *
 * ⚠️ 令牌本体**只显示一次**（后端库里只有散列）。
 *    创建成功后必须让用户当场复制走，关掉对话框就再也取不回来了。
 */
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { useI18n } from 'vue-i18n'

import { errorMessage } from '@/api/client'
import {
  createProjectToken,
  listProjectTokens,
  revokeProjectToken,
  updateProjectTokenExpiry,
} from '@/api/projects'
import PageHeader from '@/components/PageHeader.vue'
import TokenCreateDialog from '@/components/TokenCreateDialog.vue'
import TokenEditDialog from '@/components/TokenEditDialog.vue'
import TokenTable from '@/components/TokenTable.vue'
import { useTokenExpiry } from '@/composables/useTokenExpiry'
import type {
  ProjectToken,
  ProjectTokenCreated,
  ProjectTokenRole,
  TokenRow,
} from '@/types'
import { ROLE_TONE } from '@/utils/roles'

const { t } = useI18n()

const route = useRoute()
const router = useRouter()

/** 项目名。 */
const projectName = computed(() => String(route.params.name ?? '').trim())

const loading = ref(false)
const tokens = ref<ProjectToken[]>([])
/** 系统默认有效期（秒），来自后端；改了设置页会跟着变。 */
const defaultTtl = ref(30 * 24 * 3600)
/** 加载失败的原文；空串 = 没出错。 */
const loadError = ref('')

/** 创建对话框。 */
const createVisible = ref(false)
const creating = ref(false)
const tokenName = ref('')
const createRole = ref<ProjectTokenRole>('developer')

/** 刚创建出来的令牌（含本体）——只在这个对话框里存在。 */
const created = ref<ProjectTokenCreated | null>(null)

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

/** 每个角色实际能做的镜像动作（与后端 domain.Role.Actions 一致）。 */
const ROLE_ACTIONS: Record<ProjectTokenRole, string> = {
  admin: 'pull / push / delete',
  developer: 'pull / push / delete',
  reader: 'pull',
}

async function load(): Promise<void> {
  if (!projectName.value) {
    loadError.value = t('projectTokens.missingProject')
    return
  }
  loading.value = true
  loadError.value = ''
  try {
    const data = await listProjectTokens(projectName.value)
    tokens.value = data.tokens ?? []
    if (typeof data.default_ttl_seconds === 'number' && data.default_ttl_seconds > 0) {
      defaultTtl.value = data.default_ttl_seconds
    }
  } catch (err) {
    // 失败必须看得见：把后端的原文挂在页面上，
    // 不靠一个转瞬即逝的 toast，也不拿空列表冒充"还没有令牌"。
    loadError.value = errorMessage(err, t('projectTokens.loadFailed'))
    ElMessage.error(loadError.value)
  } finally {
    loading.value = false
  }
}

function backToProject(): void {
  void router.push({ name: 'project-detail', params: { name: projectName.value } })
}

function openCreate(): void {
  tokenName.value = ''
  createRole.value = 'developer'
  createMode.value = 'default'
  createCustom.value = defaultExpiryDate()
  created.value = null
  createVisible.value = true
}

async function submit(): Promise<void> {
  const name = tokenName.value.trim()
  if (!name) {
    ElMessage.warning(t('projectTokens.nameRequired'))
    return
  }
  const expiry = checkedExpiry(createMode.value, createCustom.value)
  if (!expiry) return
  creating.value = true
  try {
    created.value = await createProjectToken(projectName.value, {
      name,
      role: createRole.value,
      ...expiry,
    })
    ElMessage.success(t('projectTokens.generated'))
    await load()
  } catch (err) {
    ElMessage.error(errorMessage(err, t('projectTokens.createFailed')))
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
    await updateProjectTokenExpiry(projectName.value, target.id, expiry)
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
    await ElMessageBox.confirm(
      t('projectTokens.revokeMessage', { name: token.name }),
      t('projectTokens.revokeTitle'),
      {
        type: 'warning',
        confirmButtonText: t('projectTokens.actionRevoke'),
        cancelButtonText: t('common.cancel'),
      },
    )
  } catch {
    // 用户取消。
    return
  }
  try {
    await revokeProjectToken(projectName.value, token.id)
    ElMessage.success(t('projectTokens.revoked'))
    await load()
  } catch (err) {
    ElMessage.error(errorMessage(err, t('projectTokens.revokeFailed')))
  }
}

onMounted(() => {
  void load()
})
</script>

<template>
  <div class="page-panel">
    <page-header :title="t('projectTokens.title')">
      <template #subtitle>
        <span class="page-meta">
          <span class="mono">{{ projectName }}</span>
          <span class="page-meta__sep">·</span>
          <span>{{ t('projectTokens.subtitle') }}</span>
        </span>
      </template>

      <template #actions>
        <el-button @click="backToProject">{{ t('projectTokens.backToProject') }}</el-button>
        <el-button :loading="loading" @click="load">{{ t('common.refresh') }}</el-button>
        <el-button type="primary" :disabled="!!loadError" @click="openCreate">
          {{ t('projectTokens.generate') }}
        </el-button>
      </template>
    </page-header>

    <!-- 加载失败：把后端的原文留在页面上，不让用户对着空列表猜。 -->
    <el-alert
      v-if="loadError"
      type="error"
      :title="t('projectTokens.readFailed')"
      show-icon
      :closable="false"
      style="margin-bottom: var(--space-4)"
    >
      <div class="form-hint" style="margin-top: var(--space-2)">{{ loadError }}</div>
    </el-alert>

    <el-alert type="info" show-icon :closable="false" style="margin-bottom: var(--space-4)">
      <!-- 文案里带静态 HTML 标记（<span>/<strong>），来自语言包常量，不含用户输入。 -->
      <div class="form-hint" v-html="t('projectTokens.usageCommand')" />
      <div class="form-hint" v-html="t('projectTokens.usageScope')" />
      <div class="form-hint" v-html="t('projectTokens.usageRoleImmutable')" />
      <div class="form-hint" v-html="t('projectTokens.usageRoleAdminDeveloper')" />
    </el-alert>

    <token-table
      :tokens="tokens"
      :loading="loading"
      :empty-text="loadError ? t('projectTokens.readFailed') : t('projectTokens.empty')"
      :revoke-label="t('projectTokens.actionRevoke')"
      @edit="openEdit"
      @revoke="revoke"
    >
      <template #extra-columns>
        <el-table-column :label="t('common.role')" width="150">
          <template #default="{ row }">
            <span class="status-pill" :class="ROLE_TONE[(row as ProjectToken).role] || 'status-muted'">
              {{ t(`roles.${(row as ProjectToken).role}`) }}
            </span>
          </template>
        </el-table-column>
      </template>
    </token-table>

    <!-- 生成对话框：先填名字、角色与失效方式，生成后展示令牌本体 -->
    <token-create-dialog
      v-model="createVisible"
      v-model:token-name="tokenName"
      v-model:mode="createMode"
      v-model:custom="createCustom"
      title-key="projectTokens.createTitle"
      generate-key="projectTokens.generate"
      :default-label="formatDuration(defaultTtl)"
      :disabled-date="disabledPastDate"
      :creating="creating"
      :created-token="created?.token ?? null"
      @submit="submit"
    >
      <template #extra-fields>
        <el-form-item :label="t('common.role')" required>
          <el-select v-model="createRole" class="form-control">
            <el-option
              v-for="role in (['admin', 'developer', 'reader'] as ProjectTokenRole[])"
              :key="role"
              :label="`${t(`roles.${role}`)}（${role}）`"
              :value="role"
            />
          </el-select>
          <div class="form-hint">
            {{ t('projectTokens.currentActions', { actions: ROLE_ACTIONS[createRole] }) }}
          </div>
        </el-form-item>
      </template>
    </token-create-dialog>

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
