import { useState, type FormEvent } from 'react'
import { api } from '../../api'
import type { PricingCost, PricingRule, RuleScope } from '../../api/types'
import { useI18n } from '../../i18n'
import { useSite } from '../../site/SiteProvider'
import { useAsync } from '../../hooks/useAsync'
import { formatDateTime } from '../../lib/format'
import { Alert, Button, Empty, Modal, PageHead, Skeleton, useToast } from '../../components/ui'
import { IconPlus } from '../../components/icons'
import { Field, InputError, OpsLoadError, linesToList, listToLines, useConfirm } from './common'

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
  const [ruleError, setRuleError] = useState<string | null>(null)

  const [costEditing, setCostEditing] = useState<PricingCost | null>(null)
  const [costCreating, setCostCreating] = useState(false)
  const [costForm, setCostForm] = useState<CostForm>(EMPTY_COST)
  const [costError, setCostError] = useState<string | null>(null)

  const channelName = (id: number) =>
    id === 0 ? t('ops.pricing.platformWide') : (channels.data?.find((item) => item.id === id)?.name ?? `#${id}`)

  const openRule = (rule?: PricingRule) => {
    if (rule) {
      setRuleForm({
        name: rule.name,
        scope: rule.scope as RuleScope,
        owner: String(rule.owner_user_id),
        channel: String(rule.channel_id),
        patterns: listToLines(rule.model_patterns),
        expr: rule.expr,
        priority: String(rule.priority),
        enabled: rule.enabled,
      })
      setRuleEditing(rule)
      setRuleCreating(false)
    } else {
      setRuleForm(EMPTY_RULE)
      setRuleEditing(null)
      setRuleCreating(true)
    }
    setRuleError(null)
  }

  const openCost = (cost?: PricingCost) => {
    if (cost) {
      setCostForm({
        channel: String(cost.channel_id),
        patterns: listToLines(cost.model_patterns),
        expr: cost.expr,
        priority: String(cost.priority),
      })
      setCostEditing(cost)
      setCostCreating(false)
    } else {
      setCostForm({ ...EMPTY_COST, channel: String(channels.data?.[0]?.id ?? '') })
      setCostEditing(null)
      setCostCreating(true)
    }
    setCostError(null)
  }

  const submitRule = (event: FormEvent) => {
    event.preventDefault()
    const patterns = linesToList(ruleForm.patterns)
    if (!ruleForm.name.trim()) return setRuleError(t('ops.pricing.nameRequired'))
    if (patterns.length === 0) return setRuleError(t('ops.pricing.patternsRequired'))
    if (!ruleForm.expr.trim()) return setRuleError(t('ops.pricing.exprRequired'))
    if (!/^-?\d+$/.test(ruleForm.priority.trim())) return setRuleError(t('ops.invalidInteger'))

    if (ruleEditing) {
      confirm.request({
        title: t('ops.confirmSave'),
        body: t('ops.pricing.saveConfirm', { name: ruleEditing.name }),
        label: t('common.save'),
        run: async () => {
          await client.updateRule(ruleEditing.id, {
            name: ruleForm.name.trim(),
            scope: ruleForm.scope,
            owner_user_id: Number(ruleForm.owner),
            channel_id: Number(ruleForm.channel),
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
          owner_user_id: Number(ruleForm.owner),
          channel_id: Number(ruleForm.channel),
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
          {(rules.data?.length ?? 0) > 0 && (
            <section className="card panel" aria-label={t('ops.pricing.rules')}>
              {rules.data?.map((rule) => (
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
          {(costs.data?.length ?? 0) > 0 && (
            <section className="card panel" aria-label={t('ops.pricing.costs')}>
              {costs.data?.map((cost) => (
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
          title={ruleEditing ? t('ops.pricing.editRule') : t('ops.pricing.createRule')}
          onClose={() => {
            setRuleCreating(false)
            setRuleEditing(null)
          }}
          footer={
            <>
              <Button
                variant="secondary"
                onClick={() => {
                  setRuleCreating(false)
                  setRuleEditing(null)
                }}
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
            <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr 1fr', gap: 12 }}>
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
            </div>
            <Field label={t('ops.pricing.patterns')}>
              <textarea
                className="input"
                value={ruleForm.patterns}
                onChange={(event) => setRuleForm({ ...ruleForm, patterns: event.target.value })}
              />
            </Field>
            <Field label={t('ops.pricing.expr')}>
              <textarea
                className="input mono"
                value={ruleForm.expr}
                onChange={(event) => setRuleForm({ ...ruleForm, expr: event.target.value })}
              />
            </Field>
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
          title={costEditing ? t('ops.pricing.editCost') : t('ops.pricing.createCost')}
          onClose={() => {
            setCostCreating(false)
            setCostEditing(null)
          }}
          footer={
            <>
              <Button
                variant="secondary"
                onClick={() => {
                  setCostCreating(false)
                  setCostEditing(null)
                }}
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
            <Field label={t('ops.pricing.expr')}>
              <textarea
                className="input mono"
                value={costForm.expr}
                onChange={(event) => setCostForm({ ...costForm, expr: event.target.value })}
              />
            </Field>
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
