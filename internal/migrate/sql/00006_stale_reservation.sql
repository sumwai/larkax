-- +goose Up
-- 陈旧 pending 预留：一类新的对账 finding，以及它在运行记录里的计数列。
--
-- 背景：预留与结算之间隔着一次可能数分钟的上游调用，而释放只有两条路——
-- 结算那一步解冻，或者网关在失败收尾里调 Release。两条路都没走成时
-- （释放本身失败，或进程在两者之间硬崩），库里就留下一行永久的 pending，
-- 而**没有任何东西会发现它**：对账把 pending 行之和当成「本来就该冻住的
-- 量」，于是那笔钱两边同时存在、对得平、不报错。
--
-- 判据、阈值与「为什么只报不自动释放」见 internal/ledger/store 的
-- staleReservationAfter 与 FindingStaleReservation。

-- kind 的合法取值写在列上的 CHECK 约束里，加一个取值就得换掉这条约束。
--
-- 约束名沿用 PostgreSQL 按 <表>_<列>_check 自动生成的那个：另起一个名字
-- 会让两条约束同时存在，而旧的那条照样把新取值挡在外面——表现为
-- 「报出来了，但落库时报约束冲突」，比不报更难查。
ALTER TABLE reconcile_findings DROP CONSTRAINT reconcile_findings_kind_check;
ALTER TABLE reconcile_findings ADD CONSTRAINT reconcile_findings_kind_check
    CHECK (kind IN (
        'balance_mismatch', 'frozen_mismatch',
        'wallet_missing', 'malformed_account',
        'stale_reservation'));

-- 陈旧预留的条数单独一列，与 malformed_accounts 同理：它既不是余额差额
-- 也不是冻结差额（那两类恰恰一条都不会报——两边是对得上的），混进那两列里
-- 等于把它藏起来。
ALTER TABLE reconcile_runs ADD COLUMN stale_reservations integer NOT NULL DEFAULT 0;

COMMENT ON COLUMN reconcile_runs.stale_reservations IS
    '这一轮报出的陈旧 pending 预留条数。它不计入余额与冻结两项：那两项报的是「两边对不上」，而陈旧预留报的是「这笔钱冻着没人管」，后者两边是对得上的。';

-- +goose Down
-- 先把这一类 finding 删掉再收回约束：留着它们的话，约束加不回去
-- （库里的行不满足新的（旧的）取值集合），down 会失败。
DELETE FROM reconcile_findings WHERE kind = 'stale_reservation';

ALTER TABLE reconcile_findings DROP CONSTRAINT reconcile_findings_kind_check;
ALTER TABLE reconcile_findings ADD CONSTRAINT reconcile_findings_kind_check
    CHECK (kind IN (
        'balance_mismatch', 'frozen_mismatch',
        'wallet_missing', 'malformed_account'));

ALTER TABLE reconcile_runs DROP COLUMN IF EXISTS stale_reservations;
