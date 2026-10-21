import type {
  AliasCreateInput,
  AliasUpdateInput,
  ApiClient,
  ApiErrorBody,
  ApiKey,
  Channel,
  ChannelAlias,
  ChannelOpsClient,
  ChannelCreateInput,
  ChannelUpdateInput,
  CostCreateInput,
  CostUpdateInput,
  CreatedKey,
  KeyCreateInput,
  KeyListQuery,
  KeyUpdateInput,
  ListResponse,
  Meta,
  ModelDetailResponse,
  ModelsResponse,
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
  UsageBreakdown,
  UsageBreakdownQuery,
  UsageQuery,
  UsageRequest,
  UsageRequestsQuery,
  UsageSeries,
  UsageSeriesQuery,
  UsageSummary,
  User,
  Wallet,
  WalletEntriesQuery,
  WalletEntry,
} from './types'

export class ApiError extends Error {
  readonly status: number
  readonly code: string
  readonly requestId: string

  constructor(status: number, code: string, message: string, requestId = '') {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
    this.requestId = requestId
  }
}

type QueryValue = string | number | boolean | null | undefined

/**
 * 拼查询串。
 *
 * 只接受标量或标量数组：`type` 是唯一一个可重复参数（钱包分录的筛选），
 * 数组的每一项各出现一次，不能折成逗号分隔——服务端读的是 `q["type"]`。
 */
function buildUrl(
  base: string,
  path: string,
  query?: Record<string, QueryValue | QueryValue[]>,
): string {
  const params = new URLSearchParams()
  for (const [key, value] of Object.entries(query ?? {})) {
    if (value === undefined || value === null || value === '') continue
    const values = Array.isArray(value) ? value : [value]
    for (const item of values) {
      if (item === undefined || item === null || item === '') continue
      params.append(key, String(item))
    }
  }
  const suffix = params.toString()
  return `${base}${path}${suffix ? `?${suffix}` : ''}`
}

const WRITE_METHODS = new Set(['POST', 'PUT', 'PATCH', 'DELETE'])

export interface HttpApiOptions {
  baseUrl?: string
  fetchImpl?: typeof fetch
}

export class HttpApiClient implements ApiClient {
  readonly mode = 'http' as const
  readonly admin: OpsClient
  readonly merchant: ChannelOpsClient & MerchantPricingClient
  private readonly baseUrl: string
  private readonly doFetch: typeof fetch
  private csrfToken: string | null = null

  constructor(options: HttpApiOptions = {}) {
    this.baseUrl = (options.baseUrl ?? '/api/v1').replace(/\/$/, '')
    this.doFetch = options.fetchImpl ?? globalThis.fetch.bind(globalThis)
    this.admin = this.ops('/admin')
    this.merchant = { ...this.channelOps('/merchant'), ...this.merchantPricing('/merchant') }
  }

  /**
   * 一组运营资源。
   *
   * 两个前缀的路径与 JSON 完全同形，差别只在服务端的判定范围；前端不按
   * 自己的角色去拼路径，也不自行降级——越权显示服务端给的那句话。
   * `merchant` 只有渠道一组路由，所以它拿的是 `channelOps` 那一半。
   */
  private ops(prefix: string): OpsClient {
    const idBody = (body: { id: number }) => body.id
    return {
      ...this.channelOps(prefix),

      listModels: () => this.request<OpsModel[]>(`${prefix}/models`),
      createModel: (input: OpsModelCreateInput) =>
        this.request<{ id: number }>(`${prefix}/models`, { method: 'POST', body: input }).then(idBody),
      updateModel: async (name, input: OpsModelInput) => {
        await this.request(`${prefix}/models/${encodeURIComponent(name)}`, { method: 'PATCH', body: input })
      },
      deleteModel: async (name) => {
        await this.request<void>(`${prefix}/models/${encodeURIComponent(name)}`, { method: 'DELETE' })
      },

      listAliases: (channelId) =>
        this.request<ChannelAlias[]>(`${prefix}/aliases`, { query: { channel_id: channelId } }),
      createAlias: (input: AliasCreateInput) =>
        this.request<{ id: number }>(`${prefix}/aliases`, { method: 'POST', body: input }).then(idBody),
      updateAlias: async (id, input: AliasUpdateInput) => {
        await this.request(`${prefix}/aliases/${id}`, { method: 'PATCH', body: input })
      },
      deleteAlias: async (id) => {
        await this.request<void>(`${prefix}/aliases/${id}`, { method: 'DELETE' })
      },

      listRules: () => this.request<PricingRule[]>(`${prefix}/pricing/rules`),
      createRule: (input: RuleCreateInput) =>
        this.request<{ id: number }>(`${prefix}/pricing/rules`, { method: 'POST', body: input }).then(idBody),
      updateRule: async (id, input: RuleUpdateInput) => {
        await this.request(`${prefix}/pricing/rules/${id}`, { method: 'PATCH', body: input })
      },

      listCosts: () => this.request<PricingCost[]>(`${prefix}/pricing/costs`),
      createCost: (input: CostCreateInput) =>
        this.request<{ id: number }>(`${prefix}/pricing/costs`, { method: 'POST', body: input }).then(idBody),
      updateCost: async (id, input: CostUpdateInput) => {
        await this.request(`${prefix}/pricing/costs/${id}`, { method: 'PATCH', body: input })
      },
    }
  }

  /**
   * 商家自己的售价规则。
   *
   * 请求体只放可变字段：`scope`、`owner_user_id`、`channel_id` 在商家前缀下
   * 出现即被拒，作用域与归属由服务端按会话决定。
   */
  private merchantPricing(prefix: string): MerchantPricingClient {
    const idBody = (body: { id: number }) => body.id
    return {
      listRules: () => this.request<PricingRule[]>(`${prefix}/pricing/rules`),
      getRule: (id) => this.request<PricingRule>(`${prefix}/pricing/rules/${id}`),
      createRule: (input) =>
        this.request<{ id: number }>(`${prefix}/pricing/rules`, { method: 'POST', body: input }).then(idBody),
      updateRule: async (id, input) => {
        await this.request(`${prefix}/pricing/rules/${id}`, { method: 'PATCH', body: input })
      },
    }
  }

  /** 渠道的一组路由；两个前缀都有。 */
  private channelOps(prefix: string): ChannelOpsClient {
    const idBody = (body: { id: number }) => body.id
    return {
      listChannels: () => this.request<Channel[]>(`${prefix}/channels`),
      getChannel: (id) => this.request<Channel>(`${prefix}/channels/${id}`),
      createChannel: (input: ChannelCreateInput) =>
        this.request<{ id: number }>(`${prefix}/channels`, { method: 'POST', body: input }).then(idBody),
      updateChannel: async (id, input: ChannelUpdateInput) => {
        await this.request(`${prefix}/channels/${id}`, { method: 'PATCH', body: input })
      },
    }
  }

  /** 登录与恢复会话都会带回新的令牌，写请求靠它过 CSRF 校验。 */
  setCsrfToken(token: string | null): void {
    this.csrfToken = token
  }

  private async request<T>(
    path: string,
    options: {
      method?: string
      body?: unknown
      query?: Record<string, QueryValue | QueryValue[]>
    } = {},
  ): Promise<T> {
    const method = options.method ?? 'GET'
    const headers: Record<string, string> = { Accept: 'application/json' }
    if (options.body !== undefined) headers['Content-Type'] = 'application/json'
    if (WRITE_METHODS.has(method) && this.csrfToken) {
      headers['X-CSRF-Token'] = this.csrfToken
    }

    let response: Response
    try {
      response = await this.doFetch(buildUrl(this.baseUrl, path, options.query), {
        method,
        headers,
        credentials: 'same-origin',
        body: options.body === undefined ? undefined : JSON.stringify(options.body),
      })
    } catch (cause) {
      throw new ApiError(0, 'network_error', cause instanceof Error ? cause.message : 'network error')
    }

    if (response.status === 204) return undefined as T

    const payload = await response
      .json()
      .catch(() => null)

    if (!response.ok) {
      const body = payload as Partial<ApiErrorBody> | null
      const error = body && typeof body === 'object' ? body.error : undefined
      throw new ApiError(
        response.status,
        error?.code ?? 'http_error',
        error?.message ?? (response.statusText || 'request failed'),
        error?.request_id,
      )
    }

    // 会话恢复与登录的成功响应都带 csrf_token，收到就换成新的。
    if (payload && typeof payload === 'object' && typeof (payload as SessionResponse).csrf_token === 'string') {
      this.csrfToken = (payload as SessionResponse).csrf_token
    }
    return payload as T
  }

  meta(): Promise<Meta> {
    return this.request<Meta>('/meta')
  }

  login(username: string, password: string): Promise<SessionResponse> {
    return this.request<SessionResponse>('/auth/login', {
      method: 'POST',
      body: { username, password },
    })
  }

  async logout(): Promise<void> {
    await this.request<void>('/auth/logout', { method: 'POST' })
    this.csrfToken = null
  }

  async session(): Promise<SessionResponse | null> {
    try {
      return await this.request<SessionResponse>('/auth/session')
    } catch (error) {
      if (error instanceof ApiError && (error.status === 401 || error.status === 403)) return null
      throw error
    }
  }

  me(): Promise<User> {
    // `/me` 与两个写路径都包一层 `{user}`。
    return this.request<{ user: User }>('/me').then((body) => body.user)
  }

  updateMe(input: { display_name?: string | null; email?: string | null }): Promise<User> {
    return this.request<{ user: User }>('/me', { method: 'PATCH', body: input }).then((body) => body.user)
  }

  async changePassword(currentPassword: string, newPassword: string): Promise<void> {
    await this.request<void>('/me/password', {
      method: 'POST',
      body: { current_password: currentPassword, new_password: newPassword },
    })
  }

  listKeys(query: KeyListQuery = {}): Promise<ListResponse<ApiKey>> {
    return this.request<ListResponse<ApiKey>>('/keys', { query: { ...query } })
  }

  createKey(input: KeyCreateInput): Promise<CreatedKey> {
    // 201 + `{key, secret}`；Location 头指向新对象。
    return this.request<CreatedKey>('/keys', { method: 'POST', body: input })
  }

  getKey(id: number): Promise<ApiKey> {
    return this.request<ApiKey>(`/keys/${id}`)
  }

  updateKey(id: number, input: KeyUpdateInput): Promise<ApiKey> {
    return this.request<ApiKey>(`/keys/${id}`, { method: 'PATCH', body: input })
  }

  revokeKey(id: number): Promise<ApiKey> {
    return this.request<ApiKey>(`/keys/${id}/revoke`, { method: 'POST' })
  }

  models(): Promise<ModelsResponse> {
    return this.request<ModelsResponse>('/models')
  }

  /** 规范名可能带 `/`，按段编码但保留斜杠：服务端那条路由是多段通配。 */
  modelDetail(name: string): Promise<ModelDetailResponse> {
    return this.request<ModelDetailResponse>(`/models/${encodeURI(name)}`)
  }

  usageSummary(query: UsageQuery = {}): Promise<UsageSummary> {
    return this.request<UsageSummary>('/usage/summary', { query: { ...query } })
  }

  usageSeries(query: UsageSeriesQuery = {}): Promise<UsageSeries> {
    return this.request<UsageSeries>('/usage/series', { query: { ...query } })
  }

  usageBreakdown(query: UsageBreakdownQuery): Promise<UsageBreakdown> {
    return this.request<UsageBreakdown>('/usage/breakdown', { query: { ...query } })
  }

  usageRequests(query: UsageRequestsQuery = {}): Promise<ListResponse<UsageRequest>> {
    return this.request<ListResponse<UsageRequest>>('/usage/requests', { query: { ...query } })
  }

  wallet(): Promise<Wallet> {
    return this.request<Wallet>('/wallet')
  }

  walletEntries(query: WalletEntriesQuery = {}): Promise<ListResponse<WalletEntry>> {
    return this.request<ListResponse<WalletEntry>>('/wallet/entries', { query: { ...query } })
  }
}
