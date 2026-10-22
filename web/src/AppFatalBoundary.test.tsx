import { render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { App } from './App'

const mocks = vi.hoisted(() => ({ session: vi.fn(), meta: vi.fn() }))

vi.mock('./api', () => ({
  isMockMode: false,
  api: {
    session: mocks.session,
    meta: mocks.meta,
    setCsrfToken: vi.fn(),
    logout: vi.fn(),
  },
}))

// 外壳是整个 App 里最外一层的实际内容；它抛错时只有顶层边界能接住。
vi.mock('./components/AppShell', () => ({
  AppShell: (): never => {
    throw new Error('shell exploded')
  },
}))

describe('App 的最外层边界', () => {
  beforeEach(() => {
    mocks.session.mockReset()
    mocks.session.mockResolvedValue({
      user: {
        id: 1,
        username: 'user-account',
        display_name: 'user-account',
        email: null,
        status: 'active',
        role: 'user',
        created_at: '2026-01-01T00:00:00Z',
      },
      csrf_token: 'csrf',
      expires_at: '2026-12-01T00:00:00Z',
    })
    mocks.meta.mockReset()
    mocks.meta.mockResolvedValue({
      site_name: 'larkax',
      api_base_url: 'https://api.example.com/v1',
      currency: 'USD',
      timezone: 'Asia/Shanghai',
      locales: ['en', 'zh'],
      features: { registration: false, password_reset: 'admin', topup: false },
    })
    window.history.pushState({}, '', '/app')
    vi.spyOn(console, 'error').mockImplementation(() => {})
  })

  afterEach(() => {
    vi.restoreAllMocks()
    window.history.pushState({}, '', '/')
  })

  it('外壳渲染抛错时显示兜底内容，而不是白屏', async () => {
    render(<App />)

    expect(await screen.findByRole('heading', { name: '应用出错了' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '重新加载' })).toBeInTheDocument()
    expect(screen.getByText(/shell exploded/)).toBeInTheDocument()
  })
})
