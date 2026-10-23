import { useEffect, useRef, useState } from 'react'
import { Link, NavLink, Outlet, useLocation, useNavigate } from 'react-router-dom'
import * as DialogPrimitive from '@radix-ui/react-dialog'
import {
  Boxes,
  ChartColumn,
  ChevronLeft,
  KeyRound,
  Layers,
  LayoutGrid,
  Link2,
  LogOut,
  Menu,
  Coins,
  Rocket,
  Settings,
  Tags,
  User,
  Users,
  X,
  type LucideIcon,
} from 'lucide-react'
import { useAuth } from '../auth/AuthProvider'
import { useI18n } from '../i18n'
import type { MessageKey } from '../i18n/zh'
import { useMediaQuery, useIsMobile, useIsNarrow } from '../hooks/useMediaQuery'
import { RouteErrorBoundary } from './ErrorBoundary'
import { PageTitleReporter, useDocumentTitle, type ReportedTitle } from './PageTitle'
import { homeOf, isSharedMerchantPath, settingsOf, subjectOf, viewAllowed, viewOfPath, viewsFor, type View } from '../ops/views'
import { ActionSheet, CommandPalette, Segmented, Kbd } from './ui'
import { DropdownMenu, DropdownMenuTrigger, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator } from './ui'
import { Button } from './ui'
import { Search } from 'lucide-react'
import { cn } from './ui/lib'

type IconType = LucideIcon

export const APP_NAV: Array<{ to: string; key: MessageKey; icon: IconType; group: 'account' | 'access' }> = [
  { to: '/app', key: 'nav.overview', icon: LayoutGrid, group: 'account' },
  { to: '/app/keys', key: 'nav.keys', icon: KeyRound, group: 'account' },
  { to: '/app/usage', key: 'nav.usage', icon: ChartColumn, group: 'account' },
  { to: '/app/models', key: 'nav.models', icon: Boxes, group: 'access' },
  { to: '/app/quickstart', key: 'nav.quickstart', icon: Rocket, group: 'access' },
]

type NavItem = { to: string; key: MessageKey; icon: IconType }

/**
 * 运营视图的导航。
 *
 * merchant 前三项是运营页：同类型同地址的渠道在「上游」归组，在供模型与
 * 定价状态在「模型」。后两项是商家自己那部分：渠道与计价（只有自己的规则，
 * 没有成本页）。模型目录与别名是平台级资源，没有归属可过滤，不在商家视图里出现。
 */
const OPS_NAV: Record<Exclude<View, 'user'>, NavItem[]> = {
  merchant: [
    { to: '/app/merchant/overview', key: 'nav.overview', icon: LayoutGrid },
    { to: '/app/merchant/models', key: 'ops.nav.merchantModels', icon: Boxes },
    { to: '/app/merchant/upstreams', key: 'ops.nav.upstreams', icon: Link2 },
    { to: '/app/merchant/channels', key: 'ops.nav.channels', icon: Layers },
    { to: '/app/merchant/pricing', key: 'ops.nav.pricing', icon: Coins },
  ],
  admin: [
    { to: '/app/admin/channels', key: 'ops.nav.channels', icon: Layers },
    { to: '/app/admin/models', key: 'ops.nav.models', icon: Boxes },
    { to: '/app/admin/users', key: 'ops.nav.users', icon: Users },
    { to: '/app/admin/aliases', key: 'ops.nav.aliases', icon: Tags },
    { to: '/app/admin/pricing', key: 'ops.nav.pricing', icon: Coins },
  ],
}

/** 管理员那些进不了底栏的页：从「运营」这个二级入口进。 */
const ADMIN_SECONDARY_PATHS = ['/app/admin/models', '/app/admin/aliases', '/app/admin/pricing'] as const

type BottomTab = {
  key: MessageKey
  icon: IconType
  to?: string | 'settings'
  sheet?: 'ops'
  owns?: readonly string[]
}

type BottomNavKey = View | 'merchantSite'

const BOTTOM_NAV: Record<BottomNavKey, BottomTab[]> = {
  user: [
    { to: '/app', key: 'nav.overview', icon: LayoutGrid },
    { to: '/app/keys', key: 'nav.keys', icon: KeyRound },
    { to: '/app/usage', key: 'nav.usage', icon: ChartColumn },
    { to: '/app/models', key: 'nav.models', icon: Boxes },
    { to: 'settings', key: 'nav.settings', icon: Settings },
  ],
  merchant: [
    { to: '/app/merchant/overview', key: 'nav.overview', icon: LayoutGrid },
    { to: '/app/merchant/channels', key: 'ops.nav.channels', icon: Layers },
    { to: '/app/usage', key: 'nav.usage', icon: ChartColumn },
    { to: '/app/merchant/account', key: 'nav.account', icon: User },
    { to: 'settings', key: 'nav.settings', icon: Settings },
  ],
  merchantSite: [
    { to: '/app/merchant/overview', key: 'nav.overview', icon: LayoutGrid },
    { to: '/app/merchant/models', key: 'ops.nav.merchantModels', icon: Boxes },
    { to: '/app/merchant/upstreams', key: 'ops.nav.upstreams', icon: Link2 },
    { to: '/app/usage', key: 'nav.usage', icon: ChartColumn },
  ],
  admin: [
    { to: '/app/merchant/overview', key: 'nav.overview', icon: LayoutGrid },
    { to: '/app/admin/channels', key: 'ops.nav.channels', icon: Layers },
    { to: '/app/usage', key: 'nav.usage', icon: ChartColumn },
    { to: '/app/admin/users', key: 'ops.nav.users', icon: Users },
    { sheet: 'ops', key: 'nav.ops', icon: Layers, owns: ADMIN_SECONDARY_PATHS },
  ],
}

function initials(name: string | null | undefined): string {
  const value = (name ?? '').trim()
  if (!value) return '?'
  return Array.from(value)[0].toUpperCase()
}

function BrandMark({ size = 26 }: { size?: number }) {
  return (
    <span
      aria-hidden="true"
      className="grid shrink-0 place-items-center rounded-sm bg-primary text-primary-foreground font-bold"
      style={{ width: size, height: size, fontSize: size * 0.46 }}
    >
      L
    </span>
  )
}

export function AppShell() {
  const { t } = useI18n()
  const { user, logout, sessionExpired } = useAuth()
  const navigate = useNavigate()
  const location = useLocation()
  const narrow = useIsNarrow()
  const mobile = useIsMobile()
  // 矮而宽的窗口（例如横放的手机 812×375）：侧栏展开后自己也要滚，主列也要滚，
  // 同一页出现两条滚动条。默认收起侧栏，只留主列那一条；要看导航就点 logo 展开。
  const short = useMediaQuery('(max-height: 519.98px)')
  const [shortExpanded, setShortExpanded] = useState(false)
  const [manualCollapsed, setManualCollapsed] = useState(false)
  const [drawer, setDrawer] = useState(false)
  const [opsSheet, setOpsSheet] = useState(false)
  const [paletteOpen, setPaletteOpen] = useState(false)
  const mainRef = useRef<HTMLElement | null>(null)
  const collapsed = narrow || (short ? !shortExpanded : manualCollapsed)

  // 会话过期那一刻 `user` 先变成 null。门槛故意把页面留在原地（见 `RequireAuth`），
  // 侧栏不能反过来切回「用户」视图——那看起来就像被踢到了另一个页面，管理员还会
  // 丢掉自己那一组导航。记住最后一次非空角色，过期期间沿用它。
  const lastRoleRef = useRef(user?.role)
  if (user?.role) lastRoleRef.current = user.role
  const role = user?.role ?? (sessionExpired ? lastRoleRef.current : undefined)

  const views = viewsFor(role)
  const routeView = viewOfPath(location.pathname)
  const allowed = viewAllowed(role, location.pathname)
  const view = allowed ? routeView : views[views.length - 1]
  const siteWide = subjectOf(role) === 'site'
  const merchantItems = siteWide
    ? OPS_NAV.merchant.filter((item) => isSharedMerchantPath(item.to))
    : OPS_NAV.merchant
  const navItems: NavItem[] =
    view === 'user' ? APP_NAV : view === 'merchant' ? merchantItems : OPS_NAV[view]
  const settings = settingsOf(view === 'merchant' && siteWide ? 'admin' : view)
  const account = view === 'merchant' && !siteWide ? '/app/merchant/account' : null
  const tabs = BOTTOM_NAV[view === 'merchant' && siteWide ? 'merchantSite' : view]

  useEffect(() => {
    setDrawer(false)
  }, [location.pathname])

  // `/app` 这一项不能按前缀匹配：它是每个子路由的前缀，会先把设置页之类
  // 没有进导航的页面全抢成「概览」。
  const matched = [...navItems]
    .sort((a, b) => b.to.length - a.to.length)
    .find((item) =>
      item.to === '/app'
        ? location.pathname === item.to
        : location.pathname === item.to || location.pathname.startsWith(`${item.to}/`),
    )?.key
  const current: MessageKey =
    matched ??
    (account && location.pathname === account
      ? 'nav.account'
      : location.pathname === settings
        ? 'nav.settings'
        : allowed
          ? navItems[0].key
          : 'nav.noAccess')

  const [reportedTitle, setReportedTitle] = useState<ReportedTitle | null>(null)
  const pageTitle = reportedTitle && reportedTitle.path === location.pathname ? reportedTitle.title : t(current)
  useDocumentTitle(`${pageTitle} · ${t('common.appName')} ${t('common.console')}`)

  const paletteGroups = [
    {
      label: t('nav.sectionAccount'),
      items: [
        ...(view === 'user' ? navItems : []),
        ...(account ? [{ to: account, key: 'nav.account' as MessageKey }] : []),
        { to: settings, key: 'nav.settings' as MessageKey },
      ]
        .filter((item): item is { to: string; key: MessageKey } => 'to' in item)
        .map((item) => ({
          id: item.to,
          label: t(item.key),
          onSelect: () => navigate(item.to),
        })),
    },
    ...(view !== 'user'
      ? [
          {
            label: t(view === 'admin' ? 'ops.sectionAdmin' : 'ops.sectionMerchant'),
            items: navItems.map((item) => ({
              id: item.to,
              label: t(item.key),
              onSelect: () => navigate(item.to),
            })),
          },
        ]
      : []),
    {
      label: t('nav.logout'),
      items: [{ id: 'logout', label: t('nav.logout'), onSelect: () => void signOut() }],
    },
  ]

  const signOut = async () => {
    await logout()
    navigate('/login', { replace: true })
  }

  const skipToMain = (event: React.MouseEvent<HTMLAnchorElement>) => {
    event.preventDefault()
    mainRef.current?.focus()
  }

  const displayName = user?.display_name || user?.username || ''

  const switcher =
    views.length > 1 && (
      <Segmented
        aria-label={t('common.console')}
        value={view}
        onValueChange={(next) => navigate(homeOf(next as View))}
        options={views.map((item) => ({ value: item, label: t(`view.${item}` as MessageKey) }))}
      />
    )

  const navLinkClass = ({ isActive }: { isActive: boolean }) =>
    cn(
      'flex h-9 items-center gap-2.5 rounded-sm px-3 text-sm font-medium text-muted-foreground outline-none transition-colors hover:bg-muted hover:text-foreground focus-visible:outline-2 focus-visible:-outline-offset-4 focus-visible:outline-ring',
      isActive && 'bg-muted text-foreground',
      collapsed && 'justify-center px-0',
    )

  // 抽屉只在小屏出现，那里侧栏恒收起；导航行不能跟着收，否则只剩图标。
  const drawerLinkClass = ({ isActive }: { isActive: boolean }) =>
    cn(
      'flex h-11 items-center gap-2.5 rounded-sm px-3 text-sm font-medium text-muted-foreground outline-none transition-colors hover:bg-muted hover:text-foreground focus-visible:outline-2 focus-visible:-outline-offset-4 focus-visible:outline-ring',
      isActive && 'bg-muted text-foreground',
    )

  const renderNav = (items: NavItem[], iconSize = 17) =>
    items.map((item) => {
      const Icon = item.icon
      return (
        <NavLink key={item.to} to={item.to} end={item.to === '/app'} title={t(item.key)} className={navLinkClass}>
          <Icon size={iconSize} className="shrink-0" />
          <span className={cn(collapsed && 'hidden')}>{t(item.key)}</span>
        </NavLink>
      )
    })

  const navGroups: Array<{ label: MessageKey; group: 'account' | 'access' }> = [
    { label: 'nav.sectionAccount', group: 'account' },
    { label: 'nav.sectionAccess', group: 'access' },
  ]

  return (
    <div className="flex min-h-dvh">
      <a
        href="#main-content"
        onClick={skipToMain}
        className="sr-only focus-visible:not-sr-only focus-visible:fixed focus-visible:top-2 focus-visible:left-2 focus-visible:z-skip focus-visible:rounded-sm focus-visible:bg-popover focus-visible:px-3 focus-visible:py-2 focus-visible:text-sm focus-visible:shadow-md"
      >
        {t('nav.skipToMain')}
      </a>

      {/* 侧栏：<520 不渲染（走抽屉），520–759 收成图标轨 */}
      <aside
        className={cn(
          'sticky top-0 hidden h-dvh shrink-0 flex-col border-r border-border bg-card md:flex',
          collapsed ? 'w-(--shell-rail)' : 'w-(--shell-side)',
        )}
      >
        <div className="flex h-(--topbar-h) items-center gap-2 px-3">
          <button
            className="flex min-w-0 items-center gap-2 rounded-sm outline-none focus-visible:outline-2 focus-visible:-outline-offset-4 focus-visible:outline-ring"
            onClick={() => {
              if (!narrow && collapsed) {
                if (short) setShortExpanded(true)
                else setManualCollapsed(false)
              } else navigate(homeOf(view))
            }}
            aria-label={collapsed && !narrow ? t('nav.expand') : t('common.console')}
          >
            <BrandMark />
            <span className={cn('truncate font-semibold tracking-tight', collapsed && 'hidden')}>
              {t('common.appName')}
            </span>
          </button>
          {!collapsed && (
            <button
              className="ml-auto rounded-xs p-1.5 text-faint transition-colors hover:bg-muted hover:text-foreground"
              onClick={() => (short ? setShortExpanded(false) : setManualCollapsed(true))}
              aria-label={t('nav.collapse')}
            >
              <ChevronLeft size={16} />
            </button>
          )}
        </div>

        <nav aria-label={t('common.console')} className="flex min-h-0 flex-1 flex-col overflow-y-auto px-2 pb-3">
          <div className="flex-1">
            {view === 'user'
              ? navGroups.map((group) => (
                  <div key={group.group} className="mb-3">
                    <div className={cn('px-3 pt-2 pb-1 text-caps text-faint', collapsed && 'text-center')}>
                      {collapsed ? '·' : t(group.label)}
                    </div>
                    <div className="flex flex-col gap-0.5">
                      {renderNav(
                        navItems.filter((item) => 'group' in item && item.group === group.group),
                      )}
                    </div>
                  </div>
                ))
              : (
                  <div className="mb-3">
                    <div className={cn('px-3 pt-2 pb-1 text-caps text-faint', collapsed && 'text-center')}>
                      {collapsed ? '·' : t(view === 'admin' ? 'ops.sectionAdmin' : 'ops.sectionMerchant')}
                    </div>
                    <div className="flex flex-col gap-0.5">{renderNav(navItems)}</div>
                  </div>
                )}
          </div>
          <div className="flex flex-col gap-0.5">
            {account && (
              <NavLink to={account} title={t('nav.account')} className={navLinkClass}>
                <User size={17} className="shrink-0" />
                <span className={cn(collapsed && 'hidden')}>{t('nav.account')}</span>
              </NavLink>
            )}
            <NavLink to={settings} title={t('nav.settings')} className={navLinkClass}>
              <Settings size={17} className="shrink-0" />
              <span className={cn(collapsed && 'hidden')}>{t('nav.settings')}</span>
            </NavLink>
          </div>
        </nav>
      </aside>

      <div className="flex min-w-0 flex-1 flex-col">
        <header className="sticky top-0 z-chrome flex h-(--topbar-h) items-center gap-3 border-b border-border bg-card/85 px-4 backdrop-blur-xl">
          <Button
            variant="ghost"
            size="icon"
            className="-ml-2 md:hidden"
            onClick={() => setDrawer(true)}
            aria-label={t('nav.menu')}
          >
            <Menu size={20} />
          </Button>
          <b className="min-w-0 truncate text-md font-semibold tracking-tight">{pageTitle}</b>
          <div className="ml-auto flex items-center gap-2">
            <Button
              variant="ghost"
              size="sm"
              className="hidden gap-1.5 text-faint md:inline-flex"
              onClick={() => setPaletteOpen(true)}
              aria-label={t('nav.menu')}
            >
              <Search className="size-3.5" />
              <Kbd>⌘</Kbd>
              <Kbd>K</Kbd>
            </Button>
            <div className="hidden md:block">{switcher}</div>
            {mobile ? (
              <Link
                to={settings}
                aria-label={t('nav.settings')}
                className="grid size-8 place-items-center rounded-full bg-muted text-xs font-semibold"
              >
                {initials(displayName)}
              </Link>
            ) : (
              <DropdownMenu>
                <DropdownMenuTrigger asChild>
                  <button className="flex items-center gap-2 rounded-sm p-1 outline-none focus-visible:outline-2 focus-visible:outline-ring">
                    <span className="grid size-8 place-items-center rounded-full bg-muted text-xs font-semibold">
                      {initials(displayName)}
                    </span>
                    <span className="max-w-40 truncate text-sm">{displayName}</span>
                  </button>
                </DropdownMenuTrigger>
                <DropdownMenuContent>
                  <DropdownMenuItem onSelect={() => navigate(settings)}>
                    <Settings size={15} />
                    {t('nav.settings')}
                  </DropdownMenuItem>
                  <DropdownMenuSeparator />
                  <DropdownMenuItem danger onSelect={() => void signOut()}>
                    <LogOut size={15} />
                    {t('nav.logout')}
                  </DropdownMenuItem>
                </DropdownMenuContent>
              </DropdownMenu>
            )}
          </div>
        </header>

        <main
          id="main-content"
          ref={mainRef}
          tabIndex={-1}
          className="mx-auto w-full max-w-(--content-max) flex-1 px-4 pt-5 pb-16 outline-none md:pb-8"
        >
          <RouteErrorBoundary>
            <PageTitleReporter report={setReportedTitle}>
              <Outlet />
            </PageTitleReporter>
          </RouteErrorBoundary>
        </main>

        {/* 底栏在 DOM 里排在主内容之后：键盘用户走完正文，Tab 才进底栏。 */}
        <nav
          aria-label={t('common.console')}
          className="fixed inset-x-0 bottom-0 z-chrome flex h-(--bottomnav-h) items-stretch border-t border-border bg-card/90 pb-(--safe-bottom,0px) backdrop-blur-xl md:hidden"
        >
          {tabs.map((tab) => {
            const Icon = tab.icon
            if (tab.sheet) {
              const active = (tab.owns ?? []).some(
                (path) => location.pathname === path || location.pathname.startsWith(`${path}/`),
              )
              return (
                <button
                  key={tab.key}
                  type="button"
                  aria-current={active ? 'page' : undefined}
                  aria-haspopup="dialog"
                  aria-expanded={opsSheet}
                  onClick={() => setOpsSheet(true)}
                  className={tabClass(active)}
                >
                  <Icon size={20} />
                  <span className="text-2xs">{t(tab.key)}</span>
                </button>
              )
            }
            const to = tab.to === 'settings' ? settings : tab.to!
            return (
              <NavLink key={tab.key} to={to} end={to === '/app'} className={({ isActive }) => tabClass(isActive)}>
                <Icon size={20} />
                <span className="text-2xs">{t(tab.key)}</span>
              </NavLink>
            )
          })}
        </nav>
      </div>

      <ActionSheet
        open={opsSheet}
        onOpenChange={setOpsSheet}
        title={t('nav.ops')}
        actions={OPS_NAV.admin
          .filter((item) => (ADMIN_SECONDARY_PATHS as readonly string[]).includes(item.to))
          .map((item) => ({
            label: t(item.key),
            onSelect: () => navigate(item.to),
          }))}
      />

      <CommandPalette open={paletteOpen} onOpenChange={setPaletteOpen} groups={paletteGroups} />

      <DialogPrimitive.Root open={drawer} onOpenChange={setDrawer}>
        <DialogPrimitive.Portal>
          <DialogPrimitive.Overlay className="fixed inset-0 z-drawer bg-overlay" />
          <DialogPrimitive.Content className="fixed inset-y-0 left-0 z-drawer flex w-72 flex-col bg-popover p-4 shadow-lg outline-none">
            <div className="flex items-center justify-between pb-2">
              <div className="flex items-center gap-2">
                <BrandMark />
                <span className="font-semibold tracking-tight">{t('common.appName')}</span>
              </div>
              <DialogPrimitive.Close asChild>
                <Button variant="ghost" size="icon" className="size-8" aria-label={t('common.close')}>
                  <X size={16} />
                </Button>
              </DialogPrimitive.Close>
            </div>
            {switcher && <div className="pb-3">{switcher}</div>}
            <div className="flex flex-col gap-0.5">
              {navItems.map((item) => {
                const Icon = item.icon
                return (
                  <NavLink
                    key={item.to}
                    to={item.to}
                    end={item.to === '/app'}
                    className={drawerLinkClass}
                  >
                    <Icon size={18} className="shrink-0" />
                    <span>{t(item.key)}</span>
                  </NavLink>
                )
              })}
              {account && (
                <NavLink to={account} className={drawerLinkClass}>
                  <User size={18} className="shrink-0" />
                  <span>{t('nav.account')}</span>
                </NavLink>
              )}
              <NavLink to={settings} className={drawerLinkClass}>
                <Settings size={18} className="shrink-0" />
                <span>{t('nav.settings')}</span>
              </NavLink>
            </div>
            <div className="mt-auto pt-3">
              <Button variant="danger" className="w-full" onClick={() => void signOut()}>
                <LogOut size={15} />
                {t('nav.logout')}
              </Button>
            </div>
          </DialogPrimitive.Content>
        </DialogPrimitive.Portal>
      </DialogPrimitive.Root>
    </div>
  )
}

function tabClass(active: boolean): string {
  return cn(
    'flex min-w-0 flex-1 flex-col items-center justify-center gap-1 text-muted-foreground outline-none transition-colors hover:text-foreground focus-visible:outline-2 focus-visible:-outline-offset-8 focus-visible:outline-ring',
    active && 'text-foreground',
  )
}
