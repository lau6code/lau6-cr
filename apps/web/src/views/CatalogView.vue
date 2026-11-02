<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'

import { ElMessage } from 'element-plus'
import { useI18n } from 'vue-i18n'

import { errorMessage } from '@/api/client'
import { listProjects } from '@/api/projects'
import { deleteRepository, fetchOverview } from '@/api/registry'
import DeleteConfirmDialog from '@/components/DeleteConfirmDialog.vue'
import ImageRef from '@/components/ImageRef.vue'
import PageHeader from '@/components/PageHeader.vue'
import type { Repository } from '@/types'
import { formatBytes, formatDateTime } from '@/utils/format'

const route = useRoute()
const router = useRouter()
const { t } = useI18n()

const loading = ref(false)
const loadError = ref('')
const repositories = ref<Repository[]>([])
const keyword = ref('')
/** 项目过滤：空串 = 不限项目 */
const projectFilter = ref('')
/**
 * 当前账号能看到的项目名（来自项目列表接口）。
 *
 * 与仓库列表分开存：仓库列表是**按可见性过滤**的，
 * 一个仓库都看不到的项目不会出现在里面——但它是这个账号的合法过滤项。
 */
const projectNames = ref<string[]>([])
/**
 * 项目清单读取失败的原因。非空时必须显示：
 * 否则下拉框会**少掉几个项目**，用户会以为那些项目不存在。
 */
const projectError = ref('')

/** 待删除的镜像（整镜像删除 = 删掉它下面所有标签）。 */
const pendingDelete = ref<Repository | null>(null)
const deleteConfirmVisible = ref(false)
const deleting = ref(false)

/**
 * 仓库标签页的地址。
 *
 * 交给浏览器去开新标签页，所以这里只负责把路由解析成 href。
 */
function repoHref(repo: string): string {
  return router.resolve({ name: 'repository-tags', params: { name: repo } }).href
}

/**
 * URL 上的 ?project= 是项目过滤的唯一来源。
 *
 * 「项目」页点项目名 / 「查看」跳过来时带上项目名，这里把它落到下拉框上；
 * 用户自己改下拉框时再同步回 URL，这样链接可以分享，前进/后退也有效。
 */
watch(
  () => route.query.project,
  (value) => {
    projectFilter.value = typeof value === 'string' ? value : ''
  },
  { immediate: true },
)

// 下拉框改了 → 回写 URL。用 replace，别把每次微调都塞进浏览历史。
watch(projectFilter, (value) => {
  const current = typeof route.query.project === 'string' ? route.query.project : ''
  if (value === current) return
  const query: Record<string, string> = {}
  for (const [key, val] of Object.entries(route.query)) {
    if (key === 'project') continue
    if (typeof val === 'string') query[key] = val
  }
  if (value) query.project = value
  void router.replace({ path: route.path, query })
})

/**
 * 可选项目 = 项目清单接口返回的项目 ∪ 可见仓库里出现过的项目。
 *
 * 只从仓库反推是不够的：仓库列表按可见性过滤，
 * 一个仓库都看不到的项目（还没推镜像、或者仓库被 repo_patterns 限掉）
 * 就会从下拉框里消失，用户会以为那个项目不存在。
 */
const projectOptions = computed(() => {
  const names = new Set<string>(projectNames.value)
  for (const repo of repositories.value) {
    if (repo.project) names.add(repo.project)
  }
  // 当前选中的项目即使一个可见仓库都没有，也留在选项里：
  // 从「项目」页跳过来时 URL 带的就是它，
  // 少了它下拉框会显示成一个"没有对应项"的裸值。
  if (projectFilter.value) names.add(projectFilter.value)
  return Array.from(names).sort()
})

const filtered = computed(() => {
  const key = keyword.value.trim().toLowerCase()
  const project = projectFilter.value
  return repositories.value.filter((repo) => {
    if (project && repo.project !== project) return false
    if (key && !repo.name.toLowerCase().includes(key)) return false
    return true
  })
})

async function load(): Promise<void> {
  loading.value = true
  loadError.value = ''
  projectError.value = ''
  try {
    const data = await fetchOverview()
    repositories.value = data.repositories ?? []
  } catch (err) {
    loadError.value = errorMessage(err, t('catalog.loadFailed'))
  }
  // 项目清单单独取：它决定下拉框有哪些选项，
  // 挂了却静默继续 = 下拉框少几个项目而用户完全不知道。
  try {
    const data = await listProjects()
    projectNames.value = data.projects.map((p) => p.name)
  } catch (err) {
    projectError.value = errorMessage(err, t('projects.loadFailed'))
    projectNames.value = []
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  void load()
})

function askDelete(repo: Repository): void {
  pendingDelete.value = repo
  deleteConfirmVisible.value = true
}

async function confirmDelete(): Promise<void> {
  const repo = pendingDelete.value
  if (!repo) return
  deleting.value = true
  try {
    const res = await deleteRepository(repo.name)
    ElMessage.success(t('catalog.deleted', { name: repo.name, count: res?.deleted ?? 0 }))
    pendingDelete.value = null
    deleteConfirmVisible.value = false
    await load()
    // 收尾失败：镜像删掉了，但目录里可能残留空条目——必须说出来。
    if (res?.cleanup_warning) {
      ElMessage.warning(t('catalog.cleanupWarning', { reason: res.cleanup_warning }))
    }
  } catch (err) {
    ElMessage.error(errorMessage(err, t('catalog.deleteFailed')))
  } finally {
    deleting.value = false
  }
}
</script>

<template>
  <div class="page-panel">
    <page-header
      :title="t('route.catalog')"
      :subtitle="t('catalog.subtitle', { count: repositories.length })"
    >
      <template #actions>
        <el-button :loading="loading" @click="load">{{ t('common.refresh') }}</el-button>
      </template>
    </page-header>

    <el-alert
      v-if="loadError"
      type="error"
      :title="loadError"
      show-icon
      :closable="false"
      style="margin-bottom: var(--space-4)"
    />

    <!-- 项目清单取不到时下拉框会缺项目，必须让用户看见原因。 -->
    <el-alert
      v-if="projectError"
      type="warning"
      :title="projectError"
      show-icon
      :closable="false"
      style="margin-bottom: var(--space-4)"
    />

    <div class="toolbar">
      <!-- 项目过滤：镜像仓库住在项目里，按项目切比按名字搜更快 -->
      <el-select
          v-model="projectFilter"
          :placeholder="t('catalog.filterByProject')"
          clearable
          class="toolbar__select"
      >
        <el-option v-for="name in projectOptions" :key="name" :label="name" :value="name" />
      </el-select>
      <el-input
        v-model="keyword"
        :placeholder="t('catalog.filterByRepo')"
        clearable
        class="form-control form-control--sm"
      />
      <span class="toolbar__spacer" />
      <span class="toolbar__count">
        {{ t('catalog.matched', { matched: filtered.length, total: repositories.length }) }}
      </span>
    </div>

    <el-table
      v-loading="loading"
      :data="filtered"
      style="width: 100%"
      :empty-text="t('catalog.empty')"
    >
      <el-table-column :label="t('catalog.columnRepo')" min-width="180">
        <template #default="{ row }">
          <!-- 点镜像名新标签页打开它的标签页（每个标签带 docker pull 命令）。
               用真 <a> 而不是 button：它是导航，浏览器的中键/ctrl+点击、
               右键"复制链接地址"都得按真链接来。 -->
          <a
            class="repo-link"
            :href="repoHref(row.name)"
            target="_blank"
            rel="noopener noreferrer"
          >
            <ImageRef :value="row.name" />
          </a>
        </template>
      </el-table-column>

      <el-table-column prop="latest_tag" :label="t('catalog.columnLatestTag')" width="100">
        <template #default="{ row }">
          <span v-if="row.latest_tag" class="cell-mono" style="color: var(--primary); font-weight: 600">
            {{ row.latest_tag }}
          </span>
          <span v-else class="cell-mono">-</span>
        </template>
      </el-table-column>

      <el-table-column prop="tag_count" :label="t('catalog.columnTagCount')" width="60" align="right">
        <template #default="{ row }">
          <span class="cell-num">{{ row.tag_count }}</span>
        </template>
      </el-table-column>

      <el-table-column prop="size" :label="t('projects.columnSize')" width="80" align="right">
        <template #default="{ row }">
          <span class="cell-num">{{ formatBytes(row.size) }}</span>
        </template>
      </el-table-column>

      <el-table-column prop="created" :label="t('common.createdAt')" width="150">
        <template #default="{ row }">
          <span class="cell-mono">{{ formatDateTime(row.created) }}</span>
        </template>
      </el-table-column>

      <!-- 平台列定宽：只有"仓库"列跟着容器伸缩，
           否则大屏上多出来的宽度会把这个窄字符串的列撑得老宽。 -->
      <el-table-column :label="t('catalog.columnPlatform')" width="150">
        <template #default="{ row }">
          <!-- 一行一个平台标签会换行，把行高撑得参差不齐；
               这里用单行等宽串 + 省略号，行高始终一致。 -->
          <span
            v-if="row.platforms && row.platforms.length"
            class="cell-platforms"
            :title="row.platforms.join(t('common.listSeparator'))"
          >
            {{ row.platforms.join(' · ') }}
          </span>
          <span v-else class="cell-mono">-</span>
        </template>
      </el-table-column>

      <el-table-column :label="t('common.actions')" width="90" align="right" fixed="right">
        <template #default="{ row }">
          <el-button link type="danger" @click="askDelete(row as Repository)">
            {{ t('common.delete') }}
          </el-button>
        </template>
      </el-table-column>
    </el-table>

    <delete-confirm-dialog
      v-model="deleteConfirmVisible"
      :title="t('catalog.deleteTitle')"
      :target="pendingDelete?.name ?? ''"
      :detail="
        pendingDelete
          ? t('catalog.deleteDetail', { count: pendingDelete.tag_count })
          : ''
      "
      :loading="deleting"
      @confirm="confirmDelete"
    />
  </div>
</template>
