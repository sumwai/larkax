import { useState, type FormEvent } from 'react'
import { Link, Navigate, useLocation, useNavigate } from 'react-router-dom'
import { api } from '../api'
import { useAuth } from '../auth/AuthProvider'
import { useI18n } from '../i18n'
import { Alert, Button } from '../components/ui'
import { BrandMark } from '../components/icons'

interface Errors {
  username?: string
  password?: string
  form?: string
}

export function Login() {
  const { t } = useI18n()
  const { status, login } = useAuth()
  const navigate = useNavigate()
  const location = useLocation()
  const from = (location.state as { from?: string } | null)?.from ?? '/app'

  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [errors, setErrors] = useState<Errors>({})
  const [pending, setPending] = useState(false)

  if (status === 'authenticated') {
    return <Navigate to={from} replace />
  }

  const submit = async (event: FormEvent) => {
    event.preventDefault()
    const next: Errors = {}
    if (!username.trim()) next.username = t('login.usernameRequired')
    if (!password) next.password = t('login.passwordRequired')
    setErrors(next)
    if (next.username || next.password) return

    setPending(true)
    try {
      await login(username.trim(), password)
      navigate(from, { replace: true })
    } catch {
      setErrors({ form: t('login.failed') })
    } finally {
      setPending(false)
    }
  }

  return (
    <div className="auth-page">
      <div className="card auth-card">
        <div className="auth-head">
          <Link to="/" className="brand" style={{ padding: 0 }} aria-label={t('common.appName')}>
            <span className="brand-mark">
              <BrandMark />
            </span>
            <span className="word">{t('common.appName')}</span>
          </Link>
          <h1 style={{ fontSize: 22 }}>{t('login.title')}</h1>
          <p className="sub">{t('login.subtitle')}</p>
        </div>

        {errors.form && <Alert tone="bad">{errors.form}</Alert>}

        <form className="stack" onSubmit={submit} noValidate>
          <div className="field">
            <label htmlFor="login-username">{t('login.username')}</label>
            <input
              id="login-username"
              className={errors.username ? 'input invalid' : 'input'}
              value={username}
              autoComplete="username"
              placeholder={t('login.usernamePlaceholder')}
              aria-invalid={Boolean(errors.username)}
              aria-describedby={errors.username ? 'login-username-error' : undefined}
              onChange={(event) => {
                setUsername(event.target.value)
                if (errors.username) setErrors((previous) => ({ ...previous, username: undefined }))
              }}
            />
            {errors.username && (
              <span className="hint error" id="login-username-error">
                {errors.username}
              </span>
            )}
          </div>

          <div className="field">
            <label htmlFor="login-password">{t('login.password')}</label>
            <input
              id="login-password"
              type="password"
              className={errors.password ? 'input invalid' : 'input'}
              value={password}
              autoComplete="current-password"
              placeholder={t('login.passwordPlaceholder')}
              aria-invalid={Boolean(errors.password)}
              aria-describedby={errors.password ? 'login-password-error' : undefined}
              onChange={(event) => {
                setPassword(event.target.value)
                if (errors.password) setErrors((previous) => ({ ...previous, password: undefined }))
              }}
            />
            {errors.password && (
              <span className="hint error" id="login-password-error">
                {errors.password}
              </span>
            )}
          </div>

          <Button type="submit" loading={pending} style={{ width: '100%' }}>
            {pending ? t('login.submitting') : t('login.submit')}
          </Button>
        </form>

        {api.mode === 'mock' && <Alert tone="info">{t('login.mockHint')}</Alert>}

        <p className="auth-foot">{t('login.noRegistration')}</p>
        <p className="auth-foot">
          <Link to="/">{t('login.backHome')}</Link>
        </p>
      </div>
    </div>
  )
}
