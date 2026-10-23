import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { AuthProvider } from '../auth/AuthProvider'
import { I18nProvider } from '../i18n'
import { SiteProvider } from '../site/SiteProvider'
import { Login } from './Login'

const mocks = vi.hoisted(() => ({
  session: vi.fn(),
  login: vi.fn(),
  meta: vi.fn(),
  setCsrfToken: vi.fn(),
  setUnauthorizedHandler: vi.fn(),
}))

vi.mock('../api', () => ({ api: mocks }))

const META = {
  site_name: 'larkax',
  api_base_url: 'https://api.example.com/v1',
  currency: 'USD',
  timezone: 'UTC',
  locales: ['zh', 'en'],
  features: { registration: false, password_reset: 'admin', topup: false },
}

function renderLogin() {
  return render(
    <I18nProvider>
      <MemoryRouter initialEntries={['/login']}>
        <SiteProvider>
          <AuthProvider>
            <Login />
          </AuthProvider>
        </SiteProvider>
      </MemoryRouter>
    </I18nProvider>,
  )
}

beforeEach(() => {
  localStorage.setItem('larkax.lang', 'zh')
  mocks.session.mockResolvedValue(null)
  mocks.meta.mockResolvedValue(META)
})

describe('登录页', () => {
  it('匿名进来渲染出用户名、口令与提交按钮', async () => {
    renderLogin()

    expect(await screen.findByRole('heading', { name: '登录控制台' })).toBeInTheDocument()
    expect(screen.getByLabelText('用户名')).toBeInTheDocument()
    expect(screen.getByLabelText('口令')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '登录' })).toBeInTheDocument()
  })

  it('空表单提交时两条必填提示都出来，且不发请求', async () => {
    const user = userEvent.setup()
    renderLogin()

    await user.click(await screen.findByRole('button', { name: '登录' }))

    expect(await screen.findByText('请输入用户名')).toBeInTheDocument()
    expect(screen.getByText('请输入口令')).toBeInTheDocument()
    expect(mocks.login).not.toHaveBeenCalled()
  })

  it('401 时说口令不对，而不是笼统的出错', async () => {
    mocks.login.mockRejectedValue(Object.assign(new Error('bad credentials'), { status: 401 }))
    const user = userEvent.setup()
    renderLogin()

    await user.type(await screen.findByLabelText('用户名'), 'alice')
    await user.type(screen.getByLabelText('口令'), 'wrong')
    await user.click(screen.getByRole('button', { name: '登录' }))

    expect(await screen.findByText('用户名或口令不对。')).toBeInTheDocument()
    expect(mocks.login).toHaveBeenCalledWith('alice', 'wrong')
    // 失败后表单还在原地，不是白屏，也不是「正在登录…」卡住。
    expect(screen.getByRole('button', { name: '登录' })).toBeEnabled()
  })

  it('网络或服务端出错时走通用错误文案', async () => {
    mocks.login.mockRejectedValue(new Error('offline'))
    const user = userEvent.setup()
    renderLogin()

    await user.type(await screen.findByLabelText('用户名'), 'alice')
    await user.type(screen.getByLabelText('口令'), 'secret')
    await user.click(screen.getByRole('button', { name: '登录' }))

    expect(await screen.findByText('出了点问题，请稍后重试。')).toBeInTheDocument()
    expect(screen.queryByText('用户名或口令不对。')).toBeNull()
  })
})
