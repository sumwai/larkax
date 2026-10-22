// 用例跑在 Node 里，读文件没问题；但 web 的 tsconfig 不带 Node 的类型
// （`@types/node` 没有装），所以这一行要靠指令压掉「找不到模块」。
// @ts-expect-error node:fs 只在 vitest 的 Node 运行时里用，不进构建产物
import { readFileSync } from 'node:fs'
import { fireEvent, render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { MemoryRouter } from 'react-router-dom'
import { Models } from './Models'
import { I18nProvider } from '../i18n'
import type { Model } from '../api'

const mocks = vi.hoisted(() => ({ models: vi.fn() }))

// `ApiError` 要真的：错误分档的用例按状态码走，mock 掉就没得判。
vi.mock('../api', async () => {
  const actual = await vi.importActual<typeof import('../api')>('../api')
  return { ...actual, api: { models: mocks.models } }
})

const ITEMS: Model[] = [
  {
    name: 'claude-sonnet-4-5',
    display_name: 'Claude Sonnet 4.5',
    vendor: 'anthropic',
    family: 'claude',
    context_limit: 200_000,
    max_output: 64_000,
    capabilities: { vision: true, tools: true, reasoning: true, caching: false },
    modalities: { input: ['text', 'image'], output: ['text'] },
    pricing: {
      input_per_mtok: '3.00000000',
      output_per_mtok: '15.00000000',
      cache_read_per_mtok: '0.30000000',
      cache_write_per_mtok: null,
      reasoning_per_mtok: null,
    },
    offers: [
      { merchant: 'acme', price: '0.00350000' },
      { merchant: null, price: '0.00400000' },
    ],
  },
  {
    name: 'gpt-4.1',
    // 显示名没填时用规范名。
    display_name: '',
    vendor: 'openai',
    family: 'gpt',
    context_limit: 1_047_576,
    max_output: 32_768,
    capabilities: { tools: true, vision: false },
    modalities: { input: ['text'], output: [] },
    pricing: null,
  },
]

function renderModels() {
  return render(
    <I18nProvider>
      <MemoryRouter initialEntries={['/app/models']}>
        <Models />
      </MemoryRouter>
    </I18nProvider>,
  )
}

describe('模型列表', () => {
  beforeEach(() => {
    mocks.models.mockReset()
    mocks.models.mockResolvedValue({
      currency: 'USD',
      reference_at: '2026-10-21T09:00:00Z',
      items: ITEMS,
    })
  })

  it('列表展示目录字段，不展示价格与供应商', async () => {
    renderModels()

    const row = (await screen.findByRole('link', { name: /Claude Sonnet 4\.5/ }))
    expect(row).toHaveTextContent('anthropic · claude')
    expect(row).toHaveTextContent('上下文 200K')
    expect(row).toHaveTextContent('输入 text / image')
    expect(row).toHaveTextContent('输出 text')
    // 只列明确支持的能力：caching 为 false，不该出现角标。
    expect(row).toHaveTextContent('视觉')
    expect(row).toHaveTextContent('工具调用')
    expect(row).toHaveTextContent('推理')
    expect(row).not.toHaveTextContent('缓存')

    // 报价与供应商信息属于详情页。
    expect(screen.queryByText('供应与报价')).not.toBeInTheDocument()
    expect(screen.queryByText(/0\.0035/)).not.toBeInTheDocument()
    expect(screen.queryByText(/0\.0040/)).not.toBeInTheDocument()
    expect(screen.queryByText(/acme/)).not.toBeInTheDocument()
    expect(screen.queryByText(/输入 \/ 100 万 token/)).not.toBeInTheDocument()
  })

  it('搜索按名字与系列过滤，厂商筛选的选项来自数据', async () => {
    renderModels()
    await screen.findByRole('link', { name: /Claude Sonnet 4\.5/ })

    const search = screen.getByLabelText('搜索名字、厂商或系列')
    fireEvent.change(search, { target: { value: 'gpt' } })
    expect(screen.queryByRole('link', { name: /Claude Sonnet 4\.5/ })).not.toBeInTheDocument()
    expect(screen.getByRole('link', { name: /gpt-4\.1/ })).toBeInTheDocument()

    fireEvent.change(search, { target: { value: '' } })
    const vendor = screen.getByLabelText('厂商')
    expect(Array.from((vendor as HTMLSelectElement).options).map((option) => option.text)).toEqual([
      '全部厂商',
      'anthropic',
      'openai',
    ])

    fireEvent.change(vendor, { target: { value: 'anthropic' } })
    expect(screen.queryByRole('link', { name: /gpt-4\.1/ })).not.toBeInTheDocument()
    expect(screen.getByRole('link', { name: /Claude Sonnet 4\.5/ })).toBeInTheDocument()
  })

  it('搜索框由共享的 SearchBox 产出，一键清空后回到全量并保持聚焦', async () => {
    renderModels()
    await screen.findByRole('link', { name: /Claude Sonnet 4\.5/ })

    // 根类名与 data-testid 都由共享组件的 SearchBox 给出，不是这一页自己画的；
    // 运营页那份是同一个组件的默认分支（只差左侧放大镜）。
    const box = screen.getByTestId('search-box')
    expect(box.className).toBe('wrap-rel search-box has-icon model-search')

    const rows = () => screen.getAllByRole('link', { name: /Claude Sonnet 4\.5|gpt-4\.1/ })
    expect(rows()).toHaveLength(2)

    const search = screen.getByLabelText('搜索名字、厂商或系列') as HTMLInputElement
    fireEvent.change(search, { target: { value: 'gpt' } })
    expect(rows()).toHaveLength(1)

    fireEvent.click(screen.getByRole('button', { name: '清空搜索' }))

    // 只把值置空不够：列表要按渲染期的过滤回到全量。
    expect(search.value).toBe('')
    expect(rows()).toHaveLength(2)
    // 清空钮点完就被卸载，焦点得跟回输入框，否则接着打字会落空。
    expect(document.activeElement).toBe(search)
    expect(screen.queryByRole('button', { name: '清空搜索' })).not.toBeInTheDocument()
  })

  it('过滤后为空时说换关键词或厂商，不冒充「没有模型」', async () => {
    renderModels()
    await screen.findByRole('link', { name: /Claude Sonnet 4\.5/ })

    fireEvent.change(screen.getByLabelText('搜索名字、厂商或系列'), {
      target: { value: 'not-a-model' },
    })
    expect(screen.getByText('没有匹配的模型。换个关键词或厂商试试。')).toBeInTheDocument()
  })

  it('一条模型都没有时写清后果与下一步', async () => {
    mocks.models.mockResolvedValue({
      currency: 'USD',
      reference_at: '2026-10-21T09:00:00Z',
      items: [],
    })
    renderModels()

    expect(await screen.findByText('暂时没有可用模型')).toBeInTheDocument()
    expect(screen.getByText(/现在没有渠道在供任何模型/)).toBeInTheDocument()
    const action = screen.getByRole('link', { name: '查看快速开始' })
    expect(action).toHaveAttribute('href', '/app/quickstart')
  })

  it('每行都链到详情，显示名缺失时回落到规范名', async () => {
    renderModels()

    const claude = await screen.findByRole('link', { name: /Claude Sonnet 4\.5/ })
    expect(claude).toHaveAttribute('href', '/app/models/claude-sonnet-4-5')

    const gpt = screen.getByRole('link', { name: /gpt-4\.1/ })
    expect(gpt).toHaveAttribute('href', '/app/models/gpt-4.1')
    expect(gpt).toHaveTextContent('gpt-4.1')
  })

  it('规范名里的 ?、#、%、空格按段编码，斜杠留给详情路由', async () => {
    mocks.models.mockResolvedValue({
      currency: 'USD',
      reference_at: '2026-10-21T09:00:00Z',
      items: [{ ...ITEMS[1], name: 'acme/weird?name#1 %2', display_name: 'Weird' }],
    })
    renderModels()

    const row = await screen.findByRole('link', { name: /Weird/ })
    // 不编码的话 `?` 与 `#` 会把地址截成查询串或片段，点进去落到别的页。
    expect(row).toHaveAttribute('href', '/app/models/acme/weird%3Fname%231%20%252')
  })

  it('加载中对读屏有交代，加载完 status 与 busy 都消失', async () => {
    let resolveModels!: (value: unknown) => void
    mocks.models.mockReturnValue(
      new Promise((resolve) => {
        resolveModels = resolve
      }),
    )
    renderModels()

    expect(screen.getByRole('status')).toHaveTextContent('正在加载模型列表')
    const busy = document.querySelector('[aria-busy="true"]')
    expect(busy).not.toBeNull()
    // 骨架自己仍然对读屏隐藏，加载态靠上面那句与 busy 交代。
    expect(busy?.querySelector('.model-list')).toHaveAttribute('aria-hidden', 'true')

    resolveModels({ currency: 'USD', reference_at: '2026-10-21T09:00:00Z', items: ITEMS })
    await screen.findByRole('link', { name: /Claude Sonnet 4\.5/ })

    // 不是留一个空元素：两者都该从可及树上消失。
    expect(screen.queryByRole('status')).not.toBeInTheDocument()
    expect(document.querySelector('[aria-busy="true"]')).toBeNull()
  })

  it('5xx 说服务端暂时不可用，网络错误才让人查网络', async () => {
    const { ApiError } = await import('../api')

    mocks.models.mockRejectedValueOnce(
      new ApiError(503, 'unavailable', 'The console is temporarily unavailable.'),
    )
    const first = renderModels()
    expect(await screen.findByText(/控制台服务暂时不可用/)).toBeInTheDocument()
    expect(screen.queryByText(/检查网络/)).not.toBeInTheDocument()
    // 服务端那句英文不往界面上搬。
    expect(screen.queryByText(/The console is temporarily unavailable\./)).not.toBeInTheDocument()
    first.unmount()

    mocks.models.mockRejectedValueOnce(new Error('Failed to fetch'))
    renderModels()
    expect(await screen.findByText(/检查网络后重试/)).toBeInTheDocument()
    expect(screen.queryByText(/控制台服务暂时不可用/)).not.toBeInTheDocument()
  })

  it('筛选条独立成条，结果计数跟着筛选变化', async () => {
    renderModels()
    await screen.findByRole('link', { name: /Claude Sonnet 4\.5/ })

    const bar = document.querySelector('.model-filter-bar')
    expect(bar).not.toBeNull()
    expect(bar?.contains(screen.getByLabelText('搜索名字、厂商或系列'))).toBe(true)
    // 筛选行不再挂在页头的操作区里，否则它会跟着页面滚走。
    expect(document.querySelector('.page-actions')).toBeNull()

    // 数据到了才有计数：全量 2 条，筛到 1 条。
    expect(screen.getByText('找到 2 个模型')).toBeInTheDocument()
    fireEvent.change(screen.getByLabelText('搜索名字、厂商或系列'), {
      target: { value: 'gpt' },
    })
    expect(screen.getByText('找到 1 个模型')).toBeInTheDocument()
    expect(screen.queryByText('找到 2 个模型')).not.toBeInTheDocument()
  })

  it('筛选条的 sticky 吸附在不透明背景上', () => {
    const css = readFileSync('src/styles/components.css', 'utf8')
    const body = css.match(/\.model-filter-bar\s*\{([^}]*)\}/)?.[1] ?? ''
    const topbar = css.match(/\.app-topbar\s*\{([^}]*)\}/)?.[1] ?? ''
    const topbarHeight = topbar.match(/min-height:\s*(\d+)px/)?.[1]
    expect(topbarHeight).toBeDefined()

    expect(body).toMatch(/position:\s*sticky/)
    // 吸附位置跟着顶栏高度走：顶栏改了高度，这里不会悄悄错位。
    expect(body).toMatch(new RegExp(`top:\\s*${topbarHeight}px`))
    // 不透明：用令牌变量的实色。列表从它底下滚过，transparent 或 rgba() 都会透出来。
    expect(body).toMatch(/background:\s*var\(--surface\)/)
    expect(body).not.toMatch(/rgba?\(/)
  })

  it('没填的厂商与上下文在列表里写「未填」，不整段省略', async () => {
    mocks.models.mockResolvedValue({
      currency: 'USD',
      reference_at: '2026-10-21T09:00:00Z',
      items: [
        {
          ...ITEMS[1],
          name: 'bare-model',
          display_name: 'Bare Model',
          vendor: '',
          family: '',
          context_limit: 0,
        },
      ],
    })
    renderModels()

    const row = await screen.findByRole('link', { name: /Bare Model/ })
    const meta = row.querySelector('.model-meta') as HTMLElement
    // 厂商一段与上下文一段是两个格子，各自断言：只断「页面上有一处未填」会漏掉另一处。
    const cells = Array.from(meta.children).map((cell) => cell.textContent)
    expect(cells[0]).toBe('未填')
    expect(cells[1]).toBe('上下文 未填')
  })
})
