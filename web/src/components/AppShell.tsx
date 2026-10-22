import { useEffect, useRef, useState, type ComponentType, type MouseEvent as ReactMouseEvent } from 'react'
import { Link, NavLink, Outlet, useLocation, useNavigate } from 'react-router-dom'
import { useAuth } from '../auth/AuthProvider'
import { useI18n } from '../i18n'
import type { MessageKey } from '../i18n/zh'
import { useMediaQuery, useIsMobile, useIsNarrow } from '../hooks/useMediaQuery'
import { ActionSheet } from './ActionSheet'
import {
  homeOf,
  isSharedMerchantPath,
  settingsOf,
  subjectOf,
  viewAllowed,
  viewOfPath,
  viewsFor,
  type View,
} from '../ops/views'
import {
  BrandMark,
  IconAlias,
  IconChannel,
  IconCollapse,
  IconExternal,
  IconKey,
  IconLogout,
  IconMenu,
  IconModels,
  IconOps,
  IconOverview,
  IconPricing,
  IconQuickstart,
  IconSettings,
  IconUsage,
  IconUser,
  IconX,
} from './icons'

export const APP_NAV: Array<{
  to: string
  key: MessageKey
  icon: ComponentType<{ size?: number; className?: string }>
  group: 'account' | 'access'
}> = [
  { to: '/app', key: 'nav.overview', icon: IconOverview, group: 'account' },
  { to: '/app/keys', key: 'nav.keys', icon: IconKey, group: 'account' },
  { to: '/app/usage', key: 'nav.usage', icon: IconUsage, group: 'account' },
  { to: '/app/models', key: 'nav.models', icon: IconModels, group: 'access' },
  { to: '/app/quickstart', key: 'nav.quickstart', icon: IconQuickstart, group: 'access' },
]

type NavItem = { to: string; key: MessageKey; icon: ComponentType<{ size?: number; className?: string }> }

/**
 * 运营视图的导航。
 *
 * merchant 前三项是运营页：同类型同地址的渠道在「上游」归组，在供模型与
 * 定价状态在「模型」。后两项是商家自己那部分：渠道与计价（只有自己的规则，
 * 没有成本页）。模型目录与别名是平台级资源，没有归属可过滤，不在商家视图里出现。
 */
const OPS_NAV: Record<Exclude<View, 'user'>, NavItem[]> = {
  merchant: [
    { to: '/app/merchant/overview', key: 'nav.overview', icon: IconOverview },
    { to: '/app/merchant/models', key: 'ops.nav.merchantModels', icon: IconModels },
    { to: '/app/merchant/upstreams', key: 'ops.nav.upstreams', icon: IconExternal },
    { to: '/app/merchant/channels', key: 'ops.nav.channels', icon: IconChannel },
    { to: '/app/merchant/pricing', key: 'ops.nav.pricing', icon: IconPricing },
  ],
  admin: [
    { to: '/app/admin/channels', key: 'ops.nav.channels', icon: IconChannel },
    { to: '/app/admin/models', key: 'ops.nav.models', icon: IconModels },
    { to: '/app/admin/users', key: 'ops.nav.users', icon: IconUser },
    { to: '/app/admin/aliases', key: 'ops.nav.aliases', icon: IconAlias },
    { to: '/app/admin/pricing', key: 'ops.nav.pricing', icon: IconPricing },
  ],
}

/**
 * 管理员那些进不了底栏的页：从「运营」这个二级入口进。
 *
 * 底栏最多五个，而管理端的二级页有三个，挤不进同一个数组时不要删减——
 * 少一个就少一条路，用户只能从抽屉里绕。
 */
const ADMIN_SECONDARY_PATHS = [
  '/app/admin/models',
  '/app/admin/aliases',
  '/app/admin/pricing',
] as const

/**
 * 底栏的 tab 表，一个视图一行。
 *
 * 这里不判断角色：`view` 已经由 `views.ts` 的视图判定给出；商家与管理员共用
 * 的那三页，主语由页面自己按 `subjectOf` 取。加一页时往对应行里加一项即可。
 *
 * `to: 'settings'` 是占位——设置页的地址随视图不同，渲染时才落定。
 */
type BottomTab = {
  key: MessageKey
  icon: ComponentType<{ size?: number; className?: string }>
  to?: string | 'settings'
  /** 弹出二级入口的 tab：点它只开 sheet，不发导航。 */
  sheet?: 'ops'
  /** 弹出型 tab 在哪些地址上算当前项。 */
  owns?: readonly string[]
}

/**
 * 底栏按「看到的这一组页面」取表，不按账号角色取。
 *
 * 管理员进商家视图时能进的是概览、模型、上游与用量，这四页就是他的底栏；
 * 套用管理视图那套会出现「停在当前页却没有任何 tab 是当前项」。
 */
type BottomNavKey = View | 'merchantSite'

const BOTTOM_NAV: Record<BottomNavKey, BottomTab[]> = {
  user: [
    { to: '/app', key: 'nav.overview', icon: IconOverview },
    { to: '/app/keys', key: 'nav.keys', icon: IconKey },
    { to: '/app/usage', key: 'nav.usage', icon: IconUsage },
    { to: '/app/models', key: 'nav.models', icon: IconModels },
    { to: 'settings', key: 'nav.settings', icon: IconSettings },
  ],
  merchant: [
    { to: '/app/merchant/overview', key: 'nav.overview', icon: IconOverview },
    { to: '/app/merchant/channels', key: 'ops.nav.channels', icon: IconChannel },
    { to: '/app/usage', key: 'nav.usage', icon: IconUsage },
    { to: '/app/merchant/account', key: 'nav.account', icon: IconUser },
    { to: 'settings', key: 'nav.settings', icon: IconSettings },
  ],
  merchantSite: [
    { to: '/app/merchant/overview', key: 'nav.overview', icon: IconOverview },
    { to: '/app/merchant/models', key: 'ops.nav.merchantModels', icon: IconModels },
    { to: '/app/merchant/upstreams', key: 'ops.nav.upstreams', icon: IconExternal },
    { to: '/app/usage', key: 'nav.usage', icon: IconUsage },
  ],
  admin: [
    { to: '/app/merchant/overview', key: 'nav.overview', icon: IconOverview },
    { to: '/app/admin/channels', key: 'ops.nav.channels', icon: IconChannel },
    { to: '/app/usage', key: 'nav.usage', icon: IconUsage },
    { to: '/app/admin/users', key: 'ops.nav.users', icon: IconUser },
    { sheet: 'ops', key: 'nav.ops', icon: IconOps, owns: ADMIN_SECONDARY_PATHS },
  ],
}

function initials(name: string | null | undefined): string {
  const value = (name ?? '').trim()
  if (!value) return '?'
  return Array.from(value)[0].toUpperCase()
}

export function AppShell() {
  const { t } = useI18n()
  const { user, logout } = useAuth()
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
  const [menuOpen, setMenuOpen] = useState(false)
  const [opsSheet, setOpsSheet] = useState(false)
  const menuRef = useRef<HTMLDivElement | null>(null)
  const triggerRef = useRef<HTMLButtonElement | null>(null)
  const mainRef = useRef<HTMLElement | null>(null)
  const collapsed = narrow || (short ? !shortExpanded : manualCollapsed)

  const views = viewsFor(user?.role)
  const routeView = viewOfPath(location.pathname)
  // 地址栏可以直接敲进账号没有的视图：那一页画的是无权限，侧栏也不该把那一组
  // 入口列出来。可用的视图与切换器同源，不另写一份角色判断。
  const allowed = viewAllowed(user?.role, location.pathname)
  const view = allowed ? routeView : views[views.length - 1]
  // 管理员的商家视图只有「两种主语」的那三页；渠道与计价写的是「你名下」，
  // 管理员进那一组只会看到错的主语，所以不列链接（直接敲地址仍落到无权限）。
  const siteWide = subjectOf(user?.role) === 'site'
  const merchantItems = siteWide
    ? OPS_NAV.merchant.filter((item) => isSharedMerchantPath(item.to))
    : OPS_NAV.merchant
  const navItems: NavItem[] =
    view === 'user' ? APP_NAV : view === 'merchant' ? merchantItems : OPS_NAV[view]
  // 设置是账号级的：管理员在商家视图里点设置，落到管理视图那一个地址。
  const settings = settingsOf(view === 'merchant' && siteWide ? 'admin' : view)
  // 「账号」只对商家开放：用户与管理员的底栏没有这一项，账号段留在他们的
  // 设置页里；管理员进商家视图时也进不去这一页（不在共享路径里）。
  const account = view === 'merchant' && !siteWide ? '/app/merchant/account' : null
  // 底栏跟着当前这一组页面走（见 `BottomNavKey`）。
  const tabs = BOTTOM_NAV[view === 'merchant' && siteWide ? 'merchantSite' : view]

  useEffect(() => {
    setDrawer(false)
    setMenuOpen(false)
  }, [location.pathname])

  // 账户菜单：Esc 关闭并把焦点交还触发钮，点菜单外同样关闭。
  useEffect(() => {
    if (!menuOpen) return
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key !== 'Escape') return
      setMenuOpen(false)
      triggerRef.current?.focus()
    }
    const onPointerDown = (event: MouseEvent) => {
      if (menuRef.current?.contains(event.target as Node)) return
      setMenuOpen(false)
    }
    document.addEventListener('keydown', onKeyDown)
    document.addEventListener('mousedown', onPointerDown)
    return () => {
      document.removeEventListener('keydown', onKeyDown)
      document.removeEventListener('mousedown', onPointerDown)
    }
  }, [menuOpen])

  // `/app` 这一项不能按前缀匹配：它是每个子路由的前缀，会先把设置页之类
  // 没有进导航的页面全抢成「概览」。
  //
  // 敲进账号没有的视图时，页面正文是无权限提示；标题跟着写「无权限」，
  // 而不是回落到这个视图的第一项（那会显示「渠道」，与正文对不上）。
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

  const signOut = async () => {
    await logout()
    navigate('/login', { replace: true })
  }

  // 「跳到主内容」自己把焦点交给主列。只把视口滚过去的话，读屏与键盘用户
  // 下一步 Tab 会从页面顶部重新开始；原生片段跳转在目标可聚焦时也会移焦，
  // 但显式调用更确定，也不改动地址栏。
  const skipToMain = (event: ReactMouseEvent<HTMLAnchorElement>) => {
    event.preventDefault()
    mainRef.current?.focus()
  }

  const displayName = user?.display_name || user?.username || ''

  const switcher = views.length > 1 && (
    <div className="seg view-switch" role="group" aria-label={t('common.console')}>
      {views.map((item) => (
        <button
          key={item}
          aria-pressed={view === item}
          onClick={() => navigate(homeOf(item))}
        >
          {t(`view.${item}` as MessageKey)}
        </button>
      ))}
    </div>
  )

  const renderNav = (items: NavItem[]) =>
    items.map((item) => {
      const Icon = item.icon
      return (
        <NavLink key={item.to} to={item.to} end={item.to === '/app'} title={t(item.key)}>
          <Icon size={17} />
          <span>{t(item.key)}</span>
        </NavLink>
      )
    })

  const navGroups: Array<{ label: MessageKey; group: 'account' | 'access' }> = [
    { label: 'nav.sectionAccount', group: 'account' },
    { label: 'nav.sectionAccess', group: 'access' },
  ]

  return (
    <div className="app-shell">
      <a className="skip-link" href="#main-content" onClick={skipToMain}>
        {t('nav.skipToMain')}
      </a>
      <aside className={collapsed ? 'sidebar collapsed' : 'sidebar'} hidden={mobile}>
        <div className="side-head">
          <button
            className="brand"
            onClick={() => {
              if (!narrow && collapsed) {
                if (short) setShortExpanded(true)
                else setManualCollapsed(false)
              } else navigate(homeOf(view))
            }}
            aria-label={collapsed && !narrow ? t('nav.expand') : t('common.console')}
          >
            <span className="brand-mark">
              <BrandMark />
              <span className="expand-mark">
                <IconCollapse size={22} />
              </span>
            </span>
            <span className="word">{t('common.appName')}</span>
          </button>
          {!collapsed && (
            <button
              className="side-toggle"
              onClick={() => (short ? setShortExpanded(false) : setManualCollapsed(true))}
              aria-label={t('nav.collapse')}
            >
              <IconCollapse size={16} />
            </button>
          )}
        </div>

        <nav aria-label={t('common.console')}>
          {view === 'user' ? (
            navGroups.map((group) => (
              <div key={group.group} style={{ marginBottom: 14 }}>
                <div className="side-group-label">{t(group.label)}</div>
                <div className="nav">
                  {navItems
                    .filter((item) => 'group' in item && item.group === group.group)
                    .map((item) => {
                      const Icon = item.icon
                      return (
                        <NavLink key={item.to} to={item.to} end={item.to === '/app'} title={t(item.key)}>
                          <Icon size={17} />
                          <span>{t(item.key)}</span>
                        </NavLink>
                      )
                    })}
                </div>
              </div>
            ))
          ) : (
            <div style={{ marginBottom: 14 }}>
              <div className="side-group-label">
                {t(view === 'admin' ? 'ops.sectionAdmin' : 'ops.sectionMerchant')}
              </div>
              <div className="nav">{renderNav(navItems)}</div>
            </div>
          )}
          <div className="nav">
            {account && (
              <NavLink to={account} title={t('nav.account')}>
                <IconUser size={17} />
                <span>{t('nav.account')}</span>
              </NavLink>
            )}
            <NavLink to={settings} title={t('nav.settings')}>
              <IconSettings size={17} />
              <span>{t('nav.settings')}</span>
            </NavLink>
          </div>
        </nav>
      </aside>

      <div className="app-main">
        <header className="app-topbar">
          <button
            className="side-toggle"
            onClick={() => setDrawer(true)}
            aria-label={t('nav.menu')}
            aria-expanded={drawer}
          >
            <IconMenu size={20} />
          </button>
          <b>{t(current)}</b>
          <div className="topbar-right">
            {!mobile && switcher}
            {mobile ? (
              <Link to={settings} className="avatar" aria-label={t('nav.settings')}>
                {initials(displayName)}
              </Link>
            ) : (
              <div className="account" ref={menuRef}>
                <button
                  ref={triggerRef}
                  className="account-trigger"
                  aria-expanded={menuOpen}
                  onClick={() => setMenuOpen((open) => !open)}
                >
                  <span className="avatar">{initials(displayName)}</span>
                  <span className="account-name">{displayName}</span>
                </button>
                {menuOpen && (
                  <div className="account-menu">
                    <Link to={settings} onClick={() => setMenuOpen(false)}>
                      <IconSettings size={15} />
                      {t('nav.settings')}
                    </Link>
                    <button type="button" onClick={signOut}>
                      <IconLogout size={15} />
                      {t('nav.logout')}
                    </button>
                  </div>
                )}
              </div>
            )}
          </div>
        </header>

        <main id="main-content" ref={mainRef} tabIndex={-1} className="app-content">
          <Outlet />
        </main>

        {/*
         * 底栏在 DOM 里排在主内容之后：键盘用户走完正文，Tab 才进底栏，
         * 不会被这条固定栏截在半路。它浮在内容之上，所以聚焦元素还有一条
         * `scroll-margin-bottom`（见 components.css），滚到最后一屏时不被盖住。
         */}
        <nav className="bottomnav" aria-label={t('common.console')}>
          {tabs.map((tab) => {
            const Icon = tab.icon
            if (tab.sheet) {
              const active = (tab.owns ?? []).some(
                (path) =>
                  location.pathname === path || location.pathname.startsWith(`${path}/`),
              )
              return (
                <button
                  key={tab.key}
                  type="button"
                  aria-current={active ? 'page' : undefined}
                  aria-haspopup="dialog"
                  aria-expanded={opsSheet}
                  onClick={() => setOpsSheet(true)}
                >
                  <Icon size={20} />
                  <span>{t(tab.key)}</span>
                </button>
              )
            }
            const to = tab.to === 'settings' ? settings : tab.to!
            return (
              <NavLink key={tab.key} to={to} end={to === '/app'}>
                <Icon size={20} />
                <span>{t(tab.key)}</span>
              </NavLink>
            )
          })}
        </nav>
      </div>

      {opsSheet && (
        <ActionSheet
          title={t('nav.ops')}
          items={OPS_NAV.admin
            .filter((item) => (ADMIN_SECONDARY_PATHS as readonly string[]).includes(item.to))
            .map((item) => {
              const Icon = item.icon
              return {
                key: item.to,
                label: t(item.key),
                icon: <Icon size={18} />,
                to: item.to,
              }
            })}
          onClose={() => setOpsSheet(false)}
        />
      )}

      {drawer && (
        <div
          className="overlay sheet-overlay"
          onMouseDown={(event) => {
            if (event.target === event.currentTarget) setDrawer(false)
          }}
        >
          <div className="modal" role="dialog" aria-modal="true" aria-label={t('nav.menu')}>
            <div className="m-head">
              <div className="brand" style={{ padding: 0 }}>
                <span className="brand-mark">
                  <BrandMark />
                </span>
                <span className="word">{t('common.appName')}</span>
              </div>
              <button className="close" onClick={() => setDrawer(false)} aria-label={t('common.close')}>
                <IconX size={16} />
              </button>
            </div>
            {switcher && <div style={{ margin: '0 0 12px' }}>{switcher}</div>}
            <div className="nav drawer-nav">
              {navItems.map((item) => {
                const Icon = item.icon
                return (
                  <NavLink key={item.to} to={item.to} end={item.to === '/app'}>
                    <Icon size={18} />
                    <span>{t(item.key)}</span>
                  </NavLink>
                )
              })}
              {account && (
                <NavLink to={account}>
                  <IconUser size={18} />
                  <span>{t('nav.account')}</span>
                </NavLink>
              )}
              <NavLink to={settings}>
                <IconSettings size={18} />
                <span>{t('nav.settings')}</span>
              </NavLink>
            </div>
            <div className="form-actions">
              <button className="btn danger" onClick={signOut}>
                <IconLogout size={15} />
                {t('nav.logout')}
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  )
}
