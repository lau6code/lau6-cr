import { ElMessage } from 'element-plus'

import { translate } from '@/i18n'

/**
 * 复制文本到剪贴板。
 *
 * ⚠️ `navigator.clipboard` 只在**安全上下文**（https 或 localhost）可用，
 * 而部署环境常常是 http://<host>:5000 这种——那里它压根不存在。
 * 所以必须退回 `document.execCommand('copy')` 那条老路，
 * 否则"复制"按钮在生产环境上会静默失败。
 *
 * 返回是否成功：调用方据此提示"复制失败，请手动选中"。
 */
export async function copyText(text: string): Promise<boolean> {
  if (!text) return false

  // ① 现代 API（https / localhost）
  if (navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text)
      return true
    } catch {
      // 权限被拒时继续走兜底
    }
  }

  // ② 兜底：临时 textarea + execCommand
  const ta = document.createElement('textarea')
  ta.value = text
  // 不能用 display:none——隐藏的元素无法被选中，复制会失败。
  ta.setAttribute('readonly', '')
  ta.style.position = 'fixed'
  ta.style.top = '-1000px'
  ta.style.opacity = '0'
  document.body.appendChild(ta)
  try {
    ta.select()
    ta.setSelectionRange(0, text.length)
    return document.execCommand('copy')
  } catch {
    return false
  } finally {
    document.body.removeChild(ta)
  }
}

/**
 * 复制文本并提示结果。
 *
 * 失败也必须提示：复制不到时用户只能手动选中，
 * 静默返回等于让用户以为复制成功了。
 */
export async function copyAndNotify(text: string): Promise<void> {
  const ok = await copyText(text)
  if (ok) {
    ElMessage.success(translate('common.copied'))
  } else {
    ElMessage.warning(translate('common.copyFailed'))
  }
}
