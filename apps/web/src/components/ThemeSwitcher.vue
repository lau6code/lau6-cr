<script setup lang="ts">
/**
 * 主题切换下拉（高亮 / 暗夜 / 跟随系统）。
 *
 * 顶栏、登录页、setup 引导页共用一个组件——主题是**界面偏好**，
 * 不属于账号，所以这三处都能放；选完存 localStorage（`stores/theme.ts`），
 * 下次进来还是上次的主题。
 */
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'

import { useThemeStore, type ThemePreference } from '@/stores/theme'

const { t } = useI18n()
const theme = useThemeStore()

/** 当前偏好（"跟随系统" 也算一种偏好，不显示实际生效的亮/暗）。 */
const label = computed(() => t(`theme.${theme.preference}`))

function onCommand(command: string | number | object): void {
  theme.setPreference(command as ThemePreference)
}
</script>

<template>
  <el-dropdown trigger="click" @command="onCommand">
    <button type="button" class="switch-btn" :title="t('theme.label')">
      {{ label }}
    </button>
    <template #dropdown>
      <el-dropdown-menu>
        <el-dropdown-item command="light" :disabled="theme.preference === 'light'">
          {{ t('theme.light') }}
        </el-dropdown-item>
        <el-dropdown-item command="dark" :disabled="theme.preference === 'dark'">
          {{ t('theme.dark') }}
        </el-dropdown-item>
        <el-dropdown-item command="system" :disabled="theme.preference === 'system'">
          {{ t('theme.system') }}
        </el-dropdown-item>
      </el-dropdown-menu>
    </template>
  </el-dropdown>
</template>
