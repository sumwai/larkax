import { act, fireEvent, render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { AuthProvider, useAuth } from './AuthProvider'

const mocks = vi.hoisted(() => ({
  session: vi.fn(),
  setCsrfToken: vi.fn(),
  logout: vi.fn(),
  unauthorized: null as (() => void) | null,
}))

vi.mock('../api', () => ({
  api: {
    session: mocks.session,
    setCsrfToken: mocks.setCsrfToken,
    logout: mocks.logout,
    setUnauthorizedHandler: (handler: (() => void) | null) => {
      mocks.unauthorized = handler
    },
  },
}))

function Probe() {
  const { status, sessionExpired, logout } = useAuth()
  return (
    <>
      <span>
        status:{status} expired:{String(sessionExpired)}
      </span>
      <button type="button" onClick={() => void logout()}>
        退出
      </button>
    </>
  )
}

const SESSION = {
  user: {
    id: 1,
    username: 'demo',
    display_name: null,
    email: null,
    status: 'active',
    created_at: '2026-01-01T00:00:00Z',
  },
  csrf_token: 'csrf',
  expires_at: '2026-12-01T00:00:00Z',
}

describe('AuthProvider 的失效回调', () => {
  beforeEach(() => {
    mocks.session.mockReset()
    mocks.setCsrfToken.mockReset()
    mocks.logout.mockReset()
    mocks.unauthorized = null
  })

  it('只降级一个真的存在的会话：会话还在恢复时不提前定成匿名', async () => {
    let resolveSession!: (value: unknown) => void
    mocks.session.mockReturnValue(
      new Promise((resolve) => {
        resolveSession = resolve
      }),
    )
    render(
      <AuthProvider>
        <Probe />
      </AuthProvider>,
    )
    expect(await screen.findByText('status:loading expired:false')).toBeInTheDocument()
    expect(mocks.unauthorized).not.toBeNull()

    // 探测还没回来就收到 401：这不能当成「会话失效」，状态留在 loading。
    act(() => mocks.unauthorized?.())
    expect(screen.getByText('status:loading expired:false')).toBeInTheDocument()

    await act(async () => resolveSession(SESSION))
    expect(screen.getByText('status:authenticated expired:false')).toBeInTheDocument()

    act(() => mocks.unauthorized?.())
    // `expired` 说的是「掉下来的原因」：门槛靠它区分「先问一句」与「直接换页」。
    expect(screen.getByText('status:anonymous expired:true')).toBeInTheDocument()
    // 会话没了，令牌也不该留着让下一条写请求带着它出门。
    expect(mocks.setCsrfToken).toHaveBeenCalledWith(null)
  })

  it('主动退出登录不算会话过期', async () => {
    mocks.session.mockResolvedValue(SESSION)
    mocks.logout.mockResolvedValue(undefined)
    render(
      <AuthProvider>
        <Probe />
      </AuthProvider>,
    )
    expect(await screen.findByText('status:authenticated expired:false')).toBeInTheDocument()

    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: '退出' }))
    })

    expect(await screen.findByText('status:anonymous expired:false')).toBeInTheDocument()
  })
})
