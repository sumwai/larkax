import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { ModelDetail } from './ModelDetail'
import { I18nProvider } from '../i18n'
import type { ModelDetailResponse } from '../api'

const mocks = vi.hoisted(() => ({ modelDetail: vi.fn() }))

vi.mock('../api', async () => {
  const actual = await vi.importActual<typeof import('../api')>('../api')
  return { ...actual, api: { modelDetail: mocks.modelDetail } }
})

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
    reasoning_options: [{ type: 'toggle' }],
    canonical_model_id: 'model-x-2026',
    knowledge_cutoff: '2026-01-01',
    release_date: null,
    updated_at: '2026-10-19T09:00:00Z',
    // 服务端已按价格排好序，前端不再重排：平台自营排在第二行。
    suppliers: [
      {
        merchant: 'acme',
        price: '0.00350000',
        prices: { prompt: '0.00350000', completion: '0.01050000' },
        channel_count: 2,
      },
      {
        merchant: null,
        price: '0.00000000',
        prices: { completion: '0.01500000' },
        channel_count: 1,
      },
    ],
    unpriced_channels: 3,
  },
}

function renderDetail(path = '/app/models/vendor/model-x') {
  return render(
    <I18nProvider>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/app/models/*" element={<ModelDetail />} />
        </Routes>
      </MemoryRouter>
    </I18nProvider>,
  )
}

describe('模型详情', () => {
  beforeEach(() => {
    mocks.modelDetail.mockReset()
    mocks.modelDetail.mockResolvedValue(DETAIL)
  })

  it('按 splat 取带斜杠的规范名，原样交给接口', async () => {
    renderDetail('/app/models/vendor/model-x')

    expect(await screen.findByRole('heading', { name: 'Model X' })).toBeInTheDocument()
    expect(mocks.modelDetail).toHaveBeenCalledWith('vendor/model-x')
    // 面包屑回到清单。
    expect(screen.getByRole('link', { name: /模型/ })).toHaveAttribute('href', '/app/models')
  })

  it('六项能力按三态区分，没采集的不冒充不支持', async () => {
    renderDetail()
    await screen.findByRole('heading', { name: 'Model X' })

    expect(screen.getByText('推理').closest('li')).toHaveTextContent('支持')
    expect(screen.getByText('工具调用').closest('li')).toHaveTextContent('不支持')
    // `null` 是没采集过，与明确不支持的 `false` 分开显示。
    expect(screen.getByText('视觉').closest('li')).toHaveTextContent('未采集')
    expect(screen.getByText('音频').closest('li')).toHaveTextContent('未采集')

    // 端点非空时逐条列出。
    expect(screen.getByText('chat')).toBeInTheDocument()
    expect(screen.getByText('messages')).toBeInTheDocument()
  })

  it('供应商表保留服务端顺序，平台自营与各维度原样显示', async () => {
    renderDetail()
    await screen.findByRole('heading', { name: 'Model X' })

    const rows = screen.getAllByRole('row').slice(1) // 去掉表头
    expect(rows).toHaveLength(2)

    // 第一行是商家，两个渠道；输入与输出各按维度价显示。
    expect(rows[0]).toHaveTextContent('acme')
    expect(rows[0]).toHaveTextContent('2')
    expect(rows[0]).toHaveTextContent('$0.0035')
    expect(rows[0]).toHaveTextContent('$0.0105')

    // 第二行是平台自营。`price` 是 0 但 `prices` 里没有 `prompt`：输入那一格
    // 是「没写」，不能拿 `price` 填成免费。
    expect(rows[1]).toHaveTextContent('平台自营')
    const cells = Array.from(rows[1].querySelectorAll('td')).map((cell) => cell.textContent)
    expect(cells).toEqual(['平台自营', '1', '—', '$0.0150'])
    expect(screen.queryByText(/0\.0000/)).not.toBeInTheDocument()

    // 只给规则声明过的维度出列：缓存与推理没有声明，就不出现。
    expect(screen.queryByRole('columnheader', { name: '缓存读取' })).not.toBeInTheDocument()

    // 算不出价的渠道由模型级计数回答。
    expect(screen.getByText('另有 3 个在供渠道算不出价，没有列入上表。')).toBeInTheDocument()
    // 目录里为 0 的上限写「未填」，不写成 0。
    expect(screen.getByText('输入上限').closest('div')).toHaveTextContent('未填')
  })

  it('一条报价都没有时说明原因，不画一张空表', async () => {
    mocks.modelDetail.mockResolvedValue({
      ...DETAIL,
      model: { ...DETAIL.model, suppliers: undefined, unpriced_channels: 1 },
    })
    renderDetail()
    await screen.findByRole('heading', { name: 'Model X' })

    expect(screen.getByText('还没有报价')).toBeInTheDocument()
    expect(screen.getByText(/都没有适用的计价规则/)).toBeInTheDocument()
    expect(screen.queryByRole('table')).not.toBeInTheDocument()
  })

  it('404 只给一句本地化的话，重试会重新请求', async () => {
    const { ApiError } = await import('../api')
    mocks.modelDetail.mockRejectedValue(new ApiError(404, 'model_not_found', 'Model not found.'))
    renderDetail()

    expect(
      await screen.findByText('找不到这个模型：名字可能写错了，也可能已经没有渠道在供它。'),
    ).toBeInTheDocument()
    // 不把服务端那句英文原样搬到页面上。
    expect(screen.queryByText('Model not found.')).not.toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: '重试' }))
    await waitFor(() => expect(mocks.modelDetail).toHaveBeenCalledTimes(2))
  })
})
