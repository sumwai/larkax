import { useState, type ReactNode } from 'react'
import {
  createBrowserRouter,
  Navigate,
  RouterProvider,
  useLocation,
  type RouteObject,
} from 'react-router-dom'
import { QueryClientProvider } from '@tanstack/react-query'
import { AuthProvider, useAuth } from './auth/AuthProvider'
import { I18nProvider } from './i18n'
import { SiteProvider } from './site/SiteProvider'
import { ThemeProvider } from './theme/ThemeProvider'
import { FatalErrorBoundary, RootRouteError } from './components/ErrorBoundary'
import { MockBanner } from './components/MockBanner'
import { ConsoleShell } from './components/ConsoleShell'
import { AppToaster, Spinner } from './components/ui'
import { queryClient } from './api/queries/client'
import { Landing } from './pages/Landing'
import { Login } from './pages/Login'
import { Home } from './pages/Home'
import { Keys } from './pages/Keys'
import { Requests } from './pages/Requests'

/**
 * 路由表：落地页、登录页、控制台。控制台下属页面共用顶栏与导航。
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
            <ConsoleShell />
          </RequireAuth>
        ),
        children: [
          { index: true, element: <Home /> },
          { path: 'keys', element: <Keys /> },
          { path: 'requests', element: <Requests /> },
        ],
      },
      { path: '*', element: <Navigate to="/" replace /> },
    ],
  },
]

/**
 * 登录门槛。
 *
 * 会话恢复中不闪页面；匿名时去登录页，并把当前地址带在 `?next=` 上——
 * 放在查询串里而不是路由 state，刷新一次不会丢。
 */
function RequireAuth({ children }: { children: ReactNode }) {
  const { status } = useAuth()
  const location = useLocation()

  if (status === 'loading') {
    return (
      <div className="grid min-h-dvh place-items-center bg-background">
        <Spinner />
      </div>
    )
  }
  if (status === 'anonymous') {
    const next = `${location.pathname}${location.search}`
    return <Navigate to={`/login?next=${encodeURIComponent(next)}`} replace />
  }
  return <>{children}</>
}

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
              <QueryClientProvider client={queryClient}>
                <MockBanner />
                <AppToaster />
                <RouterProvider router={router} />
              </QueryClientProvider>
            </AuthProvider>
          </SiteProvider>
        </ThemeProvider>
      </I18nProvider>
    </FatalErrorBoundary>
  )
}
