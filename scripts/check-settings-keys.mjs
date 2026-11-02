#!/usr/bin/env node
/**
 * 设置项清单一致性校验。
 *
 * 设置项的 key 有三份清单：后端定义、前端设置页分组、设计文档。
 * 历史上它们各改各的，于是漏过项——
 * `core.project_token_ttl_seconds` 在后端定义了、文档也写了，
 * 前端 `SettingsView.vue` 的 GROUPS 里却没有，
 * 设置页不渲染它，改不了，只能靠 env。
 *
 * 这个脚本守住三份清单一致：
 *
 *   1. 后端 `AllSettingDefs()` 的每个 key 都得出现在前端 GROUPS 里；
 *   2. 前端 GROUPS 里不许出现后端没有的 key（那是改废了的入口）；
 *   3. 文档 §3 的清单与后端一致，§6.5 的 YAML 路径映射表也覆盖全部 key。
 *
 * 用法：
 *   node scripts/check-settings-keys.mjs
 *
 * 退出码非 0 表示有错——CI 或提交前跑一下。
 */
import { readFileSync } from 'node:fs'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const here = dirname(fileURLToPath(import.meta.url))
const root = resolve(here, '..')

const backendFile = join(root, 'apps/api/internal/service/settings_defs.go')
const webFile = join(root, 'apps/web/src/views/admin/SettingsView.vue')
const docFile = join(root, 'docs/design/14-settings.md')

const problems = []

/**
 * 解析后端的设置项定义。
 *
 * 真源是 `AllSettingDefs()` 里的 `Key:` 字面量——设置项只有这一处来源。
 * YAMLPath 紧跟其后（可能换行），导出与回读都靠它。
 */
function parseBackend(text) {
  const keyRe = /Key:\s*"([^"]+)"/g
  const hits = [...text.matchAll(keyRe)]
  const defs = []
  for (let i = 0; i < hits.length; i += 1) {
    const key = hits[i][1]
    // YAMLPath 在本项内、下一项之前；越界说明这一项没写路径。
    const from = hits[i].index + hits[i][0].length
    const to = i + 1 < hits.length ? hits[i + 1].index : text.length
    const segment = text.slice(from, to)
    const yaml = /YAMLPath:\s*\[\]string\{([^}]*)\}/.exec(segment)
    if (!yaml) {
      problems.push(`后端定义 ${key} 没有 YAMLPath`)
      continue
    }
    const yamlPath = yaml[1]
      .split(',')
      .map((part) => part.trim().replace(/^"|"$/g, ''))
      .filter(Boolean)
    defs.push({ key, yamlPath: yamlPath.join('.') })
  }
  return defs
}

/** 解析后端的内部键清单（不对外暴露、也不进设置页）。 */
function parseInternalKeys(text) {
  const map = /var\s+internalSettingKeys\s*=\s*map\[string\]bool\{([\s\S]*?)\}/.exec(text)
  if (!map) {
    problems.push('后端找不到 internalSettingKeys 清单')
    return []
  }
  return [...map[1].matchAll(/"([^"]+)"\s*:\s*true/g)].map((m) => m[1])
}

/** 解析前端设置页 GROUPS 里的 key。 */
function parseFrontend(text) {
  const keys = []
  for (const block of text.matchAll(/keys:\s*\[([\s\S]*?)\]/g)) {
    for (const key of block[1].matchAll(/'([^']+)'/g)) keys.push(key[1])
  }
  return keys
}

/** 取文档里某个标题到下一个二级标题之间的内容。 */
function sliceSection(text, heading) {
  const start = text.indexOf(heading)
  if (start < 0) {
    problems.push(`文档找不到章节 ${heading}`)
    return ''
  }
  const rest = text.slice(start + heading.length)
  const end = rest.search(/\n## /)
  return end < 0 ? rest : rest.slice(0, end)
}

/** 取表格第一列（反引号包着的 key）。 */
function parseTableKeys(section) {
  const keys = []
  for (const line of section.split('\n')) {
    const m = /^\|\s*`([^`]+)`\s*\|/.exec(line)
    if (m) keys.push(m[1])
  }
  return keys
}

/** 取表格前两列（key 与 YAML 路径）。 */
function parseTableRows(section) {
  const rows = []
  for (const line of section.split('\n')) {
    const m = /^\|\s*`([^`]+)`\s*\|\s*`([^`]+)`\s*\|/.exec(line)
    if (m) rows.push({ key: m[1], yamlPath: m[2] })
  }
  return rows
}

/** 找出重复出现的 key。 */
function duplicates(keys) {
  const seen = new Set()
  const dup = new Set()
  for (const key of keys) {
    if (seen.has(key)) dup.add(key)
    seen.add(key)
  }
  return [...dup]
}

const backendDefs = parseBackend(readFileSync(backendFile, 'utf8'))
const internalKeys = parseInternalKeys(readFileSync(backendFile, 'utf8'))
const frontendKeys = parseFrontend(readFileSync(webFile, 'utf8'))
const docText = readFileSync(docFile, 'utf8')
const docListKeys = parseTableKeys(sliceSection(docText, '## 3. 设置项清单'))
const docMapRows = parseTableRows(sliceSection(docText, '### 6.5 key → YAML 路径映射'))

const backendKeys = backendDefs.map((d) => d.key)
const backendSet = new Set(backendKeys)
const frontendSet = new Set(frontendKeys)

// ── 1 · 后端 → 前端 ───────────────────────────────────────────
for (const key of backendKeys) {
  if (!frontendSet.has(key)) {
    problems.push(`前端设置页没有 ${key} 的入口（后端已定义、文档已登记）`)
  }
}

// ── 2 · 前端 → 后端 ───────────────────────────────────────────
for (const key of frontendKeys) {
  if (!backendSet.has(key)) {
    problems.push(`前端设置页列了后端不存在的 ${key}`)
  }
}
for (const key of duplicates(frontendKeys)) {
  problems.push(`前端设置页重复列了 ${key}`)
}
for (const key of duplicates(backendKeys)) {
  problems.push(`后端重复定义了 ${key}`)
}

// ── 3 · 文档 §3 ↔ 后端 ────────────────────────────────────────
const docSet = new Set(docListKeys)
for (const key of backendKeys) {
  if (!docSet.has(key)) {
    problems.push(`文档 §3 的清单里没有 ${key}`)
  }
}
const internalSet = new Set(internalKeys)
for (const key of docListKeys) {
  if (!backendSet.has(key) && !internalSet.has(key)) {
    problems.push(`文档 §3 列了后端不存在的 ${key}`)
  }
}
for (const key of internalKeys) {
  if (!docSet.has(key)) {
    problems.push(`文档 §3 的清单里没有内部键 ${key}`)
  }
}

// ── 4 · 文档 §6.5 的 YAML 路径映射覆盖全部设置项 ──────────────
const defByKey = new Map(backendDefs.map((d) => [d.key, d.yamlPath]))
for (const row of docMapRows) {
  if (row.key.endsWith('.*')) {
    // 通配行（如 `access.*`）只覆盖前缀，不校验具体路径。
    continue
  }
  const expected = defByKey.get(row.key)
  if (expected === undefined) continue
  if (expected !== row.yamlPath) {
    problems.push(`文档 §6.5 的 ${row.key} 映射到 ${row.yamlPath}，后端是 ${expected}`)
  }
}
for (const key of backendKeys) {
  const covered = docMapRows.some((row) => {
    if (row.key.endsWith('.*')) return key.startsWith(row.key.slice(0, -1))
    return row.key === key
  })
  if (!covered) {
    problems.push(`文档 §6.5 的 YAML 路径映射表没有 ${key}`)
  }
}

// ── 结论 ───────────────────────────────────────────────────────
if (problems.length > 0) {
  console.error(`✗ 设置项清单校验失败（${problems.length} 项）：`)
  for (const p of problems) console.error(`  - ${p}`)
  process.exit(1)
}
console.log(
  `✓ 设置项清单校验通过：${backendKeys.length} 个设置项 + ${internalKeys.length} 个内部键，` +
    '后端定义、前端设置页、文档三处一致',
)
