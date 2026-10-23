import type { ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import { api } from '../api'
import type { RequestStatus, UsageRequest } from '../api/types'
import {
  addDays,
  exclusiveEnd,
  formatCompact,
  formatCount,
  formatDate,
  formatDateTime,
  formatRelative,
  formatSpendTick,
  moneySymbol,
  todayInZone,
} from '../lib/format'
import { useI18n } from '../i18n'
import type { MessageKey } from '../i18n/zh'
import { cn } from '../components/ui/lib'
import { modelIcon } from '../lib/modelIcon'
import { useSite } from '../site/SiteProvider'
import { useDocumentTitle } from '../components/PageTitle'
import {
  Alert,
  Button,
  Card,
  CardBody,
  CardHeader,
  CardTitle,
  LineChart,
  Money,
  PageHead,
  Skeleton,
} from '../components/ui'

/** 状态到色调：结算过的是成功，处理中待定，失败与退款各有自己的颜色。 */
const STATUS_TONE: Record<RequestStatus, 'neutral' | 'success' | 'warning' | 'danger' | 'info'> = {
  pending: 'warning',
  settled: 'success',
  released: 'info',
  refunded: 'info',
  failed: 'danger',
}

function Kpi({ label, value, hint }: { label: string; value: ReactNode; hint?: ReactNode }) {
  return (
    <Card>
      <CardBody className="flex flex-col gap-1">
        <span className="text-caps text-muted-foreground">{label}</span>
        <span className="text-xl font-semibold tracking-tight">{value}</span>
        {hint ? <span className="text-xs text-faint">{hint}</span> : null}
      </CardBody>
    </Card>
  )
}

/** 厂商图标：尺寸固定 16，跟随当前前景色。 */
function ModelGlyph({ model }: { model: string }) {
  const Icon = modelIcon(model)
  if (!Icon) return null
  return <Icon size={16} className="mt-0.5 shrink-0 text-muted-foreground" />
}

/** 状态：点 + 文字（成功 / 失败 / 处理中…），颜色取语义 token。 */
function Status({ status }: { status: RequestStatus }) {
  const { t } = useI18n()
  const tone = STATUS_TONE[status]
  return (
    <span className="inline-flex shrink-0 items-center gap-1.5 text-xs text-muted-foreground">
      <span
        aria-hidden="true"
        className={cn(
          'size-1.5 rounded-full',
          tone === 'success' && 'bg-success',
          tone === 'warning' && 'bg-warning',
          tone === 'danger' && 'bg-danger',
          tone === 'info' && 'bg-info',
          tone === 'neutral' && 'bg-faint',
        )}
      />
      {t(`status.${status}` as MessageKey)}
    </span>
  )
}

/**
 * 单行请求：左侧图标 + 模型 + 状态点，右侧 token 合计、花费与受理时刻。
 *
 * 三个数字靠右排成一条线，读的时候不必在行内来回找。
 */
function RequestRow({ request, lang }: { request: UsageRequest; lang: 'zh' | 'en' }) {
  const tokens = request.tokens
  const total = tokens.input + tokens.output + tokens.reasoning
  return (
    <div className="flex items-center gap-2 px-4 py-2.5">
      <ModelGlyph model={request.model} />
      <span className="min-w-0 flex-1 truncate text-sm font-medium">{request.model}</span>
      <span className="num w-16 shrink-0 text-right text-xs text-muted-foreground tabular-nums">
        {formatCompact(total)}
      </span>
      <span className="w-20 shrink-0 text-right">
        <Money
          value={request.cost}
          currency={request.currency}
          variant="spend"
          className="text-sm"
        />
      </span>
      <span className="flex w-16 shrink-0 justify-end">
        <Status status={request.status} />
      </span>
      <span
        className="w-24 shrink-0 text-right text-xs text-faint tabular-nums"
        title={formatDateTime(request.started_at)}
      >
        {formatRelative(request.started_at, lang)}
      </span>
    </div>
  )
}

/**
 * 用户首页：余额、今日用量、近 7 天花费与最近请求。
 *
 * 四块数据各自独立取：任何一块失败只影响那一块，其余照常显示。
 */
export function Home() {
  const { t, lang } = useI18n()
  const { meta } = useSite()

  useDocumentTitle(`${t('overview.title')} · ${meta.site_name}`)

  const wallet = useQuery({ queryKey: ['wallet'], queryFn: () => api.wallet() })

  // 时间窗按站点时区的「今天」算，与服务端分桶用的是同一个日历。
  const todayDate = todayInZone(meta.timezone)
  const today = useQuery({
    queryKey: ['usage-summary', todayDate],
    queryFn: () => api.usageSummary({ from: todayDate, to: exclusiveEnd(todayDate) }),
  })
  const series = useQuery({
    queryKey: ['usage-series', todayDate],
    queryFn: () =>
      api.usageSeries({
        interval: 'day',
        from: addDays(todayDate, -6),
        to: exclusiveEnd(todayDate),
      }),
  })
  const requests = useQuery({
    queryKey: ['usage-requests', 5],
    queryFn: () => api.usageRequests({ limit: 5 }),
  })

  const currency = wallet.data?.asset ?? today.data?.currency ?? meta.currency
  // 冻结金额是 8 位小数字符串，「0.00000000」也要判成没有冻结。
  const frozen = wallet.data?.frozen
  const hasFrozen = frozen !== undefined && /[1-9]/.test(frozen)
  // 桶起点是 RFC3339，按站点时区落回日期；直接切字符串会落到 UTC 那一天。
  const seriesTz = series.data?.tz ?? meta.timezone
  const points = (series.data?.points ?? []).map((point) => ({
    label: formatDate(point.start, seriesTz).slice(5),
    value: Number.parseFloat(point.cost),
  }))

  return (
    <>
      <PageHead title={t('overview.title')} description={t('overview.subtitle')} />

      <section className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <Kpi
          label={t('overview.balance')}
          value={
            wallet.isPending ? (
              <Skeleton className="h-7 w-24" />
            ) : (
              <Money value={wallet.data?.balance ?? null} currency={currency} variant="balance" />
            )
          }
          hint={
            hasFrozen ? (
              <>
                {t('overview.frozen')}{' '}
                <Money value={frozen} currency={currency} variant="balance" />
              </>
            ) : undefined
          }
        />
        <Kpi
          label={t('overview.todayCost')}
          value={
            today.isPending ? (
              <Skeleton className="h-7 w-20" />
            ) : (
              <Money value={today.data?.totals.cost ?? null} currency={currency} variant="spend" />
            )
          }
        />
        <Kpi
          label={t('overview.todayRequests')}
          value={
            today.isPending ? (
              <Skeleton className="h-7 w-16" />
            ) : (
              formatCount(today.data?.totals.requests, lang)
            )
          }
        />
        <Kpi
          label={t('overview.todayTokens')}
          value={
            today.isPending ? (
              <Skeleton className="h-7 w-16" />
            ) : (
              /* 推理单独一列存在：上游常把输出记在 reasoning 上，只加 input+output 会把用量算少。 */
              formatCompact(
                (today.data?.totals.tokens.input ?? 0) +
                  (today.data?.totals.tokens.output ?? 0) +
                  (today.data?.totals.tokens.reasoning ?? 0),
              )
            )
          }
        />
      </section>

      <Card>
        <CardHeader>
          <CardTitle>{t('overview.last7Days')}</CardTitle>
        </CardHeader>
        <CardBody>
          {series.isPending ? (
            <Skeleton className="h-40 w-full" />
          ) : series.isError ? (
            <Alert tone="danger" title={t('overview.seriesFailed')}>
              <Button variant="secondary" size="sm" onClick={() => void series.refetch()}>
                {t('common.retry')}
              </Button>
            </Alert>
          ) : (
            <LineChart
              series={[{ name: t('usage.cost'), color: 1, points }]}
              formatValue={(value) => `${moneySymbol(currency)}${formatSpendTick(value)}`}
            />
          )}
        </CardBody>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>{t('overview.recentRequests')}</CardTitle>
        </CardHeader>
        {requests.isPending ? (
          <CardBody className="flex flex-col gap-2">
            <Skeleton className="h-8 w-full" />
            <Skeleton className="h-8 w-full" />
          </CardBody>
        ) : requests.isError ? (
          <CardBody>
            <Alert tone="danger" title={t('overview.requestsFailed')}>
              <Button variant="secondary" size="sm" onClick={() => void requests.refetch()}>
                {t('common.retry')}
              </Button>
            </Alert>
          </CardBody>
        ) : (requests.data?.items.length ?? 0) === 0 ? (
          <CardBody>
            <p className="text-sm text-muted-foreground">{t('overview.noRequests')}</p>
          </CardBody>
        ) : (
          <>
            {/* 表头解释右侧三个数字是什么。 */}
            <div className="flex items-center gap-2 border-b border-border px-4 py-2 text-2xs font-semibold text-muted-foreground">
              <span className="min-w-0 flex-1">{t('overview.colModel')}</span>
              <span className="w-16 shrink-0 text-right">{t('overview.colTokens')}</span>
              <span className="w-20 shrink-0 text-right">{t('overview.colCost')}</span>
              <span className="w-16 shrink-0 text-right">{t('overview.colStatus')}</span>
              <span className="w-24 shrink-0 text-right">{t('overview.colTime')}</span>
            </div>
            <div className="divide-y divide-border">
              {requests.data?.items.map((request: UsageRequest) => (
                <RequestRow key={request.id} request={request} lang={lang} />
              ))}
            </div>
          </>
        )}
      </Card>
    </>
  )
}
