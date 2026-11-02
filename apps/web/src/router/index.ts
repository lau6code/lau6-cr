import { watch } from 'vue'
import { createRouter, createWebHistory } from 'vue-router'

import { i18n } from '@/i18n'
import { useAuthStore } from '@/stores/auth'

const router = createRouter({
  history: createWebHistory(import.meta.env.BASE_URL),
  routes: [
    {
      path: '/',
      name: 'catalog',
      component: () => import('@/views/CatalogView.vue'),
      meta: { requiresAuth: true, titleKey: 'route.catalog' },
    },
    {
      // 仓库的标签页（镜像目录的下钻页）。
      // 仓库名里可能带 "/"（形如 team-a/sub/app），所以用 catch-all 参数接住：
      // vue-router 编码时把 "/" 编成 %2F，匹配后 params.name 会解码回完整仓库名。
      path: '/repository/:name(.*)',
      name: 'repository-tags',
      component: () => import('@/views/RepositoryTagsView.vue'),
      meta: { requiresAuth: true, titleKey: 'route.repositoryTags' },
    },
    {
      path: '/login',
      name: 'login',
      component: () => import('@/views/LoginView.vue'),
      meta: { layout: 'blank', titleKey: 'route.login' },
    },
    {
      path: '/setup',
      name: 'setup',
      component: () => import('@/views/SetupView.vue'),
      meta: { layout: 'blank', titleKey: 'route.setup' },
    },
    {
      path: '/projects',
      name: 'projects',
      component: () => import('@/views/ProjectsView.vue'),
      meta: { requiresAuth: true, titleKey: 'route.projects' },
    },
    {
      // 单个项目的保留策略页（入口在项目列表页每一行的「保留策略」按钮）。
      //
      // ⚠️ 项目名不含 "/"（命名规则只允许字母数字点下划线连字符），
      //    所以这里用普通参数即可，不需要 catch-all。
      path: '/project-retention/:name',
      name: 'project-retention-detail',
      component: () => import('@/views/ProjectRetentionDetailView.vue'),
      meta: { requiresAuth: true, titleKey: 'route.projectRetention' },
    },
    {
      // 单个项目的访问令牌页（入口在项目列表页每一行的「令牌」按钮，
      // 与项目详情页的「项目访问令牌」按钮）。
      //
      // ⚠️ 项目名不含 "/"（命名规则只允许字母数字点下划线连字符），
      //    所以这里用普通参数即可，不需要 catch-all。
      path: '/project-tokens/:name',
      name: 'project-tokens',
      component: () => import('@/views/ProjectTokensView.vue'),
      meta: { requiresAuth: true, titleKey: 'route.projectTokens' },
    },
    {
      path: '/project-members',
      name: 'project-members',
      component: () => import('@/views/ProjectMembersView.vue'),
      meta: { requiresAuth: true, titleKey: 'route.projectMembers' },
    },
    {
      path: '/projects/:name',
      name: 'project-detail',
      component: () => import('@/views/ProjectDetailView.vue'),
      meta: { requiresAuth: true, titleKey: 'route.projectDetail' },
    },
    {
      path: '/tokens',
      name: 'tokens',
      component: () => import('@/views/TokensView.vue'),
      meta: { requiresAuth: true, titleKey: 'route.tokens' },
    },
    {
      path: '/admin/users',
      name: 'admin-users',
      component: () => import('@/views/admin/UsersView.vue'),
      meta: { requiresAuth: true, requiresAdmin: true, titleKey: 'route.adminUsers' },
    },
    {
      path: '/admin/settings',
      name: 'admin-settings',
      component: () => import('@/views/admin/SettingsView.vue'),
      meta: { requiresAuth: true, requiresAdmin: true, titleKey: 'route.adminSettings' },
    },
    {
      path: '/admin/cleanup',
      name: 'admin-cleanup',
      component: () => import('@/views/admin/CleanupView.vue'),
      meta: { requiresAuth: true, requiresAdmin: true, titleKey: 'route.adminCleanup' },
    },
    {
      path: '/:pathMatch(.*)*',
      redirect: '/',
    },
  ],
})

router.beforeEach(async (to) => {
  const auth = useAuthStore()
  await auth.ensureLoaded()

  // 实例尚未初始化：后端对所有业务端点返回 setup_required，
  // 这时既没有账号可登录，也不该把人丢到登录页——去引导页。
  if (auth.setupRequired && to.path !== '/setup') {
    return { path: '/setup' }
  }

  if (to.meta.requiresAuth && !auth.isAuthenticated) {
    return { path: '/login', query: { redirect: to.fullPath } }
  }

  if (to.meta.requiresAdmin && !auth.isSystemAdmin) {
    return { path: '/' }
  }

  return true
})

/**
 * 路由切换后更新浏览器标题。
 *
 * ⚠️ 标题跟着**当前语言**走：切语言时标题也要跟着变，
 *    所以这里监听 i18n 的 locale 而不是只在导航时算一次。
 */
router.afterEach((to) => {
  const key = to.meta.titleKey
  if (typeof key !== 'string') return
  document.title = `${i18n.global.t(key)} · lau6-cr`
})

// 切换语言时标题也要跟着变——不重新导航也得重算。
watch(
  () => i18n.global.locale.value,
  () => {
    const key = router.currentRoute.value.meta.titleKey
    if (typeof key !== 'string') return
    document.title = `${i18n.global.t(key)} · lau6-cr`
  },
)

export { router }
export default router
