/**
 * Vue I18n 实例与切换入口。
 *
 * 三种语言：简体中文（zh-CN）/ 繁体中文（zh-TW）/ 英文（en）。
 * 语言包在 `@/locales/`，键结构以 zh-CN 为准（见 `@/locales/schema.ts`）。
 */
import { createI18n } from 'vue-i18n'
import elementEn from 'element-plus/es/locale/lang/en'
import elementZhCn from 'element-plus/es/locale/lang/zh-cn'
import elementZhTw from 'element-plus/es/locale/lang/zh-tw'

import en from '@/locales/en'
import zhCN from '@/locales/zh-CN'
import zhTW from '@/locales/zh-TW'

import { DEFAULT_LOCALE, detectLocale, persistLocale, type LocaleKey } from './locale'

// 语言相关的常量从这里对外统一出口：组件只认 `@/i18n`，
// 不用再去摸 `@/i18n/locale` 的内部路径。
export {
  DEFAULT_LOCALE,
  LOCALE_LABEL_KEYS,
  LOCALE_STORAGE_KEY,
  SUPPORTED_LOCALES,
  detectLocale,
  normalizeLocale,
  persistLocale,
  type LocaleKey,
} from './locale'

export const i18n = createI18n({
  legacy: false,
  locale: detectLocale(),
  fallbackLocale: DEFAULT_LOCALE,
  messages: {
    'zh-CN': zhCN,
    'zh-TW': zhTW,
    en,
  },
  // ⚠️ 缺键必须**响**：
  //    vue-i18n 默认会把键名本身当文案渲染出来（界面上出现 "locale.zh-CN" 这种鬼东西），
  //    而这在生产构建里是静默的。打开告警，缺键在控制台里立刻可见。
  missingWarn: true,
  fallbackWarn: true,
})

/**
 * Element Plus 组件库自带文案（分页、日期选择器、表格空状态……）。
 *
 * ⚠️ 它**不跟**应用的语言包走：组件库按自己的 locale 走，
 *    所以这里要单独映射一份，切换语言时一起换。
 */
const elementLocales = {
  'zh-CN': elementZhCn,
  'zh-TW': elementZhTw,
  en: elementEn,
} as const

export function elementLocale(locale: LocaleKey) {
  return elementLocales[locale]
}

/** legacy: false 下 locale 是 ref。 */
export function currentLocale(): LocaleKey {
  return i18n.global.locale.value as LocaleKey
}

/**
 * 切换语言：改 i18n、改 `<html lang>`、记住选择。
 *
 * ⚠️ `<html lang>` 必须一起改：读屏软件靠它决定朗读语言，
 *    界面上是英文而 lang 仍是 zh-CN 时，读屏会念错。
 */
export function setLocale(locale: LocaleKey): void {
  i18n.global.locale.value = locale
  document.documentElement.setAttribute('lang', locale)
  persistLocale(locale)
}

/**
 * 在组件之外翻译。
 *
 * API 层的默认错误文案（`errorMessage(err, '...')`）不在组件里，
 * 拿不到 `useI18n()`，只能走这个入口。
 */
export function translate(key: string, named?: Record<string, unknown>): string {
  return named ? i18n.global.t(key, named) : i18n.global.t(key)
}

/**
 * 表单标签列宽。
 *
 * 英文标签普遍比中文长（"连通性测试" 4 个汉字 vs "Connectivity test" 16 个字符），
 * 固定宽度会让英文标签折成两行（"Connectivity" / "test"）。
 * 中文保持原宽度，英文按 1.6 倍放宽。
 */
export function formLabelWidth(px: number): string {
  return currentLocale() === 'en' ? `${Math.round((px * 1.6) / 5) * 5}px` : `${px}px`
}
