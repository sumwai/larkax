import { render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { App } from './App'

const mocks = vi.hoisted(() => ({
  session: vi.fn(),
  listChannels: vi.fn(),
  listModels: vi.fn(),
  listRules: vi.fn(),
  meta: vi.fn(),
}))

vi.mock('./api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./api')>()
  const ops = {
    listChannels: mocks.listChannels,
    listModels: mocks.listModels,
    listRules: mocks.listRules,
  }
  return {
    ...actual,
    isMockMode: false,
    api: {
      admin: ops,
      merchant: ops,
      meta: mocks.meta,
      session: mocks.session,
      setCsrfToken: vi.fn(),
      logout: vi.fn(),
    },
  }
})

function sessionOf(role: 'user' | 'merchant' | 'admin') {
  return {
    user: {
      id: 1,
      username: `${role}-account`,
      display_name: `${role}-account`,
      email: null,
      status: 'active',
      role,
      created_at: '2026-01-01T00:00:00Z',
    },
    csrf_token: 'csrf',
    expires_at: '2026-12-01T00:00:00Z',
  }
}

/** App 用的是 BrowserRouter，路径只能从地址栏给。 */
async function open(role: 'user' | 'merchant' | 'admin', path: string) {
  window.history.pushState({}, '', path)
  mocks.session.mockResolvedValue(sessionOf(role))
  render(<App />)
  await screen.findAllByText(`${role}-account`)
}

describe('运营视图的角色门槛', () => {
  beforeEach(() => {
    Object.values(mocks).forEach((mock) => mock.mockReset())
    mocks.listChannels.mockResolvedValue([])
    mocks.listModels.mockResolvedValue([])
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

  afterEach(() => {
    window.history.pushState({}, '', '/')
  })

  it('user 直接进 admin 页面时只看到无权限，不发运营请求', async () => {
    await open('user', '/app/admin/channels')
    expect(await screen.findByText('这个视图不在你的权限范围内。')).toBeInTheDocument()
    expect(mocks.listChannels).not.toHaveBeenCalled()
  })

  it('merchant 有计价页，页上没有成本规则', async () => {
    await open('merchant', '/app/merchant/pricing')
    expect(await screen.findByRole('heading', { name: '计价' })).toBeInTheDocument()
    expect(screen.queryByText('成本规则')).not.toBeInTheDocument()
    expect(mocks.listRules).toHaveBeenCalled()
  })

  it('/app/merchant/costs 在商家视图里没有这一页', async () => {
    // 这一页没有路由，落到最外层的 404，所以不经过 AppShell（没有账号名可等）。
    window.history.pushState({}, '', '/app/merchant/costs')
    mocks.session.mockResolvedValue(sessionOf('merchant'))
    render(<App />)
    expect(await screen.findByText('没有找到这条记录。')).toBeInTheDocument()
  })

  it('merchant 进得到自己的渠道页', async () => {
    await open('merchant', '/app/merchant/channels')
    expect(await screen.findByText('还没有渠道')).toBeInTheDocument()
    expect(mocks.listChannels).toHaveBeenCalled()
  })

  it('/app/merchant 落到渠道页', async () => {
    await open('merchant', '/app/merchant')
    expect(await screen.findByText('还没有渠道')).toBeInTheDocument()
  })

  it('无权限页留一个回到概览的出口', async () => {
    await open('merchant', '/app/admin/channels')
    expect(await screen.findByText('这个视图不在你的权限范围内。')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: '回到概览' })).toHaveAttribute('href', '/app')
  })
})
