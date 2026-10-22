import { useState, type FormEvent } from 'react'
import { api } from '../../api'
import type { PricingRule } from '../../api/types'
import { useI18n } from '../../i18n'
import { useSite } from '../../site/SiteProvider'
import { useAsync } from '../../hooks/useAsync'
import { formatDateTime } from '../../lib/format'
import { Alert, Button, Empty, Modal, PageHead, Skeleton, useToast } from '../../components/ui'
import { IconPlus } from '../../components/icons'
import { ExprEditor, exprPerMillion } from '../../components/ExprEditor'
import { Field, InputError, OpsLoadError } from './common'
import { linesToList, listToLines, useConfirm } from './common-helpers'
import { isDirty, useCloseGuard, useUnsavedGuard } from './guard'

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
 * 规则的作用域恒为 merchant、归属恒为当前商家，渠道这一维在商家规则里不存在，
 * 所以表单上没有渠道栏。请求体里不带 `scope` / `owner_user_id` / `channel_id`——
 * 但两条写路径对它们的处理并不一样：新建走 `createRule`
 * （`internal/adminapi/resources.go`），会话里有商家身份时直接把这几个字段改成
 * 会话的值，请求体里写了也被静默覆盖，不报错；修改走 `UpdateOwned`
 * （`internal/pricing/admin.go`），三个字段只要有一个非空就报错拒绝。
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
  // 打开表单时的那份值：改动过的话，关窗前要再问一句。
  const [initialForm, setInitialForm] = useState<RuleForm>(EMPTY_FORM)
  const [formError, setFormError] = useState<string | null>(null)
  const [exprError, setExprError] = useState<string | null>(null)

  const openCreate = () => {
    setForm(EMPTY_FORM)
    setInitialForm(EMPTY_FORM)
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
    setInitialForm(next)
    setFormError(null)
    setEditing(rule)
    setCreating(false)
  }

  const closeForm = () => {
    setCreating(false)
    setEditing(null)
    setFormError(null)
  }

  // 表单真的打开着才算脏：`closeForm` 不重置 form/initialForm，无条件比的话
  // 「放弃」关窗之后这一页会永远算改过，后续每次跳转都被拦。
  const dirty = (creating || editing !== null) && isDirty(form, initialForm)
  const requestClose = useCloseGuard(confirm, dirty, closeForm)
  const unsavedGuard = useUnsavedGuard(dirty)

  const submit = (event: FormEvent) => {
    event.preventDefault()
    const patterns = linesToList(form.patterns)
    if (!form.name.trim()) return setFormError(t('ops.pricing.nameRequired'))
    if (patterns.length === 0) return setFormError(t('ops.pricing.patternsRequired'))
    if (exprError) return setFormError(exprError)
    if (!form.expr.trim()) return setFormError(t('ops.pricing.exprRequired'))
    if (!/^-?\d+$/.test(form.priority.trim())) return setFormError(t('ops.invalidInteger'))

    // 只发可变字段。scope / owner_user_id / channel_id 不属于请求体：新建时
    // 服务端按会话静默覆盖，修改时出现即拒（见文件头的说明）。
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

  /**
   * 停用 / 启用单条规则。
   *
   * 从清单行上直接切，不经过编辑表单：停用是一件「现在就要它别再计价」的事，
   * 走整表单回写会顺带把别人（或别的标签页）刚改的其它字段盖回去。
   * 停用是危险动作，确认钮给危险色。
   */
  const toggleRule = (rule: PricingRule) => {
    confirm.request({
      title: t(rule.enabled ? 'ops.pricing.disable' : 'ops.pricing.enable'),
      body: t(rule.enabled ? 'ops.pricing.disableConfirm' : 'ops.pricing.enableConfirm', {
        name: rule.name,
      }),
      label: t('common.confirm'),
      danger: rule.enabled,
      run: async () => {
        await client.updateRule(rule.id, { enabled: !rule.enabled })
        toast(t('ops.saved'))
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
          <p className="hint">
            {t('ops.pricing.merchantPriceNote', { currency: meta.currency })}
          </p>
          {all.map((rule) => {
            // 表单按「每 100 万 token」填，库里存的是每 token，直接渲染会差六个数量级。
            // 带条件的表达式不是一张单价表，原样显示。
            const price = exprPerMillion(rule.expr) ?? rule.expr
            return (
              <div key={rule.id} className="ops-row">
                <div className="keyname">
                  <b>{rule.name}</b>
                  <span className="tiny">
                    {rule.model_patterns.join(', ')} · {t('ops.pricing.priority')} {rule.priority}
                  </span>
                </div>
                {/* `cell-ellipsis` 会截断，`title` 给完整的一段。 */}
                <span className="mono tiny cell-ellipsis" title={price}>
                  {price}
                </span>
                <span className="tiny">
                  {rule.enabled ? t('ops.pricing.on') : t('ops.pricing.off')} ·{' '}
                  {formatDateTime(rule.updated_at, meta.timezone)}
                </span>
                {rule.expr_error && <Alert tone="bad" title={rule.expr_error} />}
                <div className="actions">
                  <Button variant="ghost" size="sm" onClick={() => openEdit(rule)}>
                    {t('ops.edit')}
                  </Button>
                  <Button variant="ghost" size="sm" onClick={() => toggleRule(rule)}>
                    {t(rule.enabled ? 'ops.pricing.disable' : 'ops.pricing.enable')}
                  </Button>
                </div>
              </div>
            )
          })}
        </section>
      )}

      {(creating || editing) && (
        <Modal
          sheetSize="lg"
          title={editing ? t('ops.pricing.editRule') : t('ops.pricing.createRule')}
          onClose={requestClose}
          footer={
            <>
              <Button variant="secondary" onClick={requestClose}>
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
      {unsavedGuard}
    </>
  )
}
