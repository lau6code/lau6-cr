<script setup lang="ts">
/**
 * 页面标题区。
 *
 * 标题、说明、右上角操作在每个列表页都是同一套结构，
 * 收在一个组件里，避免每个页面各写一遍 .page-header。
 */
withDefaults(
  defineProps<{
    title: string
    subtitle?: string
  }>(),
  { subtitle: '' },
)
</script>

<template>
  <header class="page-header">
    <div class="page-header__title-block">
      <h1 class="page-title">
        <slot name="title">{{ title }}</slot>
      </h1>
      <p v-if="subtitle || $slots.subtitle" class="page-subtitle">
        <slot name="subtitle">{{ subtitle }}</slot>
      </p>
      <slot name="meta" />
    </div>
    <div v-if="$slots.actions" class="page-header__actions">
      <slot name="actions" />
    </div>
  </header>
</template>
