import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { Keys } from './Keys'
import { I18nProvider } from '../i18n'
import { ThemeProvider } from '../theme/ThemeProvider'
import { SiteProvider } from '../site/SiteProvider'
import { ToastProvider } from '../components/ui'
import type { ApiKey } from '../api/types'

const mocks = vi.hoisted(() => ({
  listKeys: vi.fn(),
  createKey: vi.fn(),
  updateKey: vi.fn(),
  revokeKey: vi.fn(),
  meta: vi.fn(),
}))

vi.mock('../api', () => ({
  api: {
    mode: 'http',
    listKeys: mocks.listKeys,
    createKey: mocks.createKey,
    updateKey: mocks.updateKey,
    revokeKey: mocks.revokeKey,
    meta: mocks.meta,
  },
}))

const KEY: ApiKey = {
  id: 1,
  name: 'Production',
  prefix: 'sk-a1b2c3d4',
  suffix: 'x9Yz2K',
  masked: 'sk-a1b2c3d4******************x9Yz2K',
  status: 'active',
  quota_limit: null,
  quota_used: '42.11840000',
  rate_limit_rpm: 600,
  expires_at: null,
  created_at: '2026-09-01T00:00:00Z',
  last_used_at: '2026-10-21T00:00:00Z',
}

function renderKeys() {
  return render(
    <I18nProvider>
      <ThemeProvider>
        <ToastProvider>
          <SiteProvider>
            <Keys />
          </SiteProvider>
        </ToastProvider>
      </ThemeProvider>
    </I18nProvider>,
  )
}

describe('Keys', () => {
  beforeEach(() => {
    mocks.meta.mockResolvedValue({
      site_name: 'larkax',
      api_base_url: 'https://api.example.com/v1',
      currency: 'USD',
      timezone: 'UTC',
      locales: ['en', 'zh'],
      features: { registration: false, password_reset: 'admin', topup: false },
    })
    mocks.listKeys.mockResolvedValue({ items: [], next_cursor: null })
  })

  it('没有密钥时给空态和下一步动作', async () => {
    renderKeys()

    expect(await screen.findByText('还没有 API 密钥')).toBeInTheDocument()
    expect(screen.getByText(/创建第一把密钥，就能用它调用 \/v1 接口/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '创建第一把密钥' })).toBeInTheDocument()
  })

  it('有密钥时列表显示脱敏串、累计花费与状态', async () => {
    mocks.listKeys.mockResolvedValue({ items: [KEY], next_cursor: null })
    renderKeys()

    expect((await screen.findAllByText(KEY.masked)).length).toBeGreaterThan(0)
    expect(screen.getAllByText('Production').length).toBeGreaterThan(0)
    expect(screen.getAllByText('运行中').length).toBeGreaterThan(0)
    expect(screen.getAllByText(/累计/).length).toBeGreaterThan(0)
    expect(screen.queryByText('还没有 API 密钥')).not.toBeInTheDocument()
  })

  it('创建后弹窗只显示一次完整密钥', async () => {
    mocks.createKey.mockResolvedValue({ key: { ...KEY }, secret: 'sk-' + 'a'.repeat(32) })
    const user = userEvent.setup()
    renderKeys()

    await user.click(await screen.findByRole('button', { name: '创建第一把密钥' }))
    const dialog = await screen.findByRole('dialog')
    await user.type(within(dialog).getByLabelText('名称'), 'CI')
    await user.click(within(dialog).getByRole('button', { name: '创建密钥' }))

    expect(await screen.findByText('密钥已创建')).toBeInTheDocument()
    expect(screen.getByText('sk-' + 'a'.repeat(32))).toBeInTheDocument()
    // 额度与速率留空时整个字段都不出现：服务端拒绝 quota_limit: null。
    expect(mocks.createKey).toHaveBeenCalledWith({ name: 'CI' })
  })
})
