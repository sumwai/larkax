import type { ReactNode } from 'react'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { AuthProvider, useAuth } from '../auth/AuthProvider'
import { I18nProvider } from '../i18n'
import { en } from '../i18n/en'
import { ThemeProvider } from '../theme/ThemeProvider'
import { ToastProvider } from '../components/ui'
import { MerchantAccount } from './MerchantAccount'
import { MerchantSettings, Settings } from './Settings'

const mocks = vi.hoisted(() => ({
  session: vi.fn(),
  updateMe: vi.fn(),
  changePassword: vi.fn(),
  logout: vi.fn(),
}))

vi.mock('../api', () => ({
  api: {
    session: mocks.session,
    updateMe: mocks.updateMe,
    changePassword: mocks.changePassword,
    logout: mocks.logout,
    setCsrfToken: vi.fn(),
    setUnauthorizedHandler: vi.fn(),
  },
}))

type Role = 'user' | 'merchant' | 'admin'

function userOf(role: Role) {
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

/**
 * 会话恢复完再挂页面，与 `RequireAuth` 的做法一致：设置页在首次渲染时读一次
 * `user`，挂早了会拿到 null，用户名那类校验也就无从测起。
 */
function Gate({ children }: { children: ReactNode }) {
  const { status } = useAuth()
  return status === 'authenticated' ? <>{children}</> : null
}

async function renderPage(role: Role, path: string) {
  mocks.session.mockResolvedValue({
    user: userOf(role),
    csrf_token: 'csrf',
    expires_at: '2026-12-01T00:00:00Z',
  })
  const result = render(
    <I18nProvider>
      <ThemeProvider>
        <ToastProvider>
          <AuthProvider>
            <MemoryRouter initialEntries={[path]}>
              <Gate>
                <Routes>
                  <Route path="/app/settings" element={<Settings />} />
                  <Route path="/app/admin/settings" element={<Settings />} />
                  <Route path="/app/merchant/settings" element={<MerchantSettings />} />
                  <Route path="/app/merchant/account" element={<MerchantAccount />} />
                </Routes>
              </Gate>
            </MemoryRouter>
          </AuthProvider>
        </ToastProvider>
      </ThemeProvider>
    </I18nProvider>,
  )
  await screen.findByRole('heading', { level: 1 })
  return result
}

/** 页面里各张卡片的标题，按 DOM 顺序。 */
function panelHeadings(): string[] {
  return screen.getAllByRole('heading', { level: 2 }).map((heading) => heading.textContent ?? '')
}

describe('设置与账号的分页', () => {
  beforeEach(() => {
    mocks.session.mockReset()
  })

  it('商家设置页只剩界面偏好，账号段不在这一页', async () => {
    await renderPage('merchant', '/app/merchant/settings')

    expect(screen.getByRole('heading', { name: '界面偏好' })).toBeInTheDocument()
    expect(screen.getByText('界面偏好：语言与主题。')).toBeInTheDocument()
    // 「只影响这台设备」留给共用的段说明，页副标题不再重复一遍。
    expect(screen.getByText('语言与主题偏好，只影响这台设备。')).toBeInTheDocument()
    // 账号段搬到了同级的「账号」页；留在这里就是两处都能改。
    expect(screen.queryByText('修改口令')).not.toBeInTheDocument()
    expect(screen.queryByText('账户信息')).not.toBeInTheDocument()
    // 点进设置的人常常是来改账户的：页底要有一行可点的落点。
    const entry = screen.getByRole('link', { name: '账号页' })
    expect(entry.getAttribute('href')).toBe('/app/merchant/account')
    // 段落里要靠强调色与下划线才看得出可点（样式在 `.inline-link` 里）。
    expect(entry.className).toContain('inline-link')
    // 链接是句子的一部分：`{account}` 标记丢了或写了两次，句子会断成两截而
    // 链接仍在，只比链接比不出来，所以要比整句。
    const sentence = entry.parentElement?.textContent ?? ''
    expect(sentence).toContain('账号页')
    expect(sentence).not.toContain('{account}')
    expect(sentence).toBe('显示名、邮箱与登录口令在账号页修改。')
  })

  it('商家账号页有账户信息与修改口令，不再重复退出登录', async () => {
    await renderPage('merchant', '/app/merchant/account')

    expect(screen.getByRole('heading', { name: '账号', level: 1 })).toBeInTheDocument()
    expect(screen.getByRole('heading', { name: '账户信息' })).toBeInTheDocument()
    expect(screen.getByRole('heading', { name: '修改口令' })).toBeInTheDocument()
    // 退出登录只在顶栏账户菜单与抽屉里，账号页不再放第三个入口。
    expect(screen.queryByText('退出登录')).not.toBeInTheDocument()
    // 偏好留在设置页，不在这里再出现一份。
    expect(screen.queryByRole('heading', { name: '界面偏好' })).not.toBeInTheDocument()
  })

  it.each([
    ['user', '/app/settings'],
    ['admin', '/app/admin/settings'],
  ] as const)('%s 的设置页仍带着账号段', async (role, path) => {
    await renderPage(role, path)

    // 这两类账号的底栏没有「账号」，拆走就等于改不了口令。
    for (const heading of ['账户信息', '修改口令', '界面偏好']) {
      expect(screen.getByRole('heading', { name: heading })).toBeInTheDocument()
    }
    // 退出登录只在顶栏与抽屉里。遍历三类页面而不是只看账号页：把面板加回
    // 整页也能被挡住，不只挡住加进共用账号段的那一种改法。
    expect(screen.queryByText('退出登录')).not.toBeInTheDocument()
    // 这两页的账号段就在本页，页底再指一个账号页入口会指向自己。
    expect(screen.queryByRole('link', { name: '账号页' })).not.toBeInTheDocument()
    // 「语言与主题不跨设备同步」这句在共用卡里，拆页时不能跟着丢掉。
    expect(screen.getByText('语言与主题偏好，只影响这台设备。')).toBeInTheDocument()
  })

  it('英文侧页标题与段标题不再同名', async () => {
    // `Record<MessageKey, string>` 保证键不缺，值是否撞名要靠这一条。
    expect(en['nav.account']).not.toBe(en['settings.profile'])

    localStorage.setItem('larkax.lang', 'en')
    const account = await renderPage('merchant', '/app/merchant/account')
    expect(screen.getByRole('heading', { level: 1, name: 'Account' })).toBeInTheDocument()
    expect(screen.getByRole('heading', { name: 'Account details' })).toBeInTheDocument()
    account.unmount()

    await renderPage('merchant', '/app/merchant/settings')
    // 英文的标签同样是句子的一部分，整句要连得起来读。
    const entry = screen.getByRole('link', { name: 'the Account page' })
    const sentence = entry.parentElement?.textContent ?? ''
    expect(sentence).toContain('the Account page')
    expect(sentence).not.toContain('{account}')
    expect(sentence).toBe(
      'Change your display name, email, and sign-in password on the Account page.',
    )
  })

  it('账号段只有一份实现：两个入口渲染出的标题一模一样', async () => {
    const account = await renderPage('merchant', '/app/merchant/account')
    const fromAccount = panelHeadings()
    account.unmount()

    await renderPage('user', '/app/settings')
    const fromSettings = panelHeadings()

    // 不各写一份期望值：比的是两边渲染出来的同一段文字，改文案时只改一处。
    expect(fromSettings.slice(0, fromAccount.length)).toEqual(fromAccount)
  })
})

describe('账号页的校验', () => {
  beforeEach(() => {
    mocks.session.mockReset()
  })

  it('口令的六条失败分支一条不少', async () => {
    const user = userEvent.setup()
    await renderPage('merchant', '/app/merchant/account')

    const current = screen.getByLabelText('当前口令')
    const next = screen.getByLabelText('新口令')
    const confirm = screen.getByLabelText('再输一次新口令')
    const submit = () => user.click(screen.getByRole('button', { name: '更新口令' }))

    // 当前口令未填。
    await submit()
    expect(await screen.findByText('请输入当前口令。')).toBeInTheDocument()

    // 太短。
    await user.type(current, 'old-password-1')
    await user.type(next, 'short')
    await user.type(confirm, 'short')
    await submit()
    expect(await screen.findByText('口令至少 12 个字符。')).toBeInTheDocument()

    // 太长。
    await user.clear(next)
    await user.clear(confirm)
    await user.type(next, 'x'.repeat(129))
    await user.type(confirm, 'x'.repeat(129))
    await submit()
    expect(await screen.findByText('口令最多 128 个字符。')).toBeInTheDocument()

    // 与用户名相同。
    await user.clear(next)
    await user.clear(confirm)
    await user.type(next, 'merchant-account')
    await user.type(confirm, 'merchant-account')
    await submit()
    expect(await screen.findByText('口令不能和用户名相同。')).toBeInTheDocument()

    // 与当前口令相同。
    await user.clear(next)
    await user.clear(confirm)
    await user.type(next, 'old-password-1')
    await user.type(confirm, 'old-password-1')
    await submit()
    expect(await screen.findByText('新口令不能和当前口令相同。')).toBeInTheDocument()

    // 两次不一致。
    await user.clear(next)
    await user.clear(confirm)
    await user.type(next, 'new-password-1234')
    await user.type(confirm, 'different-password')
    await submit()
    expect(await screen.findByText('两次输入的新口令不一样。')).toBeInTheDocument()

    // 以上都是前端拦下的，一次都没有发出去。
    expect(mocks.changePassword).not.toHaveBeenCalled()
  })

  it('邮箱格式、保存账户信息失败与改口令失败各有一条', async () => {
    const user = userEvent.setup()
    await renderPage('merchant', '/app/merchant/account')

    const email = screen.getByLabelText('邮箱')
    // `a@b` 过得了浏览器自带的 `type=email` 校验，过不了这里「域名要有
    // 点」的规则——用 `not-an-email` 的话表单根本不会提交，测不到这一支。
    await user.type(email, 'a@b')
    await user.click(screen.getByRole('button', { name: '保存账户信息' }))
    expect(await screen.findByText('邮箱格式不对。')).toBeInTheDocument()
    expect(mocks.updateMe).not.toHaveBeenCalled()

    await user.clear(email)
    await user.type(email, 'user@example.com')
    mocks.updateMe.mockRejectedValueOnce(new Error('boom'))
    await user.click(screen.getByRole('button', { name: '保存账户信息' }))
    expect(await screen.findByText('保存失败，请重试。')).toBeInTheDocument()

    await user.type(screen.getByLabelText('当前口令'), 'old-password-1')
    await user.type(screen.getByLabelText('新口令'), 'new-password-1234')
    await user.type(screen.getByLabelText('再输一次新口令'), 'new-password-1234')
    mocks.changePassword.mockRejectedValueOnce(new Error('boom'))
    await user.click(screen.getByRole('button', { name: '更新口令' }))
    expect(await screen.findByText('更新失败，检查当前口令是否正确。')).toBeInTheDocument()

    expect(mocks.changePassword).toHaveBeenCalledWith('old-password-1', 'new-password-1234')
  })
})
