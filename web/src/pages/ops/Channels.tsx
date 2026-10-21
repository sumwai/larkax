import { useState, type FormEvent } from 'react'
import { api } from '../../api'
import type {
  Channel,
  ChannelCreateInput,
  ChannelOpsClient,
  ChannelUpdateInput,
} from '../../api/types'
import { useI18n } from '../../i18n'
import { useSite } from '../../site/SiteProvider'
import { useAsync } from '../../hooks/useAsync'
import { useIsCompact } from '../../hooks/useMediaQuery'
import { formatDateTime } from '../../lib/format'
import {
  Alert,
  Button,
  Empty,
  Modal,
  PageHead,
  Skeleton,
  StatusPill,
  useToast,
} from '../../components/ui'
import { ActionSheet } from '../../components/ActionSheet'
import { MobileCard, MobileList } from '../../components/MobileList'
import { IconPlus } from '../../components/icons'
import {
  Field,
  InputError,
  OpsLoadError,
  linesToList,
  listToLines,
  useConfirm,
} from './common'
import { isDirty, useCloseGuard } from './guard'

interface ChannelForm {
  name: string
  type: string
  base_url: string
  credential: string
  models: string
  weight: string
  priority: string
  owner: string
}

const EMPTY_FORM: ChannelForm = {
  name: '',
  type: 'openai',
  base_url: '',
  credential: '',
  models: '',
  weight: '1',
  priority: '0',
  owner: '0',
}

/**
 * 渠道列表与编辑。
 *
 * 两个视图共用：admin 看全部并可指定归属，merchant 只看得到自己的渠道
 * （过滤与越权判定都在服务端，这里不自己判断「是不是我的」）。
 */
export function OpsChannels({ scope }: { scope: 'admin' | 'merchant' }) {
  const { t } = useI18n()
  const { meta } = useSite()
  const { show: toast } = useToast()
  const client = api[scope]
  const { data, error, loading, reload } = useAsync(() => client.listChannels(), [scope])
  // 模型提示只在 admin 视图有：merchant 前缀下没有模型目录，不去 admin 那边凑。
  const catalog = useAsync(
    () => (scope === 'admin' ? api.admin.listModels() : Promise.resolve([])),
    [scope],
  )
  const confirm = useConfirm()

  const compact = useIsCompact()
  const [viewing, setViewing] = useState<Channel | null>(null)
  // 窄屏上整张卡片是一个按钮，动作放在从底部弹出的菜单里。
  const [actions, setActions] = useState<Channel | null>(null)
  const [editing, setEditing] = useState<Channel | null>(null)
  const [creating, setCreating] = useState(false)
  const [form, setForm] = useState<ChannelForm>(EMPTY_FORM)
  // 打开时的那份值：改动过的话，关窗前要再问一句。
  const [initialForm, setInitialForm] = useState<ChannelForm>(EMPTY_FORM)
  const [formError, setFormError] = useState<string | null>(null)

  const openCreate = () => {
    setForm(EMPTY_FORM)
    setInitialForm(EMPTY_FORM)
    setFormError(null)
    setCreating(true)
  }

  const openEdit = (channel: Channel) => {
    const next: ChannelForm = {
      name: channel.name,
      type: channel.type,
      base_url: channel.base_url,
      credential: '',
      models: listToLines(channel.models),
      weight: String(channel.weight),
      priority: String(channel.priority),
      owner: String(channel.owner_user_id),
    }
    setForm(next)
    setInitialForm(next)
    setFormError(null)
    setEditing(channel)
  }

  const closeForm = () => {
    setCreating(false)
    setEditing(null)
    setFormError(null)
  }

  const requestClose = useCloseGuard(confirm, isDirty(form, initialForm), closeForm)

  const submit = (event: FormEvent) => {
    event.preventDefault()
    const models = linesToList(form.models)
    if (!form.name.trim()) return setFormError(t('ops.channel.nameRequired'))
    if (!form.type.trim()) return setFormError(t('ops.channel.typeRequired'))
    if (!form.base_url.trim()) return setFormError(t('ops.channel.baseURLRequired'))
    if (!editing && !form.credential.trim()) return setFormError(t('ops.channel.credentialRequired'))
    if (models.length === 0) return setFormError(t('ops.channel.modelsRequired'))
    if (!/^\d+$/.test(form.weight.trim())) return setFormError(t('ops.invalidInteger'))
    if (!/^-?\d+$/.test(form.priority.trim())) return setFormError(t('ops.invalidInteger'))
    if (!/^\d+$/.test(form.owner.trim())) return setFormError(t('ops.invalidInteger'))

    if (editing) {
      const input: ChannelUpdateInput = {
        name: form.name.trim(),
        type: form.type.trim(),
        base_url: form.base_url.trim(),
        models,
        weight: Number(form.weight),
        priority: Number(form.priority),
      }
      if (form.credential.trim()) input.credential = form.credential.trim()
      confirm.request({
        title: t('ops.confirmSave'),
        body: t('ops.channel.saveConfirm', { name: form.name.trim() }),
        label: t('common.save'),
        run: async () => {
          await client.updateChannel(editing.id, input)
          toast(t('ops.saved'))
          closeForm()
          reload()
        },
      })
      return
    }

    const input: ChannelCreateInput = {
      name: form.name.trim(),
      type: form.type.trim(),
      base_url: form.base_url.trim(),
      credential: form.credential.trim(),
      models,
      weight: Number(form.weight),
      priority: Number(form.priority),
    }
    if (scope === 'admin') input.owner_user_id = Number(form.owner)
    confirm.request({
      title: t('ops.confirmCreate'),
      body: t('ops.channel.createConfirm', { name: form.name.trim() }),
      label: t('ops.create'),
      run: async () => {
        await client.createChannel(input)
        toast(t('ops.created'))
        closeForm()
        reload()
      },
    })
  }

  const toggleStatus = (channel: Channel) => {
    const next = channel.status === 'enabled' ? 'disabled' : 'enabled'
    confirm.request({
      title: t(next === 'enabled' ? 'ops.channel.enable' : 'ops.channel.disable'),
      body: t(next === 'enabled' ? 'ops.channel.enableConfirm' : 'ops.channel.disableConfirm', {
        name: channel.name,
      }),
      label: t('common.confirm'),
      danger: next === 'disabled',
      run: async () => {
        await client.updateChannel(channel.id, { status: next })
        toast(t('ops.saved'))
        reload()
      },
    })
  }

  const channels = data ?? []
  const knownModels = catalog.data?.map((model) => model.canonical_name).join(', ')

  return (
    <>
      <PageHead
        title={t('ops.channel.title')}
        subtitle={scope === 'merchant' ? t('ops.channel.merchantSubtitle') : t('ops.channel.subtitle')}
        actions={
          <Button icon={<IconPlus size={15} />} onClick={openCreate}>
            {t('ops.channel.create')}
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

      {data && channels.length === 0 && (
        <div className="card">
          <Empty
            title={t('ops.channel.emptyTitle')}
            body={t('ops.channel.emptyBody')}
            action={
              <Button icon={<IconPlus size={15} />} onClick={openCreate}>
                {t('ops.channel.create')}
              </Button>
            }
          />
        </div>
      )}

      {channels.length > 0 && compact && (
        <MobileList label={t('ops.channel.title')}>
          {channels.map((channel) => (
            <MobileCard
              key={channel.id}
              title={channel.name}
              subtitle={channel.base_url}
              status={<StatusPill status={channel.status} />}
              fields={[
                { label: t('ops.channel.colType'), value: channel.type },
                {
                  label: t('ops.channel.colModels'),
                  value: t('ops.channel.modelsCount', { count: channel.models.length }),
                },
              ]}
              onPress={() => setActions(channel)}
              pressLabel={t('ops.cardActionsFor', { name: channel.name })}
            />
          ))}
        </MobileList>
      )}

      {channels.length > 0 && !compact && (
        <section className="card table ops-table" aria-label={t('ops.channel.title')}>
          <div className="thead channel-head" style={{ display: 'grid' }}>
            <span>{t('ops.channel.colName')}</span>
            <span>{t('ops.channel.colType')}</span>
            <span>{t('ops.channel.colModels')}</span>
            <span>{t('ops.channel.colStatus')}</span>
            <span className="amount">{t('ops.colActions')}</span>
          </div>
          {channels.map((channel) => (
            <div className="trow channel-row" key={channel.id}>
              <div className="keyname">
                <b>{channel.name}</b>
                <span className="mono tiny">{channel.base_url}</span>
              </div>
              <span className="muted-cell">{channel.type}</span>
              <span className="tiny">{t('ops.channel.modelsCount', { count: channel.models.length })}</span>
              <span>
                <StatusPill status={channel.status} />
              </span>
              <div className="actions">
                <Button variant="ghost" size="sm" onClick={() => setViewing(channel)}>
                  {t('ops.view')}
                </Button>
                <Button variant="ghost" size="sm" onClick={() => openEdit(channel)}>
                  {t('ops.edit')}
                </Button>
                <Button variant="ghost" size="sm" onClick={() => toggleStatus(channel)}>
                  {t(channel.status === 'enabled' ? 'ops.channel.disable' : 'ops.channel.enable')}
                </Button>
              </div>
            </div>
          ))}
        </section>
      )}

      {actions && (
        <ActionSheet
          title={actions.name}
          description={actions.base_url}
          items={[
            {
              key: 'view',
              label: t('ops.view'),
              onSelect: () => setViewing(actions),
            },
            {
              key: 'edit',
              label: t('ops.edit'),
              onSelect: () => openEdit(actions),
            },
            {
              key: 'toggle',
              label: t(actions.status === 'enabled' ? 'ops.channel.disable' : 'ops.channel.enable'),
              description: t(
                actions.status === 'enabled'
                  ? 'ops.channel.disableConfirm'
                  : 'ops.channel.enableConfirm',
                { name: actions.name },
              ),
              danger: actions.status === 'enabled',
              onSelect: () => toggleStatus(actions),
            },
          ]}
          onClose={() => setActions(null)}
        />
      )}

      {(creating || editing) && (
        <Modal
          sheetSize="lg"
          title={editing ? t('ops.channel.editTitle') : t('ops.channel.createTitle')}
          description={
            editing
              ? t('ops.channel.hint', {
                  hint: editing.credential_hint || t('common.none'),
                  keys: editing.credential_keys.join(', ') || t('common.none'),
                })
              : undefined
          }
          onClose={requestClose}
          footer={
            <>
              <Button variant="secondary" onClick={requestClose}>
                {t('common.cancel')}
              </Button>
              <Button type="submit" form="channel-form">
                {editing ? t('common.save') : t('ops.create')}
              </Button>
            </>
          }
        >
          <form id="channel-form" onSubmit={submit} style={{ display: 'grid', gap: 12 }}>
            {editing?.credential_error && <Alert tone="bad" title={editing.credential_error} />}
            <Field label={t('ops.channel.name')}>
              <input
                className="input"
                value={form.name}
                onChange={(event) => setForm({ ...form, name: event.target.value })}
              />
            </Field>
            <Field label={t('ops.channel.type')}>
              <input
                className="input"
                value={form.type}
                onChange={(event) => setForm({ ...form, type: event.target.value })}
              />
            </Field>
            <Field label={t('ops.channel.baseURL')}>
              <input
                className="input"
                value={form.base_url}
                onChange={(event) => setForm({ ...form, base_url: event.target.value })}
              />
            </Field>
            <Field
              label={t('ops.channel.credential')}
              hint={editing ? t('ops.channel.credentialKeep') : undefined}
            >
              <input
                className="input"
                type="password"
                autoComplete="off"
                placeholder={t('ops.channel.credentialPlaceholder')}
                value={form.credential}
                onChange={(event) => setForm({ ...form, credential: event.target.value })}
              />
            </Field>
            <Field label={t('ops.channel.models')} hint={knownModels && t('ops.channel.knownModels', { models: knownModels })}>
              <textarea
                className="input"
                value={form.models}
                onChange={(event) => setForm({ ...form, models: event.target.value })}
              />
            </Field>
            <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 12 }}>
              <Field label={t('ops.channel.weight')}>
                <input
                  className="input"
                  value={form.weight}
                  onChange={(event) => setForm({ ...form, weight: event.target.value })}
                />
              </Field>
              <Field label={t('ops.channel.priority')}>
                <input
                  className="input"
                  value={form.priority}
                  onChange={(event) => setForm({ ...form, priority: event.target.value })}
                />
              </Field>
            </div>
            {scope === 'admin' && !editing && (
              <Field label={t('ops.channel.owner')} hint={t('ops.channel.ownerHint')}>
                <input
                  className="input"
                  value={form.owner}
                  onChange={(event) => setForm({ ...form, owner: event.target.value })}
                />
              </Field>
            )}
            <InputError message={formError} />
            {editing && (
              <span className="hint">
                {t('ops.channel.updatedAt')} {formatDateTime(editing.updated_at, meta.timezone)}
              </span>
            )}
          </form>
        </Modal>
      )}

      {viewing && (
        <ChannelDetail
          client={client}
          id={viewing.id}
          onClose={() => setViewing(null)}
          onEdit={() => {
            const channel = viewing
            setViewing(null)
            openEdit(channel)
          }}
        />
      )}

      {confirm.dialog}
    </>
  )
}

/**
 * 渠道详情。
 *
 * 走一次单条查询而不是拿列表里那一行：列表可能是一分钟前的快照，而详情页的
 * 用途就是「这条现在到底是什么状态」。取不到时显示服务端那句话。
 */
function ChannelDetail({
  client,
  id,
  onClose,
  onEdit,
}: {
  client: ChannelOpsClient
  id: number
  onClose: () => void
  onEdit: () => void
}) {
  const { t } = useI18n()
  const { meta } = useSite()
  const detail = useAsync(() => client.getChannel(id), [id])
  const channel = detail.data

  return (
    <Modal
      sheet
      title={t('ops.channel.detailTitle')}
      description={channel?.name}
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>
            {t('common.close')}
          </Button>
          {channel && <Button onClick={onEdit}>{t('ops.edit')}</Button>}
        </>
      }
    >
      {detail.loading && !channel && (
        <div className="card panel" style={{ border: 'none' }}>
          <Skeleton height={18} />
          <Skeleton height={18} />
        </div>
      )}
      {detail.error && <OpsLoadError error={detail.error} onRetry={detail.reload} />}
      {channel && (
        <div className="sheet-rows">
          <div>
            <span>{t('ops.channel.detailID')}</span>
            <b className="num">#{channel.id}</b>
          </div>
          <div>
            <span>{t('ops.channel.baseURL')}</span>
            <b className="mono">{channel.base_url}</b>
          </div>
          <div>
            <span>{t('ops.channel.type')}</span>
            <b>{channel.type}</b>
          </div>
          <div>
            <span>{t('ops.channel.colStatus')}</span>
            <b>
              <StatusPill status={channel.status} />
            </b>
          </div>
          <div>
            <span>{t('ops.channel.detailModels')}</span>
            <b>{channel.models.join(', ') || t('common.none')}</b>
          </div>
          <div>
            <span>{t('ops.channel.weight')}</span>
            <b className="num">{channel.weight}</b>
          </div>
          <div>
            <span>{t('ops.channel.priority')}</span>
            <b className="num">{channel.priority}</b>
          </div>
          <div>
            <span>{t('ops.channel.detailOwner')}</span>
            <b>
              {channel.owner_user_id === 0
                ? t('ops.channel.detailOwnerPlatform')
                : channel.owner_user_id}
            </b>
          </div>
          <div>
            <span>{t('ops.channel.credential')}</span>
            <b className="mono">{channel.credential_hint || t('common.none')}</b>
          </div>
          <div>
            <span>{t('ops.channel.detailCreatedAt')}</span>
            <b className="num">{formatDateTime(channel.created_at, meta.timezone)}</b>
          </div>
          <div>
            <span>{t('ops.channel.updatedAt')}</span>
            <b className="num">{formatDateTime(channel.updated_at, meta.timezone)}</b>
          </div>
        </div>
      )}
    </Modal>
  )
}
