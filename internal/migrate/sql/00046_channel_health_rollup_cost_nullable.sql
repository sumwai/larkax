-- +goose Up
-- 健康度桶里的成本列改成可空：NULL 表示「这个桶里没有一条带成本的事件」。
--
-- # 它补的是哪条读法
--
-- 这一列原先 NOT NULL DEFAULT 0，而写侧从来没有写过它。读侧（internal/channels
-- 的 costPerUnit）因此把 0 判成「没有数据」——那是当时唯一能让没记过成本的渠道
-- 不被打成「免费」的办法，代价是「真的免费」与「没记过」在库里长得一样。
--
-- 写入侧接上之后（结算按成本规则算出 usage_events.upstream_cost，桶里用
-- SUM 重算），这两种情形分得开了：
--
--   - NULL：这个桶里没有一条带成本的事件——没有生效的成本规则，或者那一桶
--     的事件都发生在规则接上之前。读侧据此让成本这一维退出加权。
--   - 0：有事件，它们的成本合计确实是 0（规则写了 0）。那是一个真实的数。
--
-- SUM 在一整桶都是 NULL 时返回 NULL，而列非空会让那条 UPDATE 直接失败——
-- 失败的不只是成本这一列，同一句里的延迟与缓存也不会写进去。所以可空不是
-- 为了好看，是这条重算语句能跑通的前提。
ALTER TABLE channel_health_rollup ALTER COLUMN cost DROP NOT NULL;

COMMENT ON COLUMN channel_health_rollup.cost IS
    '这个小时桶里所有请求的上游成本合计。NULL = 桶内没有带成本的事件（没有生效的成本规则）；0 = 有事件、合计确实为 0。';

-- +goose Down
-- 回到 NOT NULL 之前先把 NULL 回填成 0：直接 SET NOT NULL 会在已有 NULL 的
-- 库上失败，而那条失败会挡住回滚本身。
UPDATE channel_health_rollup SET cost = 0 WHERE cost IS NULL;
ALTER TABLE channel_health_rollup ALTER COLUMN cost SET NOT NULL;
-- 这一列原先没有注释，Down 回到那个状态。
COMMENT ON COLUMN channel_health_rollup.cost IS NULL;
