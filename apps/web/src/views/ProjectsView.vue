<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import type { FormInstance, FormRules } from 'element-plus'
import { useI18n } from 'vue-i18n'

import { errorMessage } from '@/api/client'
import { createProject, deleteProject, listProjects } from '@/api/projects'
import DeleteConfirmDialog from '@/components/DeleteConfirmDialog.vue'
import PageHeader from '@/components/PageHeader.vue'
import ProjectEditDialog from '@/components/ProjectEditDialog.vue'
import { useAuthStore } from '@/stores/auth'
import type { Project, ProjectVisibility } from '@/types'
import { formatBytes } from '@/utils/format'
import { mergeProject } from '@/utils/project'
import { formLabelWidth } from '@/i18n'

const router = useRouter()
const auth = useAuthStore()
const { t } = useI18n()

const loading = ref(false)
const projects = ref<Project[]>([])

const dialogVisible = ref(false)
const submitting = ref(false)
const formRef = ref<FormInstance>()

const editVisible = ref(false)
const editingProject = ref<Project | null>(null)

const deleteVisible = ref(false)
const deletingProject = ref<Project | null>(null)
const deleting = ref(false)

const form = reactive({
  name: '',
  display_name: '',
  visibility: 'private' as ProjectVisibility,
})

const rules = computed<FormRules>(() => ({
  name: [
    { required: true, message: t('projects.formNameRequired'), trigger: 'blur' },
    {
      pattern: /^[a-z0-9][a-z0-9._-]*$/i,
      message: t('projects.formNamePattern'),
      trigger: 'blur',
    },
  ],
  display_name: [
    { required: true, message: t('projects.formDisplayNameRequired'), trigger: 'blur' },
  ],
}))

async function load(): Promise<void> {
  loading.value = true
  try {
    const data = await listProjects()
    projects.value = data.projects ?? []
  } catch (err) {
    ElMessage.error(errorMessage(err, t('projects.loadFailed')))
  } finally {
    loading.value = false
  }
}

function openDialog(): void {
  form.name = ''
  form.display_name = ''
  form.visibility = 'private'
  dialogVisible.value = true
}

async function submit(): Promise<void> {
  if (!formRef.value) return
  const valid = await formRef.value.validate().catch(() => false)
  if (!valid) return

  submitting.value = true
  try {
    const result = await createProject({
      name: form.name.trim(),
      display_name: form.display_name.trim(),
      visibility: form.visibility,
    })
    ElMessage.success(t('projects.created', { name: result.project.name }))
    dialogVisible.value = false
    await load()
  } catch (err) {
    ElMessage.error(errorMessage(err, t('projects.createFailed')))
  } finally {
    submitting.value = false
  }
}

/**
 * 跳到镜像目录，并把这个项目选中。
 *
 * 项目过滤通过 URL 上的 ?project= 传递，镜像目录页自己读它，
 * 这样链接可以分享，前进/后退也能回到对应的过滤状态。
 *
 * ⚠️ 项目详情页（成员管理）是另一条路——项目名链接走那条。
 */
function openCatalog(project: Project): void {
  void router.push({ path: '/', query: { project: project.name } })
}

/**
 * 进某个项目的保留策略页。
 *
 * 入口放在项目列表的每一行：保留策略是「某个项目怎么清理」的设置，
 * 跟着项目走更顺，也不用再单独维护一个跨项目的一览页。
 */
function openRetention(project: Project): void {
  void router.push({ name: 'project-retention-detail', params: { name: project.name } })
}

/**
 * 进某个项目的访问令牌页。
 *
 * 入口放在项目列表的每一行：令牌是**项目身份**的长期凭据，
 * 管理它就离不开项目上下文——从项目列表直接进比先点进项目详情再找一遍顺。
 */
function openTokens(project: Project): void {
  void router.push({ name: 'project-tokens', params: { name: project.name } })
}

/**
 * 当前用户能不能编辑这个项目。
 *
 * 与后端 isProjectAdmin 对齐：系统管理员对所有项目都有项目管理员权限；
 * 其余人只有在自己担任 admin 的项目里才能编辑。
 *
 * my_role 由后端按请求主体算好（项目列表 / 项目详情都带这个字段）。
 */
function canEdit(project: Project): boolean {
  return auth.isSystemAdmin || project.my_role === 'admin'
}

function openEdit(project: Project): void {
  editingProject.value = project
  editVisible.value = true
}

/** 打开删除确认框。删除项目仅系统管理员可做，后端同样强制。 */
function openDelete(project: Project): void {
  deletingProject.value = project
  deleteVisible.value = true
}

async function confirmDelete(): Promise<void> {
  const target = deletingProject.value
  if (!target) return
  deleting.value = true
  try {
    await deleteProject(target.name)
    ElMessage.success(t('projects.deleted', { name: target.name }))
    deleteVisible.value = false
    deletingProject.value = null
    await load()
  } catch (err) {
    ElMessage.error(errorMessage(err, t('projects.deleteFailed')))
  } finally {
    deleting.value = false
  }
}

async function onEdited(updated: Project): Promise<void> {
  // 用服务端返回的最新项目替换列表里那一行（显示名、描述、可见性都可能变了）。
  const idx = projects.value.findIndex((p) => p.name === updated.name)
  if (idx < 0) return
  projects.value[idx] = mergeProject(projects.value[idx], updated)
}

onMounted(() => {
  void load()
})
</script>

<template>
  <div class="page-panel">
    <page-header :title="t('projects.title')" :subtitle="t('projects.subtitle')">
      <template #actions>
        <!-- 创建项目是系统级操作：项目名占 Docker 顶层命名空间的一段，
             只有系统管理员能创建。后端同样强制（403）。 -->
        <el-button v-if="auth.isSystemAdmin" type="primary" @click="openDialog">
          {{ t('projects.create') }}
        </el-button>
        <el-tag v-else type="info" effect="plain">{{ t('projects.createRequiresAdmin') }}</el-tag>
      </template>
    </page-header>

    <el-table
      v-loading="loading"
      :data="projects"
      style="width: 100%"
      :empty-text="t('projects.empty')"
    >
      <el-table-column prop="name" :label="t('projects.columnName')" width="160">
        <template #default="{ row }">
          <!-- 项目名进项目详情页（成员管理在那里）。
               用 RouterLink 而不是 span + click：站内跳转得能被中键/ctrl+点击、
               右键复制链接，浏览器的前进/后退也要正常工作。 -->
          <RouterLink
            class="img-ref project-link"
            :to="{ name: 'project-detail', params: { name: row.name } }"
          >
            <span class="img-ref__name">{{ row.name }}</span>
          </RouterLink>
        </template>
      </el-table-column>

      <!-- 显示名是这一行里最长的一列，让它跟着容器伸缩；
           项目名是短标识，定宽即可。 -->
      <el-table-column
        prop="display_name"
        :label="t('projects.columnDisplayName')"
        min-width="220"
        show-overflow-tooltip
      />

      <el-table-column :label="t('projects.columnVisibility')" width="110">
        <template #default="{ row }">
          <span
            class="status-pill"
            :class="row.visibility === 'public' ? 'status-ok' : 'status-muted'"
          >
            {{
              row.visibility === 'public'
                ? t('projects.visibilityPublic')
                : t('projects.visibilityPrivate')
            }}
          </span>
        </template>
      </el-table-column>

      <el-table-column prop="member_count" :label="t('projects.columnMembers')" width="88" align="right">
        <template #default="{ row }">
          <span class="cell-num">{{ row.member_count }}</span>
        </template>
      </el-table-column>

      <el-table-column prop="repo_count" :label="t('projects.columnRepos')" width="88" align="right">
        <template #default="{ row }">
          <span class="cell-num">{{ row.repo_count }}</span>
        </template>
      </el-table-column>

      <!-- 项目内所有仓库的大小之和。 -->
      <el-table-column prop="total_size" :label="t('projects.columnSize')" width="110" align="right">
        <template #default="{ row }">
          <span class="cell-num">{{ formatBytes(row.total_size) }}</span>
        </template>
      </el-table-column>

      <el-table-column :label="t('projects.columnActions')" width="340" align="right" class-name="row-actions">
        <template #default="{ row }">
          <div class="row-actions__inner">
            <!-- 这个项目的镜像在镜像目录里（按该项目过滤）。 -->
            <el-button link type="primary" @click="openCatalog(row as Project)">
              {{ t('projects.actionImages') }}
            </el-button>
            <!-- 只有项目管理员 / 系统管理员能编辑，后端 PATCH 同样会拦。 -->
            <el-button v-if="canEdit(row as Project)" link type="primary" @click="openEdit(row as Project)">
              {{ t('projects.actionEdit') }}
            </el-button>
            <!-- 保留策略的读写接口在 requireProjectAdmin 上，
                 所以入口也只对项目管理员 / 系统管理员显示。 -->
            <el-button
              v-if="canEdit(row as Project)"
              link
              type="primary"
              @click="openRetention(row as Project)"
            >
              {{ t('projects.actionRetention') }}
            </el-button>
            <!-- 项目令牌的管理接口同样在 requireProjectAdmin 上，
                 所以入口也只对项目管理员 / 系统管理员显示。 -->
            <el-button
              v-if="canEdit(row as Project)"
              link
              type="primary"
              @click="openTokens(row as Project)"
            >
              {{ t('projects.actionTokens') }}
            </el-button>
            <!-- 删除项目仅系统管理员可做，后端 DELETE /projects/{name} 同样强制。 -->
            <el-button
              v-if="auth.isSystemAdmin"
              link
              type="danger"
              @click="openDelete(row as Project)"
            >
              {{ t('projects.actionDelete') }}
            </el-button>
          </div>
        </template>
      </el-table-column>
    </el-table>

    <el-dialog v-model="dialogVisible" :title="t('projects.create')" width="480px">
      <!-- 切语言会换掉 rules 对象：别让它顺手触发一次校验，
           否则空表单上会凭空冒出必填错误。 -->
      <el-form
        ref="formRef"
        :model="form"
        :rules="rules"
        :validate-on-rule-change="false"
        :label-width="formLabelWidth(90)"
      >
        <el-form-item :label="t('projects.formName')" prop="name">
          <el-input v-model="form.name" :placeholder="t('projects.formNamePlaceholder')" />
          <div class="form-hint">{{ t('projects.formNameHint') }}</div>
        </el-form-item>
        <el-form-item :label="t('projects.formDisplayName')" prop="display_name">
          <el-input
            v-model="form.display_name"
            :placeholder="t('projects.formDisplayNamePlaceholder')"
          />
        </el-form-item>
        <el-form-item :label="t('projects.formVisibility')" prop="visibility">
          <el-select v-model="form.visibility" class="form-control">
            <el-option :label="t('projects.visibilityPrivateOption')" value="private" />
            <el-option :label="t('projects.visibilityPublicOption')" value="public" />
          </el-select>
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="dialogVisible = false">{{ t('common.cancel') }}</el-button>
        <el-button type="primary" :loading="submitting" @click="submit">
          {{ t('common.create') }}
        </el-button>
      </template>
    </el-dialog>

    <!-- 编辑项目：只有项目管理员 / 系统管理员能看到入口（后端同样强制）。 -->
    <project-edit-dialog
      v-model="editVisible"
      :project="editingProject"
      @updated="onEdited"
    />

    <!-- 删除项目：仅系统管理员可做（后端 DELETE /projects/{name} 同样强制）。 -->
    <delete-confirm-dialog
      v-model="deleteVisible"
      :title="t('projects.deleteTitle')"
      :target="deletingProject?.name ?? ''"
      :detail="t('projects.deleteDetail')"
      :loading="deleting"
      @confirm="confirmDelete"
    />
  </div>
</template>
