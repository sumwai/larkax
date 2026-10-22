import { render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { I18nProvider } from '../../i18n'
import { SiteProvider } from '../../site/SiteProvider'
import { ToastProvider } from '../../components/ui'
import { todayInZone } from '../../lib/format'
import type { MerchantOverview as OverviewData, UnhealthyReason } from '../../api/types'
import { MerchantOverview } from './MerchantOverview'

const mocks = vi.hoisted(() => ({
  overview: vi.fn(),
  meta: vi.fn(),
}))

/** 会话角色由用例改；页面据此换主语。 */
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
      merchant: { overview: mocks.overview },
      meta: mocks.meta,
      session: vi.fn().mockResolvedValue(null),
    },
  }
})

const { ApiError } = await import('../../api')

function data(overrides: Partial<OverviewData> = {}): OverviewData {
  return {
    channels: 2,
    models_in_supply: 3,
    today: { requests: 5, cost: '1.2345' },
    unhealthy_channels: [],
    ...overrides,
  }
}

function renderPage() {
  return render(
    <MemoryRouter>
      <I18nProvider>
        <ToastProvider>
          <SiteProvider>
            <MerchantOverview />
          </SiteProvider>
        </ToastProvider>
      </I18nProvider>
    </MemoryRouter>,
  )
}

/** 取某个读数所在的卡片，避免在整页里找同一个数字。 */
function card(label: string): HTMLElement {
  const node = screen.getByText(label).closest('.kpi')
  if (!node) throw new Error(`没有找到「${label}」所在的卡片`)
  return node as HTMLElement
}

beforeEach(() => {
  auth.role = 'merchant'
  mocks.overview.mockReset()
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

describe('商家概览', () => {
  it('四个读数各自取响应里的值', async () => {
    mocks.overview.mockResolvedValue(data())
    renderPage()

    await screen.findByText('我的渠道')
    expect(card('我的渠道')).toHaveTextContent('2')
    expect(card('我的在供模型')).toHaveTextContent('3')
    expect(card('我的今日请求')).toHaveTextContent('5')
    expect(card('我的今日消耗')).toHaveTextContent('1.234')
    // 反方向也要断：标题用 mine、副标题误用 site 这种混源不该能过。
    expect(screen.queryByText(/全站/)).not.toBeInTheDocument()
  })

  it('零值照常显示，不换成空态或占位符', async () => {
    mocks.overview.mockResolvedValue(
      data({ channels: 0, models_in_supply: 0, today: { requests: 0, cost: '0' } }),
    )
    renderPage()

    await screen.findByText('我的渠道')
    expect(card('我的渠道')).toHaveTextContent('0')
    expect(card('我的在供模型')).toHaveTextContent('0')
    expect(card('我的今日请求')).toHaveTextContent('0')
    expect(card('我的今日消耗')).toHaveTextContent('0.000')
  })

  it('今日一栏写明是站点当地的日历日，金额写明售价口径', async () => {
    mocks.overview.mockResolvedValue(data())
    renderPage()

    const today = todayInZone('Asia/Shanghai')
    await screen.findByText('我的今日请求')
    expect(await screen.findByText(`${today}（站点当地日期）`)).toBeInTheDocument()
    expect(card('我的今日消耗')).toHaveTextContent('按售价计')
    expect(card('我的今日消耗')).toHaveTextContent(`${today}（站点当地日期）`)
  })

  it('时区还没取到时不写日期，读数照常显示', async () => {
    mocks.meta.mockReturnValue(new Promise(() => undefined))
    mocks.overview.mockResolvedValue(data())
    renderPage()

    expect(await screen.findByText('我的渠道')).toBeInTheDocument()
    expect(screen.queryByText(/站点当地日期/)).not.toBeInTheDocument()
  })

  it('异常渠道按 reasons 逐条出人话', async () => {
    mocks.overview.mockResolvedValue(
      data({
        unhealthy_channels: [
          { id: 7, name: 'acme-primary', reasons: ['failure_rate', 'quota_state'] },
        ],
      }),
    )
    renderPage()

    expect(await screen.findByText('失败率偏高')).toBeInTheDocument()
    expect(screen.getByText('额度状态异常')).toBeInTheDocument()
    expect(screen.getByText('acme-primary')).toBeInTheDocument()
    expect(screen.getByText('#7')).toBeInTheDocument()
  })

  // 少翻一种，真实数据里就会露出枚举原值。
  it.each([
    ['failure_rate', '失败率偏高'],
    ['quota_state', '额度状态异常'],
    ['probe_failed', '最近一次探测失败'],
    ['consecutive_failures', '连续失败次数过多'],
    ['no_requests_24h', '24 小时内没有请求'],
  ] as const)('理由 %s 翻成「%s」', async (reason: UnhealthyReason, text: string) => {
    mocks.overview.mockResolvedValue(
      data({ unhealthy_channels: [{ id: 1, name: 'acme', reasons: [reason] }] }),
    )
    renderPage()

    expect(await screen.findByText(text)).toBeInTheDocument()
    // 枚举原值不该出现在页面上。
    expect(screen.queryByText(reason)).not.toBeInTheDocument()
  })

  it('没见过的理由按「其他异常」显示，不把枚举甩给用户', async () => {
    mocks.overview.mockResolvedValue(
      data({
        unhealthy_channels: [
          { id: 1, name: 'acme', reasons: ['billing_hold' as UnhealthyReason] },
        ],
      }),
    )
    renderPage()

    const label = await screen.findByText('其他异常')
    // 原值不进正文，但留在 title 里：悬停与读屏能看到，后端加枚举时留下痕迹。
    expect(label).toHaveAttribute('title', 'billing_hold')
    expect(screen.queryByText('billing_hold')).not.toBeInTheDocument()
  })

  it('没有异常渠道时给「全部渠道正常」，不是一块空白', async () => {
    mocks.overview.mockResolvedValue(data({ unhealthy_channels: [] }))
    renderPage()

    expect(await screen.findByText('全部渠道正常')).toBeInTheDocument()
    expect(screen.queryByText('需要关注的渠道')).not.toBeInTheDocument()
  })

  it('管理员看到的是全站主语，页面上不出现「我的」', async () => {
    auth.role = 'admin'
    mocks.overview.mockResolvedValue(data({ channels: 9, models_in_supply: 8 }))
    renderPage()

    // 标题在 loading 阶段就渲染（PageHead 与 loading 无关），等它等到的是骨架屏；
    // 数据相关的元素要各自等到，否则快机器上过、慢机器上红。
    for (const label of ['全站渠道', '全站在供模型', '全站今日请求', '全站今日消耗']) {
      expect(await screen.findByText(label)).toBeInTheDocument()
    }
    expect(await screen.findByRole('heading', { name: '全站概览' })).toBeInTheDocument()
    expect(card('全站渠道')).toHaveTextContent('9')
    expect(screen.queryByText(/我的/)).not.toBeInTheDocument()
  })

  it('加载态、错误态与空态互不混淆', async () => {
    mocks.overview.mockReturnValue(new Promise(() => undefined))
    const first = renderPage()
    expect(screen.getByLabelText('正在加载运营数据')).toBeInTheDocument()
    expect(screen.queryByText('全部渠道正常')).not.toBeInTheDocument()
    first.unmount()

    mocks.overview.mockRejectedValue(new ApiError(503, 'unavailable', 'upstream down'))
    const second = renderPage()
    expect(await screen.findByText('数据加载失败。')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '重试' })).toBeInTheDocument()
    expect(screen.queryByLabelText('正在加载运营数据')).not.toBeInTheDocument()
    expect(screen.queryByText('全部渠道正常')).not.toBeInTheDocument()
    second.unmount()

    mocks.overview.mockResolvedValue(data())
    renderPage()
    expect(await screen.findByText('全部渠道正常')).toBeInTheDocument()
    expect(screen.queryByLabelText('正在加载运营数据')).not.toBeInTheDocument()
    expect(screen.queryByText('数据加载失败。')).not.toBeInTheDocument()
  })
})
