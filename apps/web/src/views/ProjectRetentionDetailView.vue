<script setup lang="ts">
/**
 * 单个项目的保留策略页。
 *
 * 项目级策略**覆盖**全局默认（03-registry.md §5.2.3）：
 * 这里配了策略，清理时就不再用全局默认。
 *
 * 页面直接承载编辑器：启用开关、tag 选择器、规则增删、重置回全局默认。
 * 地址里带项目名（/project-retention/<项目名>），可以收藏、可以分享。
 */
import { computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useI18n } from 'vue-i18n'

import PageHeader from '@/components/PageHeader.vue'
import RetentionPolicyEditor from '@/components/RetentionPolicyEditor.vue'

const route = useRoute()
const router = useRouter()
const { t } = useI18n()

/** 项目名。 */
const projectName = computed(() => String(route.params.name ?? '').trim())

function backToProject(): void {
  void router.push({ name: 'project-detail', params: { name: projectName.value } })
}
</script>

<template>
  <div class="page-panel">
    <page-header :title="projectName || t('route.projectRetention')">
      <template #subtitle>
        <span class="page-meta">
          <span>{{ t('retention.projectScopeHint') }}</span>
        </span>
      </template>

      <template #actions>
        <el-button @click="backToProject">{{ t('retention.backToProject') }}</el-button>
      </template>
    </page-header>

    <el-alert
      v-if="!projectName"
      type="error"
      :title="t('retention.missingProject')"
      show-icon
      :closable="false"
      style="margin-bottom: var(--space-4)"
    >
      <div class="form-hint" style="margin-top: var(--space-2)">
        {{ t('retention.pickFromProjects') }}
      </div>
    </el-alert>

    <retention-policy-editor
      v-else
      scope="project"
      :project-name="projectName"
    />
  </div>
</template>
