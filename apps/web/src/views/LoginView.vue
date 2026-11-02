<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import type { FormInstance, FormRules } from 'element-plus'
import { useI18n } from 'vue-i18n'

import { errorMessage } from '@/api/client'
import { fetchPasswordTransport, takePendingNotice } from '@/api/passwordTransport'
import LocaleSwitcher from '@/components/LocaleSwitcher.vue'
import ThemeSwitcher from '@/components/ThemeSwitcher.vue'
import { useAuthStore } from '@/stores/auth'

const route = useRoute()
const router = useRouter()
const auth = useAuthStore()
const { t } = useI18n()

const formRef = ref<FormInstance>()
const submitting = ref(false)
/**
 * 拉不到口令加密公钥时的原因。
 *
 * 非空 = 服务端可能要求加密登录，而客户端拿不到公钥——
 * 这时提交会失败，**必须**让用户看见，不能等提交失败才说。
 */
const transportError = ref('')

const form = reactive({
  username: '',
  password: '',
})

const rules = computed<FormRules>(() => ({
  username: [{ required: true, message: t('login.usernameRequired'), trigger: 'blur' }],
  password: [{ required: true, message: t('login.passwordRequired'), trigger: 'blur' }],
}))

/** 只接受站内相对路径，避免开放重定向 */
const redirect = computed(() => {
  const target = route.query.redirect
  return typeof target === 'string' && target.startsWith('/') ? target : '/'
})

onMounted(() => {
  if (auth.isAuthenticated) {
    void router.replace(redirect.value)
    return
  }
  // 预热公钥：提前知道服务端是否要求加密登录，
  // 也能在提交时公钥拉不到的情况下**明确报错**而不是降级成明文。
  void fetchPasswordTransport().catch((err: unknown) => {
    transportError.value = errorMessage(err, t('passwordTransport.pubkeyUnavailable'))
  })
})

async function submit(): Promise<void> {
  if (!formRef.value) return
  const valid = await formRef.value.validate().catch(() => false)
  if (!valid) return

  submitting.value = true
  try {
    const outcome = await auth.login(form.username, form.password)
    if (outcome.notice) ElMessage.warning(outcome.notice)
    ElMessage.success(t('login.success'))
    await router.push(redirect.value)
  } catch (err) {
    const notice = takePendingNotice()
    if (notice) ElMessage.warning(notice)
    ElMessage.error(errorMessage(err, t('login.failed')))
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <div class="blank-layout">
    <div class="blank-layout__card">
      <div class="blank-layout__head">
        <span class="blank-layout__brand">lau6-cr</span>
        <div class="blank-layout__head-right">
          <span class="status-pill status-muted">{{ t('login.brand') }}</span>
          <!-- 登录页没有顶栏，语言与主题的切换器得放在这里。 -->
          <LocaleSwitcher />
          <ThemeSwitcher />
        </div>
      </div>

      <el-alert
        v-if="auth.loadError"
        class="login-alert"
        type="error"
        :title="`${t('login.loadStateFailed')}：${auth.loadError}`"
        show-icon
        :closable="false"
      />

      <el-alert
        v-if="transportError"
        class="login-alert"
        type="warning"
        :title="`${transportError}：${t('login.pubkeyWarning')}`"
        show-icon
        :closable="false"
      />

      <!-- 切语言会换掉 rules 对象：别让它顺手触发一次校验，
           否则空表单上会凭空冒出必填错误。 -->
      <el-form
        ref="formRef"
        :model="form"
        :rules="rules"
        :validate-on-rule-change="false"
        label-position="top"
        @submit.prevent
      >
        <el-form-item :label="t('login.username')" prop="username">
          <el-input
            v-model="form.username"
            autocomplete="username"
            :placeholder="t('login.usernamePlaceholder')"
          />
        </el-form-item>
        <el-form-item :label="t('login.password')" prop="password">
          <el-input
            v-model="form.password"
            type="password"
            autocomplete="current-password"
            show-password
            :placeholder="t('login.passwordPlaceholder')"
            @keyup.enter="submit"
          />
        </el-form-item>
        <el-button type="primary" style="width: 100%" :loading="submitting" @click="submit">
          {{ t('login.submit') }}
        </el-button>
      </el-form>
    </div>
  </div>
</template>
