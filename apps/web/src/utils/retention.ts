/**
 * 保留策略的呈现文案。
 *
 * 规则摘要要在两处出现：项目保留策略页的规则提示，
 * 与管理页的"项目级策略总览"。放一个地方，免得两处说法不一致。
 *
 * ⚠️ 这个模块不在组件里，拿不到 useI18n()，所以走 i18n 模块的
 *    translate() 入口（见 src/i18n/index.ts）。
 */
import { translate } from '@/i18n'
import type { RetentionPolicy, RetentionRule, RetentionTemplate } from '@/types'

/** 参数单位 → 标签；没有对应说法时按原文显示，不吞掉。 */
export function unitLabel(unit: string | undefined): string {
  if (!unit) return ''
  if (unit === 'count') return translate('retention.unitCount')
  if (unit === 'days') return translate('retention.unitDays')
  return unit
}

/**
 * 模板清单读不到时退回模板原名——宁可显示英文模板名，
 * 也不能显示空白或编一条规则出来。
 */
function ruleSummary(rule: RetentionRule, templates: RetentionTemplate[] = []): string {
  const tpl = templates.find((item) => item.template === rule.template)
  const label = tpl?.label ?? rule.template
  const params = Object.entries(rule.params ?? {})
    .map(([key, value]) => {
      const unit = unitLabel(tpl?.params.find((p) => p.name === key)?.unit)
      return `${key}=${value}${unit}`
    })
    .join('，')
  const selector = rule.tag_selector
    ? translate('retention.ruleSelector', { selector: rule.tag_selector })
    : ''
  return params ? `${label}（${params}）${selector}` : `${label}${selector}`
}

/**
 * 策略没启用或没有规则时给一句明确说明——
 * "没规则"和"有规则但没命中"在页面上长得一样，得说清楚。
 */
export function policyRuleSummaries(
  policy: RetentionPolicy | null | undefined,
  templates: RetentionTemplate[] = [],
): string[] {
  if (!policy) return []
  if (!policy.enabled) return [translate('retention.disabledSummary')]
  if ((policy.rules ?? []).length === 0) return [translate('retention.noRulesSummary')]
  return policy.rules.map((rule) => ruleSummary(rule, templates))
}
