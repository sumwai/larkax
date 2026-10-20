import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { Login } from './Login'
import { AuthProvider } from '../auth/AuthProvider'
import { I18nProvider } from '../i18n'
import { ThemeProvider } from '../theme/ThemeProvider'
import { ToastProvider } from '../components/ui'

const mocks = vi.hoisted(() => ({
  session: vi.fn(),
  login: vi.fn(),
  setCsrfToken: vi.fn(),
  meta: vi.fn(),
}))

vi.mock('../api', () => ({
  api: {
    mode: 'http',
    session: mocks.session,
    login: mocks.login,
    setCsrfToken: mocks.setCsrfToken,
    meta: mocks.meta,
  },
}))

function renderLogin() {
  return render(
    <I18nProvider>
      <ThemeProvider>
        <ToastProvider>
          <AuthProvider>
            <MemoryRouter initialEntries={['/login']}>
              <Routes>
                <Route path="/login" element={<Login />} />
                <Route path="/app" element={<div>app home</div>} />
              </Routes>
            </MemoryRouter>
          </AuthProvider>
        </ToastProvider>
      </ThemeProvider>
    </I18nProvider>,
  )
}

describe('Login', () => {
  beforeEach(() => {
    mocks.session.mockResolvedValue(null)
    mocks.login.mockResolvedValue({
      user: {
        id: 1,
        username: 'demo',
        display_name: '演示',
        email: null,
        status: 'active',
        created_at: '',
      },
      csrf_token: 'csrf-1',
      expires_at: '2026-10-28T00:00:00Z',
    })
    mocks.setCsrfToken.mockReset()
  })

  it('空表单提交时两个字段各给一条错误，不发请求', async () => {
    const user = userEvent.setup()
    renderLogin()

    await screen.findByLabelText('用户名')
    await user.click(screen.getByRole('button', { name: '登录' }))

    expect(await screen.findByText('请输入用户名')).toBeInTheDocument()
    expect(screen.getByText('请输入口令')).toBeInTheDocument()
    expect(mocks.login).not.toHaveBeenCalled()
  })

  it('填好用户名与口令后登录，并把 csrf_token 交给客户端', async () => {
    const user = userEvent.setup()
    renderLogin()

    await user.type(await screen.findByLabelText('用户名'), 'demo')
    await user.type(screen.getByLabelText('口令'), 'a-long-password')
    await user.click(screen.getByRole('button', { name: '登录' }))

    expect(await screen.findByText('app home')).toBeInTheDocument()
    expect(mocks.login).toHaveBeenCalledWith('demo', 'a-long-password')
    expect(mocks.setCsrfToken).toHaveBeenCalledWith('csrf-1')
  })

  it('口令不对时留在登录页，并说明下一步', async () => {
    mocks.login.mockRejectedValueOnce(new Error('invalid credentials'))
    const user = userEvent.setup()
    renderLogin()

    await user.type(await screen.findByLabelText('用户名'), 'demo')
    await user.type(screen.getByLabelText('口令'), 'wrong-password')
    await user.click(screen.getByRole('button', { name: '登录' }))

    expect(
      await screen.findByText('用户名或口令不对。检查一下输入，或者联系运营确认账号。'),
    ).toBeInTheDocument()
  })
})
