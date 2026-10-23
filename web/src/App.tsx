import { Suspense, lazy, useState, type ReactNode } from 'react'
import {
  createBrowserRouter,
  Link,
  Navigate,
  RouterProvider,
  useLocation,
  type RouteObject,
} from 'react-router-dom'
import { QueryClientProvider } from '@tanstack/react-query'
import { AuthProvider, loginPath, useAuth } from './auth/AuthProvider'
import { I18nProvider, useI18n } from './i18n'
import { SiteProvider } from './site/SiteProvider'
import { ThemeProvider } from './theme/ThemeProvider'
import { AppShell } from './components/AppShell'
import { FatalErrorBoundary, RootRouteError } from './components/ErrorBoundary'
import { MockBanner } from './components/MockBanner'
import { Alert, AppToaster, Banner, Button, Card, Spinner } from './components/ui'
import { queryClient } from './api/queries/client'
import { viewAllowed, viewOfPath, type View } from './ops/views'
import { UnsavedGuardProvider, useUnsavedChanges, useUnsavedLeaveRef } from './pages/ops/guard'

/* 路由分包：每个页面一个 chunk，进哪页下哪页。多导出的模块按导出各自 lazy，
   模块本体只取一次（ESM 缓存），映射是纯 Promise 变换。 */
const Landing = lazy(() => import('./pages/Landing').then((m) => ({ default: m.Landing })))
const Login = lazy(() => import('./pages/Login').then((m) => ({ default: m.Login })))
const Overview = lazy(() => import('./pages/Overview').then((m) => ({ default: m.Overview })))
const Keys = lazy(() => import('./pages/Keys').then((m) => ({ default: m.Keys })))
const Usage = lazy(() => import('./pages/Usage').then((m) => ({ default: m.Usage })))
const Models = lazy(() => import('./pages/Models').then((m) => ({ default: m.Models })))
const ModelDetail = lazy(() =>
  import('./pages/ModelDetail').then((m) => ({ default: m.ModelDetail })),
)
const Quickstart = lazy(() => import('./pages/Quickstart').then((m) => ({ default: m.Quickstart })))
const Settings = lazy(() => import('./pages/Settings').then((m) => ({ default: m.Settings })))
const MerchantSettings = lazy(() =>
  import('./pages/Settings').then((m) => ({ default: m.MerchantSettings })),
)
const MerchantAccount = lazy(() =>
  import('./pages/MerchantAccount').then((m) => ({ default: m.MerchantAccount })),
)
const OpsChannels = lazy(() =>
  import('./pages/ops/Channels').then((m) => ({ default: m.OpsChannels })),
)
const OpsModels = lazy(() => import('./pages/ops/Catalog').then((m) => ({ default: m.OpsModels })))
const OpsAliases = lazy(() => import('./pages/ops/Aliases').then((m) => ({ default: m.OpsAliases })))
const OpsPricing = lazy(() => import('./pages/ops/Pricing').then((m) => ({ default: m.OpsPricing })))
const AdminUsers = lazy(() => import('./pages/admin/Users').then((m) => ({ default: m.AdminUsers })))
const AdminUserDetailPage = lazy(() =>
  import('./pages/admin/Users').then((m) => ({ default: m.AdminUserDetailPage })),
)
const MerchantOverview = lazy(() =>
  import('./pages/ops/MerchantOverview').then((m) => ({ default: m.MerchantOverview })),
)
const MerchantModels = lazy(() =>
  import('./pages/ops/MerchantModels').then((m) => ({ default: m.MerchantModels })),
)
const MerchantUpstreams = lazy(() =>
  import('./pages/ops/MerchantUpstreams').then((m) => ({ default: m.MerchantUpstreams })),
)
const MerchantPricing = lazy(() =>
  import('./pages/ops/MerchantPricing').then((m) => ({ default: m.MerchantPricing })),
)

function LoadingScreen() {
  const { t } = useI18n()
  return (
    <div className="grid min-h-dvh place-items-center bg-background">
      <div className="flex items-center gap-2 text-sm text-muted-foreground">
        <Spinner />
        {t('common.loading')}
      </div>
    </div>
  )
}

function Chunk() {
  return (
    <div className="grid min-h-40 place-items-center">
      <Spinner className="text-faint" />
    </div>
  )
}

function withChunk(node: ReactNode): ReactNode {
  return <Suspense fallback={<Chunk />}>{node}</Suspense>
}

/**
 * 会话失效但用户选了「留下」时的那条横幅。
 *
 * 非模态、一直在：这一页已经保存不了，用户什么时候想重新登录都点得到。它不直接
 * 跳转——「让开阻止器」这一步只有页面里的 `RouteBlocker` 会做，所以走它登记的
 * 入口。
 */
function SessionExpiredNotice() {
  const { t } = useI18n()
  const leaveRef = useUnsavedLeaveRef()
  return (
    <Banner tone="danger">
      <span className="min-w-0 flex-1">
        <b className="font-semibold">{t('ops.sessionExpiredTitle')}</b>{' '}
        {t('ops.sessionExpiredBody')}{' '}
        <Button variant="secondary" size="sm" onClick={() => leaveRef.current?.()}>
          {t('ops.sessionExpiredSignIn')}
        </Button>
      </span>
    </Banner>
  )
}

/**
 * 登录门槛。
 *
 * 地址带在 `?next=` 上而不是路由 state 里：会话失效这一下是被动发生的，
 * 用户可能顺手刷新一次登录页，state 一刷就没了，`next` 还在。
 *
 * 这一页有没保存的改动时不能直接跳：跳走会连页面一起卸载，连问一句的机会都没有。
 * 这种情况留在原地，由页面里的 `RouteBlocker` 把这次失效当成第三个阻断来源——
 * 用户选「放弃修改」才去登录页。
 */
function RequireAuth({ children }: { children: ReactNode }) {
  const { status, sessionExpired } = useAuth()
  const location = useLocation()
  const dirty = useUnsavedChanges()
  // 只有「会话过期」才在原地问一句。用户自己点退出登录是另一回事：既有行为是
  // 直接换页——门槛返回 `<Navigate>`，页面连阻止器一起先卸载，没保存的改动会丢。
  const defer = sessionExpired && dirty
  if (status === 'loading') return <LoadingScreen />
  if (status === 'anonymous' && !defer) {
    return <Navigate to={loginPath(location)} replace />
  }
  // 这个形状必须是固定的：第一个位置永远是横幅（没有就是 null），第二个位置永远
  // 是页面子树。若把横幅按条件插在前面，React 按位置对账会把原来的页面换成
  // 横幅、再在下一个位置挂一棵新的页面子树——表单内容和脏状态全丢。
  return (
    <>
      {defer ? <SessionExpiredNotice /> : null}
      {children}
    </>
  )
}

/**
 * 视图的角色门槛。
 *
 * 它只决定「这个页面画不画」；真正的越权判定在服务端——这里放行了，
 * 请求照样可能拿到 403，而页面显示的就是服务端给的那句话。
 *
 * 会话过期期间一律放行：那一刻 `user` 已经变成 null，按角色判会换成「没有权限」，
 * 页面先被卸掉，脏状态跟着归零，`RequireAuth` 与 `RouteBlocker` 就再没机会问那
 * 一句了。放过这段只影响「画不画」，不多给任何权限——服务端该拒的照样拒。
 */
function RequireView({ view, children }: { view: View; children: ReactNode }) {
  const { user, sessionExpired } = useAuth()
  const { t } = useI18n()
  const { pathname } = useLocation()
  // 判定按地址走：商家视图里那三页是「同一批页面、两种主语」，管理员也进得去；
  // 渠道与计价仍然只对商家开放，所以不能只看传进来的视图名。
  if (!sessionExpired && (viewOfPath(pathname) !== view || !viewAllowed(user?.role, pathname))) {
    return (
      <Alert tone="danger" title={t('ops.noAccess')}>
        {t('ops.noAccessNext')}{' '}
        <Link to="/app" className="font-medium underline underline-offset-2">
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
    <div className="grid min-h-dvh place-items-center bg-background px-4">
      <Card className="w-full max-w-sm p-6 text-center">
        <h1 className="text-lg font-semibold tracking-tight">{t('errors.notFound')}</h1>
        <div className="mt-4">
          <Link to="/" className="text-sm underline underline-offset-2">
            {t('login.backHome')}
          </Link>
        </div>
      </Card>
    </div>
  )
}

/**
 * 路由表。
 *
 * 根上那一条不带路径，只做两件事：包住所有页面、给整棵树一个错误页。
 * 路由级异常先落到 `/app` 里的 `RouteErrorBoundary`（外壳还在，用户能切走）；
 * 根级页面（落地页、登录页、404）没有内层边界，由这里的 `errorElement` 接住，
 * 显示与最外层 `FatalErrorBoundary` 同一个「致命错误页」。
 */
const ROUTES: RouteObject[] = [
  {
    errorElement: <RootRouteError />,
    children: [
      { path: '/', element: withChunk(<Landing />) },
      { path: '/login', element: withChunk(<Login />) },
      {
        path: '/app',
        element: (
          <RequireAuth>
            <AppShell />
          </RequireAuth>
        ),
        children: [
          { index: true, element: withChunk(<Overview />) },
          { path: 'keys', element: withChunk(<Keys />) },
          { path: 'usage', element: withChunk(<Usage />) },
          { path: 'models', element: withChunk(<Models />) },
          // 规范名可能带 `/`（`vendor/model`），所以用 splat 接住整段。
          { path: 'models/*', element: withChunk(<ModelDetail />) },
          { path: 'quickstart', element: withChunk(<Quickstart />) },
          { path: 'settings', element: withChunk(<Settings />) },
          { path: 'merchant', element: <Navigate to="/app/merchant/overview" replace /> },
          {
            path: 'merchant/overview',
            element: <RequireView view="merchant">{withChunk(<MerchantOverview />)}</RequireView>,
          },
          {
            path: 'merchant/models',
            element: <RequireView view="merchant">{withChunk(<MerchantModels />)}</RequireView>,
          },
          {
            path: 'merchant/upstreams',
            element: <RequireView view="merchant">{withChunk(<MerchantUpstreams />)}</RequireView>,
          },
          {
            path: 'merchant/account',
            element: <RequireView view="merchant">{withChunk(<MerchantAccount />)}</RequireView>,
          },
          {
            path: 'merchant/settings',
            element: <RequireView view="merchant">{withChunk(<MerchantSettings />)}</RequireView>,
          },
          {
            path: 'merchant/channels',
            element: (
              <RequireView view="merchant">
                {withChunk(<OpsChannels scope="merchant" />)}
              </RequireView>
            ),
          },
          {
            path: 'merchant/pricing',
            element: <RequireView view="merchant">{withChunk(<MerchantPricing />)}</RequireView>,
          },
          {
            path: 'admin/settings',
            element: <RequireView view="admin">{withChunk(<Settings />)}</RequireView>,
          },
          { path: 'admin', element: <Navigate to="/app/admin/channels" replace /> },
          {
            path: 'admin/channels',
            element: (
              <RequireView view="admin">{withChunk(<OpsChannels scope="admin" />)}</RequireView>
            ),
          },
          {
            path: 'admin/models',
            element: <RequireView view="admin">{withChunk(<OpsModels />)}</RequireView>,
          },
          {
            path: 'admin/users',
            element: <RequireView view="admin">{withChunk(<AdminUsers />)}</RequireView>,
          },
          {
            path: 'admin/users/:id',
            element: <RequireView view="admin">{withChunk(<AdminUserDetailPage />)}</RequireView>,
          },
          {
            path: 'admin/aliases',
            element: <RequireView view="admin">{withChunk(<OpsAliases />)}</RequireView>,
          },
          {
            path: 'admin/pricing',
            element: <RequireView view="admin">{withChunk(<OpsPricing />)}</RequireView>,
          },
        ],
      },
      { path: '*', element: withChunk(<NotFound />) },
    ],
  },
]

export function App() {
  // 只建一次：`createBrowserRouter` 会订阅地址变化，每次渲染换一个实例等于把
  // 订阅原地换掉。
  const [router] = useState(() => createBrowserRouter(ROUTES))
  return (
    <FatalErrorBoundary>
      <I18nProvider>
        <ThemeProvider>
          <SiteProvider>
            <AuthProvider>
              {/* 脏状态登记处：页面登记、门槛读（见 `RequireAuth`）。 */}
              <UnsavedGuardProvider>
                <QueryClientProvider client={queryClient}>
                  {/* 演示横幅与轻提示不读路由，放在 RouterProvider 外面。 */}
                  <MockBanner />
                  <AppToaster />
                  <RouterProvider router={router} />
                </QueryClientProvider>
              </UnsavedGuardProvider>
            </AuthProvider>
          </SiteProvider>
        </ThemeProvider>
      </I18nProvider>
    </FatalErrorBoundary>
  )
}
