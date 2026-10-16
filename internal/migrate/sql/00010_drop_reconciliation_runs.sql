-- +goose Up
-- 删掉 reconciliation_runs。
--
-- 00001 建了它，00004 又建了 reconcile_runs。两张表都是「每次对账运行的记录」，
-- 而只有后者有写入方：全仓对 reconciliation_runs 的引用只有它自己的 DDL。
--
-- 它不只是「一张还没有写入方的表」，而是**被取代的那一版设计**：
--
--   | | reconciliation_runs | reconcile_runs（在用） |
--   |---|---|---|
--   | 增量起点 | period_start / period_end（时间段） | watermark（上一轮比对的起点） |
--   | 结果 | our_total / upstream_total / ledger_total / discrepancy | checked_wallets / findings |
--
-- 00004 的注释写明了为什么弃用时间段：跑的过程中新写入的行会被下一轮再读一遍
-- ——「宁可重复读，不能漏读」。也就是说 reconcile_runs 是想清楚之后的那一版。
--
-- 留着它的伤害有两条：
--
-- 1. 看 schema 的人会以为对账在做三方核对（账本扣费 / 上游账单 / 渠道额度实际
--    减少量），因为那四列摆在那里，而实际上没有任何代码填它们。想知道对账到底
--    做了什么，得先去确认哪张表是活的——这个确认本身就不该需要。
-- 2. 将来真要做三方核对（`upstream_costs` 与 `usage_events` 一旦落库就有了数据），
--    来的人会先看到这张表：它的名字比 reconcile_runs 更贴切，四列也已经摆好了，
--    于是往它里面填——历史记录于是散在两处。
--
-- 所以三方核对真要落地时，那些列该加到 reconcile_runs 上，而不是复活这张表。
-- 原 DDL 与表注释留在 git 历史里的 00001_init.sql，需要时可以照着取。
DROP TABLE IF EXISTS reconciliation_runs;

-- +goose Down
-- 按 00001 的原样重建。**表里原有的数据恢复不了**：删除时就没有了。
--
-- 注意回滚后的状态是「两张对账表并存」，这正是这一条迁移要消除的那件事——
-- 回滚之后需要重新决定怎么处置它，而不是当它一直没问题。
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
