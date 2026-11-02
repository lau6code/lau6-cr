/**
 * 清理执行明细的解析。
 *
 * detail 是**可选的增强信息**（后端把它当字符串存）——没有明细不是错误；
 * 但**解析失败**是错误：明细是"为什么没删干净"的唯一来源，
 * 静默退化成空表会让用户以为"这次什么都没删"。
 */
import type { CleanupDetail, CleanupRun } from '@/types'

export interface ParsedCleanupDetail {
  /** 解析出来的明细；解析失败时为空。 */
  detail: CleanupDetail
  /** 解析失败的原因；空串 = 没出错。 */
  error: string
}

export function parseCleanupDetail(
  run: CleanupRun | null,
  invalidMessage: string,
): ParsedCleanupDetail {
  if (!run?.detail) return { detail: {}, error: '' }
  try {
    const parsed: unknown = JSON.parse(run.detail)
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
      return { detail: {}, error: invalidMessage }
    }
    return { detail: parsed as CleanupDetail, error: '' }
  } catch {
    return { detail: {}, error: invalidMessage }
  }
}
