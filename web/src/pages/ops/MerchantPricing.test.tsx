import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { I18nProvider } from '../../i18n'
import { SiteProvider } from '../../site/SiteProvider'
import { ToastProvider } from '../../components/ui'
import { MerchantPricing } from './MerchantPricing'

const mocks = vi.hoisted(() => ({
  listRules: vi.fn(),
  getRule: vi.fn(),
  createRule: vi.fn(),
  updateRule: vi.fn(),
  meta: vi.fn(),
}))

vi.mock('../../api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../api')>()
  return {
    ...actual,
    api: {
      merchant: {
        listRules: mocks.listRules,
        getRule: mocks.getRule,
        createRule: mocks.createRule,
        updateRule: mocks.updateRule,
      },
      meta: mocks.meta,
      session: vi.fn().mockResolvedValue(null),
    },
  }
})

const { ApiError } = await import('../../api')

const RULE = {
  id: 11,
  scope: 'merchant',
  owner_user_id: 1,
  channel_id: 0,
  name: 'own-price',
  model_patterns: ['claude-sonnet-4-5'],
  expr: '{"prompt": 0.000003}',
  priority: 0,
  effective_from: '2026-01-01T00:00:00Z',
  effective_to: null,
  enabled: true,
  created_by: 1,
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z',
  declared_keys: [],
}

function renderPricing() {
  return render(
    <I18nProvider>
      <ToastProvider>
        <SiteProvider>
          <MerchantPricing />
        </SiteProvider>
      </ToastProvider>
    </I18nProvider>,
  )
}

async function fillAndSubmit(name: string) {
  const [first] = await screen.findAllByRole('button', { name: '新建规则' })
  fireEvent.click(first)
  const form = screen.getByRole('dialog', { name: '新建规则' })
  fireEvent.change(within(form).getByLabelText('名称'), { target: { value: name } })
  fireEvent.change(within(form).getByLabelText('模型匹配式（每行一个）'), {
    target: { value: 'claude-sonnet-4-5' },
  })
  fireEvent.change(within(form).getByLabelText('输入'), { target: { value: '3' } })
  fireEvent.submit(document.getElementById('merchant-rule-form') as HTMLFormElement)
  return form
}

describe('商家计价页', () => {
  beforeEach(() => {
    Object.values(mocks).forEach((mock) => mock.mockReset())
    mocks.listRules.mockResolvedValue([])
    mocks.meta.mockResolvedValue({
      site_name: 'larkax',
      api_base_url: 'https://api.example.com/v1',
      currency: 'USD',
      timezone: 'Asia/Shanghai',
      locales: ['en', 'zh'],
      features: { registration: false, password_reset: 'admin', topup: false },
    })
  })

  it('新建的规则出现在自己的清单里', async () => {
    mocks.createRule.mockResolvedValue(11)
    mocks.listRules.mockResolvedValueOnce([]).mockResolvedValue([RULE])
    renderPricing()

    expect(await screen.findByText('还没有自己的计价规则')).toBeInTheDocument()
    await fillAndSubmit('own-price')

    const confirm = screen.getByRole('dialog', { name: '确认创建' })
    fireEvent.click(within(confirm).getByRole('button', { name: '创建' }))

    await waitFor(() =>
      expect(mocks.createRule).toHaveBeenCalledWith({
        name: 'own-price',
        model_patterns: ['claude-sonnet-4-5'],
        expr: '{"prompt": 0.000003}',
        priority: 0,
        enabled: true,
      }),
    )
    expect(await screen.findByText('own-price')).toBeInTheDocument()
  })

  it('编辑只发可变字段，作用域与归属不进请求体', async () => {
    mocks.listRules.mockResolvedValue([RULE])
    mocks.updateRule.mockResolvedValue(undefined)
    renderPricing()

    fireEvent.click(await screen.findByRole('button', { name: '编辑' }))
    const form = screen.getByRole('dialog', { name: '编辑规则' })
    fireEvent.change(within(form).getByLabelText('输入'), { target: { value: '4' } })
    fireEvent.submit(document.getElementById('merchant-rule-form') as HTMLFormElement)

    const confirm = screen.getByRole('dialog', { name: '确认保存' })
    fireEvent.click(within(confirm).getByRole('button', { name: '保存' }))

    await waitFor(() => expect(mocks.updateRule).toHaveBeenCalled())
    const [id, payload] = mocks.updateRule.mock.calls[0]
    expect(id).toBe(11)
    expect(payload).toEqual({
      name: 'own-price',
      model_patterns: ['claude-sonnet-4-5'],
      expr: '{"prompt": 0.000004}',
      priority: 0,
      enabled: true,
    })
    expect(payload).not.toHaveProperty('scope')
    expect(payload).not.toHaveProperty('owner_user_id')
    expect(payload).not.toHaveProperty('channel_id')
  })

  it('五行填满后，提交的 expr 是拼好的 JSON 串', async () => {
    mocks.createRule.mockResolvedValue(12)
    renderPricing()

    const [first] = await screen.findAllByRole('button', { name: '新建规则' })
    fireEvent.click(first)
    const form = screen.getByRole('dialog', { name: '新建规则' })
    fireEvent.change(within(form).getByLabelText('名称'), { target: { value: 'five-rows' } })
    fireEvent.change(within(form).getByLabelText('模型匹配式（每行一个）'), {
      target: { value: 'claude-sonnet-4-5' },
    })
    for (const [label, value] of Object.entries({
      输入: '3.5',
      输出: '15',
      缓存读: '1.5',
      缓存写: '1.5',
      推理: '6',
    })) {
      fireEvent.change(within(form).getByLabelText(label), { target: { value } })
    }
    fireEvent.submit(document.getElementById('merchant-rule-form') as HTMLFormElement)

    const confirm = screen.getByRole('dialog', { name: '确认创建' })
    fireEvent.click(within(confirm).getByRole('button', { name: '创建' }))

    await waitFor(() =>
      expect(mocks.createRule).toHaveBeenCalledWith({
        name: 'five-rows',
        model_patterns: ['claude-sonnet-4-5'],
        expr: '{"prompt": 0.0000035, "completion": 0.000015, "cache_read": 0.0000015, "cache_write": 0.0000015, "reasoning": 0.000006}',
        priority: 0,
        enabled: true,
      }),
    )
  })

  it('存量表达式的编译错误在商家页也显示出来', async () => {
    mocks.listRules.mockResolvedValue([
      { ...RULE, expr: '{"promt": 0.000003}', expr_error: '编译失败：未知的键 promt' },
    ])
    renderPricing()

    expect(await screen.findByText('编译失败：未知的键 promt')).toBeInTheDocument()
  })

  it('改别人的或平台的规则拿 404 时，页面给中文说明', async () => {
    mocks.listRules.mockResolvedValue([RULE])
    mocks.updateRule.mockRejectedValue(new ApiError(404, 'not_found', 'not found'))
    renderPricing()

    fireEvent.click(await screen.findByRole('button', { name: '编辑' }))
    const form = screen.getByRole('dialog', { name: '编辑规则' })
    fireEvent.change(within(form).getByLabelText('输入'), { target: { value: '4' } })
    fireEvent.submit(document.getElementById('merchant-rule-form') as HTMLFormElement)
    const confirm = screen.getByRole('dialog', { name: '确认保存' })
    fireEvent.click(within(confirm).getByRole('button', { name: '保存' }))

    expect(
      await screen.findByText('没有找到这个资源，可能已被删除，或者不属于你。'),
    ).toBeInTheDocument()
    expect(screen.queryByText('not found')).not.toBeInTheDocument()
  })

  it('停用走清单行上的独立入口：只发 enabled，确认钮是危险色，文案点名后果', async () => {
    mocks.listRules.mockResolvedValue([RULE])
    mocks.updateRule.mockResolvedValue(undefined)
    renderPricing()

    fireEvent.click(await screen.findByRole('button', { name: '停用' }))
    const dialog = screen.getByRole('dialog', { name: '停用' })
    // 只说「停用规则 X？」不够：点确认的人要看到后果。
    expect(dialog).toHaveTextContent('不再计价')
    const confirm = within(dialog).getByRole('button', { name: '确认' })
    expect(confirm.className).toContain('danger')

    fireEvent.click(confirm)
    await waitFor(() => expect(mocks.updateRule).toHaveBeenCalledWith(11, { enabled: false }))
    // 只带这一栏：整表单回写会把别人刚改的其它字段一起盖回去。
    expect(mocks.updateRule.mock.calls[0][1]).toEqual({ enabled: false })
  })

  it('改过之后 Esc、点遮罩、取消都先问一句；留下不动，放弃才关', async () => {
    mocks.listRules.mockResolvedValue([RULE])
    renderPricing()

    fireEvent.click(await screen.findByRole('button', { name: '编辑' }))
    const form = screen.getByRole('dialog', { name: '编辑规则' })
    fireEvent.change(within(form).getByLabelText('名称'), { target: { value: 'renamed' } })

    const discard = () => screen.getByRole('dialog', { name: '放弃未保存的修改？' })
    const keep = () => fireEvent.click(within(discard()).getByRole('button', { name: '取消' }))

    // 出口一：Esc
    fireEvent.keyDown(document, { key: 'Escape' })
    expect(discard()).toBeInTheDocument()
    keep()
    // 「留下」只收起这一问：表单与刚输入的值都还在。
    expect(screen.getByRole('dialog', { name: '编辑规则' })).toBeInTheDocument()
    expect(screen.getByLabelText('名称')).toHaveValue('renamed')

    // 脏着的时候，刷新 / 关标签页要被拦一下。
    const dirtyEvent = new Event('beforeunload', { cancelable: true })
    window.dispatchEvent(dirtyEvent)
    expect(dirtyEvent.defaultPrevented).toBe(true)

    // 出口二：点遮罩
    fireEvent.mouseDown(document.querySelector('.overlay') as Element)
    expect(discard()).toBeInTheDocument()
    keep()

    // 出口三：表单自己的「取消」
    fireEvent.click(within(form).getByRole('button', { name: '取消' }))
    expect(discard()).toBeInTheDocument()

    // 只有「放弃修改」才真的关掉表单。
    fireEvent.click(within(discard()).getByRole('button', { name: '放弃修改' }))
    expect(screen.queryByRole('dialog', { name: '编辑规则' })).not.toBeInTheDocument()

    // 关掉之后这一页不再算改过。`closeForm` 不重置 form/initialForm，脏判据里
    // 少了「表单开着」这一条的话，放弃一次之后每次刷新都还会再问。
    const cleanEvent = new Event('beforeunload', { cancelable: true })
    window.dispatchEvent(cleanEvent)
    expect(cleanEvent.defaultPrevented).toBe(false)
  })

  it('清单按每 100 万 token 显示单价，条件表达式原样，页面标一次币种', async () => {
    const tiered = 'len <= 32000 ? {"prompt": 0.0000035} : {"prompt": 0.00001}'
    mocks.listRules.mockResolvedValue([
      { ...RULE, name: 'simple', expr: '{"prompt": 0.0000035}' },
      { ...RULE, id: 12, name: 'tiered', expr: tiered },
      { ...RULE, id: 13, name: 'tiny', expr: '{"prompt": 0.0000001}' },
    ])
    renderPricing()

    // 表单按每百万填、库里是每 token，差六个数量级；原先直接渲染 rule.expr。
    expect(await screen.findByText('{"prompt": 3.5}')).toBeInTheDocument()
    expect(screen.queryByText('{"prompt": 0.0000035}')).not.toBeInTheDocument()
    // `cell-ellipsis` 会截断，完整的一段挂在 title 上。
    expect(screen.getByText('{"prompt": 3.5}')).toHaveAttribute('title', '{"prompt": 3.5}')
    // 1e-7 这一档才是十进制换算的判据：同一个值用浮点乘是 0.09999999999999999，
    // 而 `0.0000035 * 1e6` 恰好是干净的 3.5，拿它证明不了没走浮点。
    expect(screen.getByText('{"prompt": 0.1}')).toBeInTheDocument()
    expect(screen.queryByText('{"prompt": 0.0000001}')).not.toBeInTheDocument()
    // 带条件的不是一张单价表，不硬乘。
    expect(screen.getByText(tiered)).toBeInTheDocument()
    // 币种在页面里出现（每百万那句话带上它）。
    expect(screen.getByText(/USD/)).toBeInTheDocument()
  })
})
