<script setup lang="ts">
/**
 * 编辑项目对话框（项目列表页与项目详情页共用）。
 *
 * 权限层级（与后端 PATCH /api/v1/projects/{name} 一致）：
 *
 *   · 显示名 / 描述 → 项目管理员 或 系统管理员
 *   · 可见性        → 只有系统管理员
 *
 * 项目管理员提交时**不带** visibility 字段：后端对该字段单独校验系统管理员，
 * 带上就会被 403 挡掉。
 */
import { computed, reactive, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import type { FormInstance, FormRules } from 'element-plus'
import { useI18n } from 'vue-i18n'

import { errorMessage } from '@/api/client'
import { updateProject } from '@/api/projects'
import type { UpdateProjectPayload } from '@/api/projects'
import { useAuthStore } from '@/stores/auth'
import type { Project, ProjectVisibility } from '@/types'
import { formLabelWidth } from '@/i18n'

const { t } = useI18n()

const props = defineProps<{
  modelValue: boolean
  project: Project | null
}>()

const emit = defineEmits<{
  'update:modelValue': [boolean]
  /** 保存成功后把最新项目抛给父组件。 */
  updated: [Project]
}>()

const auth = useAuthStore()
const submitting = ref(false)
const formRef = ref<FormInstance>()

const form = reactive({
  display_name: '',
  description: '',
  visibility: 'private' as ProjectVisibility,
})

const visible = computed({
  get: () => props.modelValue,
  set: (v: boolean) => emit('update:modelValue', v),
})

/** 可见性是系统级开关：项目管理员只能看，不能改。 */
const canChangeVisibility = computed(() => auth.isSystemAdmin)

const rules = computed<FormRules>(() => ({
  display_name: [{ required: true, message: t('projectEdit.displayNameRequired'), trigger: 'blur' }],
}))

// 每次打开都从当前项目重新灌值，避免上一次的残留。
watch(
  () => [props.modelValue, props.project] as const,
  ([open, project]) => {
    if (!open || !project) return
    form.display_name = project.display_name ?? ''
    form.description = project.description ?? ''
    form.visibility = project.visibility
    formRef.value?.clearValidate()
  },
  { immediate: true },
)

async function submit(): Promise<void> {
  if (!formRef.value || !props.project) return
  const valid = await formRef.value.validate().catch(() => false)
  if (!valid) return

  const payload: UpdateProjectPayload = {
    display_name: form.display_name.trim(),
    description: form.description.trim(),
  }
  if (canChangeVisibility.value) payload.visibility = form.visibility

  submitting.value = true
  try {
    const result = await updateProject(props.project.name, payload)
    ElMessage.success(t('projectEdit.updated'))
    visible.value = false
    emit('updated', result.project)
  } catch (err) {
    ElMessage.error(errorMessage(err, t('projectEdit.failed')))
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <el-dialog v-model="visible" :title="t('projectEdit.title')" width="480px">
    <!-- 切语言会换掉 rules 对象：别让它顺手触发一次校验，
         否则没填过的字段会凭空冒出必填错误。 -->
    <el-form
      ref="formRef"
      :model="form"
      :rules="rules"
      :validate-on-rule-change="false"
      :label-width="formLabelWidth(90)"
    >
      <el-form-item :label="t('projectEdit.formName')">
        <el-input :model-value="project?.name" disabled />
        <div class="form-hint">{{ t('projectEdit.nameHint') }}</div>
      </el-form-item>

      <el-form-item :label="t('projectEdit.formDisplayName')" prop="display_name">
        <el-input v-model="form.display_name" :placeholder="t('projectEdit.displayNamePlaceholder')" />
      </el-form-item>

      <el-form-item :label="t('projectEdit.formDescription')" prop="description">
        <el-input
          v-model="form.description"
          type="textarea"
          :rows="3"
          :placeholder="t('projectEdit.descriptionPlaceholder')"
        />
      </el-form-item>

      <el-form-item :label="t('projectEdit.formVisibility')" prop="visibility">
        <el-select v-model="form.visibility" class="form-control" :disabled="!canChangeVisibility">
          <el-option :label="t('projectEdit.visibilityPrivateOption')" value="private" />
          <el-option :label="t('projectEdit.visibilityPublicOption')" value="public" />
        </el-select>
        <div class="form-hint">
          {{
            canChangeVisibility
              ? t('projectEdit.publicHint')
              : t('projectEdit.systemOnlyHint')
          }}
        </div>
      </el-form-item>
    </el-form>

    <template #footer>
      <el-button @click="visible = false">{{ t('common.cancel') }}</el-button>
      <el-button type="primary" :loading="submitting" @click="submit">{{ t('common.save') }}</el-button>
    </template>
  </el-dialog>
</template>
