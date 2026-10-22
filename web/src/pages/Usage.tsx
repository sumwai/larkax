import { useEffect, useState } from 'react'
import { api } from '../api'
import type { RequestStatus, UsageGroup, UsageRequest } from '../api/types'
import { useI18n } from '../i18n'
import type { MessageKey } from '../i18n/zh'
import { useSite } from '../site/SiteProvider'
import { useAsync } from '../hooks/useAsync'
import {
  addDays,
  exclusiveEnd,
  formatCount,
  formatDateTime,
  todayInZone,
} from '../lib/format'
import {
  Alert,
  Button,
  Empty,
  MoneyText,
  PageHead,
  Skeleton,
  StatusPill,
} from '../components/ui'
import { DateRangePicker } from '../components/DateRangePicker'
import { IconUsage } from '../components/icons'

type StatusFilter = '' | RequestStatus

function tokens(row: UsageRequest): string {
  return formatCount(row.tokens.input + row.tokens.output)
}

function groupName(row: UsageGroup): string {
  if ('model' in row) return row.model
  if (row.key_name) return row.key_name
  return row.key_id === null ? '—' : `#${row.key_id}`
}

function groupKey(row: UsageGroup): string {
  return 'model' in row ? row.model : `key-${row.key_id ?? 'none'}`
}

function groupTokens(row: UsageGroup): number {
  return row.tokens.input + row.tokens.output
}

export function Usage() {
  const { t, lang } = useI18n()
  const { meta, ready } = useSite()
  const today = todayInZone(meta.timezone)
  const [from, setFrom] = useState(() => addDays(today, -6))
  const [to, setTo] = useState(today)
  const [groupBy, setGroupBy] = useState<'model' | 'key'>('model')
  const [status, setStatus] = useState<StatusFilter>('')
  const [page, setPage] = useState<{ items: UsageRequest[]; next: string | null } | null>(null)
  const [loadingMore, setLoadingMore] = useState(false)
  const [moreError, setMoreError] = useState(false)

  // 首屏渲染时 meta 还是 fallback 的 UTC，站点时区一到位就要把默认时间窗
  // 改成站点时区的「近 7 天」；否则东八区凌晨会整整差一天。
  useEffect(() => {
    if (!ready) return
    setFrom(addDays(todayInZone(meta.timezone), -6))
    setTo(todayInZone(meta.timezone))
  }, [ready, meta.timezone])

  const end = exclusiveEnd(to)

  const { data, error, loading, reload } = useAsync(async () => {
    const [summary, breakdown, log] = await Promise.all([
      api.usageSummary({ from, to: end }),
      api.usageBreakdown({ from, to: end, group_by: groupBy, limit: 10 }),
      api.usageRequests({
        from,
        to: end,
        status: status === '' ? undefined : status,
        limit: 20,
      }),
    ])
    return { summary, breakdown, log }
  }, [from, end, groupBy, status])

  useEffect(() => {
    setPage(null)
    setMoreError(false)
  }, [data])

  const logItems = page ? [...(data?.log.items ?? []), ...page.items] : (data?.log.items ?? [])
  const nextCursor = page ? page.next : (data?.log.next_cursor ?? null)

  const loadMore = async () => {
    if (!nextCursor) return
    setLoadingMore(true)
    setMoreError(false)
    try {
      const result = await api.usageRequests({
        from,
        to: end,
        status: status === '' ? undefined : status,
        limit: 20,
        cursor: nextCursor,
      })
      setPage((previous) => ({
        items: [...(previous?.items ?? []), ...result.items],
        next: result.next_cursor,
      }))
    } catch {
      setMoreError(true)
    } finally {
      setLoadingMore(false)
    }
  }

  const paths = data?.summary.billing_paths ?? []
  const costTotal = paths.reduce((sum, path) => sum + Number.parseFloat(path.cost), 0)
  const pathLabel = (path: string) => {
    const known: Record<string, MessageKey> = {
      measured: 'status.measured',
      estimated: 'status.estimated',
      fallback: 'status.fallback',
    }
    return known[path] ? t(known[path]) : path
  }
  const pathTone = (path: string) => {
    switch (path) {
      case 'measured':
        return 'var(--accent)'
      case 'estimated':
        return 'var(--warning)'
      default:
        return 'var(--surface-3)'
    }
  }

  return (
    <>
      <PageHead
        title={t('usage.title')}
        subtitle={t('usage.subtitle')}
        actions={<DateRangePicker from={from} to={to} onChange={(nextFrom, nextTo) => {
          setFrom(nextFrom)
          setTo(nextTo)
        }} />}
      />

      {error && (
        <Alert tone="bad" title={t('usage.loadFailed')}>
          <Button variant="secondary" size="sm" onClick={reload} style={{ marginTop: 8 }}>
            {t('common.retry')}
          </Button>
        </Alert>
      )}

      {loading && !data && (
        <>
          <div className="kpis">
            {[0, 1, 2].map((key) => (
              <div key={key} className="card kpi">
                <Skeleton width="45%" height={10} />
                <div style={{ height: 6 }} />
                <Skeleton width="70%" height={22} />
              </div>
            ))}
          </div>
          <div className="card panel">
            <Skeleton height={16} />
            <Skeleton height={16} />
            <Skeleton height={16} />
          </div>
        </>
      )}

      {data && (
        <>
          <section className="kpis" aria-label={t('usage.title')}>
            <div className="card kpi">
              <div className="k">{t('usage.summaryRequests')}</div>
              <div className="v">{formatCount(data.summary.totals.requests, lang)}</div>
              <div className="d">
                {from} – {to}
              </div>
            </div>
            <div className="card kpi">
              <div className="k">{t('usage.summaryTokens')}</div>
              <div className="v">
                {formatCount(
                  data.summary.totals.tokens.input + data.summary.totals.tokens.output,
                  lang,
                )}
              </div>
              <div className="d">
                {t('usage.promptTokens')} {formatCount(data.summary.totals.tokens.input, lang)} ·{' '}
                {t('usage.completionTokens')}{' '}
                {formatCount(data.summary.totals.tokens.output, lang)}
              </div>
            </div>
            <div className="card kpi">
              <div className="k">{t('usage.summaryCost')}</div>
              <div className="v num">
                <MoneyText value={data.summary.totals.cost} currency={meta.currency} variant="spend" />
              </div>
              <div className="d">
                {t('usage.priceSource')} · {t('usage.intervalDay')}
              </div>
            </div>
          </section>

          <div className="grid-2">
            <section className="card panel">
              <div className="head">
                <div>
                  <h2>{t('usage.priceSource')}</h2>
                  <p>{t('models.priceNote')}</p>
                </div>
              </div>
              <div className="share-bar" aria-hidden="true">
                {paths.map((path) => (
                  <span
                    key={path.path}
                    className="share-seg"
                    style={{
                      width: `${costTotal > 0 ? (Number.parseFloat(path.cost) / costTotal) * 100 : 0}%`,
                      background: pathTone(path.path),
                    }}
                  />
                ))}
              </div>
              <div className="share-list">
                {paths.map((path) => (
                  <div key={path.path} className="share-row">
                    <span className="row" style={{ gap: 8 }}>
                      <span
                        className="share-dot"
                        style={{ background: pathTone(path.path) }}
                        aria-hidden="true"
                      />
                      {pathLabel(path.path)}
                    </span>
                    <span className="num muted-cell">
                      {formatCount(path.requests, lang)} ·{' '}
                      <MoneyText value={path.cost} currency={meta.currency} variant="spend" />
                    </span>
                  </div>
                ))}
              </div>
            </section>

            <section className="card panel">
              <div className="head">
                <div>
                  <h2>{t('usage.tokens')}</h2>
                  <p>{t('usage.subtitle')}</p>
                </div>
              </div>
              <div className="sheet-rows">
                <div>
                  <span>{t('usage.promptTokens')}</span>
                  <b className="num">{formatCount(data.summary.totals.tokens.input, lang)}</b>
                </div>
                <div>
                  <span>{t('usage.completionTokens')}</span>
                  <b className="num">{formatCount(data.summary.totals.tokens.output, lang)}</b>
                </div>
                <div>
                  <span>{t('usage.cacheReadTokens')}</span>
                  <b className="num">{formatCount(data.summary.totals.tokens.cache_read, lang)}</b>
                </div>
                <div>
                  <span>{t('usage.cacheWriteTokens')}</span>
                  <b className="num">{formatCount(data.summary.totals.tokens.cache_write, lang)}</b>
                </div>
                <div>
                  <span>{t('usage.totalTokens')}</span>
                  <b className="num">
                    {formatCount(
                      data.summary.totals.tokens.input + data.summary.totals.tokens.output,
                      lang,
                    )}
                  </b>
                </div>
              </div>
            </section>
          </div>

          <section className="card panel">
            <div className="head">
              <div>
                <h2>{t('usage.breakdown')}</h2>
                <p>{t('usage.subtitle')}</p>
              </div>
              <div className="seg" role="group" aria-label={t('usage.breakdown')}>
                <button aria-pressed={groupBy === 'model'} onClick={() => setGroupBy('model')}>
                  {t('usage.byModel')}
                </button>
                <button aria-pressed={groupBy === 'key'} onClick={() => setGroupBy('key')}>
                  {t('usage.byKey')}
                </button>
              </div>
            </div>

            {data.breakdown.items.length === 0 ? (
              <Empty title={t('usage.noBreakdown')} />
            ) : (
              <>
                <div className="table table-desktop">
                  <div className="thead breakdown-head">
                    <span>{groupBy === 'model' ? t('overview.model') : t('overview.key')}</span>
                    <span className="amount">{t('usage.requests')}</span>
                    <span className="amount">{t('usage.tokens')}</span>
                    <span className="amount">{t('usage.cost')}</span>
                  </div>
                  {data.breakdown.items.map((row) => (
                    <div className="trow breakdown-row" key={groupKey(row)}>
                      <span className="cell-ellipsis">
                        <b style={{ fontWeight: 550 }}>{groupName(row)}</b>
                      </span>
                      <span className="amount muted-cell">{formatCount(row.requests, lang)}</span>
                      <span className="amount muted-cell">
                        {formatCount(groupTokens(row), lang)}
                      </span>
                      <span className="amount">
                        <MoneyText value={row.cost} currency={meta.currency} variant="spend" />
                      </span>
                    </div>
                  ))}
                </div>
                <div className="cards-mobile">
                  {data.breakdown.items.map((row) => (
                    <div className="mcard" key={groupKey(row)}>
                      <div className="nm">{groupName(row)}</div>
                      <span className="kv">
                        {t('usage.requests')} {formatCount(row.requests, lang)} ·{' '}
                        {t('usage.tokens')} {formatCount(groupTokens(row), lang)}
                      </span>
                      <span className="kv num" style={{ justifySelf: 'end' }}>
                        <MoneyText value={row.cost} currency={meta.currency} variant="spend" />
                      </span>
                    </div>
                  ))}
                </div>
              </>
            )}
          </section>

          <section className="card panel">
            <div className="head">
              <div>
                <h2>{t('usage.log')}</h2>
                <p>
                  {from} – {to}
                </p>
              </div>
              <div className="filters" role="group" aria-label={t('usage.log')}>
                {(
                  [
                    ['', 'common.all'],
                    ['settled', 'status.settled'],
                    ['pending', 'status.pending'],
                    ['failed', 'status.failed'],
                    ['refunded', 'status.refunded'],
                  ] as Array<[StatusFilter, MessageKey]>
                ).map(([value, label]) => (
                  <button
                    key={value || 'all'}
                    className="chipbtn"
                    aria-pressed={status === value}
                    onClick={() => setStatus(value)}
                  >
                    {t(label)}
                  </button>
                ))}
              </div>
            </div>

            {logItems.length === 0 ? (
              <Empty icon={<IconUsage size={20} />} title={t('usage.noLog')} />
            ) : (
              <>
                <div className="table table-desktop">
                  <div className="thead requests-head">
                    <span>{t('overview.time')}</span>
                    <span>{t('overview.model')}</span>
                    <span>{t('overview.key')}</span>
                    <span>{t('overview.status')}</span>
                    <span className="amount">{t('usage.tokens')}</span>
                    <span className="amount">{t('usage.cost')}</span>
                  </div>
                  {logItems.map((row) => (
                    <div className="trow requests-row" key={row.request_id}>
                      <span className="muted-cell num">
                        {formatDateTime(row.started_at, meta.timezone)}
                      </span>
                      <span className="cell-ellipsis mono">{row.model}</span>
                      <span className="cell-ellipsis muted-cell">{row.key?.name || '—'}</span>
                      <span>
                        <StatusPill status={row.status} />
                      </span>
                      <span className="amount muted-cell">{tokens(row)}</span>
                      <span className="amount">
                        <MoneyText value={row.cost} currency={meta.currency} variant="spend" />
                      </span>
                    </div>
                  ))}
                </div>
                <div className="cards-mobile">
                  {logItems.map((row) => (
                    <div className="mcard" key={row.request_id}>
                      <div className="nm mono">{row.model}</div>
                      <span className="st">
                        <StatusPill status={row.status} />
                      </span>
                      <div className="kv">
                        {formatDateTime(row.started_at, meta.timezone)} · {row.key?.name || '—'} ·{' '}
                        {tokens(row)} {t('usage.tokens')}
                      </div>
                      <span className="kv num" style={{ justifySelf: 'end' }}>
                        <MoneyText value={row.cost} currency={meta.currency} variant="spend" />
                      </span>
                    </div>
                  ))}
                </div>

                {nextCursor && (
                  <div className="form-actions">
                    {moreError && <span className="hint error">{t('usage.loadFailed')}</span>}
                    <Button variant="secondary" size="sm" loading={loadingMore} onClick={loadMore}>
                      {t('usage.loadMore')}
                    </Button>
                  </div>
                )}
              </>
            )}
          </section>
        </>
      )}
    </>
  )
}
