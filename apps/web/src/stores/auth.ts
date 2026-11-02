import { computed, ref } from 'vue'
import { defineStore } from 'pinia'

import { fetchMe, login as loginRequest, logout as logoutRequest } from '@/api/auth'
import { errorMessage, isSetupRequired } from '@/api/client'
import { translate } from '@/i18n'
import type { User } from '@/types'

export const useAuthStore = defineStore('auth', () => {
  const user = ref<User | null>(null)
  const anonymous = ref(true)
  const loaded = ref(false)
  const loading = ref(false)
  /**
   * 实例尚未初始化（后端对业务端点返回 setup_required）。
   *
   * 这不是"未登录"——此时还没有任何账号可登录，该去 `/setup` 引导页。
   */
  const setupRequired = ref(false)
  /**
   * 登录态加载失败的原因（后端不可达 / 服务端故障）。
   *
   * 空串 = 没有失败。**非空时必须让用户看见**：
   * 把它静默当成"未登录"会把用户扔到登录页，还让人以为是自己被登出了。
   */
  const loadError = ref('')

  const isAuthenticated = computed(() => user.value !== null)
  const isSystemAdmin = computed(() => user.value?.is_system_admin === true)
  const displayName = computed(() => user.value?.display_name || user.value?.username || '')

  /**
   * 能否自助改密。
   *
   * 只有本地账号可以：LDAP 账号的口令由目录维护，后端对 `source != 'local'`
   * 一律拒绝（service.ChangeLocalPassword → ErrPasswordNotLocal），
   * 菜单里就不该给出这个选项。
   */
  const canChangePassword = computed(() => user.value !== null && user.value.source === 'local')

  /**
   * 拉取当前登录态。
   *
   * 三种结果必须分开，不能一律当成"未登录"：
   * - 正常：`/auth/me` 对匿名返回 200 + `anonymous:true`（不是错误）；
   * - 未初始化：50302 `setup_required` → 该去 `/setup`，不是登录页；
   * - 其他失败（后端不可达 / 5xx）：真实故障，记进 `loadError` 显式提示，
   *   不能悄悄把人踢到登录页装作"你被登出了"。
   */
  async function refresh(): Promise<void> {
    loading.value = true
    try {
      const me = await fetchMe()
      anonymous.value = me.anonymous === true
      user.value = me.user ?? null
      setupRequired.value = false
      loadError.value = ''
    } catch (err) {
      user.value = null
      anonymous.value = true
      if (isSetupRequired(err)) {
        setupRequired.value = true
        loadError.value = ''
      } else {
        setupRequired.value = false
        loadError.value = errorMessage(err, translate('login.loadStateFailed'))
      }
    } finally {
      loaded.value = true
      loading.value = false
    }
  }

  /** 路由守卫里调用：只在第一次时真正请求 /auth/me。 */
  async function ensureLoaded(): Promise<void> {
    if (!loaded.value) await refresh()
  }

  /**
   * 标记实例已完成初始化。
   *
   * 引导页在提交 setup 之前，`/auth/me` 返回的是 503 `setup_required`，
   * 于是 `setupRequired` 被置成 true；路由守卫据此把**所有**非 /setup 的跳转
   * 弹回 /setup（`router/index.ts`）。
   *
   * 所以 setup 提交成功后必须显式清掉这个标记，
   * 否则 `router.replace('/login')` 会被守卫弹回 /setup——
   * 用户看到的是"初始化完成"的提示，却还停在引导页上。
   */
  function markInitialized(): void {
    setupRequired.value = false
    loaded.value = true
    loadError.value = ''
  }

  /**
   * 登录。
   *
   * 返回里的 `notice` 非空 = 发生了降级（公钥拉不到，按明文提交了），
   * 调用方**必须**把它显示给用户——不能悄悄吞掉。
   */
  async function login(
    username: string,
    password: string,
  ): Promise<{ user: User; notice: string | null }> {
    const outcome = await loginRequest({ username, password })
    user.value = outcome.data.user
    anonymous.value = false
    loaded.value = true
    setupRequired.value = false
    loadError.value = ''
    return { user: outcome.data.user, notice: outcome.notice }
  }

  async function logout(): Promise<void> {
    try {
      await logoutRequest()
    } finally {
      user.value = null
      anonymous.value = true
      loaded.value = true
    }
  }

  return {
    user,
    loaded,
    loading,
    setupRequired,
    loadError,
    isAuthenticated,
    isSystemAdmin,
    displayName,
    canChangePassword,
    refresh,
    ensureLoaded,
    markInitialized,
    login,
    logout,
  }
})
