import { fireEvent, render, screen, waitFor } from '@testing-library/react'
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
})
