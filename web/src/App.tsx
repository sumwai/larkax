import { useState, type ReactNode } from 'react'
import {
  createBrowserRouter,
  Link,
  Navigate,
  RouterProvider,
  useLocation,
  type RouteObject,
} from 'react-router-dom'
import { AuthProvider, useAuth } from './auth/AuthProvider'
import { I18nProvider, useI18n } from './i18n'
import { SiteProvider } from './site/SiteProvider'
import { ThemeProvider } from './theme/ThemeProvider'
import { AppShell } from './components/AppShell'
import { FatalErrorBoundary, RootRouteError } from './components/ErrorBoundary'
import { MockBanner } from './components/MockBanner'
import { Alert, ToastProvider } from './components/ui'
import { viewAllowed, viewOfPath, type View } from './ops/views'
import { OpsChannels } from './pages/ops/Channels'
import { OpsModels } from './pages/ops/Catalog'
import { OpsAliases } from './pages/ops/Aliases'
import { AdminUserDetailPage, AdminUsers } from './pages/admin/Users'
import { MerchantPricing } from './pages/ops/MerchantPricing'
import { MerchantOverview } from './pages/ops/MerchantOverview'
import { MerchantModels } from './pages/ops/MerchantModels'
import { MerchantUpstreams } from './pages/ops/MerchantUpstreams'
import { OpsPricing } from './pages/ops/Pricing'
import { Landing } from './pages/Landing'
import { Login } from './pages/Login'
import { Overview } from './pages/Overview'
import { Keys } from './pages/Keys'
import { Usage } from './pages/Usage'
import { Models } from './pages/Models'
import { ModelDetail } from './pages/ModelDetail'
import { Quickstart } from './pages/Quickstart'
import { MerchantSettings, Settings } from './pages/Settings'
import { MerchantAccount } from './pages/MerchantAccount'

function LoadingScreen() {
  const { t } = useI18n()
  return (
    <div className="auth-page">
      <div className="loading">
        <span className="spin" aria-hidden="true" />
        {t('common.loading')}
      </div>
    </div>
  )
}

/**
 * 登录门槛。
 *
 * 地址带在 `?next=` 上而不是路由 state 里：会话失效这一下是被动发生的，
 * 用户可能顺手刷新一次登录页，state 一刷就没了，`next` 还在。
 */
function RequireAuth({ children }: { children: ReactNode }) {
  const { status } = useAuth()
  const location = useLocation()
  if (status === 'loading') return <LoadingScreen />
  if (status === 'anonymous') {
    const next = `${location.pathname}${location.search}${location.hash}`
    return <Navigate to={`/login?next=${encodeURIComponent(next)}`} replace />
  }
  return <>{children}</>
}

/**
 * 视图的角色门槛。
 *
 * 它只决定「这个页面画不画」；真正的越权判定在服务端——这里放行了，
 * 请求照样可能拿到 403，而页面显示的就是服务端给的那句话。
 */
function RequireView({ view, children }: { view: View; children: ReactNode }) {
  const { user } = useAuth()
  const { t } = useI18n()
  const { pathname } = useLocation()
  // 判定按地址走：商家视图里那三页是「同一批页面、两种主语」，管理员也进得去；
  // 渠道与计价仍然只对商家开放，所以不能只看传进来的视图名。
  if (viewOfPath(pathname) !== view || !viewAllowed(user?.role, pathname)) {
    return (
      <Alert tone="bad" title={t('ops.noAccess')}>
        {t('ops.noAccessNext')}{' '}
        <Link className="btn secondary sm" to="/app">
          {t('ops.backToOverview')}
        </Link>
      </Alert>
    )
  }
  return <>{children}</>
}

function NotFound() {
  const { t } = useI18n()
  return (
    <div className="auth-page">
      <div className="card auth-card" style={{ textAlign: 'center' }}>
        <h1 style={{ fontSize: 22 }}>{t('errors.notFound')}</h1>
        <Link className="btn secondary" to="/">
          {t('login.backHome')}
        </Link>
      </div>
    </div>
  )
}

/**
 * 路由表。
 *
 * 根上那一条不带路径，只做两件事：包住所有页面、给整棵树一个错误页。
 * 路由级异常先落到 `/app` 里的 `RouteErrorBoundary`（外壳还在，用户能切走）；
 * 根级页面（落地页、登录页、404）没有内层边界，由这里的 `errorElement` 接住，
 * 显示与最外层 `FatalErrorBoundary` 同一个「致命错误页」——当初挂在
 * `<BrowserRouter>` 外面时就是这个行为，转 data router 不该把它丢掉。
 */
const ROUTES: RouteObject[] = [
  {
    errorElement: <RootRouteError />,
    children: [
      { path: '/', element: <Landing /> },
      { path: '/login', element: <Login /> },
      {
        path: '/app',
        element: (
          <RequireAuth>
            <AppShell />
          </RequireAuth>
        ),
        children: [
          { index: true, element: <Overview /> },
          { path: 'keys', element: <Keys /> },
          { path: 'usage', element: <Usage /> },
          { path: 'models', element: <Models /> },
          // 规范名可能带 `/`（`vendor/model`），所以用 splat 接住整段。
          { path: 'models/*', element: <ModelDetail /> },
          { path: 'quickstart', element: <Quickstart /> },
          { path: 'settings', element: <Settings /> },
          { path: 'merchant', element: <Navigate to="/app/merchant/overview" replace /> },
          {
            path: 'merchant/overview',
            element: (
              <RequireView view="merchant">
                <MerchantOverview />
              </RequireView>
            ),
          },
          {
            path: 'merchant/models',
            element: (
              <RequireView view="merchant">
                <MerchantModels />
              </RequireView>
            ),
          },
          {
            path: 'merchant/upstreams',
            element: (
              <RequireView view="merchant">
                <MerchantUpstreams />
              </RequireView>
            ),
          },
          {
            path: 'merchant/account',
            element: (
              <RequireView view="merchant">
                <MerchantAccount />
              </RequireView>
            ),
          },
          {
            path: 'merchant/settings',
            element: (
              <RequireView view="merchant">
                <MerchantSettings />
              </RequireView>
            ),
          },
          {
            path: 'merchant/channels',
            element: (
              <RequireView view="merchant">
                <OpsChannels scope="merchant" />
              </RequireView>
            ),
          },
          {
            path: 'merchant/pricing',
            element: (
              <RequireView view="merchant">
                <MerchantPricing />
              </RequireView>
            ),
          },
          {
            path: 'admin/settings',
            element: (
              <RequireView view="admin">
                <Settings />
              </RequireView>
            ),
          },
          { path: 'admin', element: <Navigate to="/app/admin/channels" replace /> },
          {
            path: 'admin/channels',
            element: (
              <RequireView view="admin">
                <OpsChannels scope="admin" />
              </RequireView>
            ),
          },
          {
            path: 'admin/models',
            element: (
              <RequireView view="admin">
                <OpsModels />
              </RequireView>
            ),
          },
          {
            path: 'admin/users',
            element: (
              <RequireView view="admin">
                <AdminUsers />
              </RequireView>
            ),
          },
          {
            path: 'admin/users/:id',
            element: (
              <RequireView view="admin">
                <AdminUserDetailPage />
              </RequireView>
            ),
          },
          {
            path: 'admin/aliases',
            element: (
              <RequireView view="admin">
                <OpsAliases />
              </RequireView>
            ),
          },
          {
            path: 'admin/pricing',
            element: (
              <RequireView view="admin">
                <OpsPricing />
              </RequireView>
            ),
          },
        ],
      },
      { path: '*', element: <NotFound /> },
    ],
  },
]

export function App() {
  // 只建一次：`createBrowserRouter` 会订阅地址变化，每次渲染换一个实例等于把
  // 订阅原地换掉。测试里每次 `render(<App />)` 都是新的一份，互不影响。
  const [router] = useState(() => createBrowserRouter(ROUTES))
  return (
    <FatalErrorBoundary>
      <I18nProvider>
        <ThemeProvider>
          <ToastProvider>
            <SiteProvider>
              <AuthProvider>
                {/* 演示横幅不读路由，放在 RouterProvider 外面，位置与原来一致。 */}
                <MockBanner />
                <RouterProvider router={router} />
              </AuthProvider>
            </SiteProvider>
          </ToastProvider>
        </ThemeProvider>
      </I18nProvider>
    </FatalErrorBoundary>
  )
}
