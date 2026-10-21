import { fireEvent, render, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { I18nProvider } from '../../i18n'
import { SiteProvider } from '../../site/SiteProvider'
import { ToastProvider } from '../../components/ui'
import { OpsAliases } from './Aliases'

const mocks = vi.hoisted(() => ({
  listAliases: vi.fn(),
  listChannels: vi.fn(),
  listModels: vi.fn(),
  createAlias: vi.fn(),
  updateAlias: vi.fn(),
  deleteAlias: vi.fn(),
  meta: vi.fn(),
}))

vi.mock('../../api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../api')>()
  const ops = {
    listAliases: mocks.listAliases,
    listChannels: mocks.listChannels,
    listModels: mocks.listModels,
    createAlias: mocks.createAlias,
    updateAlias: mocks.updateAlias,
    deleteAlias: mocks.deleteAlias,
  }
  return {
    ...actual,
    api: {
      admin: ops,
      meta: mocks.meta,
      session: vi.fn().mockResolvedValue(null),
    },
  }
})

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

describe('别名页', () => {
  beforeEach(() => {
    Object.values(mocks).forEach((mock) => mock.mockReset())
    mocks.listAliases.mockResolvedValue([])
    mocks.listChannels.mockResolvedValue([CHANNEL])
    mocks.listModels.mockResolvedValue([])
    mocks.meta.mockResolvedValue({
      site_name: 'larkax',
      api_base_url: 'https://api.example.com/v1',
      currency: 'USD',
      timezone: 'Asia/Shanghai',
      locales: ['en', 'zh'],
      features: { registration: false, password_reset: 'admin', topup: false },
    })
  })

  it('渠道是选出来的：下拉里能看到渠道名与 ID', async () => {
    render(
      <I18nProvider>
        <ToastProvider>
          <SiteProvider>
            <OpsAliases />
          </SiteProvider>
        </ToastProvider>
      </I18nProvider>,
    )

    const [create] = await screen.findAllByRole('button', { name: '新建别名' })
    fireEvent.click(create)

    const field = screen.getByLabelText('渠道') as HTMLSelectElement
    expect(field.tagName).toBe('SELECT')
    expect(field.value).toBe('7')
    expect(screen.getByRole('option', { name: 'platform-anthropic (#7)' })).toBeInTheDocument()
  })
})
