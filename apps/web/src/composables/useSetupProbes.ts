/**
 * 向导的连通性探测载荷。
 *
 * "测试"与"提交前复核"探的是同一组参数——载荷构造只有这一份，
 * 两边算出来的必须一致（否则测的和提交的不是同一个东西）。
 */
import type { SetupRequest, SetupStorage } from '@/types'

/** 探测载荷用到的表单字段。 */
export type SetupProbeForm = Pick<SetupRequest, 'bootstrap_token' | 'cache'> & {
  storage: SetupStorage
}

export function useSetupProbes(form: SetupProbeForm) {
  /** 缓存连通性测试的载荷。 */
  function cachePayload() {
    return {
      bootstrap_token: form.bootstrap_token.trim(),
      cache: {
        driver: form.cache.driver,
        local: { max_entries: form.cache.local.max_entries },
        redis: {
          addr: form.cache.redis.addr.trim(),
          password: form.cache.redis.password,
          prefix: form.cache.redis.prefix,
          // ⚠️ null 表示"没填"：后端据此判定"选 redis 却没给数据库编号"，
          //    不会拿 0 兜底（db 0 常被别的应用占着）。
          db: form.cache.redis.db,
        },
      },
    }
  }

  /** 存储连通性测试的载荷。 */
  function storagePayload() {
    return {
      bootstrap_token: form.bootstrap_token.trim(),
      storage: { ...form.storage },
    }
  }

  return { cachePayload, storagePayload }
}
