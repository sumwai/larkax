import { useState, type FormEvent } from 'react'
import { api } from '../api'
import type { ApiKey, CreatedKey, KeyCreateInput } from '../api/types'
import { useI18n } from '../i18n'
import { useSite } from '../site/SiteProvider'
import { useAsync } from '../hooks/useAsync'
import { formatDate, formatDateTime, formatMoney, formatSpend } from '../lib/format'
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

interface CreateForm {
  name: string
  quota: string
  rate: string
  expires: string
}

const EMPTY_FORM: CreateForm = { name: '', quota: '', rate: '', expires: '' }

export function Keys() {
  const { t } = useI18n()
  const { meta } = useSite()
  const { show: toast } = useToast()
  const { data, error, loading, reload } = useAsync(() => api.listKeys({ limit: 100 }), [])

  const [creating, setCreating] = useState(false)
  const [form, setForm] = useState<CreateForm>(EMPTY_FORM)
  const [formError, setFormError] = useState<string | null>(null)
  const [submitting, setSubmitting] = useState(false)
  const [created, setCreated] = useState<CreatedKey | null>(null)
  const [revoking, setRevoking] = useState<ApiKey | null>(null)
  const [revokePending, setRevokePending] = useState(false)
  const [detail, setDetail] = useState<ApiKey | null>(null)
  const [busyId, setBusyId] = useState<number | null>(null)
  const [actionError, setActionError] = useState<string | null>(null)

  const keys = data?.items ?? []

  const openCreate = () => {
    setForm(EMPTY_FORM)
    setFormError(null)
    setCreating(true)
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
            <div className="thead keys-head" style={{ display: 'grid' }}>
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
          onClose={() => setCreating(false)}
          footer={
            <>
              <Button variant="secondary" onClick={() => setCreating(false)}>
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
                <input
                  id="key-quota"
                  className="input"
                  inputMode="decimal"
                  value={form.quota}
                  placeholder="0.00000000"
                  onChange={(event) => setForm({ ...form, quota: event.target.value })}
                />
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
                onChange={(event) => setForm({ ...form, expires: event.target.value })}
              />
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
              <b className="num">
                {detail.expires_at ? formatDateTime(detail.expires_at, meta.timezone) : t('keys.noExpiry')}
              </b>
            </div>
          </div>
          <div className="form-actions">{actions(detail, true)}</div>
        </Modal>
      )}
    </>
  )
}
