-- +goose Up
-- 健康度桶加模型维度：主键从 (channel_id, bucket) 换成
-- (channel_id, model, bucket)，另加一列「有成本事件的请求数」。
--
-- # 为什么要按模型切
--
-- 成本是**按（渠道，规范模型）**定价的，而桶原先按「渠道 × 小时」切：
-- 同一个渠道在这个小时里服务过的贵模型与便宜模型合成一个总数，再按请求数
-- 摊平。选路比较的是某个具体模型的候选，那个平均数因此答不上
-- 「给这个模型买这条渠道贵不贵」。
--
-- # 两个口径
--
--   - model = '' 是**渠道级合并行**：本渠道全模型加总。它是一等概念，不是
--     「未知」的占位符——迁移前的老行本来就是跨模型合并的，'' 正是它们的
--     准确含义。健康列（requests / successes / latency / cache）只读这一行，
--     所以它们的语义一字未变；将来要把这些列也改成按模型看，行已经在表里，
--     读侧换个过滤条件即可，不必再动一次主键。
--   - cost_events 是「这个桶里有多少条带 upstream_cost 的用量事件」。
--     单位成本从此用它做分母，与分子 SUM(upstream_cost) 是同一批事件：
--     失败的尝试不结算、也就没有用量事件，不再把平均数摊低。
--
-- 规范模型名不会是空串：写入口有两道把关——internal/catalog/admin.go 的
-- normalizeCanonicalName 拒绝空名，internal/pipeline/pipeline.go 的
-- Request.validate 在缺规范模型名时报错。所以 '' 不会与真实模型撞车。
-- （usage_events.canonical_model 只约束 NOT NULL，不约束空串，不能拿它当依据。）
--
-- # 老数据
--
-- 已有行的 model 取默认值 ''，落进合并行：健康列照旧可读。按模型查成本时
-- 它们不匹配任何模型名，读作未知——不编造一个「刚才那个模型」出来。
ALTER TABLE channel_health_rollup
    ADD COLUMN model text NOT NULL DEFAULT '',
    ADD COLUMN cost_events bigint NOT NULL DEFAULT 0;

COMMENT ON COLUMN channel_health_rollup.model IS
    '规范模型名；空串 = 本渠道全模型合并行（渠道级聚合，健康列只读这一行）。迁移前的老行也是空串。';
COMMENT ON COLUMN channel_health_rollup.cost_events IS
    '这个桶里带 upstream_cost 的用量事件条数，是单位成本的分母；与分子 SUM(upstream_cost) 取同一批事件。';

ALTER TABLE channel_health_rollup DROP CONSTRAINT channel_health_rollup_pkey;
ALTER TABLE channel_health_rollup
    ADD CONSTRAINT channel_health_rollup_pkey PRIMARY KEY (channel_id, model, bucket);

-- +goose Down
-- 回到 (channel_id, bucket)。多模型行**丢弃**，不求和：合并行（model = ''）
-- 每次调用都同时累加计数、并按桶从全部用量事件重算推导列，它已经是合并后的
-- 结果；再对全部行求和会把计数算两遍。
--
-- 少数情况下合并行可能缺失（写模型行成功、写合并行失败）：先从模型行补一条。
-- 补的时候哪些列能求和、哪些必须留空，判据是**可不可加**：
--
--   - 可加：requests / successes / failures / cost / cache_hit_tokens /
--     cache_miss_tokens。逐模型求和就是合并值。cost 与两个 cache 列上都有
--     DEFAULT 0，**不能省略**——省略不会让它们变成 NULL，而是落成 0，成本 0
--     会被读成「这段时间不花钱」，比留空更坏。
--   - 不可加：latency_p50_ms / latency_p95_ms。分位数不能由分位数求和得到，
--     而这里也没有原始样本可重算；两列本来就可空，留 NULL 是诚实的。
INSERT INTO channel_health_rollup
    (channel_id, model, bucket, requests, successes, failures,
     cost, cache_hit_tokens, cache_miss_tokens)
SELECT channel_id, '', bucket, SUM(requests), SUM(successes), SUM(failures),
       SUM(cost), SUM(cache_hit_tokens), SUM(cache_miss_tokens)
  FROM channel_health_rollup
 WHERE model <> ''
 GROUP BY channel_id, bucket
ON CONFLICT (channel_id, model, bucket) DO NOTHING;

DELETE FROM channel_health_rollup WHERE model <> '';

ALTER TABLE channel_health_rollup DROP CONSTRAINT channel_health_rollup_pkey;
ALTER TABLE channel_health_rollup
    ADD CONSTRAINT channel_health_rollup_pkey PRIMARY KEY (channel_id, bucket);

ALTER TABLE channel_health_rollup DROP COLUMN cost_events;
ALTER TABLE channel_health_rollup DROP COLUMN model;
