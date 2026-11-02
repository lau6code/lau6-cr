import { request } from './client'
import { resolvePasswordSecret, takePendingNotice, type WithNotice } from './passwordTransport'
import type { Id, RegistryUser } from '@/types'

interface CreateUserPayload {
  username: string
  password: string
  display_name: string
  is_system_admin: boolean
}

interface UpdateUserPayload {
  display_name?: string
  is_system_admin?: boolean
  disabled?: boolean
}

export function listUsers(): Promise<{ users: RegistryUser[] }> {
  return request<{ users: RegistryUser[] }>('/api/v1/users')
}

/**
 * 创建用户。
 *
 * 初始口令按服务端模式发送：明文模式发明文，其它模式发加密信封
 * （docs/design/13-password-transport.md §4.4）。
 */
export async function createUser(
  payload: CreateUserPayload,
): Promise<WithNotice<{ user: RegistryUser }>> {
  const secret = await resolvePasswordSecret(payload.username, payload.password)
  const body = secret.envelope
    ? {
        username: payload.username,
        display_name: payload.display_name,
        is_system_admin: payload.is_system_admin,
        ...secret.envelope,
      }
    : { ...payload }
  const data = await request<{ user: RegistryUser }>('/api/v1/users', {
    method: 'POST',
    body,
  })
  return { data, notice: takePendingNotice() }
}

export function updateUser(id: Id, payload: UpdateUserPayload): Promise<{ user: RegistryUser }> {
  return request<{ user: RegistryUser }>(`/api/v1/users/${encodeURIComponent(String(id))}`, {
    method: 'PATCH',
    body: payload,
  })
}

/**
 * 管理员替用户重置口令。
 *
 * 口令按服务端模式发送：明文模式发明文，其它模式发加密信封
 * （docs/design/13-password-transport.md §4.4）。
 *
 * @param username 目标用户的用户名——加密模式下它参与 HKDF 派生，
 *   必须与服务端算出来的 info 串一致（服务端用库里的同一个用户名）。
 */
export async function resetUserPassword(
  id: Id,
  username: string,
  password: string,
): Promise<WithNotice<{ username: string }>> {
  const secret = await resolvePasswordSecret(username, password)
  const body = secret.envelope ? { ...secret.envelope } : { new_password: secret.password }
  const data = await request<{ username: string }>(
    `/api/v1/users/${encodeURIComponent(String(id))}/password`,
    { method: 'PUT', body },
  )
  return { data, notice: takePendingNotice() }
}
