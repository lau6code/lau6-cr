/**
 * 向导里"被环境变量钉死"的设置项判定。
 *
 * env 的优先级最高（env > system_settings > application.yaml > 默认值），
 * 被钉死的项向导改不了——改了也是白改：运行期短暂按向导的值走，
 * 重启后 env 又盖回去，出现"初始化后关、重启后开"的翻转。
 *
 * 与后端 envPinned 同一口径（变量存在即钉死），也跟设置页一致。
 */
import { computed, type Ref } from 'vue'

import { translate } from '@/i18n'
import type { SetupStatus } from '@/types'

/**
 * 向导能碰到的、可能被环境变量钉死的设置项。
 *
 * ⚠️ 只列向导**实际会写**的那些——别的设置项钉死了与向导无关，
 *    列出来只是噪声（docs/design/08-setup.md §8）。
 */
const SETUP_PINNED_KEYS = [
  'core.public_url',
  'access.anonymous_read_enabled',
  'cache.driver',
  'cache.redis.addr',
  'cache.redis.password',
  'cache.redis.db',
  'cache.redis.prefix',
  'ldap.enabled',
  'ldap.url',
  'ldap.dc',
  'ldap.user_segment',
  'ldap.start_tls',
  'ldap.allow_create',
  'ldap.timeout_sec',
  'storage.driver',
  'storage.s3.endpoint',
  'storage.s3.bucket',
  'storage.s3.region',
  'storage.s3.path_style',
]

export function useSetupPinned(status: Ref<SetupStatus | null>) {
  const envPinned = computed(() => new Set((status.value?.env_pinned ?? []).map((p) => p.key)))

  /** 某个设置项是否被环境变量钉死（向导里显示成只读）。 */
  function isPinned(key: string): boolean {
    return envPinned.value.has(key)
  }

  /** 被钉死、且向导本来就会写的项的可读说明。 */
  const pinnedNotice = computed(() =>
    (status.value?.env_pinned ?? [])
      .filter((p) => SETUP_PINNED_KEYS.includes(p.key))
      .map((p) => translate('setup.pinnedItem', { key: p.key, envVar: p.env_var })),
  )

  return { isPinned, pinnedNotice }
}
