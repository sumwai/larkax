import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { App } from './App'
import { ApiError } from './api'
import type { ChannelOpsClient } from './api/types'

const mocks = vi.hoisted(() => ({
  session: vi.fn(),
  login: vi.fn(),
  listChannels: vi.fn(),
  getChannel: vi.fn(),
  createChannel: vi.fn(),
  updateChannel: vi.fn(),
  listAdapters: vi.fn(),
  probeModels: vi.fn(),
  listModels: vi.fn(),
  models: vi.fn(),
  listRules: vi.fn(),
  overview: vi.fn(),
  listUpstreams: vi.fn(),
  listSuppliedModels: vi.fn(),
  meta: vi.fn(),
  updateMe: vi.fn(),
  changePassword: vi.fn(),
}))

/** `AuthProvider` 注册进来的失效回调；用例用它模拟「业务请求撞上 401」。 */
const auth = vi.hoisted(() => ({ unauthorized: null as (() => void) | null }))

vi.mock('./api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./api')>()
  // 渠道那一组按接口标全：页面每次 mount 都会读适配器清单，替身缺一个方法就是
  // 一次 unhandled，而且只有跑到那个页面才会现形。标成 ChannelOpsClient 之后，
  // 下次漏一个方法在 tsc 阶段就红。
  const channelOps: ChannelOpsClient = {
    listChannels: mocks.listChannels,
    getChannel: mocks.getChannel,
    createChannel: mocks.createChannel,
    updateChannel: mocks.updateChannel,
    listAdapters: mocks.listAdapters,
    probeModels: mocks.probeModels,
  }
  const ops = {
    ...channelOps,
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
      login: mocks.login,
      updateMe: mocks.updateMe,
      changePassword: mocks.changePassword,
      setCsrfToken: vi.fn(),
      logout: vi.fn(),
      setUnauthorizedHandler: (handler: (() => void) | null) => {
        auth.unauthorized = handler
      },
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
    mocks.listAdapters.mockResolvedValue([])
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

  it('merchant 进得去账号页，设置页只剩界面偏好', async () => {
    await open('merchant', '/app/merchant/account')
    expect(await screen.findByRole('heading', { name: '账号', level: 1 })).toBeInTheDocument()
    fireEvent.click(screen.getAllByRole('link', { name: '设置' })[0])
    expect(await screen.findByRole('heading', { name: '设置', level: 1 })).toBeInTheDocument()
    expect(screen.queryByText('修改口令')).not.toBeInTheDocument()
  })

  it('merchant 进得去商家概览，主语是「我的」', async () => {
    await open('merchant', '/app/merchant/overview')
    expect(await screen.findByRole('heading', { name: '我的概览' })).toBeInTheDocument()
    expect(mocks.overview).toHaveBeenCalled()
  })

  it.each([
    '/app/merchant/overview',
    '/app/merchant/models',
    '/app/merchant/upstreams',
    '/app/merchant/account',
  ])(
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
    // 侧栏只列三种主语的那三页，渠道与计价不出现。底栏另有一套入口
    // （「渠道」在管理前缀下），所以这条要限定在侧栏里看。
    const sidebar = within(document.querySelector('.sidebar') as HTMLElement)
    expect(sidebar.queryByRole('link', { name: '渠道' })).not.toBeInTheDocument()
    expect(sidebar.queryByRole('link', { name: '计价' })).not.toBeInTheDocument()
    expect(sidebar.getAllByRole('link', { name: '上游' }).length).toBeGreaterThan(0)
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

  it.each([
    '/app/merchant/channels',
    '/app/merchant/pricing',
    '/app/merchant/settings',
    '/app/merchant/account',
  ])('admin 直接敲 %s 落到无权限页',
    async (path) => {
      await open('admin', path)

      expect(await screen.findByText('这个视图不在你的权限范围内。')).toBeInTheDocument()
      expect(screen.getByRole('link', { name: '回到概览' })).toHaveAttribute('href', '/app')
      // 无权限页不该顺手把数据也拉一遍。
      expect(mocks.listChannels).not.toHaveBeenCalled()
      expect(mocks.listRules).not.toHaveBeenCalled()
      expect(mocks.overview).not.toHaveBeenCalled()
      expect(mocks.updateMe).not.toHaveBeenCalled()
      expect(mocks.changePassword).not.toHaveBeenCalled()
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

describe('会话失效后的引导', () => {
  beforeEach(() => {
    Object.values(mocks).forEach((mock) => mock.mockReset())
    auth.unauthorized = null
    mocks.listChannels.mockResolvedValue([])
    mocks.listAdapters.mockResolvedValue([])
    mocks.listModels.mockResolvedValue([])
    mocks.models.mockResolvedValue([])
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

  it('会话失效后跳到登录页，next 是原地址，登录后回到那里', async () => {
    await open('user', '/app/settings')

    // 连着三下：同一轮会话里三个请求各自 401 时，后到的不能把 `next` 覆盖掉。
    act(() => auth.unauthorized?.())
    act(() => auth.unauthorized?.())
    act(() => auth.unauthorized?.())

    expect(window.location.pathname).toBe('/login')
    expect(new URLSearchParams(window.location.search).get('next')).toBe('/app/settings')

    mocks.login.mockResolvedValue(sessionOf('user'))
    fireEvent.change(await screen.findByLabelText('用户名'), { target: { value: 'user-account' } })
    fireEvent.change(screen.getByLabelText('口令'), { target: { value: 'a-long-password' } })
    fireEvent.click(screen.getByRole('button', { name: '登录' }))

    await waitFor(() => expect(window.location.pathname).toBe('/app/settings'))
  })

  it('匿名态停在落地页时，晚到的 401 不把人赶去登录页', async () => {
    window.history.pushState({}, '', '/')
    mocks.session.mockResolvedValue(null)
    render(<App />)

    expect(await screen.findByRole('heading', { name: '一个密钥，接入全部主流模型' })).toBeInTheDocument()
    expect(auth.unauthorized).not.toBeNull()
    act(() => auth.unauthorized?.())

    expect(window.location.pathname).toBe('/')
    expect(screen.queryByLabelText('用户名')).toBeNull()
  })

  it('403 留在原页：会话是好的，只是这次操作没权限', async () => {
    window.history.pushState({}, '', '/app/admin/channels')
    mocks.session.mockResolvedValue(sessionOf('admin'))
    mocks.listChannels.mockRejectedValue(new ApiError(403, 'forbidden', 'nope'))
    render(<App />)

    expect(await screen.findByText('没有权限访问这个资源。')).toBeInTheDocument()
    expect(window.location.pathname).toBe('/app/admin/channels')
  })

  it('登录页口令错时留在登录页，next 也不丢', async () => {
    window.history.pushState({}, '', '/login?next=%2Fapp%2Fsettings')
    mocks.session.mockResolvedValue(null)
    mocks.login.mockRejectedValue(new ApiError(401, 'invalid_credentials', 'nope'))
    render(<App />)

    fireEvent.change(await screen.findByLabelText('用户名'), { target: { value: 'demo' } })
    fireEvent.change(screen.getByLabelText('口令'), { target: { value: 'wrong-password' } })
    fireEvent.click(screen.getByRole('button', { name: '登录' }))

    expect(
      await screen.findByText('用户名或口令不对。检查一下输入，或者联系运营确认账号。'),
    ).toBeInTheDocument()
    expect(window.location.pathname).toBe('/login')
    expect(new URLSearchParams(window.location.search).get('next')).toBe('/app/settings')
  })
})
