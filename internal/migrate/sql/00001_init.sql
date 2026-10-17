-- +goose Up
-- 初始 schema。
--
-- 设计要点：
--   * 所有角色都是用户。merchant / admin 是 users 表上的布尔标记，不是独立实体。
--   * 金额一律用 numeric 定点数，绝不用浮点。
--   * 账本只追加不修改，余额是推导值。
--   * 用量事件与请求快照按时间分区，只追加。
--   * 渠道凭证的**本体**只存密文，解密点唯一。另有一列 `credential_hint` 存末四位
--     明文供界面辨识——它不参与鉴权，只用来让人看出「这条渠道换过没有」。
--
-- 关于扩展：citext 被 users.username/email 使用。pgcrypto 目前没有任何调用，
-- 渠道凭证加密在应用层做（信封加密，密钥来自环境变量，不落库）。
-- 留着是为将来可能出现的「只在库内比对」场景留位——例如用 digest() 存
-- API Key 的哈希以免明文可比。它要求较高的数据库权限，在托管 PostgreSQL 上
-- 常需单独申请；若确认不再需要，删掉这一行即可。
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE EXTENSION IF NOT EXISTS citext;

-- ═══════════════════════════════════════════════════════════════════
-- 用户与鉴权
-- ═══════════════════════════════════════════════════════════════════

CREATE TABLE users (
    id              bigserial PRIMARY KEY,
    username        citext NOT NULL UNIQUE,
    email           citext UNIQUE,
    password_hash   text,
    status          text NOT NULL DEFAULT 'active'
                    CHECK (status IN ('active', 'suspended', 'banned')),
    -- 「merchant」「admin」只是称呼，底层是同一张表上的标记
    is_merchant     boolean NOT NULL DEFAULT false,
    is_admin        boolean NOT NULL DEFAULT false,
    display_name    text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

COMMENT ON COLUMN users.is_merchant IS '特殊用户的称呼之一，不是独立实体。表示该用户可挂载上游渠道并获得分成。';
COMMENT ON COLUMN users.is_admin IS '特殊用户的称呼之一。admin 天然也是 merchant。';

CREATE TABLE api_keys (
    id              bigserial PRIMARY KEY,
    user_id         bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name            text NOT NULL DEFAULT '',
    -- 只存哈希，明文仅在创建时返回一次
    key_hash        text NOT NULL UNIQUE,
    key_prefix      text NOT NULL,
    status          text NOT NULL DEFAULT 'active'
                    CHECK (status IN ('active', 'disabled', 'revoked')),
    -- 精细化限额，NULL 表示继承用户默认
    quota_limit     numeric(20, 8),
    rate_limit_rpm  integer,
    expires_at      timestamptz,
    last_used_at    timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX api_keys_user_idx ON api_keys (user_id) WHERE status = 'active';

-- ═══════════════════════════════════════════════════════════════════
-- 钱包与账本
--
-- 账本只追加不修改。user_wallets.balance 是缓存/快照，
-- 权威值由 ledger_entries 推导，由对账任务定期校验。
-- asset 是一等字段，将来接加密货币充值不改表结构。
-- ═══════════════════════════════════════════════════════════════════

CREATE TABLE user_wallets (
    user_id         bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    asset           text NOT NULL DEFAULT 'CNY',
    balance         numeric(20, 8) NOT NULL DEFAULT 0,
    frozen          numeric(20, 8) NOT NULL DEFAULT 0,
    updated_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, asset),
    CONSTRAINT wallet_non_negative CHECK (balance >= 0 AND frozen >= 0)
);
COMMENT ON COLUMN user_wallets.balance IS '快照值。权威值 = ledger_entries 汇总，对账任务负责校验二者一致。';

-- 复式记账：每个 txn_id 下的分录金额之和为零
CREATE TABLE ledger_entries (
    id              bigserial PRIMARY KEY,
    txn_id          uuid NOT NULL,
    -- 会计科目：user:<id> / merchant_payable:<id> / platform_revenue /
    --           upstream_cost / payment_gateway:<name> / payout:<id>
    account         text NOT NULL,
    asset           text NOT NULL DEFAULT 'CNY',
    -- 借为正、贷为负；同一 txn 内求和为零
    amount          numeric(20, 8) NOT NULL,
    entry_type      text NOT NULL CHECK (entry_type IN (
                        'topup', 'consume', 'refund', 'settle',
                        'payout_request', 'payout_paid', 'adjustment')),
    -- 关联对象，便于追溯
    billing_request_id bigint,
    payout_id       bigint,
    memo            text,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ledger_txn_idx ON ledger_entries (txn_id);
CREATE INDEX ledger_account_idx ON ledger_entries (account, created_at DESC);
COMMENT ON TABLE ledger_entries IS '只追加。任何余额修正都写新分录，绝不 UPDATE。';

-- ═══════════════════════════════════════════════════════════════════
-- 模型目录与别名归一
--
-- 上游命名极不统一，同一个模型各家叫法不同，风格也混杂。
-- 内部用 canonical 规范名，各渠道别名映射到它。
-- ═══════════════════════════════════════════════════════════════════

CREATE TABLE model_catalog (
    id                  bigserial PRIMARY KEY,
    canonical_name      text NOT NULL UNIQUE,
    display_name        text NOT NULL,
    vendor              text,
    family              text,
    -- models.dev 的字段映射
    context_limit       integer,
    input_limit         integer,
    output_limit        integer,
    -- 能力矩阵
    supports_reasoning  boolean NOT NULL DEFAULT false,
    supports_tools      boolean NOT NULL DEFAULT false,
    supports_vision     boolean NOT NULL DEFAULT false,
    supports_audio      boolean NOT NULL DEFAULT false,
    supports_cache      boolean NOT NULL DEFAULT false,
    supports_batch      boolean NOT NULL DEFAULT false,
    modalities_input    text[] NOT NULL DEFAULT '{}',
    modalities_output   text[] NOT NULL DEFAULT '{}',
    -- 推理强度支持：models.dev 的 reasoning_options 原样落库
    -- 形如 [{"type":"effort","values":["low","medium","high"]}]
    --      [{"type":"toggle"}]
    --      [{"type":"budget_tokens","min":0,"max":31999}]
    reasoning_options   jsonb NOT NULL DEFAULT '[]'::jsonb,
    -- 数据来源与元信息
    canonical_model_id  text,
    knowledge_cutoff    date,
    release_date        date,
    raw                 jsonb NOT NULL DEFAULT '{}'::jsonb,
    updated_at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX model_catalog_vendor_idx ON model_catalog (vendor);

-- 按渠道把上游的实际模型名映射到规范名
CREATE TABLE model_aliases (
    id                  bigserial PRIMARY KEY,
    -- 可空表示这是一条平台级别名，不属于任何渠道。此时它由 canonical_name
    -- 唯一决定，同一条不应出现两次——但下面那条 UNIQUE 管不到它：
    -- SQL 里 NULL 不等于 NULL，channel_id IS NULL 的行可以有任意多条。
    -- 平台级与渠道级两个方向的去重各由一条**偏**唯一索引承担，
    -- 见 00007_model_alias_unique.sql。
    channel_id          bigint,
    upstream_model      text NOT NULL,
    canonical_name      text REFERENCES model_catalog(canonical_name),
    -- 对上游模型名的静态覆盖，商家可配
    overrides           jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (channel_id, upstream_model)
);

-- ═══════════════════════════════════════════════════════════════════
-- 渠道（上游账号）
-- ═══════════════════════════════════════════════════════════════════

CREATE TABLE channels (
    id                  bigserial PRIMARY KEY,
    -- 归属一个 merchant 用户
    owner_user_id       bigint REFERENCES users(id) ON DELETE RESTRICT,
    name                text NOT NULL,
    type                text NOT NULL,     -- 适配器类型，见 internal/channel 的注册表
    base_url            text NOT NULL,
    -- 凭证密文。唯一解密点是 internal/channels，插件永远拿不到
    credential_enc      bytea NOT NULL,
    credential_hint     text,              -- 末四位，供界面辨识
    -- 上游协议族：决定用哪套 usage 口径与请求格式
    protocol            text NOT NULL CHECK (protocol IN ('openai', 'anthropic', 'custom')),
    status              text NOT NULL DEFAULT 'enabled'
                        CHECK (status IN ('enabled', 'disabled', 'drained')),
    -- 该渠道支持的规范模型名
    models              text[] NOT NULL DEFAULT '{}',
    weight              integer NOT NULL DEFAULT 1,
    priority            integer NOT NULL DEFAULT 0,
    setting             jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX channels_owner_idx ON channels (owner_user_id);
CREATE INDEX channels_status_idx ON channels (status) WHERE status = 'enabled';

ALTER TABLE model_aliases
    ADD CONSTRAINT model_aliases_channel_fk
    FOREIGN KEY (channel_id) REFERENCES channels(id) ON DELETE CASCADE;

-- ═══════════════════════════════════════════════════════════════════
-- 额度监控
--
-- 多数订阅制上游不提供余额查询接口。因此 remaining_estimate 是推断值，
-- confidence 说明可信度，调度时按 confidence 权衡。
-- ═══════════════════════════════════════════════════════════════════

CREATE TABLE channel_quota_state (
    channel_id          bigint PRIMARY KEY REFERENCES channels(id) ON DELETE CASCADE,
    remaining_estimate  numeric(20, 8),
    -- 0..1。查询得来的接近 1，纯自记账的明显更低
    confidence          numeric(3, 2) NOT NULL DEFAULT 0
                        CHECK (confidence >= 0 AND confidence <= 1),
    -- 额度观测方式
    probe_kind          text NOT NULL DEFAULT 'self_accounting'
                        CHECK (probe_kind IN ('api_query', 'self_accounting', 'error_observed')),
    health              text NOT NULL DEFAULT 'unknown'
                        CHECK (health IN ('healthy', 'degraded', 'drained', 'unknown')),
    -- 滚动窗口（固定周期，或从首次使用起算）
    window_kind         text CHECK (window_kind IN ('fixed_clock', 'from_first_use')),
    window_started_at   timestamptz,
    window_budget       numeric(20, 8),
    window_used         numeric(20, 8) NOT NULL DEFAULT 0,
    resets_at           timestamptz,
    -- 上游最近一次的真实信号
    last_error_code     text,
    last_error_at       timestamptz,
    last_probe_at       timestamptz,
    updated_at          timestamptz NOT NULL DEFAULT now()
);
COMMENT ON COLUMN channel_quota_state.confidence IS '调度时按此值权衡。纯自记账的估算不应与查询值同等对待。';

-- 探针历史，用于回溯与对账
CREATE TABLE quota_probes (
    id              bigserial PRIMARY KEY,
    channel_id      bigint NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    probe_kind      text NOT NULL,
    ok              boolean NOT NULL,
    http_status     integer,
    remaining       numeric(20, 8),
    error_code      text,
    raw             jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX quota_probes_channel_idx ON quota_probes (channel_id, created_at DESC);

-- 渠道健康度（与额度监控共用埋点，不另做一套）
CREATE TABLE channel_health_rollup (
    channel_id          bigint NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    bucket              timestamptz NOT NULL,     -- 小时桶
    requests            bigint NOT NULL DEFAULT 0,
    successes           bigint NOT NULL DEFAULT 0,
    failures            bigint NOT NULL DEFAULT 0,
    latency_p50_ms      integer,
    latency_p95_ms      integer,
    cost                numeric(20, 8) NOT NULL DEFAULT 0,
    cache_hit_tokens    bigint NOT NULL DEFAULT 0,
    cache_miss_tokens   bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (channel_id, bucket)
);

-- ═══════════════════════════════════════════════════════════════════
-- 计费规则
--
-- 用表达式而非倍率乘法：倍率是「模型名 → 标量」的静态映射，
-- 没有时间维度也没有「总输入量 → 落档」的阶跃。
--
-- 表达式**求值出一组单价**，由 Go 侧乘用量——表达式本身不返回金额。
-- 这条边界是有意的：单价乘用量走整数定点运算，没有舍入；
-- 若让表达式直接返回金额（浮点），那个精确性就没了，而且逐项明细
-- 也就拆不出来，对账时无法回答「这一分钱是哪个环节产生的」。
-- 于是用量变量（p/c/len/…）只作**条件**用，不作乘数。
-- 实现见 internal/billing/expr。
-- ═══════════════════════════════════════════════════════════════════

CREATE TABLE pricing_rules (
    id              bigserial PRIMARY KEY,
    -- 作用域：谁定的价
    scope           text NOT NULL CHECK (scope IN ('platform', 'merchant', 'channel')),
    owner_user_id   bigint REFERENCES users(id) ON DELETE CASCADE,
    channel_id      bigint REFERENCES channels(id) ON DELETE CASCADE,
    name            text NOT NULL,
    -- 匹配哪些模型。空数组表示全部
    model_patterns  jsonb NOT NULL DEFAULT '[]'::jsonb,
    -- 表达式源码。变量与函数的完整清单见 internal/billing/expr。
    --
    -- 表达式**求值出一组单价**（可含一次性费用），由 Go 侧乘用量——
    -- 所以用量变量只作**条件**用，不作乘数。理由见本段开头的注释。
    expr            text NOT NULL,
    -- 优先级，数值大者先匹配
    priority        integer NOT NULL DEFAULT 0,
    effective_from  timestamptz NOT NULL DEFAULT now(),
    effective_to    timestamptz,
    enabled         boolean NOT NULL DEFAULT true,
    created_by      bigint REFERENCES users(id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    -- 本约束只管一件事：platform 作用域的规则不得挂 owner。
    -- 注意它不保证「同一作用域内生效时间不重叠」——表里有 effective_from /
    -- effective_to，但重叠与否没有数据库级保证（要用需要 EXCLUDE USING gist）。
    -- 重叠时谁生效由应用层按 priority 决胜。
    CONSTRAINT rule_scope_owner CHECK (
        (scope = 'platform' AND owner_user_id IS NULL) OR scope <> 'platform'
    )
);
CREATE INDEX pricing_rules_lookup_idx
    ON pricing_rules (scope, owner_user_id, priority DESC)
    WHERE enabled;

-- 规则版本历史，成交价的可追溯性靠它
CREATE TABLE pricing_rule_versions (
    id              bigserial PRIMARY KEY,
    rule_id         bigint NOT NULL REFERENCES pricing_rules(id) ON DELETE CASCADE,
    version         integer NOT NULL,
    expr            text NOT NULL,
    changed_by      bigint REFERENCES users(id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (rule_id, version)
);

-- 上游成本价（用于算商户应收与平台毛利），结构与规则类似
CREATE TABLE upstream_costs (
    id              bigserial PRIMARY KEY,
    channel_id      bigint NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    model_patterns  jsonb NOT NULL DEFAULT '[]'::jsonb,
    expr            text NOT NULL,
    priority        integer NOT NULL DEFAULT 0,
    effective_from  timestamptz NOT NULL DEFAULT now(),
    effective_to    timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX upstream_costs_channel_idx ON upstream_costs (channel_id, priority DESC);

-- ═══════════════════════════════════════════════════════════════════
-- 分时计价用的日历
--
-- 数据源 NateScarlet/holiday-cn（MIT），每日自动抓国务院公告。
-- papers 字段存公告 URL，可用来监控公告是否被修订。
-- ═══════════════════════════════════════════════════════════════════

CREATE TABLE calendar_days (
    region          text NOT NULL DEFAULT 'CN',
    day             date NOT NULL,
    -- 元旦 / 春节 / 国庆节 等
    holiday_name    text,
    is_off_day      boolean NOT NULL,
    -- true 表示这是调休补班日（法定安排里挪用周末上班）
    is_makeup_workday boolean NOT NULL DEFAULT false,
    source          text NOT NULL,     -- holiday-cn / chinese-days / manual
    source_year     integer NOT NULL,
    synced_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (region, day)
);
COMMENT ON COLUMN calendar_days.is_makeup_workday IS '调休补班日。holiday-cn 用 isOffDay=false 表达。';

-- 每年数据的同步状态。
--
-- 注意 status 的语义：
--   pending  = 该年公告尚未发布。对未来年份这是【正常状态】，不是缺陷，
--              价格规则自然回退到「周末为低谷」即可，不产生告警。
--   synced   = 已同步且双源一致。
--   conflict = 双源不一致，需人工裁决。
--   degraded = 公告已发布但同步失败。这才是需要告警的情况。
CREATE TABLE calendar_years (
    region          text NOT NULL DEFAULT 'CN',
    year            integer NOT NULL,
    status          text NOT NULL CHECK (status IN ('synced', 'pending', 'conflict', 'degraded')),
    day_count       integer NOT NULL DEFAULT 0,
    -- 国务院公告原文 URL，来自数据源的 papers 字段。
    -- papers 非空即表示公告已发布，可作为 pending → synced 的判据。
    source_paper_urls text[] NOT NULL DEFAULT '{}',
    -- 双源交叉校验结果
    sources_agree   boolean,
    last_error      text,
    synced_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (region, year)
);
COMMENT ON TABLE calendar_years IS 'pending 是未来年份的正常状态，不告警。只有 degraded（公告已发布却同步失败）与 conflict 需要告警。';

-- ═══════════════════════════════════════════════════════════════════
-- 计费请求（预留—结算两阶段）
-- ═══════════════════════════════════════════════════════════════════

CREATE TABLE billing_requests (
    id                  bigserial PRIMARY KEY,
    request_id          text NOT NULL UNIQUE,   -- 幂等键
    user_id             bigint NOT NULL REFERENCES users(id),
    api_key_id          bigint REFERENCES api_keys(id),
    channel_id          bigint REFERENCES channels(id),
    canonical_model     text NOT NULL,
    upstream_model      text,
    protocol            text NOT NULL,          -- openai / anthropic
    streamed            boolean NOT NULL DEFAULT false,
    -- 预留
    reserved_amount     numeric(20, 8) NOT NULL DEFAULT 0,
    reserved_asset      text NOT NULL DEFAULT 'CNY',
    -- 结算
    status              text NOT NULL DEFAULT 'pending'
                        CHECK (status IN ('pending', 'settled', 'released', 'failed', 'refunded')),
    final_amount        numeric(20, 8),
    -- 使用的计价规则快照，保证历史账单可解释
    pricing_rule_id     bigint REFERENCES pricing_rules(id),
    pricing_expr_snapshot text,
    -- 上游失败不计费的依据
    upstream_status     integer,
    error_code          text,
    -- 客户端取消的流式请求走这条路径
    client_cancelled_at timestamptz,
    -- 关联的结构快照。不做级联删除，也不会有 UPDATE 来置空：
    -- 快照是按分区 DROP 清理的，不逐行删除，所以分区一删此列就永久悬空。
    -- 读取方需自行判空并接受查不到。
    request_snapshot_id bigint,
    started_at          timestamptz NOT NULL DEFAULT now(),
    settled_at          timestamptz
);
CREATE INDEX billing_requests_user_idx ON billing_requests (user_id, started_at DESC);
CREATE INDEX billing_requests_pending_idx ON billing_requests (status) WHERE status = 'pending';

-- ═══════════════════════════════════════════════════════════════════
-- 用量事件（计量层）
--
-- 只追加、按时间分区。billing_path 区分实测与估算，
-- 这直接决定账单能否向用户解释。
-- ═══════════════════════════════════════════════════════════════════

CREATE TABLE usage_events (
    id                  bigserial,
    request_id          text NOT NULL,
    user_id             bigint NOT NULL,
    channel_id          bigint,
    canonical_model     text NOT NULL,
    -- token 明细。各家口径不同，由适配器归一化后写入
    prompt_tokens       bigint NOT NULL DEFAULT 0,
    completion_tokens   bigint NOT NULL DEFAULT 0,
    cache_read_tokens   bigint NOT NULL DEFAULT 0,
    cache_write_tokens  bigint NOT NULL DEFAULT 0,
    reasoning_tokens    bigint NOT NULL DEFAULT 0,
    -- 非 token 计量：搜索次数、图片张数、视频秒数
    unit_counts         jsonb NOT NULL DEFAULT '{}'::jsonb,
    -- 计费路径
    billing_path        text NOT NULL CHECK (billing_path IN ('measured', 'estimated', 'fallback')),
    estimator           text,              -- tiktoken / hf / count_tokens_api / char_heuristic
    -- 单价与结果，落库供对账
    unit_prices         jsonb NOT NULL DEFAULT '{}'::jsonb,
    cost                numeric(20, 8) NOT NULL,
    upstream_cost       numeric(20, 8),
    asset               text NOT NULL DEFAULT 'CNY',
    -- 分时计价的判定依据，事后可复算
    priced_at           timestamptz NOT NULL DEFAULT now(),
    local_hour          smallint,
    is_off_day          boolean,
    duration_ms         integer,
    created_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);

CREATE INDEX usage_events_request_idx ON usage_events (request_id, created_at);
CREATE INDEX usage_events_user_idx ON usage_events (user_id, created_at DESC);
CREATE INDEX usage_events_channel_idx ON usage_events (channel_id, created_at DESC);

-- 用量事件按月分区。它是只追加的财务底稿，没有留存期要求，月粒度足够。
-- 注意与下面的 request_snapshots 不同——那张表按天，因为它的留存期以天计。
-- 分区由 worker 定时预建，这里只铺种子。
CREATE TABLE usage_events_2026_10 PARTITION OF usage_events
    FOR VALUES FROM ('2026-10-01') TO ('2026-11-01');
CREATE TABLE usage_events_2026_11 PARTITION OF usage_events
    FOR VALUES FROM ('2026-11-01') TO ('2026-12-01');

CREATE TABLE usage_events_default PARTITION OF usage_events DEFAULT;
COMMENT ON TABLE usage_events_default IS '兜底分区。监控应告警它非空，说明分区创建任务漏跑了。';

-- ═══════════════════════════════════════════════════════════════════
-- 请求快照（结构留存，不留内容）
--
-- 目的只有一个：报错时能复现。所以留的是「形状」——
-- 模型名、推理深度、max_tokens、消息条数与角色序列、块的类型与顺序、
-- 工具的数量与参数键名、各段内容的字符数——而不是内容本身。
--
-- 铁律：白名单放行，其余一律替换为占位。未知字段默认脱敏，不是默认保留。
-- 脱敏器出错时丢弃快照，绝不回落成存原文。
--
-- 分区按天，默认留存 30 天。这既是隐私要求也是成本控制。
-- 按天而不是按月：留存期以天计，月分区只能整月留或整月删，
-- 「30 天」就无从谈起，而清理靠 DROP TABLE 是这份承诺能兑现的前提。
-- 分区由 larkaxctl 每日预建，保留未来若干天。
-- ═══════════════════════════════════════════════════════════════════

CREATE TABLE request_snapshots (
    id                  bigserial,
    request_id          text NOT NULL,
    user_id             bigint NOT NULL,
    direction           text NOT NULL CHECK (direction IN ('request', 'response')),
    -- 骨架格式版本。脱敏器的白名单会随上游 API 演进变化，
    -- 不带版本号的话旧快照将无法解释。
    skeleton_version    smallint NOT NULL DEFAULT 1,
    -- 采集模式。full_consented 是用户主动授权、限时开启的，
    -- 到期由后台任务清除，不依赖用户手动关。
    capture_mode        text NOT NULL DEFAULT 'skeleton'
                        CHECK (capture_mode IN ('skeleton', 'full_consented')),
    -- 结构骨架。只含白名单标量、长度、键名与引用关系
    skeleton            jsonb NOT NULL,
    -- 骨架体积超限时降级为摘要层（只留顶层标量），并置此标志
    skeleton_truncated  boolean NOT NULL DEFAULT false,
    -- 采集时的上游 HTTP 状态与错误分类，复现的入口
    upstream_status     integer,
    error_kind          text,
    -- 脱敏过程中被替换掉的字段路径，便于确认白名单是否漏了该留的东西
    redacted_paths      text[] NOT NULL DEFAULT '{}',
    created_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);

CREATE INDEX request_snapshots_request_idx ON request_snapshots (request_id, created_at);
CREATE INDEX request_snapshots_user_idx ON request_snapshots (user_id, created_at DESC);
CREATE INDEX request_snapshots_error_idx ON request_snapshots (error_kind, created_at DESC)
    WHERE error_kind IS NOT NULL;

-- 按天铺种子分区，worker 每日预建后续若干天。留 3 天够走通本地流程，
-- 真实部署由预建任务铺满留存窗口。
CREATE TABLE request_snapshots_2026_10_15 PARTITION OF request_snapshots
    FOR VALUES FROM ('2026-10-15') TO ('2026-10-16');
CREATE TABLE request_snapshots_2026_10_16 PARTITION OF request_snapshots
    FOR VALUES FROM ('2026-10-16') TO ('2026-10-17');
CREATE TABLE request_snapshots_2026_10_17 PARTITION OF request_snapshots
    FOR VALUES FROM ('2026-10-17') TO ('2026-10-18');

-- 兜底分区。落进这里的行没有到期日——「按分区整块删除」删不掉它们，
-- 而这张表对使用者承诺了 30 天留存。所以它必须和 usage_events_default 一样
-- 被监控盯着：非空即说明预建任务漏跑了，且已经产生了不受留存期约束的数据。
CREATE TABLE request_snapshots_default PARTITION OF request_snapshots DEFAULT;
COMMENT ON TABLE request_snapshots_default IS '兜底分区。必须告警其非空：落进来的数据没有到期日，留存承诺对它不成立。';

COMMENT ON COLUMN request_snapshots.skeleton IS
    '结构骨架，非原文。白名单字段记真实值（模型名、推理深度、max_tokens、各段字符数）；用户 prompt、工具名、思考内容、图片数据一律替换为占位。工具名在单个请求内映射为稳定的 T1/T2 引用，以保证 tool_use 与 tool_result 的对应关系不丢。';
COMMENT ON COLUMN request_snapshots.redacted_paths IS
    '被替换的 JSON 路径。用于两件事：确认白名单没漏放行项；排查「本该保留的字段为什么没留下」。';

-- 用户主动授权的原样采集。用于排查具体问题，必须有明确同意与自动到期。
CREATE TABLE capture_consents (
    id              bigserial PRIMARY KEY,
    user_id         bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    api_key_id      bigint REFERENCES api_keys(id),
    -- 授权范围：单个请求，或一段时间的全部请求
    scope           text NOT NULL CHECK (scope IN ('single_request', 'time_window')),
    request_id      text,
    window_start    timestamptz,
    window_end      timestamptz,
    -- 同意方式的留痕，用于合规举证
    consent_text    text NOT NULL,
    consent_version smallint NOT NULL,
    agreed_at       timestamptz NOT NULL DEFAULT now(),
    revoked_at      timestamptz,
    -- 到期强制失效，不依赖用户或后台操作
    expires_at      timestamptz NOT NULL,
    CONSTRAINT capture_consents_scope_shape CHECK (
        (scope = 'single_request' AND request_id IS NOT NULL)
        OR (scope = 'time_window' AND window_start IS NOT NULL AND window_end IS NOT NULL)
    )
);
CREATE INDEX capture_consents_active_idx ON capture_consents (user_id, expires_at)
    WHERE revoked_at IS NULL;

-- 快照读取审计。能看用户请求结构的人必须留痕，包括我们自己。
CREATE TABLE snapshot_access_log (
    id              bigserial PRIMARY KEY,
    snapshot_id     bigint NOT NULL,
    request_id      text NOT NULL,
    actor_user_id   bigint REFERENCES users(id),
    actor_kind      text NOT NULL CHECK (actor_kind IN ('owner', 'admin', 'system')),
    reason          text,
    accessed_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX snapshot_access_log_request_idx ON snapshot_access_log (request_id, accessed_at DESC);

-- ═══════════════════════════════════════════════════════════════════
-- 支付与提现
-- ═══════════════════════════════════════════════════════════════════

CREATE TABLE payment_orders (
    id              bigserial PRIMARY KEY,
    order_no        text NOT NULL UNIQUE,
    user_id         bigint NOT NULL REFERENCES users(id),
    -- stripe / alipay / wechat。加密货币作为资产品种将来可加
    gateway         text NOT NULL,
    asset           text NOT NULL DEFAULT 'CNY',
    amount          numeric(20, 8) NOT NULL,
    -- 通道手续费
    fee             numeric(20, 8) NOT NULL DEFAULT 0,
    status          text NOT NULL DEFAULT 'created'
                    CHECK (status IN ('created', 'paid', 'failed', 'expired', 'refunded')),
    gateway_txn_id  text,
    gateway_raw     jsonb NOT NULL DEFAULT '{}'::jsonb,
    paid_at         timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX payment_orders_user_idx ON payment_orders (user_id, created_at DESC);
CREATE INDEX payment_orders_gateway_idx ON payment_orders (gateway, gateway_txn_id);

CREATE TABLE payout_requests (
    id              bigserial PRIMARY KEY,
    merchant_user_id bigint NOT NULL REFERENCES users(id),
    asset           text NOT NULL DEFAULT 'CNY',
    amount          numeric(20, 8) NOT NULL CHECK (amount > 0),
    -- 商户填写的收款信息
    destination     jsonb NOT NULL DEFAULT '{}'::jsonb,
    status          text NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'approved', 'rejected', 'paid', 'cancelled')),
    reviewed_by     bigint REFERENCES users(id),
    reviewed_at     timestamptz,
    review_note     text,
    paid_at         timestamptz,
    payout_ref      text,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX payout_requests_status_idx ON payout_requests (status, created_at);

-- ═══════════════════════════════════════════════════════════════════
-- 套餐
--
-- models.dev 不收录套餐，这份目录必须自建。
-- 七个维度落在下面的 dimensions（jsonb）里。它的结构尚未定义到代码里，
-- 所以这里不指向任何包——写成「见 internal/catalog」那样的引用会指向一个
-- 不存在的地方，读的人会以为结构已经有了。
-- ═══════════════════════════════════════════════════════════════════

CREATE TABLE plans (
    id              bigserial PRIMARY KEY,
    key             text NOT NULL UNIQUE,
    name            text NOT NULL,
    description     text,
    -- 归属：NULL 表示平台策划录入。商家可覆盖为自己的实际套餐
    owner_user_id   bigint REFERENCES users(id) ON DELETE CASCADE,
    -- 七个维度的结构化描述
    dimensions      jsonb NOT NULL DEFAULT '{}'::jsonb,
    price           numeric(20, 8),
    asset           text NOT NULL DEFAULT 'CNY',
    billing_period  text CHECK (billing_period IN ('monthly', 'weekly', 'one_time')),
    enabled         boolean NOT NULL DEFAULT true,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
COMMENT ON COLUMN plans.dimensions IS '固定周期额度 / 滚动窗口 / 模型分级子额度 / 可结转补充额度 / 计价单位 / 超限策略 / 是否含 API 访问';

CREATE TABLE plan_subscriptions (
    id              bigserial PRIMARY KEY,
    user_id         bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    plan_id         bigint NOT NULL REFERENCES plans(id),
    status          text NOT NULL DEFAULT 'active'
                    CHECK (status IN ('active', 'expired', 'cancelled')),
    period_start    timestamptz NOT NULL,
    period_end      timestamptz,
    -- 按维度记账的额度使用情况
    quota_state     jsonb NOT NULL DEFAULT '{}'::jsonb,
    -- 用户的计费偏好：订阅优先还是钱包优先
    billing_preference text NOT NULL DEFAULT 'wallet'
                    CHECK (billing_preference IN ('wallet', 'subscription')),
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX plan_subscriptions_user_idx ON plan_subscriptions (user_id) WHERE status = 'active';

-- ═══════════════════════════════════════════════════════════════════
-- 插件
-- ═══════════════════════════════════════════════════════════════════

CREATE TABLE plugins (
    id              bigserial PRIMARY KEY,
    key             text NOT NULL,
    version         text NOT NULL,
    name            text NOT NULL,
    -- 源码。官方插件通过 go:embed 内置，这里存管理员上传的覆盖版
    source          text NOT NULL,
    meta            jsonb NOT NULL DEFAULT '{}'::jsonb,
    -- 能力声明。仅作展示与审计，实际能力由宿主注入决定
    capabilities    jsonb NOT NULL DEFAULT '{}'::jsonb,
    status          text NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'active', 'failed', 'disabled')),
    -- 编译错误信息，回显到管理界面
    compile_error   text,
    -- 原子发布的代次。编译失败保留旧代次
    generation      bigint NOT NULL DEFAULT 0,
    created_by      bigint REFERENCES users(id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (key, version)
);
CREATE INDEX plugins_active_idx ON plugins (key, generation DESC) WHERE status = 'active';

-- 哪个用户启用了哪个插件（平台级或商户级）
CREATE TABLE plugin_activations (
    id              bigserial PRIMARY KEY,
    plugin_key      text NOT NULL,
    scope           text NOT NULL CHECK (scope IN ('platform', 'user', 'channel')),
    user_id         bigint REFERENCES users(id) ON DELETE CASCADE,
    channel_id      bigint REFERENCES channels(id) ON DELETE CASCADE,
    config          jsonb NOT NULL DEFAULT '{}'::jsonb,
    priority        integer NOT NULL DEFAULT 0,
    enabled         boolean NOT NULL DEFAULT true,
    created_at      timestamptz NOT NULL DEFAULT now()
);

-- ═══════════════════════════════════════════════════════════════════
-- 对账
-- ═══════════════════════════════════════════════════════════════════

CREATE TABLE reconciliation_runs (
    id              bigserial PRIMARY KEY,
    period_start    timestamptz NOT NULL,
    period_end      timestamptz NOT NULL,
    status          text NOT NULL DEFAULT 'running'
                    CHECK (status IN ('running', 'completed', 'failed')),
    -- 三方核对结果
    our_total       numeric(20, 8),
    upstream_total  numeric(20, 8),
    ledger_total    numeric(20, 8),
    discrepancy     numeric(20, 8),
    details         jsonb NOT NULL DEFAULT '{}'::jsonb,
    started_at      timestamptz NOT NULL DEFAULT now(),
    finished_at     timestamptz
);
COMMENT ON TABLE reconciliation_runs IS '核对三方：账本扣费、上游账单、渠道额度实际减少量。差额即漏损。';

-- +goose Down
DROP TABLE IF EXISTS reconciliation_runs CASCADE;
DROP TABLE IF EXISTS plugin_activations CASCADE;
DROP TABLE IF EXISTS plugins CASCADE;
DROP TABLE IF EXISTS plan_subscriptions CASCADE;
DROP TABLE IF EXISTS plans CASCADE;
DROP TABLE IF EXISTS payout_requests CASCADE;
DROP TABLE IF EXISTS payment_orders CASCADE;
DROP TABLE IF EXISTS snapshot_access_log CASCADE;
DROP TABLE IF EXISTS capture_consents CASCADE;
DROP TABLE IF EXISTS request_snapshots_default CASCADE;
DROP TABLE IF EXISTS request_snapshots_2026_10_17 CASCADE;
DROP TABLE IF EXISTS request_snapshots_2026_10_16 CASCADE;
DROP TABLE IF EXISTS request_snapshots_2026_10_15 CASCADE;
DROP TABLE IF EXISTS request_snapshots CASCADE;
DROP TABLE IF EXISTS usage_events_default CASCADE;
DROP TABLE IF EXISTS usage_events_2026_11 CASCADE;
DROP TABLE IF EXISTS usage_events_2026_10 CASCADE;
DROP TABLE IF EXISTS usage_events CASCADE;
DROP TABLE IF EXISTS billing_requests CASCADE;
DROP TABLE IF EXISTS calendar_years CASCADE;
DROP TABLE IF EXISTS calendar_days CASCADE;
DROP TABLE IF EXISTS upstream_costs CASCADE;
DROP TABLE IF EXISTS pricing_rule_versions CASCADE;
DROP TABLE IF EXISTS pricing_rules CASCADE;
DROP TABLE IF EXISTS channel_health_rollup CASCADE;
DROP TABLE IF EXISTS quota_probes CASCADE;
DROP TABLE IF EXISTS channel_quota_state CASCADE;
DROP TABLE IF EXISTS channels CASCADE;
DROP TABLE IF EXISTS model_aliases CASCADE;
DROP TABLE IF EXISTS model_catalog CASCADE;
DROP TABLE IF EXISTS ledger_entries CASCADE;
DROP TABLE IF EXISTS user_wallets CASCADE;
DROP TABLE IF EXISTS api_keys CASCADE;
DROP TABLE IF EXISTS users CASCADE;
