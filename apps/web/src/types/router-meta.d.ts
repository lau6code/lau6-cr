import 'vue-router'

declare module 'vue-router' {
  interface RouteMeta {
    /** 需要登录，未登录跳 /login?redirect=... */
    requiresAuth?: boolean
    /** 需要系统管理员，不是则跳 / */
    requiresAdmin?: boolean
    /** blank = 不带页头（登录页 / 引导页） */
    layout?: 'default' | 'blank'
    /** 浏览器标题的语言包键（见 src/locales/zh-CN.ts 的 route 段）。 */
    titleKey?: string
  }
}
