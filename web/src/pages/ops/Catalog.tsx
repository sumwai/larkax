import { useRef, useState, type FormEvent, type ReactNode } from 'react'
import { api } from '../../api'
import type {
  ChannelAdapter,
  OpsModel,
  OpsModelCreateInput,
  OpsModelInput,
} from '../../api/types'
import { useI18n } from '../../i18n'
import { useSite } from '../../site/SiteProvider'
import { useAsync } from '../../hooks/useAsync'
import { useIsCompact } from '../../hooks/useMediaQuery'
import { formatDateTime } from '../../lib/format'
import {
  Button,
  Empty,
  Modal,
  PageHead,
  Skeleton,
  useToast,
} from '../../components/ui'
import { IconPlus, IconTrash } from '../../components/icons'
import { ActionSheet } from '../../components/ActionSheet'
import { MobileCard, MobileList } from '../../components/MobileList'
import { Tabs } from '../../components/Tabs'
import { deriveAdapterDefaults } from './catalog-derive'
import { Field, InputError, OpsLoadError, SearchBox } from './common'
import { linesToList, listToLines, useConfirm } from './common-helpers'
import { isDirty, useCloseGuard, useUnsavedGuard } from './guard'

const CAPABILITY_KEYS = ['reasoning', 'tools', 'vision', 'audio', 'cache', 'batch'] as const
type CapabilityKey = (typeof CAPABILITY_KEYS)[number]

// 编辑弹窗的四个分组。字段按「改什么」分，不按类型分：找上限的人不会去
// 「模态与时间」里翻，改能力的人也不需要先看到两个日期。
const MODEL_TABS = ['basic', 'limits', 'capabilities', 'modalities'] as const
type ModelTab = (typeof MODEL_TABS)[number]

const TAB_LABEL = {
  basic: 'ops.model.tabBasic',
  limits: 'ops.model.tabLimits',
  capabilities: 'ops.model.tabCapabilities',
  modalities: 'ops.model.tabModalities',
} as const

/**
 * 三个上限各自的校验文案。
 *
 * 以前共用一句「这一栏要填整数」，读屏听到的只有那句话，看不出是哪一格；
 * 三格错在同一个分组里，视觉上还能看到红色左边线，读屏不行。
 */
const LIMIT_FIELDS = [
  { field: 'context_limit', label: 'ops.model.context', message: 'ops.model.contextLimitInvalid' },
  { field: 'input_limit', label: 'ops.model.inputLimit', message: 'ops.model.inputLimitInvalid' },
  { field: 'output_limit', label: 'ops.model.outputLimit', message: 'ops.model.outputLimitInvalid' },
] as const

/** 校验失败会指名的输入框；也是「切组之后焦点该落在哪」的候选。 */
type ModelField = 'canonical_name' | (typeof LIMIT_FIELDS)[number]['field']

interface ModelForm {
  canonical_name: string
  display_name: string
  vendor: string
  family: string
  context_limit: string
  input_limit: string
  output_limit: string
  knowledge_cutoff: string
  release_date: string
  modalities_input: string
  modalities_output: string
  endpoints: string
  caps: Record<CapabilityKey, string>
}

const EMPTY_FORM: ModelForm = {
  canonical_name: '',
  display_name: '',
  vendor: '',
  family: '',
  context_limit: '0',
  input_limit: '0',
  output_limit: '0',
  knowledge_cutoff: '',
  release_date: '',
  modalities_input: 'text',
  modalities_output: 'text',
  endpoints: '',
  caps: { reasoning: 'unknown', tools: 'unknown', vision: 'unknown', audio: 'unknown', cache: 'unknown', batch: 'unknown' },
}

function capsFrom(model: OpsModel): Record<CapabilityKey, string> {
  const out = { ...EMPTY_FORM.caps }
  for (const key of CAPABILITY_KEYS) {
    const value = model.capabilities[key]
    out[key] = value === null ? 'unknown' : value ? 'yes' : 'no'
  }
  return out
}

function capValue(text: string): boolean | null {
  if (text === 'yes') return true
  if (text === 'no') return false
  return null
}

function formOf(model: OpsModel): ModelForm {
  return {
    canonical_name: model.canonical_name,
    display_name: model.display_name,
    vendor: model.vendor,
    family: model.family,
    context_limit: String(model.context_limit),
    input_limit: String(model.input_limit),
    output_limit: String(model.output_limit),
    knowledge_cutoff: model.knowledge_cutoff ?? '',
    release_date: model.release_date ?? '',
    modalities_input: listToLines(model.modalities_input),
    modalities_output: listToLines(model.modalities_output),
    endpoints: listToLines(model.capabilities.endpoints),
    caps: capsFrom(model),
  }
}

/** 模型目录：平台级资源，只有 admin 视图有这一页。 */
export function OpsModels() {
  const { t } = useI18n()
  const { meta } = useSite()
  const client = api.admin
  const { show: toast } = useToast()
  const { data, error, loading, reload } = useAsync(() => client.listModels(), [])
  // 只用来带出端点、按协议推定模态。选中的是表单里的一次选择，不随模型存下来：
  // 模型可以在多条端点上被服务，那种情况选「不指定」。
  const adapters = useAsync<ChannelAdapter[]>(() => client.listAdapters(), [])
  const confirm = useConfirm()
  const compact = useIsCompact()
  const [editing, setEditing] = useState<OpsModel | null>(null)
  // 窄屏上整张卡片是一个按钮，编辑与删除放在底部弹出的菜单里。
  const [actions, setActions] = useState<OpsModel | null>(null)
  const [creating, setCreating] = useState(false)
  const [form, setForm] = useState<ModelForm>(EMPTY_FORM)
  // 打开时的那份值：改动过的话，关窗前要再问一句。
  const [initialForm, setInitialForm] = useState<ModelForm>(EMPTY_FORM)
  // 选中的适配器类型；'' 是「不指定」，端点与模态都手填。
  const [adapterType, setAdapterType] = useState('')
  // 模态是不是「表单里现有的值」：编辑时目录里的既有值、用户改过的值都算。
  // 是的话不再按协议重推——那会把采集过的事实或人的输入冲掉。
  const [modalitiesTouched, setModalitiesTouched] = useState(false)
  const [formError, setFormError] = useState<string | null>(null)
  const [query, setQuery] = useState('')
  // 当前分组与「上一次次校验失败在哪一组」。后者只用来画标记：用户改了值
  // 也不撤，下一次提交重新判定。
  const [tab, setTab] = useState<ModelTab>('basic')
  const [errorTab, setErrorTab] = useState<ModelTab | null>(null)
  // 校验失败时点名的那一格：切组之后焦点就落在它身上。
  const [errorField, setErrorField] = useState<ModelField | null>(null)
  // 每失败一次加一，弹窗据此重新聚焦（切组会把输入框卸载再挂回来）。
  const [focusToken, setFocusToken] = useState(0)
  const fieldRefs = useRef<Partial<Record<ModelField, HTMLInputElement | null>>>({})

  const openCreate = () => {
    setForm(EMPTY_FORM)
    setInitialForm(EMPTY_FORM)
    setAdapterType('')
    setModalitiesTouched(false)
    setFormError(null)
    setTab('basic')
    setErrorTab(null)
    setErrorField(null)
    setCreating(true)
  }

  const openEdit = (model: OpsModel) => {
    const next = formOf(model)
    setForm(next)
    setInitialForm(next)
    setAdapterType('')
    // 既有模型的两栏模态是目录里采集过的事实，不是可以重推的默认值。
    setModalitiesTouched(true)
    setFormError(null)
    setTab('basic')
    setErrorTab(null)
    setErrorField(null)
    setEditing(model)
  }

  const closeForm = () => {
    setCreating(false)
    setEditing(null)
    setAdapterType('')
    setModalitiesTouched(false)
    setFormError(null)
    setTab('basic')
    setErrorTab(null)
    setErrorField(null)
  }

  // 表单真的打开着才算脏：`closeForm` 不重置 form/initialForm，无条件比的话
  // 「放弃」关窗之后这一页会永远算改过，后续每次跳转都被拦。
  const dirty = (creating || editing !== null) && isDirty(form, initialForm)
  const requestClose = useCloseGuard(confirm, dirty, closeForm)
  const unsavedGuard = useUnsavedGuard(dirty)

  // 校验失败时跳到出错的那一组：只在底部显示一句错误，用户会在一个
  // 看不到出错字段的分组里找不到它。`errorField` 与 `focusToken` 一起
  // 交给弹窗：切组卸载了输入框，焦点不能留在 `<body>` 上。
  const fail = (target: ModelTab, field: ModelField, message: string) => {
    setErrorTab(target)
    setErrorField(field)
    setTab(target)
    setFormError(message)
    setFocusToken((token) => token + 1)
  }

  const submit = (event: FormEvent) => {
    event.preventDefault()
    if (!form.canonical_name.trim()) {
      return fail('basic', 'canonical_name', t('ops.model.nameRequired'))
    }
    for (const limit of LIMIT_FIELDS) {
      if (!/^\d+$/.test(form[limit.field].trim())) {
        return fail('limits', limit.field, t(limit.message))
      }
    }
    setErrorTab(null)
    setErrorField(null)
    setFormError(null)
    const capabilities = Object.fromEntries(
      CAPABILITY_KEYS.map((key) => [key, capValue(form.caps[key])]),
    ) as Record<CapabilityKey, boolean | null>
    // 空文本是「不知道」，不是「一条都不支持」——后者要显式列出来才算。
    const endpoints = form.endpoints.trim() ? linesToList(form.endpoints) : null

    if (editing) {
      const input: OpsModelInput = {
        display_name: form.display_name.trim(),
        vendor: form.vendor.trim(),
        family: form.family.trim(),
        context_limit: Number(form.context_limit),
        input_limit: Number(form.input_limit),
        output_limit: Number(form.output_limit),
        knowledge_cutoff: form.knowledge_cutoff,
        release_date: form.release_date,
        modalities_input: linesToList(form.modalities_input),
        modalities_output: linesToList(form.modalities_output),
        capabilities: { ...capabilities, endpoints },
      }
      confirm.request({
        title: t('ops.confirmSave'),
        body: t('ops.model.saveConfirm', { name: editing.canonical_name }),
        label: t('common.save'),
        run: async () => {
          await client.updateModel(editing.canonical_name, input)
          toast(t('ops.saved'))
          closeForm()
          reload()
        },
      })
      return
    }

    const input: OpsModelCreateInput = {
      canonical_name: form.canonical_name.trim(),
      display_name: form.display_name.trim(),
      vendor: form.vendor.trim(),
      family: form.family.trim(),
      context_limit: Number(form.context_limit),
      input_limit: Number(form.input_limit),
      output_limit: Number(form.output_limit),
      knowledge_cutoff: form.knowledge_cutoff,
      release_date: form.release_date,
      modalities_input: linesToList(form.modalities_input),
      modalities_output: linesToList(form.modalities_output),
      capabilities: { ...capabilities, endpoints },
    }
    confirm.request({
      title: t('ops.confirmCreate'),
      body: t('ops.model.createConfirm', { name: input.canonical_name }),
      label: t('ops.create'),
      run: async () => {
        await client.createModel(input)
        toast(t('ops.created'))
        closeForm()
        reload()
      },
    })
  }

  const remove = (model: OpsModel) => {
    confirm.request({
      title: t('ops.model.delete'),
      body: t('ops.model.deleteConfirm', { name: model.canonical_name }),
      label: t('ops.delete'),
      danger: true,
      run: async () => {
        await client.deleteModel(model.canonical_name)
        toast(t('ops.deleted'))
        reload()
      },
    })
  }

  const all = data ?? []
  const needle = query.trim().toLowerCase()
  const models = needle
    ? all.filter((model) =>
        [model.canonical_name, model.display_name, model.vendor].some((value) =>
          value.toLowerCase().includes(needle),
        ),
      )
    : all

  // 弹窗每次重新聚焦时会问这一个：出错的那一格在就交给它，不在就落在刚切过去
  // 的那一个 tab 上。
  //
  // 第二档目前走不到：四个分组的字段都是固定渲染的，切到哪一组，对应输入就
  // 一定挂上（`fieldRefs` 在提交里已经赋值）。留着是因为字段将来若改成按条件
  // 渲染，落在这组的 tab 上仍比落回弹窗更接近「你被带到了哪一组」；tab 的 id
  // 由 `Tabs` 的 `idPrefix="model"` 给定。
  const focusAfterFailure = () => {
    const field = errorField ? fieldRefs.current[errorField] : null
    if (field) return field
    return errorTab ? document.getElementById(`model-tab-${errorTab}`) : null
  }

  const adapterList = adapters.data ?? []
  const selectedAdapter = adapterList.find((adapter) => adapter.type === adapterType) ?? null
  const adapterDefaults = selectedAdapter ? deriveAdapterDefaults(selectedAdapter) : null
  // 清单拉不到、或者服务端一个适配器都没注册：退回手填，并把原因写出来。
  const adaptersUnavailable = Boolean(adapters.error) || adapters.data?.length === 0

  // 换适配器：端点总是跟着走；模态只在没人动过的时候重推。
  //
  // 选回「不指定」时把选中适配器之前那份端点文本还回去：误选一下就把既有模型
  // 的端点冲掉，只靠取消才找得回来，代价太大。
  const manualEndpoints = useRef('')
  const chooseAdapter = (type: string) => {
    const hadAdapter = adapterType !== ''
    if (!hadAdapter && type !== '') manualEndpoints.current = form.endpoints
    setAdapterType(type)
    const adapter = adapterList.find((item) => item.type === type)
    const defaults = adapter ? deriveAdapterDefaults(adapter) : null
    const derived = defaults?.modalities && !modalitiesTouched ? defaults.modalities : null
    setForm((current) => ({
      ...current,
      endpoints: defaults?.endpoint ?? (hadAdapter ? manualEndpoints.current : current.endpoints),
      ...(derived
        ? {
            modalities_input: listToLines(derived.input),
            modalities_output: listToLines(derived.output),
          }
        : {}),
    }))
  }

  // 「按适配器重算」：把推定值写回模态，并撤掉「这是现有值」的标记——写回的
  // 是推定值，之后再换适配器它就继续跟着走，不然这个按钮按下去等于只生效一次。
  const applyDerivedModalities = () => {
    const modalities = adapterDefaults?.modalities
    if (!modalities) return
    setModalitiesTouched(false)
    setForm((current) => ({
      ...current,
      modalities_input: listToLines(modalities.input),
      modalities_output: listToLines(modalities.output),
    }))
  }

  // 每个分组自己的面板。只有选中的那一个有内容，切走的分组由 Tabs 留成空面板
  // ——隐藏的输入框留在 DOM 里既会被 Tab 走到，也会被读屏念出来。
  const panels: Record<ModelTab, ReactNode> = {
    basic: (
      <>
        <Field label={t('ops.model.name')}>
          <input
            className="input"
            ref={(element) => {
              fieldRefs.current.canonical_name = element
            }}
            value={form.canonical_name}
            readOnly={Boolean(editing)}
            onChange={(event) => setForm({ ...form, canonical_name: event.target.value })}
          />
        </Field>
        <Field label={t('ops.model.displayName')}>
          <input
            className="input"
            value={form.display_name}
            onChange={(event) => setForm({ ...form, display_name: event.target.value })}
          />
        </Field>
        <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 12 }}>
          <Field label={t('ops.model.vendor')}>
            <input
              className="input"
              value={form.vendor}
              onChange={(event) => setForm({ ...form, vendor: event.target.value })}
            />
          </Field>
          <Field label={t('ops.model.family')}>
            <input
              className="input"
              value={form.family}
              onChange={(event) => setForm({ ...form, family: event.target.value })}
            />
          </Field>
        </div>
      </>
    ),
    limits: (
      <>
        <p className="hint">{t('ops.model.limitsHint')}</p>
        <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr 1fr', gap: 12 }}>
          {LIMIT_FIELDS.map((limit) => (
            <Field key={limit.field} label={t(limit.label)}>
              <input
                className="input"
                ref={(element) => {
                  fieldRefs.current[limit.field] = element
                }}
                value={form[limit.field]}
                onChange={(event) => setForm({ ...form, [limit.field]: event.target.value })}
              />
            </Field>
          ))}
        </div>
      </>
    ),
    capabilities: (
      <>
        <p className="hint">{t('ops.model.capHint')}</p>
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(3, 1fr)', gap: 12 }}>
          {CAPABILITY_KEYS.map((key) => (
            <Field key={key} label={t(`ops.model.cap.${key}` as 'ops.model.cap.tools')}>
              <select
                className="input"
                value={form.caps[key]}
                onChange={(event) =>
                  setForm({ ...form, caps: { ...form.caps, [key]: event.target.value } })
                }
              >
                <option value="unknown">{t('ops.model.capUnknown')}</option>
                <option value="yes">{t('ops.model.capYes')}</option>
                <option value="no">{t('ops.model.capNo')}</option>
              </select>
            </Field>
          ))}
        </div>
        {adapterList.length > 0 ? (
          <Field label={t('ops.model.adapterDerive')} hint={t('ops.model.adapterDeriveHint')}>
            <select
              className="input"
              value={adapterType}
              onChange={(event) => chooseAdapter(event.target.value)}
            >
              <option value="">{t('ops.model.adapterManual')}</option>
              {adapterList.map((adapter) => (
                <option key={adapter.type} value={adapter.type}>
                  {t('ops.model.adapterOption', {
                    type: adapter.type,
                    endpoint: adapter.endpoint,
                  })}
                </option>
              ))}
            </select>
          </Field>
        ) : (
          adaptersUnavailable && <p className="hint">{t('ops.model.adaptersUnavailable')}</p>
        )}
        <Field
          label={t('ops.model.endpoints')}
          hint={
            selectedAdapter
              ? t('ops.model.endpointsDerived', { type: selectedAdapter.type })
              : t('ops.model.endpointsHint')
          }
        >
          <textarea
            className="input"
            value={form.endpoints}
            readOnly={Boolean(selectedAdapter)}
            onChange={(event) => setForm({ ...form, endpoints: event.target.value })}
          />
        </Field>
      </>
    ),
    modalities: (
      <>
        <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 12 }}>
          <Field label={t('ops.model.modalitiesInput')}>
            <textarea
              className="input"
              value={form.modalities_input}
              onChange={(event) => {
                setModalitiesTouched(true)
                setForm({ ...form, modalities_input: event.target.value })
              }}
            />
          </Field>
          <Field label={t('ops.model.modalitiesOutput')}>
            <textarea
              className="input"
              value={form.modalities_output}
              onChange={(event) => {
                setModalitiesTouched(true)
                setForm({ ...form, modalities_output: event.target.value })
              }}
            />
          </Field>
        </div>
        {selectedAdapter && adapterDefaults?.modalities && !modalitiesTouched && (
          <p className="hint">
            {t('ops.model.modalitiesDerived', { type: selectedAdapter.type })}
          </p>
        )}
        {selectedAdapter && adapterDefaults?.modalities && modalitiesTouched && (
          <p className="hint">
            {t('ops.model.modalitiesKept', { type: selectedAdapter.type })}{' '}
            <Button type="button" variant="ghost" size="sm" onClick={applyDerivedModalities}>
              {t('ops.model.modalitiesRecompute')}
            </Button>
          </p>
        )}
        {selectedAdapter && !adapterDefaults?.modalities && (
          <p className="hint">
            {t('ops.model.modalitiesUnavailable', {
              type: selectedAdapter.type,
              endpoint: selectedAdapter.endpoint,
            })}
          </p>
        )}
        <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 12 }}>
          <Field label={t('ops.model.knowledgeCutoff')}>
            <input
              className="input"
              type="date"
              value={form.knowledge_cutoff}
              onChange={(event) => setForm({ ...form, knowledge_cutoff: event.target.value })}
            />
          </Field>
          <Field label={t('ops.model.releaseDate')}>
            <input
              className="input"
              type="date"
              value={form.release_date}
              onChange={(event) => setForm({ ...form, release_date: event.target.value })}
            />
          </Field>
        </div>
      </>
    ),
  }

  return (
    <>
      <PageHead
        title={t('ops.model.title')}
        subtitle={t('ops.model.subtitle')}
        actions={
          <Button icon={<IconPlus size={15} />} onClick={openCreate}>
            {t('ops.model.create')}
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

      {data && all.length === 0 && (
        <div className="card">
          <Empty
            title={t('ops.model.emptyTitle')}
            body={t('ops.model.emptyBody')}
            action={
              <Button icon={<IconPlus size={15} />} onClick={openCreate}>
                {t('ops.model.create')}
              </Button>
            }
          />
        </div>
      )}

      {all.length > 0 && <SearchBox value={query} onChange={setQuery} />}

      {all.length > 0 && models.length === 0 && (
        <div className="card">
          <Empty title={t('ops.searchEmpty')} />
        </div>
      )}

      {models.length > 0 && compact && (
        <MobileList label={t('ops.model.title')}>
          {models.map((model) => (
            <MobileCard
              key={model.canonical_name}
              title={model.canonical_name}
              subtitle={model.display_name || t('common.none')}
              fields={[
                { label: t('ops.model.vendor'), value: model.vendor || t('common.none') },
                { label: t('ops.model.context'), value: model.context_limit.toLocaleString() },
              ]}
              onPress={() => setActions(model)}
              pressLabel={t('ops.cardActionsFor', { name: model.canonical_name })}
            />
          ))}
        </MobileList>
      )}

      {models.length > 0 && !compact && (
        <section className="card table ops-table" aria-label={t('ops.model.title')}>
          <div className="thead model-head">
            <span>{t('ops.model.name')}</span>
            <span>{t('ops.model.vendor')}</span>
            <span>{t('ops.model.context')}</span>
            <span>{t('ops.colUpdated')}</span>
            <span className="amount">{t('ops.colActions')}</span>
          </div>
          {models.map((model) => (
            <div className="trow model-row" key={model.canonical_name}>
              <div className="keyname">
                <b>{model.canonical_name}</b>
                <span className="tiny">{model.display_name || t('common.none')}</span>
              </div>
              <span className="muted-cell">{model.vendor || t('common.none')}</span>
              <span className="num muted-cell">{model.context_limit.toLocaleString()}</span>
              <span className="num muted-cell">{formatDateTime(model.updated_at, meta.timezone)}</span>
              <div className="actions">
                <Button variant="ghost" size="sm" onClick={() => openEdit(model)}>
                  {t('ops.edit')}
                </Button>
                <Button
                  variant="danger"
                  size="sm"
                  icon={<IconTrash size={14} />}
                  onClick={() => remove(model)}
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
          title={actions.canonical_name}
          description={actions.display_name || undefined}
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
          sheetSize="lg"
          title={editing ? t('ops.model.editTitle') : t('ops.model.createTitle')}
          onClose={requestClose}
          focusTarget={focusAfterFailure}
          focusToken={focusToken}
          footer={
            <>
              <Button variant="secondary" onClick={requestClose}>
                {t('common.cancel')}
              </Button>
              <Button type="submit" form="model-form">
                {editing ? t('common.save') : t('ops.create')}
              </Button>
            </>
          }
        >
          <form id="model-form" onSubmit={submit} style={{ display: 'grid', gap: 12 }}>
            <Tabs
              label={t('ops.model.formTabs')}
              idPrefix="model"
              value={tab}
              onChange={(id) => setTab(id as ModelTab)}
              items={MODEL_TABS.map((id) => ({
                id,
                label: t(TAB_LABEL[id]),
                invalid: errorTab === id,
                adornment:
                  errorTab === id ? (
                    <span className="tab-dot" aria-label={t('ops.model.tabHasError')} />
                  ) : null,
                panelClassName:
                  errorTab === id ? 'model-form-group is-invalid' : 'model-form-group',
                panel: panels[id],
              }))}
            />

            <InputError message={formError} />
          </form>
        </Modal>
      )}

      {confirm.dialog}
      {unsavedGuard}
    </>
  )
}
