#!/usr/bin/env node
/**
 * 口令传输协议的前后端一致性校验。
 *
 * 前端**没有单测框架**（仓库现状），前后端互操作只能靠这个脚本守住。
 * 它做两件事，缺一不可：
 *
 *   1. 用**前端真实实现**（apps/web/src/api/passwordCrypto.ts）按固定输入
 *      算一遍信封，和共享向量逐字段比对——前端改了 info 串、nonce 长度、
 *      base64 变体、明文 JSON 结构……这里立刻红。
 *   2. 读同一个共享向量文件；Go 侧（apps/api/internal/crypto）的
 *      TestPasswordDecryptsSharedVector 用它验证服务端能解开。
 *      两边读同一份文件 ⇒ 前端算的与服务端解的必然是同一个协议。
 *
 * ⚠️ 需要 Node ≥ 22.18（原生 TypeScript 支持，能直接 import .ts）。
 *    更老的 Node 会在这里明确报错，不会假装通过。
 *
 * 用法：
 *   node scripts/check-password-crypto.mjs
 *
 * 退出码非 0 表示有错——CI 或提交前跑一下。
 */
import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { pathToFileURL } from 'node:url'

const here = dirname(fileURLToPath(import.meta.url))
const vectorPath = resolve(here, '../apps/api/internal/crypto/testdata/password_envelope.json')
const modulePath = resolve(here, '../apps/web/src/api/passwordCrypto.ts')

const vector = JSON.parse(readFileSync(vectorPath, 'utf8'))

let encryptPassword
try {
  ;({ encryptPassword } = await import(pathToFileURL(modulePath).href))
} catch (err) {
  console.error('✗ 读不到前端加密模块。')
  console.error(`  ${err.message}`)
  console.error('  这个脚本需要 Node ≥ 22.18（原生 TypeScript 支持）。')
  process.exit(1)
}

/** base64url → 字节。 */
const fromBase64Url = (value) => new Uint8Array(Buffer.from(value, 'base64url'))

const envelope = await encryptPassword(
  { kid: vector.kid, alg: vector.alg, x25519_pub: vector.server_public_base64url },
  vector.username,
  vector.password,
  {
    esk: fromBase64Url(vector.esk_base64url),
    nonce: fromBase64Url(vector.nonce_base64url),
    ts: vector.ts,
  },
)

const diffs = []
for (const field of ['epk', 'nonce', 'ct']) {
  if (envelope[field] !== vector.expected[field]) {
    diffs.push({ field, got: envelope[field], want: vector.expected[field] })
  }
}

if (diffs.length > 0) {
  console.error('✗ 前端算出的信封与共享向量不一致：')
  for (const d of diffs) {
    console.error(`  ${d.field}`)
    console.error(`    实际 ${d.got}`)
    console.error(`    期望 ${d.want}`)
  }
  process.exit(1)
}

console.log(
  `✓ 口令传输协议一致：前端实现复现了共享向量（ct ${vector.expected.ct.slice(0, 16)}…）`,
)
