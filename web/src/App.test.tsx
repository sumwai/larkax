import { fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { App } from './App'

const mocks = vi.hoisted(() => ({
  session: vi.fn(),
  listChannels: vi.fn(),
  listModels: vi.fn(),
  models: vi.fn(),
  listRules: vi.fn(),
  overview: vi.fn(),
  listUpstreams: vi.fn(),
  listSuppliedModels: vi.fn(),
  meta: vi.fn(),
}))

vi.mock('./api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./api')>()
  const ops = {
    listChannels: mocks.listChannels,
    listModels: mocks.listModels,
    listRules: mocks.listRules,
    overview: mocks.overview,
    listUpstreams: mocks.listUpstreams,
    listSuppliedModels: mocks.listSuppliedModels,
  }
  return {
    ...actual,
    isMockMode: false,
    api: {
      admin: ops,
      merchant: ops,
      models: mocks.models,
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
    mocks.models.mockResolvedValue([])
    mocks.listRules.mockResolvedValue([])
    mocks.overview.mockResolvedValue({
      channels: 1,
      models_in_supply: 1,
      today: { requests: 2, cost: '0.1200' },
      unhealthy_channels: [],
    })
    mocks.listUpstreams.mockResolvedValue({ items: [], total: 0, truncated: false })
    mocks.listSuppliedModels.mockResolvedValue({ items: [], total: 0, truncated: false })
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

  it('/app/merchant 落到概览页', async () => {
    await open('merchant', '/app/merchant')
    expect(await screen.findByRole('heading', { name: '我的概览' })).toBeInTheDocument()
    expect(window.location.pathname).toBe('/app/merchant/overview')
  })

  it('merchant 进得去商家概览，主语是「我的」', async () => {
    await open('merchant', '/app/merchant/overview')
    expect(await screen.findByRole('heading', { name: '我的概览' })).toBeInTheDocument()
    expect(mocks.overview).toHaveBeenCalled()
  })

  it.each(['/app/merchant/overview', '/app/merchant/models', '/app/merchant/upstreams'])(
    'user 进不了 %s，也不发运营请求',
    async (path) => {
      await open('user', path)
      expect(await screen.findByText('这个视图不在你的权限范围内。')).toBeInTheDocument()
      expect(mocks.overview).not.toHaveBeenCalled()
      expect(mocks.listUpstreams).not.toHaveBeenCalled()
      expect(mocks.listSuppliedModels).not.toHaveBeenCalled()
    },
  )

  it('admin 进得去概览、模型与上游三页，主语写成全站', async () => {
    await open('admin', '/app/merchant/overview')
    expect(await screen.findByRole('heading', { name: '全站概览' })).toBeInTheDocument()
    expect(mocks.overview).toHaveBeenCalled()
    // 切换器跟着地址走：这一页在商家视图里。
    expect(screen.getByRole('button', { name: '商家' })).toHaveAttribute('aria-pressed', 'true')
    // 侧栏只列三种主语的那三页，渠道与计价不出现。
    expect(screen.queryByRole('link', { name: '渠道' })).not.toBeInTheDocument()
    expect(screen.queryByRole('link', { name: '计价' })).not.toBeInTheDocument()
    expect(screen.getAllByRole('link', { name: '上游' }).length).toBeGreaterThan(0)
  })

  /**
   * 侧栏里第一个「设置」入口。
   *
   * 用户卡和右上角头像也用同一个名字，并且排在同一份 DOM 的后面。
   */
  function sidebarSettings() {
    return screen.getAllByRole('link', { name: '设置' })[0]
  }

  it.each([
    ['user', '/app/models', '/app/settings'],
    ['merchant', '/app/merchant/channels', '/app/merchant/settings'],
    ['admin', '/app/admin/channels', '/app/admin/settings'],
  ] as const)('%s 点设置后停在原视图', async (role, home, expected) => {
    await open(role, home)
    fireEvent.click(sidebarSettings())

    expect(await screen.findByRole('heading', { name: '设置' })).toBeInTheDocument()
    expect(window.location.pathname).toBe(expected)
    if (role === 'user') {
      expect(document.querySelector('.view-switch')).toBeNull()
      return
    }
    const label = role === 'merchant' ? '商家' : '管理'
    expect(screen.getByRole('button', { name: label })).toHaveAttribute('aria-pressed', 'true')
  })

  it('admin 的切换器有三个视图，停在管理上', async () => {
    await open('admin', '/app/admin/channels')

    expect(screen.getByRole('button', { name: '用户' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '商家' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '管理' })).toHaveAttribute('aria-pressed', 'true')
  })

  it.each(['/app/merchant/channels', '/app/merchant/pricing', '/app/merchant/settings'])(
    'admin 直接敲 %s 落到无权限页',
    async (path) => {
      await open('admin', path)

      expect(await screen.findByText('这个视图不在你的权限范围内。')).toBeInTheDocument()
      expect(screen.getByRole('link', { name: '回到概览' })).toHaveAttribute('href', '/app')
      // 无权限页不该顺手把数据也拉一遍。
      expect(mocks.listChannels).not.toHaveBeenCalled()
      expect(mocks.listRules).not.toHaveBeenCalled()
      expect(mocks.overview).not.toHaveBeenCalled()
    },
  )

  it('merchant 仍是用户与商家两个视图', async () => {
    await open('merchant', '/app/merchant/channels')

    expect(screen.getByRole('button', { name: '用户' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '商家' })).toHaveAttribute('aria-pressed', 'true')
    expect(screen.queryByRole('button', { name: '管理' })).not.toBeInTheDocument()
    // 两个页面本身是否进得去由上面「有计价页」「进得到自己的渠道页」两条钉住。
    expect(await screen.findByText('还没有渠道')).toBeInTheDocument()
  })

  it('无权限页留一个回到概览的出口', async () => {
    await open('merchant', '/app/admin/channels')
    expect(await screen.findByText('这个视图不在你的权限范围内。')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: '回到概览' })).toHaveAttribute('href', '/app')
  })
})
