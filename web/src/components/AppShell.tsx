import { useEffect, useRef, useState, type ComponentType, type MouseEvent as ReactMouseEvent } from 'react'
import { Link, NavLink, Outlet, useLocation, useNavigate } from 'react-router-dom'
import { useAuth } from '../auth/AuthProvider'
import { useI18n } from '../i18n'
import type { MessageKey } from '../i18n/zh'
import { useIsMobile, useIsNarrow } from '../hooks/useMediaQuery'
import { homeOf, settingsOf, viewOfPath, viewsFor, type View } from '../ops/views'
import {
  BrandMark,
  IconAlias,
  IconChannel,
  IconCollapse,
  IconKey,
  IconLogout,
  IconMenu,
  IconModels,
  IconOverview,
  IconPricing,
  IconQuickstart,
  IconSettings,
  IconUsage,
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
 * merchant 有渠道与计价两组：计价在商家前缀下只有自己的规则，没有成本页；
 * 模型目录与别名是平台级资源，没有归属可过滤，不在商家视图里出现。
 */
const OPS_NAV: Record<Exclude<View, 'user'>, NavItem[]> = {
  merchant: [
    { to: '/app/merchant/channels', key: 'ops.nav.channels', icon: IconChannel },
    { to: '/app/merchant/pricing', key: 'ops.nav.pricing', icon: IconPricing },
  ],
  admin: [
    { to: '/app/admin/channels', key: 'ops.nav.channels', icon: IconChannel },
    { to: '/app/admin/models', key: 'ops.nav.models', icon: IconModels },
    { to: '/app/admin/aliases', key: 'ops.nav.aliases', icon: IconAlias },
    { to: '/app/admin/pricing', key: 'ops.nav.pricing', icon: IconPricing },
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
  const [manualCollapsed, setManualCollapsed] = useState(false)
  const [drawer, setDrawer] = useState(false)
  const [menuOpen, setMenuOpen] = useState(false)
  const menuRef = useRef<HTMLDivElement | null>(null)
  const triggerRef = useRef<HTMLButtonElement | null>(null)
  const mainRef = useRef<HTMLElement | null>(null)
  const collapsed = narrow || manualCollapsed

  const views = viewsFor(user?.role)
  const routeView = viewOfPath(location.pathname)
  // 地址栏可以直接敲进账号没有的视图：那一页画的是无权限，侧栏也不该把那一组
  // 入口列出来。可用的视图与切换器同源，不另写一份角色判断。
  const allowed = views.includes(routeView)
  const view = allowed ? routeView : views[views.length - 1]
  const navItems: NavItem[] = view === 'user' ? APP_NAV : OPS_NAV[view]
  const settings = settingsOf(view)

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
    (location.pathname === settings ? 'nav.settings' : allowed ? navItems[0].key : 'nav.noAccess')

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
              if (!narrow && collapsed) setManualCollapsed(false)
              else navigate(homeOf(view))
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
              onClick={() => setManualCollapsed(true)}
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

        <nav className="bottomnav" aria-label={t('common.console')}>
          {[
            ...navItems.slice(0, view === 'user' ? 3 : 4),
            { to: settings, key: 'nav.settings' as MessageKey, icon: IconSettings },
          ].map((item) => {
            const Icon = item.icon
            return (
              <NavLink key={item.to} to={item.to} end={item.to === '/app'}>
                <Icon size={20} />
                <span>{t(item.key)}</span>
              </NavLink>
            )
          })}
        </nav>
      </div>

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
