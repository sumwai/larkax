import { Link } from 'react-router-dom'
import { api } from '../../api'
import { useAuth } from '../../auth/AuthProvider'
import { useAsync } from '../../hooks/useAsync'
import { useI18n } from '../../i18n'
import type { MessageKey } from '../../i18n/zh'
import { subjectOf } from '../../ops/views'
import { formatCount } from '../../lib/format'
import { Empty, PageHead, Pill, Skeleton } from '../../components/ui'
import { IconModels } from '../../components/icons'
import { OpsLoadError } from './common'

/**
 * 在供模型清单。
 *
 * `priced` 只说「有没有一条生效中的商家规则命中它」，不代表实际按哪条计价，
 * 也不显示金额；管理员视野下问的是「全平台有没有商家配过」，规则的主人
 * 未必在供这个模型。所以两种主语都没有「我的价」这种说法。
 */
export function MerchantModels() {
  const { t, lang } = useI18n()
  const { user } = useAuth()
  const { data, error, loading, reload } = useAsync(() => api.merchant.listSuppliedModels(), [])
  const subject = subjectOf(user?.role)
  const channelsPath = subject === 'site' ? '/app/admin/channels' : '/app/merchant/channels'
  const title = t(`ops.merchant.models.title.${subject}` as MessageKey)

  return (
    <div className="page">
      <PageHead
        title={title}
        subtitle={`${t(`ops.merchant.models.subtitle.${subject}` as MessageKey)} ${t(
          `ops.merchant.models.pricedHint.${subject}` as MessageKey,
        )}`}
      />

      {loading && (
        <section className="card" aria-label={t('ops.merchant.loading')} aria-busy="true">
          <Skeleton width="40%" height={12} />
          <div style={{ height: 10 }} />
          <Skeleton width="100%" height={14} />
          <div style={{ height: 8 }} />
          <Skeleton width="100%" height={14} />
        </section>
      )}

      {error && <OpsLoadError error={error} onRetry={reload} />}

      {data && data.items.length === 0 && (
        <Empty
          icon={<IconModels size={20} />}
          title={t('ops.merchant.models.emptyTitle')}
          body={t('ops.merchant.models.emptyBody')}
          action={
            <Link className="btn secondary" to={channelsPath}>
              {t('ops.merchant.models.emptyAction')}
            </Link>
          }
        />
      )}

      {data && data.items.length > 0 && (
        <section className="card table ops-table">
          <div role="table" aria-label={title}>
            <div className="thead supply-head" role="row">
              <span role="columnheader">{t('ops.merchant.models.colModel')}</span>
              <span role="columnheader" className="num">
                {t('ops.merchant.models.colChannels')}
              </span>
              <span role="columnheader">{t('ops.merchant.models.colPriced')}</span>
            </div>
            {data.items.map((row) => (
              <div className="trow supply-row" role="row" key={row.model}>
                <div role="cell" className="keyname">
                  <b>{row.model}</b>
                  {row.display_name !== row.model && (
                    <span className="tiny">{row.display_name || t('common.none')}</span>
                  )}
                </div>
                <span role="cell" className="num">
                  {formatCount(row.channels, lang)}
                </span>
                <span role="cell">
                  <Pill tone={row.priced ? 'ok' : 'neutral'}>
                    {t(row.priced ? 'ops.merchant.models.priced' : 'ops.merchant.models.unpriced')}
                  </Pill>
                </span>
              </div>
            ))}
          </div>
          {data.truncated && (
            <div className="table-foot">
              {t('ops.merchant.models.truncated', {
                shown: data.items.length,
                total: data.total,
              })}
            </div>
          )}
        </section>
      )}
    </div>
  )
}
