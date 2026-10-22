import { useState, type FormEvent } from 'react'
import { api } from '../../api'
import type { ChannelAlias } from '../../api/types'
import { useI18n } from '../../i18n'
import { useAsync } from '../../hooks/useAsync'
import { useIsCompact } from '../../hooks/useMediaQuery'
import {
  Button,
  Empty,
  Modal,
  PageHead,
  Skeleton,
  useToast,
} from '../../components/ui'
import { ActionSheet } from '../../components/ActionSheet'
import { MobileCard, MobileList } from '../../components/MobileList'
import { IconPlus, IconTrash } from '../../components/icons'
import { Field, InputError, OpsLoadError } from './common'
import { useConfirm } from './common-helpers'
import { isDirty, useCloseGuard } from './guard'

interface AliasForm {
  channel_id: string
  upstream_model: string
  canonical_name: string
  overrides: string
}

const EMPTY_FORM: AliasForm = { channel_id: '', upstream_model: '', canonical_name: '', overrides: '{}' }

function overridesOf(alias: ChannelAlias): string {
  return JSON.stringify(alias.overrides ?? {}, null, 2)
}

/** 别名：渠道上的上游模型名 → 规范模型名。只有 admin 视图有这一页。 */
export function OpsAliases() {
  const { t } = useI18n()
  const { show: toast } = useToast()
  const { data, error, loading, reload } = useAsync(() => api.admin.listAliases(), [])
  const channels = useAsync(() => api.admin.listChannels(), [])
  const models = useAsync(() => api.admin.listModels(), [])
  const confirm = useConfirm()

  const compact = useIsCompact()
  const [editing, setEditing] = useState<ChannelAlias | null>(null)
  // 窄屏上整张卡片是一个按钮，编辑与删除放在底部弹出的菜单里。
  const [actions, setActions] = useState<ChannelAlias | null>(null)
  const [creating, setCreating] = useState(false)
  const [form, setForm] = useState<AliasForm>(EMPTY_FORM)
  // 打开时的那份值：改动过的话，关窗前要再问一句。
  const [initialForm, setInitialForm] = useState<AliasForm>(EMPTY_FORM)
  const [formError, setFormError] = useState<string | null>(null)

  const openCreate = () => {
    const next = { ...EMPTY_FORM, channel_id: String(channels.data?.[0]?.id ?? '') }
    setForm(next)
    setInitialForm(next)
    setFormError(null)
    setCreating(true)
  }

  const openEdit = (alias: ChannelAlias) => {
    const next = {
      channel_id: String(alias.channel_id),
      upstream_model: alias.upstream_model,
      canonical_name: alias.canonical_name,
      overrides: overridesOf(alias),
    }
    setForm(next)
    setInitialForm(next)
    setFormError(null)
    setEditing(alias)
  }

  const closeForm = () => {
    setCreating(false)
    setEditing(null)
    setFormError(null)
  }

  const requestClose = useCloseGuard(confirm, isDirty(form, initialForm), closeForm)

  const submit = (event: FormEvent) => {
    event.preventDefault()
    if (!/^\d+$/.test(form.channel_id.trim()) || Number(form.channel_id) <= 0) {
      return setFormError(t('ops.alias.channelRequired'))
    }
    if (!form.upstream_model.trim()) return setFormError(t('ops.alias.upstreamRequired'))
    if (!form.canonical_name.trim()) return setFormError(t('ops.alias.canonicalRequired'))
    let overrides: Record<string, unknown>
    try {
      const parsed: unknown = JSON.parse(form.overrides || '{}')
      if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
        return setFormError(t('ops.alias.overridesObject'))
      }
      overrides = parsed as Record<string, unknown>
    } catch {
      return setFormError(t('ops.alias.overridesJSON'))
    }

    if (editing) {
      confirm.request({
        title: t('ops.confirmSave'),
        body: t('ops.alias.saveConfirm', { name: editing.upstream_model }),
        label: t('common.save'),
        run: async () => {
          await api.admin.updateAlias(editing.id, {
            upstream_model: form.upstream_model.trim(),
            overrides,
          })
          toast(t('ops.saved'))
          closeForm()
          reload()
        },
      })
      return
    }

    confirm.request({
      title: t('ops.confirmCreate'),
      body: t('ops.alias.createConfirm', { name: form.upstream_model.trim() }),
      label: t('ops.create'),
      run: async () => {
        await api.admin.createAlias({
          channel_id: Number(form.channel_id),
          upstream_model: form.upstream_model.trim(),
          canonical_name: form.canonical_name.trim(),
          overrides,
        })
        toast(t('ops.created'))
        closeForm()
        reload()
      },
    })
  }

  const remove = (alias: ChannelAlias) => {
    confirm.request({
      title: t('ops.delete'),
      body: t('ops.alias.deleteConfirm', { name: alias.upstream_model }),
      label: t('ops.delete'),
      danger: true,
      run: async () => {
        await api.admin.deleteAlias(alias.id)
        toast(t('ops.deleted'))
        reload()
      },
    })
  }

  const aliases = data ?? []
  const channelName = (id: number) =>
    channels.data?.find((channel) => channel.id === id)?.name ?? `#${id}`

  return (
    <>
      <PageHead
        title={t('ops.alias.title')}
        subtitle={t('ops.alias.subtitle')}
        actions={
          <Button icon={<IconPlus size={15} />} onClick={openCreate}>
            {t('ops.alias.create')}
          </Button>
        }
      />

      {error && <OpsLoadError error={error} onRetry={reload} />}

      {loading && !data && (
        <div className="card panel">
          {[0, 1, 2].map((row) => (
            <Skeleton key={row} height={18} />
          ))}
        </div>
      )}

      {data && aliases.length === 0 && (
        <div className="card">
          <Empty
            title={t('ops.alias.emptyTitle')}
            body={t('ops.alias.emptyBody')}
            action={
              <Button icon={<IconPlus size={15} />} onClick={openCreate}>
                {t('ops.alias.create')}
              </Button>
            }
          />
        </div>
      )}

      {aliases.length > 0 && compact && (
        <MobileList label={t('ops.alias.title')}>
          {aliases.map((alias) => (
            <MobileCard
              key={alias.id}
              title={alias.upstream_model}
              subtitle={`${channelName(alias.channel_id)} → ${alias.canonical_name}`}
              fields={[
                {
                  label: t('ops.alias.overrides'),
                  value:
                    Object.keys(alias.overrides ?? {}).length === 0
                      ? t('common.none')
                      : JSON.stringify(alias.overrides),
                  mono: true,
                },
              ]}
              onPress={() => setActions(alias)}
              pressLabel={t('ops.cardActionsFor', { name: alias.upstream_model })}
            />
          ))}
        </MobileList>
      )}

      {aliases.length > 0 && !compact && (
        <section className="card table ops-table" aria-label={t('ops.alias.title')}>
          <div className="thead alias-head">
            <span>{t('ops.alias.channel')}</span>
            <span>{t('ops.alias.upstream')}</span>
            <span>{t('ops.alias.canonical')}</span>
            <span>{t('ops.alias.overrides')}</span>
            <span className="amount">{t('ops.colActions')}</span>
          </div>
          {aliases.map((alias) => (
            <div className="trow alias-row" key={alias.id}>
              <span className="muted-cell">{channelName(alias.channel_id)}</span>
              <span className="mono cell-ellipsis">{alias.upstream_model}</span>
              <span className="mono cell-ellipsis">{alias.canonical_name}</span>
              <span className="tiny cell-ellipsis">
                {Object.keys(alias.overrides ?? {}).length === 0
                  ? t('common.none')
                  : JSON.stringify(alias.overrides)}
              </span>
              <div className="actions">
                <Button variant="ghost" size="sm" onClick={() => openEdit(alias)}>
                  {t('ops.edit')}
                </Button>
                <Button
                  variant="danger"
                  size="sm"
                  icon={<IconTrash size={14} />}
                  onClick={() => remove(alias)}
                >
                  {t('ops.delete')}
                </Button>
              </div>
            </div>
          ))}
        </section>
      )}

      {actions && (
        <ActionSheet
          title={actions.upstream_model}
          description={`${channelName(actions.channel_id)} → ${actions.canonical_name}`}
          items={[
            { key: 'edit', label: t('ops.edit'), onSelect: () => openEdit(actions) },
            {
              key: 'delete',
              label: t('ops.delete'),
              danger: true,
              onSelect: () => remove(actions),
            },
          ]}
          onClose={() => setActions(null)}
        />
      )}

      {(creating || editing) && (
        <Modal
          title={editing ? t('ops.alias.editTitle') : t('ops.alias.createTitle')}
          onClose={requestClose}
          footer={
            <>
              <Button variant="secondary" onClick={requestClose}>
                {t('common.cancel')}
              </Button>
              <Button type="submit" form="alias-form">
                {editing ? t('common.save') : t('ops.create')}
              </Button>
            </>
          }
        >
          <form id="alias-form" onSubmit={submit} style={{ display: 'grid', gap: 12 }}>
            <Field label={t('ops.alias.channel')} hint={t('ops.alias.channelHint')}>
              <select
                className="input"
                value={form.channel_id}
                disabled={Boolean(editing)}
                onChange={(event) => setForm({ ...form, channel_id: event.target.value })}
              >
                {!form.channel_id && <option value="">{t('common.none')}</option>}
                {form.channel_id &&
                  !channels.data?.some((channel) => String(channel.id) === form.channel_id) && (
                    <option value={form.channel_id}>#{form.channel_id}</option>
                  )}
                {channels.data?.map((channel) => (
                  <option key={channel.id} value={String(channel.id)}>
                    {channel.name} (#{channel.id})
                  </option>
                ))}
              </select>
            </Field>
            <Field label={t('ops.alias.upstream')}>
              <input
                className="input"
                value={form.upstream_model}
                onChange={(event) => setForm({ ...form, upstream_model: event.target.value })}
              />
            </Field>
            <Field
              label={t('ops.alias.canonical')}
              hint={models.data?.map((model) => model.canonical_name).join(', ')}
            >
              <input
                className="input"
                value={form.canonical_name}
                readOnly={Boolean(editing)}
                onChange={(event) => setForm({ ...form, canonical_name: event.target.value })}
              />
            </Field>
            <Field label={t('ops.alias.overrides')} hint={t('ops.alias.overridesHint')}>
              <textarea
                className="input mono"
                value={form.overrides}
                onChange={(event) => setForm({ ...form, overrides: event.target.value })}
              />
            </Field>
            <InputError message={formError} />
          </form>
        </Modal>
      )}

      {confirm.dialog}
    </>
  )
}
