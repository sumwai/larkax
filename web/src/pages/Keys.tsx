import { useState, type FormEvent } from 'react'
import { api } from '../api'
import type { ApiKey, CreatedKey, KeyCreateInput } from '../api/types'
import { useI18n } from '../i18n'
import type { MessageKey } from '../i18n/zh'
import { useSite } from '../site/SiteProvider'
import { useAsync } from '../hooks/useAsync'
import { formatDate, formatDateTime, formatMoney, formatSpend, moneySymbol } from '../lib/format'
import {
  Alert,
  Button,
  CopyButton,
  Empty,
  Modal,
  MoneyText,
  PageHead,
  Skeleton,
  StatusPill,
  useToast,
} from '../components/ui'
import { IconChevronRight, IconKey, IconPlus, IconPower, IconTrash } from '../components/icons'
import { useConfirm } from './ops/common-helpers'
import { isDirty, useCloseGuard, useUnsavedGuard } from './ops/guard'

interface CreateForm {
  name: string
  quota: string
  rate: string
  expires: string
}

const EMPTY_FORM: CreateForm = { name: '', quota: '', rate: '', expires: '' }

interface ExpiryPreset {
  key: MessageKey
  /** 相对今天的天数；`years` 与 `days` 都不给表示永不过期（清空字段）。 */
  days?: number
  years?: number
}

const EXPIRY_PRESETS: ExpiryPreset[] = [
  { key: 'keys.expiryNever' },
  { key: 'keys.expiry7d', days: 7 },
  { key: 'keys.expiry30d', days: 30 },
  { key: 'keys.expiry90d', days: 90 },
  { key: 'keys.expiry1y', years: 1 },
]

const RATE_PRESETS = [60, 300, 600, 1200]

/** `datetime-local` 认的本地时间串，精确到分钟；快捷档位与手输写的是同一个值。 */
function toLocalInputValue(date: Date): string {
  const pad = (value: number) => String(value).padStart(2, '0')
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`
}

export function Keys() {
  const { t } = useI18n()
  const { meta } = useSite()
  const { show: toast } = useToast()
  const { data, error, loading, reload } = useAsync(() => api.listKeys({ limit: 100 }), [])
  const confirm = useConfirm()

  const [creating, setCreating] = useState(false)
  const [form, setForm] = useState<CreateForm>(EMPTY_FORM)
  // 打开弹窗时的那份值：关窗前拿它判断这一页有没有被改过。
  const [initialForm, setInitialForm] = useState<CreateForm>(EMPTY_FORM)
  // 手输过期时间后档位高亮要撤掉；拿「现在 + N 天」在渲染期重算会随分钟漂移。
  const [expiryPreset, setExpiryPreset] = useState<string | null>(null)
  const [formError, setFormError] = useState<string | null>(null)
  const [submitting, setSubmitting] = useState(false)
  const [created, setCreated] = useState<CreatedKey | null>(null)
  const [revoking, setRevoking] = useState<ApiKey | null>(null)
  const [revokePending, setRevokePending] = useState(false)
  const [detail, setDetail] = useState<ApiKey | null>(null)
  const [busyId, setBusyId] = useState<number | null>(null)
  const [actionError, setActionError] = useState<string | null>(null)

  const keys = data?.items ?? []
  // 与列表里的 MoneyText 同源：站点币种由 SiteProvider 从 /api/meta 取。
  const currencySymbol = moneySymbol(meta.currency)

  const openCreate = () => {
    setForm(EMPTY_FORM)
    setInitialForm(EMPTY_FORM)
    setExpiryPreset(null)
    setFormError(null)
    setCreating(true)
  }

  const closeCreateForm = () => {
    setCreating(false)
    setFormError(null)
  }

  // 表单真的打开着才算脏：`closeCreateForm` 只收起弹窗、不复位 form，
  // 少了 `creating` 这一条，「放弃」一次之后这一页会永远算改过，
  // 后续每次刷新与站内跳转都会被拦。
  //
  // 只比 form、不比 expiryPreset：写过时刻的档位（7 天、30 天……）会同时改
  // 输入框，差额已经落在 form 里；唯一只动高亮不动输入框的，是「永不过期」
  // 点在本来就为空的字段上——那一刻关窗没有任何会提交的内容丢失，不必再问。
  const dirty = creating && isDirty(form, initialForm)
  // 取消、Esc、点遮罩、右上角关闭钮都从这里走。
  const requestClose = useCloseGuard(confirm, dirty, closeCreateForm)
  const unsavedGuard = useUnsavedGuard(dirty)

  const applyExpiryPreset = (preset: ExpiryPreset) => {
    let value = ''
    if (preset.days) {
      const next = new Date()
      next.setDate(next.getDate() + preset.days)
      value = toLocalInputValue(next)
    } else if (preset.years) {
      const next = new Date()
      next.setFullYear(next.getFullYear() + preset.years)
      value = toLocalInputValue(next)
    }
    setExpiryPreset(preset.key)
    setForm((previous) => ({ ...previous, expires: value }))
  }

  const submit = async (event: FormEvent) => {
    event.preventDefault()
    const quota = form.quota.trim()
    const rate = form.rate.trim()
    if (quota && (!/^\d+(\.\d{1,8})?$/.test(quota) || !(Number.parseFloat(quota) > 0))) {
      setFormError(t('keys.invalidQuota'))
      return
    }
    if (rate && !/^\d+$/.test(rate)) {
      setFormError(t('keys.invalidRate'))
      return
    }
    let expiresAt: string | null = null
    if (form.expires) {
      const parsed = new Date(form.expires)
      if (Number.isNaN(parsed.getTime())) {
        setFormError(t('keys.invalidExpiry'))
        return
      }
      expiresAt = parsed.toISOString()
    }

    // 创建接口里「不限」是不给这个字段，不是给 null；额度为 0 也会被拒。
    const input: KeyCreateInput = {}
    if (form.name.trim()) input.name = form.name.trim()
    if (quota) input.quota_limit = quota
    if (rate) input.rate_limit_rpm = Number(rate)
    if (expiresAt) input.expires_at = expiresAt

    setSubmitting(true)
    setFormError(null)
    try {
      const result = await api.createKey(input)
      setCreated(result)
      setCreating(false)
      reload()
    } catch {
      setFormError(t('keys.createFailed'))
    } finally {
      setSubmitting(false)
    }
  }

  const toggle = async (key: ApiKey) => {
    const nextStatus = key.status === 'active' ? 'disabled' : 'active'
    setBusyId(key.id)
    setActionError(null)
    try {
      await api.updateKey(key.id, { status: nextStatus })
      toast(nextStatus === 'active' ? t('keys.enabledOk') : t('keys.disabledOk'))
      setDetail(null)
      reload()
    } catch {
      setActionError(t('keys.updateFailed'))
    } finally {
      setBusyId(null)
    }
  }

  const revoke = async () => {
    if (!revoking) return
    setRevokePending(true)
    setActionError(null)
    try {
      await api.revokeKey(revoking.id)
      toast(t('keys.revokedOk'))
      setRevoking(null)
      setDetail(null)
      reload()
    } catch {
      setActionError(t('keys.revokeFailed'))
    } finally {
      setRevokePending(false)
    }
  }

  const quotaText = (key: ApiKey) => (
    <>
      <b className="num">
        <MoneyText value={key.quota_used} currency={meta.currency} variant="spend" />
      </b>
      <span>
        {t('keys.quotaUsed')} ·{' '}
        {key.quota_limit ? formatMoney(key.quota_limit) : t('keys.unlimitedQuota')}
      </span>
    </>
  )

  // 「不过期」只读服务端给的布尔：NULL 与远期值两种落库表示在页面收成一个答案。
  const expiresText = (key: ApiKey) =>
    key.never_expires ? t('keys.noExpiry') : formatDateTime(key.expires_at, meta.timezone)

  const rateText = (key: ApiKey) =>
    key.rate_limit_rpm === null
      ? t('status.unlimited')
      : `${key.rate_limit_rpm} ${t('keys.perMinute')}`

  const actions = (key: ApiKey, compact = false) => {
    if (key.status === 'revoked') return <span className="tiny">{t('keys.revokedHint')}</span>
    return (
      <>
        <Button
          variant="ghost"
          size="sm"
          loading={busyId === key.id}
          icon={compact ? undefined : <IconPower size={14} />}
          onClick={() => toggle(key)}
        >
          {key.status === 'active' ? t('keys.disable') : t('keys.enable')}
        </Button>
        <Button
          variant="danger"
          size="sm"
          icon={compact ? undefined : <IconTrash size={14} />}
          onClick={() => {
            setRevoking(key)
            setDetail(null)
          }}
        >
          {t('keys.revoke')}
        </Button>
      </>
    )
  }

  return (
    <>
      <PageHead
        title={t('keys.title')}
        subtitle={t('keys.subtitle')}
        actions={
          <Button icon={<IconPlus size={15} />} onClick={openCreate}>
            {t('keys.create')}
          </Button>
        }
      />

      {(error || actionError) && (
        <Alert tone="bad" title={error ? t('keys.loadFailed') : actionError}>
          {error && (
            <Button variant="secondary" size="sm" onClick={reload} style={{ marginTop: 8 }}>
              {t('common.retry')}
            </Button>
          )}
        </Alert>
      )}

      {loading && !data && (
        <div className="card panel">
          {[0, 1, 2].map((row) => (
            <Skeleton key={row} height={18} />
          ))}
        </div>
      )}

      {data && keys.length === 0 && (
        <div className="card">
          <Empty
            icon={<IconKey size={20} />}
            title={t('keys.emptyTitle')}
            body={t('keys.emptyBody')}
            action={
              <Button icon={<IconPlus size={15} />} onClick={openCreate}>
                {t('keys.emptyAction')}
              </Button>
            }
          />
        </div>
      )}

      {keys.length > 0 && (
        <>
          <section className="card table table-desktop" aria-label={t('keys.title')}>
            <div className="thead keys-head">
              <span>{t('keys.colName')}</span>
              <span>{t('keys.colKey')}</span>
              <span>{t('keys.colQuota')}</span>
              <span>{t('keys.colRate')}</span>
              <span>{t('keys.colStatus')}</span>
              <span className="amount">{t('keys.colActions')}</span>
            </div>
            {keys.map((key) => (
              <div className="trow keys-row" key={key.id}>
                <div className="keyname">
                  <b>{key.name || t('common.none')}</b>
                  <span className="num">{formatDate(key.created_at, meta.timezone)}</span>
                </div>
                <span className="keyval mono">{key.masked}</span>
                <div className="keyname">{quotaText(key)}</div>
                <span className="muted-cell num">{rateText(key)}</span>
                <span>
                  <StatusPill status={key.status} />
                </span>
                <div className="actions">{actions(key)}</div>
              </div>
            ))}
          </section>

          <section className="cards-mobile" aria-label={t('keys.title')}>
            {keys.map((key) => (
              <button
                key={key.id}
                className="mcard"
                onClick={() => setDetail(key)}
                aria-label={t('keys.actionsFor', { name: key.name || key.masked })}
              >
                <div className="nm">{key.name || t('common.none')}</div>
                <span className="st">
                  <StatusPill status={key.status} />
                </span>
                <div className="kv mono">{key.masked}</div>
                <IconChevronRight className="chev" size={18} />
              </button>
            ))}
          </section>
        </>
      )}

      {creating && (
        <Modal
          title={t('keys.createTitle')}
          description={t('keys.createBody')}
          onClose={requestClose}
          footer={
            <>
              <Button variant="secondary" onClick={requestClose}>
                {t('common.cancel')}
              </Button>
              <Button type="submit" form="create-key-form" loading={submitting}>
                {t('keys.create')}
              </Button>
            </>
          }
        >
          <form id="create-key-form" className="stack" onSubmit={submit}>
            {formError && <Alert tone="bad">{formError}</Alert>}
            <div className="field">
              <label htmlFor="key-name">{t('keys.name')}</label>
              <input
                id="key-name"
                className="input"
                value={form.name}
                placeholder={t('keys.namePlaceholder')}
                onChange={(event) => setForm({ ...form, name: event.target.value })}
              />
              <span className="hint">{t('keys.nameOptional')}</span>
            </div>
            <div className="form-grid">
              <div className="field">
                <label htmlFor="key-quota">{t('keys.quotaLimit')}</label>
                <div className="input-affix">
                  {currencySymbol && <span className="affix">{currencySymbol}</span>}
                  <input
                    id="key-quota"
                    className="input"
                    inputMode="decimal"
                    value={form.quota}
                    placeholder="0.00000000"
                    onChange={(event) => setForm({ ...form, quota: event.target.value })}
                  />
                </div>
                <span className="hint">{t('keys.quotaHint')}</span>
              </div>
              <div className="field">
                <label htmlFor="key-rate">{t('keys.rateLimit')}</label>
                <input
                  id="key-rate"
                  className="input"
                  inputMode="numeric"
                  value={form.rate}
                  placeholder="60"
                  onChange={(event) => setForm({ ...form, rate: event.target.value })}
                />
                <div className="preset-row" role="group" aria-label={t('keys.ratePresets')}>
                  {RATE_PRESETS.map((value) => (
                    <Button
                      key={value}
                      type="button"
                      variant="secondary"
                      size="sm"
                      className="preset-btn"
                      aria-pressed={form.rate === String(value)}
                      aria-label={`${value} ${t('keys.perMinute')}`}
                      onClick={() => setForm((previous) => ({ ...previous, rate: String(value) }))}
                    >
                      {value}
                    </Button>
                  ))}
                </div>
                <span className="hint">{t('keys.rateHint')}</span>
              </div>
            </div>
            <div className="field">
              <label htmlFor="key-expires">{t('keys.expiresAt')}</label>
              <input
                id="key-expires"
                type="datetime-local"
                className="input"
                value={form.expires}
                onChange={(event) => {
                  setExpiryPreset(null)
                  setForm((previous) => ({ ...previous, expires: event.target.value }))
                }}
              />
              <div className="preset-row" role="group" aria-label={t('keys.expiryPresets')}>
                {EXPIRY_PRESETS.map((preset) => (
                  <Button
                    key={preset.key}
                    type="button"
                    variant="secondary"
                    size="sm"
                    className="preset-btn"
                    aria-pressed={expiryPreset === preset.key}
                    onClick={() => applyExpiryPreset(preset)}
                  >
                    {t(preset.key)}
                  </Button>
                ))}
              </div>
              <span className="hint">{t('keys.expiresHint')}</span>
            </div>
          </form>
        </Modal>
      )}

      {created && (
        <Modal
          title={t('keys.createdTitle')}
          description={t('keys.createdBody')}
          onClose={() => setCreated(null)}
          footer={
            <>
              <CopyButton
                text={created.secret}
                label={t('keys.copyKey')}
                className="btn secondary"
              />
              <Button onClick={() => setCreated(null)}>{t('keys.savedIt')}</Button>
            </>
          }
        >
          <div className="secret mono">{created.secret}</div>
          <div className="sheet-rows">
            <div>
              <span>{t('keys.name')}</span>
              <b>{created.key.name || t('common.none')}</b>
            </div>
            <div>
              <span>{t('keys.colQuota')}</span>
              <b>
                {created.key.quota_limit
                  ? formatMoney(created.key.quota_limit)
                  : t('keys.unlimitedQuota')}
              </b>
            </div>
            <div>
              <span>{t('keys.colRate')}</span>
              <b>{rateText(created.key)}</b>
            </div>
          </div>
        </Modal>
      )}

      {revoking && (
        <Modal
          sheetSize="sm"
          title={t('keys.revokeTitle')}
          description={`${revoking.name || ''} · ${revoking.masked}`}
          onClose={() => setRevoking(null)}
          footer={
            <>
              <Button variant="secondary" onClick={() => setRevoking(null)}>
                {t('common.cancel')}
              </Button>
              <Button variant="danger" loading={revokePending} onClick={revoke}>
                {t('keys.revokeConfirm')}
              </Button>
            </>
          }
        >
          <p className="sub">{t('keys.revokeBody')}</p>
        </Modal>
      )}

      {detail && (
        <Modal
          sheet
          title={t('keys.detailTitle')}
          description={detail.name || detail.masked}
          onClose={() => setDetail(null)}
          footer={
            <Button variant="secondary" onClick={() => setDetail(null)}>
              {t('common.close')}
            </Button>
          }
        >
          <div className="sheet-rows">
            <div>
              <span>{t('keys.colKey')}</span>
              <b className="mono">{detail.masked}</b>
            </div>
            <div>
              <span>{t('keys.colQuota')}</span>
              <b>
                {formatSpend(detail.quota_used)} /{' '}
                {detail.quota_limit ? formatMoney(detail.quota_limit) : t('status.unlimited')}
              </b>
            </div>
            <div>
              <span>{t('keys.colRate')}</span>
              <b>{rateText(detail)}</b>
            </div>
            <div>
              <span>{t('keys.colStatus')}</span>
              <b>
                <StatusPill status={detail.status} />
              </b>
            </div>
            <div>
              <span>{t('keys.createdAt')}</span>
              <b className="num">{formatDateTime(detail.created_at, meta.timezone)}</b>
            </div>
            <div>
              <span>{t('keys.lastUsed')}</span>
              <b className="num">{formatDateTime(detail.last_used_at, meta.timezone)}</b>
            </div>
            <div>
              <span>{t('keys.expiresAt')}</span>
              <b className="num">{expiresText(detail)}</b>
            </div>
          </div>
          <div className="form-actions">{actions(detail, true)}</div>
        </Modal>
      )}

      {confirm.dialog}
      {unsavedGuard}
    </>
  )
}
