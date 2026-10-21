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

function fillRuleBasics(dialog: HTMLElement) {
  fireEvent.change(within(dialog).getByLabelText('名称'), { target: { value: 'bargain' } })
  fireEvent.change(within(dialog).getByLabelText('模型匹配式（每行一个）'), {
    target: { value: 'claude-*' },
  })
  fireEvent.change(within(dialog).getByLabelText('表达式'), { target: { value: 'tokens * 2' } })
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
})
