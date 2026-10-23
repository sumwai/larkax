import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
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

/** 同一个地址、换一个低权限账号登录：用来验「过期期间放行的那段门槛能收回来」。 */
const MERCHANT_SESSION = {
  ...SESSION,
  user: {
    ...SESSION.user,
    id: 2,
    username: 'merchant-account',
    display_name: 'merchant-account',
    role: 'merchant',
  },
}

/**
 * 打开设置页、改一格显示名，再让下一次「保存账户信息」拿到 401。
 *
 * 走的是真的 HTTP 客户端：401 由 `request` 判定、`AuthProvider` 降级、门槛与
 * 确认框接力，中间任何一环断了这一组都会红，不是直接改 provider 的状态。
 *
 * `path` 用来换到 `RequireView` 之下的同一张表单：`/app/settings` 不在角色门槛里，
 * 只测它会漏掉 admin / merchant 那整条页面路。
 */
async function dirtySettings(saveResponds: () => Response, path = '/app/settings') {
  window.history.pushState({}, '', path)
  state.fetchImpl.mockImplementation(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input)
    if (url.endsWith('/auth/session')) return respond(200, SESSION)
    if (url.endsWith('/me') && init?.method === 'PATCH') return saveResponds()
    return respond(200, [])
  })
  render(<App />)
  const name = await screen.findByLabelText('显示名')
  fireEvent.change(name, { target: { value: '换了个名字' } })
  return name
}

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
    // 没有未保存的改动就直接跳，不该多问一句。
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()

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

  it('有未保存的改动时 401 先问一句：选「留下」，地址与填的内容都在', async () => {
    await dirtySettings(expired)
    fireEvent.click(screen.getByRole('button', { name: '保存账户信息' }))

    // 先等 401 的结局落到界面上（弹出确认，或直接被送去登录页），再断表单。
    // 若先 find 弹框，页面被卸载的那类缺陷会红在「找不到弹框」，表单那几行永远
    // 轮不到——红在别处与假绿是同一类问题，都会让人以为钉住了。
    await waitFor(() => {
      const asked = screen.queryByRole('dialog', { name: '会话已过期' })
      const gone = screen.queryByText('登录控制台')
      expect(asked ?? gone).not.toBeNull()
    })
    // 表单先断：页面若被卸载重挂，这一行会红在「找不到显示名」——那才是这条用例
    // 真正要钉的东西。地址放在它后面，同样会红，但不该抢在表单前面把红引到别处。
    expect(screen.getByLabelText('显示名')).toHaveValue('换了个名字')
    expect(window.location.pathname).toBe('/app/settings')

    const dialog = screen.getByRole('dialog', { name: '会话已过期' })
    // 只说「未保存的修改」不够：用户得先知道为什么突然要走。
    expect(dialog).toHaveTextContent('登录状态已经失效，留在这页也保存不了。')

    fireEvent.click(within(dialog).getByRole('button', { name: '取消' }))

    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(window.location.pathname).toBe('/app/settings')
    expect(screen.getByLabelText('显示名')).toHaveValue('换了个名字')
    // 留下之后不是什么都不说：一条非模态横幅一直挂着，随时能去登录。
    expect(screen.getByRole('button', { name: '去登录' })).toBeInTheDocument()
  })

  it('角色门槛下的页面（admin）同样留在原地：没权限提示不会顶掉还有改动的页', async () => {
    await dirtySettings(expired, '/app/admin/settings')
    fireEvent.click(screen.getByRole('button', { name: '保存账户信息' }))

    // 与上面那条设置页的用例同一个形状：先等 401 的结局落到界面上，再断页面还在，
    // 最后才去找弹框。先 find 弹框的话，角色门槛把页面卸掉的那类缺陷会红在「找不到
    // 弹框」，而这条用例要钉的「页面还活着」永远不会被轮到。
    await waitFor(() => {
      const asked = screen.queryByRole('dialog', { name: '会话已过期' })
      const gone = screen.queryByText('登录控制台')
      expect(asked ?? gone).not.toBeNull()
    })
    // 三条一起才说明「这一页还活着」：值还在、地址没变、侧栏仍是管理视图。
    // 少了任何一条，都可能只是页面被换成别的、恰好又换回来了。
    expect(screen.getByLabelText('显示名')).toHaveValue('换了个名字')
    expect(window.location.pathname).toBe('/app/admin/settings')
    expect(screen.getAllByRole('link', { name: '渠道' }).length).toBeGreaterThan(0)

    const dialog = screen.getByRole('dialog', { name: '会话已过期' })
    fireEvent.click(within(dialog).getByRole('button', { name: '取消' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())

    expect(window.location.pathname).toBe('/app/admin/settings')
    expect(screen.getByLabelText('显示名')).toHaveValue('换了个名字')
  })

  it('过期期间留在 admin 页，再登录成低权限账号：admin 页重新被角色门槛挡住', async () => {
    await dirtySettings(expired, '/app/admin/settings')
    fireEvent.click(screen.getByRole('button', { name: '保存账户信息' }))
    const dialog = await screen.findByRole('dialog', { name: '会话已过期' })
    fireEvent.click(within(dialog).getByRole('button', { name: '取消' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())

    // 走用户真会走的那条路：横幅上的「去登录」，再在登录页填表。这里不再给
    // 保存请求造 401——这一段要验的是登录之后的角色判定。
    state.fetchImpl.mockImplementation(async (input: RequestInfo | URL) => {
      const url = String(input)
      if (url.endsWith('/auth/session')) return respond(200, SESSION)
      if (url.endsWith('/auth/login')) return respond(200, MERCHANT_SESSION)
      return respond(200, [])
    })
    fireEvent.click(screen.getByRole('button', { name: '去登录' }))
    await waitFor(() => expect(window.location.pathname).toBe('/login'))

    fireEvent.change(await screen.findByLabelText('用户名'), {
      target: { value: 'merchant-account' },
    })
    fireEvent.change(screen.getByLabelText('口令'), { target: { value: 'a-long-password' } })
    fireEvent.click(screen.getByRole('button', { name: '登录' }))

    // 登录后的回跳地址仍是原来的 admin 页；这里断的是「那一页没有被画出来」，
    // 不是某一句文案：表单不在、门槛的无权限提示在。过期期间那段放行必须随
    // 登录一起收回——它靠的是 `sessionExpired`，不是「这个人进过一次就永远放行」。
    await waitFor(() => expect(window.location.pathname).toBe('/app/admin/settings'))
    expect(screen.queryByLabelText('显示名')).not.toBeInTheDocument()
    expect(screen.getByText('这个视图不在你的权限范围内。')).toBeInTheDocument()
  })

  it('留下之后再发一次 401：不再弹框，填的内容还在', async () => {
    await dirtySettings(expired)
    fireEvent.click(screen.getByRole('button', { name: '保存账户信息' }))
    const dialog = await screen.findByRole('dialog', { name: '会话已过期' })
    fireEvent.click(within(dialog).getByRole('button', { name: '取消' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())

    // 改一下邮箱把上一次的失败提示清掉，这样下面等到的提示只能来自第二次请求。
    fireEvent.change(screen.getByLabelText('邮箱'), { target: { value: 'user@example.com' } })
    expect(screen.queryByText('保存失败，请重试。')).not.toBeInTheDocument()

    // 过期状态下再保存一次：必然还是 401。同一段过期会话里不能再弹框。
    fireEvent.click(screen.getByRole('button', { name: '保存账户信息' }))
    expect(await screen.findByText('保存失败，请重试。')).toBeInTheDocument()

    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(window.location.pathname).toBe('/app/settings')
    expect(screen.getByLabelText('显示名')).toHaveValue('换了个名字')
    expect(screen.getByLabelText('邮箱')).toHaveValue('user@example.com')
  })

  it('留下之后点横幅上的「去登录」：走同一条出口留在登录页', async () => {
    await dirtySettings(expired)
    fireEvent.click(screen.getByRole('button', { name: '保存账户信息' }))
    const dialog = await screen.findByRole('dialog', { name: '会话已过期' })
    fireEvent.click(within(dialog).getByRole('button', { name: '取消' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())

    fireEvent.click(screen.getByRole('button', { name: '去登录' }))

    await waitFor(() => expect(window.location.pathname).toBe('/login'))
    expect(new URLSearchParams(window.location.search).get('next')).toBe('/app/settings')
  })

  it('留下之后点站内跳转：仍然被拦，没被静默放行', async () => {
    await dirtySettings(expired)
    fireEvent.click(screen.getByRole('button', { name: '保存账户信息' }))
    const dialog = await screen.findByRole('dialog', { name: '会话已过期' })
    fireEvent.click(within(dialog).getByRole('button', { name: '取消' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())

    // 「去登录」让开阻止器那一下必须已经复位：少了复位，这一次跳转会静默成功，
    // 用户填的东西就这么没了，没有弹框也没有报错。
    fireEvent.click(screen.getAllByRole('link', { name: '概览' })[0])
    const discard = await screen.findByRole('dialog', { name: '放弃未保存的修改？' })
    expect(window.location.pathname).toBe('/app/settings')

    // 取消这一次跳转：会话那一句不能再冒出来。一次失效只问一次，否则每点一下
    // 都会先弹「会话已过期」再弹「放弃未保存的修改」。
    fireEvent.click(within(discard).getByRole('button', { name: '取消' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(window.location.pathname).toBe('/app/settings')
    expect(screen.getByRole('button', { name: '去登录' })).toBeInTheDocument()
  })

  it('有未保存的改动时 401：选「放弃修改」才去登录页，next 是原地址', async () => {
    await dirtySettings(expired)
    fireEvent.click(screen.getByRole('button', { name: '保存账户信息' }))

    const dialog = await screen.findByRole('dialog', { name: '会话已过期' })
    fireEvent.click(within(dialog).getByRole('button', { name: '放弃修改' }))

    await waitFor(() => expect(window.location.pathname).toBe('/login'))
    expect(new URLSearchParams(window.location.search).get('next')).toBe('/app/settings')
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })

  it('失效恰好赶在被拦下的站内跳转上：只有一个框，确认后只去一次登录页', async () => {
    window.history.pushState({}, '', '/app/settings')
    let failSave!: (response: Response) => void
    const pendingSave = new Promise<Response>((resolve) => {
      failSave = resolve
    })
    state.fetchImpl.mockImplementation(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input)
      if (url.endsWith('/auth/session')) return respond(200, SESSION)
      if (url.endsWith('/me') && init?.method === 'PATCH') return pendingSave
      return respond(200, [])
    })
    render(<App />)
    fireEvent.change(await screen.findByLabelText('显示名'), { target: { value: '换了个名字' } })

    // 保存请求先悬着，页面上先点一次别的入口：这一次被未保存的改动拦下。
    fireEvent.click(screen.getByRole('button', { name: '保存账户信息' }))
    // 侧栏与窄屏底栏各有一个「概览」，点哪一个都是同一次站内跳转。
    fireEvent.click(screen.getAllByRole('link', { name: '概览' })[0])
    expect(await screen.findByRole('dialog', { name: '放弃未保存的修改？' })).toBeInTheDocument()

    // 悬着的那个请求这时才拿到 401：会话失效的说明要顶掉刚才那句，而不是叠一个框。
    await act(async () => failSave(expired()))

    const dialogs = screen.getAllByRole('dialog')
    expect(dialogs).toHaveLength(1)
    expect(dialogs[0]).toHaveAccessibleName('会话已过期')
    expect(window.location.pathname).toBe('/app/settings')

    fireEvent.click(within(dialogs[0]).getByRole('button', { name: '放弃修改' }))

    await waitFor(() => expect(window.location.pathname).toBe('/login'))
    expect(new URLSearchParams(window.location.search).get('next')).toBe('/app/settings')
    expect(screen.queryAllByRole('dialog')).toHaveLength(0)
  })
})
