import type { ReactNode } from 'react'
import { BrowserRouter, Link, Navigate, Route, Routes, useLocation } from 'react-router-dom'
import { AuthProvider, useAuth } from './auth/AuthProvider'
import { I18nProvider, useI18n } from './i18n'
import { SiteProvider } from './site/SiteProvider'
import { ThemeProvider } from './theme/ThemeProvider'
import { AppShell } from './components/AppShell'
import { MockBanner } from './components/MockBanner'
import { ToastProvider } from './components/ui'
import { Landing } from './pages/Landing'
import { Login } from './pages/Login'
import { Overview } from './pages/Overview'
import { Keys } from './pages/Keys'
import { Usage } from './pages/Usage'
import { Models } from './pages/Models'
import { Quickstart } from './pages/Quickstart'
import { Settings } from './pages/Settings'

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
                    <Route path="quickstart" element={<Quickstart />} />
                    <Route path="settings" element={<Settings />} />
                  </Route>
                  <Route path="*" element={<NotFound />} />
                </Routes>
              </BrowserRouter>
            </AuthProvider>
          </SiteProvider>
        </ToastProvider>
      </ThemeProvider>
    </I18nProvider>
  )
}
