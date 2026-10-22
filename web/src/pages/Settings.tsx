import { useState, type FormEvent } from 'react'
import { api } from '../api'
import { useAuth } from '../auth/AuthProvider'
import { useI18n } from '../i18n'
import type { Lang } from '../i18n'
import { useTheme, type Theme } from '../theme/ThemeProvider'
import { Alert, Button, PageHead, Select, useToast } from '../components/ui'

const EMAIL = /^[^\s@]+@[^\s@]+\.[^\s@]+$/

/**
 * 账号相关的两块：账户信息与修改口令。
 *
 * 商家视图把「账号」与「设置」拆成两页，用户与管理员没有「账号」这一项，
 * 这两块仍留在他们的设置页里。两处渲染的是同一份实现，不是各抄一份表单——
 * 复制一份，改文案时就会只改到一边。
 *
 * 退出登录不在这里：顶栏账户菜单与移动端抽屉各有一条，够了。再放一条就是
 * 第三个入口，还要多一个确认对话框。
 */
export function AccountSections() {
  const { t } = useI18n()
  const { user, setUser } = useAuth()
  const { show: toast } = useToast()

  const [displayName, setDisplayName] = useState(user?.display_name ?? '')
  const [email, setEmail] = useState(user?.email ?? '')
  const [profileError, setProfileError] = useState<string | null>(null)
  const [profilePending, setProfilePending] = useState(false)

  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [confirm, setConfirm] = useState('')
  const [passwordError, setPasswordError] = useState<string | null>(null)
  const [passwordPending, setPasswordPending] = useState(false)

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

  return (
    <>
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
    </>
  )
}

/**
 * 界面偏好：语言与主题。
 *
 * 两项都存在浏览器里，只影响这台设备的显示，不落服务端；商家业务配置
 * 是另一类东西，等有了商家级的 settings 作用域再放进来。
 */
export function AppearanceSection() {
  const { t, lang, setLang } = useI18n()
  const { theme, setTheme } = useTheme()

  const themeOptions: Array<{ value: Theme; label: string }> = [
    { value: 'system', label: t('settings.themeSystem') },
    { value: 'light', label: t('settings.themeLight') },
    { value: 'dark', label: t('settings.themeDark') },
  ]

  return (
    <section className="card panel">
      <div>
        <h2>{t('settings.appearance')}</h2>
        <p className="sub">{t('settings.appearanceBody')}</p>
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
  )
}

/** 用户与管理员看到的设置页：账号段与偏好段都在这一页里。 */
export function Settings() {
  const { t } = useI18n()
  return (
    <>
      <PageHead title={t('settings.title')} subtitle={t('settings.subtitle')} />
      <AccountSections />
      <AppearanceSection />
    </>
  )
}

/** 商家视图的设置页：只有界面偏好，账号段在同级的「账号」页。 */
export function MerchantSettings() {
  const { t } = useI18n()
  return (
    <>
      <PageHead title={t('settings.title')} subtitle={t('settings.preferencesSubtitle')} />
      <AppearanceSection />
    </>
  )
}
