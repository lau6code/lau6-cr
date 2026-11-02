<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { useI18n } from 'vue-i18n'

import { errorMessage } from '@/api/client'
import { listProjectMembers, listProjects } from '@/api/projects'
import PageHeader from '@/components/PageHeader.vue'
import MemberDialog from '@/components/MemberDialog.vue'
import { useProjectMembers } from '@/composables/useProjectMembers'
import { useAuthStore } from '@/stores/auth'
import type { Project, ProjectMembership, ProjectRole } from '@/types'
import { formatUnixSeconds } from '@/utils/format'
import { ROLE_TONE } from '@/utils/roles'

const auth = useAuthStore()
const { t } = useI18n()

const loading = ref(false)
const memberships = ref<ProjectMembership[]>([])
const projects = ref<Project[]>([])

const projectFilter = ref('')
const keyword = ref('')

/** 表单里选中的项目；加成员时加到这个项目。 */
const selectedProject = ref('')

/**
 * 与后端 isProjectAdmin 对齐：系统管理员对所有项目都有项目管理员权限；
 * 其余人只有在自己担任 admin 的项目里才能增删成员。
 *
 * 数据来自成员关系表本身——自己的那一行就是自己在这个项目里的角色。
 */
const manageableProjects = computed(() => {
  const names = new Set<string>()
  if (auth.isSystemAdmin) {
    for (const p of projects.value) names.add(p.name)
    return names
  }
  const me = auth.user?.username
  if (!me) return names
  for (const m of memberships.value) {
    if (m.username === me && m.role === 'admin') names.add(m.project)
  }
  return names
})

const manageableProjectList = computed(() =>
  projects.value.filter((p) => manageableProjects.value.has(p.name)),
)

const {
  dialogVisible,
  submitting,
  candidatesLoading,
  form,
  existingCandidates,
  newCandidates,
  loadCandidates,
  openDialog: openMemberDialog,
  submit,
  remove,
} = useProjectMembers({
  targetProject: () => selectedProject.value,
  requireTarget: true,
  reload: load,
})

const filtered = computed(() => {
  const key = keyword.value.trim().toLowerCase()
  return memberships.value.filter((m) => {
    if (projectFilter.value && m.project !== projectFilter.value) return false
    if (!key) return true
    return (
      m.username.toLowerCase().includes(key) ||
      m.display_name.toLowerCase().includes(key) ||
      m.project.toLowerCase().includes(key)
    )
  })
})

async function load(): Promise<void> {
  loading.value = true
  try {
    const [memberData, projectData] = await Promise.all([listProjectMembers(), listProjects()])
    memberships.value = memberData.members ?? []
    projects.value = projectData.projects ?? []
  } catch (err) {
    ElMessage.error(errorMessage(err, t('projectDetail.loadFailed')))
  } finally {
    loading.value = false
  }
}

// 换项目 → 换候选名单，已选的人也不再适用。
watch(
  selectedProject,
  (project) => {
    form.username = ''
    void loadCandidates(project)
  },
)

function openDialog(): void {
  selectedProject.value = manageableProjectList.value[0]?.name ?? ''
  openMemberDialog()
}

onMounted(() => {
  void load()
})
</script>

<template>
  <div class="page-panel">
    <page-header
      :title="t('route.projectMembers')"
      :subtitle="t('projectDetail.crossProjectSubtitle', { count: memberships.length })"
    >
      <template #actions>
        <el-button :loading="loading" @click="load">{{ t('common.refresh') }}</el-button>
        <el-button
          v-if="manageableProjectList.length > 0"
          type="primary"
          @click="openDialog"
        >
          {{ t('projectDetail.addMember') }}
        </el-button>
      </template>
    </page-header>

    <div class="toolbar">
      <el-select
        v-model="projectFilter"
        :placeholder="t('common.allProjects')"
        clearable
        class="form-control form-control--sm"
      >
        <el-option
          v-for="project in projects"
          :key="project.name"
          :label="project.display_name || project.name"
          :value="project.name"
        />
      </el-select>

      <el-input
        v-model="keyword"
        :placeholder="t('projectDetail.filterPlaceholder')"
        clearable
        class="form-control form-control--sm"
      />

      <span class="toolbar__spacer" />
      <span class="toolbar__count">
        {{ t('catalog.matched', { matched: filtered.length, total: memberships.length }) }}
      </span>
    </div>

    <el-table
      v-loading="loading"
      :data="filtered"
      style="width: 100%"
      :empty-text="t('projectDetail.emptyCrossProject')"
    >
      <el-table-column prop="project" :label="t('nav.projects')" width="150">
        <template #default="{ row }">
          <span class="mono" style="color: var(--text); font-weight: 600">{{ row.project }}</span>
        </template>
      </el-table-column>

      <el-table-column prop="username" :label="t('login.username')" width="170">
        <template #default="{ row }">
          <span class="mono" style="color: var(--text); font-weight: 600">{{ row.username }}</span>
        </template>
      </el-table-column>

      <!-- 显示名是最长的一列，让它跟着容器伸缩 -->
      <el-table-column
        prop="display_name"
        :label="t('projects.columnDisplayName')"
        min-width="220"
        show-overflow-tooltip
      />

      <el-table-column :label="t('common.role')" width="100">
        <template #default="{ row }">
          <span class="status-pill" :class="ROLE_TONE[row.role as ProjectRole] || 'status-muted'">
            {{ t(`roles.${row.role as ProjectRole}`) }}
          </span>
        </template>
      </el-table-column>

      <el-table-column :label="t('projectDetail.joinedAt')" width="150">
        <template #default="{ row }">
          <span class="cell-mono">{{ formatUnixSeconds(row.created_at) }}</span>
        </template>
      </el-table-column>

      <el-table-column :label="t('common.actions')" width="90" align="right">
        <template #default="{ row }">
          <el-button
            v-if="manageableProjects.has(row.project as string)"
            link
            type="danger"
            @click="remove(row as ProjectMembership, row.project)"
          >
            {{ t('projectDetail.remove') }}
          </el-button>
          <span v-else class="cell-mono">-</span>
        </template>
      </el-table-column>
    </el-table>

    <member-dialog
      v-model="dialogVisible"
      v-model:username="form.username"
      v-model:role="form.role"
      :candidates-loading="candidatesLoading"
      :new-candidates="newCandidates"
      :existing-candidates="existingCandidates"
      :submitting="submitting"
      :candidates-disabled="!selectedProject"
      @submit="submit"
    >
      <template #target>
        <el-form-item :label="t('nav.projects')" required>
          <el-select v-model="selectedProject" class="form-control">
            <el-option
              v-for="project in manageableProjectList"
              :key="project.name"
              :label="t('common.parenthesized', { label: project.display_name || project.name, value: project.name })"
              :value="project.name"
            />
          </el-select>
        </el-form-item>
      </template>
    </member-dialog>
  </div>
</template>
