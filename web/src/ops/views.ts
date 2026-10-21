import type { Role } from '../api/types'

/** 控制台的三个视图。切换视图只换「在看哪一组页面」，不改变服务端判定。 */
export type View = 'user' | 'merchant' | 'admin'

/** 账号实际拥有的视图；没有角色字段的旧响应按 user 处理。 */
export function viewsFor(role: Role | undefined): View[] {
  switch (role) {
    case 'admin':
      return ['user', 'merchant', 'admin']
    case 'merchant':
      return ['user', 'merchant']
    default:
      return ['user']
  }
}

/** 路径落在哪个视图里。 */
export function viewOfPath(pathname: string): View {
  if (pathname === '/app/admin' || pathname.startsWith('/app/admin/')) return 'admin'
  if (pathname === '/app/merchant' || pathname.startsWith('/app/merchant/')) return 'merchant'
  return 'user'
}

export function homeOf(view: View): string {
  if (view === 'admin') return '/app/admin/channels'
  if (view === 'merchant') return '/app/merchant/channels'
  return '/app'
}
