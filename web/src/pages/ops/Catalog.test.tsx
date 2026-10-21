import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { I18nProvider } from '../../i18n'
import { SiteProvider } from '../../site/SiteProvider'
import { ToastProvider } from '../../components/ui'
import { OpsModels } from './Catalog'

const mocks = vi.hoisted(() => ({
  listModels: vi.fn(),
  createModel: vi.fn(),
  updateModel: vi.fn(),
  deleteModel: vi.fn(),
  meta: vi.fn(),
}))

vi.mock('../../api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../api')>()
  const ops = {
    listModels: mocks.listModels,
    createModel: mocks.createModel,
    updateModel: mocks.updateModel,
    deleteModel: mocks.deleteModel,
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

const MODEL = {
  canonical_name: 'claude-sonnet-4-5',
  display_name: 'Claude Sonnet 4.5',
  vendor: 'anthropic',
  family: 'claude',
  context_limit: 200000,
  input_limit: 200000,
  output_limit: 64000,
  knowledge_cutoff: null,
  release_date: null,
  modalities_input: ['text', 'image'],
  modalities_output: ['text'],
  capabilities: {
    reasoning: true,
    tools: true,
    vision: true,
    audio: false,
    cache: null,
    batch: null,
    endpoints: ['anthropic'],
  },
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-02T00:00:00Z',
}

const UNKNOWN_ENDPOINTS = {
  ...MODEL,
  capabilities: { ...MODEL.capabilities, endpoints: null },
}

function renderModels() {
  return render(
    <I18nProvider>
      <ToastProvider>
        <SiteProvider>
          <OpsModels />
        </SiteProvider>
      </ToastProvider>
    </I18nProvider>,
  )
}

async function openCreate() {
  const [first] = await screen.findAllByRole('button', { name: '新建模型' })
  fireEvent.click(first)
  return document.getElementById('model-form') as HTMLFormElement
}

// tab 的可及名字在有校验错误时会多出圆点的 aria-label，所以按前缀匹配。
function tab(label: string) {
  return screen.getByRole('tab', { name: new RegExp(`^${label}`) })
}

describe('模型目录页', () => {
  beforeEach(() => {
    Object.values(mocks).forEach((mock) => mock.mockReset())
    mocks.meta.mockResolvedValue({
      site_name: 'larkax',
      api_base_url: 'https://api.example.com/v1',
      currency: 'USD',
      timezone: 'Asia/Shanghai',
      locales: ['en', 'zh'],
      features: { registration: false, password_reset: 'admin', topup: false },
    })
  })

  it('搜索只留下匹配的模型', async () => {
    mocks.listModels.mockResolvedValue([
      MODEL,
      { ...MODEL, canonical_name: 'gpt-5', display_name: 'GPT-5', vendor: 'openai' },
    ])
    renderModels()

    expect(await screen.findByText('claude-sonnet-4-5')).toBeInTheDocument()
    expect(screen.getByText('gpt-5')).toBeInTheDocument()

    fireEvent.change(screen.getByLabelText('搜索'), { target: { value: 'gpt' } })
    expect(screen.queryByText('claude-sonnet-4-5')).not.toBeInTheDocument()
    expect(screen.getByText('gpt-5')).toBeInTheDocument()
  })

  it('本地校验不通过时不发请求', async () => {
    mocks.listModels.mockResolvedValue([])
    renderModels()
    const form = await openCreate()

    fireEvent.submit(form)

    expect(await screen.findByText('请填规范名。')).toBeInTheDocument()
    expect(mocks.createModel).not.toHaveBeenCalled()
  })

  it('能力的三种取值原样提交：未知是 null，不支持是 false', async () => {
    mocks.listModels.mockResolvedValue([MODEL])
    mocks.createModel.mockResolvedValue(1)
    renderModels()

    const form = await openCreate()
    fireEvent.change(screen.getByLabelText('规范名'), { target: { value: 'gpt-4.1' } })
    fireEvent.click(tab('能力'))
    fireEvent.change(screen.getByLabelText('推理'), { target: { value: 'no' } })
    fireEvent.submit(form)

    expect(await screen.findByText('新建模型「gpt-4.1」？')).toBeInTheDocument()
    const buttons = screen.getAllByRole('button', { name: '创建' })
    fireEvent.click(buttons[buttons.length - 1])

    await waitFor(() => expect(mocks.createModel).toHaveBeenCalledTimes(1))
    const input = mocks.createModel.mock.calls[0][0]
    expect(input.capabilities.reasoning).toBe(false)
    expect(input.capabilities.tools).toBeNull()
    expect(input.capabilities.vision).toBeNull()
  })

  it('删除先确认，确认之后才发请求', async () => {
    mocks.listModels.mockResolvedValue([MODEL])
    mocks.deleteModel.mockResolvedValue(undefined)
    renderModels()
    await screen.findByText('claude-sonnet-4-5')

    fireEvent.click(screen.getByRole('button', { name: '删除' }))
    expect(screen.getByRole('dialog')).toHaveTextContent('删除「claude-sonnet-4-5」')
    expect(mocks.deleteModel).not.toHaveBeenCalled()

    // 确认弹窗里也有一个「删除」，它在 DOM 里靠后。
    const deletes = screen.getAllByRole('button', { name: '删除' })
    fireEvent.click(deletes[deletes.length - 1])
    await waitFor(() => expect(mocks.deleteModel).toHaveBeenCalledWith('claude-sonnet-4-5'))
  })

  it('端点未知的模型原样保存，提交的仍是 null 而不是空数组', async () => {
    mocks.listModels.mockResolvedValue([UNKNOWN_ENDPOINTS])
    mocks.updateModel.mockResolvedValue(undefined)
    renderModels()

    fireEvent.click(await screen.findByRole('button', { name: '编辑' }))
    fireEvent.submit(document.getElementById('model-form') as HTMLFormElement)

    const saves = await screen.findAllByRole('button', { name: '保存' })
    fireEvent.click(saves[saves.length - 1])

    await waitFor(() => expect(mocks.updateModel).toHaveBeenCalledTimes(1))
    const [name, input] = mocks.updateModel.mock.calls[0]
    expect(name).toBe('claude-sonnet-4-5')
    expect(input.capabilities.endpoints).toBeNull()
  })

  it('服务端冲突显示后端文案，不换成自己的话', async () => {
    mocks.listModels.mockResolvedValue([])
    mocks.createModel.mockRejectedValue(
      new ApiError(409, 'conflict', 'model already exists'),
    )
    renderModels()

    const form = await openCreate()
    fireEvent.change(screen.getByLabelText('规范名'), { target: { value: 'dup' } })
    fireEvent.submit(form)

    const buttons = await screen.findAllByRole('button', { name: '创建' })
    fireEvent.click(buttons[buttons.length - 1])

    expect(await screen.findByText('model already exists')).toBeInTheDocument()
  })

  it('四组 tab 都在，切换后看到对应字段', async () => {
    mocks.listModels.mockResolvedValue([])
    renderModels()
    await openCreate()

    for (const label of ['基本', '限制', '能力', '模态与时间']) {
      expect(tab(label)).toBeInTheDocument()
    }
    // 这组 tab 的名字：读屏进来先听到「模型字段分组」，再听到四个选项。
    expect(screen.getByRole('tablist', { name: '模型字段分组' })).toBeInTheDocument()
    // 默认停在「基本」。
    expect(tab('基本')).toHaveAttribute('aria-selected', 'true')
    expect(screen.getByLabelText('规范名')).toBeInTheDocument()

    fireEvent.click(tab('限制'))
    expect(screen.getByLabelText('上下文上限')).toBeInTheDocument()
    expect(screen.queryByLabelText('规范名')).not.toBeInTheDocument()

    fireEvent.click(tab('能力'))
    expect(screen.getByLabelText('端点（每行一个）')).toBeInTheDocument()
    expect(screen.getByLabelText('推理')).toBeInTheDocument()

    fireEvent.click(tab('模态与时间'))
    expect(tab('模态与时间')).toHaveAttribute('aria-selected', 'true')
    expect(screen.getByLabelText('知识截止')).toBeInTheDocument()
    expect(screen.getByLabelText('输入模态（每行一个）')).toBeInTheDocument()
  })

  it('规范名留空提交时停在「基本」组并标出错误', async () => {
    mocks.listModels.mockResolvedValue([])
    renderModels()
    const form = await openCreate()

    // 先切到别组再提交：错误必须把人带回看不到的那个组。
    fireEvent.click(tab('能力'))
    fireEvent.submit(form)

    expect(await screen.findByText('请填规范名。')).toBeInTheDocument()
    expect(tab('基本')).toHaveAttribute('aria-selected', 'true')
    expect(screen.getByLabelText('规范名')).toBeInTheDocument()
    expect(within(tab('基本')).getByLabelText('这一组有校验错误')).toBeInTheDocument()
    expect(mocks.createModel).not.toHaveBeenCalled()
  })

  it('上限填非整数时跳到「限制」组', async () => {
    mocks.listModels.mockResolvedValue([])
    renderModels()
    const form = await openCreate()

    fireEvent.change(screen.getByLabelText('规范名'), { target: { value: 'gpt-4.1' } })
    fireEvent.click(tab('限制'))
    fireEvent.change(screen.getByLabelText('上下文上限'), { target: { value: '12.5' } })
    fireEvent.click(tab('基本'))
    fireEvent.submit(form)

    expect(await screen.findByText('上下文上限要填整数。')).toBeInTheDocument()
    expect(tab('限制')).toHaveAttribute('aria-selected', 'true')
    // 出错的分组带上 `aria-invalid`，其余三个不带：读屏听得出该回哪一组。
    expect(tab('限制')).toHaveAttribute('aria-invalid', 'true')
    for (const label of ['基本', '能力', '模态与时间']) {
      expect(tab(label)).not.toHaveAttribute('aria-invalid')
    }
    expect(screen.getByLabelText('上下文上限')).toHaveValue('12.5')
    expect(within(tab('限制')).getByLabelText('这一组有校验错误')).toBeInTheDocument()
    expect(mocks.createModel).not.toHaveBeenCalled()
  })

  it('切换分组不丢已填内容', async () => {
    mocks.listModels.mockResolvedValue([])
    renderModels()
    await openCreate()

    fireEvent.change(screen.getByLabelText('显示名'), { target: { value: '保留我' } })
    fireEvent.change(screen.getByLabelText('厂商'), { target: { value: 'example' } })

    fireEvent.click(tab('能力'))
    fireEvent.click(tab('模态与时间'))
    fireEvent.click(tab('基本'))

    expect(screen.getByLabelText('显示名')).toHaveValue('保留我')
    expect(screen.getByLabelText('厂商')).toHaveValue('example')
  })

  it('保存发出的载荷与改动前逐字段一致', async () => {
    mocks.listModels.mockResolvedValue([])
    mocks.createModel.mockResolvedValue(1)
    renderModels()
    const form = await openCreate()

    fireEvent.change(screen.getByLabelText('规范名'), { target: { value: 'gpt-x' } })
    fireEvent.change(screen.getByLabelText('显示名'), { target: { value: 'GPT X' } })
    fireEvent.change(screen.getByLabelText('厂商'), { target: { value: 'openai' } })
    fireEvent.change(screen.getByLabelText('系列'), { target: { value: 'gpt' } })

    fireEvent.click(tab('限制'))
    fireEvent.change(screen.getByLabelText('上下文上限'), { target: { value: '1000' } })
    fireEvent.change(screen.getByLabelText('输入上限'), { target: { value: '900' } })
    fireEvent.change(screen.getByLabelText('输出上限'), { target: { value: '100' } })

    fireEvent.click(tab('能力'))
    fireEvent.change(screen.getByLabelText('推理'), { target: { value: 'no' } })
    fireEvent.change(screen.getByLabelText('工具'), { target: { value: 'yes' } })
    fireEvent.change(screen.getByLabelText('音频'), { target: { value: 'yes' } })
    fireEvent.change(screen.getByLabelText('缓存'), { target: { value: 'no' } })
    fireEvent.change(screen.getByLabelText('端点（每行一个）'), {
      target: { value: 'chat\nresponses' },
    })

    fireEvent.click(tab('模态与时间'))
    fireEvent.change(screen.getByLabelText('输入模态（每行一个）'), {
      target: { value: 'text\nimage' },
    })
    fireEvent.change(screen.getByLabelText('输出模态（每行一个）'), { target: { value: 'text' } })
    fireEvent.change(screen.getByLabelText('知识截止'), { target: { value: '2024-03-01' } })
    fireEvent.change(screen.getByLabelText('发布日期'), { target: { value: '2024-06-01' } })

    fireEvent.submit(form)
    const buttons = await screen.findAllByRole('button', { name: '创建' })
    fireEvent.click(buttons[buttons.length - 1])

    await waitFor(() => expect(mocks.createModel).toHaveBeenCalledTimes(1))
    expect(mocks.createModel.mock.calls[0][0]).toEqual({
      canonical_name: 'gpt-x',
      display_name: 'GPT X',
      vendor: 'openai',
      family: 'gpt',
      context_limit: 1000,
      input_limit: 900,
      output_limit: 100,
      knowledge_cutoff: '2024-03-01',
      release_date: '2024-06-01',
      modalities_input: ['text', 'image'],
      modalities_output: ['text'],
      capabilities: {
        reasoning: false,
        tools: true,
        vision: null,
        audio: true,
        cache: false,
        batch: null,
        endpoints: ['chat', 'responses'],
      },
    })
  })

  it('规范名留空提交时，焦点落在规范名输入框上', async () => {
    mocks.listModels.mockResolvedValue([])
    renderModels()
    const form = await openCreate()

    // 先切到别组：错误会把人带回「基本」，而切组本身会把原来的输入框卸载。
    fireEvent.click(tab('能力'))
    fireEvent.submit(form)

    expect(await screen.findByText('请填规范名。')).toBeInTheDocument()
    expect(screen.getByLabelText('规范名')).toHaveFocus()
    expect(mocks.createModel).not.toHaveBeenCalled()
  })

  // 三格共用一个错位实现时，只测一格看不出另两格错到了哪里，所以逐格来。
  it.each([
    ['上下文上限', '上下文上限要填整数。'],
    ['输入上限', '输入上限要填整数。'],
    ['输出上限', '输出上限要填整数。'],
  ])('%s填错时，焦点落在这一格上', async (label, message) => {
    mocks.listModels.mockResolvedValue([])
    renderModels()
    const form = await openCreate()

    fireEvent.change(screen.getByLabelText('规范名'), { target: { value: 'gpt-4.1' } })
    fireEvent.click(tab('限制'))
    fireEvent.change(screen.getByLabelText(label), { target: { value: '12.5' } })
    fireEvent.click(tab('基本'))
    fireEvent.submit(form)

    expect(await screen.findByText(message)).toBeInTheDocument()
    expect(screen.getByLabelText(label)).toHaveFocus()
    expect(mocks.createModel).not.toHaveBeenCalled()
  })

  it('错误句是一块常驻的 alert 区域：出错时念出来，改好后清空', async () => {
    mocks.listModels.mockResolvedValue([])
    renderModels()
    const form = await openCreate()

    fireEvent.submit(form)
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('请填规范名。')

    fireEvent.change(screen.getByLabelText('规范名'), { target: { value: 'gpt-4.1' } })
    fireEvent.submit(form)

    // 同一个节点只是被清空：没有新句子可念，也不会把旧句子再念一遍。
    expect(screen.getByRole('alert')).toBeEmptyDOMElement()
  })
})
