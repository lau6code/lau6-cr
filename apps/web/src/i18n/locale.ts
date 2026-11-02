/**
 * i18n 基础设施：语言探测、归一化、持久化与 Vue I18n 实例。
 *
 * 三种语言：简体中文（zh-CN）/ 繁体中文（zh-TW）/ 英文（en）。
 *
 * ⚠️ 语言选择存在**浏览器本地**（localStorage），不落服务端：
 * 它是"这台设备上怎么看这个界面"的偏好，不是账号属性。
 */

export type LocaleKey = 'zh-CN' | 'zh-TW' | 'en'

export const SUPPORTED_LOCALES: LocaleKey[] = ['zh-CN', 'zh-TW', 'en']

/**
 * 语言代码 → 语言包里的显示名键。
 *
 * ⚠️ 语言代码带连字符（`zh-CN`），语言包的键用小驼峰（`zhCN`）：
 *    直接拼 `locale.${code}` 查不到键，vue-i18n 会把**键名本身**当文案显示出来
 *    （切换器上就会出现 "locale.zh-CN" 这种鬼东西）。
 */
export const LOCALE_LABEL_KEYS: Record<LocaleKey, string> = {
  'zh-CN': 'locale.zhCN',
  'zh-TW': 'locale.zhTW',
  en: 'locale.en',
}

/** 兜底语言：任何语言下缺键都回落到它。 */
export const DEFAULT_LOCALE: LocaleKey = 'zh-CN'

export const LOCALE_STORAGE_KEY = 'lau6-cr.locale'

/**
 * 把任意语言标记归一化到受支持的语言。
 *
 * 浏览器可能给出 `zh-Hant-TW`、`zh-HK`、`en-GB` 这类标记，
 * 前端要的是"选哪份语言包"，不是"原样透传"。
 */
export function normalizeLocale(input: string | null | undefined): LocaleKey {
  if (!input) return DEFAULT_LOCALE
  const tag = input.replace('_', '-').toLowerCase()
  // 繁体：zh-TW / zh-Hant / zh-HK / zh-MO
  if (tag === 'zh-tw' || tag === 'zh-hant' || tag === 'zh-hk' || tag === 'zh-mo') return 'zh-TW'
  // 简体：zh / zh-CN / zh-Hans / zh-SG
  if (tag === 'zh' || tag === 'zh-cn' || tag === 'zh-hans' || tag === 'zh-sg') return 'zh-CN'
  if (tag === 'en' || tag.startsWith('en-')) return 'en'
  return DEFAULT_LOCALE
}

/**
 * 探测初始语言：已保存的选择优先，其次浏览器语言。
 *
 * ⚠️ 读 localStorage 可能抛（隐私模式 / 存储被禁用）：
 *    那时退回浏览器语言，不让界面直接崩掉。
 */
export function detectLocale(): LocaleKey {
  try {
    const saved = window.localStorage.getItem(LOCALE_STORAGE_KEY)
    if (saved) return normalizeLocale(saved)
  } catch {
    // 存不了就当没存过。
  }
  return normalizeLocale(window.navigator.language)
}

export function persistLocale(locale: LocaleKey): void {
  try {
    window.localStorage.setItem(LOCALE_STORAGE_KEY, locale)
  } catch {
    // 存不了不影响本次会话内的切换。
  }
}
