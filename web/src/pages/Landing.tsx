import { Link } from 'react-router-dom'
import { api } from '../api'
import { useAuth } from '../auth/AuthProvider'
import { useSite } from '../site/SiteProvider'
import { useI18n } from '../i18n'
import type { MessageKey } from '../i18n/zh'
import { useAsync } from '../hooks/useAsync'
import { formatMoney, moneySymbol } from '../lib/format'
import { BrandMark } from '../components/icons'
import { CopyButton } from '../components/ui'

export function Landing() {
  const { t } = useI18n()
  const { meta } = useSite()
  const { status } = useAuth()
  const signedIn = status === 'authenticated'
  const { data: models } = useAsync(() => api.models(), [])
  const preview = (models?.items ?? []).filter((model) => model.pricing).slice(0, 4)
  const symbol = moneySymbol(meta.currency)

  return (
    <div className="landing">
      <header className="landing-nav">
        <Link to="/" className="brand" aria-label={t('common.appName')}>
          <span className="brand-mark">
            <BrandMark />
          </span>
          <span className="word">{t('common.appName')}</span>
        </Link>
        <nav className="links" aria-label={t('common.console')}>
          <a href="#features">{t('landing.navFeatures')}</a>
          <a href="#pricing">{t('landing.navModels')}</a>
          <a href="https://docs.larkax.com" target="_blank" rel="noreferrer">
            {t('landing.navDocs')}
          </a>
        </nav>
        <div className="spacer" />
        <Link className={signedIn ? 'btn' : 'btn secondary'} to={signedIn ? '/app' : '/login'}>
          {signedIn ? t('common.console') : t('login.submit')}
        </Link>
      </header>

      <main className="landing-main">
        <section className="hero">
          <span className="eyebrow">{t('landing.eyebrow')}</span>
          <h1>{t('landing.title')}</h1>
          <p className="sub">{t('landing.subtitle')}</p>
          <div className="hero-actions">
            <Link className="btn" to={signedIn ? '/app' : '/login'}>
              {signedIn ? t('common.console') : t('landing.ctaLogin')}
            </Link>
            <Link className="btn secondary" to="/app/models">
              {t('landing.ctaModels')}
            </Link>
          </div>
          <div className="base-url-box">
            <span className="tiny">{t('landing.baseUrlLabel')}</span>
            <div className="copyrow">
              <span className="copyfield mono">{meta.api_base_url}</span>
              <CopyButton text={meta.api_base_url} />
            </div>
            <span className="hint">{t('landing.baseUrlHint')}</span>
          </div>
        </section>

        <section id="features" className="feature-grid">
          {(
            [
              ['landing.feature1Title', 'landing.feature1Body'],
              ['landing.feature2Title', 'landing.feature2Body'],
              ['landing.feature3Title', 'landing.feature3Body'],
            ] as Array<[MessageKey, MessageKey]>
          ).map(([title, body]) => (
            <article key={title} className="card feature">
              <h3>{t(title)}</h3>
              <p>{t(body)}</p>
            </article>
          ))}
        </section>

        <section id="pricing" className="card panel">
          <div className="head">
            <div>
              <h2>{t('landing.navModels')}</h2>
              <p>{t('models.subtitle')}</p>
            </div>
            <Link className="btn secondary sm" to="/app/models">
              {t('landing.ctaModels')}
            </Link>
          </div>
          {preview.length > 0 && (
            <div className="price-strip">
              {preview.map((model) => (
                <div key={model.name} className="price-item">
                  <span className="mono tiny">{model.name}</span>
                  <div className="row" style={{ justifyContent: 'space-between', gap: 8 }}>
                    <span className="tiny">{t('models.inputPrice')}</span>
                    <b className="num">
                      {symbol}
                      {formatMoney(model.pricing?.input_per_mtok)}
                    </b>
                  </div>
                  <div className="row" style={{ justifyContent: 'space-between', gap: 8 }}>
                    <span className="tiny">{t('models.outputPrice')}</span>
                    <b className="num">
                      {symbol}
                      {formatMoney(model.pricing?.output_per_mtok)}
                    </b>
                  </div>
                </div>
              ))}
            </div>
          )}
        </section>
      </main>

      <footer className="site-footer">{t('landing.footer')}</footer>
    </div>
  )
}
