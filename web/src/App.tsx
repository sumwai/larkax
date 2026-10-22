import type { ReactNode } from 'react'
import { BrowserRouter, Link, Navigate, Route, Routes, useLocation } from 'react-router-dom'
import { AuthProvider, useAuth } from './auth/AuthProvider'
import { I18nProvider, useI18n } from './i18n'
import { SiteProvider } from './site/SiteProvider'
import { ThemeProvider } from './theme/ThemeProvider'
import { AppShell } from './components/AppShell'
import { FatalErrorBoundary } from './components/ErrorBoundary'
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

function RequireAuth({ children }: { children: ReactNode }) {
  const { status } = useAuth()
  const location = useLocation()
  if (status === 'loading') return <LoadingScreen />
  if (status === 'anonymous') {
    return <Navigate to="/login" replace state={{ from: location.pathname }} />
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

export function App() {
  return (
    <FatalErrorBoundary>
      <I18nProvider>
        <ThemeProvider>
          <ToastProvider>
            <SiteProvider>
              <AuthProvider>
                <BrowserRouter>
                  <MockBanner />
                  <Routes>
                    <Route path="/" element={<Landing />} />
                    <Route path="/login" element={<Login />} />
                    <Route
                      path="/app"
                      element={
                        <RequireAuth>
                          <AppShell />
                        </RequireAuth>
                      }
                    >
                      <Route index element={<Overview />} />
                      <Route path="keys" element={<Keys />} />
                      <Route path="usage" element={<Usage />} />
                      <Route path="models" element={<Models />} />
                      {/* 规范名可能带 `/`（`vendor/model`），所以用 splat 接住整段。 */}
                      <Route path="models/*" element={<ModelDetail />} />
                      <Route path="quickstart" element={<Quickstart />} />
                      <Route path="settings" element={<Settings />} />
                      <Route
                        path="merchant"
                        element={<Navigate to="/app/merchant/overview" replace />}
                      />
                      <Route
                        path="merchant/overview"
                        element={
                          <RequireView view="merchant">
                            <MerchantOverview />
                          </RequireView>
                        }
                      />
                      <Route
                        path="merchant/models"
                        element={
                          <RequireView view="merchant">
                            <MerchantModels />
                          </RequireView>
                        }
                      />
                      <Route
                        path="merchant/upstreams"
                        element={
                          <RequireView view="merchant">
                            <MerchantUpstreams />
                          </RequireView>
                        }
                      />
                      <Route
                        path="merchant/account"
                        element={
                          <RequireView view="merchant">
                            <MerchantAccount />
                          </RequireView>
                        }
                      />
                      <Route
                        path="merchant/settings"
                        element={
                          <RequireView view="merchant">
                            <MerchantSettings />
                          </RequireView>
                        }
                      />
                      <Route
                        path="merchant/channels"
                        element={
                          <RequireView view="merchant">
                            <OpsChannels scope="merchant" />
                          </RequireView>
                        }
                      />
                      <Route
                        path="merchant/pricing"
                        element={
                          <RequireView view="merchant">
                            <MerchantPricing />
                          </RequireView>
                        }
                      />
                      <Route
                        path="admin/settings"
                        element={
                          <RequireView view="admin">
                            <Settings />
                          </RequireView>
                        }
                      />
                      <Route path="admin" element={<Navigate to="/app/admin/channels" replace />} />
                      <Route
                        path="admin/channels"
                        element={
                          <RequireView view="admin">
                            <OpsChannels scope="admin" />
                          </RequireView>
                        }
                      />
                      <Route
                        path="admin/models"
                        element={
                          <RequireView view="admin">
                            <OpsModels />
                          </RequireView>
                        }
                      />
                      <Route
                        path="admin/users"
                        element={
                          <RequireView view="admin">
                            <AdminUsers />
                          </RequireView>
                        }
                      />
                      <Route
                        path="admin/users/:id"
                        element={
                          <RequireView view="admin">
                            <AdminUserDetailPage />
                          </RequireView>
                        }
                      />
                      <Route
                        path="admin/aliases"
                        element={
                          <RequireView view="admin">
                            <OpsAliases />
                          </RequireView>
                        }
                      />
                      <Route
                        path="admin/pricing"
                        element={
                          <RequireView view="admin">
                            <OpsPricing />
                          </RequireView>
                        }
                      />
                    </Route>
                    <Route path="*" element={<NotFound />} />
                  </Routes>
                </BrowserRouter>
              </AuthProvider>
            </SiteProvider>
          </ToastProvider>
        </ThemeProvider>
      </I18nProvider>
    </FatalErrorBoundary>
  )
}
