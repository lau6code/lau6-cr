<script setup lang="ts">
/**
 * 语言切换下拉。
 *
 * 顶栏、登录页、setup 引导页共用一个组件——切换器是**界面偏好**，
 * 不属于账号，所以这三处都能放；语言选完存 localStorage（`@/i18n` 的
 * persistLocale），下次进来还是上次的语言。
 */
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'

import { currentLocale, LOCALE_LABEL_KEYS, setLocale, SUPPORTED_LOCALES, type LocaleKey } from '@/i18n'

const { t } = useI18n()

/** 当前语言的显示名（"简体中文" / "繁體中文" / "English"）。 */
const label = computed(() => t(LOCALE_LABEL_KEYS[currentLocale()]))

function onCommand(command: string | number | object): void {
  setLocale(command as LocaleKey)
}
</script>

<template>
  <el-dropdown trigger="click" @command="onCommand">
    <button type="button" class="switch-btn" :title="t('locale.label')">
      {{ label }}
    </button>
    <template #dropdown>
      <el-dropdown-menu>
        <el-dropdown-item
          v-for="item in SUPPORTED_LOCALES"
          :key="item"
          :command="item"
          :disabled="item === currentLocale()"
        >
          {{ t(LOCALE_LABEL_KEYS[item]) }}
        </el-dropdown-item>
      </el-dropdown-menu>
    </template>
  </el-dropdown>
</template>
