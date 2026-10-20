import { useEffect, useRef, useState } from 'react'
import { Link } from 'react-router-dom'
import { api } from '../api'
import type { UsageRequest, WalletEntry, WalletEntryType } from '../api/types'
import type { MessageKey } from '../i18n/zh'
import { useI18n } from '../i18n'
import { useSite } from '../site/SiteProvider'
import {
  addDays,
  exclusiveEnd,
  formatBalance,
  formatCount,
  formatDateTime,
  formatMoney,
  moneySymbol,
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
import { SpendChart } from '../components/SpendChart'
import { IconOverview } from '../components/icons'

interface SectionState<T> {
  data: T | null
  error: boolean
  loading: boolean
}

/**
 * 一块数据的加载状态。
 *
 * 概览上的四块请求互不依赖：余额读失败不该把今日数字和图表一起带走。
 * `key` 是这块数据的参数指纹，变了就重新取；`reload` 只重取这一块。
 */
function useSection<T>(load: () => Promise<T>, key: string) {
  const loader = useRef(load)
  loader.current = load
  const [state, setState] = useState<SectionState<T>>({ data: null, error: false, loading: true })
  const [tick, setTick] = useState(0)

  useEffect(() => {
    let alive = true
    setState((previous) => ({ ...previous, loading: true, error: false }))
    loader
      .current()
      .then((data) => {
        if (alive) setState({ data, error: false, loading: false })
      })
      .catch(() => {
        if (alive) setState((previous) => ({ data: previous.data, error: true, loading: false }))
      })
    return () => {
      alive = false
    }
  }, [key, tick])

  return { ...state, reload: () => setTick((value) => value + 1) }
}

function SectionError({ title, onRetry }: { title: string; onRetry: () => void }) {
  const { t } = useI18n()
  return (
    <Alert tone="bad" title={title}>
      <Button variant="secondary" size="sm" onClick={onRetry} style={{ marginTop: 8 }}>
        {t('common.retry')}
      </Button>
    </Alert>
  )
}

function requestTokens(row: UsageRequest): string {
  return formatCount(row.tokens.input + row.tokens.output)
}

const ENTRY_LABEL: Record<WalletEntryType, MessageKey> = {
  topup: 'wallet.type.topup',
  consume: 'wallet.type.consume',
  refund: 'wallet.type.refund',
  adjustment: 'wallet.type.adjustment',
}

/** 正数补一个 `+`，让「入账 / 扣费」在视觉上先分开。 */
function entryAmount(entry: WalletEntry): string {
  const value = formatMoney(entry.amount)
  return entry.amount.startsWith('-') || value === '—' ? value : `+${value}`
}

export function Overview() {
  const { t, lang } = useI18n()
  const { meta } = useSite()
  const today = todayInZone(meta.timezone)
  const weekFrom = addDays(today, -6)
  const end = exclusiveEnd(today)

  const wallet = useSection(() => api.wallet(), 'wallet')
  const summary = useSection(() => api.usageSummary({ from: today, to: end }), `${today}:${end}`)
  const series = useSection(
    () => api.usageSeries({ from: weekFrom, to: end, interval: 'day' }),
    `${weekFrom}:${end}`,
  )
  const requests = useSection(
    () => api.usageRequests({ from: weekFrom, to: end, limit: 8 }),
    `${weekFrom}:${end}`,
  )
  const entries = useSection(() => api.walletEntries({ limit: 6 }), 'entries')

  const symbol = moneySymbol(meta.currency)

  return (
    <>
      <PageHead title={t('overview.title')} subtitle={t('overview.subtitle')} />

      {wallet.loading && !wallet.data && (
        <div className="card panel">
          <Skeleton width="30%" height={10} />
          <Skeleton width="55%" height={36} />
          <Skeleton width="40%" height={12} />
        </div>
      )}

      {wallet.error && <SectionError title={t('overview.walletFailed')} onRetry={wallet.reload} />}

      {wallet.data && (
        <section className="balance" aria-label={t('overview.balance')}>
          <div className="label">{t('overview.balance')}</div>
          <div className="amount num">
            <MoneyText value={wallet.data.balance} currency={meta.currency} variant="balance" />
          </div>
          <div className="meta">
            <span>
              {t('overview.frozen')} {symbol}
              <span className="num">{formatBalance(wallet.data.frozen)}</span>
            </span>
            <span>
              {t('overview.updatedAt')} {formatDateTime(wallet.data.updated_at, meta.timezone)}
            </span>
          </div>
        </section>
      )}

      {summary.loading && !summary.data && (
        <div className="kpis">
          {[0, 1, 2].map((key) => (
            <div key={key} className="card kpi">
              <Skeleton width="50%" height={10} />
              <div style={{ height: 6 }} />
              <Skeleton width="70%" height={22} />
            </div>
          ))}
        </div>
      )}

      {summary.error && (
        <SectionError title={t('overview.summaryFailed')} onRetry={summary.reload} />
      )}

      {summary.data && (
        <section className="kpis" aria-label={t('overview.title')}>
          <div className="card kpi">
            <div className="k">{t('overview.todayRequests')}</div>
            <div className="v">{formatCount(summary.data.totals.requests, lang)}</div>
            <div className="d">{today}</div>
          </div>
          <div className="card kpi">
            <div className="k">{t('overview.todayTokens')}</div>
            <div className="v">
              {formatCount(summary.data.totals.tokens.input + summary.data.totals.tokens.output, lang)}
            </div>
            <div className="d">
              {t('usage.promptTokens')} {formatCount(summary.data.totals.tokens.input, lang)} ·{' '}
              {t('usage.completionTokens')} {formatCount(summary.data.totals.tokens.output, lang)}
            </div>
          </div>
          <div className="card kpi">
            <div className="k">{t('overview.todayCost')}</div>
            <div className="v num">
              <MoneyText value={summary.data.totals.cost} currency={meta.currency} variant="spend" />
            </div>
            <div className="d">{today}</div>
          </div>
        </section>
      )}

      <section className="card panel">
        <div className="head">
          <div>
            <h2>{t('overview.last7Days')}</h2>
            {series.data && (
              <p>
                {t('overview.last7DaysTotal')}{' '}
                <MoneyText
                  value={series.data.points
                    .reduce((sum, point) => sum + Number.parseFloat(point.cost), 0)
                    .toFixed(8)}
                  currency={meta.currency}
                  variant="spend"
                />
              </p>
            )}
          </div>
          <Link className="btn ghost sm" to="/app/usage">
            {t('overview.viewUsage')}
          </Link>
        </div>
        {series.error ? (
          <SectionError title={t('overview.seriesFailed')} onRetry={series.reload} />
        ) : series.loading && !series.data ? (
          <Skeleton height={120} />
        ) : series.data ? (
          <SpendChart
            points={series.data.points}
            currency={meta.currency}
            timezone={series.data.tz || meta.timezone}
          />
        ) : null}
      </section>

      <section className="card panel">
        <div className="head">
          <div>
            <h2>{t('overview.recentRequests')}</h2>
            <p>{t('overview.subtitle')}</p>
          </div>
          <Link className="btn ghost sm" to="/app/usage">
            {t('overview.viewUsage')}
          </Link>
        </div>

        {requests.error ? (
          <SectionError title={t('overview.requestsFailed')} onRetry={requests.reload} />
        ) : requests.loading && !requests.data ? (
          <Skeleton height={100} />
        ) : requests.data && requests.data.items.length === 0 ? (
          <Empty icon={<IconOverview size={20} />} title={t('overview.noRequests')} />
        ) : requests.data ? (
          <>
            <div className="table table-desktop">
              <div className="thead requests-head" style={{ display: 'grid' }}>
                <span>{t('overview.time')}</span>
                <span>{t('overview.model')}</span>
                <span>{t('overview.key')}</span>
                <span>{t('overview.status')}</span>
                <span>{t('overview.tokens')}</span>
                <span className="amount">{t('overview.cost')}</span>
              </div>
              {requests.data.items.map((row) => (
                <div className="trow requests-row" key={row.request_id}>
                  <span className="muted-cell num">
                    {formatDateTime(row.started_at, meta.timezone)}
                  </span>
                  <span className="cell-ellipsis mono">{row.model}</span>
                  <span className="cell-ellipsis muted-cell">{row.key?.name || '—'}</span>
                  <span>
                    <StatusPill status={row.status} />
                  </span>
                  <span className="muted-cell num">{requestTokens(row)}</span>
                  <span className="amount">
                    <MoneyText value={row.cost} currency={meta.currency} variant="spend" />
                  </span>
                </div>
              ))}
            </div>

            <div className="cards-mobile">
              {requests.data.items.map((row) => (
                <div className="mcard" key={row.request_id}>
                  <div className="nm mono">{row.model}</div>
                  <span className="st">
                    <StatusPill status={row.status} />
                  </span>
                  <div className="kv">
                    {formatDateTime(row.started_at, meta.timezone)} · {row.key?.name || '—'} ·{' '}
                    {requestTokens(row)} {t('overview.tokens')}
                  </div>
                  <span className="kv num" style={{ justifySelf: 'end' }}>
                    <MoneyText value={row.cost} currency={meta.currency} variant="spend" />
                  </span>
                </div>
              ))}
            </div>
          </>
        ) : null}
      </section>

      <section className="card panel">
        <div className="head">
          <div>
            <h2>{t('overview.entries')}</h2>
            <p>{t('overview.entriesHint')}</p>
          </div>
        </div>

        {entries.error ? (
          <SectionError title={t('overview.entriesFailed')} onRetry={entries.reload} />
        ) : entries.loading && !entries.data ? (
          <Skeleton height={100} />
        ) : entries.data && entries.data.items.length === 0 ? (
          <Empty title={t('overview.noEntries')} />
        ) : entries.data ? (
          <>
            <div className="table table-desktop">
              <div className="thead entries-head" style={{ display: 'grid' }}>
                <span>{t('overview.time')}</span>
                <span>{t('overview.entryType')}</span>
                <span>{t('overview.entryMemo')}</span>
                <span className="amount">{t('overview.amount')}</span>
              </div>
              {entries.data.items.map((entry) => (
                <div className="trow entries-row" key={entry.id}>
                  <span className="muted-cell num">
                    {formatDateTime(entry.created_at, meta.timezone)}
                  </span>
                  <span className="muted-cell">{t(ENTRY_LABEL[entry.type])}</span>
                  <span className="cell-ellipsis muted-cell">{entry.memo || '—'}</span>
                  <span className="amount num">
                    {entryAmount(entry)}{' '}
                    <span className="tiny">{entry.asset || meta.currency}</span>
                  </span>
                </div>
              ))}
            </div>

            <div className="cards-mobile">
              {entries.data.items.map((entry) => (
                <div className="mcard entries-mcard" key={entry.id}>
                  <div className="nm">{t(ENTRY_LABEL[entry.type])}</div>
                  <span className="kv num" style={{ justifySelf: 'end' }}>
                    {entryAmount(entry)}{' '}
                    <span className="tiny">{entry.asset || meta.currency}</span>
                  </span>
                  <div className="kv">
                    {formatDateTime(entry.created_at, meta.timezone)} · {entry.memo || '—'}
                  </div>
                </div>
              ))}
            </div>
          </>
        ) : null}
      </section>
    </>
  )
}
