<script setup lang="ts">
import { computed } from 'vue'
import { useRoute } from 'vue-router'
import { useI18n } from 'vue-i18n'

import { useAuthStore } from '@/stores/auth'

const route = useRoute()
const auth = useAuthStore()
const { t } = useI18n()

interface NavItem {
  label: string
  /** 窄屏侧栏收起时显示的短标签 */
  short: string
  to: string
}

/**
 * 侧栏按职责分成两组：仓库侧的日常操作、管理员的系统操作。
 *
 * 组标题是固定的，不再跟着当前路由变——否则同一组里的
 * "镜像目录"会突然被划到"管理"下面。
 *
 * ⚠️ 账号自己的凭据（个人令牌）不在侧栏里：
 *    它跟着用户名走，入口在 navbar 的用户菜单里。
 */
const repoItems = computed<NavItem[]>(() => [
  { label: t('nav.projects'), short: t('nav.shortProjects'), to: '/projects' },
  { label: t('nav.catalog'), short: t('nav.shortCatalog'), to: '/' },
  { label: t('nav.members'), short: t('nav.shortMembers'), to: '/project-members' },
])

const adminItems = computed<NavItem[]>(() => {
  if (!auth.isSystemAdmin) return []
  return [
    { label: t('nav.users'), short: t('nav.shortUsers'), to: '/admin/users' },
    { label: t('nav.cleanup'), short: t('nav.shortCleanup'), to: '/admin/cleanup' },
    { label: t('nav.settings'), short: t('nav.shortSettings'), to: '/admin/settings' },
  ]
})

/**
 * 是否高亮。
 *
 * 不能靠 RouterLink 自带的 active-class：路由表里 `/projects` 和
 * `/projects/:name` 是**平级的两条**记录（不是父子嵌套），
 * vue-router 拿当前路由的 matched 去比对时匹配不到父记录，
 * 于是进项目详情页时"项目"会掉高亮。这里自己按路径判断。
 */
function isActive(to: string): boolean {
  const path = route.path
  // 仓库标签页（/repository/<仓库名>）是镜像目录的下钻页，
  // 菜单上仍算「镜像目录」这一项。
  if (to === '/') return path === '/' || path.startsWith('/repository/')
  // 保留策略页与项目令牌页是某个项目的下钻页，入口在项目列表的每一行，
  // 菜单上仍算「项目」这一组。
  if (to === '/projects') {
    return (
      path === '/projects' ||
      path.startsWith('/projects/') ||
      path.startsWith('/project-retention/') ||
      path.startsWith('/project-tokens/')
    )
  }
  return path === to || path.startsWith(`${to}/`)
}
</script>

<template>
  <aside class="sidebar">
    <div class="sidebar__label">{{ t('nav.groupRepo') }}</div>
    <nav :aria-label="t('nav.repoNavLabel')">
      <RouterLink
        v-for="item in repoItems"
        :key="item.to"
        :to="item.to"
        :class="{ active: isActive(item.to) }"
        :title="item.label"
        :aria-label="item.label"
      >
        <span class="nav-text nav-text--full">{{ item.label }}</span>
        <span class="nav-text nav-text--short" aria-hidden="true">{{ item.short }}</span>
      </RouterLink>
    </nav>

    <template v-if="adminItems.length">
      <div class="sidebar__label">{{ t('nav.groupManage') }}</div>
      <nav :aria-label="t('nav.manageNavLabel')">
        <RouterLink
          v-for="item in adminItems"
          :key="item.to"
          :to="item.to"
          :class="{ active: isActive(item.to) }"
          :title="item.label"
          :aria-label="item.label"
        >
          <span class="nav-text nav-text--full">{{ item.label }}</span>
          <span class="nav-text nav-text--short" aria-hidden="true">{{ item.short }}</span>
        </RouterLink>
      </nav>
    </template>

    <div class="sidebar__spacer" />
  </aside>
</template>
