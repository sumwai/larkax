import { Link } from 'react-router-dom'
import { useI18n } from '../i18n'
import { useSite } from '../site/SiteProvider'
import { useDocumentTitle } from '../components/PageTitle'
import { Badge, Card, CardBody, CopyButton, buttonVariants } from '../components/ui'

/**
 * 单页落地页：没有路由、没有数据依赖，只读 token。
 * 三块内容——标题与入口、API base URL、三条能力说明。
 */
export function Landing() {
  const { t } = useI18n()
  const { meta } = useSite()

  useDocumentTitle(`${t('landing.title')} · ${t('common.appName')}`)

  return (
    <div className="flex min-h-dvh flex-col bg-background">
      <header className="mx-auto flex w-full max-w-(--content-max) items-center gap-2 px-4 py-4">
        <span
          aria-hidden="true"
          className="flex size-7 items-center justify-center rounded-xs bg-primary font-mono text-xs font-semibold text-primary-foreground"
        >
          lx
        </span>
        <span className="text-md font-semibold tracking-tight">{t('common.appName')}</span>
        <nav className="ml-auto flex items-center gap-2">
          <Link to="/login" className={buttonVariants({ variant: 'ghost' })}>
            {t('landing.ctaLogin')}
          </Link>
        </nav>
      </header>

      <main className="mx-auto flex w-full max-w-(--content-max) flex-1 flex-col items-center justify-center px-4 pb-16">
        <section className="max-w-xl py-10 text-center">
          <Badge>{t('landing.eyebrow')}</Badge>
          <h1 className="mt-4 text-2xl font-semibold tracking-tight text-balance">
            {t('landing.title')}
          </h1>
          <p className="mt-3 text-base text-muted-foreground">{t('landing.subtitle')}</p>
          <div className="mt-6 flex justify-center">
            <Link to="/login" className={buttonVariants({ size: 'lg' })}>
              {t('landing.ctaLogin')}
            </Link>
          </div>
        </section>

        <Card className="w-full max-w-md">
          <CardBody className="flex flex-col gap-1.5">
            <div className="flex items-center justify-between gap-2">
              <span className="text-caps text-muted-foreground">{t('landing.baseUrlLabel')}</span>
              <CopyButton
                value={meta.api_base_url}
                label={t('common.copy')}
                copiedLabel={t('common.copied')}
              />
            </div>
            <span className="mono truncate text-sm">{meta.api_base_url}</span>
            <p className="text-xs text-faint">{t('landing.baseUrlHint')}</p>
          </CardBody>
        </Card>

        <section className="mt-8 grid w-full gap-4 sm:grid-cols-3">
          {(
            [
              ['landing.feature1Title', 'landing.feature1Body'],
              ['landing.feature2Title', 'landing.feature2Body'],
              ['landing.feature3Title', 'landing.feature3Body'],
            ] as const
          ).map(([titleKey, bodyKey]) => (
            <Card key={titleKey}>
              <CardBody className="flex flex-col gap-1.5">
                <h2 className="text-md font-semibold tracking-tight">{t(titleKey)}</h2>
                <p className="text-sm text-muted-foreground">{t(bodyKey)}</p>
              </CardBody>
            </Card>
          ))}
        </section>
      </main>

      <footer className="border-t border-border px-4 py-6 text-center text-xs text-faint">
        {t('landing.footer')}
      </footer>
    </div>
  )
}
