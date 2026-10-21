/**
 * 控制台 API 的响应形状。
 *
 * 契约来源是 `internal/consoleapi`：字段名与 JSON 标签逐字对齐，前端不猜。
 * 金额一律是十进制字符串（8 位小数），时间一律是 RFC3339；不在客户端用
 * double 表示钱。
 */

/** 十进制金额字符串，例如 `"12.34000000"`。 */
export type Money = string

export interface MetaFeatures {
  registration: boolean
  /** `admin` 表示找回口令要靠运维；将来可能多一种。 */
  password_reset: string
  topup: boolean
}

export interface Meta {
  site_name: string
  /** 调用 API 的基址，落在落地页与快速开始里。 */
  api_base_url: string
  currency: string
  /** IANA 时区名，例如 `Asia/Shanghai`；界面上的绝对时间按它显示。 */
  timezone: string
  locales: string[]
  features: MetaFeatures
}

/** 账号在控制台里拥有的视图；服务端据此判定越权，前端只决定显示哪几组页面。 */
export type Role = 'user' | 'merchant' | 'admin'

export interface User {
  id: number
  username: string
  display_name: string | null
  email: string | null
  status: string
  /** 控制台视图的可见范围。旧响应没有这个字段时按 `user` 处理。 */
  role?: Role
  created_at: string
}

export interface SessionResponse {
  user: User
  csrf_token: string
  expires_at: string
}

export interface ListResponse<T> {
  items: T[]
  next_cursor: string | null
}

export type KeyStatus = 'active' | 'disabled' | 'revoked'

export interface ApiKey {
  id: number
  /** 备注名；没填时是空串。 */
  name: string
  /** `sk-` + 前 8 位。 */
  prefix: string
  /** 后 6 位。老密钥（`larkax_` 方案）为 null。 */
  suffix: string | null
  /** 接口给的展示形态，前端不自己拼。 */
  masked: string
  status: KeyStatus
  /** null 表示不限。 */
  quota_limit: Money | null
  /** 累计花费，界面文案写「累计」。 */
  quota_used: Money
  /** null 表示不限，0 表示不放行。 */
  rate_limit_rpm: number | null
  expires_at: string | null
  last_used_at: string | null
  created_at: string
}

export interface CreatedKey {
  key: ApiKey
  /** 完整密钥，只在创建响应里出现一次。 */
  secret: string
}

/**
 * 创建请求体：全部字段可选。
 *
 * `quota_limit` 要么给正数，要么**不给这个字段**表示不限；服务端拒绝 null
 * 与 0（见 `handleCreateKey`）。所以页面在额度留空时不能带这个键。
 */
export interface KeyCreateInput {
  name?: string
  quota_limit?: Money
  rate_limit_rpm?: number
  expires_at?: string
}

/** 修改请求体：不出现的键表示不改，null 表示清空（额度与限速即「不限」）。 */
export interface KeyUpdateInput {
  name?: string
  quota_limit?: Money | null
  rate_limit_rpm?: number | null
  expires_at?: string | null
  status?: 'active' | 'disabled'
}

export interface ModelPricing {
  /** 每 100 万输入 token 的参考价。 */
  input_per_mtok: Money
  /** 每 100 万输出 token 的参考价。 */
  output_per_mtok: Money
  /** null 表示没有配置，与 0（免费）不是一回事。 */
  cache_read_per_mtok: Money | null
  cache_write_per_mtok: Money | null
}

export interface ModelModalities {
  input: string[]
  output: string[]
}

export interface Model {
  /** 规范模型名，也是调用时 `model` 字段的值。 */
  name: string
  display_name: string
  vendor: string
  context_limit: number
  max_output: number
  /** 只包含已采集的能力；键为 `tools` / `vision` / `reasoning` / `caching`。 */
  capabilities: Record<string, boolean>
  modalities: ModelModalities
  /** 没有平台计价规则时为 null，界面显示「—」。 */
  pricing: ModelPricing | null
}

export interface ModelsResponse {
  currency: string
  /** 参考价的求值时刻。 */
  reference_at: string
  items: Model[]
}

export interface Tokens {
  input: number
  output: number
  cache_read: number
  cache_write: number
  reasoning: number
}

export interface UsageTotals {
  cost: Money
  requests: number
  tokens: Tokens
}

/** 按计费路径分档。 */
export interface UsagePath {
  path: string
  cost: Money
  requests: number
}

export interface UsageSummary {
  from: string
  to: string
  currency: string
  totals: UsageTotals
  billing_paths: UsagePath[]
}

export interface UsageSeriesPoint extends UsageTotals {
  /** 桶的起点，带站点偏移的 RFC3339。 */
  start: string
}

export interface UsageSeries {
  interval: 'day' | 'hour'
  tz: string
  points: UsageSeriesPoint[]
}

export interface UsageModelGroup extends UsageTotals {
  model: string
}

export interface UsageKeyGroup extends UsageTotals {
  /** null 表示这一行的调用没有关联密钥（旧数据）。 */
  key_id: number | null
  key_name: string
}

export type UsageGroup = UsageModelGroup | UsageKeyGroup

export interface UsageBreakdown {
  group_by: 'model' | 'key'
  items: UsageGroup[]
  /** 超出 limit 的各组合计。 */
  other: UsageTotals
}

/** 计费请求的状态。 */
export type RequestStatus = 'pending' | 'settled' | 'released' | 'failed' | 'refunded'

export interface UsageRequestKey {
  id: number
  name: string
}

export interface UsageRequest {
  id: number
  request_id: string
  model: string
  /** 没有关联密钥（旧数据或内部调用）时为 null。 */
  key: UsageRequestKey | null
  status: RequestStatus
  cost: Money
  currency: string
  tokens: Tokens
  /** 走了哪一档计费路径；没结算时为 null。 */
  billing_path: string | null
  streamed: boolean
  protocol: string
  duration_ms: number | null
  started_at: string
  settled_at: string | null
}

export type UsageRequestPage = ListResponse<UsageRequest>

export interface Wallet {
  asset: string
  balance: Money
  frozen: Money
  updated_at: string
}

/** 用户钱包上会出现的四种分录。 */
export type WalletEntryType = 'topup' | 'consume' | 'refund' | 'adjustment'

export interface WalletEntry {
  id: number
  created_at: string
  type: WalletEntryType
  /** 余额变动本身：正数是入账，负数是扣费。前端不再翻符号。 */
  amount: Money
  memo: string
  billing_request_id: number | null
  /**
   * 这条分录的币种。同一条时间线里可能混着不同币种；旧响应没有这个字段，
   * 界面退回站点币种。
   */
  asset?: string
}

export type WalletEntryPage = ListResponse<WalletEntry>

/** 用量四个读共用的时间窗与筛选。`from` 含、`to` 不含。 */
export interface UsageQuery {
  /** RFC3339 或 `YYYY-MM-DD`；后者按站点时区解释成当天 00:00。 */
  from?: string
  to?: string
  key_id?: number
  model?: string
}

export interface ListQuery {
  limit?: number
  cursor?: string | null
}

export interface KeyListQuery extends ListQuery {
  status?: KeyStatus
}

export interface UsageRequestsQuery extends UsageQuery {
  status?: RequestStatus
  limit?: number
  cursor?: string | null
}

export interface UsageSeriesQuery extends UsageQuery {
  interval?: 'day' | 'hour'
}

export interface UsageBreakdownQuery extends UsageQuery {
  group_by: 'model' | 'key'
  limit?: number
}

export interface WalletEntriesQuery {
  from?: string
  to?: string
  /** 可重复参数；空数组与不传都表示不限类型。 */
  type?: WalletEntryType[]
  limit?: number
  cursor?: string | null
}

export interface ApiErrorBody {
  error: {
    code: string
    message: string
    request_id: string
  }
}

/* ── 运营资源（admin 是全集；merchant 前缀下只有渠道） ─────────────────── */

/** 渠道状态；服务端只有 `enabled` 与 `disabled`。 */
export type ChannelStatus = 'enabled' | 'disabled'

export interface Channel {
  id: number
  /** 0 表示平台自营。 */
  owner_user_id: number
  name: string
  type: string
  base_url: string
  status: string
  models: string[]
  weight: number
  priority: number
  setting: Record<string, unknown>
  credential_keys: string[]
  /** 凭证末四位，用来确认「挂的是我刚发的那个」。 */
  credential_hint: string
  credential_error?: string
  deleted_at: string | null
  created_at: string
  updated_at: string
}

export interface ChannelCreateInput {
  /** 只由 admin 给；0 或不给表示平台自营。merchant 视图由服务端决定归属。 */
  owner_user_id?: number
  name: string
  type: string
  base_url: string
  credential: string
  models: string[]
  weight?: number
  priority?: number
  setting?: Record<string, unknown>
}

/** 不出现的键表示不改；`credential` 不出现时密文一个字节都不动。 */
export interface ChannelUpdateInput {
  name?: string
  type?: string
  base_url?: string
  credential?: string
  models?: string[]
  weight?: number
  priority?: number
  setting?: Record<string, unknown>
  status?: ChannelStatus
}

/** 能力开关三态：`null` 是未知，`false` 是明确不支持。 */
export interface OpsCapabilities {
  reasoning: boolean | null
  tools: boolean | null
  vision: boolean | null
  audio: boolean | null
  cache: boolean | null
  batch: boolean | null
  /** `null` 是未知，`[]` 是一条都不支持。 */
  endpoints: string[] | null
}

export interface OpsModel {
  id: number
  canonical_name: string
  display_name: string
  vendor: string
  family: string
  context_limit: number
  input_limit: number
  output_limit: number
  capabilities: OpsCapabilities
  modalities_input: string[]
  modalities_output: string[]
  reasoning_options: unknown[]
  canonical_model_id: string
  knowledge_cutoff: string | null
  release_date: string | null
  raw: Record<string, unknown>
  updated_at: string
}

export interface OpsCapabilitiesInput {
  reasoning?: boolean | null
  tools?: boolean | null
  vision?: boolean | null
  audio?: boolean | null
  cache?: boolean | null
  batch?: boolean | null
  endpoints?: string[] | null
}

export interface OpsModelInput {
  display_name?: string
  vendor?: string
  family?: string
  context_limit?: number
  input_limit?: number
  output_limit?: number
  capabilities?: OpsCapabilitiesInput
  modalities_input?: string[]
  modalities_output?: string[]
  canonical_model_id?: string
  /** 空串表示清空（只在 update 里）。 */
  knowledge_cutoff?: string | null
  release_date?: string | null
}

export interface OpsModelCreateInput extends OpsModelInput {
  canonical_name: string
}

export interface ChannelAlias {
  id: number
  channel_id: number
  upstream_model: string
  canonical_name: string
  overrides: Record<string, unknown>
  created_at: string
}

export interface AliasCreateInput {
  channel_id: number
  upstream_model: string
  canonical_name: string
  overrides?: Record<string, unknown>
}

export interface AliasUpdateInput {
  upstream_model?: string
  overrides?: Record<string, unknown>
}

/** 售价规则的作用域。 */
export type RuleScope = 'platform' | 'merchant' | 'channel'

export interface PricingRule {
  id: number
  scope: string
  owner_user_id: number
  channel_id: number
  name: string
  model_patterns: string[]
  expr: string
  priority: number
  effective_from: string
  effective_to: string | null
  enabled: boolean
  created_by: number
  created_at: string
  updated_at: string
  /** 表达式里引用到的变量名，界面可以提示拼错的键。 */
  declared_keys: string[]
  expr_error?: string
}

export interface RuleCreateInput {
  scope: RuleScope
  owner_user_id?: number
  channel_id?: number
  name: string
  model_patterns: string[]
  expr: string
  priority?: number
  effective_from?: string
  effective_to?: string | null
  enabled?: boolean
}

export interface RuleUpdateInput {
  scope?: RuleScope
  owner_user_id?: number
  channel_id?: number
  name?: string
  model_patterns?: string[]
  expr?: string
  priority?: number
  effective_from?: string
  effective_to?: string | null
  /** `effective_to` 给空串时用它显式清空；不传表示不改。 */
  clear_effective_to?: boolean
  enabled?: boolean
}

/**
 * 商家自助的那一半。
 *
 * `Omit` 掉的三栏是「出现即拒」：作用域由服务端定成 merchant，归属是会话
 * 用户，渠道这一维在商家规则里根本不存在。类型上拿不到，就不容易顺手带上。
 */
export type MerchantRuleCreateInput = Omit<
  RuleCreateInput,
  'scope' | 'owner_user_id' | 'channel_id'
>

export type MerchantRuleUpdateInput = Omit<
  RuleUpdateInput,
  'scope' | 'owner_user_id' | 'channel_id'
>

/** `/api/v1/merchant/pricing/rules*`：只有规则，没有成本。 */
export interface MerchantPricingClient {
  listRules(): Promise<PricingRule[]>
  getRule(id: number): Promise<PricingRule>
  createRule(input: MerchantRuleCreateInput): Promise<number>
  updateRule(id: number, input: MerchantRuleUpdateInput): Promise<void>
}

export interface PricingCost {
  id: number
  channel_id: number
  model_patterns: string[]
  expr: string
  priority: number
  effective_from: string
  effective_to: string | null
  created_at: string
  declared_keys: string[]
  expr_error?: string
}

export interface CostCreateInput {
  channel_id: number
  model_patterns: string[]
  expr: string
  priority?: number
  effective_from?: string
  effective_to?: string | null
}

export interface CostUpdateInput {
  channel_id?: number
  model_patterns?: string[]
  expr?: string
  priority?: number
  effective_from?: string
  effective_to?: string | null
  clear_effective_to?: boolean
}

/**
 * 渠道的读写口，两个前缀都有。
 *
 * admin 与 merchant 的路径形状相同，JSON 逐字对齐 `internal/adminapi/resources.go`。
 * 差异在范围：merchant 只看得到、也只改得了自己的渠道，归属由服务端按会话决定。
 */
export interface ChannelOpsClient {
  listChannels(): Promise<Channel[]>
  getChannel(id: number): Promise<Channel>
  createChannel(input: ChannelCreateInput): Promise<number>
  updateChannel(id: number, input: ChannelUpdateInput): Promise<void>
}

/**
 * 平台级运营资源：模型目录、别名、计价。
 *
 * 只有 admin 前缀下有这几组路由——它们没有「归属」可过滤，不给商家视图开一份。
 */
export interface OpsClient extends ChannelOpsClient {
  listModels(): Promise<OpsModel[]>
  createModel(input: OpsModelCreateInput): Promise<number>
  updateModel(name: string, input: OpsModelInput): Promise<void>
  deleteModel(name: string): Promise<void>

  listAliases(channelId?: number): Promise<ChannelAlias[]>
  createAlias(input: AliasCreateInput): Promise<number>
  updateAlias(id: number, input: AliasUpdateInput): Promise<void>
  deleteAlias(id: number): Promise<void>

  listRules(): Promise<PricingRule[]>
  createRule(input: RuleCreateInput): Promise<number>
  updateRule(id: number, input: RuleUpdateInput): Promise<void>

  listCosts(): Promise<PricingCost[]>
  createCost(input: CostCreateInput): Promise<number>
  updateCost(id: number, input: CostUpdateInput): Promise<void>
}


/**
 * 控制台用到的全部读写出入口。HTTP 实现见 `client.ts`，开发期替身见 `mock.ts`；
 * 页面只依赖这个接口，替换实现只改 `api/index.ts` 一处。
 */
export interface ApiClient {
  readonly mode: 'mock' | 'http'
  setCsrfToken(token: string | null): void
  meta(): Promise<Meta>
  login(username: string, password: string): Promise<SessionResponse>
  logout(): Promise<void>
  session(): Promise<SessionResponse | null>
  me(): Promise<User>
  updateMe(input: { display_name?: string | null; email?: string | null }): Promise<User>
  changePassword(currentPassword: string, newPassword: string): Promise<void>
  listKeys(query?: KeyListQuery): Promise<ListResponse<ApiKey>>
  createKey(input: KeyCreateInput): Promise<CreatedKey>
  getKey(id: number): Promise<ApiKey>
  updateKey(id: number, input: KeyUpdateInput): Promise<ApiKey>
  revokeKey(id: number): Promise<ApiKey>
  models(): Promise<ModelsResponse>
  usageSummary(query?: UsageQuery): Promise<UsageSummary>
  usageSeries(query?: UsageSeriesQuery): Promise<UsageSeries>
  usageBreakdown(query: UsageBreakdownQuery): Promise<UsageBreakdown>
  usageRequests(query?: UsageRequestsQuery): Promise<ListResponse<UsageRequest>>
  wallet(): Promise<Wallet>
  walletEntries(query?: WalletEntriesQuery): Promise<ListResponse<WalletEntry>>
  /** `/api/v1/admin/*` 下的运营资源；账号没有 admin 角色时服务端一律 403。 */
  admin: OpsClient
  /**
   * `/api/v1/merchant/*`：自有渠道与自己的售价规则。
   * 范围由服务端限定；模型目录、别名与成本规则在商家前缀下没有路由。
   */
  merchant: ChannelOpsClient & MerchantPricingClient
}
