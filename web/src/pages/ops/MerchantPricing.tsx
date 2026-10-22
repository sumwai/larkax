import { useState, type FormEvent } from 'react'
import { api } from '../../api'
import type { PricingRule } from '../../api/types'
import { useI18n } from '../../i18n'
import { useSite } from '../../site/SiteProvider'
import { useAsync } from '../../hooks/useAsync'
import { formatDateTime } from '../../lib/format'
import { Alert, Button, Empty, Modal, PageHead, Skeleton, useToast } from '../../components/ui'
import { IconPlus } from '../../components/icons'
import { ExprEditor } from '../../components/ExprEditor'
import { Field, InputError, OpsLoadError } from './common'
import { linesToList, listToLines, useConfirm } from './common-helpers'

interface RuleForm {
  name: string
  patterns: string
  expr: string
  priority: string
  enabled: boolean
}

const EMPTY_FORM: RuleForm = { name: '', patterns: '', expr: '', priority: '0', enabled: true }

/**
 * 商家给自己的模型定价。
 *
 * 规则的作用域恒为 merchant、归属恒为当前商家——这两项由服务端按会话填，
 * 请求体里不出现；渠道这一维在商家规则里不存在，所以表单上没有渠道栏。
 * 平台规则对商家不可见，这里也不做任何「平台参考价」的对照。
 */
export function MerchantPricing() {
  const { t } = useI18n()
  const { meta } = useSite()
  const client = api.merchant
  const { show: toast } = useToast()
  const { data, error, loading, reload } = useAsync(() => client.listRules(), [])
  const confirm = useConfirm()

  const [editing, setEditing] = useState<PricingRule | null>(null)
  const [creating, setCreating] = useState(false)
  const [form, setForm] = useState<RuleForm>(EMPTY_FORM)
  const [formError, setFormError] = useState<string | null>(null)
  const [exprError, setExprError] = useState<string | null>(null)

  const openCreate = () => {
    setForm(EMPTY_FORM)
    setFormError(null)
    setEditing(null)
    setCreating(true)
  }

  const openEdit = (rule: PricingRule) => {
    const next: RuleForm = {
      name: rule.name,
      patterns: listToLines(rule.model_patterns),
      expr: rule.expr,
      priority: String(rule.priority),
      enabled: rule.enabled,
    }
    setForm(next)
    setFormError(null)
    setEditing(rule)
    setCreating(false)
  }

  const closeForm = () => {
    setCreating(false)
    setEditing(null)
    setFormError(null)
  }

  const submit = (event: FormEvent) => {
    event.preventDefault()
    const patterns = linesToList(form.patterns)
    if (!form.name.trim()) return setFormError(t('ops.pricing.nameRequired'))
    if (patterns.length === 0) return setFormError(t('ops.pricing.patternsRequired'))
    if (exprError) return setFormError(exprError)
    if (!form.expr.trim()) return setFormError(t('ops.pricing.exprRequired'))
    if (!/^-?\d+$/.test(form.priority.trim())) return setFormError(t('ops.invalidInteger'))

    // 只发可变字段：scope / owner_user_id / channel_id 出现即被服务端拒绝。
    const input = {
      name: form.name.trim(),
      model_patterns: patterns,
      expr: form.expr,
      priority: Number(form.priority),
      enabled: form.enabled,
    }

    if (editing) {
      confirm.request({
        title: t('ops.confirmSave'),
        body: t('ops.pricing.saveConfirm', { name: editing.name }),
        label: t('common.save'),
        run: async () => {
          await client.updateRule(editing.id, input)
          toast(t('ops.saved'))
          closeForm()
          reload()
        },
      })
      return
    }

    confirm.request({
      title: t('ops.confirmCreate'),
      body: t('ops.pricing.createConfirm', { name: input.name }),
      label: t('ops.create'),
      run: async () => {
        await client.createRule(input)
        toast(t('ops.created'))
        closeForm()
        reload()
      },
    })
  }

  const all = data ?? []

  return (
    <>
      <PageHead
        title={t('ops.pricing.merchantTitle')}
        subtitle={t('ops.pricing.merchantSubtitle')}
        actions={
          <Button icon={<IconPlus size={15} />} onClick={openCreate}>
            {t('ops.pricing.createRule')}
          </Button>
        }
      />

      {error && <OpsLoadError error={error} onRetry={reload} />}

      {loading && !data && (
        <div className="card panel">
          <Skeleton height={18} />
          <Skeleton height={18} />
        </div>
      )}

      {data && all.length === 0 && (
        <div className="card">
          <Empty
            title={t('ops.pricing.merchantEmpty')}
            body={t('ops.pricing.merchantEmptyBody')}
            action={
              <Button icon={<IconPlus size={15} />} onClick={openCreate}>
                {t('ops.pricing.createRule')}
              </Button>
            }
          />
        </div>
      )}

      {all.length > 0 && (
        <section className="card panel" aria-label={t('ops.pricing.merchantTitle')}>
          {all.map((rule) => (
            <div key={rule.id} className="ops-row">
              <div className="keyname">
                <b>{rule.name}</b>
                <span className="tiny">
                  {rule.model_patterns.join(', ')} · {t('ops.pricing.priority')} {rule.priority}
                </span>
              </div>
              <span className="mono tiny cell-ellipsis">{rule.expr}</span>
              <span className="tiny">
                {rule.enabled ? t('ops.pricing.on') : t('ops.pricing.off')} ·{' '}
                {formatDateTime(rule.updated_at, meta.timezone)}
              </span>
              {rule.expr_error && <Alert tone="bad" title={rule.expr_error} />}
              <div className="actions">
                <Button variant="ghost" size="sm" onClick={() => openEdit(rule)}>
                  {t('ops.edit')}
                </Button>
              </div>
            </div>
          ))}
        </section>
      )}

      {(creating || editing) && (
        <Modal
          sheetSize="lg"
          title={editing ? t('ops.pricing.editRule') : t('ops.pricing.createRule')}
          onClose={closeForm}
          footer={
            <>
              <Button variant="secondary" onClick={closeForm}>
                {t('common.cancel')}
              </Button>
              <Button type="submit" form="merchant-rule-form">
                {editing ? t('common.save') : t('ops.create')}
              </Button>
            </>
          }
        >
          <form id="merchant-rule-form" onSubmit={submit} style={{ display: 'grid', gap: 12 }}>
            <Field label={t('ops.pricing.name')}>
              <input
                className="input"
                value={form.name}
                onChange={(event) => setForm({ ...form, name: event.target.value })}
              />
            </Field>
            <Field label={t('ops.pricing.patterns')} hint={t('ops.pricing.merchantPatternsHint')}>
              <textarea
                className="input"
                value={form.patterns}
                onChange={(event) => setForm({ ...form, patterns: event.target.value })}
              />
            </Field>
            <ExprEditor
              value={form.expr}
              onChange={(expr) => setForm({ ...form, expr })}
              onErrorChange={setExprError}
            />
            <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 12 }}>
              <Field label={t('ops.pricing.priority')}>
                <input
                  className="input"
                  value={form.priority}
                  onChange={(event) => setForm({ ...form, priority: event.target.value })}
                />
              </Field>
              <Field label={t('ops.pricing.enabled')}>
                <input
                  type="checkbox"
                  checked={form.enabled}
                  onChange={(event) => setForm({ ...form, enabled: event.target.checked })}
                />
              </Field>
            </div>
            <InputError message={formError} />
          </form>
        </Modal>
      )}

      {confirm.dialog}
    </>
  )
}
