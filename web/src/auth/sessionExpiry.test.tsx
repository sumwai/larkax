import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { App } from '../App'
import type { HttpApiClient } from '../api/client'

/**
 * 这一组用真的 HTTP 客户端，只把 `fetch` 换成按路径回应的替身。
 *
 * 「业务请求撞上 401 → 跳登录页」横跨三层：客户端的判定、AuthProvider 的降级、
 * 门槛的跳转。只给其中一层做替身，把层与层之间的接线改断了也不会红。
 */
const state = vi.hoisted(() => ({
  client: null as unknown as HttpApiClient,
  fetchImpl: vi.fn(),
}))

vi.mock('../api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../api')>()
  state.client = new actual.HttpApiClient({
    baseUrl: '/api/v1',
    fetchImpl: state.fetchImpl as unknown as typeof fetch,
  })
  return { ...actual, isMockMode: false, api: state.client }
})

function respond(status: number, body: unknown): Response {
  return {
    status,
    ok: status >= 200 && status < 300,
    statusText: String(status),
    json: async () => body,
    headers: new Headers(),
  } as unknown as Response
}

const SESSION = {
  user: {
    id: 1,
    username: 'admin-account',
    display_name: 'admin-account',
    email: null,
    status: 'active',
    role: 'admin',
    created_at: '2026-01-01T00:00:00Z',
  },
  csrf_token: 'csrf',
  expires_at: '2026-12-01T00:00:00Z',
}

const expired = () => respond(401, { error: { code: 'unauthenticated' } })

describe('会话失效的端到端路径', () => {
  beforeEach(async () => {
    state.fetchImpl.mockReset()
    // 客户端带着上一轮的「已通知」标志；一次成功的会话恢复把它清零。
    state.fetchImpl.mockResolvedValue(respond(200, SESSION))
    await state.client.session()
  })

  afterEach(() => {
    window.history.pushState({}, '', '/')
  })

  it('业务请求 401 跳到登录页，next 是原地址，登录后回到那里', async () => {
    window.history.pushState({}, '', '/app/admin/channels')
    let sessionExpired = true
    state.fetchImpl.mockImplementation(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url.endsWith('/auth/session')) return respond(200, SESSION)
      if (url.endsWith('/auth/login')) {
        sessionExpired = false
        return respond(200, SESSION)
      }
      if (url.includes('/admin/channels')) return sessionExpired ? expired() : respond(200, [])
      if (url.includes('/admin/adapters')) return respond(200, { items: [] })
      return respond(200, [])
    })

    render(<App />)

    await waitFor(() => expect(window.location.pathname).toBe('/login'))
    expect(new URLSearchParams(window.location.search).get('next')).toBe('/app/admin/channels')

    fireEvent.change(await screen.findByLabelText('用户名'), {
      target: { value: 'admin-account' },
    })
    fireEvent.change(screen.getByLabelText('口令'), { target: { value: 'a-long-password' } })
    fireEvent.click(screen.getByRole('button', { name: '登录' }))

    await waitFor(() => expect(window.location.pathname).toBe('/app/admin/channels'))
  })

  it('403 留在原页：会话是好的，只是这次操作没权限', async () => {
    window.history.pushState({}, '', '/app/admin/channels')
    state.fetchImpl.mockImplementation(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url.endsWith('/auth/session')) return respond(200, SESSION)
      if (url.includes('/admin/channels')) {
        return respond(403, { error: { code: 'forbidden', message: 'nope' } })
      }
      if (url.includes('/admin/adapters')) return respond(200, { items: [] })
      return respond(200, [])
    })

    render(<App />)

    expect(await screen.findByText('没有权限访问这个资源。')).toBeInTheDocument()
    expect(window.location.pathname).toBe('/app/admin/channels')
  })
})
