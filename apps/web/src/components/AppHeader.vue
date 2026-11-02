<script setup lang="ts">
import { computed, reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import type { FormInstance, FormRules } from 'element-plus'
import { useI18n } from 'vue-i18n'

import { errorMessage } from '@/api/client'
import { changePassword } from '@/api/auth'
import { takePendingNotice } from '@/api/passwordTransport'
import { formLabelWidth } from '@/i18n'
import LocaleSwitcher from '@/components/LocaleSwitcher.vue'
import ThemeSwitcher from '@/components/ThemeSwitcher.vue'
import { useAuthStore } from '@/stores/auth'

const route = useRoute()
const router = useRouter()
const auth = useAuthStore()
const { t } = useI18n()

interface Crumb {
  label: string
  /** 为空表示当前页，不可点 */
  to?: string
  /** 用等宽呈现——项目名、仓库名都是机器标识 */
  mono?: boolean
}

/**
 * 面包屑。
 *
 * 侧栏已经表达了"我在哪个功能区"，面包屑补的是**当前对象**：
 * 在哪个项目里、哪个设置分组。
 */
const crumbs = computed<Crumb[]>(() => {
  const path = route.path

  if (path.startsWith('/projects/')) {
    const name = decodeURIComponent(path.slice('/projects/'.length))
    return [
      { label: t('nav.projects'), to: '/projects' },
      { label: name, mono: true },
    ]
  }
  if (path === '/projects') return [{ label: t('nav.projects'), to: '/projects' }]
  if (path.startsWith('/project-retention/')) {
    const name = decodeURIComponent(path.slice('/project-retention/'.length))
    return [
      { label: t('nav.projects'), to: '/projects' },
      { label: name, mono: true, to: `/projects/${encodeURIComponent(name)}` },
      { label: t('route.projectRetention') },
    ]
  }
  if (path.startsWith('/project-tokens/')) {
    const name = decodeURIComponent(path.slice('/project-tokens/'.length))
    return [
      { label: t('nav.projects'), to: '/projects' },
      { label: name, mono: true, to: `/projects/${encodeURIComponent(name)}` },
      { label: t('route.projectTokens') },
    ]
  }
  if (path === '/project-members') return [{ label: t('nav.members'), to: '/project-members' }]
  if (path === '/admin/users') return [{ label: t('nav.users'), to: '/admin/users' }]
  if (path === '/admin/cleanup') return [{ label: t('nav.cleanup'), to: '/admin/cleanup' }]
  if (path === '/admin/settings') return [{ label: t('nav.settings'), to: '/admin/settings' }]
  if (path === '/tokens') return [{ label: t('route.tokens') }]
  if (path === '/') {
    // 镜像目录按项目过滤时，面包屑补出当前项目——"在哪个项目里"。
    const project = route.query.project
    if (typeof project === 'string' && project) {
      return [
        { label: t('nav.catalog'), to: '/' },
        { label: project, mono: true },
      ]
    }
  }
  return [{ label: t('nav.catalog'), to: '/' }]
})

// ── 修改密码 ───────────────────────────────────────────────

const passwordVisible = ref(false)
const passwordSubmitting = ref(false)
const passwordFormRef = ref<FormInstance>()

const passwordForm = reactive({
  old_password: '',
  new_password: '',
  confirm_password: '',
})

const passwordRules = computed<FormRules>(() => ({
  old_password: [{ required: true, message: t('changePassword.oldRequired'), trigger: 'blur' }],
  new_password: [
    { required: true, message: t('changePassword.newRequired'), trigger: 'blur' },
    { min: 12, message: t('changePassword.newTooShort'), trigger: 'blur' },
  ],
  confirm_password: [
    { required: true, message: t('changePassword.confirmRequired'), trigger: 'blur' },
    {
      validator: (_rule, value, callback) => {
        if (value !== passwordForm.new_password) {
          callback(new Error(t('changePassword.confirmMismatch')))
          return
        }
        callback()
      },
      trigger: 'blur',
    },
  ],
}))

function openPasswordDialog(): void {
  passwordForm.old_password = ''
  passwordForm.new_password = ''
  passwordForm.confirm_password = ''
  passwordVisible.value = true
}

async function submitPassword(): Promise<void> {
  if (!passwordFormRef.value) return
  const valid = await passwordFormRef.value.validate().catch(() => false)
  if (!valid) return

  passwordSubmitting.value = true
  try {
    const outcome = await changePassword(
      {
        old_password: passwordForm.old_password,
        new_password: passwordForm.new_password,
      },
      auth.user?.username ?? '',
    )
    if (outcome.notice) ElMessage.warning(outcome.notice)
    ElMessage.success(t('changePassword.success'))
    passwordVisible.value = false
  } catch (err) {
    const notice = takePendingNotice()
    if (notice) ElMessage.warning(notice)
    ElMessage.error(errorMessage(err, t('changePassword.failed')))
  } finally {
    passwordSubmitting.value = false
  }
}

// ── 用户菜单 ───────────────────────────────────────────────

async function handleCommand(command: string | number | object): Promise<void> {
  const cmd = String(command)
  if (cmd === 'personal-tokens') {
    // 个人令牌是账号自己的凭据，入口放在用户名下拉框里。
    await router.push('/tokens')
    return
  }
  if (cmd === 'change-password') {
    // 菜单里已经隐藏了，这里再挡一道——避免 LDAP 账号绕过菜单直接触发
    if (!auth.canChangePassword) return
    openPasswordDialog()
    return
  }
  if (cmd !== 'logout') return
  try {
    await auth.logout()
    ElMessage.success(t('account.loggedOut'))
  } catch (err) {
    // ⚠️ 服务端销毁会话失败会回 5xx（那一把会话**仍然有效**）。
    //    但 Cookie 已经被服务端清掉了——浏览器这一侧确实登出了，
    //    所以照样跳登录页，只是必须把"服务端会话可能仍然有效"说出来。
    ElMessage.error(errorMessage(err, t('account.logoutIncomplete')))
  }
  await router.push('/login')
}
</script>

<template>
  <header class="topbar">
    <div class="topbar-left">
      <button type="button" class="brand" @click="router.push('/')">lau6-cr</button>

      <nav class="breadcrumb" :aria-label="t('nav.breadcrumb')">
        <template v-for="(crumb, index) in crumbs" :key="crumb.label">
          <span v-if="index > 0" class="breadcrumb-sep" aria-hidden="true">/</span>
          <RouterLink
            v-if="crumb.to && index < crumbs.length - 1"
            :to="crumb.to"
            :class="crumb.mono ? 'breadcrumb-mono' : undefined"
          >
            {{ crumb.label }}
          </RouterLink>
          <span
            v-else
            :class="index === crumbs.length - 1 ? 'breadcrumb-current' : 'breadcrumb-muted'"
            :aria-current="index === crumbs.length - 1 ? 'page' : undefined"
          >
            <span :class="crumb.mono ? 'breadcrumb-mono' : undefined">{{ crumb.label }}</span>
          </span>
        </template>
      </nav>
    </div>

    <div class="topbar-user">
      <template v-if="auth.isAuthenticated">
        <!-- 语言与主题是**界面偏好**，不属于账号——
             所以它们是顶栏上独立的入口，不塞进用户菜单里。 -->
        <LocaleSwitcher />
        <ThemeSwitcher />

        <el-dropdown trigger="click" @command="handleCommand">
          <button type="button" class="user-menu-trigger">
            <span class="topbar-username">{{ auth.displayName }}</span>
            <span v-if="auth.isSystemAdmin" class="badge badge--danger">
              {{ t('account.adminBadge') }}
            </span>
          </button>
          <template #dropdown>
            <el-dropdown-menu>
              <!-- 个人令牌：账号自己的长期凭据。 -->
              <el-dropdown-item command="personal-tokens">
                {{ t('account.personalTokens') }}
              </el-dropdown-item>
              <!-- LDAP 账号的口令在目录里维护，不给这个选项 -->
              <el-dropdown-item v-if="auth.canChangePassword" command="change-password">
                {{ t('account.changePassword') }}
              </el-dropdown-item>
              <el-dropdown-item command="logout">{{ t('account.logout') }}</el-dropdown-item>
            </el-dropdown-menu>
          </template>
        </el-dropdown>
      </template>
      <el-button v-else type="primary" plain @click="router.push('/login')">
        {{ t('route.login') }}
      </el-button>
    </div>
  </header>

  <el-dialog v-model="passwordVisible" :title="t('changePassword.title')" width="420px">
    <!-- 切语言会换掉 rules 对象：别让它顺手触发一次校验，
         否则空表单上会凭空冒出必填错误。 -->
    <el-form
      ref="passwordFormRef"
      :model="passwordForm"
      :rules="passwordRules"
      :validate-on-rule-change="false"
      :label-width="formLabelWidth(90)"
      @submit.prevent
    >
      <el-form-item :label="t('changePassword.oldPassword')" prop="old_password">
        <el-input
          v-model="passwordForm.old_password"
          type="password"
          show-password
          autocomplete="current-password"
          :placeholder="t('changePassword.oldPasswordPlaceholder')"
        />
      </el-form-item>
      <el-form-item :label="t('changePassword.newPassword')" prop="new_password">
        <el-input
          v-model="passwordForm.new_password"
          type="password"
          show-password
          autocomplete="new-password"
          :placeholder="t('changePassword.newPasswordPlaceholder')"
        />
      </el-form-item>
      <el-form-item :label="t('changePassword.confirmPassword')" prop="confirm_password">
        <el-input
          v-model="passwordForm.confirm_password"
          type="password"
          show-password
          autocomplete="new-password"
          :placeholder="t('changePassword.confirmPasswordPlaceholder')"
        />
      </el-form-item>
    </el-form>
    <!-- 提示放在表单外面：el-form 根节点是 flex 容器，
         直接塞进去会被挤进标签那一列、窄到一行几个字。 -->
    <div class="form-hint">{{ t('changePassword.hint') }}</div>
    <template #footer>
      <el-button @click="passwordVisible = false">{{ t('common.cancel') }}</el-button>
      <el-button type="primary" :loading="passwordSubmitting" @click="submitPassword">
        {{ t('changePassword.submit') }}
      </el-button>
    </template>
  </el-dialog>
</template>
