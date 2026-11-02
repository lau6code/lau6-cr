/**
 * 统一的 fetch 封装：拆业务信封、抛 ApiError。
 * 不引入任何 HTTP 客户端库。
 */

import { translate } from '@/i18n'

interface ApiEnvelope<T> {
  code: number
  message: string
  data: T
}

type HttpMethod = 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE'

type QueryParams = Record<string, string | number | boolean | null | undefined>

interface RequestOptions {
  method?: HttpMethod
  body?: unknown
  query?: QueryParams
  signal?: AbortSignal
}

/** code 来自信封，status 是 HTTP 状态码。 */
class ApiError extends Error {
  readonly code: number
  readonly status: number
  readonly data: unknown

  constructor(message: string, code: number, status: number, data?: unknown) {
    super(message)
    this.name = 'ApiError'
    this.code = code
    this.status = status
    this.data = data
  }
}

/** setup_required：实例尚未初始化，业务端点全部返回此码。 */
const CODE_SETUP_REQUIRED = 50302

function isApiError(err: unknown): err is ApiError {
  return err instanceof ApiError
}

/**
 * 把错误翻译成能直接展示给用户的文案。
 *
 * @param fallback 后端没给可用文案时的兜底（**调用方必须给**，不能留空）。
 */
export function errorMessage(err: unknown, fallback = translate('common.requestFailed')): string {
  if (isApiError(err)) return err.message || fallback
  if (err instanceof Error && err.message) return err.message
  return fallback
}

export function isSetupRequired(err: unknown): boolean {
  return isApiError(err) && err.code === CODE_SETUP_REQUIRED
}

function buildUrl(path: string, query?: QueryParams): string {
  const url = new URL(path, window.location.origin)
  if (query) {
    for (const [key, value] of Object.entries(query)) {
      if (value === undefined || value === null || value === '') continue
      url.searchParams.set(key, String(value))
    }
  }
  return url.toString()
}

function redirectToSetup(): void {
  if (window.location.pathname === '/setup') return
  window.location.assign('/setup')
}

/**
 * 发起一次业务请求。
 * - HTTP 2xx 且信封 code === 0：返回 data
 * - 其余情况：抛 ApiError，message 可直接展示给用户
 */
export async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const { method = 'GET', body, query, signal } = options

  const headers: Record<string, string> = { Accept: 'application/json' }
  if (body !== undefined) headers['Content-Type'] = 'application/json'

  const init: RequestInit = {
    method,
    credentials: 'include',
    headers,
  }
  if (signal !== undefined) init.signal = signal
  if (body !== undefined) init.body = JSON.stringify(body)

  let response: Response
  try {
    response = await fetch(buildUrl(path, query), init)
  } catch {
    throw new ApiError(translate('common.networkUnavailable'), -1, 0)
  }

  const raw = await response.text()
  let payload: unknown
  if (raw) {
    try {
      payload = JSON.parse(raw)
    } catch {
      payload = raw
    }
  }

  const envelope =
    payload && typeof payload === 'object' ? (payload as Partial<ApiEnvelope<unknown>>) : undefined

  if (!response.ok) {
    const message =
      (typeof envelope?.message === 'string' && envelope.message) ||
      translate('common.httpFailed', { status: response.status })
    const code = typeof envelope?.code === 'number' ? envelope.code : response.status
    if (code === CODE_SETUP_REQUIRED) redirectToSetup()
    throw new ApiError(message, code, response.status, envelope?.data)
  }

  if (envelope && typeof envelope.code === 'number') {
    if (envelope.code !== 0) {
      if (envelope.code === CODE_SETUP_REQUIRED) redirectToSetup()
      throw new ApiError(
        envelope.message || translate('common.requestFailed'),
        envelope.code,
        response.status,
        envelope.data,
      )
    }
    return envelope.data as T
  }

  // 2xx 却拿不到业务信封：后端契约破了（空 body / 非 JSON / 缺 code）。
  //
  // ⚠️ 这里必须抛错而不是把原始内容当数据返回：
  //    调用方拿到 undefined 会渲染成"暂无数据"，失败就静默消失了。
  throw new ApiError(translate('common.requestFailed'), response.status, response.status)
}
