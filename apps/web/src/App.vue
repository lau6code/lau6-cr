<script setup lang="ts">
import { computed } from 'vue'
import { useRoute } from 'vue-router'

import AppHeader from '@/components/AppHeader.vue'
import AppSidebar from '@/components/AppSidebar.vue'
import { currentLocale, elementLocale } from '@/i18n'

const route = useRoute()

/** 登录页与引导页不带导航外壳 */
const blankLayout = computed(() => route.meta.layout === 'blank')

/**
 * Element Plus 自带文案的 locale。
 *
 * ⚠️ 不能在 app.use(ElementPlus) 时固定：固定了就切不动。
 *    组件库的文案（分页、日期选择器）走 <el-config-provider>，
 *    跟着当前语言一起换。
 */
const epLocale = computed(() => elementLocale(currentLocale()))
</script>

<template>
  <el-config-provider :locale="epLocale">
    <router-view v-if="blankLayout" />

    <div v-else class="shell">
      <app-header />
      <div class="shell-body">
        <app-sidebar />
        <main class="content">
          <router-view />
        </main>
      </div>
    </div>
  </el-config-provider>
</template>
