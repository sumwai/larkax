import { render, screen, within } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { I18nProvider } from '../../i18n'
import { SiteProvider } from '../../site/SiteProvider'
import { ToastProvider } from '../../components/ui'
import type { MerchantUpstream, MerchantUpstreamsResponse } from '../../api/types'
import { MerchantUpstreams } from './MerchantUpstreams'
import { mockViewport } from '../../test/media'

const mocks = vi.hoisted(() => ({
  listUpstreams: vi.fn(),
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
      merchant: { listUpstreams: mocks.listUpstreams },
      meta: mocks.meta,
      session: vi.fn().mockResolvedValue(null),
    },
  }
})

const { ApiError } = await import('../../api')

/** 同一地址下两个账号，组健康取最差的一档。 */
const UPSTREAM: MerchantUpstream = {
  type: 'openai',
  base_url: 'https://api.example.com/v1',
  health: 'drained',
  requests: 400,
  failures: 100,
  failure_rate: 0.25,
  channels: [
    { id: 1, name: 'acme-primary', status: 'enabled', health: 'healthy' },
    { id: 2, name: 'acme-backup', status: 'disabled', health: 'degraded' },
  ],
}

function page(overrides: Partial<MerchantUpstreamsResponse> = {}): MerchantUpstreamsResponse {
  return { items: [UPSTREAM], total: 1, truncated: false, ...overrides }
}

function renderPage() {
  return render(
    <MemoryRouter>
      <I18nProvider>
        <ToastProvider>
          <SiteProvider>
            <MerchantUpstreams />
          </SiteProvider>
        </ToastProvider>
      </I18nProvider>
    </MemoryRouter>,
  )
}

beforeEach(() => {
  auth.role = 'merchant'
  mocks.listUpstreams.mockReset()
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

describe('商家上游', () => {
  it('按 (type, base_url) 成组，组内多个账号并列', async () => {
    mocks.listUpstreams.mockResolvedValue(page())
    renderPage()

    const base = await screen.findByText('https://api.example.com/v1')
    const group = base.closest('article') as HTMLElement
    expect(group).toHaveTextContent('openai')
    expect(within(group).getByText('acme-primary')).toBeInTheDocument()
    expect(within(group).getByText('#1')).toBeInTheDocument()
    expect(within(group).getByText('acme-backup')).toBeInTheDocument()
    expect(within(group).getByText('#2')).toBeInTheDocument()
    // 反方向也要断：标题用 mine、副标题误用 site 这种混源不该能过。
    expect(screen.queryByText(/全站/)).not.toBeInTheDocument()
  })

  // 后端给 0..1 的小数，页面只换单位；两端各核一次。
  it.each([
    [0, '0.0%'],
    [0.25, '25.0%'],
    [1, '100.0%'],
  ])('失败率 %s 显示为 %s', async (rate: number, text: string) => {
    mocks.listUpstreams.mockResolvedValue(
      page({ items: [{ ...UPSTREAM, failure_rate: rate }] }),
    )
    renderPage()

    expect(await screen.findByText(text)).toBeInTheDocument()
  })

  it('健康档位有文字，不只靠颜色', async () => {
    mocks.listUpstreams.mockResolvedValue(page())
    renderPage()

    await screen.findByText('https://api.example.com/v1')
    expect(screen.getByText('已耗尽')).toBeInTheDocument()
    expect(screen.getByText('正常')).toBeInTheDocument()
    expect(screen.getByText('降级')).toBeInTheDocument()
  })

  it('渠道的启停状态与健康分开显示', async () => {
    mocks.listUpstreams.mockResolvedValue(page())
    renderPage()

    const group = (await screen.findByText('https://api.example.com/v1')).closest(
      'article',
    ) as HTMLElement
    expect(within(group).getByText('已启用')).toBeInTheDocument()
    expect(within(group).getByText('已停用')).toBeInTheDocument()
  })

  it('组内渠道是有表头的数据表', async () => {
    mocks.listUpstreams.mockResolvedValue(page())
    renderPage()

    const table = await screen.findByRole('table', {
      name: 'openai https://api.example.com/v1',
    })
    expect(within(table).getAllByRole('columnheader')).toHaveLength(3)
    // 表头 + 两个账号。
    expect(within(table).getAllByRole('row')).toHaveLength(3)
  })

  it('truncated 为真时说明裁到了哪里', async () => {
    mocks.listUpstreams.mockResolvedValue(page({ total: 5, truncated: true }))
    renderPage()

    expect(await screen.findByText('共 5 组，只显示前 1 组。')).toBeInTheDocument()
  })

  it('truncated 为假时没有那段说明', async () => {
    mocks.listUpstreams.mockResolvedValue(page({ total: 1, truncated: false }))
    renderPage()

    await screen.findByText('https://api.example.com/v1')
    expect(screen.queryByText(/只显示前/)).not.toBeInTheDocument()
  })

  it('管理员的主语是全站，页面上不出现「我的」', async () => {
    auth.role = 'admin'
    mocks.listUpstreams.mockResolvedValue(page())
    renderPage()

    expect(await screen.findByRole('heading', { name: '全站上游' })).toBeInTheDocument()
    expect(screen.queryByText(/我的/)).not.toBeInTheDocument()
  })

  it('空态指到接渠道的那一页', async () => {
    mocks.listUpstreams.mockResolvedValue({ items: [], total: 0, truncated: false })
    renderPage()

    expect(await screen.findByText('还没有上游')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: '去接一条渠道' })).toHaveAttribute(
      'href',
      '/app/merchant/channels',
    )
  })

  it('加载态、错误态与空态互不混淆', async () => {
    mocks.listUpstreams.mockReturnValue(new Promise(() => undefined))
    const first = renderPage()
    expect(screen.getByLabelText('正在加载运营数据')).toBeInTheDocument()
    expect(screen.queryByText('还没有上游')).not.toBeInTheDocument()
    first.unmount()

    mocks.listUpstreams.mockRejectedValue(new ApiError(503, 'unavailable', 'upstream down'))
    const second = renderPage()
    expect(await screen.findByText('数据加载失败。')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '重试' })).toBeInTheDocument()
    expect(screen.queryByLabelText('正在加载运营数据')).not.toBeInTheDocument()
    second.unmount()

    mocks.listUpstreams.mockResolvedValue({ items: [], total: 0, truncated: false })
    renderPage()
    expect(await screen.findByText('还没有上游')).toBeInTheDocument()
    expect(screen.queryByLabelText('正在加载运营数据')).not.toBeInTheDocument()
    expect(screen.queryByText('数据加载失败。')).not.toBeInTheDocument()
  })
})

describe('商家上游：窄屏卡片', () => {
  it('组内渠道改成卡片，桌面表格的列头不在 DOM 里', async () => {
    const restore = mockViewport(375, 667)
    try {
      mocks.listUpstreams.mockResolvedValue(page())
      renderPage()

      const list = await screen.findByRole('region', {
        name: 'openai https://api.example.com/v1',
      })
      expect(within(list).getByText('acme-primary')).toBeInTheDocument()
      expect(within(list).getByText('acme-backup')).toBeInTheDocument()
      expect(within(list).getByText('#2')).toBeInTheDocument()
      expect(within(list).getByText('已停用')).toBeInTheDocument()
      expect(screen.queryAllByRole('columnheader')).toHaveLength(0)
    } finally {
      restore()
    }
  })
})
