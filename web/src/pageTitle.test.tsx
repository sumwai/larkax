import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import type { ReactNode } from 'react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { Link, MemoryRouter, Route, Routes, useNavigate } from 'react-router-dom'
import { AuthProvider } from './auth/AuthProvider'
import { AppShell } from './components/AppShell'
import { ToastProvider } from './components/ui'
import { I18nProvider, useI18n } from './i18n'
import { ApiError } from './api'
import type { ModelDetailResponse } from './api'
import { Landing } from './pages/Landing'
import { Login } from './pages/Login'
import { ModelDetail } from './pages/ModelDetail'
import { SiteProvider } from './site/SiteProvider'
import { ThemeProvider } from './theme/ThemeProvider'

const mocks = vi.hoisted(() => ({
  session: vi.fn(),
  login: vi.fn(),
  logout: vi.fn(),
  models: vi.fn(),
  meta: vi.fn(),
  modelDetail: vi.fn(),
}))

vi.mock('./api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('./api')>()
  return {
    ...actual,
    api: {
      mode: 'http',
      session: mocks.session,
      login: mocks.login,
      logout: mocks.logout,
      models: mocks.models,
      meta: mocks.meta,
      modelDetail: mocks.modelDetail,
      setCsrfToken: vi.fn(),
      setUnauthorizedHandler: vi.fn(),
    },
  }
})

const USER = {
  id: 1,
  username: 'user-account',
  display_name: 'user-account',
  email: null,
  status: 'active',
  role: 'user',
  created_at: '2026-01-01T00:00:00Z',
}

/** 展示名与地址里的名字故意不同：直接用地址段也会在判据 2 上现原形。 */
const DETAIL: ModelDetailResponse = {
  currency: 'USD',
  reference_at: '2026-10-21T09:00:00Z',
  model: {
    name: 'vendor/model-x',
    display_name: 'Model X',
    vendor: 'acme',
    family: 'x',
    context_limit: 200_000,
    input_limit: 0,
    output_limit: 64_000,
    capabilities: {
      reasoning: true,
      tools: false,
      vision: null,
      audio: null,
      cache: true,
      batch: false,
      endpoints: ['chat', 'messages'],
    },
    modalities_input: ['text'],
    modalities_output: ['text'],
    reasoning_options: [],
    canonical_model_id: 'model-x-2026',
    knowledge_cutoff: '2026-01-01',
    release_date: null,
    updated_at: '2026-10-19T09:00:00Z',
    suppliers: [],
    unpriced_channels: 0,
  },
}

/** 控制台里进得去的每一页，以及它该有的页面名。 */
const CONSOLE_ROUTES: Array<[string, string]> = [
  ['/app', '概览'],
  ['/app/keys', 'API 密钥'],
  ['/app/usage', '用量'],
  ['/app/models', '模型'],
  ['/app/quickstart', '快速开始'],
  ['/app/settings', '设置'],
]

function TestNav() {
  const navigate = useNavigate()
  return (
    <div>
      {CONSOLE_ROUTES.map(([path]) => (
        <button key={path} type="button" onClick={() => navigate(path)}>
          {`去 ${path}`}
        </button>
      ))}
    </div>
  )
}

function LanguageSwitch() {
  const { setLang } = useI18n()
  return (
    <button type="button" onClick={() => setLang('en')}>
      switch-language
    </button>
  )
}

function Providers({ path, children }: { path: string; children: ReactNode }) {
  return (
    <I18nProvider>
      <ThemeProvider>
        <ToastProvider>
          <SiteProvider>
            <AuthProvider>
              <MemoryRouter initialEntries={[path]}>{children}</MemoryRouter>
            </AuthProvider>
          </SiteProvider>
        </ToastProvider>
      </ThemeProvider>
    </I18nProvider>
  )
}

function renderConsole(path: string) {
  return render(
    <Providers path={path}>
      <TestNav />
      <LanguageSwitch />
      <Routes>
        <Route path="/app" element={<AppShell />}>
          <Route index element={<p>概览正文</p>} />
          <Route path="keys" element={<p>密钥正文</p>} />
          <Route path="usage" element={<p>用量正文</p>} />
          <Route
            path="models"
            element={
              <p>
                模型正文
                <Link to="/app/models/vendor/model-x">Model X 详情</Link>
              </p>
            }
          />
          <Route path="models/*" element={<ModelDetail />} />
          <Route path="quickstart" element={<p>快速开始正文</p>} />
          <Route path="settings" element={<p>设置正文</p>} />
        </Route>
      </Routes>
    </Providers>,
  )
}

/** 顶栏标题那一行的文字；判据拿它去对拍 `document.title`，不手抄字符串。 */
function topbarTitle(): string {
  const title = screen.getByRole('banner').querySelector('b')?.textContent
  if (!title) throw new Error('顶栏没有标题')
  return title
}

/** 标题里不该出现「页面名 · 页面名」。 */
function expectNoRepeatedName() {
  expect(document.title).not.toMatch(/^(.+) · \1$/)
}

/** 控制台标题的站点后缀；顶栏那一行不带它。 */
const CONSOLE_SUFFIX = ' · larkax 控制台'
const CONSOLE_SUFFIX_EN = ' · larkax Console'

/**
 * 标签页标题 = 顶栏那一行 + 站点后缀。
 *
 * 顶栏文字从 DOM 读，后缀只在这里拼；再断后缀只出现在末尾一次，
 * 页名自己含后缀时上面那条就红，`expectNoRepeatedName` 才有牙齿。
 */
function expectConsoleTitle(suffix = CONSOLE_SUFFIX) {
  const expected = `${topbarTitle()}${suffix}`
  expect(document.title).toBe(expected)
  expect(document.title.indexOf(suffix)).toBe(expected.length - suffix.length)
  expect(document.title.lastIndexOf(suffix)).toBe(expected.length - suffix.length)
  expectNoRepeatedName()
}

describe('控制台标题', () => {
  beforeEach(() => {
    Object.values(mocks).forEach((mock) => mock.mockReset())
    // `index.html` 里的静态标题；页面的钩子必须把它覆盖掉。
    document.title = 'larkax 控制台'
    mocks.session.mockResolvedValue({
      user: USER,
      csrf_token: 'csrf',
      expires_at: '2026-12-01T00:00:00Z',
    })
    mocks.logout.mockResolvedValue(undefined)
    mocks.models.mockResolvedValue([])
    mocks.meta.mockResolvedValue({
      site_name: 'larkax',
      api_base_url: 'https://api.example.com/v1',
      currency: 'USD',
      timezone: 'Asia/Shanghai',
      locales: ['en', 'zh'],
      features: { registration: false, password_reset: 'admin', topup: false },
    })
    mocks.modelDetail.mockResolvedValue(DETAIL)
  })

  it('各主要路由的标题与顶栏对拍，且互不相同', async () => {
    await renderConsole('/app')
    await screen.findAllByText('user-account')

    const titles = new Set<string>()
    for (const [path, expected] of CONSOLE_ROUTES) {
      fireEvent.click(screen.getByRole('button', { name: `去 ${path}` }))
      await waitFor(() => expect(topbarTitle()).toBe(expected))
      expectConsoleTitle()
      titles.add(document.title)
    }

    expect(titles.size).toBe(CONSOLE_ROUTES.length)
  })

  it('模型详情页的标题是展示名，不是地址里的名字', async () => {
    await renderConsole('/app/models/vendor/model-x')
    await screen.findAllByText('user-account')

    await waitFor(() => expect(topbarTitle()).toBe('Model X'))
    expectConsoleTitle()
    // 展示名之外的两条岔路：父级页面名、地址段，都不能是它。
    expect(document.title).not.toBe(`模型${CONSOLE_SUFFIX}`)
    expect(document.title).not.toContain('vendor/model-x')
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('Model X')

    // 回到列表页：详情页的报告撤掉，标题退回导航反推的名字。
    fireEvent.click(screen.getByRole('button', { name: '去 /app/models' }))
    await waitFor(() => expect(topbarTitle()).toBe('模型'))
    expectConsoleTitle()
  })

  it('详情页数据没到时标题先写「模型」，到货后换成展示名', async () => {
    let resolveDetail: (value: ModelDetailResponse) => void = () => undefined
    mocks.modelDetail.mockReturnValue(
      new Promise<ModelDetailResponse>((resolve) => {
        resolveDetail = resolve
      }),
    )
    await renderConsole('/app/models/vendor/model-x')
    await screen.findAllByText('user-account')
    await waitFor(() => expect(topbarTitle()).toBe('模型'))
    expectConsoleTitle()

    await act(async () => {
      resolveDetail(DETAIL)
    })
    await waitFor(() => expect(topbarTitle()).toBe('Model X'))
    expectConsoleTitle()
  })

  it('详情页 404 时标题回落到「模型」', async () => {
    mocks.modelDetail.mockRejectedValue(new ApiError(404, 'not_found', 'missing'))
    await renderConsole('/app/models/vendor/missing')
    await screen.findAllByText('user-account')
    await screen.findByText(/找不到这个模型/)
    expect(topbarTitle()).toBe('模型')
    expectConsoleTitle()
  })

  it('切换语言后标题跟着变，并等于切换后的顶栏文字', async () => {
    await renderConsole('/app/settings')
    await screen.findAllByText('user-account')
    await waitFor(() => expect(topbarTitle()).toBe('设置'))
    expectConsoleTitle()

    fireEvent.click(screen.getByRole('button', { name: 'switch-language' }))
    await waitFor(() => expect(topbarTitle()).toBe('Settings'))
    // 后缀跟着语言走：英文下是「larkax Console」。
    expectConsoleTitle(CONSOLE_SUFFIX_EN)
  })
})

describe('公开页标题', () => {
  beforeEach(() => {
    Object.values(mocks).forEach((mock) => mock.mockReset())
    // `index.html` 里的静态标题；公开页的钩子必须把它覆盖掉。
    document.title = 'larkax 控制台'
    mocks.session.mockResolvedValue(null)
    mocks.models.mockResolvedValue([])
    mocks.meta.mockResolvedValue({
      site_name: 'larkax',
      api_base_url: 'https://api.example.com/v1',
      currency: 'USD',
      timezone: 'Asia/Shanghai',
      locales: ['en', 'zh'],
      features: { registration: false, password_reset: 'admin', topup: false },
    })
  })

  it('落地页有标题，且不含「控制台」', async () => {
    render(
      <Providers path="/">
        <Landing />
      </Providers>,
    )

    const heading = await screen.findByRole('heading', { level: 1 })
    await waitFor(() => expect(document.title).toBe(heading.textContent))
    // 不是 `index.html` 那个静态兜底，也不是页内大标题里带「控制台」的那句。
    expect(document.title).not.toBe('larkax 控制台')
    expect(document.title).not.toContain('控制台')
    expectNoRepeatedName()
  })

  it('登录页有标题，且不含「控制台」', async () => {
    render(
      <Providers path="/login">
        <Login />
      </Providers>,
    )

    await screen.findByRole('heading', { name: '登录控制台' })
    await waitFor(() => expect(document.title).toBe('登录'))
    expect(document.title).not.toBe('larkax 控制台')
    expect(document.title).not.toContain('控制台')
    expectNoRepeatedName()
  })
})
