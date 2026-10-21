import { useState, type FormEvent } from 'react'
import { api } from '../../api'
import type { PricingCost, PricingRule, RuleScope } from '../../api/types'
import { useI18n } from '../../i18n'
import { useSite } from '../../site/SiteProvider'
import { useAsync } from '../../hooks/useAsync'
import { formatDateTime } from '../../lib/format'
import { Alert, Button, Empty, Modal, PageHead, Skeleton, useToast } from '../../components/ui'
import { IconPlus } from '../../components/icons'
import { ExprEditor } from '../../components/ExprEditor'
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

type Tab = 'rules' | 'costs'

interface RuleForm {
  name: string
  scope: RuleScope
  owner: string
  channel: string
  patterns: string
  expr: string
  priority: string
  enabled: boolean
}

interface CostForm {
  channel: string
  patterns: string
  expr: string
  priority: string
}

const EMPTY_RULE: RuleForm = {
  name: '',
  scope: 'platform',
  owner: '0',
  channel: '0',
  patterns: '',
  expr: '',
  priority: '0',
  enabled: true,
}

const EMPTY_COST: CostForm = { channel: '', patterns: '', expr: '', priority: '0' }

/** 计价规则与成本：平台级资源，只有 admin 视图有这一页。 */
export function OpsPricing() {
  const { t } = useI18n()
  const { meta } = useSite()
  const client = api.admin
  const { show: toast } = useToast()
  const [tab, setTab] = useState<Tab>('rules')

  const rules = useAsync(() => client.listRules(), [])
  const costs = useAsync(() => client.listCosts(), [])
  const channels = useAsync(() => client.listChannels(), [])
  const confirm = useConfirm()

  const [ruleEditing, setRuleEditing] = useState<PricingRule | null>(null)
  const [ruleCreating, setRuleCreating] = useState(false)
  const [ruleForm, setRuleForm] = useState<RuleForm>(EMPTY_RULE)
  // 打开时的那份值：改动过的话，关窗前要再问一句。
  const [ruleInitial, setRuleInitial] = useState<RuleForm>(EMPTY_RULE)
  const [ruleError, setRuleError] = useState<string | null>(null)
  // 表达式编辑器报上来的第一条错误（含「一项都没填」），提交时不再往下走。
  const [ruleExprError, setRuleExprError] = useState<string | null>(null)
  const [ruleQuery, setRuleQuery] = useState('')

  const [costEditing, setCostEditing] = useState<PricingCost | null>(null)
  const [costCreating, setCostCreating] = useState(false)
  const [costForm, setCostForm] = useState<CostForm>(EMPTY_COST)
  const [costInitial, setCostInitial] = useState<CostForm>(EMPTY_COST)
  const [costError, setCostError] = useState<string | null>(null)
  const [costExprError, setCostExprError] = useState<string | null>(null)
  const [costQuery, setCostQuery] = useState('')

  const channelName = (id: number) =>
    id === 0 ? t('ops.pricing.platformWide') : (channels.data?.find((item) => item.id === id)?.name ?? `#${id}`)

  const openRule = (rule?: PricingRule) => {
    if (rule) {
      const next: RuleForm = {
        name: rule.name,
        scope: rule.scope as RuleScope,
        owner: String(rule.owner_user_id),
        channel: String(rule.channel_id),
        patterns: listToLines(rule.model_patterns),
        expr: rule.expr,
        priority: String(rule.priority),
        enabled: rule.enabled,
      }
      setRuleForm(next)
      setRuleInitial(next)
      setRuleEditing(rule)
      setRuleCreating(false)
    } else {
      setRuleForm(EMPTY_RULE)
      setRuleInitial(EMPTY_RULE)
      setRuleEditing(null)
      setRuleCreating(true)
    }
    setRuleError(null)
  }

  const openCost = (cost?: PricingCost) => {
    if (cost) {
      const next: CostForm = {
        channel: String(cost.channel_id),
        patterns: listToLines(cost.model_patterns),
        expr: cost.expr,
        priority: String(cost.priority),
      }
      setCostForm(next)
      setCostInitial(next)
      setCostEditing(cost)
      setCostCreating(false)
    } else {
      const blank = { ...EMPTY_COST, channel: String(channels.data?.[0]?.id ?? '') }
      setCostForm(blank)
      setCostInitial(blank)
      setCostEditing(null)
      setCostCreating(true)
    }
    setCostError(null)
  }

  const closeRuleForm = () => {
    setRuleCreating(false)
    setRuleEditing(null)
  }

  const closeCostForm = () => {
    setCostCreating(false)
    setCostEditing(null)
  }

  const requestCloseRule = useCloseGuard(confirm, isDirty(ruleForm, ruleInitial), closeRuleForm)
  const requestCloseCost = useCloseGuard(confirm, isDirty(costForm, costInitial), closeCostForm)

  const submitRule = (event: FormEvent) => {
    event.preventDefault()
    const patterns = linesToList(ruleForm.patterns)
    if (!ruleForm.name.trim()) return setRuleError(t('ops.pricing.nameRequired'))
    if (patterns.length === 0) return setRuleError(t('ops.pricing.patternsRequired'))
    if (ruleExprError) return setRuleError(ruleExprError)
    if (!ruleForm.expr.trim()) return setRuleError(t('ops.pricing.exprRequired'))
    if (!/^-?\d+$/.test(ruleForm.priority.trim())) return setRuleError(t('ops.invalidInteger'))
    // 平台级规则没有归属，那两个栏也不在表单上：整数校验与发送都只看非平台级。
    const platform = ruleForm.scope === 'platform'
    if (!platform && !/^\d+$/.test(ruleForm.owner.trim())) {
      return setRuleError(t('ops.invalidInteger'))
    }
    if (!platform && !/^\d+$/.test(ruleForm.channel.trim())) {
      return setRuleError(t('ops.invalidInteger'))
    }

    if (ruleEditing) {
      confirm.request({
        title: t('ops.confirmSave'),
        body: t('ops.pricing.saveConfirm', { name: ruleEditing.name }),
        label: t('common.save'),
        run: async () => {
          await client.updateRule(ruleEditing.id, {
            name: ruleForm.name.trim(),
            scope: ruleForm.scope,
            owner_user_id: platform ? 0 : Number(ruleForm.owner),
            channel_id: platform ? 0 : Number(ruleForm.channel),
            model_patterns: patterns,
            expr: ruleForm.expr,
            priority: Number(ruleForm.priority),
            enabled: ruleForm.enabled,
          })
          toast(t('ops.saved'))
          setRuleEditing(null)
          rules.reload()
        },
      })
      return
    }

    confirm.request({
      title: t('ops.confirmCreate'),
      body: t('ops.pricing.createConfirm', { name: ruleForm.name.trim() }),
      label: t('ops.create'),
      run: async () => {
        await client.createRule({
          name: ruleForm.name.trim(),
          scope: ruleForm.scope,
          owner_user_id: platform ? 0 : Number(ruleForm.owner),
          channel_id: platform ? 0 : Number(ruleForm.channel),
          model_patterns: patterns,
          expr: ruleForm.expr,
          priority: Number(ruleForm.priority),
          enabled: ruleForm.enabled,
        })
        toast(t('ops.created'))
        setRuleCreating(false)
        rules.reload()
      },
    })
  }

  const submitCost = (event: FormEvent) => {
    event.preventDefault()
    const patterns = linesToList(costForm.patterns)
    if (!/^\d+$/.test(costForm.channel.trim()) || Number(costForm.channel) <= 0) {
      return setCostError(t('ops.pricing.channelRequired'))
    }
    if (patterns.length === 0) return setCostError(t('ops.pricing.patternsRequired'))
    if (costExprError) return setCostError(costExprError)
    if (!costForm.expr.trim()) return setCostError(t('ops.pricing.exprRequired'))
    if (!/^-?\d+$/.test(costForm.priority.trim())) return setCostError(t('ops.invalidInteger'))

    if (costEditing) {
      confirm.request({
        title: t('ops.confirmSave'),
        body: t('ops.pricing.costSaveConfirm', { id: costEditing.id }),
        label: t('common.save'),
        run: async () => {
          await client.updateCost(costEditing.id, {
            channel_id: Number(costForm.channel),
            model_patterns: patterns,
            expr: costForm.expr,
            priority: Number(costForm.priority),
          })
          toast(t('ops.saved'))
          setCostEditing(null)
          costs.reload()
        },
      })
      return
    }

    confirm.request({
      title: t('ops.confirmCreate'),
      body: t('ops.pricing.costCreateConfirm', { channel: channelName(Number(costForm.channel)) }),
      label: t('ops.create'),
      run: async () => {
        await client.createCost({
          channel_id: Number(costForm.channel),
          model_patterns: patterns,
          expr: costForm.expr,
          priority: Number(costForm.priority),
        })
        toast(t('ops.created'))
        setCostCreating(false)
        costs.reload()
      },
    })
  }

  const toggleRule = (rule: PricingRule) => {
    confirm.request({
      title: t(rule.enabled ? 'ops.pricing.disable' : 'ops.pricing.enable'),
      body: t(rule.enabled ? 'ops.pricing.disableConfirm' : 'ops.pricing.enableConfirm', { name: rule.name }),
      label: t('common.confirm'),
      danger: rule.enabled,
      run: async () => {
        await client.updateRule(rule.id, { enabled: !rule.enabled })
        toast(t('ops.saved'))
        rules.reload()
      },
    })
  }

  const ruleNeedle = ruleQuery.trim().toLowerCase()
  const ruleRows = (rules.data ?? []).filter(
    (rule) =>
      !ruleNeedle ||
      [rule.name, rule.expr, ...rule.model_patterns].some((value) =>
        value.toLowerCase().includes(ruleNeedle),
      ),
  )
  const costNeedle = costQuery.trim().toLowerCase()
  const costRows = (costs.data ?? []).filter(
    (cost) =>
      !costNeedle ||
      [channelName(cost.channel_id), cost.expr, ...cost.model_patterns].some((value) =>
        value.toLowerCase().includes(costNeedle),
      ),
  )

  return (
    <>
      <PageHead
        title={t('ops.pricing.title')}
        subtitle={t('ops.pricing.subtitle')}
        actions={
          <Button
            icon={<IconPlus size={15} />}
            onClick={() => (tab === 'rules' ? openRule() : openCost())}
          >
            {tab === 'rules' ? t('ops.pricing.createRule') : t('ops.pricing.createCost')}
          </Button>
        }
      />

      <div className="seg" role="group" aria-label={t('ops.pricing.title')}>
        <button aria-pressed={tab === 'rules'} onClick={() => setTab('rules')}>
          {t('ops.pricing.rules')}
        </button>
        <button aria-pressed={tab === 'costs'} onClick={() => setTab('costs')}>
          {t('ops.pricing.costs')}
        </button>
      </div>

      {tab === 'rules' && (
        <>
          {rules.error && <OpsLoadError error={rules.error} onRetry={rules.reload} />}
          {rules.loading && !rules.data && (
            <div className="card panel">
              <Skeleton height={18} />
              <Skeleton height={18} />
            </div>
          )}
          {rules.data && rules.data.length === 0 && (
            <div className="card">
              <Empty title={t('ops.pricing.noRules')} body={t('ops.pricing.noRulesBody')} />
            </div>
          )}
          {(rules.data?.length ?? 0) > 0 && <SearchBox value={ruleQuery} onChange={setRuleQuery} />}
          {(rules.data?.length ?? 0) > 0 && ruleRows.length === 0 && (
            <div className="card">
              <Empty title={t('ops.searchEmpty')} />
            </div>
          )}
          {ruleRows.length > 0 && (
            <section className="card panel" aria-label={t('ops.pricing.rules')}>
              {ruleRows.map((rule) => (
                <div key={rule.id} className="ops-row">
                  <div className="keyname">
                    <b>{rule.name}</b>
                    <span className="tiny">
                      {rule.scope} · {rule.model_patterns.join(', ')} · {t('ops.pricing.priority')}{' '}
                      {rule.priority}
                    </span>
                  </div>
                  <span className="mono tiny cell-ellipsis">{rule.expr}</span>
                  <span className="tiny">
                    {rule.enabled ? t('ops.pricing.on') : t('ops.pricing.off')} ·{' '}
                    {formatDateTime(rule.updated_at, meta.timezone)}
                  </span>
                  {rule.expr_error && <Alert tone="bad" title={rule.expr_error} />}
                  <div className="actions">
                    <Button variant="ghost" size="sm" onClick={() => openRule(rule)}>
                      {t('ops.edit')}
                    </Button>
                    <Button variant="ghost" size="sm" onClick={() => toggleRule(rule)}>
                      {t(rule.enabled ? 'ops.pricing.disable' : 'ops.pricing.enable')}
                    </Button>
                  </div>
                </div>
              ))}
            </section>
          )}
        </>
      )}

      {tab === 'costs' && (
        <>
          {costs.error && <OpsLoadError error={costs.error} onRetry={costs.reload} />}
          {costs.loading && !costs.data && (
            <div className="card panel">
              <Skeleton height={18} />
              <Skeleton height={18} />
            </div>
          )}
          {costs.data && costs.data.length === 0 && (
            <div className="card">
              <Empty title={t('ops.pricing.noCosts')} body={t('ops.pricing.noCostsBody')} />
            </div>
          )}
          {(costs.data?.length ?? 0) > 0 && <SearchBox value={costQuery} onChange={setCostQuery} />}
          {(costs.data?.length ?? 0) > 0 && costRows.length === 0 && (
            <div className="card">
              <Empty title={t('ops.searchEmpty')} />
            </div>
          )}
          {costRows.length > 0 && (
            <section className="card panel" aria-label={t('ops.pricing.costs')}>
              {costRows.map((cost) => (
                <div key={cost.id} className="ops-row">
                  <div className="keyname">
                    <b>{channelName(cost.channel_id)}</b>
                    <span className="tiny">
                      {cost.model_patterns.join(', ')} · {t('ops.pricing.priority')} {cost.priority}
                    </span>
                  </div>
                  <span className="mono tiny cell-ellipsis">{cost.expr}</span>
                  <span className="tiny">{formatDateTime(cost.created_at, meta.timezone)}</span>
                  {cost.expr_error && <Alert tone="bad" title={cost.expr_error} />}
                  <div className="actions">
                    <Button variant="ghost" size="sm" onClick={() => openCost(cost)}>
                      {t('ops.edit')}
                    </Button>
                  </div>
                </div>
              ))}
            </section>
          )}
        </>
      )}

      {(ruleCreating || ruleEditing) && (
        <Modal
          sheetSize="lg"
          title={ruleEditing ? t('ops.pricing.editRule') : t('ops.pricing.createRule')}
          onClose={requestCloseRule}
          footer={
            <>
              <Button
                variant="secondary"
                onClick={requestCloseRule}
              >
                {t('common.cancel')}
              </Button>
              <Button type="submit" form="rule-form">
                {ruleEditing ? t('common.save') : t('ops.create')}
              </Button>
            </>
          }
        >
          <form id="rule-form" onSubmit={submitRule} style={{ display: 'grid', gap: 12 }}>
            <Field label={t('ops.pricing.name')}>
              <input
                className="input"
                value={ruleForm.name}
                onChange={(event) => setRuleForm({ ...ruleForm, name: event.target.value })}
              />
            </Field>
            <div
              style={{
                display: 'grid',
                gridTemplateColumns: ruleForm.scope === 'platform' ? '1fr' : '1fr 1fr 1fr',
                gap: 12,
              }}
            >
              <Field label={t('ops.pricing.scope')}>
                <select
                  className="input"
                  value={ruleForm.scope}
                  onChange={(event) =>
                    setRuleForm({ ...ruleForm, scope: event.target.value as RuleScope })
                  }
                >
                  <option value="platform">platform</option>
                  <option value="merchant">merchant</option>
                  <option value="channel">channel</option>
                </select>
              </Field>
              {/* 平台级规则没有归属，这两栏留着只会让人以为填了有用。 */}
              {ruleForm.scope !== 'platform' && (
                <>
                  <Field label={t('ops.pricing.owner')}>
                    <input
                      className="input"
                      value={ruleForm.owner}
                      onChange={(event) => setRuleForm({ ...ruleForm, owner: event.target.value })}
                    />
                  </Field>
                  <Field label={t('ops.pricing.channel')}>
                    <select
                      className="input"
                      value={ruleForm.channel}
                      onChange={(event) => setRuleForm({ ...ruleForm, channel: event.target.value })}
                    >
                      <option value="0">{t('ops.pricing.platformWide')}</option>
                      {channels.data?.map((channel) => (
                        <option key={channel.id} value={String(channel.id)}>
                          {channel.name} (#{channel.id})
                        </option>
                      ))}
                    </select>
                  </Field>
                </>
              )}
            </div>
            <Field label={t('ops.pricing.patterns')}>
              <textarea
                className="input"
                value={ruleForm.patterns}
                onChange={(event) => setRuleForm({ ...ruleForm, patterns: event.target.value })}
              />
            </Field>
            <ExprEditor
              value={ruleForm.expr}
              onChange={(expr) => setRuleForm({ ...ruleForm, expr })}
              onErrorChange={setRuleExprError}
            />
            <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 12 }}>
              <Field label={t('ops.pricing.priority')}>
                <input
                  className="input"
                  value={ruleForm.priority}
                  onChange={(event) => setRuleForm({ ...ruleForm, priority: event.target.value })}
                />
              </Field>
              <Field label={t('ops.pricing.enabled')}>
                <input
                  type="checkbox"
                  checked={ruleForm.enabled}
                  onChange={(event) => setRuleForm({ ...ruleForm, enabled: event.target.checked })}
                />
              </Field>
            </div>
            <InputError message={ruleError} />
          </form>
        </Modal>
      )}

      {(costCreating || costEditing) && (
        <Modal
          sheetSize="lg"
          title={costEditing ? t('ops.pricing.editCost') : t('ops.pricing.createCost')}
          onClose={requestCloseCost}
          footer={
            <>
              <Button
                variant="secondary"
                onClick={requestCloseCost}
              >
                {t('common.cancel')}
              </Button>
              <Button type="submit" form="cost-form">
                {costEditing ? t('common.save') : t('ops.create')}
              </Button>
            </>
          }
        >
          <form id="cost-form" onSubmit={submitCost} style={{ display: 'grid', gap: 12 }}>
            <Field label={t('ops.pricing.channel')} hint={t('ops.pricing.channelHint')}>
              <select
                className="input"
                value={costForm.channel}
                onChange={(event) => setCostForm({ ...costForm, channel: event.target.value })}
              >
                {costForm.channel &&
                  !channels.data?.some((channel) => String(channel.id) === costForm.channel) && (
                    <option value={costForm.channel}>#{costForm.channel}</option>
                  )}
                {channels.data?.map((channel) => (
                  <option key={channel.id} value={String(channel.id)}>
                    {channel.name} (#{channel.id})
                  </option>
                ))}
              </select>
            </Field>
            <Field label={t('ops.pricing.patterns')}>
              <textarea
                className="input"
                value={costForm.patterns}
                onChange={(event) => setCostForm({ ...costForm, patterns: event.target.value })}
              />
            </Field>
            <ExprEditor
              value={costForm.expr}
              onChange={(expr) => setCostForm({ ...costForm, expr })}
              onErrorChange={setCostExprError}
            />
            <Field label={t('ops.pricing.priority')}>
              <input
                className="input"
                value={costForm.priority}
                onChange={(event) => setCostForm({ ...costForm, priority: event.target.value })}
              />
            </Field>
            <InputError message={costError} />
          </form>
        </Modal>
      )}

      {confirm.dialog}
    </>
  )
}
