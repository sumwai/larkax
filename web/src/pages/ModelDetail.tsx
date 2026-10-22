import { Link, useParams } from 'react-router-dom'
import { api, ApiError } from '../api'
import type { ModelCapabilities, ModelDetailResponse, PriceDimension, SupplierPrice } from '../api/types'
import { Alert, Button, Empty, PageHead, Pill, Skeleton } from '../components/ui'
import { IconModels } from '../components/icons'
import { useAsync } from '../hooks/useAsync'
import { useI18n } from '../i18n'
import type { MessageKey } from '../i18n/zh'
import { formatCount, formatDateTime, formatUnitPrice, moneySymbol } from '../lib/format'
import { useSite } from '../site/SiteProvider'

type CapabilityKey = 'reasoning' | 'tools' | 'vision' | 'audio' | 'cache' | 'batch'

/** 六项能力的展示顺序固定，不随对象键的顺序漂移。 */
const CAPABILITY_KEYS: CapabilityKey[] = ['reasoning', 'tools', 'vision', 'audio', 'cache', 'batch']

const CAPABILITY_LABEL: Record<CapabilityKey, MessageKey> = {
  reasoning: 'models.capability.reasoning',
  tools: 'models.capability.tools',
  vision: 'models.capability.vision',
  audio: 'models.capability.audio',
  cache: 'models.capability.caching',
  batch: 'models.capability.batch',
}

/** 维度列的顺序由这里定，不按某一行的键顺序排。 */
const DIMENSIONS: PriceDimension[] = ['prompt', 'completion', 'cache_read', 'cache_write', 'reasoning']

const DIMENSION_LABEL: Record<PriceDimension, MessageKey> = {
  prompt: 'models.detail.dim.prompt',
  completion: 'models.detail.dim.completion',
  cache_read: 'models.detail.dim.cache_read',
  cache_write: 'models.detail.dim.cache_write',
  reasoning: 'models.detail.dim.reasoning',
}

/** 三态能力：`null` 是没采集过，与明确不支持的 `false` 分开显示。 */
function CapabilityState({ value }: { value: boolean | null }) {
  const { t } = useI18n()
  if (value === null) return <Pill tone="warn">{t('models.detail.capUnknown')}</Pill>
  return (
    <Pill tone={value ? 'ok' : 'neutral'}>
      {value ? t('models.detail.capYes') : t('models.detail.capNo')}
    </Pill>
  )
}

/** 端点声明同样是三态：`null` 没采集、`[]` 明确一条都不支持。 */
function Endpoints({ endpoints }: { endpoints: ModelCapabilities['endpoints'] }) {
  const { t } = useI18n()
  if (endpoints === null) return <Pill tone="warn">{t('models.detail.capUnknown')}</Pill>
  if (endpoints.length === 0) return <span className="model-muted">{t('models.detail.endpointsNone')}</span>
  return (
    <span className="badges">
      {endpoints.map((endpoint) => (
        <span key={endpoint} className="tag">
          {endpoint}
        </span>
      ))}
    </span>
  )
}

/** 目录里 `0` 表示没填，不是「上限是 0」。 */
function limitText(value: number, t: (key: MessageKey) => string, lang: 'zh' | 'en'): string {
  return value > 0 ? formatCount(value, lang) : t('models.detail.notFilled')
}

function SupplierTable({ suppliers, currency }: { suppliers: SupplierPrice[]; currency: string }) {
  const { t, lang } = useI18n()
  const columns = DIMENSIONS.filter((dimension) =>
    suppliers.some((supplier) => supplier.prices?.[dimension] !== undefined),
  )
  return (
    <div className="model-table-wrap">
      <table className="model-table">
        <caption className="sr-only">{t('models.detail.suppliers')}</caption>
        <thead>
          <tr>
            <th scope="col">{t('models.detail.merchant')}</th>
            <th scope="col" className="num">
              {t('models.detail.channelCount')}
            </th>
            {columns.map((dimension) => (
              <th key={dimension} scope="col" className="num">
                {t(DIMENSION_LABEL[dimension])}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {suppliers.map((supplier, index) => (
            // 同一商家可以有多套维度价，行之间没有稳定的业务主键；顺序由服务端给。
            <tr key={index}>
              <td>
                {supplier.merchant ?? <span className="model-muted">{t('models.detail.platform')}</span>}
              </td>
              <td className="num">{formatCount(supplier.channel_count, lang)}</td>
              {columns.map((dimension) => {
                // 输入价只看 `prices.prompt`：`price` 在没有输入价时是
                // `0.00000000`，拿它填这一列会把「没写」显示成「免费」。
                const value = supplier.prices?.[dimension]
                return (
                  <td key={dimension} className="num">
                    {value === undefined ? (
                      <span className="model-missing">—</span>
                    ) : (
                      `${moneySymbol(currency)}${formatUnitPrice(value)}`
                    )}
                  </td>
                )
              })}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

export function ModelDetail() {
  const { t, lang } = useI18n()
  const { meta } = useSite()
  const name = useParams()['*'] ?? ''
  const { data, error, loading, reload } = useAsync<ModelDetailResponse | null>(
    () => (name ? api.modelDetail(name) : Promise.resolve(null)),
    [name],
  )

  const back = (
    <Link className="model-back" to="/app/models">
      <span aria-hidden="true">←</span> {t('models.title')}
    </Link>
  )

  if (loading && !data) {
    return (
      <>
        {back}
        <div className="model-detail" aria-hidden="true">
          {[0, 1, 2].map((key) => (
            <div key={key} className="model-section">
              <Skeleton width="30%" height={13} />
              <Skeleton width="65%" height={12} />
              <Skeleton width="50%" height={12} />
            </div>
          ))}
        </div>
      </>
    )
  }

  // 名字写错与「已不在供」在服务端是同一个 404，这里也只说一句话；
  // 正文不按错误码分叉，也不把服务端那句英文原样给出去。
  if (!name || (error instanceof ApiError && error.status === 404) || (!data && !error)) {
    return (
      <>
        {back}
        <Alert tone="bad" title={t('models.detail.notFound')}>
          <Button variant="secondary" size="sm" onClick={reload} style={{ marginTop: 8 }}>
            {t('common.retry')}
          </Button>
        </Alert>
      </>
    )
  }

  if (error || !data) {
    return (
      <>
        {back}
        <Alert tone="bad" title={t('models.detail.loadFailed')}>
          <Button variant="secondary" size="sm" onClick={reload} style={{ marginTop: 8 }}>
            {t('common.retry')}
          </Button>
        </Alert>
      </>
    )
  }

  const model = data.model
  const currency = data.currency || meta.currency
  const suppliers = model.suppliers ?? []
  const options = model.reasoning_options ?? []

  return (
    <>
      {back}
      <PageHead
        title={model.display_name || model.name}
        keepTitleOnNarrow
        subtitle={
          <>
            <span className="mono">{model.name}</span>
            {[model.vendor, model.family].filter(Boolean).length > 0 && (
              <> · {[model.vendor, model.family].filter(Boolean).join(' · ')}</>
            )}
          </>
        }
      />

      <div className="model-detail">
        <section className="model-section">
          <h2>{t('models.detail.overview')}</h2>
          <dl className="model-facts">
            <div>
              <dt>{t('models.detail.name')}</dt>
              <dd className="mono">{model.name}</dd>
            </div>
            <div>
              <dt>{t('models.detail.canonicalId')}</dt>
              <dd className="mono">{model.canonical_model_id || '—'}</dd>
            </div>
            <div>
              <dt>{t('models.detail.vendor')}</dt>
              <dd>{model.vendor || '—'}</dd>
            </div>
            <div>
              <dt>{t('models.detail.family')}</dt>
              <dd>{model.family || '—'}</dd>
            </div>
          </dl>
        </section>

        <section className="model-section">
          <h2>{t('models.detail.limits')}</h2>
          <dl className="model-facts">
            <div>
              <dt>{t('models.detail.contextLimit')}</dt>
              <dd className="num">{limitText(model.context_limit, t, lang)}</dd>
            </div>
            <div>
              <dt>{t('models.detail.inputLimit')}</dt>
              <dd className="num">{limitText(model.input_limit, t, lang)}</dd>
            </div>
            <div>
              <dt>{t('models.detail.outputLimit')}</dt>
              <dd className="num">{limitText(model.output_limit, t, lang)}</dd>
            </div>
          </dl>
        </section>

        <section className="model-section">
          <h2>{t('models.detail.capabilities')}</h2>
          <ul className="model-caps">
            {CAPABILITY_KEYS.map((key) => (
              <li key={key}>
                <span>{t(CAPABILITY_LABEL[key])}</span>
                <CapabilityState value={model.capabilities[key]} />
              </li>
            ))}
          </ul>
          <div className="model-fact-inline">
            <span>{t('models.detail.endpoints')}</span>
            <Endpoints endpoints={model.capabilities.endpoints} />
          </div>
        </section>

        <section className="model-section">
          <h2>{t('models.detail.modalities')}</h2>
          <dl className="model-facts">
            <div>
              <dt>{t('models.modalityInput')}</dt>
              <dd>
                {model.modalities_input.length > 0 ? (
                  <span className="badges">
                    {model.modalities_input.map((item) => (
                      <span key={item} className="tag">
                        {item}
                      </span>
                    ))}
                  </span>
                ) : (
                  <span className="model-muted">{t('models.modalityNone')}</span>
                )}
              </dd>
            </div>
            <div>
              <dt>{t('models.modalityOutput')}</dt>
              <dd>
                {model.modalities_output.length > 0 ? (
                  <span className="badges">
                    {model.modalities_output.map((item) => (
                      <span key={item} className="tag">
                        {item}
                      </span>
                    ))}
                  </span>
                ) : (
                  <span className="model-muted">{t('models.modalityNone')}</span>
                )}
              </dd>
            </div>
          </dl>
        </section>

        <section className="model-section">
          <h2>{t('models.detail.dates')}</h2>
          <dl className="model-facts">
            <div>
              <dt>{t('models.detail.knowledgeCutoff')}</dt>
              <dd className="num">{model.knowledge_cutoff || t('models.detail.notFilled')}</dd>
            </div>
            <div>
              <dt>{t('models.detail.releaseDate')}</dt>
              <dd className="num">{model.release_date || t('models.detail.notFilled')}</dd>
            </div>
            <div>
              <dt>{t('models.detail.updatedAt')}</dt>
              <dd className="num">{formatDateTime(model.updated_at, meta.timezone)}</dd>
            </div>
          </dl>
        </section>

        <section className="model-section">
          <h2>{t('models.detail.reasoningOptions')}</h2>
          {options.length > 0 ? (
            <pre className="codeblock">
              <code>{JSON.stringify(options, null, 2)}</code>
            </pre>
          ) : (
            <p className="model-muted">{t('models.detail.reasoningOptionsNone')}</p>
          )}
        </section>

        <section className="model-section wide">
          <h2>{t('models.detail.suppliers')}</h2>
          {suppliers.length > 0 ? (
            <>
              <p className="model-section-hint">
                {t('models.detail.suppliersHint', { currency })}
              </p>
              <SupplierTable suppliers={suppliers} currency={currency} />
              {model.unpriced_channels > 0 && (
                <p className="model-section-hint">
                  {t('models.detail.unpriced', { count: model.unpriced_channels })}
                </p>
              )}
              <p className="model-section-hint">
                {t('models.referenceAt', {
                  time: formatDateTime(data.reference_at, meta.timezone),
                })}
              </p>
            </>
          ) : (
            <Empty
              icon={<IconModels size={20} />}
              title={t('models.detail.noSuppliersTitle')}
              body={t('models.detail.noSuppliersBody')}
            />
          )}
        </section>
      </div>
    </>
  )
}
