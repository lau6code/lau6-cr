import { request } from './client'
import { resolvePasswordSecret, takePendingNotice, type WithNotice } from './passwordTransport'
import type {
  CacheTestResult,
  SetupCacheTestRequest,
  SetupDatabaseInfo,
  SetupDatabaseTestRequest,
  SetupEnvPinned,
  SetupLdapTestRequest,
  SetupRequest,
  SetupStatus,
  SetupStorageInfo,
  SetupStorageTestRequest,
  SetupTestResult,
  StorageTestResult,
} from '@/types'

export function fetchSetupStatus(): Promise<SetupStatus> {
  return request<SetupStatus>('/setup/status')
}

/**
 * 读取当前生效的数据库连接信息（用于预填）。
 *
 * ⚠️ 后端**不回显口令**——口令要在表单里自己填。
 * 实例初始化后这个端点返回 409。
 */
export function fetchSetupDatabase(): Promise<SetupDatabaseInfo> {
  return request<SetupDatabaseInfo>('/setup/database')
}

/**
 * 读取当前生效的存储配置（用于预填）。
 *
 * ⚠️ 后端**不回显凭据**——只给 access_key_set / secret_key_set。
 * 实例初始化后这个端点返回 409。
 */
export function fetchSetupStorage(): Promise<SetupStorageInfo> {
  return request<SetupStorageInfo>('/setup/storage')
}

/**
 * 提交初始化。
 *
 * 管理员口令按服务端模式发送：明文模式发明文，其它模式发加密信封
 * （docs/design/13-password-transport.md §4.4）。
 */
export async function submitSetup(
  payload: SetupRequest,
): Promise<WithNotice<{ initialized: boolean; ignored_env_pinned: SetupEnvPinned[] }>> {
  const { admin_password: adminPassword, admin_username: adminUsername, ...rest } = payload
  const secret = await resolvePasswordSecret(adminUsername, adminPassword)
  const body = secret.envelope
    ? { ...rest, admin_username: adminUsername, ...secret.envelope }
    : { ...payload }
  const data = await request<{ initialized: boolean; ignored_env_pinned: SetupEnvPinned[] }>('/setup', {
    method: 'POST',
    body,
  })
  return { data, notice: takePendingNotice() }
}

/**
 * 测试数据库连通性。
 *
 * 失败时后端返回非 2xx（400 参数错 / 503 连不上），
 * 这里直接把错误抛给调用方——**不做兜底**，失败必须让用户看见。
 */
export function testSetupDatabase(payload: SetupDatabaseTestRequest): Promise<SetupTestResult> {
  return request<SetupTestResult>('/setup/test/database', {
    method: 'POST',
    body: payload,
  })
}

/** 测试 LDAP 连通性。失败时的语义同 testSetupDatabase。 */
export function testSetupLDAP(payload: SetupLdapTestRequest): Promise<SetupTestResult> {
  return request<SetupTestResult>('/setup/test/ldap', {
    method: 'POST',
    body: payload,
  })
}

/**
 * 测试缓存后端连通性。
 *
 * 不只测"连不连得上"，还读回淘汰策略 / 持久化 / requirepass——
 * 地址密码填对了不代表 Redis 配对了（docs/design/12-cache.md §5）。
 */
export function testSetupCache(payload: SetupCacheTestRequest): Promise<CacheTestResult> {
  return request<CacheTestResult>('/setup/test/cache', {
    method: 'POST',
    body: payload,
  })
}

/**
 * 测试存储后端能不能真的用。
 *
 * s3 会真写一个探针对象 → 读回 → 删掉（不是只做格式校验）：
 * 只读探针证明不了写权限，而"只给了读权限"是 CI 里最常见的坑。
 *
 * 失败时的语义同 testSetupDatabase——**不做兜底**，失败必须让用户看见。
 */
export function testSetupStorage(payload: SetupStorageTestRequest): Promise<StorageTestResult> {
  return request<StorageTestResult>('/setup/test/storage', {
    method: 'POST',
    body: payload,
  })
}
