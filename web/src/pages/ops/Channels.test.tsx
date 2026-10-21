import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { I18nProvider } from '../../i18n'
import { SiteProvider } from '../../site/SiteProvider'
import { ToastProvider } from '../../components/ui'
import { OpsChannels } from './Channels'
import { mockViewport } from '../../test/media'

const mocks = vi.hoisted(() => ({
  listChannels: vi.fn(),
  listModels: vi.fn(),
  createChannel: vi.fn(),
  updateChannel: vi.fn(),
  getChannel: vi.fn(),
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

describe('运营渠道页', () => {
  beforeEach(() => {
    Object.values(mocks).forEach((mock) => mock.mockReset())
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
  beforeEach(() => {
    Object.values(mocks).forEach((mock) => mock.mockReset())
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
})
