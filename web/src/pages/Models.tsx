import { useState } from 'react'
import { Link } from 'react-router-dom'
import { api } from '../api'
import { useI18n } from '../i18n'
import { useAsync } from '../hooks/useAsync'
import { formatCompact } from '../lib/format'
import { encodeModelName } from '../lib/modelName'
import type { MessageKey } from '../i18n/zh'
import { Alert, Button, Empty, PageHead, Skeleton } from '../components/ui'
import { IconModels } from '../components/icons'
import { SearchBox } from './ops/common'

const CAPABILITY: Record<string, MessageKey> = {
  vision: 'models.capability.vision',
  tools: 'models.capability.tools',
  reasoning: 'models.capability.reasoning',
  caching: 'models.capability.caching',
}

/** 能力的展示顺序固定，不随对象键的顺序漂移。 */
const CAPABILITY_ORDER = ['vision', 'tools', 'reasoning', 'caching']

/**
 * 详情的地址。
 *
 * 规范名可能带 `/`（`vendor/model` 这类），所以按段编码、把斜杠留给详情路由
 * 的 splat；其余字符交给 `encodeModelName`，免得 `?` 与 `#` 把地址截断。
 */
function detailPath(name: string): string {
  return `/app/models/${encodeModelName(name)}`
}

function modalityList(values: string[] | undefined, none: string): string {
  return values && values.length > 0 ? values.join(' / ') : none
}

export function Models() {
  const { t } = useI18n()
  const { data, error, loading, reload } = useAsync(() => api.models(), [])
  const [query, setQuery] = useState('')
  const [vendor, setVendor] = useState('')

  const all = data?.items ?? []
  // 厂商筛选不写死：目录里出现什么就列什么。
  const vendors = Array.from(new Set(all.map((model) => model.vendor).filter(Boolean))).sort()

  const needle = query.trim().toLowerCase()
  const items = all.filter((model) => {
    if (vendor && model.vendor !== vendor) return false
    if (!needle) return true
    return [model.name, model.display_name, model.vendor, model.family]
      .filter(Boolean)
      .join(' ')
      .toLowerCase()
      .includes(needle)
  })
  const filtered = needle !== '' || vendor !== ''

  return (
    <>
      <PageHead
        title={t('models.title')}
        subtitle={t('models.subtitle')}
        actions={
          <div className="model-filters">
            <SearchBox
              id="model-search"
              icon
              className="model-search"
              placeholder={t('models.searchPlaceholder')}
              value={query}
              onChange={setQuery}
            />
            <label className="sr-only" htmlFor="model-vendor">
              {t('models.vendor')}
            </label>
            <select
              id="model-vendor"
              className="input"
              value={vendor}
              onChange={(event) => setVendor(event.target.value)}
            >
              <option value="">{t('models.allVendors')}</option>
              {vendors.map((name) => (
                <option key={name} value={name}>
                  {name}
                </option>
              ))}
            </select>
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
        <div className="model-list" aria-hidden="true">
          {[0, 1, 2, 3].map((key) => (
            <div key={key} className="model-skeleton">
              <Skeleton width="35%" height={16} />
              <Skeleton width="60%" height={12} />
            </div>
          ))}
        </div>
      )}

      {data && items.length === 0 && (
        <div className="card">
          {filtered ? (
            <Empty icon={<IconModels size={20} />} title={t('models.noModels')} />
          ) : (
            <Empty
              icon={<IconModels size={20} />}
              title={t('models.emptyTitle')}
              body={t('models.emptyBody')}
              action={
                <Link className="btn secondary" to="/app/quickstart">
                  {t('models.emptyAction')}
                </Link>
              }
            />
          )}
        </div>
      )}

      {data && items.length > 0 && (
        <div className="model-list">
          {items.map((model) => {
            const badges = CAPABILITY_ORDER.filter((key) => model.capabilities[key] === true)
            return (
              <Link key={model.name} className="model-item" to={detailPath(model.name)}>
                <span className="model-item-main">
                  <span className="model-item-title">
                    <b>{model.display_name || model.name}</b>
                    <span className="mono tiny">{model.name}</span>
                  </span>
                  <span className="model-meta tiny">
                    <span>{[model.vendor, model.family].filter(Boolean).join(' · ')}</span>
                    {model.context_limit > 0 && (
                      <span>
                        {t('models.context')} {formatCompact(model.context_limit)}
                      </span>
                    )}
                    <span>
                      {t('models.modalityInput')}{' '}
                      {modalityList(model.modalities.input, t('models.modalityNone'))}
                    </span>
                    <span>
                      {t('models.modalityOutput')}{' '}
                      {modalityList(model.modalities.output, t('models.modalityNone'))}
                    </span>
                  </span>
                  {badges.length > 0 && (
                    <span className="badges">
                      {badges.map((key) => (
                        <span key={key} className="tag">
                          {t(CAPABILITY[key])}
                        </span>
                      ))}
                    </span>
                  )}
                </span>
                <span className="model-item-go" aria-hidden="true">
                  →
                </span>
              </Link>
            )
          })}
        </div>
      )}
    </>
  )
}
