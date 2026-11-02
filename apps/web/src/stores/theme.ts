/**
 * 主题：高亮（亮色）与暗夜（暗色）。
 *
 * 默认**跟随系统**（prefers-color-scheme）：用户没选过时，
 * 系统是深色的就用暗夜，是浅色的就用高亮。
 * 用户选过之后以他的选择为准（存 localStorage）。
 */
import { defineStore } from 'pinia'
import { computed, ref, watch } from 'vue'

/** system = 跟随操作系统。 */
export type ThemePreference = 'light' | 'dark' | 'system'

type ResolvedTheme = 'light' | 'dark'

const THEME_STORAGE_KEY = 'lau6-cr.theme'

/** 深色偏好媒体查询。 */
const DARK_QUERY = '(prefers-color-scheme: dark)'

/** 读已保存的主题偏好；读不到就当"跟随系统"。 */
function readStoredPreference(): ThemePreference {
  try {
    const raw = window.localStorage.getItem(THEME_STORAGE_KEY)
    if (raw === 'light' || raw === 'dark' || raw === 'system') return raw
  } catch {
    // 存不了就当没选过。
  }
  return 'system'
}

function persistPreference(preference: ThemePreference): void {
  try {
    window.localStorage.setItem(THEME_STORAGE_KEY, preference)
  } catch {
    // 存不了不影响本次会话内的切换。
  }
}

export const useThemeStore = defineStore('theme', () => {
  /** 用户的偏好。 */
  const preference = ref<ThemePreference>(readStoredPreference())

  /** 系统当前是不是深色。 */
  const systemDark = ref(window.matchMedia(DARK_QUERY).matches)

  // 跟随系统时，系统改主题这里要跟着改。
  window.matchMedia(DARK_QUERY).addEventListener('change', (event) => {
    systemDark.value = event.matches
  })

  /** 实际生效的主题。 */
  const resolved = computed<ResolvedTheme>(() => {
    if (preference.value === 'system') return systemDark.value ? 'dark' : 'light'
    return preference.value
  })

  /**
   * 把主题落到 `<html>` 上。
   *
   * Element Plus 的暗色变量表也是靠 `html.dark` 激活的
   * （theme-chalk/dark/css-vars.css），所以只切这个类名就够了。
   */
  function applyTheme(theme: ResolvedTheme): void {
    document.documentElement.classList.toggle('dark', theme === 'dark')
  }

  watch(resolved, applyTheme, { immediate: true })
  watch(preference, persistPreference)

  /** 设置主题偏好。 */
  function setPreference(value: ThemePreference): void {
    preference.value = value
  }

  return { preference, setPreference }
})
