// 用例跑在 Node 里，读文件没问题；但 web 的 tsconfig 不带 Node 的类型
// （`@types/node` 没有装），所以这一行要靠指令压掉「找不到模块」。
// 哪天真装了 Node 类型，这条指令反而会报「多余」，正好提醒把它删掉。
// @ts-expect-error node:fs 只在 vitest 的 Node 运行时里用，不进构建产物
import { readFileSync } from 'node:fs'
import { fireEvent, render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { AuthProvider } from '../auth/AuthProvider'
import { I18nProvider } from '../i18n'
import { AppShell } from './AppShell'
import { ToastProvider } from './ui'
import { mockViewport } from '../test/media'

// vitest 不处理 CSS（`test.css: false`），`?raw` 拿到的也是空串；直接读文件。
// 要断言生效样式的用例按 `main.tsx` 的导入顺序注入整份样式表——只抽一条规则
// 注入，后面出现的覆盖规则（比如 `.skip-link:not(:focus) { display: none }`）
// 就看不见了，而那种覆盖恰恰会让链接聚不了焦。
// vitest 从 web/ 目录跑，相对路径即可。
const stylesCss = ['tokens.css', 'base.css', 'components.css']
  .map((name) => readFileSync(`src/styles/${name}`, 'utf8'))
  .join('\n')

/** 把整份样式表注入文档，返回撤销函数。 */
function injectStyles() {
  const style = document.createElement('style')
  style.textContent = stylesCss
  document.head.appendChild(style)
  return () => style.remove()
}

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
  const result = render(
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
  return result
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

  it('admin 有用户、商家与管理三个视图，运营导航四组齐全', async () => {
    await renderShell('admin', '/app/admin/channels')
    for (const label of ['用户', '商家', '管理']) {
      expect(screen.getByRole('button', { name: label })).toBeInTheDocument()
    }
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
    // 管理员也有商家视图，但只有三种主语的那三页（见下一条用例）。
    for (const label of ['用户', '商家', '管理']) {
      expect(within(switcher).getByRole('button', { name: label })).toBeInTheDocument()
    }

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

describe('AppShell 跳到主内容', () => {
  /** 取出某条规则声明的原文；后面断言「有没有这一项」时用。 */
  function ruleBody(pattern: RegExp) {
    return stylesCss.match(pattern)?.[1] ?? ''
  }

  function useMatchMedia(matches: boolean) {
    const previous = window.matchMedia
    window.matchMedia = ((query: string) =>
      ({
        matches: matches && query.includes('max-width'),
        media: query,
        onchange: null,
        addEventListener: () => undefined,
        removeEventListener: () => undefined,
        addListener: () => undefined,
        removeListener: () => undefined,
        dispatchEvent: () => false,
      }) as unknown as MediaQueryList) as typeof window.matchMedia
    return () => {
      window.matchMedia = previous
    }
  }

  it('第一次按 Tab 落在「跳到主内容」上', async () => {
    await renderShell('user')
    const user = userEvent.setup()
    await user.tab()
    expect(document.activeElement).toBe(screen.getByRole('link', { name: '跳到主内容' }))
  })

  it('回车后焦点落在主内容容器上', async () => {
    await renderShell('user')
    const user = userEvent.setup()
    await user.tab()
    await user.keyboard('{Enter}')

    const main = document.getElementById('main-content')
    expect(main?.tagName).toBe('MAIN')
    expect(document.activeElement).toBe(main)
    // `-1` 让它能被程序聚焦，但**不**进入 Tab 序列；写成 `0` 会让每个键盘用户
    // 多走一站，还会把主内容当成可编辑区域。
    expect(main).toHaveAttribute('tabindex', '-1')
  })

  it('默认不可见但可聚焦，聚焦态另有一套样式', async () => {
    const base = ruleBody(/\.skip-link\s*\{([^}]*)\}/)
    const focus = ruleBody(/\.skip-link:focus\s*\{([^}]*)\}/)
    // 隐藏靠 1px 的盒子加裁剪，不能用把元素藏死的两种写法。
    expect(base).toMatch(/position:\s*absolute/)
    expect(base).toMatch(/max-width:\s*1px/)
    expect(base).toMatch(/max-height:\s*1px/)
    expect(base).toMatch(/overflow:\s*hidden/)
    expect(base).toMatch(/clip-path:\s*inset\(50%\)/)
    expect(base).not.toMatch(/display:\s*none/)
    expect(base).not.toMatch(/visibility:\s*hidden/)
    // 它一直在左上角（只是被裁成一个点），聚焦时只还原尺寸与裁剪；位置与
    // 层级必须压过顶栏，否则聚焦后弹在顶栏下面，等于没弹。
    expect(base).toMatch(/top:\s*\d+px/)
    expect(base).toMatch(/left:\s*\d+px/)
    expect(base).toMatch(/background:/)
    const linkZ = Number(base.match(/z-index:\s*(\d+)/)?.[1])
    const topbarZ = Number(ruleBody(/\.app-topbar\s*\{([^}]*)\}/).match(/z-index:\s*(\d+)/)?.[1])
    expect(topbarZ).toBeGreaterThan(0)
    expect(linkZ).toBeGreaterThan(topbarZ)
    expect(focus).toMatch(/max-width:\s*none/)
    expect(focus).toMatch(/max-height:\s*none/)
    expect(focus).toMatch(/clip-path:\s*none/)

    const removeStyles = injectStyles()
    try {
      await renderShell('user')
      const link = screen.getByRole('link', { name: '跳到主内容' })
      // 整份样式表生效之后的 computed 值：藏死的写法会在这里露馅。
      const computed = getComputedStyle(link)
      expect(computed.display).not.toBe('none')
      expect(computed.visibility).not.toBe('hidden')
      expect(computed.position).toBe('absolute')
      expect(computed.maxWidth).toBe('1px')
      expect(computed.maxHeight).toBe('1px')
      expect(computed.overflow).toBe('hidden')
      // `focus()` 本身证明不了可聚焦（jsdom 对 display:none 的元素也照样聚焦），
      // 所以这里用真实的 Tab 走位：user-event 会跳过被藏死的元素。
      const user = userEvent.setup()
      await user.tab()
      expect(document.activeElement).toBe(link)
    } finally {
      removeStyles()
    }
  })

  it('移动端：它仍是第一个落点，跳转不把抽屉带出来', async () => {
    const restore = useMatchMedia(true)
    try {
      mocks.session.mockResolvedValue({
        user: userOf('user'),
        csrf_token: 'csrf',
        expires_at: '2026-12-01T00:00:00Z',
      })
      render(
        <I18nProvider>
          <ToastProvider>
            <AuthProvider>
              <MemoryRouter initialEntries={['/app']}>
                <AppShell />
              </MemoryRouter>
            </AuthProvider>
          </ToastProvider>
        </I18nProvider>,
      )
      // 移动端顶栏只留首字母头像，账号名不出现，等菜单按钮就够了。
      await screen.findByRole('button', { name: '菜单' })
      const user = userEvent.setup()
      await user.tab()
      expect(document.activeElement).toBe(screen.getByRole('link', { name: '跳到主内容' }))

      await user.keyboard('{Enter}')
      expect(document.activeElement).toBe(document.getElementById('main-content'))
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    } finally {
      restore()
    }
  })
})

describe('AppShell 底部 tab', () => {
  beforeEach(() => {
    mocks.session.mockReset()
  })

  /** 底栏按 DOM 顺序的可见文字；弹出型 tab 也是其中一个孩子。 */
  function tabLabels(): string[] {
    const nav = document.querySelector('.bottomnav') as HTMLElement
    return [...nav.children].map((child) => child.textContent ?? '')
  }

  function bottomNav(): HTMLElement {
    return document.querySelector('.bottomnav') as HTMLElement
  }

  it('三种角色的 tab 集合与顺序固定，各自不超过 5 个', async () => {
    const cases = [
      {
        role: 'user' as const,
        path: '/app',
        labels: ['概览', 'API 密钥', '用量', '模型', '设置'],
      },
      {
        role: 'merchant' as const,
        path: '/app/merchant/overview',
        labels: ['概览', '渠道', '用量', '设置'],
      },
      {
        role: 'admin' as const,
        path: '/app/admin/channels',
        labels: ['概览', '渠道', '用量', '用户', '运营'],
      },
    ]
    for (const item of cases) {
      const { unmount } = await renderShell(item.role, item.path)
      expect(tabLabels()).toEqual(item.labels)
      expect(tabLabels().length).toBeLessThanOrEqual(5)
      unmount()
    }
  })

  it('管理员进商家视图时，底栏换成他能进的那四页，当前项不落空', async () => {
    const { unmount } = await renderShell('admin', '/app/merchant/models')
    expect(tabLabels()).toEqual(['概览', '模型', '上游', '用量'])
    expect(within(bottomNav()).getByRole('link', { name: '模型' })).toHaveAttribute(
      'aria-current',
      'page',
    )
    unmount()
  })

  it('当前项的文字本身在，可及名不是空图标', async () => {
    await renderShell('user', '/app/keys')
    const current = within(bottomNav()).getByRole('link', { name: 'API 密钥' })
    expect(current).toHaveAttribute('aria-current', 'page')
    expect(current.textContent).toBe('API 密钥')
  })

  it('商家与管理员的「概览」都指向全站主语那一页', async () => {
    const merchant = await renderShell('merchant', '/app/merchant/channels')
    expect(within(bottomNav()).getByRole('link', { name: '概览' }).getAttribute('href')).toBe(
      '/app/merchant/overview',
    )
    merchant.unmount()

    await renderShell('admin', '/app/admin/channels')
    expect(within(bottomNav()).getByRole('link', { name: '概览' }).getAttribute('href')).toBe(
      '/app/merchant/overview',
    )
  })

  it('管理员的「运营」从底栏弹出二级入口，停在那三页上时算当前项', async () => {
    const { unmount } = await renderShell('admin', '/app/admin/models')
    const ops = within(bottomNav()).getByRole('button', { name: '运营' })
    expect(ops).toHaveAttribute('aria-current', 'page')
    expect(ops).toHaveAttribute('aria-haspopup', 'dialog')

    fireEvent.click(ops)
    const sheet = screen.getByRole('dialog')
    for (const label of ['模型目录', '别名', '计价']) {
      expect(within(sheet).getByRole('link', { name: label })).toBeInTheDocument()
    }
    unmount()

    const other = await renderShell('admin', '/app/admin/channels')
    expect(within(bottomNav()).getByRole('button', { name: '运营' })).not.toHaveAttribute(
      'aria-current',
    )
    other.unmount()
  })
})

describe('AppShell 移动端焦点顺序', () => {
  beforeEach(() => {
    mocks.session.mockReset()
  })

  it('底栏排在主内容之后：Tab 走完正文才进底栏，不被固定栏截断', async () => {
    const restore = mockViewport(375, 667)
    try {
      mocks.session.mockResolvedValue({
        user: userOf('user'),
        csrf_token: 'csrf',
        expires_at: '2026-12-01T00:00:00Z',
      })
      render(
        <I18nProvider>
          <ToastProvider>
            <AuthProvider>
              <MemoryRouter initialEntries={['/app']}>
                <Routes>
                  <Route path="/app" element={<AppShell />}>
                    <Route index element={<button type="button">正文动作</button>} />
                  </Route>
                </Routes>
              </MemoryRouter>
            </AuthProvider>
          </ToastProvider>
        </I18nProvider>,
      )
      await screen.findByRole('button', { name: '菜单' })

      const main = document.getElementById('main-content') as HTMLElement
      const nav = document.querySelector('.bottomnav') as HTMLElement
      // DOM 顺序：主内容在前、底栏在后。视觉上底栏固定在页面底部，但读屏与
      // Tab 走的是这条顺序。
      expect(main.compareDocumentPosition(nav) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()

      const user = userEvent.setup()
      screen.getByRole('button', { name: '正文动作' }).focus()
      await user.tab()
      expect(document.activeElement).toBe(within(nav).getAllByRole('link')[0])
    } finally {
      restore()
    }
  })
})

describe('AppShell 矮视口', () => {
  beforeEach(() => {
    mocks.session.mockReset()
  })

  it('812×375 下侧栏默认收起，点 logo 展开、点收起钮再收回去', async () => {
    const restore = mockViewport(812, 375)
    try {
      await renderShell('admin', '/app/admin/channels')
      const aside = document.querySelector('.sidebar') as HTMLElement
      // 展开的侧栏自己也要滚，主列也要滚，同一页会出现两条滚动条。
      expect(aside.className).toContain('collapsed')

      fireEvent.click(screen.getByRole('button', { name: '展开侧边栏' }))
      expect(aside.className).not.toContain('collapsed')

      fireEvent.click(screen.getByRole('button', { name: '收起侧边栏' }))
      expect(aside.className).toContain('collapsed')
    } finally {
      restore()
    }
  })
})
