/**
 * 向导的步骤推进与前置校验。
 *
 * 步骤用 key 而不是下标——external 形态下会少一步（没有存储步骤），
 * 下标全乱。每一步的前置校验不通过就不许进下一步。
 */
import { computed, ref, type Ref } from 'vue'
import { ElMessage } from 'element-plus'

import { translate } from '@/i18n'
import type { SetupRequest, SetupStorage } from '@/types'

/** 向导的步骤。 */
export type SetupStepKey = 'database' | 'cache' | 'storage' | 'public_url' | 'admin' | 'confirm'

const STEP_LABELS: Record<SetupStepKey, string> = {
  database: 'setup.stepDatabase',
  cache: 'setup.stepCache',
  storage: 'setup.stepStorage',
  public_url: 'setup.stepPublicUrl',
  admin: 'setup.stepAdmin',
  confirm: 'setup.stepConfirm',
}

/** 步骤推进与校验用到的表单字段。 */
export type SetupWizardForm = Pick<
  SetupRequest,
  'database' | 'cache' | 'public_url' | 'admin_username' | 'admin_password'
> & {
  storage: SetupStorage
  admin_password_confirm: string
}

export interface UseSetupWizardOptions {
  form: SetupWizardForm
  /** distribution 是否与本进程同容器（external 形态下没有存储步骤）。 */
  distributionEmbedded: Ref<boolean>
  /** 某个设置项是否被环境变量钉死。 */
  isPinned: (key: string) => boolean
}

export function useSetupWizard(options: UseSetupWizardOptions) {
  const { form, distributionEmbedded, isPinned } = options

  const activeStep = ref(0)

  /** 步骤顺序（按形态裁剪）。 */
  const stepOrder = computed<SetupStepKey[]>(() => {
    const order: SetupStepKey[] = ['database', 'cache']
    if (distributionEmbedded.value) order.push('storage')
    order.push('public_url', 'admin', 'confirm')
    return order
  })

  const STEPS = computed(() => stepOrder.value.map((key) => translate(STEP_LABELS[key])))

  /** 当前步骤的 key。 */
  const activeKey = computed(() => stepOrder.value[activeStep.value])

  const isLastStep = computed(() => activeStep.value === STEPS.value.length - 1)

  /** 每步的前置校验，返回 false 表示不允许进入下一步。 */
  function validateStep(step: SetupStepKey): boolean {
    if (step === 'database') {
      if (form.database.driver === 'sqlite') {
        if (!form.database.sqlite_path.trim()) {
          ElMessage.warning(translate('setup.sqlitePathRequired'))
          return false
        }
      } else {
        if (!form.database.host.trim()) {
          ElMessage.warning(translate('setup.dbHostRequired'))
          return false
        }
        if (!form.database.username.trim()) {
          ElMessage.warning(translate('setup.dbUsernameRequired'))
          return false
        }
        if (!form.database.name.trim()) {
          ElMessage.warning(translate('setup.dbNameRequired'))
          return false
        }
      }
    }
    if (step === 'cache' && form.cache.driver === 'redis') {
      // ⚠️ 被 env 钉死的项不校验：值由环境变量说了算，
      //    拿"没填"去卡他只会把初始化卡死（后端也会忽略这个值）。
      if (!isPinned('cache.redis.addr') && !form.cache.redis.addr.trim()) {
        ElMessage.warning(translate('setup.cacheRequired'))
        return false
      }
      // ⚠️ 数据库编号必填：共享 Redis 上 db 0 常被别人占着，
      //    默默用 0 等于把签名密钥写进别人的库（12-cache.md §2）。
      //
      //    用 typeof 判而不是 `=== null`：输入框被清空时组件可能给出
      //    undefined，两种都得当成"没填"。
      if (!isPinned('cache.redis.db') && typeof form.cache.redis.db !== 'number') {
        ElMessage.warning(translate('setup.cacheDbRequired'))
        return false
      }
    }
    if (step === 'storage' && form.storage.driver === 's3') {
      if (!form.storage.s3.endpoint.trim()) {
        ElMessage.warning(translate('setup.storageRequired'))
        return false
      }
      if (!form.storage.s3.bucket.trim()) {
        ElMessage.warning(translate('setup.storageBucketRequired'))
        return false
      }
    }
    if (step === 'public_url') {
      const url = form.public_url.trim()
      // 被环境变量钉死时这一栏是只读的：值由 env 决定，用户改不了，
      // 拿"格式不对"去卡他只会把初始化卡死（后端也会忽略这个值）。
      if (!isPinned('core.public_url') && !/^https?:\/\/[^\s]+$/.test(url)) {
        ElMessage.warning(translate('setup.publicUrlInvalid'))
        return false
      }
    }
    if (step === 'admin') {
      if (!/^[a-zA-Z0-9._-]{3,32}$/.test(form.admin_username.trim())) {
        ElMessage.warning(translate('setup.adminUsernameInvalid'))
        return false
      }
      if (form.admin_password.length < 12) {
        ElMessage.warning(translate('setup.adminPasswordTooShort'))
        return false
      }
      if (form.admin_password !== form.admin_password_confirm) {
        ElMessage.warning(translate('setup.adminPasswordMismatch'))
        return false
      }
    }
    return true
  }

  function next(): void {
    if (!validateStep(activeKey.value)) return
    if (activeStep.value < STEPS.value.length - 1) {
      activeStep.value += 1
    }
  }

  function prev(): void {
    if (activeStep.value > 0) {
      activeStep.value -= 1
    }
  }

  return { STEPS, activeStep, activeKey, isLastStep, next, prev }
}
