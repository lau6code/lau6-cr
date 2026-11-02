<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { useI18n } from 'vue-i18n'

import { errorMessage } from '@/api/client'
import { getProject } from '@/api/projects'
import PageHeader from '@/components/PageHeader.vue'
import MemberDialog from '@/components/MemberDialog.vue'
import ProjectEditDialog from '@/components/ProjectEditDialog.vue'
import { useProjectMembers } from '@/composables/useProjectMembers'
import { useAuthStore } from '@/stores/auth'
import type { Project, ProjectMember, ProjectRole } from '@/types'
import { ROLE_TONE } from '@/utils/roles'
import { mergeProject } from '@/utils/project'

const route = useRoute()
const router = useRouter()
const auth = useAuthStore()
const { t } = useI18n()

const projectName = computed(() => String(route.params.name ?? ''))

const loading = ref(false)
const project = ref<Project | null>(null)
const members = ref<ProjectMember[]>([])

const editVisible = ref(false)

/**
 * 当前用户是不是这个项目的管理员。
 *
 * 与后端 isProjectAdmin 对齐：系统管理员对所有项目都有项目管理员权限；
 * 其余人只有在自己担任 admin 的项目里才能管理成员 / 编辑项目。
 *
 * my_role 由后端按请求主体算好（项目详情接口带这个字段）。
 */
const isProjectAdmin = computed(
  () => auth.isSystemAdmin || project.value?.my_role === 'admin',
)

const {
  dialogVisible,
  submitting,
  candidatesLoading,
  form,
  existingCandidates,
  newCandidates,
  openDialog,
  submit,
  remove,
} = useProjectMembers({
  targetProject: () => projectName.value,
  requireTarget: false,
  reload: load,
})

async function load(): Promise<void> {
  loading.value = true
  try {
    const data = await getProject(projectName.value)
    project.value = data.project
    members.value = data.members ?? []
  } catch (err) {
    ElMessage.error(errorMessage(err, t('projectDetail.loadFailed')))
  } finally {
    loading.value = false
  }
}

/** 去项目令牌页（项目管理员专属）。 */
function goToTokens(): void {
  void router.push({ name: 'project-tokens', params: { name: projectName.value } })
}

/** 项目属性被改过（显示名 / 描述 / 可见性）后刷新详情。 */
function onEdited(updated: Project): void {
  if (!project.value) {
    project.value = updated
    return
  }
  project.value = mergeProject(project.value, updated)
}

onMounted(() => {
  void load()
})
</script>

<template>
  <div class="page-panel">
    <page-header :title="project?.display_name || projectName">
      <template #subtitle>
        <span class="page-meta">
          <span class="mono">{{ project?.name || projectName }}</span>
          <template v-if="project">
            <span class="page-meta__sep">·</span>
            <span
              class="status-pill"
              :class="project.visibility === 'public' ? 'status-ok' : 'status-muted'"
            >
              {{
                project.visibility === 'public'
                  ? t('projects.visibilityPublic')
                  : t('projects.visibilityPrivate')
              }}
            </span>
            <span class="page-meta__sep">·</span>
            <span>{{ t('projectDetail.repoCount', { count: project.repo_count }) }}</span>
          </template>
        </span>
      </template>

      <template #actions>
        <el-button :loading="loading" @click="load">{{ t('common.refresh') }}</el-button>
        <!-- 只有项目管理员 / 系统管理员能编辑项目。 -->
        <el-button v-if="isProjectAdmin" type="primary" @click="editVisible = true">
          {{ t('projectDetail.editProject') }}
        </el-button>
        <!-- 项目令牌的管理接口要求项目管理员权限，普通成员点进去必然 403，
             所以入口只对项目管理员显示。 -->
        <el-button v-if="isProjectAdmin" @click="goToTokens">
          {{ t('projectTokens.title') }}
        </el-button>
      </template>
    </page-header>

    <div class="section-title" style="margin-bottom: var(--space-3)">
      <span class="page-title" style="font-size: var(--fs-md)">
        {{ t('projectDetail.membersTitle', { count: members.length }) }}
      </span>
      <el-button v-if="isProjectAdmin" type="primary" size="small" @click="openDialog">
        {{ t('projectDetail.addMember') }}
      </el-button>
      <!-- 不是项目管理员时说明为什么没有按钮，而不是给一个必然 403 的按钮。 -->
      <span v-else class="form-hint" style="margin-top: 0">{{ t('projectDetail.requiresAdmin') }}</span>
    </div>

    <el-table
      v-loading="loading"
      :data="members"
      style="width: 100%"
      :empty-text="t('projectDetail.empty')"
    >
      <el-table-column prop="username" :label="t('login.username')" width="180">
        <template #default="{ row }">
          <span class="mono" style="color: var(--text); font-weight: 600">{{ row.username }}</span>
        </template>
      </el-table-column>

      <!-- 显示名可能很长，让它跟着容器伸缩 -->
      <el-table-column
        prop="display_name"
        :label="t('projects.columnDisplayName')"
        min-width="240"
        show-overflow-tooltip
      />

      <el-table-column :label="t('common.role')" width="110">
        <template #default="{ row }">
          <span class="status-pill" :class="ROLE_TONE[row.role as ProjectRole] || 'status-muted'">
            {{ t(`roles.${row.role as ProjectRole}`) }}
          </span>
        </template>
      </el-table-column>

      <el-table-column :label="t('common.actions')" width="96" align="right">
        <template #default="{ row }">
          <el-button v-if="isProjectAdmin" link type="danger" @click="remove(row as ProjectMember, projectName)">
            {{ t('projectDetail.remove') }}
          </el-button>
          <span v-else class="cell-mono">-</span>
        </template>
      </el-table-column>
    </el-table>

    <!-- 保留策略不在这里配：它有自己的页面（/project-retention/<项目名>），
         入口在项目列表页每一行的「保留策略」按钮，
         按项目找、按项目改，不必跨项目看一遍。 -->

    <member-dialog
      v-model="dialogVisible"
      v-model:username="form.username"
      v-model:role="form.role"
      :candidates-loading="candidatesLoading"
      :new-candidates="newCandidates"
      :existing-candidates="existingCandidates"
      :submitting="submitting"
      @submit="submit"
    />

    <project-edit-dialog v-model="editVisible" :project="project" @updated="onEdited" />
  </div>
</template>
