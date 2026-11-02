/**
 * 项目成员管理（项目详情页与跨项目成员页共用）。
 *
 * 两个页面的增删流程完全一样，差别只有两处：
 *   1. **能不能管**：详情页读后端算好的 my_role，跨项目页在前端用成员关系重算；
 *      权限判定各页自己算，这里不统一。
 *   2. **加到哪个项目**：详情页固定是当前项目，跨项目页是表单里选的那个
 *      ——这一条由调用方以参数传入。
 */
import { computed, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'

import { errorMessage } from '@/api/client'
import { addMember, listMemberCandidates, removeMember } from '@/api/projects'
import { translate } from '@/i18n'
import type { Id, MemberCandidate, ProjectRole } from '@/types'

/** 要移除的成员（两页的成员记录形状不同，公共字段一致）。 */
export interface RemovableMember {
  user_id: Id
  username: string
}

export interface UseProjectMembersOptions {
  /** 往哪个项目加成员。 */
  targetProject: () => string
  /** 目标项目为空时拦下提交（跨项目页要拦；详情页的项目永远非空）。 */
  requireTarget: boolean
  /** 增删成功后的刷新。 */
  reload: () => Promise<void> | void
}

export function useProjectMembers(options: UseProjectMembersOptions) {
  const { targetProject, requireTarget, reload } = options

  const dialogVisible = ref(false)
  const submitting = ref(false)
  const candidatesLoading = ref(false)
  const candidates = ref<MemberCandidate[]>([])

  const form = reactive({
    username: '',
    role: 'reader' as ProjectRole,
  })

  /** 候选用户按"是否已是本项目成员"分桶：已加入的提交后会覆盖角色。 */
  const existingCandidates = computed(() => candidates.value.filter((c) => c.current_role !== null))
  const newCandidates = computed(() => candidates.value.filter((c) => c.current_role === null))

  /** 拉取某个项目的候选用户。 */
  async function loadCandidates(project: string): Promise<void> {
    if (!project) {
      candidates.value = []
      return
    }
    candidatesLoading.value = true
    try {
      const data = await listMemberCandidates(project)
      candidates.value = data.candidates ?? []
    } catch (err) {
      ElMessage.error(errorMessage(err, translate('projectDetail.candidatesFailed')))
      candidates.value = []
    } finally {
      candidatesLoading.value = false
    }
  }

  /** 打开"添加成员"对话框：清空表单并拉候选名单。 */
  function openDialog(): void {
    form.username = ''
    form.role = 'reader'
    dialogVisible.value = true
    void loadCandidates(targetProject())
  }

  async function submit(): Promise<void> {
    if (requireTarget && !targetProject()) {
      ElMessage.warning(translate('projectDetail.selectProject'))
      return
    }
    if (!form.username) {
      ElMessage.warning(translate('projectDetail.selectUser'))
      return
    }
    submitting.value = true
    try {
      await addMember(targetProject(), {
        username: form.username,
        role: form.role,
      })
      ElMessage.success(translate('projectDetail.memberAdded'))
      dialogVisible.value = false
      await reload()
    } catch (err) {
      ElMessage.error(errorMessage(err, translate('projectDetail.addFailed')))
    } finally {
      submitting.value = false
    }
  }

  async function remove(member: RemovableMember, project: string): Promise<void> {
    try {
      await ElMessageBox.confirm(
        translate('projectDetail.removeConfirm', { username: member.username, project }),
        translate('projectDetail.removeTitle'),
        {
          type: 'warning',
          confirmButtonText: translate('projectDetail.remove'),
          cancelButtonText: translate('common.cancel'),
        },
      )
    } catch {
      // 用户取消，不是失败。
      return
    }
    try {
      await removeMember(project, member.user_id)
      ElMessage.success(translate('projectDetail.memberRemoved'))
      await reload()
    } catch (err) {
      ElMessage.error(errorMessage(err, translate('projectDetail.removeFailed')))
    }
  }

  return {
    dialogVisible,
    submitting,
    candidatesLoading,
    form,
    existingCandidates,
    newCandidates,
    loadCandidates,
    openDialog,
    submit,
    remove,
  }
}
