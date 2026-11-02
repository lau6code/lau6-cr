/**
 * 清理页的枚举 → 文案键映射。
 *
 * 这里的取值都是后端下发的固定字符串（触发方式、执行状态、跳过原因），
 * 映射表只负责"取值 → 语言包键"；表里没有的取值按原文显示，不吞掉。
 */

/** 触发方式。清理与 blob 回收的取值合并在一张表里（schedule / manual 两边都有）。 */
export const TRIGGER_LABELS: Record<string, string> = {
  schedule: 'cleanup.triggerSchedule',
  manual: 'cleanup.triggerManual',
  push: 'cleanup.triggerPush',
  after_cleanup: 'cleanup.triggerAfterCleanup',
}

/** 清理执行的状态。 */
export const STATUS_LABELS: Record<string, string> = {
  running: 'cleanup.statusRunning',
  success: 'cleanup.statusSuccess',
  failed: 'cleanup.statusFailed',
}

/** 跳过原因；表里没有的按原文显示，不吞掉。 */
export const SKIP_REASON_LABELS: Record<string, string> = {
  unknown_time: 'cleanup.skipUnknownTime',
  shared_digest: 'cleanup.skipSharedDigest',
  protected_latest: 'cleanup.skipProtectedLatest',
  no_digest: 'cleanup.skipNoDigest',
}
