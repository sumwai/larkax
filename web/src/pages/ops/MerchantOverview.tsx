import { api } from '../../api'
import type { UnhealthyReason } from '../../api/types'
import { useAuth } from '../../auth/AuthProvider'
import { useAsync } from '../../hooks/useAsync'
import { useI18n } from '../../i18n'
import type { MessageKey } from '../../i18n/zh'
import { formatCount, todayInZone } from '../../lib/format'
import { subjectOf } from '../../ops/views'
import { useSite } from '../../site/SiteProvider'
import { Empty, MoneyText, PageHead, Skeleton } from '../../components/ui'
import { IconCheck } from '../../components/icons'
import { OpsLoadError } from './common'

/**
 * 服务端给的异常理由，一条一句人话。
 *
 * 表按联合类型写全：将来服务端新增一种理由时，这里先编译不过，不会静默
 * 落进「其他异常」。运行期仍可能收到表外的值（前后端版本不一定同时上线），
 * 所以查不到时另给一句，不把枚举原样显示。
 */
const REASONS: Record<UnhealthyReason, MessageKey> = {
  failure_rate: 'ops.merchant.reason.failureRate',
  quota_state: 'ops.merchant.reason.quotaState',
  probe_failed: 'ops.merchant.reason.probeFailed',
  consecutive_failures: 'ops.merchant.reason.consecutiveFailures',
  no_requests_24h: 'ops.merchant.reason.noRequests',
}

/**
 * 理由 → 文案。表里没有的值显示「其他异常」，原始理由放进 `title`：悬停与
 * 读屏能看到，页面正文不出现枚举原值，后端单独加上一种理由时也留下痕迹。
 */
function reasonLabel(reason: UnhealthyReason): { key: MessageKey; title?: string } {
  if (Object.prototype.hasOwnProperty.call(REASONS, reason)) return { key: REASONS[reason] }
  return { key: 'ops.merchant.reason.unknown', title: reason }
}

/**
 * 运营概览：名下渠道、在供模型、今日用量，以及需要关注的渠道。
 *
 * 「今天」是站点当地日历日，不是最近 24 小时；金额是售价口径。
 * 四个读数都直接取响应，页面不重算任何一项。
 */
export function MerchantOverview() {
  const { t, lang } = useI18n()
  const { user } = useAuth()
  const { meta, ready } = useSite()
  const { data, error, loading, reload } = useAsync(() => api.merchant.overview(), [])
  const subject = subjectOf(user?.role)
  // 时区还没取到时不写日期：那天只能按 UTC 算，与「今日」的窗口对不上。
  const today = ready ? todayInZone(meta.timezone) : null

  return (
    <div className="page">
      <PageHead
        title={t(`ops.merchant.overview.title.${subject}` as MessageKey)}
        subtitle={t(`ops.merchant.overview.subtitle.${subject}` as MessageKey)}
      />

      {loading && (
        <section
          className="kpis four"
          aria-label={t('ops.merchant.loading')}
          aria-busy="true"
        >
          {[0, 1, 2, 3].map((key) => (
            <div key={key} className="card kpi">
              <Skeleton width="50%" height={10} />
              <div style={{ height: 6 }} />
              <Skeleton width="70%" height={22} />
            </div>
          ))}
        </section>
      )}

      {error && <OpsLoadError error={error} onRetry={reload} />}

      {data && (
        <>
          <section className="kpis four" aria-label={t('ops.merchant.overview.kpis')}>
            <div className="card kpi">
              <div className="k">{t(`ops.merchant.kpi.channels.${subject}` as MessageKey)}</div>
              <div className="v">{formatCount(data.channels, lang)}</div>
            </div>
            <div className="card kpi">
              <div className="k">{t(`ops.merchant.kpi.models.${subject}` as MessageKey)}</div>
              <div className="v">{formatCount(data.models_in_supply, lang)}</div>
            </div>
            <div className="card kpi">
              <div className="k">{t(`ops.merchant.kpi.requests.${subject}` as MessageKey)}</div>
              <div className="v">{formatCount(data.today.requests, lang)}</div>
              {today && <div className="d">{t('ops.merchant.todayHint', { date: today })}</div>}
            </div>
            <div className="card kpi">
              <div className="k">{t(`ops.merchant.kpi.cost.${subject}` as MessageKey)}</div>
              <div className="v num">
                <MoneyText value={data.today.cost} currency={meta.currency} variant="spend" />
              </div>
              <div className="d">
                {t('ops.merchant.priceBasis')}
                {today ? ` · ${t('ops.merchant.todayHint', { date: today })}` : ''}
              </div>
            </div>
          </section>

          {data.unhealthy_channels.length === 0 ? (
            <Empty
              icon={<IconCheck size={20} />}
              title={t('ops.merchant.unhealthy.allNormal')}
              body={t('ops.merchant.unhealthy.allNormalBody')}
            />
          ) : (
            <section className="card" aria-label={t('ops.merchant.unhealthy.title')}>
              <div className="ops-block-head">
                <b>{t('ops.merchant.unhealthy.title')}</b>
                <span className="tiny">{t('ops.merchant.unhealthy.hint')}</span>
              </div>
              <ul className="unhealthy-list">
                {data.unhealthy_channels.map((channel) => (
                  <li key={channel.id} className="unhealthy-row">
                    <div className="keyname">
                      <b>{channel.name}</b>
                      <span className="tiny">#{channel.id}</span>
                    </div>
                    <ul className="reason-list">
                      {channel.reasons.map((reason) => {
                        const label = reasonLabel(reason)
                        return (
                          <li key={reason} title={label.title}>
                            {t(label.key)}
                          </li>
                        )
                      })}
                    </ul>
                  </li>
                ))}
              </ul>
            </section>
          )}
        </>
      )}
    </div>
  )
}
