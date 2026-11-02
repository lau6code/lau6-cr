import { request } from './client'
import type {
  CleanupRun,
  GCRun,
  GCStatus,
  RetentionPolicy,
  RetentionRule,
  RetentionTemplates,
} from '@/types'

/**
 * 保留策略的写入请求体。
 *
 * 全局默认与项目级用同一个结构（后端两个 PUT 接口的请求体一样）。
 */
export interface RetentionPolicyPayload {
  enabled: boolean
  algorithm: string
  tag_selector: string
  rules: RetentionRule[]
}

interface RunCleanupPayload {
  /** 项目名；空串 = 全部项目。 */
  project?: string
  /** true = 试运行（只评估不删除）；省略时取后端全局设置。 */
  dry_run?: boolean
}

interface CleanupRunsQuery {
  limit?: number
  offset?: number
}

// ── 项目级保留策略 ─────────────────────────────────────────

/**
 * 项目级保留策略的读取结果。
 *
 * policy 是项目自己的策略（没配过时是"关闭且无规则"的骨架），
 * effective 是**实际会被执行**的那条：项目级优先，没有则用全局默认；
 * source 说明生效的是哪一条（"project" | "global"）。
 *
 * 为什么必须把 effective 一起给：项目管理员可能没配项目级策略，
 * 那时生效的是全局默认——只显示骨架会让他以为"这个项目不受清理影响"，
 * 而实际上全局默认正在删它的镜像。
 */
interface ProjectRetention {
  policy: RetentionPolicy
  effective: RetentionPolicy
  source: 'project' | 'global'
}

/**
 * 读取项目级保留策略。
 *
 * 权限：项目管理员或系统管理员（后端 requireProjectAdmin 强制）。
 */
export function getProjectRetention(name: string): Promise<ProjectRetention> {
  return request<ProjectRetention>(
    `/api/v1/projects/${encodeURIComponent(name)}/retention`,
  )
}

/** 覆盖全局默认。 */
export function putProjectRetention(
  name: string,
  payload: RetentionPolicyPayload,
): Promise<{ policy: RetentionPolicy }> {
  return request<{ policy: RetentionPolicy }>(
    `/api/v1/projects/${encodeURIComponent(name)}/retention`,
    { method: 'PUT', body: payload },
  )
}

/** 回落到全局默认。 */
export function deleteProjectRetention(name: string): Promise<Record<string, never>> {
  return request<Record<string, never>>(
    `/api/v1/projects/${encodeURIComponent(name)}/retention`,
    { method: 'DELETE' },
  )
}

// ── 全局默认保留策略与规则模板 ─────────────────────────────

/** 权限：系统管理员。 */
export function getGlobalRetentionPolicy(): Promise<{ policy: RetentionPolicy }> {
  return request<{ policy: RetentionPolicy }>('/api/v1/admin/cleanup/policy')
}

/** 权限：系统管理员。 */
export function putGlobalRetentionPolicy(
  payload: RetentionPolicyPayload,
): Promise<{ policy: RetentionPolicy }> {
  return request<{ policy: RetentionPolicy }>('/api/v1/admin/cleanup/policy', {
    method: 'PUT',
    body: payload,
  })
}

/**
 * 列出全部保留策略：全局默认 + 各项目级覆盖。
 *
 * 权限：系统管理员（后端 /admin 分组强制）。
 */
export function listRetentionPolicies(): Promise<{ policies: RetentionPolicy[] }> {
  return request<{ policies: RetentionPolicy[] }>('/api/v1/admin/cleanup/policies')
}

/**
 * 可用的规则模板清单（前端用它渲染规则编辑器，不硬编码模板列表）。
 *
 * ⚠️ 路径**不在 /admin 分组下**：它是静态的规则元数据（不含任何用户数据），
 * 但**项目管理员必须能读到**——否则他们配不了项目级规则。
 * 放在 /admin 里会让项目管理员必然 403（03-registry.md §5.2.1）。
 */
export function listRetentionTemplates(): Promise<RetentionTemplates> {
  return request<RetentionTemplates>('/api/v1/retention/templates')
}

// ── 手动执行与执行历史 ─────────────────────────────────────

/**
 * 手动执行一次清理。
 *
 * dry_run 省略时后端取全局设置（默认 true = 试运行），
 * 所以这里总是把它显式带上，避免"以为在删、其实在试"。
 */
export function runCleanup(payload: RunCleanupPayload): Promise<{ run: CleanupRun }> {
  return request<{ run: CleanupRun }>('/api/v1/admin/cleanup/run', {
    method: 'POST',
    body: payload,
  })
}

export function listCleanupRuns(
  query: CleanupRunsQuery = {},
): Promise<{ runs: CleanupRun[] }> {
  return request<{ runs: CleanupRun[] }>('/api/v1/admin/cleanup/runs', {
    query: {
      limit: query.limit ?? 50,
      offset: query.offset ?? 0,
    },
  })
}

export function getCleanupRun(id: number | string): Promise<{ run: CleanupRun }> {
  return request<{ run: CleanupRun }>(
    `/api/v1/admin/cleanup/runs/${encodeURIComponent(String(id))}`,
  )
}

// ── blob 回收（第 ② 层）────────────────────────────────────

interface RunGCPayload {
  /** true = 试运行（只评估不删除）；省略时取后端全局设置。 */
  dry_run?: boolean
}

export function getGCStatus(): Promise<GCStatus> {
  return request<GCStatus>('/api/v1/admin/cleanup/gc/status')
}

/**
 * 手动执行一次 blob 回收。
 *
 * dry_run 省略时后端取全局设置（默认 true = 试运行）。
 */
export function runGC(payload: RunGCPayload = {}): Promise<{ run: GCRun }> {
  return request<{ run: GCRun }>('/api/v1/admin/cleanup/gc', {
    method: 'POST',
    body: payload,
  })
}

export function listGCRuns(query: CleanupRunsQuery = {}): Promise<{ runs: GCRun[] }> {
  return request<{ runs: GCRun[] }>('/api/v1/admin/cleanup/gc/runs', {
    query: {
      limit: query.limit ?? 50,
      offset: query.offset ?? 0,
    },
  })
}

