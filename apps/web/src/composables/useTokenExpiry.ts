/**
 * 令牌失效策略（个人令牌与项目令牌共用）。
 *
 * 两个页面的失效语义完全一样：默认 / 永久 / 指定时刻，
 * 失效时刻的计算、校验、复制也一模一样——差别只在用哪一套文案，
 * 所以文案键由调用方传进来，逻辑只有这一份。
 */
import { ref, type Ref } from 'vue'
import { ElMessage } from 'element-plus'

import { translate } from '@/i18n'
import type { Id } from '@/types'

/** 失效方式。 */
export type ExpiryMode = 'default' | 'never' | 'custom'

/**
 * 失效策略的请求体形状。
 *
 * PersonalTokenExpiry 与 ProjectTokenExpiry 结构完全一样，共用这一份。
 */
export interface TokenExpiry {
  never_expires: boolean
  expires_at?: number | null
}

/** 带失效时间的令牌（个人令牌与项目令牌都有这两个字段）。 */
export interface ExpirableToken {
  id: Id
  expires_at: number | null
}

/** 失效相关文案键。 */
export interface TokenExpiryStrings {
  /** "30 天"的文案键。 */
  durationDays: string
  /** "12 小时"的文案键。 */
  durationHours: string
  /** 秒数的文案键。 */
  durationSeconds: string
  /** 失效时间非法时的提示键。 */
  invalidExpiry: string
}

/**
 * 令牌是否已过期（永久令牌永不过期）。
 *
 * 单独导出而不是只在组合式函数里：列表表格也要用同一份判定，
 * 两处各写一遍，改了口径只改一处就会对不上。
 */
export function tokenIsExpired(token: ExpirableToken): boolean {
  if (token.expires_at === null) return false
  return token.expires_at * 1000 <= Date.now()
}

export interface UseTokenExpiryOptions {
  /** 系统默认有效期（秒），来自后端。 */
  defaultTtl: Ref<number>
  /** 文案键。 */
  strings: TokenExpiryStrings
}

export function useTokenExpiry(options: UseTokenExpiryOptions) {
  const { defaultTtl, strings } = options

  /** 创建对话框的失效方式与时间。 */
  const createMode = ref<ExpiryMode>('default')
  const createCustom = ref<Date | null>(null)

  /** 改失效时间对话框的目标与选择。 */
  const editVisible = ref(false)
  const editTarget = ref<ExpirableToken | null>(null)
  const editMode = ref<ExpiryMode>('default')
  const editCustom = ref<Date | null>(null)

  /** 系统默认有效期对应的到期时刻（用来给"指定失效时间"预填）。 */
  function defaultExpiryDate(): Date {
    return new Date(Date.now() + defaultTtl.value * 1000)
  }

  /** 把"秒"说成人话：30 天 / 12 小时。 */
  function formatDuration(seconds: number): string {
    if (seconds % 86400 === 0) return translate(strings.durationDays, { count: seconds / 86400 })
    if (seconds % 3600 === 0) return translate(strings.durationHours, { count: seconds / 3600 })
    return translate(strings.durationSeconds, { count: seconds })
  }

  /**
   * 把界面上的选择翻译成后端要的失效策略。
   *
   * 返回 null = 用户还没选时间（调用方负责提示）。
   */
  function buildExpiry(mode: ExpiryMode, custom: Date | null): TokenExpiry | null {
    if (mode === 'never') return { never_expires: true }
    if (mode === 'custom') {
      if (!custom) return null
      // 必须晚于现在——过去的失效时间等于"立即作废"，后端也会拒。
      if (custom.getTime() <= Date.now()) return null
      return { never_expires: false, expires_at: Math.floor(custom.getTime() / 1000) }
    }
    return { never_expires: false }
  }

  /** 校验失效时间；不合法时提示用户并返回 null。 */
  function checkedExpiry(mode: ExpiryMode, custom: Date | null): TokenExpiry | null {
    const expiry = buildExpiry(mode, custom)
    if (!expiry) ElMessage.warning(translate(strings.invalidExpiry))
    return expiry
  }

  /** 打开"改失效时间"对话框，按当前状态预填。 */
  function openEdit(token: ExpirableToken): void {
    editTarget.value = token
    if (token.expires_at === null) {
      editMode.value = 'never'
      editCustom.value = null
    } else {
      editMode.value = 'custom'
      editCustom.value = new Date(token.expires_at * 1000)
    }
    editVisible.value = true
  }

  /** 禁用今天之前的日期（选过去的时间没有意义）。 */
  function disabledPastDate(date: Date): boolean {
    const today = new Date()
    today.setHours(0, 0, 0, 0)
    return date.getTime() < today.getTime()
  }

  return {
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
  }
}
