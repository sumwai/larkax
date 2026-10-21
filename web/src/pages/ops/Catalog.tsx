import { useState, type FormEvent } from 'react'
import { api } from '../../api'
import type { OpsModel, OpsModelCreateInput, OpsModelInput } from '../../api/types'
import { useI18n } from '../../i18n'
import { useSite } from '../../site/SiteProvider'
import { useAsync } from '../../hooks/useAsync'
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
import {
  Field,
  InputError,
  OpsLoadError,
  SearchBox,
  linesToList,
  listToLines,
  useConfirm,
} from './common'
import { isDirty, useCloseGuard } from './guard'

const CAPABILITY_KEYS = ['reasoning', 'tools', 'vision', 'audio', 'cache', 'batch'] as const
type CapabilityKey = (typeof CAPABILITY_KEYS)[number]

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
  const confirm = useConfirm()
  const [editing, setEditing] = useState<OpsModel | null>(null)
  const [creating, setCreating] = useState(false)
  const [form, setForm] = useState<ModelForm>(EMPTY_FORM)
  // 打开时的那份值：改动过的话，关窗前要再问一句。
  const [initialForm, setInitialForm] = useState<ModelForm>(EMPTY_FORM)
  const [formError, setFormError] = useState<string | null>(null)
  const [query, setQuery] = useState('')

  const openCreate = () => {
    setForm(EMPTY_FORM)
    setInitialForm(EMPTY_FORM)
    setFormError(null)
    setCreating(true)
  }

  const openEdit = (model: OpsModel) => {
    const next = formOf(model)
    setForm(next)
    setInitialForm(next)
    setFormError(null)
    setEditing(model)
  }

  const closeForm = () => {
    setCreating(false)
    setEditing(null)
    setFormError(null)
  }

  const requestClose = useCloseGuard(confirm, isDirty(form, initialForm), closeForm)

  const submit = (event: FormEvent) => {
    event.preventDefault()
    if (!form.canonical_name.trim()) return setFormError(t('ops.model.nameRequired'))
    for (const key of ['context_limit', 'input_limit', 'output_limit'] as const) {
      if (!/^\d+$/.test(form[key].trim())) return setFormError(t('ops.invalidInteger'))
    }
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

      {models.length > 0 && (
        <section className="card table ops-table" aria-label={t('ops.model.title')}>
          <div className="thead model-head" style={{ display: 'grid' }}>
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

      {(creating || editing) && (
        <Modal
          title={editing ? t('ops.model.editTitle') : t('ops.model.createTitle')}
          onClose={requestClose}
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
            <Field label={t('ops.model.name')}>
              <input
                className="input"
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
            <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr 1fr', gap: 12 }}>
              <Field label={t('ops.model.context')}>
                <input
                  className="input"
                  value={form.context_limit}
                  onChange={(event) => setForm({ ...form, context_limit: event.target.value })}
                />
              </Field>
              <Field label={t('ops.model.inputLimit')}>
                <input
                  className="input"
                  value={form.input_limit}
                  onChange={(event) => setForm({ ...form, input_limit: event.target.value })}
                />
              </Field>
              <Field label={t('ops.model.outputLimit')}>
                <input
                  className="input"
                  value={form.output_limit}
                  onChange={(event) => setForm({ ...form, output_limit: event.target.value })}
                />
              </Field>
            </div>
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
            <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 12 }}>
              <Field label={t('ops.model.modalitiesInput')}>
                <textarea
                  className="input"
                  value={form.modalities_input}
                  onChange={(event) => setForm({ ...form, modalities_input: event.target.value })}
                />
              </Field>
              <Field label={t('ops.model.modalitiesOutput')}>
                <textarea
                  className="input"
                  value={form.modalities_output}
                  onChange={(event) => setForm({ ...form, modalities_output: event.target.value })}
                />
              </Field>
            </div>
            <Field label={t('ops.model.endpoints')} hint={t('ops.model.endpointsHint')}>
              <textarea
                className="input"
                value={form.endpoints}
                onChange={(event) => setForm({ ...form, endpoints: event.target.value })}
              />
            </Field>
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
            <InputError message={formError} />
          </form>
        </Modal>
      )}

      {confirm.dialog}
    </>
  )
}
