import { request } from './client'
import type { Id, PersonalToken, PersonalTokenCreated, PersonalTokenExpiry } from '@/types'

/**
 * 列出当前用户的个人令牌。
 *
 * `default_ttl_seconds` 是系统默认有效期——用户不指定失效时间时用它。
 */
export function listPersonalTokens(): Promise<{
  tokens: PersonalToken[]
  default_ttl_seconds: number
}> {
  return request<{ tokens: PersonalToken[]; default_ttl_seconds: number }>(
    '/api/v1/auth/tokens',
  )
}

/**
 * 生成一枚个人令牌。
 *
 * ⚠️ 响应里的 `token` 是**原始令牌**，只此一次——
 *    后端库里只存散列，刷新页面就再也取不回来了。
 *
 * `expiry` 决定失效时间：永久 / 指定时刻 / 系统默认。
 */
export function createPersonalToken(
  name: string,
  expiry: PersonalTokenExpiry,
): Promise<PersonalTokenCreated> {
  return request<PersonalTokenCreated>('/api/v1/auth/tokens', {
    method: 'POST',
    body: { name, ...expiry },
  })
}

/**
 * 改一枚个人令牌的失效时间（只能改自己的）。
 *
 * 令牌本体不变——散列不动，CI 里已配好的凭据不用重配。
 */
export function updatePersonalTokenExpiry(
  id: Id,
  expiry: PersonalTokenExpiry,
): Promise<Record<string, never>> {
  return request<Record<string, never>>(
    `/api/v1/auth/tokens/${encodeURIComponent(String(id))}`,
    { method: 'PATCH', body: { ...expiry } },
  )
}

/** 只能吊销自己的。 */
export function revokePersonalToken(id: Id): Promise<Record<string, never>> {
  return request<Record<string, never>>(
    `/api/v1/auth/tokens/${encodeURIComponent(String(id))}`,
    { method: 'DELETE' },
  )
}
