import { fireEvent, render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { MemoryRouter } from 'react-router-dom'
import { Models } from './Models'
import { I18nProvider } from '../i18n'
import type { Model } from '../api'

const mocks = vi.hoisted(() => ({ models: vi.fn() }))

vi.mock('../api', () => ({
  api: { models: mocks.models },
}))

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
})
