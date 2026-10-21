import type { Role } from '../api/types'

/** 控制台的三个视图。切换视图只换「在看哪一组页面」，不改变服务端判定。 */
export type View = 'user' | 'merchant' | 'admin'

/**
 * 同一批页面两种主语：商家看到的是自己名下，管理员看到的是全站。
 *
 * 三页的标题、读数标签与侧栏都在这里取，不各自判断角色——一处写死，
 * 另一处就会在某个身份下说错。请求本身发到同一个地址，范围由服务端定。
 */
export type Subject = 'mine' | 'site'

export function subjectOf(role: Role | undefined): Subject {
  return role === 'admin' ? 'site' : 'mine'
}

/**
 * 账号实际拥有的视图；没有角色字段的旧响应按 user 处理。
 *
 * 管理员也有商家视图，但它只有「两种主语」的那三页（见
 * {@link MERCHANT_SHARED_PATHS}）：概览、模型、上游的文案会按会话角色
 * 说清是「我的」还是「全站」。渠道与计价两页仍然只对商家开放——那两页
 * 写的是「你名下」，管理员没有「以谁的身份看」这层归属。
 *
 * 顺序有讲究：路由进不去时侧栏与切换器落到最后一个视图，管理员该落回
 * 「管理」，所以 admin 排在末尾。
 */
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

/**
 * 商家视图里同一批页面、两种主语的三页。
 *
 * 管理员进这三页看到的是全站数据；其余商家页（渠道、计价、设置）仍然
 * 只对商家开放。
 */
export const MERCHANT_SHARED_PATHS = [
  '/app/merchant/overview',
  '/app/merchant/models',
  '/app/merchant/upstreams',
] as const

/**
 * 地址是不是商家视图里那三页之一。
 *
 * `/app/merchant` 本身也认——它是重定向到概览的入口，拦下来会让管理员
 * 敲这个地址时看到无权限而不是概览。但这里只认这三页各自的子路径，
 * 不能用 `/app/merchant` 当前缀：那样 `/app/merchant/settings` 之类
 * 也会被一起放行。
 */
export function isSharedMerchantPath(pathname: string): boolean {
  if (pathname === '/app/merchant') return true
  return MERCHANT_SHARED_PATHS.some(
    (path) => pathname === path || pathname.startsWith(`${path}/`),
  )
}

/**
 * 这个账号能不能打开这个地址。
 *
 * `RequireView` 与侧栏的「当前视图」读同一份判定，免得正文说无权限、
 * 标题却按视图首页写。越权判定最终仍在服务端。
 */
export function viewAllowed(role: Role | undefined, pathname: string): boolean {
  const view = viewOfPath(pathname)
  if (!viewsFor(role).includes(view)) return false
  if (view === 'merchant' && role === 'admin') return isSharedMerchantPath(pathname)
  return true
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
  if (view === 'merchant') return '/app/merchant/overview'
  return '/app'
}
