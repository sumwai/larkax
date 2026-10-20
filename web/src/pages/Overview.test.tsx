import { render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { MemoryRouter } from 'react-router-dom'
import { Overview } from './Overview'
import { I18nProvider } from '../i18n'
import { SiteProvider } from '../site/SiteProvider'

const mocks = vi.hoisted(() => ({
  wallet: vi.fn(),
  walletEntries: vi.fn(),
  usageSummary: vi.fn(),
  usageSeries: vi.fn(),
  usageRequests: vi.fn(),
  meta: vi.fn(),
}))

vi.mock('../api', () => ({
  api: {
    mode: 'http',
    wallet: mocks.wallet,
    walletEntries: mocks.walletEntries,
    usageSummary: mocks.usageSummary,
    usageSeries: mocks.usageSeries,
    usageRequests: mocks.usageRequests,
    meta: mocks.meta,
  },
}))

const SUMMARY = {
  from: '2026-10-21T00:00:00+08:00',
  to: '2026-10-22T00:00:00+08:00',
  currency: 'USD',
  totals: {
    cost: '6.61000000',
    requests: 1234,
    tokens: { input: 213_800, output: 112_900, cache_read: 0, cache_write: 0, reasoning: 0 },
  },
  billing_paths: [],
}

function renderOverview() {
  return render(
    <I18nProvider>
      <SiteProvider>
        <MemoryRouter>
          <Overview />
        </MemoryRouter>
      </SiteProvider>
    </I18nProvider>,
  )
}

describe('Overview', () => {
  beforeEach(() => {
    mocks.meta.mockResolvedValue({
      site_name: 'larkax',
      api_base_url: 'https://api.example.com/v1',
      currency: 'USD',
      timezone: 'Asia/Shanghai',
      locales: ['en', 'zh'],
      features: { registration: false, password_reset: 'admin', topup: false },
    })
    mocks.wallet.mockResolvedValue({
      asset: 'USD',
      balance: '128.45670000',
      frozen: '1.20000000',
      updated_at: '2026-10-21T00:00:00Z',
    })
    mocks.walletEntries.mockResolvedValue({ items: [], next_cursor: null })
    mocks.usageSummary.mockResolvedValue(SUMMARY)
    mocks.usageSeries.mockResolvedValue({ interval: 'day', tz: 'Asia/Shanghai', points: [] })
    mocks.usageRequests.mockResolvedValue({ items: [], next_cursor: null })
  })

  it('余额加载失败时，今日数字照常渲染，失败的那块可以重试', async () => {
    mocks.wallet.mockRejectedValue(new Error('503'))
    renderOverview()

    expect(await screen.findByText('1,234')).toBeInTheDocument()
    expect(await screen.findByText('余额加载失败。其余内容不受影响。')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '重试' })).toBeInTheDocument()
    expect(screen.queryByText('128.46')).not.toBeInTheDocument()
  })
})
