<script setup lang="ts">
import { computed } from 'vue'

/**
 * 镜像引用。
 *
 * 形如 team-a/api-gateway —— 斜杠左边是项目命名空间，右边是仓库名。
 * 两段用不同颜色分开并用等宽字体呈现：它在任何地方都是一段
 * 机器标识（要拼进 docker pull 命令），不是散文。
 */
const props = defineProps<{
  value: string
  tag?: string
}>()

const parts = computed(() => {
  const slash = props.value.indexOf('/')
  if (slash < 0) return { namespace: '', name: props.value }
  return {
    namespace: props.value.slice(0, slash),
    name: props.value.slice(slash + 1),
  }
})
</script>

<template>
  <span class="img-ref" :title="value">
    <template v-if="parts.namespace">
      <span class="img-ref__ns">{{ parts.namespace }}</span>
      <span class="img-ref__sep">/</span>
    </template>
    <span class="img-ref__name">{{ parts.name }}</span>
    <template v-if="tag">
      <span class="img-ref__tag-sep">:</span>
      <span class="img-ref__tag">{{ tag }}</span>
    </template>
  </span>
</template>
