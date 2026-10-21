import { render, screen, within } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { I18nProvider } from '../../i18n'
import { SiteProvider } from '../../site/SiteProvider'
import { ToastProvider } from '../../components/ui'
import type { MerchantModel, MerchantModelsResponse } from '../../api/types'
import { MerchantModels } from './MerchantModels'

const mocks = vi.hoisted(() => ({
  listSuppliedModels: vi.fn(),
  meta: vi.fn(),
}))

const auth = vi.hoisted(() => ({ role: 'merchant' as 'merchant' | 'admin' }))

vi.mock('../../auth/AuthProvider', () => ({
  useAuth: () => ({
    status: 'authenticated',
    user: {
      id: 2,
      username: 'shop',
      display_name: 'shop',
      email: null,
      status: 'active',
      role: auth.role,
      created_at: '2026-01-01T00:00:00Z',
    },
    login: vi.fn(),
    logout: vi.fn(),
    setUser: vi.fn(),
  }),
}))

vi.mock('../../api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../api')>()
  return {
    ...actual,
    api: {
      merchant: { listSuppliedModels: mocks.listSuppliedModels },
      meta: mocks.meta,
      session: vi.fn().mockResolvedValue(null),
    },
  }
})

const { ApiError } = await import('../../api')

const MODELS: MerchantModel[] = [
  { model: 'gpt-4.1', display_name: 'GPT-4.1', channels: 2, priced: true },
  { model: 'claude-sonnet-4-5', display_name: 'Claude Sonnet 4.5', channels: 1, priced: false },
]

function page(overrides: Partial<MerchantModelsResponse> = {}): MerchantModelsResponse {
  return { items: MODELS, total: MODELS.length, truncated: false, ...overrides }
}

function renderPage() {
  return render(
    <MemoryRouter>
      <I18nProvider>
        <ToastProvider>
          <SiteProvider>
            <MerchantModels />
          </SiteProvider>
        </ToastProvider>
      </I18nProvider>
    </MemoryRouter>,
  )
}

function row(model: string): HTMLElement {
  const node = screen.getByRole('row', { name: new RegExp(model) })
  return node
}

beforeEach(() => {
  auth.role = 'merchant'
  mocks.listSuppliedModels.mockReset()
  mocks.meta.mockReset()
  mocks.meta.mockResolvedValue({
    site_name: 'larkax',
    api_base_url: 'https://api.example.com/v1',
    currency: 'USD',
    timezone: 'Asia/Shanghai',
    locales: ['en', 'zh'],
    features: { registration: false, password_reset: 'admin', topup: false },
  })
})

describe('商家模型', () => {
  it('列出规范名、展示名与在供渠道数', async () => {
    mocks.listSuppliedModels.mockResolvedValue(page())
    renderPage()

    const first = await screen.findByRole('row', { name: /gpt-4\.1/ })
    expect(first).toHaveTextContent('GPT-4.1')
    expect(first).toHaveTextContent('2')
  })

  it('priced 只出「已定价 / 未定价」两种文案', async () => {
    mocks.listSuppliedModels.mockResolvedValue(page())
    renderPage()

    await screen.findByRole('row', { name: /gpt-4\.1/ })
    expect(within(row('gpt-4.1')).getByText('已定价')).toBeInTheDocument()
    expect(within(row('claude-sonnet-4-5')).getByText('未定价')).toBeInTheDocument()
  })

  it('管理员视野下主语是全站，页面上不出现「我的」', async () => {
    auth.role = 'admin'
    mocks.listSuppliedModels.mockResolvedValue(page())
    renderPage()

    expect(await screen.findByRole('heading', { name: '全站模型' })).toBeInTheDocument()
    // 已定价说的是「有商家配过」，不是「我的价」。
    expect(within(row('gpt-4.1')).getByText('已定价')).toBeInTheDocument()
    expect(screen.queryByText(/我的/)).not.toBeInTheDocument()
  })

  it('清单是有表头的数据表，脚注留在表外', async () => {
    mocks.listSuppliedModels.mockResolvedValue(page({ total: 7, truncated: true }))
    renderPage()

    const table = await screen.findByRole('table', { name: '我的模型' })
    expect(within(table).getAllByRole('columnheader')).toHaveLength(3)
    // 表头 + 两行数据。
    expect(within(table).getAllByRole('row')).toHaveLength(3)
    expect(within(table).queryByText('共 7 个，只显示前 2 个。')).not.toBeInTheDocument()
    expect(screen.getByText('共 7 个，只显示前 2 个。')).toBeInTheDocument()
  })

  it('truncated 为真时说明裁到了哪里', async () => {
    mocks.listSuppliedModels.mockResolvedValue(page({ total: 7, truncated: true }))
    renderPage()

    expect(await screen.findByText('共 7 个，只显示前 2 个。')).toBeInTheDocument()
  })

  it('truncated 为假时没有那段说明', async () => {
    mocks.listSuppliedModels.mockResolvedValue(page({ total: 2, truncated: false }))
    renderPage()

    await screen.findByRole('row', { name: /gpt-4\.1/ })
    expect(screen.queryByText(/只显示前/)).not.toBeInTheDocument()
  })

  it('空态指到接渠道的那一页', async () => {
    mocks.listSuppliedModels.mockResolvedValue({ items: [], total: 0, truncated: false })
    renderPage()

    expect(await screen.findByText('还没有在供模型')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: '去接一条渠道' })).toHaveAttribute(
      'href',
      '/app/merchant/channels',
    )
  })

  it('管理员空态指向管理侧的渠道页', async () => {
    auth.role = 'admin'
    mocks.listSuppliedModels.mockResolvedValue({ items: [], total: 0, truncated: false })
    renderPage()

    expect(await screen.findByRole('link', { name: '去接一条渠道' })).toHaveAttribute(
      'href',
      '/app/admin/channels',
    )
  })

  it('加载态、错误态与空态互不混淆', async () => {
    mocks.listSuppliedModels.mockReturnValue(new Promise(() => undefined))
    const first = renderPage()
    expect(screen.getByLabelText('正在加载运营数据')).toBeInTheDocument()
    expect(screen.queryByText('还没有在供模型')).not.toBeInTheDocument()
    first.unmount()

    mocks.listSuppliedModels.mockRejectedValue(new ApiError(503, 'unavailable', 'upstream down'))
    const second = renderPage()
    expect(await screen.findByText('数据加载失败。')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '重试' })).toBeInTheDocument()
    expect(screen.queryByLabelText('正在加载运营数据')).not.toBeInTheDocument()
    second.unmount()

    mocks.listSuppliedModels.mockResolvedValue({ items: [], total: 0, truncated: false })
    renderPage()
    expect(await screen.findByText('还没有在供模型')).toBeInTheDocument()
    expect(screen.queryByLabelText('正在加载运营数据')).not.toBeInTheDocument()
    expect(screen.queryByText('数据加载失败。')).not.toBeInTheDocument()
  })
})
