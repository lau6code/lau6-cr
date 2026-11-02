/**
 * 口令传输的传输层：拉服务端公钥 + 决定明文还是密文。
 *
 * 模式语义见 docs/design/13-password-transport.md §4.6：
 *
 *   - `password.mode=plaintext` → 只收明文（公钥接口返回 enabled=false）
 *   - `password.mode=optional`  → 两者都收（优先解密）
 *   - `password.mode=required`  → 只收密文（明文一律 400）
 *
 * 客户端通过公钥接口的 `enabled` 字段判断服务端是否接受密文
 *（§8 的开放问题给的就是这个方案）。
 */
import { errorMessage, request } from './client'
import { translate } from '@/i18n'
import {
  appLayerCryptoAvailable,
  encryptPassword,
  PASSWORD_ALG,
  PasswordCryptoError,
  type PasswordEnvelope,
  type PubKeyInfo,
} from './passwordCrypto'

interface PasswordTransport {
  keys: PubKeyInfo[]
  /** 服务端是否接受密文口令（plaintext 模式下为 false）。 */
  enabled: boolean
}

export interface WithNotice<T> {
  data: T
  /**
   * 降级提示。
   *
   * 非 null = 发生了降级（公钥拉不到，按明文提交了），必须让用户看见。
   */
  notice: string | null
}

interface PasswordSecret {
  /** 加密信封（加密模式）；明文模式为 null。 */
  envelope: PasswordEnvelope | null
  /** 明文口令（明文模式）。 */
  password: string
  /** 明文旧口令（明文模式，改密场景）。 */
  oldPassword?: string
  /** 降级提示；null = 没有降级。 */
  notice: string | null
}

/** 上一次**成功**拉到的传输状态。 */
let lastKnown: PasswordTransport | null = null

/**
 * 待提示的降级提示。
 *
 * 降级发生在**请求发出之前**，而请求可能失败——失败时调用方拿不到
 * 返回值里的 notice，所以提示先存在这里，由调用方在失败分支取走。
 */
let pendingNotice: string | null = null

/**
 * 取走待提示的降级提示（取一次就没了）。
 *
 * 失败分支用它把"为什么降级了"告诉用户——降级不能是静默的。
 */
export function takePendingNotice(): string | null {
  const notice = pendingNotice
  pendingNotice = null
  return notice
}

/**
 * 拉取口令传输状态。
 *
 * 页面挂载时先拉一次：提前知道服务端是否要求加密登录，
 * 公钥拉不到也能立刻报错，而不是提交时才失败。
 *
 * 失败会抛错——调用方必须自己决定怎么办，**不能**静默当成"明文模式"。
 */
export async function fetchPasswordTransport(): Promise<PasswordTransport> {
  const data = await request<PasswordTransport>('/api/v1/auth/pubkey')
  lastKnown = data
  return data
}

/**
 * 解析口令来源：按服务端模式决定明文还是密文。
 *
 * ⚠️ **不降级**：服务端 enabled=true（要加密）而公钥拿不到时，
 *    直接抛错——偷偷改成明文提交只会把"公钥拉不到"变成"口令不对"。
 *
 * ⚠️ **从未成功拉过公钥**时无法判断服务端模式：这时按明文提交，
 *    并给出降级提示（服务端若是 required 会明确拒绝，用户看得见）。
 */
export async function resolvePasswordSecret(
  username: string,
  password: string,
  oldPassword?: string,
): Promise<PasswordSecret> {
  // 每次解析都重置：待提示只属于**本次**请求，
  // 否则上一次失败的降级提示会被下一次成功请求顺带取走。
  pendingNotice = null

  let transport: PasswordTransport | null = null
  let notice: string | null = null

  try {
    transport = await fetchPasswordTransport()
  } catch (err) {
    if (lastKnown?.enabled) {
      // 之前成功拉过（说明服务端要加密）→ 明确报错，不降级。
      throw new Error(
        `${translate('passwordTransport.cannotEncrypt')}：${errorMessage(
          err,
          translate('passwordTransport.networkFailed'),
        )}`,
      )
    }
    notice = translate('passwordTransport.plaintextFallback')
    pendingNotice = notice
  }

  if (!transport || !transport.enabled) {
    return { envelope: null, password, oldPassword, notice }
  }

  // 服务端要密文，但**客户端**做不了加密（老浏览器缺 BigInt / TextEncoder）：
  // 显式降级成明文并给出提示，而不是抛一个看不懂的 TypeError。
  // 服务端若是 required 模式会明确拒绝明文，用户看得见（fail-closed）。
  if (!appLayerCryptoAvailable()) {
    notice = translate('passwordTransport.clientCryptoUnavailable')
    pendingNotice = notice
    return { envelope: null, password, oldPassword, notice }
  }

  const key = transport.keys.find((item) => item.alg === PASSWORD_ALG)
  if (!key) {
    throw new Error(translate('passwordTransport.noUsableKey'))
  }
  let envelope: PasswordEnvelope
  try {
    envelope = await encryptPassword(key, username, password, { oldPassword })
  } catch (err) {
    // crypto 层只抛错误码（它不碰 i18n），这里翻成文案。
    if (err instanceof PasswordCryptoError) {
      throw new Error(translate(`passwordTransport.${err.code}`, err.params))
    }
    throw err
  }
  return { envelope, password: '', oldPassword: undefined, notice: null }
}
