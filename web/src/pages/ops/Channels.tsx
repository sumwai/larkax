import { useEffect, useRef, useState, type FormEvent } from 'react'
import { ApiError, api } from '../../api'
import type {
  Channel,
  ChannelAdapter,
  ChannelCreateInput,
  ChannelOpsClient,
  ChannelProbeInput,
  ChannelProbeResult,
  ChannelUpdateInput,
} from '../../api/types'
import { useI18n } from '../../i18n'
import type { MessageKey } from '../../i18n/zh'
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
import { Field, InputError, OpsLoadError } from './common'
import { linesToList, listToLines, useConfirm } from './common-helpers'
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

type Translate = (key: MessageKey, vars?: Record<string, string | number>) => string

/**
 * 把一次探测的失败翻成一句人话。
 *
 * 502 底下几种成因的处置不同（连不上 / 太大 / 上游报错），状态码相同但要说的
 * 不是同一句话；504 是「慢」不是「坏」，与 502 分开写。
 *
 * 503 不给单一原因：服务端在这一格下有两种来源（本实例没配主密钥、探测用适配器
 * 装配失败），都只有状态码没有机器码，前端分不出来。所以这里说「稍后重试」并把
 * 两种可能并列，不替服务端断言是哪一种。
 */
function probeErrorText(error: unknown, t: Translate): string {
  if (!(error instanceof ApiError)) {
    const message = error instanceof Error ? error.message : String(error)
    return t('ops.channel.probeFailed', { message })
  }
  if (error.status === 502) {
    if (error.code === 'upstream_unreachable') return t('ops.channel.probeUnreachable')
    if (error.code === 'upstream_response_too_large') return t('ops.channel.probeTooLarge')
    return t('ops.channel.probeUpstream', { message: error.message })
  }
  switch (error.status) {
    case 400:
      return t('ops.channel.probeBadRequest', { message: error.message })
    case 404:
      return t('ops.channel.probeNotFound')
    case 503:
      return t('ops.channel.probeUnavailable')
    case 504:
      return t('ops.channel.probeTimeout')
    default:
      return t('ops.channel.probeFailed', { message: error.message })
  }
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
  // 适配器清单只给管理员：商家配的是自己的上游账号，不选平台适配器，这一页
  // 在商家视图下仍是手填类型。
  const adapters = useAsync(
    () => (scope === 'admin' ? client.listAdapters() : Promise.resolve<ChannelAdapter[]>([])),
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
  // 探测：清单、失败文案、进行中与 429 的倒计时。清单只用来渲染可勾选项，
  // 提交体仍由 `form.models` 这一份文本决定（见 toggleModel）。
  const [probe, setProbe] = useState<ChannelProbeResult | null>(null)
  const [probeError, setProbeError] = useState<string | null>(null)
  const [probing, setProbing] = useState(false)
  const [cooldown, setCooldown] = useState(0)
  // 429 但没给 `Retry-After`：只知道「现在别来」，不知道等多久。这时禁用按钮、
  // 不编一个秒数，重开表单才解除——编出来的倒计时会和真实限制对不上。
  const [rateLimited, setRateLimited] = useState(false)

  useEffect(() => {
    if (cooldown <= 0) return
    const timer = setTimeout(() => setCooldown((seconds) => seconds - 1), 1000)
    return () => clearTimeout(timer)
  }, [cooldown])

  // 每发起一次探测、每重置一次都递增；回来的结果只有序号还对得上才写进状态。
  // 否则「探测途中关掉表单、再打开另一张」会把上一次的结果画到新表单上。
  const probeSeq = useRef(0)

  const resetProbe = () => {
    probeSeq.current += 1
    setProbe(null)
    setProbeError(null)
    setProbing(false)
    setCooldown(0)
    setRateLimited(false)
  }

  const openCreate = () => {
    setForm(EMPTY_FORM)
    setInitialForm(EMPTY_FORM)
    setFormError(null)
    resetProbe()
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
    resetProbe()
    setEditing(channel)
  }

  const closeForm = () => {
    setCreating(false)
    setEditing(null)
    setFormError(null)
    resetProbe()
  }

  const requestClose = useCloseGuard(confirm, isDirty(form, initialForm), closeForm)

  // 勾选状态就是 `form.models` 里的那些行：勾上写一行、取消删一行。
  // 再次探测只替换可选项清单（`probe`），不碰 `form.models`——已勾选因此不会
  // 因为清单变了而丢；手填的行也在这同一份文本里。
  const selectedModels = linesToList(form.models)
  const toggleModel = (model: string, checked: boolean) => {
    const next = checked
      ? selectedModels.includes(model)
        ? selectedModels
        : [...selectedModels, model]
      : selectedModels.filter((item) => item !== model)
    setForm({ ...form, models: listToLines(next) })
  }

  const runProbe = async () => {
    const baseURL = form.base_url.trim()
    if (!baseURL) return
    const seq = ++probeSeq.current
    setProbing(true)
    setProbeError(null)
    // 编辑态带上 channel_id：类型取库里那一行（界面也把类型锁住了），地址与
    // 凭证用表单上的新值覆盖。创建态没有 id，类型就是选择器里选的那个。
    const input: ChannelProbeInput = editing
      ? { channel_id: editing.id, base_url: baseURL }
      : { type: form.type.trim(), base_url: baseURL }
    if (form.credential.trim()) input.credential = form.credential.trim()
    try {
      const result = await client.probeModels(input)
      if (seq === probeSeq.current) setProbe(result)
    } catch (error) {
      if (seq !== probeSeq.current) return
      setProbe(null)
      // 429 单独处理：带 `Retry-After` 就按服务端给的秒数倒计时；没带就只禁用、
      // 说明「请稍后再试」，不编一个数——倒计时必须是服务端说的那个。
      if (error instanceof ApiError && error.status === 429) {
        if (error.retryAfter !== null) setCooldown(error.retryAfter)
        else setRateLimited(true)
        return
      }
      setProbeError(probeErrorText(error, t))
    } finally {
      if (seq === probeSeq.current) setProbing(false)
    }
  }

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
          <div className="thead channel-head">
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
            {scope === 'admin' && (adapters.error || adapters.data?.length === 0) && (
              <Alert tone="warn" title={t('ops.channel.adaptersUnavailable')} />
            )}
            {scope === 'admin' && !editing && adapters.data && adapters.data.length > 0 ? (
              <Field label={t('ops.channel.type')}>
                <select
                  className="input"
                  value={form.type}
                  onChange={(event) => setForm({ ...form, type: event.target.value })}
                >
                  {adapters.data.map((adapter) => (
                    <option key={adapter.type} value={adapter.type}>
                      {t('ops.channel.adapterOption', {
                        type: adapter.type,
                        endpoint: adapter.endpoint,
                      })}
                    </option>
                  ))}
                  {form.type && !adapters.data.some((adapter) => adapter.type === form.type) && (
                    <option value={form.type}>{form.type}</option>
                  )}
                </select>
              </Field>
            ) : (
              <Field
                label={t('ops.channel.type')}
                hint={editing ? t('ops.channel.typeLocked') : undefined}
              >
                <input
                  className="input"
                  value={form.type}
                  disabled={Boolean(editing)}
                  onChange={(event) => setForm({ ...form, type: event.target.value })}
                />
              </Field>
            )}
            <Field label={t('ops.channel.baseURL')}>
              <input
                className="input"
                value={form.base_url}
                onChange={(event) => setForm({ ...form, base_url: event.target.value })}
              />
            </Field>
            {scope === 'admin' && (
              <div style={{ display: 'flex', alignItems: 'center', gap: 8, flexWrap: 'wrap' }}>
                <Button
                  type="button"
                  variant="secondary"
                  size="sm"
                  disabled={!form.base_url.trim() || probing || cooldown > 0 || rateLimited}
                  onClick={() => void runProbe()}
                >
                  {probing ? t('ops.channel.probing') : t('ops.channel.probe')}
                </Button>
                {cooldown > 0 && (
                  <span className="hint">
                    {t('ops.channel.probeCooldown', { seconds: cooldown })}
                  </span>
                )}
                {rateLimited && (
                  <span className="hint">{t('ops.channel.probeRateLimited')}</span>
                )}
              </div>
            )}
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
            {scope === 'admin' && probeError && <Alert tone="bad" title={probeError} />}
            {scope === 'admin' && probe && !probe.supported && (
              <p className="hint">{t('ops.channel.probeUnsupported')}</p>
            )}
            {scope === 'admin' && probe && probe.supported && probe.models.length === 0 && (
              <p className="hint">{t('ops.channel.probeEmpty')}</p>
            )}
            {scope === 'admin' && probe && probe.supported && probe.models.length > 0 && (
              <fieldset
                style={{
                  border: '1px solid var(--line, #e5e7eb)',
                  borderRadius: 8,
                  padding: '8px 10px',
                  margin: 0,
                }}
              >
                <legend className="tiny">{t('ops.channel.probePick')}</legend>
                <div style={{ display: 'grid', gap: 6, maxHeight: 220, overflowY: 'auto' }}>
                  {probe.models.map((model) => (
                    <label key={model} style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                      <input
                        type="checkbox"
                        checked={selectedModels.includes(model)}
                        onChange={(event) => toggleModel(model, event.target.checked)}
                      />
                      <span className="mono" style={{ overflowWrap: 'anywhere' }}>
                        {model}
                      </span>
                    </label>
                  ))}
                </div>
                {probe.truncated && (
                  <p className="hint">
                    {t('ops.channel.probeTruncated', {
                      total: probe.total,
                      shown: probe.models.length,
                    })}
                  </p>
                )}
              </fieldset>
            )}
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
