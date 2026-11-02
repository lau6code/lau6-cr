#!/usr/bin/env node
/**
 * 前端语言包一致性校验。
 *
 * 前端**没有单测框架**（仓库现状），语言包的一致性只能靠这个脚本守住。
 * 它检查三件事：
 *
 *   1. 三个语言包的**键结构完全一致**（zh-CN 是结构真源）。
 *   2. 代码里引用的每个 i18n 键在 zh-CN 里**都存在**——
 *      漏键时 vue-i18n 会把键名本身当文案显示出来（界面上出现 "settings.groupBasic"）。
 *   3. 同一条文案在三个语言包里**占位符完全一致**（`{name}` 之类）。
 *
 * 用法：
 *   node scripts/check-i18n.mjs
 *
 * 退出码非 0 表示有错——CI 或提交前跑一下。
 */
import { readFileSync, readdirSync, statSync } from 'node:fs'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const here = dirname(fileURLToPath(import.meta.url))
const srcDir = resolve(here, '../apps/web/src')
const localesDir = join(srcDir, 'locales')

/** 语言包真源：键结构以它为准。 */
const SOURCE = 'zh-CN'
const OTHERS = ['zh-TW', 'en']

/**
 * 读语言包。
 *
 * 语言包有两种写法：
 *   - `export default { ... }`（zh-CN）
 *   - `const messages: Messages = { ... }` + `export default messages`（zh-TW / en）
 * 这里把 TS 类型标注和 import/export 剥掉后按 CommonJS 求值，
 * 只取结构，不做类型检查（类型检查由 `Messages` 标注负责）。
 */
function loadMessages(file) {
  const raw = readFileSync(file, 'utf8')
  const body = raw
    .replace(/^import[\s\S]*?from\s+['"][^'"]+['"]\s*$/gm, '')
    .replace(/^(const\s+\w+)\s*:\s*Messages\s*=/m, '$1 =')
    .replace(/^export\s+default\s+\w+\s*$/m, 'module.exports = messages')
    .replace(/^export\s+default\s*/m, 'module.exports = ')
  const sandbox = { module: { exports: {} } }
  // eslint-disable-next-line no-new-func
  new Function('module', 'exports', body)(sandbox.module, sandbox.module.exports)
  return sandbox.module.exports
}

/** 把嵌套对象摊平成 `a.b.c` 形式的键列表。 */
function flatten(obj, prefix = '') {
  return Object.entries(obj).flatMap(([key, value]) =>
    value && typeof value === 'object' ? flatten(value, `${prefix}${key}.`) : [`${prefix}${key}`],
  )
}

/** 取一条文案里的占位符名（`{name}` → `name`），按出现顺序。 */
function placeholders(text) {
  return [...String(text).matchAll(/\{([a-zA-Z][a-zA-Z0-9]*)\}/g)].map((m) => m[1])
}

/** 递归列出 src 下的 .ts / .vue 文件（跳过语言包与 i18n 实现本身）。 */
function listSourceFiles(dir) {
  const out = []
  for (const entry of readdirSync(dir)) {
    if (entry === 'locales' || entry === 'i18n') continue
    const full = join(dir, entry)
    if (statSync(full).isDirectory()) out.push(...listSourceFiles(full))
    else if (/\.(ts|vue)$/.test(entry)) out.push(full)
  }
  return out
}

const problems = []

// ── 1 · 键结构一致 ─────────────────────────────────────────────
const sourceMessages = loadMessages(join(localesDir, `${SOURCE}.ts`))
const sourceKeys = flatten(sourceMessages)
const sourceSet = new Set(sourceKeys)

for (const locale of OTHERS) {
  const keys = flatten(loadMessages(join(localesDir, `${locale}.ts`)))
  const set = new Set(keys)
  const missing = sourceKeys.filter((k) => !set.has(k))
  const extra = keys.filter((k) => !sourceSet.has(k))
  for (const k of missing) problems.push(`${locale}.ts 缺少键 ${k}`)
  for (const k of extra) problems.push(`${locale}.ts 多出键 ${k}`)
  // 顺序不一致会让 diff 难读，但不影响运行——只提示不报错。
  const orderDiff = keys.filter((k, i) => sourceKeys[i] !== k)
  if (orderDiff.length) {
    console.warn(`⚠️  ${locale}.ts 的键顺序与 ${SOURCE}.ts 不一致（${orderDiff.length} 处）`)
  }
}

// ── 2 · 代码里引用的键都存在 ───────────────────────────────────
// 只认"看起来像 i18n 键"的字面量：首段是已知节名，且含点号。
//
// ⚠️ 设置项 key（如 `cleanup.enabled`）不是 i18n 键，它们长得很像——
//    所以先把设置页 GROUPS 里列的 key 收集成白名单，排掉。
const settingsView = readFileSync(join(srcDir, 'views/admin/SettingsView.vue'), 'utf8')
const settingKeys = new Set(
  [...settingsView.matchAll(/keys:\s*\[([\s\S]*?)\]/g)].flatMap((m) =>
    [...m[1].matchAll(/['"]([\w.]+)['"]/g)].map((x) => x[1]),
  ),
)

const sections = new Set(sourceKeys.map((k) => k.split('.')[0]))
const sourceFiles = listSourceFiles(srcDir)
for (const file of sourceFiles) {
  const text = readFileSync(file, 'utf8')
  const rel = file.slice(srcDir.length + 1)
  for (const match of text.matchAll(/['"]([a-zA-Z][a-zA-Z0-9]*\.[a-zA-Z0-9_.]+)['"]/g)) {
    const key = match[1]
    if (!sections.has(key.split('.')[0])) continue
    if (settingKeys.has(key)) continue
    if (!sourceSet.has(key)) problems.push(`${rel} 引用了未定义的键 ${key}`)
  }
}

// ── 3 · 占位符一致 ─────────────────────────────────────────────
const values = {}
for (const locale of [SOURCE, ...OTHERS]) {
  values[locale] = loadMessages(join(localesDir, `${locale}.ts`))
}
const flattenValues = (obj, prefix = '') =>
  Object.entries(obj).flatMap(([key, value]) =>
    value && typeof value === 'object'
      ? flattenValues(value, `${prefix}${key}.`)
      : [[`${prefix}${key}`, value]],
  )
for (const [key, text] of flattenValues(sourceMessages)) {
  // 复数形式用 `|` 分段（'one | other'），要**逐段**比，不能把整条一起比。
  const sourceForms = String(text).split('|')
  for (const locale of OTHERS) {
    const other = flattenValues(values[locale]).find(([k]) => k === key)?.[1]
    if (other === undefined) continue
    const otherForms = String(other).split('|')
    // 源语言只有一种形式时，只比第一段——其它语言的复数形态由该语言自己决定。
    const forms = sourceForms.length === 1 ? 1 : otherForms.length
    if (sourceForms.length > 1 && otherForms.length !== sourceForms.length) {
      problems.push(`${locale}.ts 的 ${key} 复数分段数不一致：${sourceForms.length} vs ${otherForms.length}`)
      continue
    }
    for (let i = 0; i < forms; i += 1) {
      const expected = placeholders(sourceForms[i])
      const actual = placeholders(otherForms[i])
      // 只比**集合**：vue-i18n 的具名插值与顺序无关，
      // 各语言语序不同是正常的（英文常把宾语放前面）。
      if ([...expected].sort().join(',') !== [...actual].sort().join(',')) {
        problems.push(`${locale}.ts 的 ${key} 占位符不一致：${expected} vs ${actual}`)
      }
    }
  }
}

// ── 结论 ───────────────────────────────────────────────────────
if (problems.length > 0) {
  console.error(`✗ 语言包校验失败（${problems.length} 项）：`)
  for (const p of problems) console.error(`  - ${p}`)
  process.exit(1)
}
console.log(`✓ 语言包校验通过：${sourceKeys.length} 个键，三种语言结构与占位符一致`)
