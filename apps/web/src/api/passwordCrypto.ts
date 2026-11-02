/**
 * 口令传输的应用层加密（客户端侧）。
 *
 * 方案见 docs/design/13-password-transport.md §4.1：
 *
 *   shared = X25519(esk, server_pub)
 *   key    = HKDF-SHA256(ikm=shared, salt=nonce(16B), info="lau6-cr password v1 | " + username + kid)
 *   ct     = AES-256-GCM(key, nonce, JSON{username, password, ts})
 *
 * ⚠️ **为什么用 @noble/* 而不是浏览器的 WebCrypto**：
 *    `crypto.subtle` **只在安全上下文暴露**（HTTPS 或 localhost），
 *    明文 HTTP 部署（内网直出）下它是 `undefined` —— 加密会直接抛
 *    TypeError，整个登录流程崩掉。而应用层加密恰恰就是为了在这种
 *    链路上保护口令，用一个只在 HTTPS 下才有的 API 去实现它是自相矛盾。
 *    noble 是纯 JS 实现，任何环境都能跑，输出与 WebCrypto 逐字节一致
 *    （scripts/check-password-crypto.mjs 守着这条等价关系）。
 *
 * ⚠️ **WebCrypto 不支持 X25519**（crypto.subtle 只有 P-256 等），
 *    所以 X25519 这里自带一份纯 JS 实现（RFC 7748 的 Montgomery ladder）。
 *
 * ⚠️ 这里的编码约定必须与后端（apps/api/internal/crypto/password_transport.go）
 *    **逐字一致**：base64url 无填充、nonce 16 字节、ts 是 unix 毫秒、
 *    info 串里 username 在前 kid 在后。
 *
 * ⚠️ 这一层**不碰 i18n**：失败原因用错误码抛出去，由调用方
 *    （api/passwordTransport.ts）翻成文案。这样本模块不依赖别名解析，
 *    能被 Node 直接 import 跑互操作校验。
 */
import { gcm } from '@noble/ciphers/aes.js'
import { hmac } from '@noble/hashes/hmac.js'
import { sha256 } from '@noble/hashes/sha2.js'
import { randomBytes } from '@noble/hashes/utils.js'

/** 算法标识（与后端的 crypto.PasswordAlg 对齐）。 */
export const PASSWORD_ALG = 'X25519-HKDF-SHA256-AESGCM'

/** nonce 字节数（同时是 HKDF 的 salt 与 AES-GCM 的 IV）。 */
const PASSWORD_NONCE_SIZE = 16

export interface PubKeyInfo {
  kid: string
  alg: string
  x25519_pub: string
}

export interface PasswordEnvelope {
  kid: string
  epk: string
  nonce: string
  ct: string
}

/** 口令加密的失败原因（错误码，不是文案）。 */
type PasswordCryptoErrorCode =
  | 'scalarLength'
  | 'pubkeyLength'
  | 'sharedSecretInvalid'
  | 'serverPubkeyInvalid'
  | 'unsupportedAlg'

/**
 * 口令加密失败。
 *
 * 带 code 与 params：调用方拿 code 去语言包取文案（`passwordTransport.<code>`），
 * params 是文案里的占位符（如 unsupportedAlg 的 {alg}）。
 */
export class PasswordCryptoError extends Error {
  // declare：纯类型声明，不产生运行时代码——
  // 这样 Node 的类型剥离（互操作校验脚本要 import 本模块）也不会碰到类字段语义。
  declare readonly code: PasswordCryptoErrorCode
  declare readonly params: Record<string, string>

  constructor(code: PasswordCryptoErrorCode, params: Record<string, string> = {}) {
    super(code)
    this.name = 'PasswordCryptoError'
    this.code = code
    this.params = params
  }
}

/**
 * 加密选项。
 *
 * esk / nonce / ts 只有测试向量会显式传（要可复现），
 * 生产路径一律留空 → 随机生成 + 当前时间。
 */
interface PasswordEncryptOptions {
  /** 旧口令（改密场景，进明文 JSON）。 */
  oldPassword?: string
  /** 临时私钥（32 字节）。 */
  esk?: Uint8Array
  /** nonce（16 字节）。 */
  nonce?: Uint8Array
  /** 时间戳（unix 毫秒）。 */
  ts?: number
}

/**
 * 当前环境能不能做应用层加密。
 *
 * 依赖 BigInt（X25519 的模运算）与 TextEncoder（明文 JSON 编码）。
 * 两者在现代浏览器里都有；这里留一道是为了老浏览器上**显式降级**
 * 而不是抛一个看不懂的 TypeError。
 */
export function appLayerCryptoAvailable(): boolean {
  return typeof BigInt === 'function' && typeof TextEncoder === 'function'
}

// ─────────────────────────── base64url ───────────────────────────

function toBase64Url(bytes: Uint8Array): string {
  let binary = ''
  for (let i = 0; i < bytes.length; i += 1) binary += String.fromCharCode(bytes[i])
  return btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
}

function fromBase64Url(value: string): Uint8Array {
  const normalized = value.replace(/-/g, '+').replace(/_/g, '/')
  const padded = normalized + '='.repeat((4 - (normalized.length % 4)) % 4)
  const binary = atob(padded)
  const out = new Uint8Array(binary.length)
  for (let i = 0; i < binary.length; i += 1) out[i] = binary.charCodeAt(i)
  return out
}

// ─────────────────────────── X25519 ───────────────────────────

/** 域的素数 p = 2^255 - 19。 */
const P = (1n << 255n) - 19n

/** Montgomery ladder 的 a24 = (486662 - 2) / 4。 */
const A24 = 121665n

function mod(a: bigint): bigint {
  const r = a % P
  return r < 0n ? r + P : r
}

function powMod(base: bigint, exponent: bigint): bigint {
  let result = 1n
  let b = mod(base)
  let e = exponent
  while (e > 0n) {
    if (e & 1n) result = (result * b) % P
    b = (b * b) % P
    e >>= 1n
  }
  return result
}

/**
 * 条件交换（常数时间）。
 *
 * swap 为 0 或 1：0 → 原样返回；1 → 交换两个值。
 */
function cswap(swap: bigint, a: bigint, b: bigint): [bigint, bigint] {
  const diff = mod(a - b)
  const mask = -swap & 1n
  const t = (diff * mask) % P
  return [mod(a - t), mod(b + t)]
}

function bytesToBigIntLE(bytes: Uint8Array): bigint {
  let out = 0n
  for (let i = bytes.length - 1; i >= 0; i -= 1) out = (out << 8n) | BigInt(bytes[i])
  return out
}

function bigIntToBytesLE(value: bigint, length: number): Uint8Array {
  const out = new Uint8Array(length)
  let v = value
  for (let i = 0; i < length; i += 1) {
    out[i] = Number(v & 0xffn)
    v >>= 8n
  }
  return out
}

/** 按 RFC 7748 夹紧标量（清低 3 位、置高 2 位、置第 254 位）。 */
function clampScalar(scalar: Uint8Array): Uint8Array {
  const s = scalar.slice()
  s[0] &= 248
  s[31] &= 127
  s[31] |= 64
  return s
}

/**
 * X25519 标量乘（RFC 7748）。
 *
 * @param scalar 32 字节私钥（内部会夹紧）
 * @param u 32 字节对端公钥
 * @returns 32 字节共享密钥
 */
function x25519(scalar: Uint8Array, u: Uint8Array): Uint8Array {
  if (scalar.length !== 32) throw new PasswordCryptoError('scalarLength')
  if (u.length !== 32) throw new PasswordCryptoError('pubkeyLength')

  const k = bytesToBigIntLE(clampScalar(scalar))
  const uBytes = u.slice()
  uBytes[31] &= 127 // 忽略最高位
  const x1 = bytesToBigIntLE(uBytes)

  let x2 = 1n
  let z2 = 0n
  let x3 = x1
  let z3 = 1n
  let swap = 0n

  for (let t = 254; t >= 0; t -= 1) {
    const kt = (k >> BigInt(t)) & 1n
    swap ^= kt
    ;[x2, x3] = cswap(swap, x2, x3)
    ;[z2, z3] = cswap(swap, z2, z3)
    swap = kt

    const a = mod(x2 + z2)
    const aa = mod(a * a)
    const b = mod(x2 - z2)
    const bb = mod(b * b)
    const e = mod(aa - bb)
    const c = mod(x3 + z3)
    const d = mod(x3 - z3)
    const da = mod(d * a)
    const cb = mod(c * b)

    const sum = mod(da + cb)
    const diff = mod(da - cb)
    x3 = mod(sum * sum)
    z3 = mod(x1 * mod(diff * diff))
    x2 = mod(aa * bb)
    z2 = mod(e * mod(aa + A24 * e))
  }

  ;[x2, x3] = cswap(swap, x2, x3)
  ;[z2, z3] = cswap(swap, z2, z3)

  const shared = mod(x2 * powMod(z2, P - 2n))
  const out = bigIntToBytesLE(shared, 32)
  // 全零 = 对端给了低阶点，共享密钥无效。
  if (out.every((byte) => byte === 0)) throw new PasswordCryptoError('sharedSecretInvalid')
  return out
}

/** 基点 u = 9。 */
const BASE_POINT = (() => {
  const point = new Uint8Array(32)
  point[0] = 9
  return point
})()

function x25519PublicKey(scalar: Uint8Array): Uint8Array {
  return x25519(scalar, BASE_POINT)
}

// ─────────────────────────── HKDF / AES-GCM ───────────────────────────

function hmacSha256(key: Uint8Array, data: Uint8Array): Uint8Array {
  return hmac(sha256, key, data)
}

/** HKDF-SHA256（RFC 5869）：extract + expand，输出 32 字节。 */
function hkdfSha256(ikm: Uint8Array, salt: Uint8Array, info: string): Uint8Array {
  // RFC 5869：salt 为空时用 32 字节全零。
  const effectiveSalt = salt.length > 0 ? salt : new Uint8Array(32)
  const prk = hmacSha256(effectiveSalt, ikm)
  const infoBytes = new TextEncoder().encode(info)
  const input = new Uint8Array(infoBytes.length + 1)
  input.set(infoBytes, 0)
  input[input.length - 1] = 1
  const block = hmacSha256(prk, input)
  return block.slice(0, 32)
}

function aesGcmEncrypt(key: Uint8Array, iv: Uint8Array, plaintext: Uint8Array): Uint8Array {
  return gcm(key, iv).encrypt(plaintext)
}

// ─────────────────────────── 信封 ───────────────────────────

/** 返回 HKDF 的 info 串（与后端 crypto.PasswordKDFInfo 一致）。 */
function passwordKdfInfo(username: string, kid: string): string {
  return `lau6-cr password v1 | ${username}${kid}`
}

/**
 * 用服务端公钥加密口令，生成一个信封。
 *
 * @param key 服务端公钥
 * @param username 用户名（参与 HKDF 的 info 派生）
 * @param password 口令
 * @param options 旧口令 / 测试向量用的固定 esk、nonce、ts
 */
export async function encryptPassword(
  key: PubKeyInfo,
  username: string,
  password: string,
  options: PasswordEncryptOptions = {},
): Promise<PasswordEnvelope> {
  if (key.alg !== PASSWORD_ALG) {
    throw new PasswordCryptoError('unsupportedAlg', { alg: key.alg })
  }
  const serverPub = fromBase64Url(key.x25519_pub)
  if (serverPub.length !== 32) throw new PasswordCryptoError('serverPubkeyInvalid')

  const esk = options.esk ?? randomBytes(32)
  const epk = x25519PublicKey(esk)
  const shared = x25519(esk, serverPub)

  const nonce = options.nonce ?? randomBytes(PASSWORD_NONCE_SIZE)
  const derived = hkdfSha256(shared, nonce, passwordKdfInfo(username, key.kid))

  const payload: Record<string, unknown> = {
    username,
    password,
    ts: options.ts ?? Date.now(),
  }
  if (options.oldPassword) payload.old_password = options.oldPassword

  const plaintext = new TextEncoder().encode(JSON.stringify(payload))
  const ciphertext = aesGcmEncrypt(derived, nonce, plaintext)

  return {
    kid: key.kid,
    epk: toBase64Url(epk),
    nonce: toBase64Url(nonce),
    ct: toBase64Url(ciphertext),
  }
}
