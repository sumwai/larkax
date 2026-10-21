import { useState, type FormEvent } from 'react'
import { useNavigate } from 'react-router-dom'
import { api } from '../api'
import { useAuth } from '../auth/AuthProvider'
import { useI18n } from '../i18n'
import type { Lang } from '../i18n'
import { useTheme, type Theme } from '../theme/ThemeProvider'
import {
  Alert,
  Button,
  Modal,
  PageHead,
  Select,
  useToast,
} from '../components/ui'
import { IconLogout } from '../components/icons'

const EMAIL = /^[^\s@]+@[^\s@]+\.[^\s@]+$/

export function Settings() {
  const { t, lang, setLang } = useI18n()
  const { theme, setTheme } = useTheme()
  const { user, setUser, logout } = useAuth()
  const { show: toast } = useToast()
  const navigate = useNavigate()

  const [displayName, setDisplayName] = useState(user?.display_name ?? '')
  const [email, setEmail] = useState(user?.email ?? '')
  const [profileError, setProfileError] = useState<string | null>(null)
  const [profilePending, setProfilePending] = useState(false)

  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [confirm, setConfirm] = useState('')
  const [passwordError, setPasswordError] = useState<string | null>(null)
  const [passwordPending, setPasswordPending] = useState(false)

  const [logoutOpen, setLogoutOpen] = useState(false)
  const [logoutPending, setLogoutPending] = useState(false)

  const saveProfile = async (event: FormEvent) => {
    event.preventDefault()
    const trimmedEmail = email.trim()
    if (trimmedEmail && !EMAIL.test(trimmedEmail)) {
      setProfileError(t('settings.emailInvalid'))
      return
    }
    setProfilePending(true)
    setProfileError(null)
    try {
      const updated = await api.updateMe({
        display_name: displayName.trim() || null,
        email: trimmedEmail || null,
      })
      setUser(updated)
      toast(t('settings.profileSaved'))
    } catch {
      setProfileError(t('settings.profileFailed'))
    } finally {
      setProfilePending(false)
    }
  }

  const changePassword = async (event: FormEvent) => {
    event.preventDefault()
    if (!current) {
      setPasswordError(t('settings.currentRequired'))
      return
    }
    if (next.length < 12) {
      setPasswordError(t('settings.passwordTooShort'))
      return
    }
    if (next.length > 128) {
      setPasswordError(t('settings.passwordTooLong'))
      return
    }
    if (next === user?.username) {
      setPasswordError(t('settings.passwordEqualsUsername'))
      return
    }
    if (next === current) {
      setPasswordError(t('settings.passwordSame'))
      return
    }
    if (next !== confirm) {
      setPasswordError(t('settings.passwordMismatch'))
      return
    }

    setPasswordPending(true)
    setPasswordError(null)
    try {
      await api.changePassword(current, next)
      setCurrent('')
      setNext('')
      setConfirm('')
      toast(t('settings.passwordChanged'))
    } catch {
      setPasswordError(t('settings.passwordFailed'))
    } finally {
      setPasswordPending(false)
    }
  }

  const doLogout = async () => {
    setLogoutPending(true)
    try {
      await logout()
      navigate('/login', { replace: true })
    } finally {
      setLogoutPending(false)
    }
  }

  const themeOptions: Array<{ value: Theme; label: string }> = [
    { value: 'system', label: t('settings.themeSystem') },
    { value: 'light', label: t('settings.themeLight') },
    { value: 'dark', label: t('settings.themeDark') },
  ]

  return (
    <>
      <PageHead title={t('settings.title')} subtitle={t('settings.subtitle')} />

      <div className="grid-2">
        <section className="card panel">
          <div>
            <h2>{t('settings.profile')}</h2>
            <p className="sub">{t('settings.profileBody')}</p>
          </div>
          <form className="stack" onSubmit={saveProfile}>
            {profileError && <Alert tone="bad">{profileError}</Alert>}
            <div className="field">
              <label htmlFor="settings-username">{t('settings.username')}</label>
              <input
                id="settings-username"
                className="input is-readonly"
                value={user?.username ?? ''}
                readOnly
              />
            </div>
            <div className="field">
              <label htmlFor="settings-display-name">{t('settings.displayName')}</label>
              <input
                id="settings-display-name"
                className="input"
                value={displayName}
                onChange={(event) => setDisplayName(event.target.value)}
              />
            </div>
            <div className="field">
              <label htmlFor="settings-email">{t('settings.email')}</label>
              <input
                id="settings-email"
                type="email"
                className={profileError ? 'input invalid' : 'input'}
                value={email}
                placeholder="user@example.com"
                onChange={(event) => {
                  setEmail(event.target.value)
                  if (profileError) setProfileError(null)
                }}
              />
            </div>
            <div className="form-actions">
              <Button type="submit" loading={profilePending}>
                {t('settings.saveProfile')}
              </Button>
            </div>
          </form>
        </section>

        <section className="card panel">
          <div>
            <h2>{t('settings.password')}</h2>
            <p className="sub">{t('settings.passwordBody')}</p>
          </div>
          <form className="stack" onSubmit={changePassword}>
            {passwordError && <Alert tone="bad">{passwordError}</Alert>}
            <div className="field">
              <label htmlFor="settings-current">{t('settings.currentPassword')}</label>
              <input
                id="settings-current"
                type="password"
                className="input"
                autoComplete="current-password"
                value={current}
                onChange={(event) => setCurrent(event.target.value)}
              />
            </div>
            <div className="field">
              <label htmlFor="settings-next">{t('settings.newPassword')}</label>
              <input
                id="settings-next"
                type="password"
                className="input"
                autoComplete="new-password"
                value={next}
                onChange={(event) => setNext(event.target.value)}
              />
            </div>
            <div className="field">
              <label htmlFor="settings-confirm">{t('settings.confirmPassword')}</label>
              <input
                id="settings-confirm"
                type="password"
                className="input"
                autoComplete="new-password"
                value={confirm}
                onChange={(event) => setConfirm(event.target.value)}
              />
            </div>
            <div className="form-actions">
              <Button type="submit" loading={passwordPending}>
                {t('settings.changePassword')}
              </Button>
            </div>
          </form>
        </section>
      </div>

      <section className="card panel">
        <div>
          <h2>{t('settings.appearance')}</h2>
          <p className="sub">{t('settings.subtitle')}</p>
        </div>
        <div className="form-grid">
          <div className="field">
            <label>{t('settings.language')}</label>
            <Select<Lang>
              value={lang}
              ariaLabel={t('settings.language')}
              options={[
                { value: 'zh', label: '中文' },
                { value: 'en', label: 'English' },
              ]}
              onChange={setLang}
            />
          </div>
          <div className="field">
            <label>{t('settings.theme')}</label>
            <div className="seg" role="group" aria-label={t('settings.theme')}>
              {themeOptions.map((option) => (
                <button
                  key={option.value}
                  aria-pressed={theme === option.value}
                  onClick={() => setTheme(option.value)}
                >
                  {option.label}
                </button>
              ))}
            </div>
          </div>
        </div>
      </section>

      <section className="card panel">
        <div className="head">
          <div>
            <h2>{t('settings.logout')}</h2>
            <p>{t('settings.logoutBody')}</p>
          </div>
          <Button variant="danger" icon={<IconLogout size={15} />} onClick={() => setLogoutOpen(true)}>
            {t('settings.logout')}
          </Button>
        </div>
      </section>

      {logoutOpen && (
        <Modal
          sheetSize="sm"
          title={t('settings.logout')}
          description={t('settings.logoutBody')}
          onClose={() => setLogoutOpen(false)}
          footer={
            <>
              <Button variant="secondary" onClick={() => setLogoutOpen(false)}>
                {t('common.cancel')}
              </Button>
              <Button variant="danger" loading={logoutPending} onClick={doLogout}>
                {t('settings.logoutConfirm')}
              </Button>
            </>
          }
        >
          <p className="sub">{user?.display_name ?? user?.username}</p>
        </Modal>
      )}
    </>
  )
}
