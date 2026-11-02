import { request } from './client'
import type { RegistryDeleteResult, RegistryOverview, RepoTags } from '@/types'

/** 列表页主接口：一次拿全可见仓库，不做 N+1。 */
export function fetchOverview(): Promise<RegistryOverview> {
  return request<RegistryOverview>('/api/v1/registry/overview')
}

/**
 * 某仓库的 tag 列表（含每个 tag 的创建时间/大小/平台/摘要）。
 *
 * 仓库名里可以带 "/"（形如 team-a/sub/app），所以要整体编码：
 * 否则斜杠会被当成路径分隔符，后端切不出仓库名。
 */
export function fetchTags(repo: string): Promise<RepoTags> {
  return request<RepoTags>(`/api/v1/registry/${encodeURIComponent(repo)}/tags`)
}

/**
 * 删除单个 tag。
 *
 * 按 **digest** 删（registry 规范：只能按 digest 删 manifest），
 * 所以要先拿到 tag 的摘要——fetchTags 里带了。
 */
export function deleteTag(repo: string, digest: string): Promise<RegistryDeleteResult> {
  return request<RegistryDeleteResult>(
    `/api/v1/registry/${encodeURIComponent(repo)}/manifests/${encodeURIComponent(digest)}`,
    { method: 'DELETE' },
  )
}

/** 删除整个镜像（该仓库下的所有 tag）。 */
export function deleteRepository(repo: string): Promise<RegistryDeleteResult> {
  return request<RegistryDeleteResult>(`/api/v1/registry/${encodeURIComponent(repo)}`, {
    method: 'DELETE',
  })
}
