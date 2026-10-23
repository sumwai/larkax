import { NavLink, Outlet, useNavigate } from 'react-router-dom'
import { useAuth } from '../auth/AuthProvider'
import { useI18n } from '../i18n'
import { useSite } from '../site/SiteProvider'
import type { MessageKey } from '../i18n/zh'
import { Button } from './ui'

/** 控制台的导航表。页面多了以后从这张表长出来，不另写一份。 */
const NAV: { to: string; label: MessageKey }[] = [
  { to: '/app', label: 'nav.overview' },
  { to: '/app/keys', label: 'nav.keys' },
  { to: '/app/requests', label: 'nav.requests' },
]

/**
 * 控制台外壳：顶栏（品牌、导航、账号）加内容区。
 *
 * 侧栏还没做，导航先横排在顶栏里；页面之间靠这张表互达。
 */
export function ConsoleShell() {
  const { t } = useI18n()
  const { meta } = useSite()
  const { user, logout } = useAuth()
  const navigate = useNavigate()

  return (
    <div className="min-h-dvh bg-background">
      <header className="border-b border-border bg-card">
        <div className="mx-auto flex w-full max-w-(--content-max) items-center gap-3 px-4 py-3">
          <NavLink to="/" className="flex items-center gap-2">
            <span
              aria-hidden="true"
              className="flex size-7 items-center justify-center rounded-xs bg-primary font-mono text-xs font-semibold text-primary-foreground"
            >
              lx
            </span>
            <span className="text-md font-semibold tracking-tight">{meta.site_name}</span>
          </NavLink>

          <nav className="flex items-center gap-1">
            {NAV.map((item) => (
              <NavLink
                key={item.to}
                to={item.to}
                end={item.to === '/app'}
                className={({ isActive }) =>
                  `rounded-sm px-2.5 py-1 text-sm transition-colors ${
                    isActive
                      ? 'bg-muted font-medium text-foreground'
                      : 'text-muted-foreground hover:bg-muted hover:text-foreground'
                  }`
                }
              >
                {t(item.label)}
              </NavLink>
            ))}
          </nav>

          <span className="ml-auto truncate text-sm text-muted-foreground">
            {user?.display_name || user?.username || ''}
          </span>
          <Button
            variant="ghost"
            size="sm"
            onClick={() => {
              void logout().then(() => navigate('/login', { replace: true }))
            }}
          >
            {t('nav.logout')}
          </Button>
        </div>
      </header>

      <main className="mx-auto flex w-full max-w-(--content-max) flex-col gap-6 px-4 py-8">
        <Outlet />
      </main>
    </div>
  )
}
