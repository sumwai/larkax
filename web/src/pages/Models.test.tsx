import { render, screen, within } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { I18nProvider } from '../i18n'
import { SiteProvider } from '../site/SiteProvider'
import { Models } from './Models'

const mocks = vi.hoisted(() => ({ models: vi.fn(), meta: vi.fn() }))

vi.mock('../api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../api')>()
  return {
    ...actual,
    api: {
      models: mocks.models,
      meta: mocks.meta,
      session: vi.fn().mockResolvedValue(null),
    },
  }
})

function modelOf(name: string, offers?: Array<{ merchant: string | null; price: string }>) {
  return {
    name,
    display_name: name.toUpperCase(),
    vendor: 'example',
    context_limit: 200_000,
    max_output: 64_000,
    capabilities: { tools: true },
    modalities: { input: ['text'], output: ['text'] },
    pricing: null,
    ...(offers ? { offers } : {}),
  }
}

function renderModels(items: ReturnType<typeof modelOf>[]) {
  mocks.models.mockResolvedValue({
    currency: 'USD',
    reference_at: '2026-01-01T00:00:00Z',
    items,
  })
  return render(
    <I18nProvider>
      <SiteProvider>
        <Models />
      </SiteProvider>
    </I18nProvider>,
  )
}

describe('用户视图的模型列表', () => {
  beforeEach(() => {
    Object.values(mocks).forEach((mock) => mock.mockReset())
    mocks.meta.mockResolvedValue({
      site_name: 'larkax',
      api_base_url: 'https://api.example.com/v1',
      currency: 'USD',
      timezone: 'Asia/Shanghai',
      locales: ['en', 'zh'],
      features: { registration: false, password_reset: 'admin', topup: false },
    })
  })

  it('报价按价格升序，平台自营显示「平台」，价格字符串原样展示', async () => {
    renderModels([
      modelOf('claude-sonnet-4-5', [
        { merchant: 'northwind', price: '3.150000' },
        { merchant: null, price: '2.800000' },
        { merchant: 'acme', price: '0.003500' },
      ]),
    ])

    const card = (await screen.findByText('CLAUDE-SONNET-4-5')).closest('article') as HTMLElement
    const offers = within(card).getByLabelText('提供该模型的商家')
    const merchants = Array.from(offers.querySelectorAll('.offers-row'))
      .slice(1)
      .map((row) => row.firstElementChild?.textContent)

    expect(merchants).toEqual(['acme', '平台', 'northwind'])
    // 价格是服务端给的字符串：小额价原样留着，不被格式化成 0.00。
    expect(within(offers).getByText('$0.003500')).toBeInTheDocument()
    expect(within(offers).getByText('$2.800000')).toBeInTheDocument()
  })

  it('没有报价的模型不显示提供方与价格两栏', async () => {
    renderModels([modelOf('gpt-4.1')])

    expect(await screen.findByText('GPT-4.1')).toBeInTheDocument()
    expect(screen.queryByLabelText('提供该模型的商家')).not.toBeInTheDocument()
    expect(screen.queryByText('提供方')).not.toBeInTheDocument()
    expect(screen.queryByText('平台')).not.toBeInTheDocument()
  })
})
