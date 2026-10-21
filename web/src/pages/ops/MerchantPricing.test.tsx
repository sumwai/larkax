import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { I18nProvider } from '../../i18n'
import { SiteProvider } from '../../site/SiteProvider'
import { ToastProvider } from '../../components/ui'
import { MerchantPricing } from './MerchantPricing'

const mocks = vi.hoisted(() => ({
  listRules: vi.fn(),
  getRule: vi.fn(),
  createRule: vi.fn(),
  updateRule: vi.fn(),
  meta: vi.fn(),
}))

vi.mock('../../api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../api')>()
  return {
    ...actual,
    api: {
      merchant: {
        listRules: mocks.listRules,
        getRule: mocks.getRule,
        createRule: mocks.createRule,
        updateRule: mocks.updateRule,
      },
      meta: mocks.meta,
      session: vi.fn().mockResolvedValue(null),
    },
  }
})

const { ApiError } = await import('../../api')

const RULE = {
  id: 11,
  scope: 'merchant',
  owner_user_id: 1,
  channel_id: 0,
  name: 'own-price',
  model_patterns: ['claude-*'],
  expr: 'input_tokens * 0.003',
  priority: 0,
  effective_from: '2026-01-01T00:00:00Z',
  effective_to: null,
  enabled: true,
  created_by: 1,
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z',
  declared_keys: [],
}

function renderPricing() {
  return render(
    <I18nProvider>
      <ToastProvider>
        <SiteProvider>
          <MerchantPricing />
        </SiteProvider>
      </ToastProvider>
    </I18nProvider>,
  )
}

async function fillAndSubmit(name: string) {
  const [first] = await screen.findAllByRole('button', { name: '新建规则' })
  fireEvent.click(first)
  const form = screen.getByRole('dialog', { name: '新建规则' })
  fireEvent.change(within(form).getByLabelText('名称'), { target: { value: name } })
  fireEvent.change(within(form).getByLabelText('模型匹配式（每行一个）'), {
    target: { value: 'claude-*' },
  })
  fireEvent.change(within(form).getByLabelText('表达式'), {
    target: { value: 'input_tokens * 0.003' },
  })
  fireEvent.submit(document.getElementById('merchant-rule-form') as HTMLFormElement)
  return form
}

describe('商家计价页', () => {
  beforeEach(() => {
    Object.values(mocks).forEach((mock) => mock.mockReset())
    mocks.listRules.mockResolvedValue([])
    mocks.meta.mockResolvedValue({
      site_name: 'larkax',
      api_base_url: 'https://api.example.com/v1',
      currency: 'USD',
      timezone: 'Asia/Shanghai',
      locales: ['en', 'zh'],
      features: { registration: false, password_reset: 'admin', topup: false },
    })
  })

  it('新建的规则出现在自己的清单里', async () => {
    mocks.createRule.mockResolvedValue(11)
    mocks.listRules.mockResolvedValueOnce([]).mockResolvedValue([RULE])
    renderPricing()

    expect(await screen.findByText('还没有自己的计价规则')).toBeInTheDocument()
    await fillAndSubmit('own-price')

    const confirm = screen.getByRole('dialog', { name: '确认创建' })
    fireEvent.click(within(confirm).getByRole('button', { name: '创建' }))

    await waitFor(() =>
      expect(mocks.createRule).toHaveBeenCalledWith({
        name: 'own-price',
        model_patterns: ['claude-*'],
        expr: 'input_tokens * 0.003',
        priority: 0,
        enabled: true,
      }),
    )
    expect(await screen.findByText('own-price')).toBeInTheDocument()
  })

  it('编辑只发可变字段，作用域与归属不进请求体', async () => {
    mocks.listRules.mockResolvedValue([RULE])
    mocks.updateRule.mockResolvedValue(undefined)
    renderPricing()

    fireEvent.click(await screen.findByRole('button', { name: '编辑' }))
    const form = screen.getByRole('dialog', { name: '编辑规则' })
    fireEvent.change(within(form).getByLabelText('表达式'), {
      target: { value: 'input_tokens * 0.004' },
    })
    fireEvent.submit(document.getElementById('merchant-rule-form') as HTMLFormElement)

    const confirm = screen.getByRole('dialog', { name: '确认保存' })
    fireEvent.click(within(confirm).getByRole('button', { name: '保存' }))

    await waitFor(() => expect(mocks.updateRule).toHaveBeenCalled())
    const [id, payload] = mocks.updateRule.mock.calls[0]
    expect(id).toBe(11)
    expect(payload).toEqual({
      name: 'own-price',
      model_patterns: ['claude-*'],
      expr: 'input_tokens * 0.004',
      priority: 0,
      enabled: true,
    })
    expect(payload).not.toHaveProperty('scope')
    expect(payload).not.toHaveProperty('owner_user_id')
    expect(payload).not.toHaveProperty('channel_id')
  })

  it('改别人的或平台的规则拿 404 时，页面给中文说明', async () => {
    mocks.listRules.mockResolvedValue([RULE])
    mocks.updateRule.mockRejectedValue(new ApiError(404, 'not_found', 'not found'))
    renderPricing()

    fireEvent.click(await screen.findByRole('button', { name: '编辑' }))
    const form = screen.getByRole('dialog', { name: '编辑规则' })
    fireEvent.change(within(form).getByLabelText('表达式'), { target: { value: 'x * 1' } })
    fireEvent.submit(document.getElementById('merchant-rule-form') as HTMLFormElement)
    const confirm = screen.getByRole('dialog', { name: '确认保存' })
    fireEvent.click(within(confirm).getByRole('button', { name: '保存' }))

    expect(
      await screen.findByText('没有找到这个资源，可能已被删除，或者不属于你。'),
    ).toBeInTheDocument()
    expect(screen.queryByText('not found')).not.toBeInTheDocument()
  })
})
