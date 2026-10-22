import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { I18nProvider } from '../../i18n'
import { SiteProvider } from '../../site/SiteProvider'
import { ToastProvider } from '../../components/ui'
import { OpsChannels } from './Channels'
import type { ChannelProbeResult } from '../../api/types'
import { mockViewport } from '../../test/media'

const mocks = vi.hoisted(() => ({
  listChannels: vi.fn(),
  listModels: vi.fn(),
  createChannel: vi.fn(),
  updateChannel: vi.fn(),
  getChannel: vi.fn(),
  listAdapters: vi.fn(),
  probeModels: vi.fn(),
  meta: vi.fn(),
}))

vi.mock('../../api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../api')>()
  const ops = {
    listChannels: mocks.listChannels,
    listModels: mocks.listModels,
    createChannel: mocks.createChannel,
    updateChannel: mocks.updateChannel,
    getChannel: mocks.getChannel,
    listAdapters: mocks.listAdapters,
    probeModels: mocks.probeModels,
  }
  return {
    ...actual,
    api: {
      admin: ops,
      merchant: ops,
      meta: mocks.meta,
      session: vi.fn().mockResolvedValue(null),
    },
  }
})

const { ApiError } = await import('../../api')

/** 与后端注册表同一组类型与端点（顺序按类型升序）。 */
const ADAPTERS = [
  { type: 'anthropic', protocol: 'anthropic', endpoint: '/v1/messages' },
  { type: 'openai', protocol: 'openai', endpoint: '/v1/chat/completions' },
  { type: 'responses', protocol: 'responses', endpoint: '/v1/responses' },
  { type: 'systemone', protocol: 'systemone', endpoint: '/v1/systemone' },
]

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
  credential_keys: ['k-placeholder'],
  credential_hint: 'abcd',
  credential_error: null,
  deleted_at: null,
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-02T00:00:00Z',
}

function renderChannels(scope: 'admin' | 'merchant' = 'admin') {
  return render(
    <I18nProvider>
      <ToastProvider>
        <SiteProvider>
          <OpsChannels scope={scope} />
        </SiteProvider>
      </ToastProvider>
    </I18nProvider>,
  )
}

/** 页头与空态里各有一个「新建渠道」，点第一个即可。 */
async function clickCreate() {
  const [first] = await screen.findAllByRole('button', { name: '新建渠道' })
  fireEvent.click(first)
  return document.getElementById('channel-form') as HTMLFormElement
}

function fillValidChannel() {
  fireEvent.change(screen.getByLabelText('名称'), { target: { value: 'dup' } })
  fireEvent.change(screen.getByLabelText('上游地址'), {
    target: { value: 'https://api.example.com/v1' },
  })
  fireEvent.change(screen.getByLabelText('凭证'), { target: { value: 'placeholder' } })
  fireEvent.change(screen.getByLabelText('支持的模型（每行一个）'), {
    target: { value: 'gpt-4.1' },
  })
}

/**
 * 每个用例前的默认桩：先清空所有 mock，再补上两份默认数据。
 *
 * 两个 describe 共用这一份，省得新加的默认桩只落进其中一个。
 */
function resetMocks() {
  Object.values(mocks).forEach((mock) => mock.mockReset())
  mocks.listModels.mockResolvedValue([])
  mocks.listAdapters.mockResolvedValue(ADAPTERS)
  mocks.meta.mockResolvedValue({
    site_name: 'larkax',
    api_base_url: 'https://api.example.com/v1',
    currency: 'USD',
    timezone: 'Asia/Shanghai',
    locales: ['en', 'zh'],
    features: { registration: false, password_reset: 'admin', topup: false },
  })
}

describe('运营渠道页', () => {
  beforeEach(resetMocks)

  it('空列表显示空态与新建入口', async () => {
    mocks.listChannels.mockResolvedValue([])
    renderChannels()
    expect(await screen.findByText('还没有渠道')).toBeInTheDocument()
    expect(screen.getAllByRole('button', { name: '新建渠道' }).length).toBeGreaterThan(0)
  })

  it('加载中先显示骨架屏，不显示空态', async () => {
    let release: (value: unknown[]) => void = () => {}
    mocks.listChannels.mockReturnValue(new Promise((resolve) => (release = resolve)))
    renderChannels()

    expect(document.querySelectorAll('.sk').length).toBeGreaterThan(0)
    expect(screen.queryByText('还没有渠道')).not.toBeInTheDocument()

    release([])
    expect(await screen.findByText('还没有渠道')).toBeInTheDocument()
  })

  it('加载失败遇到 403 时显示本地文案，并给出重试', async () => {
    mocks.listChannels.mockRejectedValue(
      new ApiError(403, 'forbidden', 'you do not have permission to perform this action'),
    )
    renderChannels()
    expect(await screen.findByText('没有权限访问这个资源。')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '重试' })).toBeInTheDocument()
  })

  it('5xx 用我们自己的话，不透出服务端给运维看的那句', async () => {
    mocks.listChannels.mockRejectedValue(new ApiError(500, 'internal', 'dial tcp: connection refused'))
    renderChannels()
    expect(await screen.findByText('数据加载失败。')).toBeInTheDocument()
    expect(screen.queryByText('dial tcp: connection refused')).not.toBeInTheDocument()
  })

  it('别人的渠道返回 404，页面用中文说明，不显示后端的 not found', async () => {
    mocks.listChannels.mockRejectedValue(new ApiError(404, 'not_found', 'channel not found'))
    renderChannels('merchant')
    expect(
      await screen.findByText('没有找到这个资源，可能已被删除，或者不属于你。'),
    ).toBeInTheDocument()
    expect(screen.queryByText('channel not found')).not.toBeInTheDocument()
  })

  it('停用渠道先确认，确认之后才发请求', async () => {
    mocks.listChannels.mockResolvedValue([CHANNEL])
    mocks.updateChannel.mockResolvedValue(undefined)
    renderChannels()

    fireEvent.click(await screen.findByRole('button', { name: '停用' }))
    expect(screen.getByRole('dialog')).toHaveTextContent('停用「platform-anthropic」')
    expect(mocks.updateChannel).not.toHaveBeenCalled()

    const confirmButton = screen.getByRole('button', { name: '确认' })
    // 停用是危险动作，确认钮要跟「保存」区分开。
    expect(confirmButton).toHaveClass('danger')

    fireEvent.click(confirmButton)
    await waitFor(() =>
      expect(mocks.updateChannel).toHaveBeenCalledWith(7, { status: 'disabled' }),
    )
  })

  it('越权写操作在确认弹窗里显示 403 的本地文案', async () => {
    mocks.listChannels.mockResolvedValue([CHANNEL])
    mocks.updateChannel.mockRejectedValue(
      new ApiError(403, 'forbidden', 'you do not have permission to perform this action'),
    )
    renderChannels('merchant')

    fireEvent.click(await screen.findByRole('button', { name: '停用' }))
    fireEvent.click(screen.getByRole('button', { name: '确认' }))

    expect(await screen.findByText('没有权限访问这个资源。')).toBeInTheDocument()
    expect(screen.getByRole('dialog')).toBeInTheDocument()
  })

  it('点「查看」走一次单条查询，显示详情', async () => {
    mocks.listChannels.mockResolvedValue([CHANNEL])
    mocks.getChannel.mockResolvedValue(CHANNEL)
    renderChannels()

    fireEvent.click(await screen.findByRole('button', { name: '查看' }))

    await waitFor(() => expect(mocks.getChannel).toHaveBeenCalledWith(7))
    const dialog = await screen.findByRole('dialog')
    expect(dialog).toHaveTextContent('渠道详情')
    expect(dialog).toHaveTextContent('https://api.example.com/anthropic')
    expect(dialog).toHaveTextContent('abcd')
  })

  it('详情取不到（404）时同样用本地文案', async () => {
    mocks.listChannels.mockResolvedValue([CHANNEL])
    mocks.getChannel.mockRejectedValue(new ApiError(404, 'not_found', 'channel not found'))
    renderChannels('merchant')

    fireEvent.click(await screen.findByRole('button', { name: '查看' }))

    expect(
      await screen.findByText('没有找到这个资源，可能已被删除，或者不属于你。'),
    ).toBeInTheDocument()
    expect(screen.queryByText('channel not found')).not.toBeInTheDocument()
  })

  it('表单改过之后按 Esc 先问一句，确认才关', async () => {
    mocks.listChannels.mockResolvedValue([CHANNEL])
    renderChannels()

    fireEvent.click(await screen.findByRole('button', { name: '编辑' }))
    fireEvent.change(screen.getByLabelText('名称'), { target: { value: 'renamed' } })
    fireEvent.keyDown(document, { key: 'Escape' })

    const guard = screen.getByRole('dialog', { name: '放弃未保存的修改？' })
    fireEvent.click(guard.querySelector('button.danger') as HTMLButtonElement)

    await waitFor(() =>
      expect(screen.queryByRole('dialog', { name: '编辑渠道' })).not.toBeInTheDocument(),
    )
  })

  it('凭证栏不给占位符形态的示例，只说明该填什么', async () => {
    mocks.listChannels.mockResolvedValue([])
    renderChannels()
    await clickCreate()

    const credential = screen.getByLabelText('凭证')
    expect(credential).toHaveAttribute('placeholder', '上游的密钥或 token')
  })

  it('本地校验不通过时不发请求，错误留在表单里', async () => {
    mocks.listChannels.mockResolvedValue([])
    renderChannels()
    const form = await clickCreate()

    fireEvent.submit(form)

    expect(await screen.findByText('请填渠道名称。')).toBeInTheDocument()
    expect(mocks.createChannel).not.toHaveBeenCalled()
  })

  it('服务端 400 与本地校验分开显示，文案来自服务端', async () => {
    mocks.listChannels.mockResolvedValue([])
    mocks.createChannel.mockRejectedValue(
      new ApiError(400, 'invalid_request', 'name is already taken'),
    )
    renderChannels()
    const form = await clickCreate()
    fillValidChannel()
    fireEvent.submit(form)

    // 本地校验通过后进入确认弹窗；确认才发请求。
    expect(await screen.findByText('创建渠道「dup」？')).toBeInTheDocument()
    expect(mocks.createChannel).not.toHaveBeenCalled()

    const creates = screen.getAllByRole('button', { name: '创建' })
    fireEvent.click(creates[creates.length - 1])

    expect(await screen.findByText('name is already taken')).toBeInTheDocument()
    expect(mocks.createChannel).toHaveBeenCalledTimes(1)
  })
})

describe('运营渠道页：窄屏卡片', () => {
  beforeEach(resetMocks)

  it('清单改成卡片，动作用底部菜单，表格列头不在 DOM 里', async () => {
    const restore = mockViewport(375, 667)
    try {
      mocks.listChannels.mockResolvedValue([CHANNEL])
      renderChannels()

      const list = await screen.findByRole('region', { name: '渠道' })
      const card = within(list).getByRole('button', { name: '打开「platform-anthropic」的操作' })
      // 主标题、关键字段、状态三样都在卡片上。
      expect(card).toHaveTextContent('platform-anthropic')
      expect(card).toHaveTextContent('https://api.example.com/anthropic')
      expect(card).toHaveTextContent('anthropic')
      expect(card).toHaveTextContent('1 个')
      expect(card).toHaveTextContent('已启用')
      // 两套清单不能同时进 DOM：读屏会把同一份数据读两遍。
      expect(screen.queryAllByRole('columnheader')).toHaveLength(0)
      expect(screen.queryAllByRole('table')).toHaveLength(0)

      fireEvent.click(card)
      const sheet = screen.getByRole('dialog')
      expect(within(sheet).getByRole('button', { name: '查看' })).toBeInTheDocument()
      expect(within(sheet).getByRole('button', { name: '编辑' })).toBeInTheDocument()
      const disable = within(sheet).getByRole('button', { name: /停用/ })
      // 停用是危险动作：红色之外，可及名里还有「危险操作」。
      expect(disable.className).toContain('danger')
      expect(disable).toHaveAccessibleName(/危险操作/)
    } finally {
      restore()
    }
  })

  // #928：渠道表单不再靠手填适配器与模型。
  async function openCreateAndFillAddress(address = 'https://api.example.com/v1') {
    const form = await clickCreate()
    fireEvent.change(screen.getByLabelText('上游地址'), { target: { value: address } })
    return form
  }

  it('类型改成选择器，选项来自适配器接口', async () => {
    mocks.listChannels.mockResolvedValue([])
    renderChannels()
    await clickCreate()

    // 适配器清单是异步来的：等下拉框真的出现，别抓住加载期间那个文本框。
    const type = (await screen.findByRole('combobox', { name: '类型（适配器）' })) as HTMLSelectElement
    expect(type.tagName).toBe('SELECT')
    expect([...type.options].map((option) => option.value)).toEqual(
      ADAPTERS.map((adapter) => adapter.type),
    )
    expect(type.options[0]).toHaveTextContent('anthropic（/v1/messages）')
  })

  it('适配器清单取不到时退回手填，表单仍然能提交', async () => {
    mocks.listChannels.mockResolvedValue([])
    mocks.listAdapters.mockRejectedValue(new ApiError(500, 'internal', 'boom'))
    mocks.createChannel.mockResolvedValue(9)
    renderChannels()
    const form = await clickCreate()

    expect(await screen.findByText('没有可用的适配器清单，可以直接填类型。')).toBeInTheDocument()
    const type = screen.getByLabelText('类型（适配器）')
    expect(type.tagName).toBe('INPUT')
    fireEvent.change(type, { target: { value: 'legacy-adapter' } })
    fillValidChannel()
    fireEvent.submit(form)

    const creates = await screen.findAllByRole('button', { name: '创建' })
    fireEvent.click(creates[creates.length - 1])
    await waitFor(() =>
      expect(mocks.createChannel).toHaveBeenCalledWith(
        expect.objectContaining({ type: 'legacy-adapter' }),
      ),
    )
  })

  it('适配器清单是空的：不摆一个没得选的下拉框，仍可手填', async () => {
    mocks.listChannels.mockResolvedValue([])
    mocks.listAdapters.mockResolvedValue([])
    renderChannels()
    await clickCreate()

    expect(
      await screen.findByText('没有可用的适配器清单，可以直接填类型。'),
    ).toBeInTheDocument()
    expect(screen.getByLabelText('类型（适配器）').tagName).toBe('INPUT')
  })

  it('勾选的模型写进提交体，再探测一次也不会丢', async () => {
    mocks.listChannels.mockResolvedValue([])
    mocks.probeModels
      .mockResolvedValueOnce({
        supported: true,
        models: ['gpt-4o', 'gpt-4o-mini'],
        total: 2,
        truncated: false,
      })
      .mockResolvedValueOnce({
        supported: true,
        models: ['gpt-4.1', 'gpt-4o', 'gpt-4o-mini'],
        total: 3,
        truncated: false,
      })
    mocks.createChannel.mockResolvedValue(9)
    renderChannels()
    const form = await openCreateAndFillAddress()
    fireEvent.change(screen.getByLabelText('名称'), { target: { value: 'picked' } })
    fireEvent.change(screen.getByLabelText('凭证'), { target: { value: 'placeholder' } })

    fireEvent.click(screen.getByRole('button', { name: '探测模型' }))
    // 新建态带 type + 地址 + 表单里填的凭证；没有 channel_id。
    expect(mocks.probeModels).toHaveBeenNthCalledWith(1, {
      type: 'openai',
      base_url: 'https://api.example.com/v1',
      credential: 'placeholder',
    })
    const picked = await screen.findByRole('checkbox', { name: 'gpt-4o' })
    fireEvent.click(picked)
    expect(picked).toBeChecked()

    // 第二次探测的清单更大：已勾的那个必须还在，不能被新清单覆盖掉。
    fireEvent.click(screen.getByRole('button', { name: '探测模型' }))
    expect(await screen.findByRole('checkbox', { name: 'gpt-4.1' })).toBeInTheDocument()
    expect(screen.getByRole('checkbox', { name: 'gpt-4o' })).toBeChecked()
    expect(screen.getByLabelText('支持的模型（每行一个）')).toHaveValue('gpt-4o')

    fireEvent.submit(form)
    const creates = await screen.findAllByRole('button', { name: '创建' })
    fireEvent.click(creates[creates.length - 1])
    await waitFor(() =>
      expect(mocks.createChannel).toHaveBeenCalledWith(
        expect.objectContaining({ models: ['gpt-4o'] }),
      ),
    )
  })

  it('不支持列模型时给出说明，仍可手填', async () => {
    mocks.listChannels.mockResolvedValue([])
    mocks.probeModels.mockResolvedValue({ supported: false, models: [], total: 0, truncated: false })
    renderChannels()
    await openCreateAndFillAddress()

    fireEvent.click(screen.getByRole('button', { name: '探测模型' }))
    expect(
      await screen.findByText('这个适配器不能列出模型，请在下面手填。'),
    ).toBeInTheDocument()
    expect(screen.getByLabelText('支持的模型（每行一个）')).toBeEnabled()
  })

  it('上游没给清单时给出说明，仍可手填', async () => {
    mocks.listChannels.mockResolvedValue([])
    mocks.probeModels.mockResolvedValue({ supported: true, models: [], total: 0, truncated: false })
    renderChannels()
    await openCreateAndFillAddress()

    fireEvent.click(screen.getByRole('button', { name: '探测模型' }))
    expect(
      await screen.findByText('上游没有返回任何模型，请在下面手填。'),
    ).toBeInTheDocument()
    expect(screen.getByLabelText('支持的模型（每行一个）')).toBeEnabled()
  })

  it('探测失败时给出人话，仍可手填', async () => {
    mocks.listChannels.mockResolvedValue([])
    mocks.probeModels.mockRejectedValue(
      new ApiError(502, 'upstream_unreachable', 'the upstream call failed'),
    )
    renderChannels()
    await openCreateAndFillAddress()

    fireEvent.click(screen.getByRole('button', { name: '探测模型' }))
    expect(await screen.findByText('连不上这个地址：检查地址与网络。')).toBeInTheDocument()
    expect(screen.getByLabelText('支持的模型（每行一个）')).toBeEnabled()
  })

  it('504 说「慢」、502 说「连不上」，两句文案不同', async () => {
    mocks.listChannels.mockResolvedValue([])
    mocks.probeModels
      .mockRejectedValueOnce(
        new ApiError(504, 'gateway_timeout', 'the upstream did not answer within 10s'),
      )
      .mockRejectedValueOnce(new ApiError(502, 'upstream_unreachable', 'the upstream call failed'))
    renderChannels()
    await openCreateAndFillAddress()

    fireEvent.click(screen.getByRole('button', { name: '探测模型' }))
    const slow = await screen.findByText(/上游没有在时限内答复/)
    fireEvent.click(screen.getByRole('button', { name: '探测模型' }))
    const unreachable = await screen.findByText(/连不上这个地址/)

    expect(slow.textContent).not.toBe(unreachable.textContent)
    expect(unreachable.textContent).not.toContain('时限')
  })

  it('429 按 Retry-After 倒计时，期间不能再点', async () => {
    mocks.listChannels.mockResolvedValue([])
    mocks.probeModels.mockRejectedValue(
      new ApiError(429, 'rate_limited', 'too many probe requests', '', 1),
    )
    renderChannels()
    await openCreateAndFillAddress()

    fireEvent.click(screen.getByRole('button', { name: '探测模型' }))
    expect(await screen.findByText('1 秒后可再探测')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '探测模型' })).toBeDisabled()

    await waitFor(
      () => expect(screen.getByRole('button', { name: '探测模型' })).toBeEnabled(),
      { timeout: 3000 },
    )
    expect(screen.queryByText(/秒后可再探测/)).not.toBeInTheDocument()
  })

  it('429 没给 Retry-After 时不编一个秒数：只禁用并说请稍后再试', async () => {
    mocks.listChannels.mockResolvedValue([])
    mocks.probeModels.mockRejectedValue(
      new ApiError(429, 'rate_limited', 'too many probe requests'),
    )
    renderChannels()
    await openCreateAndFillAddress()

    fireEvent.click(screen.getByRole('button', { name: '探测模型' }))
    expect(
      await screen.findByText('探测被限流，稍后再试；重开表单可以再点一次。'),
    ).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '探测模型' })).toBeDisabled()
    expect(screen.queryByText(/秒后可再探测/)).not.toBeInTheDocument()
  })

  it('编辑已有渠道：类型只读，探测按 channel_id 走', async () => {
    mocks.listChannels.mockResolvedValue([CHANNEL])
    mocks.probeModels.mockResolvedValue({
      supported: true,
      models: ['claude-sonnet-4-5'],
      total: 1,
      truncated: false,
    })
    renderChannels()

    fireEvent.click(await screen.findByRole('button', { name: '编辑' }))
    const type = await screen.findByLabelText('类型（适配器）')
    expect(type.tagName).toBe('INPUT')
    expect(type).toHaveValue('anthropic')
    expect(type).toBeDisabled()
    expect(screen.getByText(/已存渠道的类型不可在这里改/)).toBeInTheDocument()

    // 探测带 channel_id 与表单里的地址；不另传 type 去覆盖库里那一行。
    fireEvent.click(screen.getByRole('button', { name: '探测模型' }))
    expect(
      await screen.findByRole('checkbox', { name: 'claude-sonnet-4-5' }),
    ).toBeInTheDocument()
    expect(mocks.probeModels).toHaveBeenCalledWith({
      channel_id: 7,
      base_url: 'https://api.example.com/anthropic',
    })
  })

  it('探测按钮只在地址非空时可点', async () => {
    mocks.listChannels.mockResolvedValue([])
    renderChannels()
    await clickCreate()

    expect(screen.getByRole('button', { name: '探测模型' })).toBeDisabled()
    fireEvent.change(screen.getByLabelText('上游地址'), {
      target: { value: 'https://api.example.com/v1' },
    })
    expect(screen.getByRole('button', { name: '探测模型' })).toBeEnabled()
    expect(mocks.probeModels).not.toHaveBeenCalled()
  })

  it('清单被截断时说清上游一共给了多少', async () => {
    mocks.listChannels.mockResolvedValue([])
    mocks.probeModels.mockResolvedValue({
      supported: true,
      models: ['m-1'],
      total: 2500,
      truncated: true,
    })
    renderChannels()
    await openCreateAndFillAddress()

    fireEvent.click(screen.getByRole('button', { name: '探测模型' }))
    expect(
      await screen.findByText('上游共 2500 个模型，这里只列出前 1 个。'),
    ).toBeInTheDocument()
  })

  it('探测还没回来就关掉表单，结果不会落进下一次打开的表单', async () => {
    mocks.listChannels.mockResolvedValue([])
    let settle: (result: ChannelProbeResult) => void = () => {}
    mocks.probeModels.mockImplementation(
      () =>
        new Promise<ChannelProbeResult>((resolve) => {
          settle = resolve
        }),
    )
    renderChannels()
    await openCreateAndFillAddress()

    fireEvent.click(screen.getByRole('button', { name: '探测模型' }))
    // 改过地址，关窗要先确认一次。
    fireEvent.keyDown(document, { key: 'Escape' })
    const guard = screen.getByRole('dialog', { name: '放弃未保存的修改？' })
    fireEvent.click(guard.querySelector('button.danger') as HTMLButtonElement)
    await waitFor(() =>
      expect(screen.queryByRole('dialog', { name: '新建渠道' })).not.toBeInTheDocument(),
    )

    // 重新打开表单之后，上一次探测才回来：清单不能画到这张新表单上。
    await openCreateAndFillAddress()
    await act(async () => {
      settle({ supported: true, models: ['gpt-4o'], total: 1, truncated: false })
    })
    expect(screen.queryByRole('checkbox', { name: 'gpt-4o' })).not.toBeInTheDocument()
  })

  it('商家视图不出现探测：这一轮只做管理员渠道页', async () => {
    mocks.listChannels.mockResolvedValue([])
    renderChannels('merchant')
    await clickCreate()

    expect(screen.queryByRole('button', { name: '探测模型' })).not.toBeInTheDocument()
    expect(screen.getByLabelText('支持的模型（每行一个）')).toBeInTheDocument()
    expect(mocks.listAdapters).not.toHaveBeenCalled()
  })
})
