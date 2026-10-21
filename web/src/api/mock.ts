import { ApiError } from './client'
import type {
  AliasUpdateInput,
  ApiClient,
  ApiKey,
  Channel,
  ChannelAlias,
  ChannelCreateInput,
  CostCreateInput,
  CostUpdateInput,
  CreatedKey,
  KeyCreateInput,
  KeyListQuery,
  KeyUpdateInput,
  ListResponse,
  Meta,
  Model,
  ModelsResponse,
  ChannelOpsClient,
  MerchantPricingClient,
  OpsClient,
  OpsModel,
  OpsModelCreateInput,
  OpsModelInput,
  PricingCost,
  PricingRule,
  RuleCreateInput,
  RuleUpdateInput,
  SessionResponse,
  Tokens,
  UsageBreakdown,
  UsageBreakdownQuery,
  UsageGroup,
  UsagePath,
  UsageQuery,
  UsageRequest,
  UsageRequestsQuery,
  UsageSeries,
  UsageSeriesQuery,
  UsageSummary,
  UsageTotals,
  User,
  Wallet,
  WalletEntriesQuery,
  WalletEntry,
} from './types'

/**
 * 后端未就绪时的同形状替身。
 *
 * 只在 `VITE_API_MODE` 不是 `http` 时启用；数据全在内存里，刷新页面后
 * 新建的密钥会消失（登录态放 sessionStorage，刷新仍在）。
 */

const SESSION_KEY = 'larkax.mock.session'
const DAY_MS = 86_400_000
const MOCK_CSRF = 'mock-csrf-token'
const SITE_TZ = 'Asia/Shanghai'
const SITE_OFFSET = '+08:00'

/**
 * 站点时区里的日期，`YYYY-MM-DD`。
 *
 * 演示数据的分桶必须与 `todayInZone(SITE_TZ)` 用的是同一个「今天」；
 * 按 UTC 取日期，在东八区的凌晨会整整差一天，界面就会显示「今日 0」而
 * 请求日志里明明有今天的行。
 */
function dateOnly(date: Date): string {
  const parts = new Intl.DateTimeFormat('en-CA', {
    timeZone: SITE_TZ,
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
  }).formatToParts(date)
  const pick = (type: string) => parts.find((part) => part.type === type)?.value ?? ''
  return `${pick('year')}-${pick('month')}-${pick('day')}`
}

function addDays(date: Date, days: number): Date {
  return new Date(date.getTime() + days * DAY_MS)
}

function money(amount: number): string {
  return amount.toFixed(8)
}

function delay(): Promise<void> {
  const ms = 90 + Math.floor(Math.random() * 120)
  return new Promise((resolve) => setTimeout(resolve, ms))
}

function randomSecret(): string {
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_'
  const bytes = new Uint8Array(32)
  if (typeof crypto !== 'undefined' && crypto.getRandomValues) {
    crypto.getRandomValues(bytes)
  } else {
    for (let i = 0; i < bytes.length; i += 1) bytes[i] = Math.floor(Math.random() * 256)
  }
  let out = ''
  for (const byte of bytes) out += alphabet[byte % alphabet.length]
  return `sk-${out}`
}

function mask(secret: string): { prefix: string; suffix: string; masked: string } {
  const body = secret.slice(3)
  const prefix = `sk-${body.slice(0, 8)}`
  const suffix = body.slice(-6)
  return { prefix, suffix, masked: `${prefix}${'*'.repeat(18)}${suffix}` }
}

function tokens(prompt: number, completion: number, cacheRead = 0, cacheWrite = 0, reasoning = 0): Tokens {
  return { input: prompt, output: completion, cache_read: cacheRead, cache_write: cacheWrite, reasoning }
}

function totals(cost: number, requests: number, tokensValue: Tokens): UsageTotals {
  return { cost: money(cost), requests, tokens: tokensValue }
}

/** 固定的演示数据，不随刷新变化。 */
const DAILY = [
  { requests: 412, prompt: 182_400, completion: 96_300, cost: 5.8124 },
  { requests: 388, prompt: 171_900, completion: 90_100, cost: 5.3291 },
  { requests: 496, prompt: 224_700, completion: 118_600, cost: 6.9403 },
  { requests: 301, prompt: 140_200, completion: 74_800, cost: 4.3188 },
  { requests: 275, prompt: 128_600, completion: 68_400, cost: 3.9072 },
  { requests: 522, prompt: 241_300, completion: 127_200, cost: 7.4581 },
  { requests: 468, prompt: 213_800, completion: 112_900, cost: 6.6127 },
]

const MODELS: Model[] = [
  {
    name: 'claude-sonnet-4-5',
    display_name: 'Claude Sonnet 4.5',
    vendor: 'anthropic',
    context_limit: 200_000,
    max_output: 64_000,
    capabilities: { tools: true, vision: true, reasoning: true, caching: true },
    modalities: { input: ['text', 'image'], output: ['text'] },
    pricing: {
      input_per_mtok: '3.00000000',
      output_per_mtok: '15.00000000',
      cache_read_per_mtok: '0.30000000',
      cache_write_per_mtok: '3.75000000',
    },
    offers: [
      { merchant: 'acme', price: '0.00350000' },
      { merchant: null, price: '0.00400000' },
    ],
  },
  {
    name: 'claude-opus-4-1',
    display_name: 'Claude Opus 4.1',
    vendor: 'anthropic',
    context_limit: 200_000,
    max_output: 32_000,
    capabilities: { tools: true, vision: true, reasoning: true, caching: true },
    modalities: { input: ['text', 'image'], output: ['text'] },
    pricing: {
      input_per_mtok: '15.00000000',
      output_per_mtok: '75.00000000',
      cache_read_per_mtok: '1.50000000',
      cache_write_per_mtok: '18.75000000',
    },
  },
  {
    name: 'gpt-4.1',
    display_name: 'GPT-4.1',
    vendor: 'openai',
    context_limit: 1_047_576,
    max_output: 32_768,
    capabilities: { tools: true, vision: true },
    modalities: { input: ['text', 'image'], output: ['text'] },
    pricing: {
      input_per_mtok: '2.00000000',
      output_per_mtok: '8.00000000',
      cache_read_per_mtok: '0.50000000',
      cache_write_per_mtok: null,
    },
    offers: [{ merchant: null, price: '0.00200000' }],
  },
  {
    name: 'gpt-4.1-mini',
    display_name: 'GPT-4.1 mini',
    vendor: 'openai',
    context_limit: 1_047_576,
    max_output: 32_768,
    capabilities: { tools: true, vision: true },
    modalities: { input: ['text', 'image'], output: ['text'] },
    pricing: {
      input_per_mtok: '0.40000000',
      output_per_mtok: '1.60000000',
      cache_read_per_mtok: '0.10000000',
      cache_write_per_mtok: null,
    },
  },
  {
    name: 'deepseek-chat',
    display_name: 'DeepSeek Chat',
    vendor: 'deepseek',
    context_limit: 131_072,
    max_output: 8_192,
    capabilities: { tools: true },
    modalities: { input: ['text'], output: ['text'] },
    pricing: {
      input_per_mtok: '0.27000000',
      output_per_mtok: '1.10000000',
      cache_read_per_mtok: null,
      cache_write_per_mtok: null,
    },
  },
  {
    name: 'example-embed',
    display_name: 'Example Embed',
    vendor: 'example',
    context_limit: 8_192,
    max_output: 0,
    capabilities: {},
    modalities: { input: ['text'], output: ['embedding'] },
    pricing: null,
  },
]

let nextKeyId = 4

const keys: ApiKey[] = [
  {
    id: 1,
    name: 'Production',
    ...mask('sk-placeholder000000000000000000000'),
    status: 'active',
    quota_limit: null,
    quota_used: '42.11840000',
    rate_limit_rpm: 600,
    expires_at: null,
    created_at: new Date(Date.now() - 42 * DAY_MS).toISOString(),
    last_used_at: new Date(Date.now() - 12 * 60_000).toISOString(),
  },
  {
    id: 2,
    name: 'CI',
    ...mask('sk-placeholder111111111111111111111'),
    status: 'disabled',
    quota_limit: '20.00000000',
    quota_used: '3.21370000',
    rate_limit_rpm: 60,
    expires_at: null,
    created_at: new Date(Date.now() - 21 * DAY_MS).toISOString(),
    last_used_at: new Date(Date.now() - 3 * DAY_MS).toISOString(),
  },
  {
    id: 3,
    name: 'Staging',
    ...mask('sk-placeholder222222222222222222222'),
    status: 'revoked',
    quota_limit: '5.00000000',
    quota_used: '0.00000000',
    rate_limit_rpm: null,
    expires_at: null,
    created_at: new Date(Date.now() - 60 * DAY_MS).toISOString(),
    last_used_at: null,
  },
]

const user: User = {
  id: 1,
  username: 'demo',
  display_name: '演示账号',
  email: 'user@example.com',
  status: 'active',
  // 演示数据默认给 admin：运营页是这次要展示的东西，只看得见「用户」就没法看。
  role: 'admin',
  created_at: new Date(Date.now() - 90 * DAY_MS).toISOString(),
}

const wallet: Wallet = {
  asset: 'USD',
  balance: '128.45670000',
  frozen: '1.20000000',
  updated_at: new Date().toISOString(),
}

function readSession(): string | null {
  try {
    return sessionStorage.getItem(SESSION_KEY)
  } catch {
    return null
  }
}

function writeSession(username: string | null): void {
  try {
    if (username === null) sessionStorage.removeItem(SESSION_KEY)
    else sessionStorage.setItem(SESSION_KEY, username)
  } catch {
    // 隐私模式下 sessionStorage 可能不可用，登录态就只留在这一个页面里。
  }
}

function requireSession(): User {
  if (readSession() === null) {
    throw new ApiError(401, 'unauthenticated', 'not signed in')
  }
  return user
}

interface DailyRow {
  bucket: string
  requests: number
  prompt: number
  completion: number
  total_tokens: number
  cost: number
}

function dailyRows(): DailyRow[] {
  const today = new Date()
  return DAILY.map((day, index) => ({
    bucket: dateOnly(addDays(today, index - (DAILY.length - 1))),
    requests: day.requests,
    prompt: day.prompt,
    completion: day.completion,
    total_tokens: day.prompt + day.completion,
    cost: day.cost,
  }))
}

/** 只取日期部分：页面传来的可能是 `YYYY-MM-DD`，也可能是 RFC3339。 */
function dayOf(value: string): string {
  return value.slice(0, 10)
}

/** 与服务端同一个默认：没给 `from` / `to` 时是最近 30 天。 */
function resolveWindow(query: UsageQuery = {}): { from: string; to: string } {
  const today = new Date()
  const to = query.to ? dayOf(query.to) : dateOnly(addDays(today, 1))
  const from = query.from ? dayOf(query.from) : dateOnly(addDays(today, -29))
  return { from, to }
}

function inWindow(bucket: string, from: string, to: string): boolean {
  return bucket >= from && bucket < to
}

function filterRange(rows: DailyRow[], from: string, to: string) {
  return rows.filter((row) => inWindow(row.bucket, from, to))
}

/** 把行里的 token 折成五个槽位；缓存与推理在演示数据里固定占一小部分。 */
function rowTokens(prompt: number, completion: number): Tokens {
  return tokens(prompt, completion, Math.round(prompt * 0.18), Math.round(prompt * 0.04), 0)
}

function summarize(rows: DailyRow[]): UsageTotals {
  const prompt = rows.reduce((sum, day) => sum + day.prompt, 0)
  const completion = rows.reduce((sum, day) => sum + day.completion, 0)
  const requests = rows.reduce((sum, day) => sum + day.requests, 0)
  const cost = rows.reduce((sum, day) => sum + day.cost, 0)
  return totals(cost, requests, rowTokens(prompt, completion))
}

const REQUEST_MODELS = ['claude-sonnet-4-5', 'gpt-4.1', 'deepseek-chat', 'gpt-4.1-mini']
const BILLING_PATHS = ['measured', 'estimated', 'fallback']

function requests(): UsageRequest[] {
  const rows: UsageRequest[] = []
  const now = Date.now()
  for (let i = 0; i < 36; i += 1) {
    const model = REQUEST_MODELS[i % REQUEST_MODELS.length]
    const failed = i % 11 === 4
    const pending = i % 17 === 9
    const key = keys[i % 2]
    const prompt = 320 + ((i * 137) % 1_800)
    const completion = 180 + ((i * 89) % 1_200)
    const startedAt = new Date(now - i * 37 * 60_000)
    rows.push({
      id: 40_000 + i,
      request_id: `req_${String(20251_000 + i)}`,
      model,
      key: { id: key.id, name: key.name },
      status: failed ? 'failed' : pending ? 'pending' : 'settled',
      cost: pending ? money(0) : money(prompt * 0.000003 + completion * 0.000015),
      currency: 'USD',
      tokens: pending ? tokens(0, 0) : rowTokens(prompt, completion),
      billing_path: pending ? null : BILLING_PATHS[i % BILLING_PATHS.length],
      streamed: i % 3 === 0,
      protocol: i % 2 === 0 ? 'openai' : 'anthropic',
      duration_ms: pending ? null : 640 + ((i * 173) % 3_200),
      started_at: startedAt.toISOString(),
      settled_at: pending ? null : new Date(now - i * 36 * 60_000).toISOString(),
    })
  }
  return rows
}

function entries(): WalletEntry[] {
  const rows: WalletEntry[] = []
  const now = Date.now()
  const templates: Array<[WalletEntry['type'], number, string, number | null, string]> = [
    ['consume', -2.4183, 'claude-sonnet-4-5 · 今日结算', 40_001, 'USD'],
    ['consume', -1.9027, 'gpt-4.1 · 今日结算', 40_002, 'USD'],
    ['topup', 100, '运营入账', null, 'USD'],
    ['consume', -4.1174, 'deepseek-chat · 昨日结算', 40_003, 'USD'],
    ['refund', 0.3211, '重复计费退回', 40_004, 'USD'],
    ['adjustment', -0.5, '运营调整（人民币）', null, 'CNY'],
  ]
  templates.forEach(([type, amount, memo, billingRequestID, asset], index) => {
    rows.push({
      id: 1_000 + index,
      type,
      amount: money(amount),
      memo,
      asset,
      billing_request_id: billingRequestID,
      created_at: new Date(now - index * 19 * 3_600_000).toISOString(),
    })
  })
  return rows
}

function bucketStart(bucket: string): string {
  return `${bucket}T00:00:00${SITE_OFFSET}`
}

/* ── 运营资源 ─────────────────────────────────────────────────────── */

/** 服务端那两句固定文案；界面原样显示，不自己编。 */
const PERMISSION_DENIED = 'you do not have permission to perform this action'
const CHANNEL_NOT_FOUND = 'channel not found'

const ago = (hours: number) => new Date(Date.now() - hours * 3_600_000).toISOString()

const OPS_CHANNELS: Channel[] = [
  {
    id: 1,
    owner_user_id: 0,
    name: 'platform-anthropic',
    type: 'anthropic',
    base_url: 'https://api.example.com/anthropic',
    status: 'enabled',
    models: ['claude-sonnet-4-5'],
    weight: 1,
    priority: 10,
    setting: {},
    credential_keys: ['k-placeholder'],
    credential_hint: 'abcd',
    deleted_at: null,
    created_at: ago(720),
    updated_at: ago(12),
  },
  {
    id: 2,
    owner_user_id: 2,
    name: 'acme-openai',
    type: 'openai',
    base_url: 'https://api.example.com/v1',
    status: 'enabled',
    models: ['gpt-4.1'],
    weight: 2,
    priority: 5,
    setting: {},
    credential_keys: ['k-placeholder'],
    credential_hint: 'wxyz',
    deleted_at: null,
    created_at: ago(400),
    updated_at: ago(30),
  },
  {
    id: 3,
    owner_user_id: 1,
    name: 'demo-self',
    type: 'openai',
    base_url: 'https://api.example.com/self',
    status: 'disabled',
    models: ['gpt-4.1'],
    weight: 1,
    priority: 0,
    setting: {},
    credential_keys: ['k-placeholder'],
    credential_hint: 'demo',
    deleted_at: null,
    created_at: ago(90),
    updated_at: ago(5),
  },
]

const OPS_MODELS: OpsModel[] = [
  {
    id: 1,
    canonical_name: 'claude-sonnet-4-5',
    display_name: 'Claude Sonnet 4.5',
    vendor: 'anthropic',
    family: 'claude',
    context_limit: 200_000,
    input_limit: 0,
    output_limit: 0,
    capabilities: {
      reasoning: true,
      tools: true,
      vision: true,
      audio: null,
      cache: true,
      batch: null,
      endpoints: ['chat', 'messages'],
    },
    modalities_input: ['text', 'image'],
    modalities_output: ['text'],
    reasoning_options: [],
    canonical_model_id: '',
    knowledge_cutoff: '2025-03-01',
    release_date: '2025-09-29',
    raw: {},
    updated_at: ago(48),
  },
  {
    id: 2,
    canonical_name: 'gpt-4.1',
    display_name: 'GPT-4.1',
    vendor: 'openai',
    family: 'gpt',
    context_limit: 1_000_000,
    input_limit: 0,
    output_limit: 32_768,
    capabilities: {
      reasoning: false,
      tools: true,
      vision: true,
      audio: null,
      cache: null,
      batch: null,
      endpoints: ['chat'],
    },
    modalities_input: ['text', 'image'],
    modalities_output: ['text'],
    reasoning_options: [],
    canonical_model_id: '',
    knowledge_cutoff: null,
    release_date: null,
    raw: {},
    updated_at: ago(72),
  },
]

const OPS_ALIASES: ChannelAlias[] = [
  {
    id: 1,
    channel_id: 1,
    upstream_model: 'claude-sonnet-4-5-20250929',
    canonical_name: 'claude-sonnet-4-5',
    overrides: {},
    created_at: ago(600),
  },
  {
    id: 2,
    channel_id: 2,
    upstream_model: 'gpt-4.1-2025-04-14',
    canonical_name: 'gpt-4.1',
    overrides: { reasoning_effort: 'medium' },
    created_at: ago(300),
  },
]

const OPS_RULES: PricingRule[] = [
  {
    id: 1,
    scope: 'platform',
    owner_user_id: 0,
    channel_id: 0,
    name: 'default-prices',
    model_patterns: ['*'],
    expr: 'price = input_tokens * 0.000003 + output_tokens * 0.000015',
    priority: 0,
    effective_from: ago(24 * 365),
    effective_to: null,
    enabled: true,
    created_by: 1,
    created_at: ago(24 * 365),
    updated_at: ago(96),
    declared_keys: ['input_tokens', 'output_tokens'],
  },
  {
    id: 2,
    scope: 'merchant',
    owner_user_id: 2,
    channel_id: 0,
    name: 'acme-discount',
    model_patterns: ['gpt-4.1'],
    expr: 'price = tokens_in * 0.000002 + tokens_out * 0.000010',
    priority: 10,
    effective_from: ago(240),
    effective_to: null,
    enabled: false,
    created_by: 2,
    created_at: ago(240),
    updated_at: ago(20),
    declared_keys: ['tokens_in', 'tokens_out'],
    expr_error: 'unknown variable tokens_in',
  },
]

const OPS_COSTS: PricingCost[] = [
  {
    id: 1,
    channel_id: 1,
    model_patterns: ['claude-*'],
    expr: 'cost = input_tokens * 0.000002 + output_tokens * 0.000012',
    priority: 0,
    effective_from: ago(24 * 200),
    effective_to: null,
    created_at: ago(24 * 200),
    declared_keys: ['input_tokens', 'output_tokens'],
  },
]

function opsID(): number {
  return 1_000 + Math.floor(Math.random() * 9_000)
}

/**
 * 一组运营资源的替身。
 *
 * admin 拿全集；merchant 只通过 `channelsOf` 取走渠道那一半——模型、别名与
 * 计价没有商家前缀的路由。渠道按服务端的判定演戏：merchant 只看得到、也只
 * 改得了自己的渠道，别人的渠道与不存在的渠道都回 404（不透露「存在但不是你的」）。
 */
function mockOpsClient(scope: 'admin' | 'merchant'): OpsClient & MerchantPricingClient {
  const isAdmin = scope === 'admin'
  const denied = () => new ApiError(403, 'forbidden', PERMISSION_DENIED)

  const visibleChannels = () =>
    OPS_CHANNELS.filter(
      (channel) =>
        channel.deleted_at === null && (isAdmin || channel.owner_user_id === user.id),
    )

  const ownChannel = (id: number): Channel => {
    const channel = visibleChannels().find((item) => item.id === id)
    if (!channel) throw new ApiError(404, 'not_found', CHANNEL_NOT_FOUND)
    return channel
  }

  const requireAdmin = () => {
    if (!isAdmin) throw denied()
  }

  const channelInputError = (input: ChannelCreateInput): void => {
    if (!input.name?.trim()) throw new ApiError(400, 'invalid_request', 'name is required')
    if (!input.type?.trim()) throw new ApiError(400, 'invalid_request', 'type is required')
    if (!input.base_url?.trim()) throw new ApiError(400, 'invalid_request', 'base_url is required')
    if (!input.credential?.trim())
      throw new ApiError(400, 'invalid_request', 'credential is required')
    if (!input.models || input.models.length === 0)
      throw new ApiError(400, 'invalid_request', 'at least one model is required')
  }

  return {
    async listChannels() {
      await delay()
      requireSession()
      return visibleChannels().map((channel) => ({ ...channel }))
    },

    async getChannel(id) {
      await delay()
      requireSession()
      return { ...ownChannel(id) }
    },

    async createChannel(input) {
      await delay()
      requireSession()
      channelInputError(input)
      const created: Channel = {
        id: opsID(),
        owner_user_id: isAdmin ? (input.owner_user_id ?? 0) : user.id,
        name: input.name.trim(),
        type: input.type.trim(),
        base_url: input.base_url.trim(),
        status: 'enabled',
        models: [...input.models],
        weight: input.weight ?? 1,
        priority: input.priority ?? 0,
        setting: input.setting ?? {},
        credential_keys: ['k-placeholder'],
        credential_hint: input.credential.slice(-4),
        deleted_at: null,
        created_at: new Date().toISOString(),
        updated_at: new Date().toISOString(),
      }
      OPS_CHANNELS.push(created)
      return created.id
    },

    async updateChannel(id, input) {
      await delay()
      requireSession()
      const channel = ownChannel(id)
      if (
        (input.name !== undefined && !input.name.trim()) ||
        (input.type !== undefined && !input.type.trim()) ||
        (input.base_url !== undefined && !input.base_url.trim()) ||
        (input.models !== undefined && input.models.length === 0)
      ) {
        throw new ApiError(400, 'invalid_request', 'the field cannot be empty')
      }
      Object.assign(channel, {
        ...(input.name !== undefined ? { name: input.name.trim() } : {}),
        ...(input.type !== undefined ? { type: input.type.trim() } : {}),
        ...(input.base_url !== undefined ? { base_url: input.base_url.trim() } : {}),
        ...(input.models !== undefined ? { models: [...input.models] } : {}),
        ...(input.weight !== undefined ? { weight: input.weight } : {}),
        ...(input.priority !== undefined ? { priority: input.priority } : {}),
        ...(input.setting !== undefined ? { setting: input.setting } : {}),
        ...(input.status !== undefined ? { status: input.status } : {}),
        updated_at: new Date().toISOString(),
      })
      if (input.credential !== undefined && input.credential !== '') {
        channel.credential_hint = input.credential.slice(-4)
        channel.credential_keys = ['k-placeholder']
      }
    },

    async listModels() {
      await delay()
      requireSession()
      return OPS_MODELS.map((model) => ({ ...model }))
    },

    async createModel(input: OpsModelCreateInput) {
      await delay()
      requireSession()
      requireAdmin()
      if (!input.canonical_name.trim()) {
        throw new ApiError(400, 'invalid_request', 'canonical_name is required')
      }
      if (OPS_MODELS.some((model) => model.canonical_name === input.canonical_name)) {
        throw new ApiError(409, 'conflict', 'model already exists')
      }
      const created: OpsModel = {
        ...OPS_MODELS[1],
        ...input,
        id: opsID(),
        canonical_name: input.canonical_name,
        capabilities: { ...OPS_MODELS[1].capabilities, ...(input.capabilities ?? {}) },
        updated_at: new Date().toISOString(),
      } as OpsModel
      OPS_MODELS.push(created)
      return created.id
    },

    async updateModel(name, input: OpsModelInput) {
      await delay()
      requireSession()
      requireAdmin()
      const model = OPS_MODELS.find((item) => item.canonical_name === name)
      if (!model) throw new ApiError(404, 'not_found', 'model not found')
      const { capabilities, ...rest } = input
      Object.assign(model, rest, {
        ...(capabilities
          ? { capabilities: { ...model.capabilities, ...capabilities } }
          : {}),
        updated_at: new Date().toISOString(),
      })
      if (input.knowledge_cutoff === '') model.knowledge_cutoff = null
      if (input.release_date === '') model.release_date = null
    },

    async deleteModel(name) {
      await delay()
      requireSession()
      requireAdmin()
      const index = OPS_MODELS.findIndex((item) => item.canonical_name === name)
      if (index < 0) throw new ApiError(404, 'not_found', 'model not found')
      OPS_MODELS.splice(index, 1)
    },

    async listAliases(channelId) {
      await delay()
      requireSession()
      return OPS_ALIASES.filter(
        (alias) =>
          (channelId === undefined || alias.channel_id === channelId) &&
          (isAdmin || ownChannel(alias.channel_id).id === alias.channel_id),
      ).map((alias) => ({ ...alias }))
    },

    async createAlias(input) {
      await delay()
      requireSession()
      requireAdmin()
      if (!input.channel_id) throw new ApiError(400, 'invalid_request', 'channel_id is required')
      if (!input.upstream_model.trim() || !input.canonical_name.trim()) {
        throw new ApiError(400, 'invalid_request', 'upstream_model and canonical_name are required')
      }
      const created: ChannelAlias = {
        id: opsID(),
        channel_id: input.channel_id,
        upstream_model: input.upstream_model.trim(),
        canonical_name: input.canonical_name.trim(),
        overrides: input.overrides ?? {},
        created_at: new Date().toISOString(),
      }
      OPS_ALIASES.push(created)
      return created.id
    },

    async updateAlias(id, input: AliasUpdateInput) {
      await delay()
      requireSession()
      requireAdmin()
      const alias = OPS_ALIASES.find((item) => item.id === id)
      if (!alias) throw new ApiError(404, 'not_found', 'alias not found')
      if (input.upstream_model === '') {
        throw new ApiError(400, 'invalid_request', 'upstream_model cannot be empty')
      }
      Object.assign(alias, input)
    },

    async deleteAlias(id) {
      await delay()
      requireSession()
      requireAdmin()
      const index = OPS_ALIASES.findIndex((item) => item.id === id)
      if (index < 0) throw new ApiError(404, 'not_found', 'alias not found')
      OPS_ALIASES.splice(index, 1)
    },

    async listRules() {
      await delay()
      requireSession()
      const rows = isAdmin ? OPS_RULES : OPS_RULES.filter((rule) => rule.owner_user_id === user.id)
      return rows.map((rule) => ({ ...rule }))
    },

    async getRule(id) {
      await delay()
      requireSession()
      const rule = OPS_RULES.find((item) => item.id === id)
      // 别人的、平台的、不存在的都是同一句 404：能区分「不是你的」就等于
      // 告诉调用方这一行存在。
      if (!rule || (!isAdmin && rule.owner_user_id !== user.id)) {
        throw new ApiError(404, 'not_found', 'not found')
      }
      return { ...rule }
    },

    async createRule(input: RuleCreateInput) {
      await delay()
      requireSession()
      if (!input.name.trim()) throw new ApiError(400, 'invalid_request', 'name is required')
      if (!input.expr.trim()) throw new ApiError(400, 'invalid_request', 'expr is required')
      if (input.model_patterns.length === 0) {
        throw new ApiError(400, 'invalid_request', 'at least one model pattern is required')
      }
      // 商家建规则时作用域与归属由会话决定，渠道这一维不存在。
      const created: PricingRule = {
        id: opsID(),
        scope: isAdmin ? input.scope : 'merchant',
        owner_user_id: isAdmin ? (input.owner_user_id ?? 0) : user.id,
        channel_id: isAdmin ? (input.channel_id ?? 0) : 0,
        name: input.name.trim(),
        model_patterns: [...input.model_patterns],
        expr: input.expr,
        priority: input.priority ?? 0,
        effective_from: input.effective_from ?? new Date().toISOString(),
        effective_to: input.effective_to ?? null,
        enabled: input.enabled ?? true,
        created_by: user.id,
        created_at: new Date().toISOString(),
        updated_at: new Date().toISOString(),
        declared_keys: [],
      }
      OPS_RULES.push(created)
      return created.id
    },

    async updateRule(id, input: RuleUpdateInput) {
      await delay()
      requireSession()
      const rule = OPS_RULES.find((item) => item.id === id)
      if (!rule || (!isAdmin && rule.owner_user_id !== user.id)) {
        throw new ApiError(404, 'not_found', 'not found')
      }
      if (!isAdmin && (input.scope !== undefined || input.owner_user_id !== undefined || input.channel_id !== undefined)) {
        throw new ApiError(400, 'invalid_request', 'scope, owner_user_id and channel_id are set by the server')
      }
      if (input.expr !== undefined && !input.expr.trim()) {
        throw new ApiError(400, 'invalid_request', 'expr is required')
      }
      if (input.model_patterns !== undefined && input.model_patterns.length === 0) {
        throw new ApiError(400, 'invalid_request', 'at least one model pattern is required')
      }
      const { clear_effective_to: clearTo, ...rest } = input
      Object.assign(rule, rest, { updated_at: new Date().toISOString() })
      if (clearTo) rule.effective_to = null
    },

    async listCosts() {
      await delay()
      requireSession()
      return OPS_COSTS.map((cost) => ({ ...cost }))
    },

    async createCost(input: CostCreateInput) {
      await delay()
      requireSession()
      requireAdmin()
      if (!input.channel_id) throw new ApiError(400, 'invalid_request', 'channel_id is required')
      if (!input.expr.trim()) throw new ApiError(400, 'invalid_request', 'expr is required')
      if (input.model_patterns.length === 0) {
        throw new ApiError(400, 'invalid_request', 'at least one model pattern is required')
      }
      const created: PricingCost = {
        id: opsID(),
        channel_id: input.channel_id,
        model_patterns: [...input.model_patterns],
        expr: input.expr,
        priority: input.priority ?? 0,
        effective_from: input.effective_from ?? new Date().toISOString(),
        effective_to: input.effective_to ?? null,
        created_at: new Date().toISOString(),
        declared_keys: [],
      }
      OPS_COSTS.push(created)
      return created.id
    },

    async updateCost(id, input: CostUpdateInput) {
      await delay()
      requireSession()
      requireAdmin()
      const cost = OPS_COSTS.find((item) => item.id === id)
      if (!cost) throw new ApiError(404, 'not_found', 'cost rule not found')
      if (input.expr !== undefined && !input.expr.trim()) {
        throw new ApiError(400, 'invalid_request', 'expr is required')
      }
      const { clear_effective_to: clearTo, ...rest } = input
      Object.assign(cost, rest)
      if (clearTo) cost.effective_to = null
    },
  }
}

/** merchant 前缀下的两组路由：自有渠道与自己的售价规则，没有成本。 */
function merchantClientOf(
  client: OpsClient & MerchantPricingClient,
): ChannelOpsClient & MerchantPricingClient {
  const {
    listChannels,
    getChannel,
    createChannel,
    updateChannel,
    listRules,
    getRule,
    createRule,
    updateRule,
  } = client
  return {
    listChannels,
    getChannel,
    createChannel,
    updateChannel,
    listRules,
    getRule,
    createRule,
    updateRule,
  }
}

export class MockApiClient implements ApiClient {
  readonly mode = 'mock' as const
  readonly admin: OpsClient = mockOpsClient('admin')
  readonly merchant: ChannelOpsClient & MerchantPricingClient = merchantClientOf(mockOpsClient('merchant'))

  setCsrfToken(): void {
    // mock 不做 CSRF 校验；保留方法是为了与 HTTP 实现同形。
  }

  async meta(): Promise<Meta> {
    await delay()
    return {
      site_name: 'larkax',
      api_base_url: 'https://api.example.com/v1',
      currency: 'USD',
      timezone: SITE_TZ,
      locales: ['en', 'zh'],
      features: { registration: false, password_reset: 'admin', topup: false },
    }
  }

  async login(username: string, password: string): Promise<SessionResponse> {
    await delay()
    if (!username.trim() || !password) {
      throw new ApiError(401, 'invalid_credentials', 'Invalid username or password.')
    }
    user.username = username.trim()
    if (!user.display_name) user.display_name = username.trim()
    writeSession(user.username)
    return {
      user: { ...user },
      csrf_token: MOCK_CSRF,
      expires_at: new Date(Date.now() + 7 * DAY_MS).toISOString(),
    }
  }

  async logout(): Promise<void> {
    await delay()
    writeSession(null)
  }

  async session(): Promise<SessionResponse | null> {
    await delay()
    if (readSession() === null) return null
    return {
      user: { ...user },
      csrf_token: MOCK_CSRF,
      expires_at: new Date(Date.now() + 7 * DAY_MS).toISOString(),
    }
  }

  async me(): Promise<User> {
    await delay()
    return { ...requireSession() }
  }

  async updateMe(input: { display_name?: string | null; email?: string | null }): Promise<User> {
    await delay()
    requireSession()
    if (input.display_name !== undefined) user.display_name = input.display_name
    if (input.email !== undefined) user.email = input.email
    return { ...user }
  }

  async changePassword(currentPassword: string, newPassword: string): Promise<void> {
    await delay()
    requireSession()
    if (!currentPassword) {
      throw new ApiError(400, 'invalid_request', 'current password is required')
    }
    if (newPassword.length < 12 || newPassword.length > 128) {
      throw new ApiError(400, 'weak_password', 'password must be 12-128 characters')
    }
  }

  async listKeys(query: KeyListQuery = {}): Promise<ListResponse<ApiKey>> {
    await delay()
    requireSession()
    let items = keys.slice()
    if (query.status) items = items.filter((key) => key.status === query.status)
    const offset = Number.parseInt(query.cursor ?? '0', 10) || 0
    const limit = query.limit ?? 50
    const page = items.slice(offset, offset + limit)
    return {
      items: page.map((key) => ({ ...key })),
      next_cursor: offset + limit < items.length ? String(offset + limit) : null,
    }
  }

  async createKey(input: KeyCreateInput): Promise<CreatedKey> {
    await delay()
    requireSession()
    if (input.quota_limit !== undefined) {
      const limit = Number.parseFloat(input.quota_limit)
      if (!(limit > 0)) {
        throw new ApiError(400, 'invalid_request', 'quota_limit 必须大于 0；不限就不给这个字段')
      }
    }
    const secret = randomSecret()
    const created: ApiKey = {
      id: nextKeyId,
      name: input.name?.trim() ?? '',
      ...mask(secret),
      status: 'active',
      quota_limit: input.quota_limit ?? null,
      quota_used: '0.00000000',
      rate_limit_rpm: input.rate_limit_rpm ?? null,
      expires_at: input.expires_at ?? null,
      created_at: new Date().toISOString(),
      last_used_at: null,
    }
    nextKeyId += 1
    keys.push(created)
    return { key: { ...created }, secret }
  }

  async getKey(id: number): Promise<ApiKey> {
    await delay()
    requireSession()
    const key = keys.find((candidate) => candidate.id === id)
    if (!key) throw new ApiError(404, 'not_found', 'Not found.')
    return { ...key }
  }

  async updateKey(id: number, input: KeyUpdateInput): Promise<ApiKey> {
    await delay()
    requireSession()
    const key = keys.find((candidate) => candidate.id === id)
    if (!key) throw new ApiError(404, 'not_found', 'Not found.')
    if (key.status === 'revoked') {
      throw new ApiError(409, 'conflict', 'This key has been revoked and can no longer be modified.')
    }
    if (input.name !== undefined) key.name = input.name
    if (input.quota_limit !== undefined) key.quota_limit = input.quota_limit
    if (input.rate_limit_rpm !== undefined) key.rate_limit_rpm = input.rate_limit_rpm
    if (input.expires_at !== undefined) key.expires_at = input.expires_at
    if (input.status !== undefined) key.status = input.status
    return { ...key }
  }

  async revokeKey(id: number): Promise<ApiKey> {
    await delay()
    requireSession()
    const key = keys.find((candidate) => candidate.id === id)
    if (!key) throw new ApiError(404, 'not_found', 'Not found.')
    key.status = 'revoked'
    return { ...key }
  }

  async models(): Promise<ModelsResponse> {
    await delay()
    return {
      currency: 'USD',
      reference_at: new Date().toISOString(),
      items: MODELS.map((model) => ({
        ...model,
        capabilities: { ...model.capabilities },
        modalities: { input: [...model.modalities.input], output: [...model.modalities.output] },
        pricing: model.pricing && { ...model.pricing },
      })),
    }
  }

  async usageSummary(query: UsageQuery = {}): Promise<UsageSummary> {
    await delay()
    requireSession()
    const { from, to } = resolveWindow(query)
    const rows = filterRange(dailyRows(), from, to)
    const summary = summarize(rows)
    const paths: UsagePath[] = [
      { path: 'measured', cost: money(Number.parseFloat(summary.cost) * 0.74), requests: Math.round(summary.requests * 0.72) },
      { path: 'estimated', cost: money(Number.parseFloat(summary.cost) * 0.23), requests: Math.round(summary.requests * 0.24) },
      {
        path: 'fallback',
        cost: money(Number.parseFloat(summary.cost) * 0.03),
        requests: summary.requests - Math.round(summary.requests * 0.72) - Math.round(summary.requests * 0.24),
      },
    ]
    return { from, to, currency: 'USD', totals: summary, billing_paths: paths }
  }

  async usageSeries(query: UsageSeriesQuery = {}): Promise<UsageSeries> {
    await delay()
    requireSession()
    const { from, to } = resolveWindow(query)
    const rows = filterRange(dailyRows(), from, to)
    return {
      interval: query.interval ?? 'day',
      tz: SITE_TZ,
      points: rows.map((row) => ({
        start: bucketStart(row.bucket),
        cost: money(row.cost),
        requests: row.requests,
        tokens: rowTokens(row.prompt, row.completion),
      })),
    }
  }

  async usageBreakdown(query: UsageBreakdownQuery): Promise<UsageBreakdown> {
    await delay()
    requireSession()
    const { from, to } = resolveWindow(query)
    const rows = filterRange(dailyRows(), from, to)
    const summary = summarize(rows)
    if (query.group_by === 'key') {
      const share = [0.62, 0.27, 0.11]
      const items: UsageGroup[] = keys.slice(0, 3).map((key, index) => ({
        key_id: key.id,
        key_name: key.name,
        cost: money(Number.parseFloat(summary.cost) * share[index]),
        requests: Math.round(summary.requests * share[index]),
        tokens: rowTokens(
          Math.round(summary.tokens.input * share[index]),
          Math.round(summary.tokens.output * share[index]),
        ),
      }))
      return { group_by: 'key', items, other: totals(0, 0, tokens(0, 0)) }
    }
    const shares = [0.47, 0.24, 0.16, 0.13]
    const items: UsageGroup[] = MODELS.filter((model) => model.pricing !== null)
      .slice(0, 4)
      .map((model, index) => ({
        model: model.name,
        cost: money(Number.parseFloat(summary.cost) * shares[index]),
        requests: Math.round(summary.requests * shares[index]),
        tokens: rowTokens(
          Math.round(summary.tokens.input * shares[index]),
          Math.round(summary.tokens.output * shares[index]),
        ),
      }))
    return { group_by: 'model', items, other: totals(0, 0, tokens(0, 0)) }
  }

  async usageRequests(query: UsageRequestsQuery = {}): Promise<ListResponse<UsageRequest>> {
    await delay()
    requireSession()
    const { from, to } = resolveWindow(query)
    let items = requests()
    if (query.status) items = items.filter((row) => row.status === query.status)
    if (query.key_id !== undefined) items = items.filter((row) => row.key?.id === query.key_id)
    if (query.model) items = items.filter((row) => row.model === query.model)
    items = items.filter((row) => inWindow(dayOf(row.started_at), from, to))
    const offset = Number.parseInt(query.cursor ?? '0', 10) || 0
    const limit = query.limit ?? 20
    return {
      items: items.slice(offset, offset + limit),
      next_cursor: offset + limit < items.length ? String(offset + limit) : null,
    }
  }

  async wallet(): Promise<Wallet> {
    await delay()
    requireSession()
    return { ...wallet }
  }

  async walletEntries(query: WalletEntriesQuery = {}): Promise<ListResponse<WalletEntry>> {
    await delay()
    requireSession()
    let all = entries()
    const { from, to } = resolveWindow(query)
    all = all.filter((entry) => inWindow(dayOf(entry.created_at), from, to))
    if (query.type && query.type.length > 0) {
      all = all.filter((entry) => query.type?.includes(entry.type))
    }
    const offset = Number.parseInt(query.cursor ?? '0', 10) || 0
    const limit = query.limit ?? 50
    return {
      items: all.slice(offset, offset + limit),
      next_cursor: offset + limit < all.length ? String(offset + limit) : null,
    }
  }
}
