-- +goose Up
-- 给 ledger_entries.entry_type 加一档 payout_reject：提现被驳回、冻结解除。
--
-- # 为什么需要新的一档，而不是复用既有的
--
-- 驳回的形状是 PayoutRequest 的**反向**（借 merchant_payable:<商户>、
-- 贷 payout:<提现单id>），而 entryDirections 里没有任何一档允许这个组合：
-- settle 与 payout_request 在 merchant_payable 上只许贷、在 payout 上只许借，
-- refund 的方向表里根本没有 payout 科目，payout_paid 没有 merchant_payable。
--
-- 复用相近的那一档有两条坏处，都不是排版问题：
--
--   * 复用 refund：账面上「驳回了一笔提现」与「退了一笔款」从此同一个
--     entry_type，而这两件事的对账对象不同——退款要对原始消费，
--     驳回要对提现单。更要紧的是 refund 的方向表要为此放开 payout 科目，
--     于是 refund 交易里可以出现提现单的待付，而那不是退款。
--   * 复用 payout_request 并把方向放开成双向：账面分不出「商户申请」与
--     「审批驳回」，而这两件事在审计时的问题完全不同（谁申请的 vs 谁驳的），
--     这正是当初把 settle 与 payout_request 分成两档的理由。
--
-- # 为什么加一档是安全的
--
-- 这是**扩**取值，不是收窄：库里已有的行全部满足新约束，ADD CONSTRAINT
-- 不会拒绝任何存量数据（真拒绝了就是数据里本来有脏值，那正是要报出来的）。
-- 旧二进制 + 新库照常工作，新二进制 + 旧库只在走驳回时写不进去——
-- 迁移与二进制一起发布即可（docs/development.md 的部署顺序）。
--
-- 约束名 ledger_entries_entry_type_check 由 PostgreSQL 按
-- `<表>_<列>_check` 自动生成（00001 里那个 CHECK 写在列定义上，没有起名），
-- 这里按它 DROP 再 ADD。**不写 IF EXISTS**：名字写错时静默留下旧约束、
-- 新取值写不进去，那种失败要到线上第一次驳回到达才现形。
ALTER TABLE ledger_entries DROP CONSTRAINT ledger_entries_entry_type_check;
ALTER TABLE ledger_entries ADD CONSTRAINT ledger_entries_entry_type_check
    CHECK (entry_type IN (
        'topup', 'consume', 'refund', 'settle',
        'payout_request', 'payout_paid', 'payout_reject', 'adjustment'));

-- +goose Down
-- 撤掉 payout_reject 那一档。
--
-- 回滚前必须先确认库里没有该取值的分录：真有的话这条 ADD CONSTRAINT 会
-- 失败并指名是哪一行——那正是应当发生的（删掉一档取值而留下这种行，
-- 等于让账本里出现一个约束不认的类型）。有的话处置方式是先想清楚那些
-- 驳回记录怎么办，而不是删约束。
ALTER TABLE ledger_entries DROP CONSTRAINT ledger_entries_entry_type_check;
ALTER TABLE ledger_entries ADD CONSTRAINT ledger_entries_entry_type_check
    CHECK (entry_type IN (
        'topup', 'consume', 'refund', 'settle',
        'payout_request', 'payout_paid', 'adjustment'));
