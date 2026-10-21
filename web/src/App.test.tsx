import { render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { App } from './App'

const mocks = vi.hoisted(() => ({
  session: vi.fn(),
  listChannels: vi.fn(),
  listModels: vi.fn(),
  meta: vi.fn(),
}))

vi.mock('./api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./api')>()
  const ops = {
    listChannels: mocks.listChannels,
    listModels: mocks.listModels,
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

  it('merchant 进得到自己的渠道页', async () => {
    await open('merchant', '/app/merchant/channels')
    expect(await screen.findByText('还没有渠道')).toBeInTheDocument()
    expect(mocks.listChannels).toHaveBeenCalled()
  })
})
