-- +goose Up
-- 用量事件补一列「这笔请求预留时占着的额度包面值」，与 cost（实收）并列。
--
-- # 为什么在 usage_events 上，而不是让读侧去 join billing_requests
--
-- 控制台的三个聚合视图（usage/summary、usage/series、usage/breakdown）与
-- 渠道用量共用同一段求和列（internal/ledger/store 的 usageTotalsColumns）。
-- 面值落在这张表上，那一处补一次，聚合侧就一次全部拿到；将来再加一个聚合
-- 视图也不必各自记得去关联账单表。行级的 usage/requests 读的是
-- billing_requests 上的同名列，两处同源同口径。
--
-- # 值的来源
--
-- 写入口把**结算那一笔** billing_requests.plan_reserved_amount 原样搬过来，
-- 不在用量事件这一层重新推算——两份推算迟早分岔，而对不上时看不出是哪一份
-- 错了。额度包覆盖的请求按预留那一刻占住的面值记，不是最终实收的金额：
-- cost 回答「这笔用量收了多少」，这一列回答「它当时占住了包里的多少」。
-- 两者不合并，也不相加——一个是实收，一个是上界。
--
-- 默认 0 的含义是「这笔请求没有用到额度包」，不是「没有数据」：这一列
-- NOT NULL，读侧因此不必区分 NULL 与 0。
ALTER TABLE usage_events
    ADD COLUMN plan_reserved_amount numeric(20, 8) NOT NULL DEFAULT 0;

COMMENT ON COLUMN usage_events.plan_reserved_amount IS
    '这笔请求预留时占着的额度包面值上界（来自结算那一笔 billing_requests.plan_reserved_amount）；0 表示没有用到额度包。与 cost 并列，不合并也不相加。';

-- usage_events 是分区表。纯 ADD COLUMN 时 PG 保证递归到已有分区，所以这一条
-- 是**双保险**：它防的是将来有人把上面的 ALTER TABLE 改成 ALTER TABLE ONLY——
-- 那时新列只落在父表上，读侧按分区扫会在某次查询才现形，而那时离迁移已经很远。
--
-- 它排在回填**之前**：真有分区缺列时，先报的是这里的 RAISE（点名缺了哪几个
-- 分区），而不是下面那条 UPDATE 的「列不存在」。
--
-- DO 块里有分号，要包在 StatementBegin/StatementEnd 里，否则 goose 会按分号
-- 把这一句切成两半。
-- +goose StatementBegin
DO $$
DECLARE
    missing text;
BEGIN
    SELECT string_agg(c.relname, ', ') INTO missing
      FROM pg_inherits i
      JOIN pg_class c ON c.oid = i.inhrelid
     WHERE i.inhparent = 'usage_events'::regclass
       AND NOT EXISTS (
           SELECT 1
             FROM pg_attribute a
            WHERE a.attrelid = c.oid
              AND a.attname = 'plan_reserved_amount'
              AND a.attnum > 0
              AND NOT a.attisdropped);
    IF missing IS NOT NULL THEN
        RAISE EXCEPTION 'usage_events 的已建分区里缺 plan_reserved_amount: %', missing;
    END IF;
END $$;
-- +goose StatementEnd

-- # 历史行回填
--
-- 新行由写入口填；已经落库的已结算行要一次性补齐，否则「行级对得上、聚合
-- 总量对不上」在历史数据上仍然存在。三条边界：
--
--   - 只碰 b.plan_reserved_amount <> 0 的行，其余保持默认的 0；
--   - 按 request_id 关联。billing_requests.request_id 是唯一键，一行账单只
--     对应一个请求，不会把同一笔面值摊到多行上；
--   - billing_requests 里已经没有的请求保持 0——没有可搬的值，不编造。
--
-- usage_events 是分区表，按父表写即可，PG 会把行路由到各自的分区。
UPDATE usage_events e
   SET plan_reserved_amount = b.plan_reserved_amount
  FROM billing_requests b
 WHERE b.request_id = e.request_id
   AND b.plan_reserved_amount <> 0;

-- +goose Down
-- 删掉这一列之后，三个聚合视图失去这一栏；行级 usage/requests 读的是
-- billing_requests 上的同名列，不受影响。
ALTER TABLE usage_events DROP COLUMN plan_reserved_amount;
