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

/** 一条供应报价：谁在供这个模型、每 1000 个输入 token 多少钱。 */
export interface ModelOffer {
  /** 商家用户名；null 表示平台自营。 */
  merchant: string | null
  /** 每 1000 个输入 token 的金额。 */
  price: Money
}

export interface Model {
  /** 规范模型名，也是调用时 `model` 字段的值。 */
  name: string
  display_name: string
  vendor: string
  /** 厂商下的系列（如 `claude`、`gpt`）。清单接口还没带这个字段时省略。 */
  family?: string
  context_limit: number
  max_output: number
  /** 只包含已采集的能力；键为 `tools` / `vision` / `reasoning` / `caching`。 */
  capabilities: Record<string, boolean>
  modalities: ModelModalities
  /** 没有平台计价规则时为 null，界面显示「—」。 */
  pricing: ModelPricing | null
  /**
   * 按渠道归属折出来的报价，已按价格升序、同商家同价只留一条。
   * 没有报价时服务端省略这个字段（不是空数组）。
   */
  offers?: ModelOffer[]
}

export interface ModelsResponse {
  currency: string
  /** 参考价的求值时刻。 */
  reference_at: string
  items: Model[]
}

/** 供应商报价里的一个计价维度；接口只给规则声明过的那些。 */
export type PriceDimension = 'prompt' | 'completion' | 'cache_read' | 'cache_write' | 'reasoning'

/** 一条供应商报价：一个商家（`null` 为平台自营）× 一整套维度价。 */
export interface SupplierPrice {
  merchant: string | null
  /** 每 1000 个输入 token 的金额；没有输入价时是 `0.00000000`，不能当「免费」用。 */
  price: Money
  /** 只含声明过的维度；一个都没有时省略整个字段。 */
  prices?: Partial<Record<PriceDimension, Money>>
  channel_count: number
}

/** 单模型页的能力：六项三态，`null` 是没采集过，与 `false` 不同。 */
export interface ModelCapabilities {
  reasoning: boolean | null
  tools: boolean | null
  vision: boolean | null
  audio: boolean | null
  cache: boolean | null
  batch: boolean | null
  /** `null` 未采集；`[]` 明确声明一条都不支持。 */
  endpoints: string[] | null
}

export interface ModelDetailData {
  name: string
  display_name: string
  vendor: string
  family: string
  /** 三个上限为 `0` 表示目录里没填，不是「上限是 0」。 */
  context_limit: number
  input_limit: number
  output_limit: number
  capabilities: ModelCapabilities
  modalities_input: string[]
  modalities_output: string[]
  reasoning_options?: unknown[] | null
  canonical_model_id: string
  knowledge_cutoff: string | null
  release_date: string | null
  updated_at: string
  /** 按服务端给的顺序展示，前端不重排；没有报价时省略。 */
  suppliers?: SupplierPrice[]
  /** 在供但算不出价的渠道条数，`0` 也会输出。 */
  unpriced_channels: number
}

export interface ModelDetailResponse {
  currency: string
  reference_at: string
  model: ModelDetailData
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

/* ── 商家运营：概览、上游与在供模型 ────────────────────────────────── */

/**
 * 上游健康。取组内最差一档：`healthy` < `unknown` < `degraded` < `drained`。
 *
 * `unknown` 是「还没探测过」或探测没给结论，与「明确坏了」不是一回事，
 * 页面上要给不同的文字。
 */
export type ChannelHealth = 'healthy' | 'unknown' | 'degraded' | 'drained'

/** 异常渠道的理由。多条同时成立时全部出现。 */
export type UnhealthyReason =
  | 'failure_rate'
  | 'quota_state'
  | 'probe_failed'
  | 'consecutive_failures'
  | 'no_requests_24h'

export interface MerchantOverview {
  /** 名下未软删的渠道数（含停用的）；管理员会话下是全站。 */
  channels: number
  /** 在供的规范模型数，判据与模型清单同源。 */
  models_in_supply: number
  /** 站点当地的今天，不是最近 24 小时；`cost` 是售价口径。 */
  today: { requests: number; cost: Money }
  /** 最近 24 小时窗口内的异常渠道；正常时是空数组。 */
  unhealthy_channels: Array<{ id: number; name: string; reasons: UnhealthyReason[] }>
}

export interface MerchantUpstreamChannel {
  id: number
  name: string
  status: string
  health: ChannelHealth
}

/** 按 `(type, base_url)` 归并的一条上游；组内多条渠道是多个账号。 */
export interface MerchantUpstream {
  type: string
  base_url: string
  health: ChannelHealth
  requests: number
  failures: number
  /** 0..1 的小数，页面只负责换成百分比。 */
  failure_rate: number
  channels: MerchantUpstreamChannel[]
}

export interface MerchantUpstreamsResponse {
  items: MerchantUpstream[]
  /** 截断前的总条数。 */
  total: number
  truncated: boolean
}

export interface MerchantModel {
  model: string
  display_name: string
  /** 名下有这个模型在供的渠道数（同址多个账号各算一条）。 */
  channels: number
  /**
   * 有没有一条生效中的商家规则命中它。
   *
   * 它回答「配过价没有」，不是「实际按哪条计价」，也不是金额；管理员
   * 视野下问的是「全平台有没有商家配过」，规则的主人未必在供这个模型。
   */
  priced: boolean
}

export interface MerchantModelsResponse {
  items: MerchantModel[]
  total: number
  truncated: boolean
}

/** `/api/v1/merchant/overview|upstreams|models`：只读，admin 会话下看全站。 */
export interface MerchantOpsClient {
  overview(): Promise<MerchantOverview>
  listUpstreams(query?: { limit?: number }): Promise<MerchantUpstreamsResponse>
  listSuppliedModels(query?: { limit?: number }): Promise<MerchantModelsResponse>
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
 * 渠道表单可选的上游适配器。
 *
 * 来自服务端的适配器注册表，与建渠道时的类型校验同一份来源；`endpoint` 是
 * 这类适配器把请求发到哪条路径。
 */
export interface ChannelAdapter {
  type: string
  protocol: string
  endpoint: string
}

/**
 * 一次模型探测的输入。
 *
 * 给 `channel_id` 时类型、地址、凭证都从库里那一行取；`base_url` 与
 * `credential` 非空则覆盖（编辑中的表单还没保存）。不给 `channel_id` 时
 * `type` 与 `base_url` 必填，`credential` 可以为空（无鉴权的自建上游）。
 */
export interface ChannelProbeInput {
  channel_id?: number
  type?: string
  base_url?: string
  credential?: string
}

/** 探测结果：`models` 已去重升序；`total` 是截断之前的条数。 */
export interface ChannelProbeResult {
  /** false 表示上游没有「列出模型」这个能力，不是失败。 */
  supported: boolean
  models: string[]
  total: number
  truncated: boolean
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
  /** 渠道表单里可选的适配器；顺序由服务端定（按类型升序）。 */
  listAdapters(): Promise<ChannelAdapter[]>
  /** 拿一份地址与凭证向上游问一次模型清单。不落库。 */
  probeModels(input: ChannelProbeInput): Promise<ChannelProbeResult>
}

/**
 * 清单页的三种状态口径。
 *
 * 销户流程里的 `deletion_requested` / `deleted` 不在筛选里——这一页不管那两种，
 * 它们只会出现在详情里，见 {@link AdminUserStatus}。
 */
export type UserStatus = 'active' | 'suspended' | 'banned'

/** 详情里可能看到的全部状态；多出的两种只读，改它会被服务端拒绝。 */
export type AdminUserStatus = UserStatus | 'deletion_requested' | 'deleted'

/**
 * 用户清单的一行。
 *
 * `email` 是**打码后**的写法（本地部分只留首字符）；完整邮箱只在详情里给，
 * 见 {@link AdminUserDetail.email_full}。
 */
export interface AdminUserSummary {
  id: number
  username: string
  email: string | null
  display_name: string | null
  role: Role
  status: AdminUserStatus
  created_at: string
  last_seen_at: string | null
}

/** 用户详情：清单的全部字段，加上钱包、key 与近 7 天的汇总。金额是定点数字符串。 */
export interface AdminUserDetail extends AdminUserSummary {
  email_full: string | null
  balance: Money
  active_key_count: number
  requests_7d: number
  amount_7d: Money
  channel_count: number
  key_last_used_at: string | null
}

export interface AdminUserQuery {
  query?: string
  role?: Role
  status?: UserStatus
  limit?: number
  /** 上一页的 `next_cursor`；换筛选条件时要从第一页重取，游标不绑定条件。 */
  cursor?: string | null
}

/** 两个字段都可省略，但至少给一个——空 body 会被服务端拒。 */
export interface AdminUserUpdateInput {
  role?: Role
  status?: UserStatus
}

/**
 * 平台级运营资源：模型目录、别名、计价。
 *
 * 只有 admin 前缀下有这几组路由——它们没有「归属」可过滤，不给商家视图开一份。
 * 用户管理同理：判据里有「不能改自己的角色/状态」，离不开操作者是谁。
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

  listUsers(query?: AdminUserQuery): Promise<ListResponse<AdminUserSummary>>
  getUser(id: number): Promise<AdminUserDetail>
  updateUser(id: number, input: AdminUserUpdateInput): Promise<AdminUserDetail>
}


/**
 * 控制台用到的全部读写出入口。HTTP 实现见 `client.ts`，开发期替身见 `mock.ts`；
 * 页面只依赖这个接口，替换实现只改 `api/index.ts` 一处。
 */
export interface ApiClient {
  readonly mode: 'mock' | 'http'
  setCsrfToken(token: string | null): void
  /**
   * 注册「会话失效」回调：业务请求收到 401 时通知，路由据此回到登录页。
   *
   * 只通知 401——403 是权限不够，会话本身是好的。只保留一个回调，
   * 后注册的覆盖前一个；`null` 注销。登录接口与会话探测的 401 不通知。
   */
  setUnauthorizedHandler(handler: (() => void) | null): void
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
  /** 单模型页的数据；未知模型与无供给都回 404。 */
  modelDetail(name: string): Promise<ModelDetailResponse>
  usageSummary(query?: UsageQuery): Promise<UsageSummary>
  usageSeries(query?: UsageSeriesQuery): Promise<UsageSeries>
  usageBreakdown(query: UsageBreakdownQuery): Promise<UsageBreakdown>
  usageRequests(query?: UsageRequestsQuery): Promise<ListResponse<UsageRequest>>
  wallet(): Promise<Wallet>
  walletEntries(query?: WalletEntriesQuery): Promise<ListResponse<WalletEntry>>
  /** `/api/v1/admin/*` 下的运营资源；账号没有 admin 角色时服务端一律 403。 */
  admin: OpsClient
  /**
   * `/api/v1/merchant/*`：自有渠道、自己的售价规则，以及只读的运营概览、
   * 上游与在供模型。范围由服务端限定；模型目录、别名与成本规则在商家前缀
   * 下没有路由。管理员会话下后三组给的是全站数据。
   */
  merchant: ChannelOpsClient & MerchantPricingClient & MerchantOpsClient
}
