/**
 * 项目角色的展示色调。
 *
 * 成员角色（ProjectRole）与项目令牌角色（ProjectTokenRole）取值相同，
 * 但含义不同：前者随成员关系变化，后者是创建时固化的——
 * 展示上没差别，所以共用一份色调表。
 */
import type { ProjectRole, ProjectTokenRole } from '@/types'

export const ROLE_TONE: Record<ProjectRole | ProjectTokenRole, string> = {
  admin: 'status-err',
  developer: 'status-info',
  reader: 'status-muted',
}
