import { useState, type ReactNode } from 'react'
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { createMemoryRouter, Link, Outlet, RouterProvider } from 'react-router-dom'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { I18nProvider } from '../../i18n'
import { AuthProvider, useAuth } from '../../auth/AuthProvider'
import { ThemeProvider } from '../../theme/ThemeProvider'
import { ToastProvider } from '../../components/ui'
import { SiteProvider } from '../../site/SiteProvider'
import { OpsAliases } from './Aliases'
import { OpsChannels } from './Channels'
import { OpsModels } from './Catalog'
import { OpsPricing } from './Pricing'
import { Settings } from '../Settings'
import { isDirty, useUnsavedGuard } from './guard'

const mocks = vi.hoisted(() => ({
  session: vi.fn(),
  meta: vi.fn(),
  updateMe: vi.fn(),
  changePassword: vi.fn(),
  logout: vi.fn(),
  listAliases: vi.fn(),
  listChannels: vi.fn(),
  listModels: vi.fn(),
  listAdapters: vi.fn(),
  listRules: vi.fn(),
  listCosts: vi.fn(),
}))

vi.mock('../../api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../api')>()
  const ops = {
    listAliases: mocks.listAliases,
    listChannels: mocks.listChannels,
    listModels: mocks.listModels,
    listAdapters: mocks.listAdapters,
    listRules: mocks.listRules,
    listCosts: mocks.listCosts,
  }
  return {
    ...actual,
    isMockMode: false,
    api: {
      admin: ops,
      merchant: ops,
      meta: mocks.meta,
      session: mocks.session,
      updateMe: mocks.updateMe,
      changePassword: mocks.changePassword,
      logout: mocks.logout,
      setCsrfToken: vi.fn(),
      setUnauthorizedHandler: vi.fn(),
    },
  }
})

const USER = {
  id: 1,
  username: 'admin-account',
  display_name: 'admin-account',
  email: null,
  status: 'active',
  role: 'admin',
  created_at: '2026-01-01T00:00:00Z',
}

const CHANNEL = {
  id: 7,
  owner_user_id: 0,
  name: 'platform-anthropic',
  type: 'anthropic',
  base_url: 'https://api.example.com/anthropic',
  status: 'enabled',
  models: ['claude-sonnet-4-5'],
  weight: 1,
  priority: 0,
  setting: {},
  credential_keys: [],
  credential_hint: '',
  deleted_at: null,
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-02T00:00:00Z',
}

/**
 * 派发一次 beforeunload。
 *
 * jsdom 不会真的导航，也不会弹原生确认框，所以这里能断的是「有人在这次事件上
 * 调了 preventDefault」——浏览器据此才弹框。要证明真浏览器会弹，见报告里的
 * Playwright 取证。
 */
function dispatchBeforeUnload(): Event {
  const event = new Event('beforeunload', { cancelable: true })
  window.dispatchEvent(event)
  return event
}

/**
 * 一张最小的表单：只用来钉「脏状态 → 两个出口」这一层本身。
 *
 * 点「去别处」只是**发起一次站内路由导航**的手段，等价于浏览器后退；弹窗表单
 * 在真实 UI 里走得到的就是后退那条（遮罩铺满视口，弹窗开着时点不到侧栏）。
 */
function TestForm() {
  const [value, setValue] = useState('')
  const [initial, setInitial] = useState('')
  const guard = useUnsavedGuard(isDirty(value, initial))
  return (
    <>
      <label>
        字段
        <input value={value} onChange={(event) => setValue(event.target.value)} />
      </label>
      <button type="button" onClick={() => setInitial(value)}>
        保存
      </button>
      {guard}
    </>
  )
}

function mountForm(initialEntries: string[] = ['/form'], initialIndex = initialEntries.length - 1) {
  const router = createMemoryRouter(
    [
      {
        element: (
          <>
            <Link to="/elsewhere">去别处</Link>
            <Outlet />
          </>
        ),
        children: [
          { path: '/form', element: <TestForm /> },
          { path: '/elsewhere', element: <h1>别的页面</h1> },
        ],
      },
    ],
    { initialEntries, initialIndex },
  )
  render(
    <I18nProvider>
      <RouterProvider router={router} />
    </I18nProvider>,
  )
  return router
}

/** 已经改过一格的表单。 */
function dirtyForm() {
  const router = mountForm()
  fireEvent.change(screen.getByLabelText('字段'), { target: { value: '改了' } })
  return router
}

describe('未保存改动的两个出口', () => {
  beforeEach(() => {
    Object.values(mocks).forEach((mock) => mock.mockReset())
    window.history.pushState({}, '', '/')
  })

  it('脏状态下点站内链接：弹出确认框，地址不动', async () => {
    const router = dirtyForm()
    fireEvent.click(screen.getByRole('link', { name: '去别处' }))

    const dialog = await screen.findByRole('dialog', { name: '放弃未保存的修改？' })
    expect(dialog).toHaveTextContent('关掉或离开这一页之后，填的内容不会保留。')
    // 「拦住了」的判据是地址没变，不是某个 mock 被调用。
    expect(router.state.location.pathname).toBe('/form')
    expect(screen.queryByRole('heading', { name: '别的页面' })).not.toBeInTheDocument()
  })

  it('选「留下」：留在原页，填的内容还在', async () => {
    const router = dirtyForm()
    fireEvent.click(screen.getByRole('link', { name: '去别处' }))
    const dialog = await screen.findByRole('dialog', { name: '放弃未保存的修改？' })

    fireEvent.click(within(dialog).getByRole('button', { name: '取消' }))

    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(router.state.location.pathname).toBe('/form')
    expect(screen.getByLabelText('字段')).toHaveValue('改了')
  })

  it('选「留下」之后再点同一个链接：照样拦，地址仍不变', async () => {
    const router = dirtyForm()
    fireEvent.click(screen.getByRole('link', { name: '去别处' }))
    const first = await screen.findByRole('dialog', { name: '放弃未保存的修改？' })

    fireEvent.click(within(first).getByRole('button', { name: '取消' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(router.state.location.pathname).toBe('/form')

    // 作废的只是这一次跳转；阻止器要回到待命，下一次跳转仍得经过它。
    // 少了这一步复位，下面这行会被静默吞掉——没有框、也没有报错。
    fireEvent.click(screen.getByRole('link', { name: '去别处' }))
    const second = await screen.findByRole('dialog', { name: '放弃未保存的修改？' })
    expect(second).toHaveTextContent('关掉或离开这一页之后，填的内容不会保留。')
    expect(router.state.location.pathname).toBe('/form')
    expect(screen.queryByRole('heading', { name: '别的页面' })).not.toBeInTheDocument()
  })

  it('选「放弃修改」：这才跳走', async () => {
    const router = dirtyForm()
    fireEvent.click(screen.getByRole('link', { name: '去别处' }))
    const dialog = await screen.findByRole('dialog', { name: '放弃未保存的修改？' })

    fireEvent.click(within(dialog).getByRole('button', { name: '放弃修改' }))

    expect(await screen.findByRole('heading', { name: '别的页面' })).toBeInTheDocument()
    expect(router.state.location.pathname).toBe('/elsewhere')
  })

  it('后退（POP）也拦：地址不变，选「放弃修改」才真的退', async () => {
    // 前进/后退走的是 POP，与点链接（PUSH）不是同一条判定；这条盯住 POP。
    const router = mountForm(['/elsewhere', '/form'], 1)
    fireEvent.change(screen.getByLabelText('字段'), { target: { value: '改了' } })

    void router.navigate(-1)
    const dialog = await screen.findByRole('dialog', { name: '放弃未保存的修改？' })
    expect(router.state.location.pathname).toBe('/form')
    expect(screen.queryByRole('heading', { name: '别的页面' })).not.toBeInTheDocument()

    fireEvent.click(within(dialog).getByRole('button', { name: '放弃修改' }))
    expect(await screen.findByRole('heading', { name: '别的页面' })).toBeInTheDocument()
    expect(router.state.location.pathname).toBe('/elsewhere')
  })

  it('没改过时点站内链接：不拦，直接跳', async () => {
    const router = mountForm()
    fireEvent.click(screen.getByRole('link', { name: '去别处' }))

    expect(await screen.findByRole('heading', { name: '别的页面' })).toBeInTheDocument()
    expect(router.state.location.pathname).toBe('/elsewhere')
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })

  it('保存成功（值回到初始）后跳转不再拦', async () => {
    const router = dirtyForm()
    fireEvent.click(screen.getByRole('button', { name: '保存' }))
    fireEvent.click(screen.getByRole('link', { name: '去别处' }))

    expect(await screen.findByRole('heading', { name: '别的页面' })).toBeInTheDocument()
    expect(router.state.location.pathname).toBe('/elsewhere')
  })

  it('脏状态下刷新被拦下：beforeunload 的默认行为被取消', () => {
    const router = mountForm()
    // 没改过时不该有监听：这一次不能取消默认行为。
    expect(dispatchBeforeUnload().defaultPrevented).toBe(false)

    fireEvent.change(screen.getByLabelText('字段'), { target: { value: '改了' } })
    expect(dispatchBeforeUnload().defaultPrevented).toBe(true)

    // 保存之后监听要撤掉，否则「干净」的页面刷新也弹框。
    fireEvent.click(screen.getByRole('button', { name: '保存' }))
    expect(dispatchBeforeUnload().defaultPrevented).toBe(false)
    expect(router.state.location.pathname).toBe('/form')
  })

  it('没有 data router 时照样能用：不挂阻止器，但 beforeunload 生效', () => {
    // 四个运营页的单测都是裸渲染页面。这条钉住「按 context 判断再挂 blocker」：
    // 谁把它换成无条件 useBlocker，这里会直接抛错。
    render(
      <I18nProvider>
        <TestForm />
      </I18nProvider>,
    )
    fireEvent.change(screen.getByLabelText('字段'), { target: { value: '改了' } })
    expect(dispatchBeforeUnload().defaultPrevented).toBe(true)
  })
})

/**
 * 把一页放进 data router，并在页面上放一个「去别处」的入口。
 *
 * 真页面里这个入口是侧栏；这里只关心「导航发生时会先问一句」，所以用最朴素的
 * 一条链接，省掉外壳。四处弹窗表单的真实入口是浏览器后退（见 `TestForm` 上方的
 * 说明）；整页表单（设置页 / 账号页）才是点侧栏。
 */
function mountPage(page: ReactNode, path: string) {
  const router = createMemoryRouter(
    [
      {
        element: (
          <>
            <Link to="/elsewhere">去别处</Link>
            <Outlet />
          </>
        ),
        children: [
          { path, element: page },
          { path: '/elsewhere', element: <h1>别的页面</h1> },
        ],
      },
    ],
    { initialEntries: [path] },
  )
  render(
    <I18nProvider>
      <ThemeProvider>
        <ToastProvider>
          <SiteProvider>
            <AuthProvider>
              <RouterProvider router={router} />
            </AuthProvider>
          </SiteProvider>
        </ToastProvider>
      </ThemeProvider>
    </I18nProvider>,
  )
  return router
}

/** 会话恢复完再挂页面：设置页在首次渲染时读一次 user。 */
function Gate({ children }: { children: ReactNode }) {
  const { status } = useAuth()
  return status === 'authenticated' ? <>{children}</> : null
}

async function clickOtherEntry(router: ReturnType<typeof mountPage>) {
  fireEvent.click(screen.getByRole('link', { name: '去别处' }))
  const dialog = await screen.findByRole('dialog', { name: '放弃未保存的修改？' })
  expect(router.state.location.pathname).not.toBe('/elsewhere')
  return dialog
}

describe('四处弹窗表单与整页表单接的是同一层', () => {
  beforeEach(() => {
    Object.values(mocks).forEach((mock) => mock.mockReset())
    mocks.session.mockResolvedValue({
      user: USER,
      csrf_token: 'csrf',
      expires_at: '2026-12-01T00:00:00Z',
    })
    mocks.meta.mockResolvedValue({
      site_name: 'larkax',
      api_base_url: 'https://api.example.com/v1',
      currency: 'USD',
      timezone: 'UTC',
      locales: ['en', 'zh'],
      features: { registration: false, password_reset: 'admin', topup: false },
    })
    mocks.listAliases.mockResolvedValue([])
    mocks.listChannels.mockResolvedValue([CHANNEL])
    mocks.listModels.mockResolvedValue([])
    mocks.listAdapters.mockResolvedValue([])
    mocks.listRules.mockResolvedValue([])
    mocks.listCosts.mockResolvedValue([])
  })

  it('别名：改了字段以后点别的入口要确认', async () => {
    const router = mountPage(<OpsAliases />, '/app/admin/aliases')
    fireEvent.click((await screen.findAllByRole('button', { name: '新建别名' }))[0])
    const form = await screen.findByRole('dialog', { name: '新建别名' })
    fireEvent.change(within(form).getAllByRole('textbox')[0], { target: { value: 'upstream-x' } })

    await clickOtherEntry(router)
  })

  it('放弃关掉表单之后，这一页不再算改过：再点别的入口不拦', async () => {
    const router = mountPage(<OpsAliases />, '/app/admin/aliases')
    fireEvent.click((await screen.findAllByRole('button', { name: '新建别名' }))[0])
    const form = await screen.findByRole('dialog', { name: '新建别名' })
    fireEvent.change(within(form).getAllByRole('textbox')[0], { target: { value: 'upstream-x' } })

    // 关窗走的是弹窗自己的出口，不是路由出口。
    fireEvent.click(within(form).getByRole('button', { name: '关闭' }))
    const discard = await screen.findByRole('dialog', { name: '放弃未保存的修改？' })
    fireEvent.click(within(discard).getByRole('button', { name: '放弃修改' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())

    // `closeForm` 不重置表单值，只是把表单关了；脏状态必须跟着一起归零，
    // 否则这一页从此永久拦住所有跳转。
    fireEvent.click(screen.getByRole('link', { name: '去别处' }))
    expect(await screen.findByRole('heading', { name: '别的页面' })).toBeInTheDocument()
    expect(router.state.location.pathname).toBe('/elsewhere')
  })

  it('渠道：改了字段以后点别的入口要确认', async () => {
    const router = mountPage(<OpsChannels scope="admin" />, '/app/admin/channels')
    fireEvent.click((await screen.findAllByRole('button', { name: '新建渠道' }))[0])
    const form = await screen.findByRole('dialog', { name: '新建渠道' })
    fireEvent.change(within(form).getAllByRole('textbox')[0], { target: { value: 'channel-x' } })

    await clickOtherEntry(router)
  })

  it('渠道：放弃关掉表单之后，这一页不再算改过：再点别的入口不拦', async () => {
    const router = mountPage(<OpsChannels scope="admin" />, '/app/admin/channels')
    fireEvent.click((await screen.findAllByRole('button', { name: '新建渠道' }))[0])
    const form = await screen.findByRole('dialog', { name: '新建渠道' })
    fireEvent.change(within(form).getAllByRole('textbox')[0], { target: { value: 'channel-x' } })

    fireEvent.click(within(form).getByRole('button', { name: '关闭' }))
    const discard = await screen.findByRole('dialog', { name: '放弃未保存的修改？' })
    fireEvent.click(within(discard).getByRole('button', { name: '放弃修改' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())

    // 与别名那一条同理：`closeForm` 只关表单、不重置 form/initialForm，
    // 脏状态必须靠「表单还开着」这半个条件一起归零。
    fireEvent.click(screen.getByRole('link', { name: '去别处' }))
    expect(await screen.findByRole('heading', { name: '别的页面' })).toBeInTheDocument()
    expect(router.state.location.pathname).toBe('/elsewhere')
  })

  it('模型目录：改了字段以后点别的入口要确认', async () => {
    const router = mountPage(<OpsModels />, '/app/admin/models')
    fireEvent.click((await screen.findAllByRole('button', { name: '新建模型' }))[0])
    const form = await screen.findByRole('dialog', { name: '新建模型' })
    fireEvent.change(within(form).getAllByRole('textbox')[0], { target: { value: 'vendor/model-x' } })

    await clickOtherEntry(router)
  })

  it('模型目录：放弃关掉表单之后，这一页不再算改过：再点别的入口不拦', async () => {
    const router = mountPage(<OpsModels />, '/app/admin/models')
    fireEvent.click((await screen.findAllByRole('button', { name: '新建模型' }))[0])
    const form = await screen.findByRole('dialog', { name: '新建模型' })
    fireEvent.change(within(form).getAllByRole('textbox')[0], { target: { value: 'vendor/model-x' } })

    fireEvent.click(within(form).getByRole('button', { name: '关闭' }))
    const discard = await screen.findByRole('dialog', { name: '放弃未保存的修改？' })
    fireEvent.click(within(discard).getByRole('button', { name: '放弃修改' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())

    // `closeForm` 重置的是「表单开没开」，form/initialForm 原样留着；
    // 判脏还得看表单是否开着，否则放弃之后整页永久算改过。
    fireEvent.click(screen.getByRole('link', { name: '去别处' }))
    expect(await screen.findByRole('heading', { name: '别的页面' })).toBeInTheDocument()
    expect(router.state.location.pathname).toBe('/elsewhere')
  })

  it('计价规则：改了字段以后点别的入口要确认', async () => {
    const router = mountPage(<OpsPricing />, '/app/admin/pricing')
    fireEvent.click(await screen.findByRole('button', { name: '新建规则' }))
    const form = await screen.findByRole('dialog', { name: '新建规则' })
    fireEvent.change(within(form).getAllByRole('textbox')[0], { target: { value: 'rule-x' } })

    await clickOtherEntry(router)
  })

  it('计价规则：放弃关掉表单之后，这一页不再算改过：再点别的入口不拦', async () => {
    const router = mountPage(<OpsPricing />, '/app/admin/pricing')
    fireEvent.click(await screen.findByRole('button', { name: '新建规则' }))
    const form = await screen.findByRole('dialog', { name: '新建规则' })
    fireEvent.change(within(form).getAllByRole('textbox')[0], { target: { value: 'rule-x' } })

    fireEvent.click(within(form).getByRole('button', { name: '关闭' }))
    const discard = await screen.findByRole('dialog', { name: '放弃未保存的修改？' })
    fireEvent.click(within(discard).getByRole('button', { name: '放弃修改' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())

    // 两张表单同页各算各的：规则这张放弃之后，规则那半个条件要归零，
    // 不能把整页拖成永久改过。
    //
    // 这里「成本那一半是干净的」不是恒真前提，而是这两条用例一起钉出来的：
    // 删掉任一张表单的脏判定条件，都只有对应用例变红（与下面成本那条互为交叉证明）。
    fireEvent.click(screen.getByRole('link', { name: '去别处' }))
    expect(await screen.findByRole('heading', { name: '别的页面' })).toBeInTheDocument()
    expect(router.state.location.pathname).toBe('/elsewhere')
  })

  it('计价成本：放弃关掉表单之后，这一页不再算改过：再点别的入口不拦', async () => {
    const router = mountPage(<OpsPricing />, '/app/admin/pricing')
    // 成本表单在另一个 tab 下：切过去之后新建按钮才会换成「新建成本规则」。
    fireEvent.click(await screen.findByRole('button', { name: '成本规则' }))
    fireEvent.click(await screen.findByRole('button', { name: '新建成本规则' }))
    const form = await screen.findByRole('dialog', { name: '新建成本规则' })
    fireEvent.change(within(form).getAllByRole('textbox')[0], { target: { value: 'gpt-*' } })

    fireEvent.click(within(form).getByRole('button', { name: '关闭' }))
    const discard = await screen.findByRole('dialog', { name: '放弃未保存的修改？' })
    fireEvent.click(within(discard).getByRole('button', { name: '放弃修改' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())

    // 与上面规则那条互为镜像：成本这半个条件归零，规则那一半（全程没开）不受影响。
    //
    // 「规则那一半不受影响」同样不是恒真前提，而是这两条用例一起钉出来的：
    // 删掉 `costDirty` 里的条件只有这一条变红，删掉 `ruleDirty` 里的只有规则那条变红。
    fireEvent.click(screen.getByRole('link', { name: '去别处' }))
    expect(await screen.findByRole('heading', { name: '别的页面' })).toBeInTheDocument()
    expect(router.state.location.pathname).toBe('/elsewhere')
  })

  it('设置页的账户信息：改了以后点别的入口要确认，刷新也拦', async () => {
    const router = mountPage(
      <Gate>
        <Settings />
      </Gate>,
      '/app/settings',
    )
    const name = await screen.findByLabelText('显示名')
    fireEvent.change(name, { target: { value: '换了个名字' } })

    expect(dispatchBeforeUnload().defaultPrevented).toBe(true)
    await clickOtherEntry(router)
  })

  it('设置页保存成功后跳转不拦', async () => {
    mocks.updateMe.mockResolvedValue({ ...USER, display_name: '换了个名字' })
    const router = mountPage(
      <Gate>
        <Settings />
      </Gate>,
      '/app/settings',
    )
    fireEvent.change(await screen.findByLabelText('显示名'), { target: { value: '换了个名字' } })
    fireEvent.click(screen.getByRole('button', { name: '保存账户信息' }))
    // 等保存落地：`setUser` 换了基准值，脏状态才归零。
    expect(await screen.findByText('账户信息已保存。')).toBeInTheDocument()

    fireEvent.click(screen.getByRole('link', { name: '去别处' }))
    expect(await screen.findByRole('heading', { name: '别的页面' })).toBeInTheDocument()
    expect(router.state.location.pathname).toBe('/elsewhere')
  })

  it('设置页保存后两格都换成服务端回传的值：之后跳转与刷新都不拦', async () => {
    // 两格都要跟着回传值复位，否则会被一直算成「改过」——提示已保存，随后每次
    // 跳转仍弹框。这里钉的是「表单以服务端回传值为准」这条契约，不假设服务端
    // 会不会改写，两格各造一种「回传值与输入不同」的形状，删掉任何一格的复位
    // 这一条都会红：显示名靠首尾空格造差异；`type="email"` 自己会吃掉首尾空白，
    // 所以邮箱直接给一个与回传值写法不同的值。
    mocks.updateMe.mockResolvedValue({
      ...USER,
      display_name: '换了个名字',
      email: 'user@example.com',
    })
    const router = mountPage(
      <Gate>
        <Settings />
      </Gate>,
      '/app/settings',
    )
    fireEvent.change(await screen.findByLabelText('显示名'), { target: { value: ' 换了个名字 ' } })
    fireEvent.change(screen.getByLabelText('邮箱'), { target: { value: ' User@Example.COM ' } })
    fireEvent.click(screen.getByRole('button', { name: '保存账户信息' }))
    expect(await screen.findByText('账户信息已保存。')).toBeInTheDocument()

    expect(dispatchBeforeUnload().defaultPrevented).toBe(false)

    fireEvent.click(screen.getByRole('link', { name: '去别处' }))
    expect(await screen.findByRole('heading', { name: '别的页面' })).toBeInTheDocument()
    expect(router.state.location.pathname).toBe('/elsewhere')
  })
})
