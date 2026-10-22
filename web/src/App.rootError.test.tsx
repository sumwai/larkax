import { render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { App } from './App'

const mocks = vi.hoisted(() => ({
  session: vi.fn(),
  meta: vi.fn(),
}))

// 根级页面（落地页、登录页、404）没有内层错误边界。让落地页在渲染期抛错，
// 就是转成 data router 之后最容易丢掉的那条路。
vi.mock('./pages/Landing', () => ({
  Landing: () => {
    throw new Error('landing exploded')
  },
}))

vi.mock('./api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./api')>()
  return {
    ...actual,
    isMockMode: false,
    api: {
      ...actual.api,
      session: mocks.session,
      meta: mocks.meta,
      setCsrfToken: vi.fn(),
      setUnauthorizedHandler: vi.fn(),
    },
  }
})

describe('根路由的错误页', () => {
  beforeEach(() => {
    Object.values(mocks).forEach((mock) => mock.mockReset())
    mocks.session.mockResolvedValue(null)
    mocks.meta.mockResolvedValue({
      site_name: 'larkax',
      api_base_url: 'https://api.example.com/v1',
      currency: 'USD',
      timezone: 'UTC',
      locales: ['en', 'zh'],
      features: { registration: false, password_reset: 'admin', topup: false },
    })
    window.history.pushState({}, '', '/')
  })

  it('根级页面渲染抛错时仍然显示致命错误页，而不是 router 自带的白页', async () => {
    render(<App />)

    expect(await screen.findByText('应用出错了')).toBeInTheDocument()
    // 路由内层的错误页是另一张（「这个页面出错了」），这里不该出现。
    expect(screen.queryByText('这个页面出错了')).not.toBeInTheDocument()
  })
})
