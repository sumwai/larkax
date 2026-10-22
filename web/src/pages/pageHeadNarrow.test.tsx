import { render, screen } from '@testing-library/react'
import type { ReactElement } from 'react'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { I18nProvider } from '../i18n'
import { en } from '../i18n/en'
import { zh, type MessageKey } from '../i18n/zh'
import { SiteProvider } from '../site/SiteProvider'
import { ThemeProvider } from '../theme/ThemeProvider'
import { ToastProvider } from '../components/ui'
import { Overview } from './Overview'
import { MerchantSettings, Settings } from './Settings'
import { Usage } from './Usage'
import { Keys } from './Keys'
import { Models } from './Models'
import { Quickstart } from './Quickstart'
import { ModelDetail } from './ModelDetail'
import { MerchantAccount } from './MerchantAccount'
import { MerchantOverview } from './ops/MerchantOverview'
import { MerchantModels } from './ops/MerchantModels'
import { MerchantUpstreams } from './ops/MerchantUpstreams'
import { OpsChannels } from './ops/Channels'
import { OpsPricing } from './ops/Pricing'
import { MerchantPricing } from './ops/MerchantPricing'
import { OpsModels } from './ops/Catalog'
import { OpsAliases } from './ops/Aliases'
import { AdminUsers, AdminUserDetailPage } from './admin/Users'

/** 窄屏隐藏标题的那个类，与 components.css 里的选择器同字。 */
const HIDDEN = 'page-head-title-narrow-hidden'

const mocks = vi.hoisted(() => ({ modelDetail: vi.fn() }))

vi.mock('../auth/AuthProvider', () => ({
  useAuth: () => ({
    status: 'authenticated',
    user: {
      id: 2,
      username: 'shop',
      display_name: 'shop',
      email: null,
      status: 'active',
      role: 'merchant',
      created_at: '2026-01-01T00:00:00Z',
    },
    login: vi.fn(),
    logout: vi.fn(),
    setUser: vi.fn(),
  }),
}))

vi.mock('../api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../api')>()
  /*
   * 这些行只关心页头，数据一律停在「还没回来」：没单独给的端点返回一个永不
   * 落地的值，页面停在骨架屏，`PageHead` 照常渲染。模型详情页是例外——它的
   * 页头在拿到模型之后才画，所以那一个端点单独给结果。
   */
  const pending = (): unknown =>
    new Proxy((): Promise<never> => new Promise(() => {}), {
      get: (_target, prop) => (prop === 'then' ? undefined : pending()),
    })
  const stub = new Proxy(
    { mode: 'mock', modelDetail: mocks.modelDetail },
    { get: (target, prop) => (prop in target ? Reflect.get(target, prop) : pending()) },
  )
  return { ...actual, api: stub }
})

type Row = {
  name: string
  url: string
  route: string
  element: ReactElement
  /** 页头 h1 的文案来源：i18n 键，或模型详情页那种运行期文案。 */
  title: { key: MessageKey } | { text: string }
  /** 窄屏顶栏会显示的那一句（`AppShell` 里 `current` 推出的键）。 */
  topbar: MessageKey
  expect: 'hidden' | 'kept'
}

/*
 * 每一行是「页面 → h1 文案来源 → 顶栏文案 → 窄屏下的期望」。
 * 隐藏与保留的分界是文案是否同文：同文才隐藏，不同文必须留着——
 * 否则等于为了去重把页面唯一的那句标题也去掉。
 *
 * 商家视图那几页（MerchantOverview / MerchantModels / MerchantUpstreams /
 * MerchantPricing）只按 i18n 文案判过，没有浏览器实测：demo 账号进不去商家
 * 视图，实测那两格是「无权限」。
 */
const ROWS: Row[] = [
  { name: '概览', url: '/app', route: '/app', element: <Overview />, title: { key: 'overview.title' }, topbar: 'nav.overview', expect: 'hidden' },
  { name: '密钥', url: '/app/keys', route: '/app/keys', element: <Keys />, title: { key: 'keys.title' }, topbar: 'nav.keys', expect: 'hidden' },
  { name: '用量', url: '/app/usage', route: '/app/usage', element: <Usage />, title: { key: 'usage.title' }, topbar: 'nav.usage', expect: 'hidden' },
  { name: '模型列表', url: '/app/models', route: '/app/models', element: <Models />, title: { key: 'models.title' }, topbar: 'nav.models', expect: 'hidden' },
  { name: '快速开始', url: '/app/quickstart', route: '/app/quickstart', element: <Quickstart />, title: { key: 'quickstart.title' }, topbar: 'nav.quickstart', expect: 'hidden' },
  { name: '设置', url: '/app/settings', route: '/app/settings', element: <Settings />, title: { key: 'settings.title' }, topbar: 'nav.settings', expect: 'hidden' },
  { name: '商家设置', url: '/app/merchant/settings', route: '/app/merchant/settings', element: <MerchantSettings />, title: { key: 'settings.title' }, topbar: 'nav.settings', expect: 'hidden' },
  { name: '账号', url: '/app/merchant/account', route: '/app/merchant/account', element: <MerchantAccount />, title: { key: 'nav.account' }, topbar: 'nav.account', expect: 'hidden' },
  { name: '模型详情', url: '/app/models/model-x', route: '/app/models/*', element: <ModelDetail />, title: { text: 'Model X' }, topbar: 'nav.models', expect: 'kept' },
  { name: '商家概览', url: '/app/merchant/overview', route: '/app/merchant/overview', element: <MerchantOverview />, title: { key: 'ops.merchant.overview.title.mine' }, topbar: 'nav.overview', expect: 'kept' },
  { name: '商家模型', url: '/app/merchant/models', route: '/app/merchant/models', element: <MerchantModels />, title: { key: 'ops.merchant.models.title.mine' }, topbar: 'ops.nav.merchantModels', expect: 'kept' },
  { name: '上游', url: '/app/merchant/upstreams', route: '/app/merchant/upstreams', element: <MerchantUpstreams />, title: { key: 'ops.merchant.upstreams.title.mine' }, topbar: 'ops.nav.upstreams', expect: 'kept' },
  { name: '渠道（商家）', url: '/app/merchant/channels', route: '/app/merchant/channels', element: <OpsChannels scope="merchant" />, title: { key: 'ops.channel.title' }, topbar: 'ops.nav.channels', expect: 'hidden' },
  { name: '渠道（管理）', url: '/app/admin/channels', route: '/app/admin/channels', element: <OpsChannels scope="admin" />, title: { key: 'ops.channel.title' }, topbar: 'ops.nav.channels', expect: 'hidden' },
  { name: '模型目录', url: '/app/admin/models', route: '/app/admin/models', element: <OpsModels />, title: { key: 'ops.model.title' }, topbar: 'ops.nav.models', expect: 'hidden' },
  { name: '计价（管理）', url: '/app/admin/pricing', route: '/app/admin/pricing', element: <OpsPricing />, title: { key: 'ops.pricing.title' }, topbar: 'ops.nav.pricing', expect: 'hidden' },
  { name: '计价（商家）', url: '/app/merchant/pricing', route: '/app/merchant/pricing', element: <MerchantPricing />, title: { key: 'ops.pricing.merchantTitle' }, topbar: 'ops.nav.pricing', expect: 'hidden' },
  { name: '别名', url: '/app/admin/aliases', route: '/app/admin/aliases', element: <OpsAliases />, title: { key: 'ops.alias.title' }, topbar: 'ops.nav.aliases', expect: 'kept' },
  { name: '用户列表', url: '/app/admin/users', route: '/app/admin/users', element: <AdminUsers />, title: { key: 'ops.users.title' }, topbar: 'ops.nav.users', expect: 'hidden' },
  { name: '用户详情', url: '/app/admin/users/7', route: '/app/admin/users/:id', element: <AdminUserDetailPage />, title: { key: 'ops.users.detailTitle' }, topbar: 'ops.nav.users', expect: 'kept' },
]

beforeEach(() => {
  mocks.modelDetail.mockReset()
  mocks.modelDetail.mockResolvedValue({
    currency: 'USD',
    reference_at: '2026-10-21T09:00:00Z',
    model: {
      name: 'vendor/model-x',
      display_name: 'Model X',
      vendor: 'acme',
      family: 'x',
      canonical_model_id: 'model-x-2026',
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
        endpoints: ['chat'],
      },
      modalities_input: ['text'],
      modalities_output: ['text'],
      reasoning_options: [],
      knowledge_cutoff: null,
      release_date: null,
      suppliers: [],
    },
  })
})

function renderPage(row: Row) {
  return render(
    <MemoryRouter initialEntries={[row.url]}>
      <I18nProvider>
        <ThemeProvider>
          <ToastProvider>
            <SiteProvider>
              <Routes>
                <Route path={row.route} element={row.element} />
              </Routes>
            </SiteProvider>
          </ToastProvider>
        </ThemeProvider>
      </I18nProvider>
    </MemoryRouter>,
  )
}

describe('窄屏页头标题：按「是否与顶栏同文」决定隐藏还是保留', () => {
  it.each(ROWS)('$name', async (row) => {
    renderPage(row)
    // 模型详情页拿到模型之后才画页头，所以这里等一等，其余页面同步就有。
    const h1 = await screen.findByRole('heading', { level: 1 })

    expect(h1.classList.contains(HIDDEN), `${row.name}：窄屏是否隐藏`).toBe(
      row.expect === 'hidden',
    )

    if ('key' in row.title) {
      // 页面确实画的是表里说的那个键，而不是另一个碰巧同文的键。
      // 界面语言由运行环境决定，两本词典里取到哪一本都算数。
      expect(
        [zh[row.title.key], en[row.title.key]],
        `${row.name}：页面标题的文案来源`,
      ).toContain(h1.textContent)
      // 决定是照文案判的：两本词典都得成立，谁改了文案这一行就红。
      for (const [lang, dict] of [
        ['zh', zh],
        ['en', en],
      ] as const) {
        const same = dict[row.title.key] === dict[row.topbar]
        expect(
          same,
          `${row.name}：${lang} 里 ${row.title.key} 与 ${row.topbar} 是否同文`,
        ).toBe(row.expect === 'hidden')
      }
    } else {
      // 运行期文案（模型显示名）永远可能与顶栏不同文，必须保留。
      expect(h1.textContent, `${row.name}：模型显示名`).toBe(row.title.text)
      expect(
        [zh[row.topbar], en[row.topbar]],
        `${row.name}：不能与顶栏同文`,
      ).not.toContain(h1.textContent)
    }
  })
})
