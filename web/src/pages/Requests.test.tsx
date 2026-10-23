import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen, within } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { UsageRequest } from '../api/types'
import { I18nProvider } from '../i18n'
import { SiteProvider } from '../site/SiteProvider'
import { Requests } from './Requests'

const mocks = vi.hoisted(() => ({
  usageRequests: vi.fn(),
  listKeys: vi.fn(),
  models: vi.fn(),
  meta: vi.fn(),
}))

vi.mock('../api', () => ({ api: mocks }))

const META = {
  site_name: 'larkax',
  api_base_url: 'https://api.example.com/v1',
  currency: 'USD',
  timezone: 'UTC',
  locales: ['zh', 'en'],
  features: { registration: false, password_reset: 'admin', topup: false },
}

function usageRequest(patch: Partial<UsageRequest>): UsageRequest {
  return {
    id: 1,
    request_id: 'req_default',
    model: 'gpt-4.1',
    key: { id: 1, name: 'prod' },
    provider: 'primary',
    status: 'settled',
    cost: '0.01000000',
    currency: 'USD',
    tokens: { input: 0, output: 0, cache_read: 0, cache_write: 0, reasoning: 0 },
    billing_path: 'measured',
    streamed: true,
    protocol: 'openai',
    duration_ms: 2000,
    tool_result_tail: false,
    first_token_ms: 200,
    user_agent: 'cli/1.0',
    started_at: '2026-10-20T08:00:00Z',
    settled_at: '2026-10-20T08:00:03Z',
    ...patch,
  }
}

const ROWS: UsageRequest[] = [
  usageRequest({
    id: 1,
    request_id: 'req_alpha',
    tool_result_tail: true,
    duration_ms: 3000,
    first_token_ms: 250,
    tokens: { input: 1234, output: 800, cache_read: 500, cache_write: 0, reasoning: 200 },
  }),
  usageRequest({ id: 2, request_id: 'req_beta', model: 'claude-sonnet-4-5', provider: 'backup' }),
]

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <I18nProvider>
      <QueryClientProvider client={client}>
        <MemoryRouter>
          <SiteProvider>
            <Requests />
          </SiteProvider>
        </MemoryRouter>
      </QueryClientProvider>
    </I18nProvider>,
  )
}

/** 一次请求占一行，行根上有 `data-request-id`，断言据此归到具体那一行。 */
async function rowOf(requestId: string): Promise<HTMLElement> {
  const cell = await screen.findByText(requestId)
  const row = cell.closest('[data-request-id]')
  if (!(row instanceof HTMLElement)) throw new Error(`没找到 ${requestId} 那一行`)
  return row
}

beforeEach(() => {
  // jsdom 的 navigator 是英文环境，语言偏好写进 localStorage 才拿得到中文文案。
  localStorage.setItem('larkax.lang', 'zh')
  mocks.usageRequests.mockResolvedValue({ items: ROWS, page: 1, limit: 20, count: 45 })
  mocks.listKeys.mockResolvedValue({ items: [], next_cursor: null })
  mocks.models.mockResolvedValue({ currency: 'USD', reference_at: '', items: [] })
  mocks.meta.mockResolvedValue(META)
})

describe('请求日志页', () => {
  it('工具回填标记只出现在 tool_result 那一行，TTFT / TPS 取接口给的字段', async () => {
    renderPage()

    const alpha = await rowOf('req_alpha')
    expect(within(alpha).getByText('工具回填')).toBeInTheDocument()
    // TTFT 直接显示接口给的毫秒数；TPS = (800 + 200) ÷ (3000 − 250) ms。
    expect(within(alpha).getByText('250 ms')).toBeInTheDocument()
    expect(within(alpha).getByText('363.6')).toBeInTheDocument()
    // 输入 / 缓存在同一列的两行，紧凑单位逐字对上。
    expect(within(alpha).getByText('1,234')).toBeInTheDocument()
    expect(within(alpha).getByText('500')).toBeInTheDocument()

    const beta = await rowOf('req_beta')
    expect(within(beta).queryByText('工具回填')).toBeNull()
    // 没有产出时 TPS 是占位符，不拿总耗时凑一个数出来。
    expect(within(beta).getByText('—')).toBeInTheDocument()

    // 底栏说明按本页实际行数与接口给的总数走。
    const pager = screen.getByRole('navigation', { name: '翻页' })
    expect(within(pager).getByText('本页 2 条')).toBeInTheDocument()
  })

  it('接口失败时走错误态并给重试按钮，不白屏', async () => {
    mocks.usageRequests.mockRejectedValue(new Error('boom'))

    renderPage()

    expect(await screen.findByText('请求日志加载失败。')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '重试' })).toBeInTheDocument()
  })
})
