import { request } from './client'
import type {
  Id,
  MemberCandidate,
  Project,
  ProjectDetail,
  ProjectMembership,
  ProjectRole,
  ProjectToken,
  ProjectTokenCreated,
  ProjectTokenExpiry,
  ProjectTokenRole,
  ProjectVisibility,
} from '@/types'

interface CreateProjectPayload {
  name: string
  display_name: string
  visibility: ProjectVisibility
}

/**
 * 编辑项目的请求体。
 *
 * 字段都是可选的：只带要改的字段过去，后端按 PATCH 语义处理。
 * `visibility` 只有系统管理员能改（带了会被后端 403 拒绝），
 * 所以项目管理员提交时**不要**带这个字段。
 */
export interface UpdateProjectPayload {
  display_name?: string
  description?: string
  visibility?: ProjectVisibility
}

interface AddMemberPayload {
  username: string
  role: ProjectRole
}

/**
 * 生成项目令牌的请求体。
 *
 * 角色**只能在这里给一次**——后端 PATCH 时带 role 会直接 400，
 * 因为角色就是这枚令牌的权限，改它等于换了一枚凭据。
 *
 * 失效策略三选一：`{never_expires: true}` 永久、`{expires_at}` 指定时刻、
 * 都不给走系统默认。
 */
interface CreateProjectTokenPayload {
  name: string
  role: ProjectTokenRole
  never_expires?: boolean
  expires_at?: number | null
}

export function listProjects(): Promise<{ projects: Project[] }> {
  return request<{ projects: Project[] }>('/api/v1/projects')
}

export function createProject(payload: CreateProjectPayload): Promise<{ project: Project }> {
  return request<{ project: Project }>('/api/v1/projects', {
    method: 'POST',
    body: payload,
  })
}

/**
 * 删除项目。
 *
 * 权限：仅系统管理员（后端 requireAdmin 强制）。
 */
export function deleteProject(name: string): Promise<Record<string, never>> {
  return request<Record<string, never>>(
    `/api/v1/projects/${encodeURIComponent(name)}`,
    { method: 'DELETE' },
  )
}

export function getProject(name: string): Promise<ProjectDetail> {
  return request<ProjectDetail>(`/api/v1/projects/${encodeURIComponent(name)}`)
}

/**
 * 编辑项目。
 *
 * 权限：项目管理员或系统管理员（后端 requireProjectAdmin 中间件强制）。
 */
export function updateProject(
  name: string,
  payload: UpdateProjectPayload,
): Promise<{ project: Project }> {
  return request<{ project: Project }>(`/api/v1/projects/${encodeURIComponent(name)}`, {
    method: 'PATCH',
    body: payload,
  })
}

export function addMember(name: string, payload: AddMemberPayload): Promise<Record<string, never>> {
  return request<Record<string, never>>(
    `/api/v1/projects/${encodeURIComponent(name)}/members`,
    { method: 'POST', body: payload },
  )
}

export function removeMember(name: string, userId: Id): Promise<Record<string, never>> {
  return request<Record<string, never>>(
    `/api/v1/projects/${encodeURIComponent(name)}/members/${encodeURIComponent(String(userId))}`,
    { method: 'DELETE' },
  )
}

export function listProjectMembers(): Promise<{ members: ProjectMembership[] }> {
  return request<{ members: ProjectMembership[] }>('/api/v1/project-members')
}

export function listMemberCandidates(
  name: string,
): Promise<{ candidates: MemberCandidate[] }> {
  return request<{ candidates: MemberCandidate[] }>(
    `/api/v1/projects/${encodeURIComponent(name)}/members/candidates`,
  )
}

/**
 * 列出某个项目的项目令牌。
 *
 * `default_ttl_seconds` 是系统默认有效期——不指定失效时间时用它。
 */
export function listProjectTokens(name: string): Promise<{
  tokens: ProjectToken[]
  default_ttl_seconds: number
}> {
  return request<{ tokens: ProjectToken[]; default_ttl_seconds: number }>(
    `/api/v1/projects/${encodeURIComponent(name)}/tokens`,
  )
}

/**
 * 生成一枚项目令牌。
 *
 * ⚠️ 响应里的 `token` 是**原始令牌**，只此一次——
 *    后端库里只存散列，刷新页面就再也取不回来了。
 */
export function createProjectToken(
  name: string,
  payload: CreateProjectTokenPayload,
): Promise<ProjectTokenCreated> {
  return request<ProjectTokenCreated>(`/api/v1/projects/${encodeURIComponent(name)}/tokens`, {
    method: 'POST',
    body: payload,
  })
}

/**
 * 改一枚项目令牌的失效时间（只能改本项目的）。
 *
 * 令牌本体不变——散列不动，CI 里已配好的凭据不用重配。
 */
export function updateProjectTokenExpiry(
  name: string,
  id: Id,
  expiry: ProjectTokenExpiry,
): Promise<Record<string, never>> {
  return request<Record<string, never>>(
    `/api/v1/projects/${encodeURIComponent(name)}/tokens/${encodeURIComponent(String(id))}`,
    { method: 'PATCH', body: { ...expiry } },
  )
}

/** 只能吊销本项目的。 */
export function revokeProjectToken(name: string, id: Id): Promise<Record<string, never>> {
  return request<Record<string, never>>(
    `/api/v1/projects/${encodeURIComponent(name)}/tokens/${encodeURIComponent(String(id))}`,
    { method: 'DELETE' },
  )
}
