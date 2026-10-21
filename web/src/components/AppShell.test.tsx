import { render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { beforeEach, describe, expect, it, vi } from 'vitest'
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

function userOf(role: 'user' | 'merchant' | 'admin') {
  return {
    id: 1,
    username: `${role}-account`,
    display_name: `${role}-account`,
    email: null,
    status: 'active',
    role,
    created_at: '2026-01-01T00:00:00Z',
  }
}

async function renderShell(role: 'user' | 'merchant' | 'admin', path = '/app') {
  mocks.session.mockResolvedValue({
    user: userOf(role),
    csrf_token: 'csrf',
    expires_at: '2026-12-01T00:00:00Z',
  })
  render(
    <I18nProvider>
      <ToastProvider>
        <AuthProvider>
          <MemoryRouter initialEntries={[path]}>
            <AppShell />
          </MemoryRouter>
        </AuthProvider>
      </ToastProvider>
    </I18nProvider>,
  )
  // 会话恢复完、账号名出现之后再看视图切换（名字在顶栏与侧栏各出现一次）。
  await screen.findAllByText(`${role}-account`)
}

describe('AppShell 视图切换', () => {
  beforeEach(() => {
    mocks.session.mockReset()
  })

  it('user 只有「用户」视图，看不到商家与管理', async () => {
    await renderShell('user')
    expect(screen.queryByRole('button', { name: '商家' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '管理' })).not.toBeInTheDocument()
  })

  it('merchant 多一个「商家」视图，仍看不到管理', async () => {
    await renderShell('merchant')
    expect(screen.getByRole('button', { name: '用户' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '商家' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '管理' })).not.toBeInTheDocument()
  })

  it('admin 只有用户与管理，运营导航四组齐全', async () => {
    await renderShell('admin', '/app/admin/channels')
    for (const label of ['用户', '管理']) {
      expect(screen.getByRole('button', { name: label })).toBeInTheDocument()
    }
    expect(screen.queryByRole('button', { name: '商家' })).not.toBeInTheDocument()
    for (const label of ['渠道', '模型目录', '别名', '计价']) {
      expect(screen.getAllByRole('link', { name: label }).length).toBeGreaterThan(0)
    }
  })

  it('merchant 的运营导航是渠道与计价，没有模型目录与别名', async () => {
    await renderShell('merchant', '/app/merchant/channels')
    for (const label of ['渠道', '计价']) {
      expect(screen.getAllByRole('link', { name: label }).length).toBeGreaterThan(0)
    }
    for (const label of ['模型目录', '别名']) {
      expect(screen.queryAllByRole('link', { name: label })).toHaveLength(0)
    }
    // 成本规则在商家前缀下没有路由，导航里也不该出现。
    expect(screen.queryByText('成本规则')).not.toBeInTheDocument()
  })

  it('merchant 落在管理视图的地址上时，侧栏也不列管理组入口', async () => {
    await renderShell('merchant', '/app/admin/channels')
    for (const label of ['模型目录', '别名']) {
      expect(screen.queryAllByRole('link', { name: label })).toHaveLength(0)
    }
    expect(screen.queryByText('平台管理')).not.toBeInTheDocument()
  })
})
