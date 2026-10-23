import { fireEvent, render, screen, within } from '@testing-library/react'
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

const META = {
  site_name: 'larkax',
  api_base_url: 'https://api.example.com/v1',
  currency: 'USD',
  timezone: 'UTC',
  locales: ['en', 'zh'],
  features: { registration: false, password_reset: 'admin', topup: false },
}

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
  never_expires: true,
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
    mocks.meta.mockResolvedValue(META)
    mocks.listKeys.mockResolvedValue({ items: [], next_cursor: null })
  })

  it('没有密钥时给空态和下一步动作', async () => {
    renderKeys()

    expect(await screen.findByText('还没有 API 密钥')).toBeInTheDocument()
    expect(screen.getByText(/创建第一把密钥，就能用它调用 \/v1 接口/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '创建第一把密钥' })).toBeInTheDocument()
  })

  // 详情只读 never_expires：下面第一条的 expires_at 是夹具，用来证明页面不看这个值。
  it('never_expires 为真时显示「不过期」，远期时刻不进页面', async () => {
    mocks.listKeys.mockResolvedValue({
      items: [{ ...KEY, expires_at: '9999-12-31T23:59:59Z', never_expires: true }],
      next_cursor: null,
    })
    const user = userEvent.setup()
    renderKeys()

    await user.click(await screen.findByRole('button', { name: '打开 Production 的操作' }))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText('不过期')).toBeInTheDocument()
    expect(within(dialog).queryByText(/9999-12-31/)).not.toBeInTheDocument()
  })

  it('never_expires 为假时按时刻显示', async () => {
    mocks.listKeys.mockResolvedValue({
      items: [{ ...KEY, expires_at: '2027-03-04T05:06:00Z', never_expires: false }],
      next_cursor: null,
    })
    const user = userEvent.setup()
    renderKeys()

    await user.click(await screen.findByRole('button', { name: '打开 Production 的操作' }))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText('2027-03-04 05:06')).toBeInTheDocument()
    expect(within(dialog).queryByText('不过期')).not.toBeInTheDocument()
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

  it('额度输入框前的货币符号跟着站点币种走', async () => {
    const user = userEvent.setup()
    mocks.meta.mockResolvedValueOnce({ ...META, currency: 'CNY' })
    const first = renderKeys()

    await user.click(await screen.findByRole('button', { name: '创建第一把密钥' }))
    const cnyDialog = await screen.findByRole('dialog')
    expect(await within(cnyDialog).findByText('¥')).toBeInTheDocument()
    expect(within(cnyDialog).queryByText('$')).not.toBeInTheDocument()
    first.unmount()

    mocks.meta.mockResolvedValueOnce(META)
    renderKeys()
    await user.click(await screen.findByRole('button', { name: '创建第一把密钥' }))
    const usdDialog = await screen.findByRole('dialog')
    expect(await within(usdDialog).findByText('$')).toBeInTheDocument()
    expect(within(usdDialog).queryByText('¥')).not.toBeInTheDocument()
  })

  it('额度与速率手输的值照旧提交', async () => {
    const user = userEvent.setup()
    mocks.createKey.mockResolvedValue({ key: { ...KEY }, secret: 'sk-' + 'c'.repeat(32) })
    renderKeys()

    await user.click(await screen.findByRole('button', { name: '创建第一把密钥' }))
    const dialog = await screen.findByRole('dialog')
    await user.type(within(dialog).getByLabelText('额度上限'), '12.5')
    await user.type(within(dialog).getByLabelText('速率上限'), '42')
    await user.click(within(dialog).getByRole('button', { name: '创建密钥' }))

    expect(await screen.findByText('密钥已创建')).toBeInTheDocument()
    expect(mocks.createKey).toHaveBeenLastCalledWith({ quota_limit: '12.5', rate_limit_rpm: 42 })
  })

  it('速率档位写进同一个输入框，自由输入不被锁死', async () => {
    const user = userEvent.setup()
    renderKeys()

    await user.click(await screen.findByRole('button', { name: '创建第一把密钥' }))
    const dialog = await screen.findByRole('dialog')
    const rate = within(dialog).getByLabelText('速率上限')
    await user.click(within(dialog).getByRole('button', { name: '300 次/分' }))
    expect(rate).toHaveValue('300')
    expect(within(dialog).getByRole('button', { name: '300 次/分' })).toHaveAttribute(
      'aria-pressed',
      'true',
    )

    await user.clear(rate)
    await user.type(rate, '42')
    expect(rate).toHaveValue('42')
    expect(within(dialog).getByRole('button', { name: '300 次/分' })).toHaveAttribute(
      'aria-pressed',
      'false',
    )
  })

  // 快捷档位算的是「现在 + N 天」，把 Date 冻住，期望值才不会随跑测试的墙上时间漂。
  const BASE_TIME = new Date('2026-10-23T10:15:00Z')

  function localInput(date: Date): string {
    const pad = (value: number) => String(value).padStart(2, '0')
    return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`
  }

  function shiftedBy(apply: (date: Date) => void): string {
    const next = new Date(BASE_TIME)
    apply(next)
    return localInput(next)
  }

  const EXPIRY_CASES: [label: string, manual: string][] = [
    ['7 天', shiftedBy((date) => date.setDate(date.getDate() + 7))],
    ['30 天', shiftedBy((date) => date.setDate(date.getDate() + 30))],
    ['90 天', shiftedBy((date) => date.setDate(date.getDate() + 90))],
    ['1 年', shiftedBy((date) => date.setFullYear(date.getFullYear() + 1))],
  ]

  // 只冻结 Date：setTimeout 仍是真表，RTL 与 userEvent 的等待不用跟着造假。
  async function withFrozenDate(run: () => Promise<void>) {
    vi.useFakeTimers({ toFake: ['Date'] })
    vi.setSystemTime(BASE_TIME)
    try {
      await run()
    } finally {
      vi.useRealTimers()
    }
  }

  it.each(EXPIRY_CASES)(
    '过期快捷档位「%s」与手输同一时刻提交的请求体逐字节一致',
    async (label, manual) => {
      mocks.createKey.mockResolvedValue({ key: { ...KEY }, secret: 'sk-' + 'd'.repeat(32) })
      const user = userEvent.setup()

      await withFrozenDate(async () => {
        const preset = renderKeys()
        await user.click(await screen.findByRole('button', { name: '创建第一把密钥' }))
        const presetDialog = await screen.findByRole('dialog')
        await user.click(within(presetDialog).getByRole('button', { name: label }))
        expect(within(presetDialog).getByLabelText('过期时间')).toHaveValue(manual)
        await user.click(within(presetDialog).getByRole('button', { name: '创建密钥' }))
        expect(await screen.findByText('密钥已创建')).toBeInTheDocument()
        const fromPreset = mocks.createKey.mock.calls.at(-1)?.[0]
        preset.unmount()

        const typed = renderKeys()
        await user.click(await screen.findByRole('button', { name: '创建第一把密钥' }))
        const typedDialog = await screen.findByRole('dialog')
        fireEvent.change(within(typedDialog).getByLabelText('过期时间'), {
          target: { value: manual },
        })
        await user.click(within(typedDialog).getByRole('button', { name: '创建密钥' }))
        expect(await screen.findByText('密钥已创建')).toBeInTheDocument()
        const fromManual = mocks.createKey.mock.calls.at(-1)?.[0]
        typed.unmount()

        expect(fromPreset).toEqual(fromManual)
        expect(fromPreset?.expires_at).toBe(new Date(manual).toISOString())
      })
    },
  )

  it('「永不过期」清空字段，提交时不带 expires_at', async () => {
    mocks.createKey.mockResolvedValue({ key: { ...KEY }, secret: 'sk-' + 'e'.repeat(32) })
    const user = userEvent.setup()

    await withFrozenDate(async () => {
      renderKeys()
      await user.click(await screen.findByRole('button', { name: '创建第一把密钥' }))
      const dialog = await screen.findByRole('dialog')

      await user.click(within(dialog).getByRole('button', { name: '7 天' }))
      expect(within(dialog).getByLabelText('过期时间')).not.toHaveValue('')

      await user.click(within(dialog).getByRole('button', { name: '永不过期' }))
      expect(within(dialog).getByLabelText('过期时间')).toHaveValue('')
      await user.click(within(dialog).getByRole('button', { name: '创建密钥' }))
      expect(await screen.findByText('密钥已创建')).toBeInTheDocument()
      expect(mocks.createKey.mock.calls.at(-1)?.[0]).toEqual({})
    })
  })

  /* ── 离开确认 ─────────────────────────────────────────────────── */

  const createDialog = () => screen.getByRole('dialog', { name: '创建密钥' })
  const discardDialog = () => screen.queryByRole('dialog', { name: '放弃未保存的修改？' })

  /** 创建密钥弹窗的三个关闭出口：底部取消钮、Esc、点遮罩。 */
  type Exit = '取消' | 'Esc' | '遮罩'

  function closeCreate(exit: Exit) {
    const dialog = createDialog()
    if (exit === '取消') fireEvent.click(within(dialog).getByRole('button', { name: '取消' }))
    else if (exit === 'Esc') fireEvent.keyDown(document, { key: 'Escape' })
    else fireEvent.mouseDown(dialog.parentElement as Element)
  }

  async function openFilledForm(user: ReturnType<typeof userEvent.setup>) {
    await user.click(await screen.findByRole('button', { name: '创建第一把密钥' }))
    const dialog = await screen.findByRole('dialog', { name: '创建密钥' })
    await user.type(within(dialog).getByLabelText('名称'), 'CI')
    return dialog
  }

  function discard() {
    return within(screen.getByRole('dialog', { name: '放弃未保存的修改？' }))
  }

  it.each(['取消', 'Esc', '遮罩'] as const)(
    '填过内容后从「%s」关闭，先问一句；选「留下」时弹窗与输入都还在',
    async (exit) => {
      const user = userEvent.setup()
      renderKeys()
      await openFilledForm(user)

      closeCreate(exit)
      // 「留下」就是取消这一问，不是放弃。
      fireEvent.click(discard().getByRole('button', { name: '取消' }))
      expect(discardDialog()).not.toBeInTheDocument()
      expect(createDialog()).toBeInTheDocument()
      expect(screen.getByLabelText('名称')).toHaveValue('CI')

      // 同一个出口再走一次，这次点「放弃修改」才真的收起表单。
      closeCreate(exit)
      fireEvent.click(discard().getByRole('button', { name: '放弃修改' }))
      expect(screen.queryByRole('dialog', { name: '创建密钥' })).not.toBeInTheDocument()
    },
  )

  it('表单没填过任何东西时关闭不问', async () => {
    const user = userEvent.setup()
    renderKeys()
    await user.click(await screen.findByRole('button', { name: '创建第一把密钥' }))
    await screen.findByRole('dialog', { name: '创建密钥' })

    fireEvent.click(within(createDialog()).getByRole('button', { name: '取消' }))
    expect(discardDialog()).not.toBeInTheDocument()
    expect(screen.queryByRole('dialog', { name: '创建密钥' })).not.toBeInTheDocument()
  })

  it('提交成功后的关闭不是放弃，不再问一句', async () => {
    mocks.createKey.mockResolvedValue({ key: { ...KEY }, secret: 'sk-' + 'f'.repeat(32) })
    const user = userEvent.setup()
    renderKeys()
    await openFilledForm(user)

    await user.click(within(createDialog()).getByRole('button', { name: '创建密钥' }))
    expect(await screen.findByText('密钥已创建')).toBeInTheDocument()
    expect(discardDialog()).not.toBeInTheDocument()
    expect(screen.queryByRole('dialog', { name: '创建密钥' })).not.toBeInTheDocument()
  })

  it('放弃关窗之后这一页不再算改过：刷新与再打开都不拦', async () => {
    const user = userEvent.setup()
    renderKeys()
    await openFilledForm(user)
    closeCreate('取消')
    fireEvent.click(discard().getByRole('button', { name: '放弃修改' }))
    expect(screen.queryByRole('dialog', { name: '创建密钥' })).not.toBeInTheDocument()

    // 表单内容还留在内存里，但弹窗已经收起——脏判据里少了「弹窗开着」这一条的话，
    // 这里仍会拦下刷新。
    const cleanEvent = new Event('beforeunload', { cancelable: true })
    window.dispatchEvent(cleanEvent)
    expect(cleanEvent.defaultPrevented).toBe(false)

    // 换个入口再打开、直接关：还是不问。
    fireEvent.click(await screen.findByRole('button', { name: '创建第一把密钥' }))
    fireEvent.click(within(createDialog()).getByRole('button', { name: '取消' }))
    expect(discardDialog()).not.toBeInTheDocument()
    expect(screen.queryByRole('dialog', { name: '创建密钥' })).not.toBeInTheDocument()
  })

  // `expiryPreset` 本身不进脏判据：写时刻的档位会同时改输入框，差额落在 form 里。
  it('点了写时刻的过期档位算改过，关闭要问一句', async () => {
    const user = userEvent.setup()
    renderKeys()
    await user.click(await screen.findByRole('button', { name: '创建第一把密钥' }))
    const dialog = await screen.findByRole('dialog', { name: '创建密钥' })

    await user.click(within(dialog).getByRole('button', { name: '7 天' }))
    fireEvent.click(within(dialog).getByRole('button', { name: '取消' }))
    expect(discardDialog()).toBeInTheDocument()
  })

  it('只点「永不过期」不算改过：字段本来就空，关窗不问', async () => {
    const user = userEvent.setup()
    renderKeys()
    await user.click(await screen.findByRole('button', { name: '创建第一把密钥' }))
    const dialog = await screen.findByRole('dialog', { name: '创建密钥' })

    await user.click(within(dialog).getByRole('button', { name: '永不过期' }))
    fireEvent.click(within(dialog).getByRole('button', { name: '取消' }))
    expect(discardDialog()).not.toBeInTheDocument()
    expect(screen.queryByRole('dialog', { name: '创建密钥' })).not.toBeInTheDocument()
  })
})
