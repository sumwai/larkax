import { Link } from 'react-router-dom'
import { api } from '../../api'
import type { ChannelHealth, MerchantUpstreamChannel } from '../../api/types'
import { useAuth } from '../../auth/AuthProvider'
import { useAsync } from '../../hooks/useAsync'
import { useIsCompact } from '../../hooks/useMediaQuery'
import { useI18n } from '../../i18n'
import type { MessageKey } from '../../i18n/zh'
import { subjectOf } from '../../ops/views'
import { formatCount } from '../../lib/format'
import { Empty, PageHead, Pill, Skeleton, StatusPill } from '../../components/ui'
import { MobileCard, MobileList } from '../../components/MobileList'
import { IconChannel } from '../../components/icons'
import { OpsLoadError } from './common'

type HealthTone = 'ok' | 'neutral' | 'warn' | 'bad'

/** 组健康取组内最差的一档，档位由服务端给；页面只负责换颜色与文案。 */
const HEALTH: Record<ChannelHealth, { tone: HealthTone; key: MessageKey }> = {
  healthy: { tone: 'ok', key: 'ops.merchant.health.healthy' },
  unknown: { tone: 'neutral', key: 'ops.merchant.health.unknown' },
  degraded: { tone: 'warn', key: 'ops.merchant.health.degraded' },
  drained: { tone: 'bad', key: 'ops.merchant.health.drained' },
}

/**
 * 0..1 的失败率换成一位小数的百分比。
 *
 * 比率本身由服务端算好，这里只换单位——不在页面上重算分子分母。
 */
function formatRate(rate: number): string {
  return `${(rate * 100).toFixed(1)}%`
}

function HealthPill({ health }: { health: ChannelHealth }) {
  const { t } = useI18n()
  // 表写全了，服务端新增一档时这里先编译不过；运行期仍可能收到表外的值。
  const meta = HEALTH[health] ?? HEALTH.unknown
  // 颜色之外必须有文字：色觉障碍的读者靠这一句分辨各档。
  return <Pill tone={meta.tone}>{t(meta.key)}</Pill>
}

function ChannelRow({ channel }: { channel: MerchantUpstreamChannel }) {
  return (
    <div className="trow upstream-channel-row" role="row">
      <div role="cell" className="keyname">
        <b>{channel.name}</b>
        <span className="tiny">#{channel.id}</span>
      </div>
      <span role="cell">
        {/* 渠道启停的文案只有一处来源：`ui.tsx` 的状态表，别在页面里再抄一份。 */}
        <StatusPill status={channel.status} />
      </span>
      <span role="cell">
        <HealthPill health={channel.health} />
      </span>
    </div>
  )
}

/**
 * 上游清单。
 *
 * 按 `(type, base_url)` 归并：同一个地址下的多条渠道是多个账号，并列显示。
 * 请求数、失败数与失败率都取服务端给的值，页面只把比率换成百分比。
 */
export function MerchantUpstreams() {
  const { t, lang } = useI18n()
  const compact = useIsCompact()
  const { user } = useAuth()
  const { data, error, loading, reload } = useAsync(() => api.merchant.listUpstreams(), [])
  const subject = subjectOf(user?.role)
  const channelsPath = subject === 'site' ? '/app/admin/channels' : '/app/merchant/channels'
  const title = t(`ops.merchant.upstreams.title.${subject}` as MessageKey)

  return (
    <div className="page">
      <PageHead
        title={title}
        subtitle={t(`ops.merchant.upstreams.subtitle.${subject}` as MessageKey)}
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
          icon={<IconChannel size={20} />}
          title={t('ops.merchant.upstreams.emptyTitle')}
          body={t('ops.merchant.upstreams.emptyBody')}
          action={
            <Link className="btn secondary" to={channelsPath}>
              {t('ops.merchant.upstreams.emptyAction')}
            </Link>
          }
        />
      )}

      {data && data.items.length > 0 && (
        <>
          {data.items.map((upstream) => (
            <article className="card upstream" key={`${upstream.type}\u0000${upstream.base_url}`}>
              <div className="upstream-head">
                <div className="keyname">
                  <b>{upstream.base_url}</b>
                  <span className="tiny">{upstream.type}</span>
                </div>
                <HealthPill health={upstream.health} />
                <div className="upstream-stats">
                  <span>
                    {t('ops.merchant.upstreams.colRequests')}{' '}
                    <b className="num">{formatCount(upstream.requests, lang)}</b>
                  </span>
                  <span>
                    {t('ops.merchant.upstreams.colFailures')}{' '}
                    <b className="num">{formatCount(upstream.failures, lang)}</b>
                  </span>
                  <span>
                    {t('ops.merchant.upstreams.colRate')}{' '}
                    <b className="num">{formatRate(upstream.failure_rate)}</b>
                  </span>
                </div>
              </div>

              {compact ? (
                <MobileList label={`${upstream.type} ${upstream.base_url}`}>
                  {upstream.channels.map((channel) => (
                    <MobileCard
                      key={channel.id}
                      title={channel.name}
                      subtitle={`#${channel.id}`}
                      status={<StatusPill status={channel.status} />}
                      fields={[
                        {
                          label: t('ops.merchant.upstreams.colHealth'),
                          value: <HealthPill health={channel.health} />,
                        },
                      ]}
                    />
                  ))}
                </MobileList>
              ) : (
                <div role="table" aria-label={`${upstream.type} ${upstream.base_url}`}>
                  <div className="thead upstream-channels-head" role="row">
                    <span role="columnheader">{t('ops.merchant.upstreams.colChannel')}</span>
                    <span role="columnheader">
                      {t('ops.merchant.upstreams.colChannelStatus')}
                    </span>
                    <span role="columnheader">{t('ops.merchant.upstreams.colHealth')}</span>
                  </div>
                  {upstream.channels.map((channel) => (
                    <ChannelRow key={channel.id} channel={channel} />
                  ))}
                </div>
              )}
            </article>
          ))}

          {data.truncated && (
            <div className="table-foot">
              {t('ops.merchant.upstreams.truncated', {
                shown: data.items.length,
                total: data.total,
              })}
            </div>
          )}
        </>
      )}
    </div>
  )
}
