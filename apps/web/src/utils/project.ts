/**
 * 项目对象的合并。
 */

import type { Project } from '@/types'

/**
 * 把服务端返回的最新项目合并进当前项目。
 *
 * ⚠️ 合并而不是整行替换：my_role 是"我在这个项目里的角色"，
 * 属于**查看者**的上下文。编辑接口的响应里它可能为空，
 * 一旦被覆盖掉，"编辑项目 / 添加成员"入口会在保存后凭空消失。
 */
export function mergeProject(current: Project, updated: Project): Project {
  return {
    ...current,
    ...updated,
    my_role: updated.my_role ?? current.my_role,
  }
}
