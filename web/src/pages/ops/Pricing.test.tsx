import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { I18nProvider } from '../../i18n'
import { SiteProvider } from '../../site/SiteProvider'
import { ToastProvider } from '../../components/ui'
import { OpsPricing } from './Pricing'

const mocks = vi.hoisted(() => ({
  listRules: vi.fn(),
  listCosts: vi.fn(),
  listChannels: vi.fn(),
  createRule: vi.fn(),
  updateRule: vi.fn(),
  createCost: vi.fn(),
  meta: vi.fn(),
}))

vi.mock('../../api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../api')>()
  const ops = {
    listRules: mocks.listRules,
    listCosts: mocks.listCosts,
    listChannels: mocks.listChannels,
    createRule: mocks.createRule,
    updateRule: mocks.updateRule,
    createCost: mocks.createCost,
  }
  return {
    ...actual,
    api: {
      admin: ops,
      meta: mocks.meta,
      session: vi.fn().mockResolvedValue(null),
    },
  }
})

// 与商家页共用这一句（同一个 i18n 键）；两边的用例断的是同一串字。
const PRICE_NOTE =
  '清单里的单价是每 100 万 token 的金额，币种 USD。换算不出单价的表达式按原文显示，并标上「原文」。'

const RULE = {
  id: 1,
  scope: 'platform',
  owner_user_id: 0,
  channel_id: 0,
  name: 'standard',
  model_patterns: ['claude-*'],
  expr: 'tokens * 1',
  priority: 0,
  effective_from: '2026-01-01T00:00:00Z',
  effective_to: null,
  enabled: true,
  created_by: 0,
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z',
  declared_keys: [],
}

const COST = {
  id: 7,
  channel_id: 0,
  model_patterns: ['claude-*'],
  expr: '{"prompt": 0.000003}',
  priority: 0,
  effective_from: '2026-01-01T00:00:00Z',
  effective_to: null,
  created_at: '2026-01-01T00:00:00Z',
  declared_keys: [],
}

function renderPricing() {
  return render(
    <I18nProvider>
      <ToastProvider>
        <SiteProvider>
          <OpsPricing />
        </SiteProvider>
      </ToastProvider>
    </I18nProvider>,
  )
}

async function openRuleForm() {
  fireEvent.click(await screen.findByRole('button', { name: '新建规则' }))
  return screen.getByRole('dialog', { name: '新建规则' })
}

function fillRuleBasics(dialog: HTMLElement, expr = 'tokens * 2') {
  fireEvent.change(within(dialog).getByLabelText('名称'), { target: { value: 'bargain' } })
  fireEvent.change(within(dialog).getByLabelText('模型匹配式（每行一个）'), {
    target: { value: 'claude-*' },
  })
  // 新建时默认是表单模式；要填一段自由写的表达式，先切过去。
  fireEvent.click(within(dialog).getByRole('button', { name: '表达式' }))
  fireEvent.change(within(dialog).getByLabelText('表达式'), { target: { value: expr } })
}

function fillRows(dialog: HTMLElement, values: Record<string, string>) {
  for (const [label, value] of Object.entries(values)) {
    fireEvent.change(within(dialog).getByLabelText(label), { target: { value } })
  }
}

describe('计价页', () => {
  beforeEach(() => {
    Object.values(mocks).forEach((mock) => mock.mockReset())
    mocks.listRules.mockResolvedValue([])
    mocks.listCosts.mockResolvedValue([])
    mocks.listChannels.mockResolvedValue([])
    mocks.meta.mockResolvedValue({
      site_name: 'larkax',
      api_base_url: 'https://api.example.com/v1',
      currency: 'USD',
      timezone: 'Asia/Shanghai',
      locales: ['en', 'zh'],
      features: { registration: false, password_reset: 'admin', topup: false },
    })
  })

  it('平台级规则不显示归属与渠道两栏，提交时两个 ID 都是 0', async () => {
    renderPricing()
    const dialog = await openRuleForm()
    expect(screen.queryByLabelText('归属用户 ID')).not.toBeInTheDocument()
    expect(screen.queryByLabelText('渠道')).not.toBeInTheDocument()

    fillRuleBasics(dialog)
    fireEvent.click(screen.getByRole('button', { name: '创建' }))

    const confirm = screen.getByRole('dialog', { name: '确认创建' })
    fireEvent.click(within(confirm).getByRole('button', { name: '创建' }))
    await waitFor(() =>
      expect(mocks.createRule).toHaveBeenCalledWith({
        name: 'bargain',
        scope: 'platform',
        owner_user_id: 0,
        channel_id: 0,
        model_patterns: ['claude-*'],
        expr: 'tokens * 2',
        priority: 0,
        enabled: true,
      }),
    )
  })

  it('五行填满后，提交的 expr 是拼好的 JSON 串', async () => {
    renderPricing()
    const dialog = await openRuleForm()
    fireEvent.change(within(dialog).getByLabelText('名称'), { target: { value: 'bargain' } })
    fireEvent.change(within(dialog).getByLabelText('模型匹配式（每行一个）'), {
      target: { value: 'example-model' },
    })
    fillRows(dialog, {
      输入: '3.5',
      输出: '15',
      缓存读: '1.5',
      缓存写: '1.5',
      推理: '6',
    })

    fireEvent.click(screen.getByRole('button', { name: '创建' }))
    const confirm = screen.getByRole('dialog', { name: '确认创建' })
    fireEvent.click(within(confirm).getByRole('button', { name: '创建' }))

    await waitFor(() =>
      expect(mocks.createRule).toHaveBeenCalledWith(
        expect.objectContaining({
          expr: '{"prompt": 0.0000035, "completion": 0.000015, "cache_read": 0.0000015, "cache_write": 0.0000015, "reasoning": 0.000006}',
        }),
      ),
    )
  })

  it('一项单价都没填时拦在本地，不发请求', async () => {
    renderPricing()
    const dialog = await openRuleForm()
    fireEvent.change(within(dialog).getByLabelText('名称'), { target: { value: 'bargain' } })
    fireEvent.change(within(dialog).getByLabelText('模型匹配式（每行一个）'), {
      target: { value: 'example-model' },
    })

    fireEvent.submit(document.getElementById('rule-form') as HTMLFormElement)

    expect(
      await screen.findByText(
        '至少要有一项单价；这次请求用到的每种 token 都要有单价，否则结算时会失败。',
      ),
    ).toBeInTheDocument()
    expect(mocks.createRule).not.toHaveBeenCalled()
  })

  it('归属用户不是整数时挡在本地，不把 NaN 当成 0 发出去', async () => {
    renderPricing()
    const dialog = await openRuleForm()
    fireEvent.change(screen.getByLabelText('作用域'), { target: { value: 'merchant' } })
    fillRuleBasics(dialog)
    fireEvent.change(screen.getByLabelText('归属用户 ID'), { target: { value: 'abc' } })

    fireEvent.submit(document.getElementById('rule-form') as HTMLFormElement)

    expect(await screen.findByText('这一栏要填整数。')).toBeInTheDocument()
    expect(mocks.createRule).not.toHaveBeenCalled()
  })

  it('搜索只保留匹配的规则', async () => {
    mocks.listRules.mockResolvedValue([
      RULE,
      { ...RULE, id: 2, name: 'wholesale', model_patterns: ['gpt-*'] },
    ])
    renderPricing()

    expect(await screen.findByText('standard')).toBeInTheDocument()
    expect(screen.getByText('wholesale')).toBeInTheDocument()

    fireEvent.change(screen.getByLabelText('搜索'), { target: { value: 'whole' } })
    expect(screen.queryByText('standard')).not.toBeInTheDocument()
    expect(screen.getByText('wholesale')).toBeInTheDocument()
  })

  it('搜索框与模型列表由同一个组件产出，一键清空后回到全量并保持聚焦', async () => {
    mocks.listRules.mockResolvedValue([
      RULE,
      { ...RULE, id: 2, name: 'wholesale', model_patterns: ['gpt-*'] },
    ])
    renderPricing()
    expect(await screen.findByText('standard')).toBeInTheDocument()

    // 模型列表页断的是同一个类名与 data-testid，只多一个左侧放大镜。
    const box = screen.getByTestId('search-box')
    expect(box.className).toBe('wrap-rel search-box')

    const search = screen.getByLabelText('搜索') as HTMLInputElement
    fireEvent.change(search, { target: { value: 'whole' } })
    expect(screen.queryByText('standard')).not.toBeInTheDocument()

    fireEvent.click(within(box).getByRole('button', { name: '清空搜索' }))

    expect(search.value).toBe('')
    expect(screen.getByText('standard')).toBeInTheDocument()
    expect(document.activeElement).toBe(search)
  })

  it('规则行按每 100 万 token 显示，换算不出来的按原文并带「原文」角标', async () => {
    const tiered = 'len <= 32000 ? {"prompt": 0.0000035} : {"prompt": 0.00001}'
    mocks.listRules.mockResolvedValue([
      { ...RULE, expr: '{"prompt": 0.0000035}' },
      { ...RULE, id: 2, name: 'tiny', expr: '{"prompt": 0.0000001}' },
      { ...RULE, id: 3, name: 'tiered', expr: tiered },
    ])
    renderPricing()

    // 1e-7 乘 1e6 是 0.1；3.5 那档乘出来恰好干净，证明不了没走浮点。
    expect(await screen.findByText('{"prompt": 0.1}')).toBeInTheDocument()
    expect(screen.getByText('{"prompt": 3.5}')).toBeInTheDocument()
    expect(screen.queryByText('{"prompt": 0.0000001}')).not.toBeInTheDocument()
    // `cell-ellipsis` 会截断，完整的一段挂在 title 上。
    expect(screen.getByText('{"prompt": 0.1}')).toHaveAttribute('title', '{"prompt": 0.1}')
    // 带条件的表达式不是一张单价表：显示原文，旁边挂可见的「原文」角标。
    expect(screen.getByText(tiered)).toBeInTheDocument()
    expect(screen.getByText(tiered)).toHaveAttribute('title', tiered)
    // 换算成功的那两行没有角标，只有换算不出来的这一行有。
    expect(screen.getAllByText('原文')).toHaveLength(1)
    // 角标自己带一句解释：它只说「原文」，为什么这么显示要挂在 title 上。
    expect(screen.getByText('原文')).toHaveAttribute(
      'title',
      '这段表达式换算不出每 100 万 token 的单价，按原文显示。',
    )
    // 单位与币种写在页面上，与商家页同一句。
    expect(screen.getByText(PRICE_NOTE)).toBeInTheDocument()
  })

  it('成本行与规则行同口径：每 100 万 token、同一句提示、换算不了带角标', async () => {
    const tiered = 'len <= 32000 ? {"prompt": 0.00002} : {"prompt": 0.00006}'
    mocks.listCosts.mockResolvedValue([
      { ...COST, expr: '{"prompt": 0.0000001}' },
      { ...COST, id: 8, expr: '{"prompt": 0.00002, "completion": 0.00006}' },
      { ...COST, id: 9, expr: tiered },
    ])
    renderPricing()

    fireEvent.click(await screen.findByRole('button', { name: '成本规则' }))

    // 1e-7 这一档：十进制换算得 0.1，浮点乘是 0.09999999999999999。
    expect(await screen.findByText('{"prompt": 0.1}')).toBeInTheDocument()
    expect(screen.getByText('{"prompt": 20, "completion": 60}')).toBeInTheDocument()
    expect(screen.queryByText('{"prompt": 0.0000001}')).not.toBeInTheDocument()
    expect(screen.getByText(tiered)).toHaveAttribute('title', tiered)
    expect(screen.getAllByText('原文')).toHaveLength(1)
    expect(screen.getByText(PRICE_NOTE)).toBeInTheDocument()
  })
})
