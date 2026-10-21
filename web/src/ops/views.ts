import type { Role } from '../api/types'

/** 控制台的三个视图。切换视图只换「在看哪一组页面」，不改变服务端判定。 */
export type View = 'user' | 'merchant' | 'admin'

/**
 * 账号实际拥有的视图；没有角色字段的旧响应按 user 处理。
 *
 * 管理员没有商家视图：商家页显示的是「调用者名下」的资源，而管理员没有
 * 「以谁的身份看」这层信息——列出来的是全平台数据，文案却在说「你自己的」。
 * `RequireView` 用同一张表，所以直接敲 `/app/merchant/*` 也会落到无权限页。
 */
export function viewsFor(role: Role | undefined): View[] {
  switch (role) {
    case 'admin':
      return ['user', 'admin']
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

/**
 * 设置页在各自视图里的落点。
 *
 * 设置本身是账号级的，三处渲染同一页；分成不同地址是为了让「在看哪个视图」
 * 仍然由地址栏决定——否则从运营视图点进设置，页面会被判成用户视图。
 */
export function settingsOf(view: View): string {
  if (view === 'admin') return '/app/admin/settings'
  if (view === 'merchant') return '/app/merchant/settings'
  return '/app/settings'
}

export function homeOf(view: View): string {
  if (view === 'admin') return '/app/admin/channels'
  if (view === 'merchant') return '/app/merchant/channels'
  return '/app'
}
