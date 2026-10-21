import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { I18nProvider } from '../../i18n'
import { AuthProvider } from '../../auth/AuthProvider'
import { SiteProvider } from '../../site/SiteProvider'
import { ToastProvider } from '../../components/ui'
import { AdminUserDetailPage, AdminUsers } from './Users'

const mocks = vi.hoisted(() => ({
  listUsers: vi.fn(),
  getUser: vi.fn(),
  updateUser: vi.fn(),
  meta: vi.fn(),
  session: vi.fn(),
  setCsrfToken: vi.fn(),
}))

vi.mock('../../api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../api')>()
  return {
    ...actual,
    api: {
      admin: {
        listUsers: mocks.listUsers,
        getUser: mocks.getUser,
        updateUser: mocks.updateUser,
      },
      meta: mocks.meta,
      session: mocks.session,
      setCsrfToken: mocks.setCsrfToken,
    },
  }
})

const { ApiError } = await import('../../api')

const SUMMARY = {
  id: 2,
  username: 'alice',
  email: 'a***@example.com',
  display_name: 'Alice',
  role: 'merchant' as const,
  status: 'active' as const,
  created_at: '2026-01-02T03:04:05Z',
  last_seen_at: '2026-02-03T04:05:06Z',
}

const OTHER = {
  ...SUMMARY,
  id: 3,
  username: 'bob',
  email: null,
  display_name: null,
  role: 'user' as const,
  last_seen_at: null,
}

const DETAIL = {
  ...SUMMARY,
  email_full: 'alice@example.com',
  balance: '12.5',
  active_key_count: 3,
  requests_7d: 1200,
  amount_7d: '4.25',
  channel_count: 2,
  key_last_used_at: '2026-02-04T05:06:07Z',
}

function renderList() {
  return render(
    <I18nProvider>
      <ToastProvider>
        <SiteProvider>
          <MemoryRouter initialEntries={['/app/admin/users']}>
            <Routes>
              <Route path="/app/admin/users" element={<AdminUsers />} />
            </Routes>
          </MemoryRouter>
        </SiteProvider>
      </ToastProvider>
    </I18nProvider>,
  )
}

function renderDetail(id = 2) {
  return render(
    <I18nProvider>
      <ToastProvider>
        <SiteProvider>
          <AuthProvider>
            <MemoryRouter initialEntries={[`/app/admin/users/${id}`]}>
              <Routes>
                <Route path="/app/admin/users/:id" element={<AdminUserDetailPage />} />
              </Routes>
            </MemoryRouter>
          </AuthProvider>
        </SiteProvider>
      </ToastProvider>
    </I18nProvider>,
  )
}

describe('用户列表页', () => {
  beforeEach(() => {
    Object.values(mocks).forEach((mock) => mock.mockReset())
    mocks.meta.mockResolvedValue({
      site_name: 'larkax',
      api_base_url: 'https://api.example.com/v1',
      currency: 'USD',
      timezone: 'Asia/Shanghai',
      locales: ['en', 'zh'],
      features: { registration: false, password_reset: 'admin', topup: false },
    })
    mocks.session.mockResolvedValue(null)
  })

  it('列表里只有打码邮箱，页面上不出现完整地址', async () => {
    mocks.listUsers.mockResolvedValue({ items: [SUMMARY, OTHER], next_cursor: null })
    renderList()

    expect(await screen.findByText('alice')).toBeInTheDocument()
    expect(screen.getByText('a***@example.com')).toBeInTheDocument()
    expect(screen.queryByText('alice@example.com')).not.toBeInTheDocument()
    // 没有邮箱的行不留空白。
    expect(screen.getByText('bob')).toBeInTheDocument()
  })

  it('搜索停手三百毫秒才发请求，连续输入只发一次', async () => {
    mocks.listUsers.mockResolvedValue({ items: [SUMMARY], next_cursor: null })
    renderList()
    await screen.findByText('alice')
    expect(mocks.listUsers).toHaveBeenCalledTimes(1)

    const search = screen.getByLabelText('搜索')
    fireEvent.change(search, { target: { value: 'a' } })
    fireEvent.change(search, { target: { value: 'al' } })
    // 停手之前不发：每敲一个字发一次，服务端看到的多半是半截词。
    expect(mocks.listUsers).toHaveBeenCalledTimes(1)

    await waitFor(() => expect(mocks.listUsers).toHaveBeenCalledTimes(2))
    expect(mocks.listUsers).toHaveBeenLastCalledWith({
      query: 'al',
      role: undefined,
      status: undefined,
      limit: 50,
    })
  })

  it('角色与状态筛选把参数带到请求上，游标回到第一页', async () => {
    mocks.listUsers.mockResolvedValueOnce({ items: [SUMMARY], next_cursor: 'cursor-2' })
    mocks.listUsers.mockResolvedValue({ items: [OTHER], next_cursor: null })
    renderList()
    await screen.findByText('alice')

    // 先翻一页，确认筛选后不会带着旧游标。
    fireEvent.click(screen.getByRole('button', { name: '加载更多' }))
    await waitFor(() => expect(mocks.listUsers).toHaveBeenCalledTimes(2))
    expect(mocks.listUsers).toHaveBeenLastCalledWith(
      expect.objectContaining({ cursor: 'cursor-2' }),
    )

    fireEvent.change(screen.getByLabelText('按角色筛选'), { target: { value: 'merchant' } })
    await waitFor(() => expect(mocks.listUsers).toHaveBeenCalledTimes(3))
    expect(mocks.listUsers).toHaveBeenLastCalledWith({
      query: undefined,
      role: 'merchant',
      status: undefined,
      limit: 50,
    })

    fireEvent.change(screen.getByLabelText('按状态筛选'), { target: { value: 'suspended' } })
    await waitFor(() => expect(mocks.listUsers).toHaveBeenCalledTimes(4))
    expect(mocks.listUsers).toHaveBeenLastCalledWith({
      query: undefined,
      role: 'merchant',
      status: 'suspended',
      limit: 50,
    })
  })

  it('翻页在途时改筛选，旧那一页不会追进新条件的列表', async () => {
    let finishMore: (value: unknown) => void = () => {}
    mocks.listUsers
      .mockResolvedValueOnce({ items: [SUMMARY], next_cursor: 'cursor-2' })
      .mockImplementationOnce(
        () =>
          new Promise((resolve) => {
            finishMore = resolve
          }),
      )
    mocks.listUsers.mockResolvedValue({ items: [OTHER], next_cursor: null })
    renderList()
    await screen.findByText('alice')

    fireEvent.click(screen.getByRole('button', { name: '加载更多' }))
    // 翻页还挂着的时候换条件：旧那一页这时才回来。
    fireEvent.change(screen.getByLabelText('按角色筛选'), { target: { value: 'admin' } })
    expect(await screen.findByText('bob')).toBeInTheDocument()

    finishMore({ items: [{ ...SUMMARY, id: 9, username: 'stale' }], next_cursor: null })
    // 让那个挂起的请求真的落地，再看列表。
    await act(async () => {
      await Promise.resolve()
    })
    expect(screen.queryByText('stale')).not.toBeInTheDocument()
    expect(screen.queryByText('alice')).not.toBeInTheDocument()
    expect(screen.getByText('bob')).toBeInTheDocument()
  })

  it('没有筛选且一条也没有时，说的是「还没有用户」', async () => {
    mocks.listUsers.mockResolvedValue({ items: [], next_cursor: null })
    renderList()

    expect(await screen.findByText('还没有用户')).toBeInTheDocument()
    expect(screen.queryByText('没有匹配的用户')).not.toBeInTheDocument()
  })

  it('有筛选但没匹配时另说一句，清空筛选会重新取回全部', async () => {
    mocks.listUsers.mockResolvedValue({ items: [], next_cursor: null })
    renderList()
    expect(await screen.findByText('还没有用户')).toBeInTheDocument()

    fireEvent.change(screen.getByLabelText('按角色筛选'), { target: { value: 'admin' } })
    expect(await screen.findByText('没有匹配的用户')).toBeInTheDocument()
    expect(screen.queryByText('还没有用户')).not.toBeInTheDocument()

    mocks.listUsers.mockResolvedValue({ items: [SUMMARY], next_cursor: null })
    fireEvent.click(screen.getByRole('button', { name: '清空筛选' }))

    expect(await screen.findByText('alice')).toBeInTheDocument()
    expect(mocks.listUsers).toHaveBeenLastCalledWith({
      query: undefined,
      role: undefined,
      status: undefined,
      limit: 50,
    })
  })
})

describe('用户详情页', () => {
  beforeEach(() => {
    Object.values(mocks).forEach((mock) => mock.mockReset())
    mocks.meta.mockResolvedValue({
      site_name: 'larkax',
      api_base_url: 'https://api.example.com/v1',
      currency: 'USD',
      timezone: 'Asia/Shanghai',
      locales: ['en', 'zh'],
      features: { registration: false, password_reset: 'admin', topup: false },
    })
    mocks.session.mockResolvedValue(null)
  })

  it('详情给完整邮箱与六个指标，从没登录过的写「从未登录」', async () => {
    mocks.getUser.mockResolvedValue({
      ...DETAIL,
      last_seen_at: null,
      key_last_used_at: null,
    })
    renderDetail()

    expect(await screen.findByText('alice@example.com')).toBeInTheDocument()
    expect(screen.getByText('生效密钥')).toBeInTheDocument()
    expect(screen.getByText('近 7 天请求')).toBeInTheDocument()
    expect(screen.getByText('渠道数')).toBeInTheDocument()
    expect(screen.getByText('密钥最近使用')).toBeInTheDocument()
    expect(screen.getAllByText('从未登录').length).toBeGreaterThan(0)
    expect(screen.getByText('从未使用')).toBeInTheDocument()
  })

  it('改角色成功后显示新角色，且没有再取一次详情', async () => {
    mocks.getUser.mockResolvedValue(DETAIL)
    mocks.updateUser.mockResolvedValue({ ...DETAIL, role: 'user' })
    renderDetail()

    expect(await screen.findByText('商家')).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: '修改角色' }))
    fireEvent.change(screen.getByLabelText('角色'), { target: { value: 'user' } })
    fireEvent.click(screen.getByRole('button', { name: '确认' }))

    await waitFor(() => expect(mocks.updateUser).toHaveBeenCalled())
    const profile = screen.getByLabelText('账号资料')
    expect(within(profile).getByText('用户')).toBeInTheDocument()
    expect(mocks.updateUser).toHaveBeenCalledWith(2, { role: 'user' })
    // PATCH 的响应体就是详情：刷新不该再取一次。
    expect(mocks.getUser).toHaveBeenCalledTimes(1)
  })

  it('服务端拒绝时展示的是它给的那句话', async () => {
    mocks.getUser.mockResolvedValue(DETAIL)
    mocks.updateUser.mockRejectedValue(
      new ApiError(400, 'invalid', 'an administrator cannot change their own role'),
    )
    renderDetail()

    fireEvent.click(await screen.findByRole('button', { name: '修改角色' }))
    fireEvent.change(screen.getByLabelText('角色'), { target: { value: 'admin' } })
    fireEvent.click(screen.getByRole('button', { name: '确认' }))

    expect(
      await screen.findByText('an administrator cannot change their own role'),
    ).toBeInTheDocument()
  })

  it('409 另起一档，并给一条回列表的路', async () => {
    mocks.getUser.mockResolvedValue(DETAIL)
    mocks.updateUser.mockRejectedValue(
      new ApiError(409, 'conflict', 'this change would leave no active administrator'),
    )
    renderDetail()

    fireEvent.click(await screen.findByRole('button', { name: '修改状态' }))
    fireEvent.change(screen.getByLabelText('状态'), { target: { value: 'banned' } })
    fireEvent.click(screen.getByRole('button', { name: '确认' }))

    expect(
      await screen.findByText('this change would leave no active administrator'),
    ).toBeInTheDocument()
    expect(screen.getByText(/服务端拒绝了这次改动/)).toBeInTheDocument()
    // 顶栏本来就有一条回列表的链接，这里要的是错误块里那一条。
    const alert = screen.getByRole('alert')
    expect(within(alert).getByRole('link', { name: '返回用户列表' })).toBeInTheDocument()
  })

  it('自己的账号不能改，并写明原因', async () => {
    mocks.session.mockResolvedValue({
      user: {
        id: 2,
        username: 'alice',
        display_name: 'Alice',
        email: 'alice@example.com',
        status: 'active',
        role: 'admin',
        created_at: '2026-01-02T03:04:05Z',
      },
      csrf_token: 'csrf-placeholder',
      expires_at: '2026-12-31T00:00:00Z',
    })
    mocks.getUser.mockResolvedValue(DETAIL)
    renderDetail()

    expect(await screen.findByText(/这是你自己的账号/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '修改角色' })).toBeDisabled()
    expect(screen.getByRole('button', { name: '修改状态' })).toBeDisabled()
  })
})
