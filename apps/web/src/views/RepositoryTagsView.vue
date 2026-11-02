<script setup lang="ts">
/**
 * 仓库标签页。
 *
 * 镜像目录页点仓库名后**新标签页**打开这里：列出该仓库的全部 tag，
 * 每个 tag 给出创建时间与可直接粘贴执行的 `docker pull` 命令，
 * 也可以就地删除单个标签。
 *
 * 仓库名放在路径里（/repository/<仓库名>）。仓库名可能带 "/"
 * （形如 team-a/sub/app），路由用 catch-all 参数接住，
 * vue-router 解码后还原成完整仓库名。
 */
import { computed, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'

import { ElMessage } from 'element-plus'
import { DocumentCopy } from '@element-plus/icons-vue'
import { useI18n } from 'vue-i18n'

import { errorMessage } from '@/api/client'
import { deleteTag, fetchTags } from '@/api/registry'
import DeleteConfirmDialog from '@/components/DeleteConfirmDialog.vue'
import ImageRef from '@/components/ImageRef.vue'
import PageHeader from '@/components/PageHeader.vue'
import { copyAndNotify } from '@/utils/clipboard'
import { formatBytes, formatDateTime } from '@/utils/format'
import type { TagInfo } from '@/types'

const route = useRoute()
const router = useRouter()
const { t } = useI18n()

/** 完整仓库名（形如 team-a/myapp）；空串说明地址不完整。 */
const repo = computed(() => String(route.params.name ?? '').trim())

const loading = ref(false)
const loadError = ref('')
const tags = ref<TagInfo[]>([])
/** 拼 docker pull 命令用的前缀（host[:port]）。 */
const pullPrefix = ref('')

/** 待删除的标签（null = 没有待删的）。 */
const pendingDelete = ref<TagInfo | null>(null)
/** 删除确认框是否打开。 */
const confirmVisible = ref(false)
const deleting = ref(false)

/**
 * 实际用的前缀：后端给的优先，没有就用当前地址。
 *
 * 与 Harbor 的做法一致：拿不到对外地址时，浏览器当前地址总能拼出一条可用的命令。
 */
const prefix = computed(() => {
  const given = pullPrefix.value.trim()
  if (given) return given
  return window.location.host
})

function pullCommand(tag: string): string {
  return `docker pull ${prefix.value}/${repo.value}:${tag}`
}

async function load(): Promise<void> {
  // 地址里没有仓库名：直接说清楚，别渲染一张空表。
  if (!repo.value) {
    loadError.value = t('repository.missingRepo')
    tags.value = []
    return
  }
  loading.value = true
  loadError.value = ''
  try {
    const data = await fetchTags(repo.value)
    tags.value = data?.tags ?? []
    pullPrefix.value = data?.pull_prefix ?? ''
  } catch (err) {
    loadError.value = errorMessage(err, t('repository.loadFailed'))
    tags.value = []
  } finally {
    loading.value = false
  }
}

// 首次进入与切换仓库（同一路由不同参数）都要重新拉。
watch(
  repo,
  () => {
    void load()
  },
  { immediate: true },
)

async function copy(command: string): Promise<void> {
  await copyAndNotify(command)
}

function askDelete(tag: TagInfo): void {
  pendingDelete.value = tag
  confirmVisible.value = true
}

async function confirmDelete(): Promise<void> {
  const tag = pendingDelete.value
  if (!tag || !repo.value) return
  deleting.value = true
  try {
    // 按 digest 删（registry 规范）：同一个 digest 可能有多个 tag 指向它，
    // 按 tag 名删会误伤别的 tag。
    const res = await deleteTag(repo.value, tag.digest)
    ElMessage.success(t('repository.deleted', { tag: tag.tag }))
    pendingDelete.value = null
    confirmVisible.value = false
    await load()
    // 收尾失败：删除本身成功了，但目录可能残留空条目——必须说出来。
    if (res?.cleanup_warning) {
      ElMessage.warning(t('repository.cleanupWarning', { reason: res.cleanup_warning }))
    }
  } catch (err) {
    ElMessage.error(errorMessage(err, t('repository.deleteFailed')))
  } finally {
    deleting.value = false
  }
}

function backToCatalog(): void {
  void router.push('/')
}
</script>

<template>
  <div class="page-panel">
    <page-header :title="repo || t('route.repositoryTags')">
      <template #title>
        <ImageRef v-if="repo" :value="repo" />
        <template v-else>{{ t('route.repositoryTags') }}</template>
      </template>

      <template #subtitle>
        <span>{{ t('repository.tagsCount', { count: tags.length }) }}</span>
      </template>

      <template #actions>
        <el-button @click="backToCatalog">{{ t('repository.backToCatalog') }}</el-button>
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

    <el-table
      v-loading="loading"
      :data="tags"
      style="width: 100%"
      :empty-text="t('repository.empty')"
    >
      <el-table-column prop="tag" :label="t('repository.columnTag')" width="110">
        <template #default="{ row }">
          <span class="cell-mono">{{ row.tag }}</span>
        </template>
      </el-table-column>

      <el-table-column :label="t('common.createdAt')" width="150">
        <template #default="{ row }">
          <span class="cell-mono">{{ formatDateTime(row.created) }}</span>
        </template>
      </el-table-column>

      <!-- 平台一行一个会换行，把行高撑得参差不齐；
           这里用单行等宽串 + 省略号，行高始终一致。 -->
      <el-table-column :label="t('catalog.columnPlatform')" width="150">
        <template #default="{ row }">
          <span
            v-if="row.platforms && row.platforms.length"
            class="cell-platforms"
            :title="row.platforms.join('、')"
          >
            {{ row.platforms.join(' · ') }}
          </span>
          <span v-else class="cell-mono">-</span>
        </template>
      </el-table-column>

      <!-- 读取失败的 tag 没有大小信息：显示 "-" 而不是 0 B——
           后者会被读成"这个镜像确实是空的"，那是编造出来的假值。 -->
      <el-table-column :label="t('projects.columnSize')" width="90" align="right">
        <template #default="{ row }">
          <span v-if="row.error" class="cell-mono">-</span>
          <span v-else class="cell-num">{{ formatBytes(row.size) }}</span>
        </template>
      </el-table-column>

      <!-- 拉取失败 / 度量失败的标签要**显式标出来**：
           后端给的是 error 字段，页面不能把它显示成"这个 tag 没有这些信息"——
           两者在字段上长得一模一样，用户会以为仓库本来就是空的。 -->
      <el-table-column :label="t('common.status')" width="110">
        <template #default="{ row }">
          <el-tooltip v-if="row.error" :content="row.error" placement="top">
            <el-tag type="warning" size="small" effect="plain">{{ t('repository.readFailed') }}</el-tag>
          </el-tooltip>
          <span v-else class="cell-muted">{{ t('repository.ok') }}</span>
        </template>
      </el-table-column>

      <!-- 拉取命令很长：给它固定宽度，否则会被挤成省略号；
           省略时用 title 悬停看全，复制按钮复制的是完整命令。 -->
      <el-table-column :label="t('repository.columnPullCommand')" min-width="340">
        <template #default="{ row }">
          <span class="cell-mono cell-command" :title="pullCommand(row.tag)">
            {{ pullCommand(row.tag) }}
          </span>
        </template>
      </el-table-column>

      <el-table-column :label="t('common.actions')" width="90" align="right" fixed="right">
        <template #default="{ row }">
          <!-- 复制只有一个动作，做成图标按钮：文字按钮在这个列里太占地方。 -->
          <el-tooltip :content="t('repository.copyPullCommand')" placement="top">
            <el-button
              link
              type="primary"
              :icon="DocumentCopy"
              :aria-label="t('repository.copyPullCommand')"
              @click="copy(pullCommand(row.tag))"
            />
          </el-tooltip>
          <!-- 拿不到摘要就没法删（registry 规范只认 digest）：
               把原因摆在明面上，别让用户点了才吃一个 400。 -->
          <el-tooltip
            v-if="!row.digest"
            :content="row.error || t('repository.noDigest')"
            placement="top"
          >
            <span><el-button link type="danger" disabled>{{ t('common.delete') }}</el-button></span>
          </el-tooltip>
          <el-button v-else link type="danger" @click="askDelete(row as TagInfo)">
            {{ t('common.delete') }}
          </el-button>
        </template>
      </el-table-column>
    </el-table>

    <p v-if="tags.length" class="form-hint" style="margin-top: var(--space-3)">
      {{ t('repository.pullHint') }}
      <code>docker login {{ prefix }}</code>。
    </p>

    <delete-confirm-dialog
      v-model="confirmVisible"
      :title="t('repository.deleteTitle')"
      :target="pendingDelete ? `${repo}:${pendingDelete.tag}` : ''"
      :detail="t('repository.deleteDetail')"
      :loading="deleting"
      @confirm="confirmDelete"
    />
  </div>
</template>
