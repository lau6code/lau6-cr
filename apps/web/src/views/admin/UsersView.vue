<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import type { FormInstance, FormRules } from 'element-plus'
import { useI18n } from 'vue-i18n'

import { errorMessage } from '@/api/client'
import { takePendingNotice } from '@/api/passwordTransport'
import { createUser, listUsers, resetUserPassword, updateUser } from '@/api/users'
import PageHeader from '@/components/PageHeader.vue'
import type { RegistryUser } from '@/types'
import { formLabelWidth } from '@/i18n'

const { t } = useI18n()

const loading = ref(false)
const users = ref<RegistryUser[]>([])

const dialogVisible = ref(false)
const submitting = ref(false)
const formRef = ref<FormInstance>()

const form = reactive({
  username: '',
  password: '',
  display_name: '',
  is_system_admin: false,
})

const rules = computed<FormRules>(() => ({
  username: [
    { required: true, message: t('users.usernameRequired'), trigger: 'blur' },
    {
      pattern: /^[a-zA-Z0-9._-]{3,32}$/,
      message: t('users.usernamePattern'),
      trigger: 'blur',
    },
  ],
  password: [
    { required: true, message: t('users.passwordRequired'), trigger: 'blur' },
    { min: 12, message: t('users.passwordTooShort'), trigger: 'blur' },
  ],
  display_name: [{ required: true, message: t('users.displayNameRequired'), trigger: 'blur' }],
}))

const resetVisible = ref(false)
const resetSubmitting = ref(false)
const resetTarget = ref<RegistryUser | null>(null)
const resetFormRef = ref<FormInstance>()

const resetForm = reactive({
  password: '',
  confirm_password: '',
})

const resetRules = computed<FormRules>(() => ({
  password: [
    { required: true, message: t('changePassword.newRequired'), trigger: 'blur' },
    { min: 12, message: t('changePassword.newTooShort'), trigger: 'blur' },
  ],
  confirm_password: [
    { required: true, message: t('changePassword.confirmRequired'), trigger: 'blur' },
    {
      validator: (_rule, value, callback) => {
        if (value !== resetForm.password) {
          callback(new Error(t('changePassword.confirmMismatch')))
          return
        }
        callback()
      },
      trigger: 'blur',
    },
  ],
}))

/**
 * 这个账号能不能被管理员重置口令。
 *
 * LDAP 账号的口令在目录里；系统管理员账号不允许被别的管理员重置
 * （能重置 = 能登录别人的管理员账号）。见 docs/design/02-auth.md §1.2。
 */
function canResetPassword(user: RegistryUser): boolean {
  return user.source !== 'ldap' && !user.is_system_admin
}

async function load(): Promise<void> {
  loading.value = true
  try {
    const data = await listUsers()
    users.value = data.users ?? []
  } catch (err) {
    ElMessage.error(errorMessage(err, t('users.loadFailed')))
  } finally {
    loading.value = false
  }
}

function openDialog(): void {
  form.username = ''
  form.password = ''
  form.display_name = ''
  form.is_system_admin = false
  dialogVisible.value = true
}

async function submit(): Promise<void> {
  if (!formRef.value) return
  const valid = await formRef.value.validate().catch(() => false)
  if (!valid) return

  submitting.value = true
  try {
    const outcome = await createUser({
      username: form.username.trim(),
      password: form.password,
      display_name: form.display_name.trim(),
      is_system_admin: form.is_system_admin,
    })
    if (outcome.notice) ElMessage.warning(outcome.notice)
    ElMessage.success(t('users.created'))
    dialogVisible.value = false
    await load()
  } catch (err) {
    const notice = takePendingNotice()
    if (notice) ElMessage.warning(notice)
    ElMessage.error(errorMessage(err, t('users.createFailed')))
  } finally {
    submitting.value = false
  }
}

async function toggleDisabled(user: RegistryUser): Promise<void> {
  const next = !user.disabled
  try {
    await updateUser(user.id, { disabled: next })
    ElMessage.success(next ? t('users.disabled') : t('users.enabled'))
    await load()
  } catch (err) {
    ElMessage.error(errorMessage(err, t('users.statusFailed')))
  }
}

async function toggleAdmin(user: RegistryUser): Promise<void> {
  const next = !user.is_system_admin
  try {
    await updateUser(user.id, { is_system_admin: next })
    ElMessage.success(next ? t('users.grantedAdmin') : t('users.revokedAdmin'))
    await load()
  } catch (err) {
    ElMessage.error(errorMessage(err, t('users.adminFailed')))
  }
}

function openResetDialog(user: RegistryUser): void {
  resetTarget.value = user
  resetForm.password = ''
  resetForm.confirm_password = ''
  resetVisible.value = true
}

async function submitReset(): Promise<void> {
  if (!resetFormRef.value || !resetTarget.value) return
  const valid = await resetFormRef.value.validate().catch(() => false)
  if (!valid) return

  resetSubmitting.value = true
  try {
    const outcome = await resetUserPassword(
      resetTarget.value.id,
      resetTarget.value.username,
      resetForm.password,
    )
    if (outcome.notice) ElMessage.warning(outcome.notice)
    ElMessage.success(t('users.passwordReset'))
    resetVisible.value = false
  } catch (err) {
    const notice = takePendingNotice()
    if (notice) ElMessage.warning(notice)
    ElMessage.error(errorMessage(err, t('users.resetFailed')))
  } finally {
    resetSubmitting.value = false
  }
}

onMounted(() => {
  void load()
})
</script>

<template>
  <div class="page-panel">
    <page-header
      :title="t('route.adminUsers')"
      :subtitle="t('users.subtitle', { count: users.length })"
    >
      <template #actions>
        <el-button :loading="loading" @click="load">{{ t('common.refresh') }}</el-button>
        <el-button type="primary" @click="openDialog">{{ t('users.create') }}</el-button>
      </template>
    </page-header>

    <el-table
      v-loading="loading"
      :data="users"
      style="width: 100%"
      :empty-text="t('users.empty')"
    >
      <el-table-column prop="username" :label="t('login.username')" width="180">
        <template #default="{ row }">
          <span class="mono" style="color: var(--text); font-weight: 600">{{ row.username }}</span>
        </template>
      </el-table-column>

      <!-- 显示名可能很长（中文全名 + 部门括号），让它跟着容器伸缩 -->
      <el-table-column
        prop="display_name"
        :label="t('projects.columnDisplayName')"
        min-width="240"
        show-overflow-tooltip
      />

      <el-table-column :label="t('users.columnSource')" width="100">
        <template #default="{ row }">
          <span
            class="status-pill"
            :class="row.source === 'ldap' ? 'status-warn' : 'status-muted'"
          >
            {{ row.source || 'local' }}
          </span>
        </template>
      </el-table-column>

      <el-table-column :label="t('users.columnAdmin')" width="112" align="center">
        <template #default="{ row }">
          <span v-if="row.is_system_admin" class="status-pill status-err">{{ t('common.yes') }}</span>
          <span v-else class="cell-mono">{{ t('common.no') }}</span>
        </template>
      </el-table-column>

      <el-table-column :label="t('common.status')" width="100" align="center">
        <template #default="{ row }">
          <span v-if="row.disabled" class="status-pill status-muted">{{ t('users.disabled') }}</span>
          <span v-else class="status-pill status-ok">{{ t('users.normal') }}</span>
        </template>
      </el-table-column>

      <el-table-column :label="t('common.actions')" width="260" align="right">
        <template #default="{ row }">
          <el-button link type="primary" @click="toggleAdmin(row as RegistryUser)">
            {{ row.is_system_admin ? t('users.revokeAdmin') : t('users.grantAdmin') }}
          </el-button>
          <el-button
            v-if="canResetPassword(row as RegistryUser)"
            link
            type="primary"
            @click="openResetDialog(row as RegistryUser)"
          >
            {{ t('users.resetPassword') }}
          </el-button>
          <el-button
            link
            :type="row.disabled ? 'success' : 'danger'"
            @click="toggleDisabled(row as RegistryUser)"
          >
            {{ row.disabled ? t('users.enable') : t('users.disable') }}
          </el-button>
        </template>
      </el-table-column>
    </el-table>

    <el-dialog v-model="dialogVisible" :title="t('users.create')" width="480px">
      <!-- 切语言会换掉 rules 对象：别让它顺手触发一次校验，
           否则空表单上会凭空冒出必填错误。 -->
      <el-form
        ref="formRef"
        :model="form"
        :rules="rules"
        :validate-on-rule-change="false"
        :label-width="formLabelWidth(100)"
      >
        <el-form-item :label="t('login.username')" prop="username">
          <el-input v-model="form.username" :placeholder="t('users.usernamePlaceholder')" />
        </el-form-item>
        <el-form-item :label="t('projects.columnDisplayName')" prop="display_name">
          <el-input v-model="form.display_name" />
        </el-form-item>
        <el-form-item :label="t('login.password')" prop="password">
          <el-input
            v-model="form.password"
            type="password"
            show-password
            :placeholder="t('users.passwordPlaceholder')"
          />
        </el-form-item>
        <el-form-item :label="t('users.columnAdmin')">
          <el-switch v-model="form.is_system_admin" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="dialogVisible = false">{{ t('common.cancel') }}</el-button>
        <el-button type="primary" :loading="submitting" @click="submit">
          {{ t('common.create') }}
        </el-button>
      </template>
    </el-dialog>

    <el-dialog
      v-model="resetVisible"
      :title="t('users.resetPasswordTitle', { username: resetTarget?.username ?? '' })"
      width="480px"
    >
      <!-- 切语言会换掉 rules 对象：别让它顺手触发一次校验，
           否则空表单上会凭空冒出必填错误。 -->
      <el-form
        ref="resetFormRef"
        :model="resetForm"
        :rules="resetRules"
        :validate-on-rule-change="false"
        :label-width="formLabelWidth(100)"
      >
        <el-form-item :label="t('changePassword.newPassword')" prop="password">
          <el-input
            v-model="resetForm.password"
            type="password"
            show-password
            autocomplete="new-password"
            :placeholder="t('changePassword.newPasswordPlaceholder')"
          />
        </el-form-item>
        <el-form-item :label="t('changePassword.confirmPassword')" prop="confirm_password">
          <el-input
            v-model="resetForm.confirm_password"
            type="password"
            show-password
            autocomplete="new-password"
            :placeholder="t('changePassword.confirmPasswordPlaceholder')"
          />
        </el-form-item>
      </el-form>
      <div class="form-hint">{{ t('users.resetHint') }}</div>
      <template #footer>
        <el-button @click="resetVisible = false">{{ t('common.cancel') }}</el-button>
        <el-button type="primary" :loading="resetSubmitting" @click="submitReset">
          {{ t('users.resetPassword') }}
        </el-button>
      </template>
    </el-dialog>
  </div>
</template>
