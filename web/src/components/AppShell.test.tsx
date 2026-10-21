import { fireEvent, render, screen, within } from '@testing-library/react'
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
  // 会话恢复完、账号名出现之后再看视图切换（名字在顶栏的账户区）。
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

describe('AppShell 桌面端顶栏', () => {
  beforeEach(() => {
    mocks.session.mockReset()
  })

  it('账户菜单能打开，菜单里是设置与退出登录，Esc 关掉并把焦点还给触发钮', async () => {
    await renderShell('user')
    const trigger = screen.getByRole('button', { name: /user-account/ })
    expect(trigger).toHaveAttribute('aria-expanded', 'false')

    fireEvent.click(trigger)
    expect(trigger).toHaveAttribute('aria-expanded', 'true')
    const account = trigger.closest('.account') as HTMLElement
    expect(within(account).getByRole('link', { name: '设置' })).toBeInTheDocument()
    expect(within(account).getByRole('button', { name: '退出登录' })).toBeInTheDocument()

    fireEvent.keyDown(document, { key: 'Escape' })
    expect(within(account).queryByRole('button', { name: '退出登录' })).not.toBeInTheDocument()
    expect(trigger).toHaveAttribute('aria-expanded', 'false')
    expect(trigger).toHaveFocus()
  })

  it('点菜单外也把菜单关掉', async () => {
    await renderShell('user')
    const trigger = screen.getByRole('button', { name: /user-account/ })
    fireEvent.click(trigger)
    const account = trigger.closest('.account') as HTMLElement
    expect(within(account).getByRole('button', { name: '退出登录' })).toBeInTheDocument()

    fireEvent.mouseDown(document.body)
    expect(within(account).queryByRole('button', { name: '退出登录' })).not.toBeInTheDocument()
  })

  it('商家从账户菜单进设置，落在商家视图的设置页', async () => {
    await renderShell('merchant', '/app/merchant/channels')
    const trigger = screen.getByRole('button', { name: /merchant-account/ })
    fireEvent.click(trigger)

    const account = trigger.closest('.account') as HTMLElement
    const settingsLink = within(account).getByRole('link', { name: '设置' })
    expect(settingsLink).toHaveAttribute('href', '/app/merchant/settings')

    fireEvent.click(settingsLink)
    expect(within(account).queryByRole('link', { name: '设置' })).not.toBeInTheDocument()
  })

  it('换路由后菜单自己收起（不经过菜单项）', async () => {
    await renderShell('admin', '/app/admin/channels')
    const trigger = screen.getByRole('button', { name: /admin-account/ })
    fireEvent.click(trigger)
    const account = trigger.closest('.account') as HTMLElement
    expect(within(account).getByRole('button', { name: '退出登录' })).toBeInTheDocument()

    // 顶栏切换器不经过菜单项，菜单只能靠路由变化收起。
    fireEvent.click(within(screen.getByRole('banner')).getByRole('button', { name: '用户' }))
    expect(within(account).queryByRole('button', { name: '退出登录' })).not.toBeInTheDocument()
  })

  it('视图切换器在顶栏，侧栏里没有第二份', async () => {
    await renderShell('admin', '/app/admin/channels')
    const topbar = screen.getByRole('banner')
    const switcher = within(topbar).getByRole('group', { name: '控制台' })
    for (const label of ['用户', '管理']) {
      expect(within(switcher).getByRole('button', { name: label })).toBeInTheDocument()
    }
    expect(within(switcher).queryByRole('button', { name: '商家' })).not.toBeInTheDocument()

    const sidebar = screen.getByRole('complementary')
    expect(within(sidebar).queryByRole('group', { name: '控制台' })).not.toBeInTheDocument()
    // 账户与退出也从侧栏搬走了。
    expect(within(sidebar).queryByRole('button', { name: '退出登录' })).not.toBeInTheDocument()
  })

  it('管理员直敲商家页：标题写无权限，切换器高亮管理', async () => {
    await renderShell('admin', '/app/merchant/channels')
    const topbar = screen.getByRole('banner')
    expect(within(topbar).getByText('无权限')).toBeInTheDocument()
    expect(within(topbar).getByRole('button', { name: '管理' })).toHaveAttribute(
      'aria-pressed',
      'true',
    )
    expect(within(topbar).getByRole('button', { name: '用户' })).toHaveAttribute(
      'aria-pressed',
      'false',
    )
  })

  it.each([
    ['/app/keys', 'API 密钥'],
    ['/app/usage', '用量'],
  ])('%s 的顶栏标题是「%s」', async (path, title) => {
    await renderShell('user', path)
    expect(within(screen.getByRole('banner')).getByText(title)).toBeInTheDocument()
  })

  it('收起侧栏后 logo 仍是可点的按钮，点一下展开', async () => {
    await renderShell('user')
    fireEvent.click(screen.getByRole('button', { name: '收起侧边栏' }))

    const logo = screen.getByRole('button', { name: '展开侧边栏' })
    fireEvent.click(logo)
    expect(screen.getByRole('button', { name: '收起侧边栏' })).toBeInTheDocument()
  })
})
