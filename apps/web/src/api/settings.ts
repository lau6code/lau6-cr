import { request } from './client'
import type { SettingsPayload } from '@/types'

interface PatchSettingsPayload {
  version: number
  changes: Record<string, unknown>
}

export function fetchSettings(): Promise<SettingsPayload> {
  return request<SettingsPayload>('/api/v1/settings')
}

/** 批量修改，带 version 乐观锁；返回新的 version。 */
export function patchSettings(payload: PatchSettingsPayload): Promise<SettingsPayload> {
  return request<SettingsPayload>('/api/v1/settings', {
    method: 'PATCH',
    body: payload,
  })
}

/**
 * 强制重新导出 application.yaml。
 *
 * 用于"YAML 导出失败"之后的补救：DB 与内存快照是对的，只是文件是旧的。
 */
export function exportConfig(): Promise<{ exported_at: number }> {
  return request<{ exported_at: number }>('/api/v1/config/export', { method: 'POST' })
}

export interface ConfigBackup {
  /** 备份 ID（时间戳），回滚时用它指定目标。 */
  name: string
  /** 备份文件路径。 */
  file: string
  /** 文件字节数。 */
  size: number
  /** 备份时间（unix 秒）。 */
  created_at: number
}

export function listConfigBackups(): Promise<{ backups: ConfigBackup[] }> {
  return request<{ backups: ConfigBackup[] }>('/api/v1/config/backups')
}

/**
 * 回滚到指定备份。
 *
 * ⚠️ 只改配置文件，运行期仍以 system_settings 为准——要生效需重启容器。
 */
export function rollbackConfig(name: string): Promise<{
  path: string
  pending_restart: boolean
  note: string
}> {
  return request<{ path: string; pending_restart: boolean; note: string }>(
    '/api/v1/config/rollback',
    { method: 'POST', body: { name } },
  )
}
