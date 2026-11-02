import { request } from './client'
import { resolvePasswordSecret, takePendingNotice, type WithNotice } from './passwordTransport'
import type { LoginResult, MeResponse } from '@/types'

interface LoginPayload {
  username: string
  password: string
}

/**
 * 登录。
 *
 * 口令按服务端模式发送：明文模式发明文，其它模式发加密信封
 * （docs/design/13-password-transport.md §4.4）。
 */
export async function login(payload: LoginPayload): Promise<WithNotice<LoginResult>> {
  const secret = await resolvePasswordSecret(payload.username, payload.password)
  const body = secret.envelope
    ? { username: payload.username, ...secret.envelope }
    : { username: payload.username, password: secret.password }
  // 失败时降级提示留在 pendingNotice 里，由调用方在错误分支取走。
  const data = await request<LoginResult>('/api/v1/auth/login', {
    method: 'POST',
    body,
  })
  return { data, notice: takePendingNotice() }
}

export function logout(): Promise<Record<string, never>> {
  return request<Record<string, never>>('/api/v1/auth/logout', { method: 'POST' })
}

/** 匿名访问返回 200 + anonymous:true，不抛错。 */
export function fetchMe(): Promise<MeResponse> {
  return request<MeResponse>('/api/v1/auth/me')
}

interface ChangePasswordPayload {
  old_password: string
  new_password: string
}

/**
 * 自助改密。LDAP 账号会被后端拒绝（口令由目录维护）。
 *
 * @param username 当前登录用户的用户名——加密模式下它参与 HKDF 派生，
 *   必须与服务端算出来的 info 串一致。
 */
export async function changePassword(
  payload: ChangePasswordPayload,
  username: string,
): Promise<WithNotice<Record<string, never>>> {
  const secret = await resolvePasswordSecret(username, payload.new_password, payload.old_password)
  const body = secret.envelope
    ? { username, ...secret.envelope }
    : { old_password: payload.old_password, new_password: payload.new_password }
  const data = await request<Record<string, never>>('/api/v1/auth/password', {
    method: 'POST',
    body,
  })
  return { data, notice: takePendingNotice() }
}
