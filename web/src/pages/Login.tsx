import { useState, type FormEvent } from 'react'
import { Link, useNavigate, useSearchParams } from 'react-router-dom'
import { useAuth } from '../auth/AuthProvider'
import { useI18n } from '../i18n'
import { useSite } from '../site/SiteProvider'
import { useDocumentTitle } from '../components/PageTitle'
import { Alert, Button, Card, CardBody, Field, Input } from '../components/ui'

/**
 * 登录页：用户名 + 口令。
 *
 * 失败文案分两种：服务端明确说口令不对（401）时用 `login.failed`，
 * 其余（网络、5xx）走 `errors.generic`——断言错了原因会把用户引向
 * 改口令，而问题可能在自己这一侧。
 *
 * 登录后的去向读 `?next=`：地址带在查询串上而不是路由 state 里，
 * 刷新一次不会丢。
 */
export function Login() {
  const { t } = useI18n()
  const { meta } = useSite()
  const { login } = useAuth()
  const navigate = useNavigate()
  const [params] = useSearchParams()

  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState<'failed' | 'generic' | null>(null)
  const [missing, setMissing] = useState<{ username?: boolean; password?: boolean }>({})
  const [busy, setBusy] = useState(false)

  useDocumentTitle(`${t('login.documentTitle')} · ${meta.site_name}`)

  const next = params.get('next') || '/app'

  async function onSubmit(event: FormEvent) {
    event.preventDefault()
    const empty = {
      username: username.trim() === '' || undefined,
      password: password === '' || undefined,
    }
    setMissing(empty)
    if (empty.username || empty.password) return

    setBusy(true)
    setError(null)
    try {
      await login(username.trim(), password)
      navigate(next, { replace: true })
    } catch (err) {
      const status = (err as { status?: number }).status
      setError(status === 401 ? 'failed' : 'generic')
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="grid min-h-dvh place-items-center bg-background px-4">
      <div className="w-full max-w-sm">
        <Card>
          <CardBody className="flex flex-col gap-5 p-6">
            <div>
              <h1 className="text-lg font-semibold tracking-tight">{t('login.title')}</h1>
              <p className="mt-1 text-sm text-muted-foreground">{t('login.subtitle')}</p>
            </div>

            {error ? (
              <Alert
                tone="danger"
                title={t(error === 'failed' ? 'login.failed' : 'errors.generic')}
              />
            ) : null}

            <form className="flex flex-col gap-4" onSubmit={onSubmit} noValidate>
              <Field
                label={t('login.username')}
                error={missing.username ? t('login.usernameRequired') : undefined}
              >
                {({ inputId, describedBy }) => (
                  <Input
                    id={inputId}
                    aria-describedby={describedBy}
                    invalid={Boolean(missing.username)}
                    autoComplete="username"
                    placeholder={t('login.usernamePlaceholder')}
                    value={username}
                    onChange={(event) => setUsername(event.target.value)}
                  />
                )}
              </Field>

              <Field
                label={t('login.password')}
                error={missing.password ? t('login.passwordRequired') : undefined}
              >
                {({ inputId, describedBy }) => (
                  <Input
                    id={inputId}
                    type="password"
                    aria-describedby={describedBy}
                    invalid={Boolean(missing.password)}
                    autoComplete="current-password"
                    placeholder={t('login.passwordPlaceholder')}
                    value={password}
                    onChange={(event) => setPassword(event.target.value)}
                  />
                )}
              </Field>

              <Button type="submit" busy={busy}>
                {busy ? t('login.submitting') : t('login.submit')}
              </Button>
            </form>

            <div className="flex flex-col gap-1 text-xs text-faint">
              <p>{t('login.noRegistration')}</p>
              <Link to="/" className="underline underline-offset-2">
                {t('login.backHome')}
              </Link>
            </div>
          </CardBody>
        </Card>
      </div>
    </div>
  )
}
