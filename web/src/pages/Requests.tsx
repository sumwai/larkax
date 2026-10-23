import { useEffect, useRef, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { ChevronLeft, ChevronRight } from 'lucide-react'
import { api } from '../api'
import type { RequestStatus, UsageRequest } from '../api/types'
import { addDays, exclusiveEnd, formatCompact, formatDateTime, todayInZone } from '../lib/format'
import { modelIcon } from '../lib/modelIcon'
import { useI18n } from '../i18n'
import type { MessageKey } from '../i18n/zh'
import { useSite } from '../site/SiteProvider'
import { useDocumentTitle } from '../components/PageTitle'
import {
  Alert,
  Badge,
  Button,
  Card,
  CardBody,
  Input,
  PageHead,
  Segmented,
  Select,
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

/** 每页条数：一屏放得下，翻页也不必太快。 */
const PAGE_SIZE = 20

const RANGES = [
  { id: 'today', days: 0 },
  { id: '7d', days: 6 },
  { id: '30d', days: 29 },
] as const

type RangeId = (typeof RANGES)[number]['id']

/** 入口协议：库里的 `openai-responses` 在界面上写成 `responses`，其余照原值。 */
function protocolLabel(protocol: string): string {
  return protocol === 'openai-responses' ? 'responses' : protocol || '—'
}

function Duration({ ms }: { ms: number | null }) {
  if (ms === null) return <span className="text-faint">—</span>
  if (ms < 1000) return <span className="num">{ms} ms</span>
  return <span className="num">{(ms / 1000).toFixed(1)}s</span>
}

/** TTFT：毫秒口径与耗时一致。null 是「这次没有可测的第一个块」。 */
function Ttft({ ms }: { ms: number | null }) {
  if (ms === null) return <span className="text-faint">—</span>
  if (ms < 1000) return <span className="num">{ms} ms</span>
  return <span className="num">{(ms / 1000).toFixed(2)}s</span>
}

/**
 * TPS：第一个内容块之后的生成速度 =（输出 + 推理）÷（耗时 − TTFT）。
 *
 * 推理 token 也是从模型流出来的输出，只算正文会把推理占比高的模型算得
 * 偏慢（deepseek 那次正文 8、推理 40，速度差了六倍）。分母不含排队与
 * TTFT：总耗时里它们占的是等待，不是生成。缺 TTFT（非流式）、缺耗时，
 * 或耗时没超过 TTFT 时算不出来，显示「—」。
 */
function Tps({
  output,
  reasoning,
  durationMs,
  firstTokenMs,
}: {
  output: number
  reasoning: number
  durationMs: number | null
  firstTokenMs: number | null
}) {
  const produced = output + reasoning
  if (produced <= 0 || durationMs === null || firstTokenMs === null) {
    return <span className="text-faint">—</span>
  }
  const window = durationMs - firstTokenMs
  if (window <= 0) return <span className="text-faint">—</span>
  const tps = produced / (window / 1000)
  return <span className="num">{tps.toFixed(1)}</span>
}

/**
 * 单行请求：请求 ID / 密钥、模型 / 供应商、协议、输入 / 缓存、输出 / 推理、
 * TTFT、TPS、总耗时、状态、User-Agent、受理时刻。
 *
 * TTFT、TPS、User-Agent 三列现在都没有数据源：账本只记了总耗时与 token，
 * 既没记首字节时刻，也没记调用方的 User-Agent。缺的位置显示「—」，
 * 不拿总耗时凑一个假的 TTFT。
 */
/** 厂商图标：尺寸固定 16，跟随当前前景色。 */
function ModelGlyph({ model }: { model: string }) {
  const Icon = modelIcon(model)
  if (!Icon) return null
  return <Icon size={16} className="mt-0.5 shrink-0 text-muted-foreground" />
}

function RequestRow({ request, timezone }: { request: UsageRequest; timezone: string }) {
  const { t } = useI18n()
  const tokens = request.tokens
  const missing = t('requests.notRecorded')
  return (
    <div className="flex items-center gap-3 px-4 py-2.5">
      <span className="w-40 shrink-0">
        <span
          className="mono block truncate text-2xs text-muted-foreground"
          title={request.request_id}
        >
          {request.request_id}
        </span>
        <span className="block truncate text-xs text-faint">
          {request.key?.name || t('common.none')}
        </span>
      </span>
      <span className="flex min-w-0 flex-1 items-start gap-2">
        {/* 图标按模型名查厂商表；查不到的模型不画图标，也不留空框。 */}
        <ModelGlyph model={request.model} />
        <span className="min-w-0 flex-1">
          <span className="block truncate text-sm font-medium">{request.model}</span>
          <span className="block truncate text-xs text-faint" title={request.provider}>
            {request.provider || t('common.none')}
          </span>
        </span>
      </span>
      <span className="w-20 shrink-0 truncate text-xs text-muted-foreground">
        {protocolLabel(request.protocol)}
      </span>
      {/* 两组 token 各占两行：主量在上（输入 / 输出），从量在下（缓存 / 推理）。
          大数换 K/M/B：万级往上的四五个零在列宽里读不出差别。 */}
      <span className="w-20 shrink-0 text-right">
        <span className="num block text-xs text-foreground tabular-nums">
          {formatCompact(tokens.input)}
        </span>
        <span className="num block text-2xs text-faint tabular-nums">
          {formatCompact(tokens.cache_read)}
        </span>
      </span>
      <span className="w-20 shrink-0 text-right">
        <span className="num block text-xs text-foreground tabular-nums">
          {formatCompact(tokens.output)}
        </span>
        <span className="num block text-2xs text-faint tabular-nums">
          {formatCompact(tokens.reasoning)}
        </span>
      </span>
      <span className="w-14 shrink-0 text-right text-xs text-muted-foreground">
        <Ttft ms={request.first_token_ms} />
      </span>
      <span className="w-14 shrink-0 text-right text-xs text-muted-foreground">
        <Tps
          output={tokens.output}
          reasoning={tokens.reasoning}
          durationMs={request.duration_ms}
          firstTokenMs={request.first_token_ms}
        />
      </span>
      <span className="w-16 shrink-0 text-right text-xs text-muted-foreground">
        <Duration ms={request.duration_ms} />
      </span>
      <span className="flex w-20 shrink-0 justify-end">
        <Badge tone={STATUS_TONE[request.status]} dot>
          {t(`status.${request.status}` as MessageKey)}
        </Badge>
      </span>
      <span
        className="hidden w-24 shrink-0 truncate text-2xs text-faint lg:block"
        title={request.user_agent ?? missing}
      >
        {request.user_agent || '—'}
      </span>
      <span className="hidden w-32 shrink-0 text-right text-xs text-muted-foreground tabular-nums md:block">
        {formatDateTime(request.started_at, timezone)}
      </span>
    </div>
  )
}

/**
 * 请求日志：按时间倒序一页一页读自己的每一次调用。
 *
 * 服务端只给游标、不给总数，所以页码靠前端攒一条游标栈：第 n 页用第 n 页
 * 起点的 cursor 取，取回来后把它的 `next_cursor` 记成第 n+1 页的起点。
 * 没有总数就拿不出「第 3 / 8 页」，底栏只标当前页与有没有下一页。
 */
export function Requests() {
  const { t } = useI18n()
  const { meta } = useSite()
  const [range, setRange] = useState<RangeId>('7d')
  const [status, setStatus] = useState<'all' | 'settled' | 'failed'>('all')
  const [keyId, setKeyId] = useState('all')
  const [model, setModel] = useState('all')
  const [search, setSearch] = useState('')

  // 游标栈：cursors[n] 是第 n+1 页的起点，第一页永远是 null。
  const [pageIndex, setPageIndex] = useState(0)
  const [cursors, setCursors] = useState<(string | null)[]>([null])

  useDocumentTitle(`${t('requests.title')} · ${meta.site_name}`)

  // 时间窗按站点时区的日历算，与服务端分桶同一套。
  const today = todayInZone(meta.timezone)
  const days = RANGES.find((item) => item.id === range)?.days ?? 6
  const from = addDays(today, -days)
  const to = exclusiveEnd(today)

  // 换筛选条件就回到第一页：旧的游标属于另一套条件，接着用会跳行。
  const lastFilters = useRef(`${range}|${status}|${keyId}|${model}`)
  useEffect(() => {
    const next = `${range}|${status}|${keyId}|${model}`
    if (lastFilters.current !== next) {
      lastFilters.current = next
      setPageIndex(0)
      setCursors([null])
    }
  }, [range, status, keyId, model])

  const cursor = cursors[pageIndex] ?? null
  const requests = useQuery({
    queryKey: ['requests', range, status, keyId, model, meta.timezone, cursor],
    queryFn: () =>
      api.usageRequests({
        from,
        to,
        limit: PAGE_SIZE,
        cursor,
        ...(status === 'all' ? {} : { status }),
        ...(keyId === 'all' ? {} : { key_id: Number(keyId) }),
        ...(model === 'all' ? {} : { model }),
      }),
    placeholderData: (previous) => previous,
  })

  // 下一页的起点只能是这一页给的：拿回来记进栈里，翻页时直接取。
  const nextCursor = requests.data?.next_cursor ?? null
  useEffect(() => {
    if (!nextCursor) return
    setCursors((prev) => {
      if (prev[pageIndex + 1] === nextCursor) return prev
      const next = [...prev]
      next[pageIndex + 1] = nextCursor
      return next
    })
  }, [nextCursor, pageIndex])

  const keys = useQuery({ queryKey: ['keys', 'requests-filter'], queryFn: () => api.listKeys() })
  const models = useQuery({ queryKey: ['models', 'requests-filter'], queryFn: () => api.models() })

  // 搜索只在当前这一页里找：服务端没有按请求 ID 检索的参数，
  // 跨页的「全部匹配」得等接口支持，先不做成假的。
  const needle = search.trim().toLowerCase()
  const pageItems = requests.data?.items ?? []
  const items =
    needle === ''
      ? pageItems
      : pageItems.filter(
          (item) =>
            item.request_id.toLowerCase().includes(needle) ||
            item.model.toLowerCase().includes(needle) ||
            (item.key?.name ?? '').toLowerCase().includes(needle),
        )
  const timezone = meta.timezone
  const hasNext = nextCursor !== null

  return (
    <>
      <PageHead title={t('requests.title')} description={t('requests.subtitle')} />

      {/* 左：搜索 + 模型 + 密钥；右：状态 + 时间。两组各自成块，中间留白分开。 */}
      <div className="flex flex-wrap items-center gap-3">
        <Input
          className="h-9 w-56"
          type="search"
          aria-label={t('common.search')}
          placeholder={t('requests.searchPlaceholder')}
          value={search}
          onChange={(event) => {
            setSearch(event.target.value)
            setPageIndex(0)
            setCursors([null])
          }}
        />
        <Select
          className="w-44"
          ariaLabel={t('requests.filterModel')}
          value={model}
          onValueChange={setModel}
          options={[
            { value: 'all', label: t('requests.allModels') },
            ...(models.data?.items ?? []).map((item) => ({
              value: item.name,
              label: item.display_name || item.name,
            })),
          ]}
        />
        <Select
          className="w-44"
          ariaLabel={t('requests.filterKey')}
          value={keyId}
          onValueChange={setKeyId}
          options={[
            { value: 'all', label: t('requests.allKeys') },
            ...(keys.data?.items ?? [])
              .filter((item) => item.status !== 'revoked')
              // 没起名字的密钥用前缀当标签：掩码整串太长，选中后会把触发器撑成两行。
              .map((item) => ({ value: String(item.id), label: item.name || item.prefix })),
          ]}
        />
        <div className="ml-auto flex items-center gap-2">
          <Segmented
            size="md"
            ariaLabel={t('requests.status')}
            value={status}
            onValueChange={setStatus}
            options={[
              { value: 'all', label: t('common.all') },
              { value: 'settled', label: t('status.settled') },
              { value: 'failed', label: t('status.failed') },
            ]}
          />
          <Segmented
            size="md"
            ariaLabel={t('requests.range')}
            value={range}
            onValueChange={setRange}
            options={[
              { value: 'today', label: t('common.today') },
              { value: '7d', label: t('requests.range7d') },
              { value: '30d', label: t('requests.range30d') },
            ]}
          />
        </div>
      </div>

      <Card>
        {requests.isPending ? (
          <CardBody className="flex flex-col gap-2">
            <Skeleton className="h-9 w-full" />
            <Skeleton className="h-9 w-full" />
            <Skeleton className="h-9 w-full" />
          </CardBody>
        ) : requests.isError ? (
          <CardBody>
            <Alert tone="danger" title={t('requests.loadFailed')}>
              <Button variant="secondary" size="sm" onClick={() => void requests.refetch()}>
                {t('common.retry')}
              </Button>
            </Alert>
          </CardBody>
        ) : items.length === 0 ? (
          <CardBody className="py-8">
            <p className="text-md font-semibold">{t('requests.emptyTitle')}</p>
            <p className="mt-1 text-sm text-muted-foreground">{t('requests.emptyBody')}</p>
          </CardBody>
        ) : (
          <>
            {/* 表头与行共用同一组列宽，右半边的数字才对得上。 */}
            <div className="sticky top-0 z-sticky flex items-center gap-3 border-b border-border bg-card px-4 py-2 text-2xs font-semibold text-muted-foreground">
              <span className="w-40 shrink-0">{t('requests.colId')}</span>
              <span className="min-w-0 flex-1">{t('requests.colModel')}</span>
              <span className="w-20 shrink-0">{t('requests.colProtocol')}</span>
              <span className="w-20 shrink-0 text-right">{t('requests.colInputCache')}</span>
              <span className="w-20 shrink-0 text-right">{t('requests.colOutputReasoning')}</span>
              <span className="w-14 shrink-0 text-right">{t('requests.colTtft')}</span>
              <span className="w-14 shrink-0 text-right">{t('requests.colTps')}</span>
              <span className="w-16 shrink-0 text-right">{t('requests.colDuration')}</span>
              <span className="flex w-20 shrink-0 justify-end">{t('requests.colStatus')}</span>
              <span className="hidden w-24 shrink-0 lg:block">{t('requests.colUserAgent')}</span>
              <span className="hidden w-32 shrink-0 text-right md:block">
                {t('requests.colTime')}
              </span>
            </div>
            <div className="divide-y divide-border">
              {items.map((request) => (
                <RequestRow key={request.id} request={request} timezone={timezone} />
              ))}
            </div>
            {/* 没有总数：只标当前页与本页条数，翻页靠游标栈。 */}
            <nav
              aria-label={t('requests.pager')}
              className="flex items-center justify-between gap-3 border-t border-border px-4 py-2 text-sm text-muted-foreground"
            >
              <span className="num text-xs">
                {t('requests.pageRows').replace('{n}', String(items.length))}
              </span>
              <div className="flex items-center gap-1">
                <Button
                  variant="ghost"
                  size="icon"
                  className="size-8"
                  aria-label={t('requests.prevPage')}
                  disabled={pageIndex === 0 || requests.isFetching}
                  onClick={() => setPageIndex((current) => Math.max(0, current - 1))}
                >
                  <ChevronLeft className="size-4" />
                </Button>
                <span className="num min-w-14 text-center text-xs">
                  {t('requests.pageLabel').replace('{n}', String(pageIndex + 1))}
                </span>
                <Button
                  variant="ghost"
                  size="icon"
                  className="size-8"
                  aria-label={t('requests.nextPage')}
                  disabled={!hasNext || requests.isFetching}
                  onClick={() => setPageIndex((current) => current + 1)}
                >
                  <ChevronRight className="size-4" />
                </Button>
              </div>
            </nav>
          </>
        )}
      </Card>
    </>
  )
}
