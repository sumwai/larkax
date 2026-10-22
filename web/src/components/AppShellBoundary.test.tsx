import { fireEvent, render, screen } from '@testing-library/react'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { AuthProvider } from '../auth/AuthProvider'
import { I18nProvider } from '../i18n'
import { AppShell } from './AppShell'
import { ToastProvider } from './ui'

const mocks = vi.hoisted(() => ({ session: vi.fn() }))

vi.mock('../api', () => ({
  api: {
    session: mocks.session,
    setCsrfToken: vi.fn(),
    logout: vi.fn(),
  },
}))

function ThrowingPage(): never {
  throw new Error('页面渲染失败')
}

async function renderShellWithBoom() {
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
  const result = render(
    <I18nProvider>
      <ToastProvider>
        <AuthProvider>
          <MemoryRouter initialEntries={['/app']}>
            <Routes>
              <Route path="/app" element={<AppShell />}>
                <Route index element={<ThrowingPage />} />
                <Route path="keys" element={<p>密钥页正常显示</p>} />
              </Route>
            </Routes>
          </MemoryRouter>
        </AuthProvider>
      </ToastProvider>
    </I18nProvider>,
  )
  await screen.findAllByText('user-account')
  return result
}

describe('AppShell 的渲染期错误', () => {
  beforeEach(() => {
    mocks.session.mockReset()
    vi.spyOn(console, 'error').mockImplementation(() => {})
  })

  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('显示错误说明，外壳与可点的侧栏链接仍在', async () => {
    await renderShellWithBoom()

    expect(await screen.findByRole('heading', { name: '这个页面出错了' })).toBeInTheDocument()
    const sideLink = screen.getAllByRole('link', { name: 'API 密钥' })[0]
    expect(sideLink).toBeInTheDocument()
    expect(sideLink).toBeVisible()
    expect(sideLink).toHaveAttribute('href', '/app/keys')
  })

  it('点侧栏切到别的路由后恢复正常页', async () => {
    await renderShellWithBoom()

    expect(await screen.findByRole('heading', { name: '这个页面出错了' })).toBeInTheDocument()
    fireEvent.click(screen.getAllByRole('link', { name: 'API 密钥' })[0])

    expect(await screen.findByText('密钥页正常显示')).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: '这个页面出错了' })).toBeNull()
  })
})
