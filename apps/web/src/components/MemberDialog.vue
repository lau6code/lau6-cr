<script setup lang="ts">
/**
 * "添加成员"对话框。
 *
 * 项目详情页与跨项目成员页共用：候选用户、角色、提交动作两边完全一样，
 * 差别只有跨项目页多一个"目标项目"下拉——那一段用插槽插在表单最前面。
 */
import { useI18n } from 'vue-i18n'

import { formLabelWidth } from '@/i18n'
import type { MemberCandidate, ProjectRole } from '@/types'

const visible = defineModel<boolean>({ required: true })
const username = defineModel<string>('username', { required: true })
const role = defineModel<ProjectRole>('role', { required: true })

defineProps<{
  /** 候选用户是否在加载。 */
  candidatesLoading: boolean
  /** 尚未加入的候选用户。 */
  newCandidates: MemberCandidate[]
  /** 已是成员的候选用户。 */
  existingCandidates: MemberCandidate[]
  /** 提交中。 */
  submitting: boolean
  /** 目标项目没选时禁用用户选择（跨项目页要拦）。 */
  candidatesDisabled?: boolean
}>()

const emit = defineEmits<{ submit: [] }>()

const { t } = useI18n()
</script>

<template>
  <el-dialog v-model="visible" :title="t('projectDetail.addMember')" width="440px">
    <el-form :label-width="formLabelWidth(80)">
      <slot name="target" />

      <el-form-item :label="t('projectDetail.user')" required>
        <el-select
          v-model="username"
          filterable
          :loading="candidatesLoading"
          :disabled="candidatesDisabled"
          :placeholder="t('projectDetail.candidatesPlaceholder')"
          class="form-control"
        >
          <el-option-group v-if="newCandidates.length > 0" :label="t('projectDetail.addable')">
            <el-option
              v-for="candidate in newCandidates"
              :key="candidate.user_id"
              :label="
                candidate.display_name
                  ? t('common.parenthesized', { label: candidate.username, value: candidate.display_name })
                  : candidate.username
              "
              :value="candidate.username"
            />
          </el-option-group>
          <el-option-group
            v-if="existingCandidates.length > 0"
            :label="t('projectDetail.existingMembers')"
          >
            <el-option
              v-for="candidate in existingCandidates"
              :key="candidate.user_id"
              :label="
                (candidate.display_name
                  ? t('common.parenthesized', { label: candidate.username, value: candidate.display_name })
                  : candidate.username) +
                ` · ${t('projectDetail.currentRole')}: ${t(`roles.${candidate.current_role as ProjectRole}`)}`
              "
              :value="candidate.username"
            />
          </el-option-group>
        </el-select>
        <div class="form-hint">{{ t('projectDetail.candidatesHint') }}</div>
      </el-form-item>

      <el-form-item :label="t('common.role')">
        <el-select v-model="role" class="form-control">
          <el-option :label="t('common.parenthesized', { label: t('roles.admin'), value: 'admin' })" value="admin" />
          <el-option
            :label="t('common.parenthesized', { label: t('roles.developer'), value: 'developer' })"
            value="developer"
          />
          <el-option :label="t('common.parenthesized', { label: t('roles.reader'), value: 'reader' })" value="reader" />
        </el-select>
      </el-form-item>
    </el-form>

    <template #footer>
      <el-button @click="visible = false">{{ t('common.cancel') }}</el-button>
      <el-button type="primary" :loading="submitting" @click="emit('submit')">
        {{ t('projectDetail.add') }}
      </el-button>
    </template>
  </el-dialog>
</template>
