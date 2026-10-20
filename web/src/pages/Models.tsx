import { useState } from 'react'
import { api } from '../api'
import { useI18n } from '../i18n'
import { useSite } from '../site/SiteProvider'
import { useAsync } from '../hooks/useAsync'
import { formatCount, formatDateTime, formatMoney, moneySymbol } from '../lib/format'
import type { MessageKey } from '../i18n/zh'
import { Alert, Button, Empty, PageHead, Skeleton } from '../components/ui'
import { IconModels, IconSearch } from '../components/icons'

const CAPABILITY: Record<string, MessageKey> = {
  vision: 'models.capability.vision',
  tools: 'models.capability.tools',
  reasoning: 'models.capability.reasoning',
  caching: 'models.capability.caching',
}

/** 能力的展示顺序固定，不随对象键的顺序漂移。 */
const CAPABILITY_ORDER = ['vision', 'tools', 'reasoning', 'caching']

export function Models() {
  const { t } = useI18n()
  const { meta } = useSite()
  const { data, error, loading, reload } = useAsync(() => api.models(), [])
  const [query, setQuery] = useState('')
  const symbol = moneySymbol(meta.currency)

  const needle = query.trim().toLowerCase()
  const items =
    data?.items.filter((model) => {
      if (!needle) return true
      return [model.name, model.display_name, model.vendor]
        .join(' ')
        .toLowerCase()
        .includes(needle)
    }) ?? []

  return (
    <>
      <PageHead
        title={t('models.title')}
        subtitle={t('models.subtitle')}
        actions={
          <div className="wrap-rel" style={{ minWidth: 220 }}>
            <label className="sr-only" htmlFor="model-search">
              {t('models.searchPlaceholder')}
            </label>
            <input
              id="model-search"
              className="input"
              style={{ paddingLeft: 36 }}
              value={query}
              placeholder={t('models.searchPlaceholder')}
              onChange={(event) => setQuery(event.target.value)}
            />
            <IconSearch
              size={15}
              style={{ position: 'absolute', left: 12, top: 14, color: 'var(--faint)' }}
            />
          </div>
        }
      />

      {error && (
        <Alert tone="bad" title={t('models.loadFailed')}>
          <Button variant="secondary" size="sm" onClick={reload} style={{ marginTop: 8 }}>
            {t('common.retry')}
          </Button>
        </Alert>
      )}

      {loading && !data && (
        <div className="model-grid">
          {[0, 1, 2, 3].map((key) => (
            <div key={key} className="card panel">
              <Skeleton width="55%" height={18} />
              <Skeleton width="35%" height={12} />
              <Skeleton height={38} />
            </div>
          ))}
        </div>
      )}

      {data && (
        <>
          <div className="row">
            <span className="tiny">
              {t('models.referenceAt', { time: formatDateTime(data.reference_at, meta.timezone) })}
            </span>
            <span className="spacer" />
            <span className="tiny">{t('models.priceNote')}</span>
          </div>

          {items.length === 0 ? (
            <div className="card">
              <Empty icon={<IconModels size={20} />} title={t('models.noModels')} />
            </div>
          ) : (
            <div className="model-grid">
              {items.map((model) => (
                <article key={model.name} className="card model-card">
                  <div className="name">
                    <b>{model.display_name}</b>
                    <span className="tag">{model.vendor}</span>
                  </div>
                  <div className="row" style={{ gap: 8 }}>
                    <span className="mono tiny">{model.name}</span>
                    {model.context_limit > 0 && (
                      <span className="tiny">
                        {t('models.context')} · {formatCount(model.context_limit)}
                      </span>
                    )}
                  </div>
                  <div className="badges">
                    {CAPABILITY_ORDER.filter((key) => model.capabilities[key] === true).map((key) => (
                      <span key={key} className="tag">
                        {t(CAPABILITY[key])}
                      </span>
                    ))}
                  </div>
                  <div className="price">
                    <div>
                      <span>{t('models.inputPrice')}</span>
                      <b>
                        {model.pricing
                          ? `${symbol}${formatMoney(model.pricing.input_per_mtok)}`
                          : t('common.none')}
                      </b>
                    </div>
                    <div>
                      <span>{t('models.outputPrice')}</span>
                      <b>
                        {model.pricing
                          ? `${symbol}${formatMoney(model.pricing.output_per_mtok)}`
                          : t('common.none')}
                      </b>
                    </div>
                  </div>
                  {!model.pricing && <span className="price-note">{t('models.noPricing')}</span>}
                </article>
              ))}
            </div>
          )}
        </>
      )}
    </>
  )
}
