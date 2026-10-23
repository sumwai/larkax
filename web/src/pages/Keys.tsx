import { useMemo, useState, type ReactNode } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '../api'
import { ApiError } from '../api/client'
import type { ApiKey, KeyStatus } from '../api/types'
import { addDays, formatDate, formatDateTime, moneySymbol, todayInZone } from '../lib/format'
import { keySnippets } from '../lib/keyExport'
import { MagpieIcon } from '../assets/import-targets/MagpieIcon'
import ccSwitchIcon from '../assets/import-targets/cc-switch.png'
import lodyIcon from '../assets/import-targets/lody.png'
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
  DialogContent,
  Field,
  Input,
  Money,
  PageHead,
  Pagination,
  CopyButton,
  Segmented,
  Skeleton,
  toast,
} from '../components/ui'
import { ChevronDown } from 'lucide-react'

/** 每页行数：一屏能放下，翻页也不必太长。 */
const PAGE_SIZE = 10

const STATUS_TONE: Record<KeyStatus, 'success' | 'neutral' | 'danger'> = {
  active: 'success',
  disabled: 'neutral',
  revoked: 'danger',
}

/** 三个限制各占一列：额度（累计 / 上限）、速率、过期。 */
function QuotaCell({ item }: { item: ApiKey }) {
  const { t } = useI18n()
  return (
    <span className="w-32 shrink-0 text-right text-xs text-muted-foreground">
      <Money value={item.quota_used} variant="spend" />
      <span className="text-faint"> / </span>
      {item.quota_limit === null ? (
        <span className="text-faint">{t('status.unlimited')}</span>
      ) : (
        <Money value={item.quota_limit} />
      )}
    </span>
  )
}

/** 速率：null 表示不限。 */
function RateCell({ item }: { item: ApiKey }) {
  const { t } = useI18n()
  return (
    <span className="w-20 shrink-0 text-right text-xs text-faint">
      {item.rate_limit_rpm === null
        ? t('status.unlimited')
        : `${item.rate_limit_rpm} ${t('keys.perMinute')}`}
    </span>
  )
}

/** 过期：按站点时区显示日期；`never_expires` 是服务端算好的。 */
function ExpiryCell({ item }: { item: ApiKey }) {
  const { t } = useI18n()
  const { meta } = useSite()
  return (
    <span className="hidden w-24 shrink-0 text-right text-xs text-faint md:block">
      {item.never_expires || !item.expires_at
        ? t('keys.noExpiry')
        : formatDate(item.expires_at, meta.timezone)}
    </span>
  )
}

/** 创建表单的状态：额度与速率用预设值，空串表示不限；过期用档位名。 */
interface Draft {
  name: string
  quota: string
  rate: string
  expiry: string
}

const EMPTY_DRAFT: Draft = { name: '', quota: '', rate: '', expiry: '' }

/** 额度预设（按站点币种显示）；空串是不限。 */
const QUOTA_PRESETS = ['', '10', '50', '100', '500'] as const

/** 速率预设，次/分；空串是不限。 */
const RATE_PRESETS = ['', '20', '60', '120', '600'] as const

/** 过期档位：值与天数，空串是不过期。 */
const EXPIRY_PRESETS = [
  { value: '', days: 0, label: 'keys.expiryNever' },
  { value: '7d', days: 7, label: 'keys.expiry7d' },
  { value: '30d', days: 30, label: 'keys.expiry30d' },
  { value: '90d', days: 90, label: 'keys.expiry90d' },
  { value: '1y', days: 365, label: 'keys.expiry1y' },
] as const

/** 一排可点的预设值：选中的那个用底色标出来，不再让人手填。 */
function Presets<T extends string>({
  value,
  options,
  onChange,
  ariaLabel,
  className,
}: {
  value: T
  options: { value: T; label: ReactNode }[]
  onChange: (value: T) => void
  ariaLabel: string
  className?: string
}) {
  return (
    <div
      role="group"
      aria-label={ariaLabel}
      className={`inline-flex flex-wrap gap-1.5 ${className ?? ''}`}
    >
      {options.map((option) => {
        const active = option.value === value
        return (
          <button
            key={option.value}
            type="button"
            aria-pressed={active}
            onClick={() => onChange(option.value)}
            className={`rounded-full border px-2 py-0.5 text-xs transition-colors ${
              active
                ? 'border-ring bg-primary-soft font-medium text-foreground'
                : 'border-border text-muted-foreground hover:bg-muted hover:text-foreground'
            }`}
          >
            {option.label}
          </button>
        )
      })}
    </div>
  )
}

/**
 * 整把密钥只在创建响应里出现一次，所以拿它单独晒一屏：
 * 复制走统一的 `copyText`（非安全上下文下会失败），关掉就再也拿不回来。
 */
function CreatedKeyDialog({ secret, onClose }: { secret: string; onClose: () => void }) {
  const { t } = useI18n()
  const { meta } = useSite()
  const [showing, setShowing] = useState<'secret' | 'export'>('secret')
  const snippets = useMemo(
    () => keySnippets(secret, meta.api_base_url, meta.site_name),
    [secret, meta.api_base_url, meta.site_name],
  )
  const targets = [
    {
      id: 'magpie',
      name: 'magpie',
      icon: <MagpieIcon className="size-7" />,
      link: snippets.magpie,
    },
    {
      id: 'ccswitch',
      name: 'CC Switch',
      icon: <img src={ccSwitchIcon} alt="" className="size-8" />,
      link: snippets.ccSwitch,
    },
    {
      id: 'lody',
      name: 'Lody',
      icon: <img src={lodyIcon} alt="" className="size-8" />,
      link: snippets.lody,
    },
  ]
  return (
    <DialogContent
      open
      size={showing === 'export' ? 'lg' : 'md'}
      dismissOnOutsideClick={false}
      onOpenChange={(open) => {
        if (open) return
        // 导出屏上的关闭（X、Esc）退回密钥那一屏：整把密钥只出现这一次。
        if (showing === 'export') setShowing('secret')
        else onClose()
      }}
      title={showing === 'export' ? t('keys.exportTitle') : t('keys.createdTitle')}
      description={showing === 'export' ? t('keys.exportBody') : t('keys.createdBody')}
      footer={
        <div className="flex items-center gap-2">
          {showing === 'export' ? (
            <Button variant="secondary" onClick={() => setShowing('secret')}>
              {t('common.back')}
            </Button>
          ) : (
            <Button variant="secondary" onClick={() => setShowing('export')}>
              {t('keys.export')}
            </Button>
          )}
          <Button onClick={onClose}>{t('keys.savedIt')}</Button>
        </div>
      }
    >
      {showing === 'secret' ? (
        /* 密钥与复制按钮同处一条：分两行时复制按钮会读成一个输入框。 */
        <div className="flex items-center gap-2 rounded-md border border-input bg-muted py-1.5 pl-3 pr-1.5">
          <code className="mono min-w-0 flex-1 overflow-x-auto whitespace-nowrap text-sm">
            {secret}
          </code>
          <CopyButton
            className="shrink-0"
            value={secret}
            label={t('keys.copyKey')}
            copiedLabel={t('common.copied')}
          />
        </div>
      ) : (
        /* 链接本身不铺在页面上：点图标交给系统的 scheme 处理器，各客户端自己弹确认框。 */
        <div className="grid grid-cols-3 gap-3">
          {targets.map((target) => (
            <a
              key={target.id}
              href={target.link}
              className="flex flex-col items-center gap-2 rounded-md border border-border px-3 py-5 text-center transition-colors duration-(--dur) hover:bg-muted focus-visible:bg-muted focus-visible:outline-none"
            >
              <span className="flex size-10 items-center justify-center rounded-md border border-border">
                {target.icon}
              </span>
              <span className="text-sm font-medium">{target.name}</span>
            </a>
          ))}
        </div>
      )}
    </DialogContent>
  )
}

/** 创建表单：名称、额度上限、速率上限、过期时间，后三个留空即不限。 */
/**
 * 创建与编辑共用的表单：名称 + 三组限制。
 *
 * 两个对话框的字段、预设、校验都从这里出；分开写两份的那一天，
 * 就是两处开始对不上的那一天。
 */
function KeyFormFields({
  draft,
  setDraft,
  advanced,
  setAdvanced,
  error,
  expiryOptions,
}: {
  draft: Draft
  setDraft: (draft: Draft) => void
  advanced: boolean
  setAdvanced: (open: boolean) => void
  error: MessageKey | null
  /** 编辑时多一个「保持现状」档，创建时只有那五个。 */
  expiryOptions?: { value: string; label: string }[]
}) {
  const { t } = useI18n()
  const { meta } = useSite()
  const symbol = moneySymbol(meta.currency)

  return (
    <div className="flex flex-col gap-4">
      {error ? <Alert tone="danger" title={t(error)} /> : null}

      <Field label={t('keys.name')} hint={t('keys.nameOptional')}>
        {({ inputId, describedBy }) => (
          <Input
            id={inputId}
            aria-describedby={describedBy}
            placeholder={t('keys.namePlaceholder')}
            value={draft.name}
            onChange={(event) => setDraft({ ...draft, name: event.target.value })}
          />
        )}
      </Field>

      <div className="rounded-sm border border-border">
        <button
          type="button"
          aria-expanded={advanced}
          onClick={() => setAdvanced(!advanced)}
          className="flex w-full cursor-pointer items-center justify-between gap-2 px-3 py-2 text-sm text-muted-foreground hover:text-foreground"
        >
          {t('keys.advanced')}
          <ChevronDown
            className={`size-4 transition-transform duration-(--dur) ${advanced ? 'rotate-180' : ''}`}
            aria-hidden="true"
          />
        </button>

        {/* 0fr → 1fr：高度靠网格行跑过渡，对话框的 auto 高度跟着长。 */}
        <div
          className={`grid transition-[grid-template-rows] duration-(--dur) ${advanced ? 'grid-rows-[1fr]' : 'grid-rows-[0fr]'}`}
        >
          <div className="overflow-hidden">
            <div className="flex flex-col gap-3 border-t border-border px-3 py-3">
              <div className="flex flex-col gap-1.5">
                <span className="text-sm font-medium">{t('keys.quotaLimit')}</span>
                <div className="flex flex-wrap items-center gap-2">
                  <Input
                    aria-label={t('keys.quotaLimit')}
                    className="h-7 w-24 text-sm"
                    inputMode="decimal"
                    placeholder={`${symbol}0.00`}
                    value={draft.quota}
                    onChange={(event) => setDraft({ ...draft, quota: event.target.value })}
                  />
                  <div className="flex min-w-0 flex-wrap gap-1.5">
                    <Presets
                      ariaLabel={t('keys.quotaLimit')}
                      value={draft.quota}
                      onChange={(quota) => setDraft({ ...draft, quota })}
                      options={QUOTA_PRESETS.map((quota) => ({
                        value: quota,
                        label:
                          quota === '' ? (
                            t('status.unlimited')
                          ) : (
                            <span className="num">
                              {symbol}
                              {quota}
                            </span>
                          ),
                      }))}
                    />
                  </div>
                </div>
              </div>

              <div className="flex flex-col gap-1.5">
                <span className="text-sm font-medium">
                  {t('keys.rateLimit')}
                  <span className="ml-1 text-xs font-normal text-faint">{t('keys.perMinute')}</span>
                </span>
                <div className="flex flex-wrap items-center gap-2">
                  <Input
                    aria-label={t('keys.rateLimit')}
                    className="h-7 w-24 text-sm"
                    inputMode="numeric"
                    placeholder="0"
                    value={draft.rate}
                    onChange={(event) => setDraft({ ...draft, rate: event.target.value })}
                  />
                  <div className="flex min-w-0 flex-wrap gap-1.5">
                    <Presets
                      ariaLabel={t('keys.rateLimit')}
                      value={draft.rate}
                      onChange={(rate) => setDraft({ ...draft, rate })}
                      options={RATE_PRESETS.map((rate) => ({
                        value: rate,
                        label:
                          rate === '' ? t('status.unlimited') : <span className="num">{rate}</span>,
                      }))}
                    />
                  </div>
                </div>
              </div>

              <div className="flex flex-col gap-1.5">
                <span className="text-sm font-medium">{t('keys.expiresAt')}</span>
                <Presets
                  ariaLabel={t('keys.expiresAt')}
                  value={draft.expiry}
                  onChange={(expiry) => setDraft({ ...draft, expiry })}
                  options={
                    expiryOptions ??
                    EXPIRY_PRESETS.map((item) => ({ value: item.value, label: t(item.label) }))
                  }
                />
              </div>
            </div>
          </div>
        </div>
      </div>
    </div>
  )
}

function CreateKeyDialog({ onCreated }: { onCreated: (secret: string) => void }) {
  const { t } = useI18n()
  const { meta } = useSite()
  const [open, setOpen] = useState(false)
  const [draft, setDraft] = useState<Draft>(EMPTY_DRAFT)
  const [error, setError] = useState<MessageKey | null>(null)
  const [advanced, setAdvanced] = useState(false)

  const expiryDays = EXPIRY_PRESETS.find((item) => item.value === draft.expiry)?.days ?? 0

  const create = useMutation({
    mutationFn: () =>
      api.createKey({
        name: draft.name.trim() || undefined,
        quota_limit: draft.quota.trim() || undefined,
        rate_limit_rpm: draft.rate.trim() === '' ? undefined : Number(draft.rate.trim()),
        // 档位换成一个具体时刻：站点时区当天 23:59:59，比「当天零点」少一天歧义。
        expires_at:
          expiryDays === 0
            ? undefined
            : new Date(`${addDays(todayInZone(meta.timezone), expiryDays)}T23:59:59`).toISOString(),
      }),
    onSuccess: (created) => {
      close()
      onCreated(created.secret)
    },
    onError: (err) => {
      // 400 是我们填的字段不对，其余（网络、5xx）不该让人去改表单。
      setError(
        err instanceof ApiError && err.status === 400 ? 'keys.createFailed' : 'errors.generic',
      )
    },
  })

  function submit() {
    setError(null)
    const quota = draft.quota.trim()
    if (quota !== '' && !/^\d+(\.\d{1,8})?$/.test(quota)) {
      setError('keys.invalidQuota')
      return
    }
    const rate = draft.rate.trim()
    if (rate !== '' && !/^\d+$/.test(rate)) {
      setError('keys.invalidRate')
      return
    }
    create.mutate()
  }

  /**
   * 关闭时把表单清干净：名称、预设、展开状态、上次的错误都不留给下一次。
   * 取消、Esc、点遮罩都走这里，不只靠「创建成功」那一条路。
   */
  function close() {
    setOpen(false)
    setDraft(EMPTY_DRAFT)
    setAdvanced(false)
    setError(null)
  }

  return (
    <>
      <Button onClick={() => setOpen(true)}>{t('keys.create')}</Button>
      <DialogContent
        open={open}
        onOpenChange={(next) => {
          if (next) setOpen(true)
          else close()
        }}
        title={t('keys.createTitle')}
        description={t('keys.createBody')}
        footer={
          <>
            <Button variant="secondary" onClick={close}>
              {t('common.cancel')}
            </Button>
            <Button busy={create.isPending} onClick={submit}>
              {t('keys.create')}
            </Button>
          </>
        }
      >
        <KeyFormFields
          draft={draft}
          setDraft={setDraft}
          advanced={advanced}
          setAdvanced={setAdvanced}
          error={error}
        />
      </DialogContent>
    </>
  )
}

/**
 * 编辑一把密钥：名称与三组限制。
 *
 * 过期时间这一个字段有三种意图：不动（空档）、改成「不过期」（`null`）、
 * 或换成新的档位。三者不能混成一个 `expires_at`，否则「不改」会变成
 * 「清空」。
 */
function EditKeyDialog({ item }: { item: ApiKey }) {
  const { t } = useI18n()
  const { meta } = useSite()
  const queryClient = useQueryClient()
  const [open, setOpen] = useState(false)
  const [advanced, setAdvanced] = useState(false)
  const [error, setError] = useState<MessageKey | null>(null)
  const [draft, setDraft] = useState<Draft>(EMPTY_DRAFT)

  const update = useMutation({
    mutationFn: () => {
      const quota = draft.quota.trim()
      const rate = draft.rate.trim()
      const days = EXPIRY_PRESETS.find((preset) => preset.value === draft.expiry)?.days
      return api.updateKey(item.id, {
        name: draft.name.trim(),
        quota_limit: quota === '' ? null : quota,
        rate_limit_rpm: rate === '' ? null : Number(rate),
        // 空档不传：保持原来的到期时间；选了就换算成站点时区的当天 23:59:59。
        ...(draft.expiry === ''
          ? {}
          : days === undefined
            ? { expires_at: null }
            : {
                expires_at: new Date(
                  `${addDays(todayInZone(meta.timezone), days)}T23:59:59`,
                ).toISOString(),
              }),
      })
    },
    onSuccess: () => {
      close()
      toast.success(t('keys.updatedOk'))
      void queryClient.invalidateQueries({ queryKey: ['keys'] })
    },
    onError: (err) => {
      setError(
        err instanceof ApiError && err.status === 400 ? 'keys.createFailed' : 'errors.generic',
      )
    },
  })

  function openDialog() {
    setDraft({
      name: item.name ?? '',
      quota: item.quota_limit ?? '',
      rate: item.rate_limit_rpm === null ? '' : String(item.rate_limit_rpm),
      expiry: '',
    })
    setAdvanced(false)
    setError(null)
    setOpen(true)
  }

  function close() {
    setOpen(false)
    setDraft(EMPTY_DRAFT)
    setAdvanced(false)
    setError(null)
  }

  function submit() {
    setError(null)
    const quota = draft.quota.trim()
    if (quota !== '' && !/^\d+(\.\d{1,8})?$/.test(quota)) {
      setError('keys.invalidQuota')
      return
    }
    const rate = draft.rate.trim()
    if (rate !== '' && !/^\d+$/.test(rate)) {
      setError('keys.invalidRate')
      return
    }
    update.mutate()
  }

  return (
    <>
      <Button variant="ghost" size="sm" onClick={openDialog}>
        {t('keys.edit')}
      </Button>
      <DialogContent
        open={open}
        onOpenChange={(next) => {
          if (next) setOpen(true)
          else close()
        }}
        title={t('keys.editTitle')}
        description={t('keys.editBody')}
        footer={
          <>
            <Button variant="secondary" onClick={close}>
              {t('common.cancel')}
            </Button>
            <Button busy={update.isPending} onClick={submit}>
              {t('common.save')}
            </Button>
          </>
        }
      >
        <KeyFormFields
          draft={draft}
          setDraft={setDraft}
          advanced={advanced}
          setAdvanced={setAdvanced}
          error={error}
          expiryOptions={[
            { value: '', label: t('keys.expiryKeep') },
            ...EXPIRY_PRESETS.map((preset) => ({
              value: preset.value === '' ? 'never' : preset.value,
              label: t(preset.label),
            })),
          ]}
        />
      </DialogContent>
    </>
  )
}

/** 一行密钥：状态、额度、最近使用与操作。 */
function KeyRow({ item }: { item: ApiKey }) {
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const [lastError, setLastError] = useState<MessageKey | null>(null)

  const refresh = () => queryClient.invalidateQueries({ queryKey: ['keys'] })

  const toggle = useMutation({
    mutationFn: () =>
      api.updateKey(item.id, { status: item.status === 'active' ? 'disabled' : 'active' }),
    onSuccess: () => {
      setLastError(null)
      toast.success(t(item.status === 'active' ? 'keys.disabledOk' : 'keys.enabledOk'))
      void refresh()
    },
    onError: () => setLastError('keys.updateFailed'),
  })

  return (
    <div className="flex flex-wrap items-center gap-3 px-4 py-3">
      <span className="min-w-0 flex-1">
        <span className="block truncate text-sm font-medium" title={item.name || undefined}>
          {item.name || t('common.none')}
        </span>
        <span className="mono block truncate text-xs text-faint">{item.masked}</span>
        {lastError ? (
          <span className="mt-1 block text-xs text-danger-strong">{t(lastError)}</span>
        ) : null}
      </span>

      <QuotaCell item={item} />
      <RateCell item={item} />
      <ExpiryCell item={item} />

      <span className="hidden w-32 shrink-0 text-xs text-faint sm:block">
        {item.last_used_at ? formatDateTime(item.last_used_at) : t('common.none')}
      </span>

      <span className="w-20 shrink-0">
        <Badge tone={STATUS_TONE[item.status]} dot>
          {t(`status.${item.status}` as MessageKey)}
        </Badge>
      </span>

      <span className="flex w-28 shrink-0 items-center justify-end gap-1">
        <EditKeyDialog item={item} />
        <Button variant="ghost" size="sm" busy={toggle.isPending} onClick={() => toggle.mutate()}>
          {t(item.status === 'active' ? 'keys.disable' : 'keys.enable')}
        </Button>
      </span>
    </div>
  )
}

/**
 * API 密钥页：建一把密钥就能调 `/v1`。
 *
 * 创建后整把密钥只出现一次，所以创建对话框关掉之后立刻换成「已创建」那张，
 * 不给第二次机会。
 */
export function Keys() {
  const { t } = useI18n()
  const queryClient = useQueryClient()
  const [secret, setSecret] = useState<string | null>(null)
  const [search, setSearch] = useState('')
  const [status, setStatus] = useState<'all' | 'active' | 'disabled'>('all')
  const [page, setPage] = useState(1)

  useDocumentTitle(`${t('keys.title')} · ${t('common.appName')}`)

  const keys = useQuery({ queryKey: ['keys'], queryFn: () => api.listKeys() })
  // 已吊销的密钥不出现在用户的列表里：吊销不可逆，留着只会让人以为它还能用。
  // 服务端的列表接口仍会带上它们，筛选放在这一层。
  const visible = (keys.data?.items ?? [])
    .filter((item) => item.status !== 'revoked')
    .filter((item) => status === 'all' || item.status === status)
  // 名称与密钥前缀都参与匹配：手上只有一串前缀时也能定位到具体那把。
  const needle = search.trim().toLowerCase()
  const matched =
    needle === ''
      ? visible
      : visible.filter(
          (item) =>
            item.name.toLowerCase().includes(needle) ||
            item.masked.toLowerCase().includes(needle) ||
            item.prefix.toLowerCase().includes(needle),
        )
  // 服务端一次给全量，分页在客户端切；页数随筛选结果收缩，页码越界时夹回最后一页。
  const pageCount = Math.max(1, Math.ceil(matched.length / PAGE_SIZE))
  const current = Math.min(page, pageCount)
  const paged = matched.slice((current - 1) * PAGE_SIZE, current * PAGE_SIZE)

  return (
    <>
      <PageHead
        title={t('keys.title')}
        description={t('keys.subtitle')}
        actions={
          <CreateKeyDialog
            onCreated={(createdSecret) => {
              setSecret(createdSecret)
              void queryClient.invalidateQueries({ queryKey: ['keys'] })
            }}
          />
        }
      />

      <div className="flex flex-wrap items-center gap-3">
        <Input
          className="h-8 w-full max-w-xs text-sm"
          type="search"
          aria-label={t('common.search')}
          placeholder={t('common.search')}
          value={search}
          onChange={(event) => {
            setSearch(event.target.value)
            setPage(1)
          }}
        />
        <Segmented
          className="ml-auto"
          size="sm"
          ariaLabel={t('keys.colStatus')}
          value={status}
          onValueChange={(next) => {
            setStatus(next)
            setPage(1)
          }}
          options={[
            { value: 'all', label: t('common.all') },
            { value: 'active', label: t('status.active') },
            { value: 'disabled', label: t('status.disabled') },
          ]}
        />
      </div>

      <Card>
        {keys.isPending ? (
          <CardBody className="flex flex-col gap-2">
            <Skeleton className="h-10 w-full" />
            <Skeleton className="h-10 w-full" />
          </CardBody>
        ) : keys.isError ? (
          <CardBody>
            <Alert tone="danger" title={t('keys.loadFailed')}>
              <Button variant="secondary" size="sm" onClick={() => void keys.refetch()}>
                {t('common.retry')}
              </Button>
            </Alert>
          </CardBody>
        ) : matched.length === 0 ? (
          <CardBody className="flex flex-col items-start gap-3 py-8">
            <div>
              <p className="text-md font-semibold">{t('keys.emptyTitle')}</p>
              <p className="mt-1 text-sm text-muted-foreground">{t('keys.emptyBody')}</p>
            </div>
          </CardBody>
        ) : (
          <>
            {/* 表头：列宽与下面的行一致，三列限制才不会读成一段文字。 */}
            <div className="sticky top-0 z-sticky flex items-center gap-3 border-b border-border bg-card px-4 py-2 text-2xs font-semibold text-muted-foreground">
              <span className="min-w-0 flex-1">{t('keys.colName')}</span>
              <span className="w-32 shrink-0 text-right">{t('keys.colQuota')}</span>
              <span className="w-20 shrink-0 text-right">{t('keys.colRate')}</span>
              <span className="hidden w-24 shrink-0 text-right md:block">
                {t('keys.expiresAt')}
              </span>
              <span className="hidden w-32 shrink-0 sm:block">{t('keys.lastUsed')}</span>
              <span className="w-20 shrink-0">{t('keys.colStatus')}</span>
              <span className="w-28 shrink-0" />
            </div>
            <div className="divide-y divide-border">
              {paged.map((item) => (
                <KeyRow key={item.id} item={item} />
              ))}
            </div>
          </>
        )}
        {matched.length > PAGE_SIZE ? (
          <Pagination
            className="border-t border-border px-4 py-2"
            page={current}
            pageSize={PAGE_SIZE}
            total={matched.length}
            onPageChange={setPage}
          />
        ) : null}
      </Card>

      {secret ? <CreatedKeyDialog secret={secret} onClose={() => setSecret(null)} /> : null}
    </>
  )
}
