-- +goose Up
-- 科目余额下界：一类新的对账 finding。
--
-- 背景：00065 给额度包期末结转加了 `carryover:<源订阅 id>` 科目。前四类检查
-- 一条都看不见它——余额类只比 `user%` 科目与钱包，冻结类比的是预留表，畸形
-- 科目只扫 `user%`，面值类只比 `prepaid`。于是它被扣成负数时，账本上没有任何
-- 一处会报，而负数意味着并出去的比结转进来的多：一份额度被凭空多并了出去。
--
-- 这一类 finding 的两列与其他几类的关系又不同：snapshot 是**下界**（目前恒为
-- 0），ledger 是该科目的账本余额，`ledger < snapshot` 才是报出来的条件。
-- 判据、下界表与取数方式见 internal/ledger/store 的 FindingAccountBelowFloor
-- 与 reconcile_floor.go。
--
-- kind 的合法取值写在列上的 CHECK 约束里，加一个取值就得换掉这条约束。
-- 约束名沿用 PostgreSQL 按 <表>_<列>_check 自动生成的那个，与 00006 / 00054
-- 同一条理由：另起一个名字会让两条约束同时存在，而旧的那条照样把新取值挡在
-- 外面——表现为「报出来了，但落库时报约束冲突」，比不报更难查。
ALTER TABLE reconcile_findings DROP CONSTRAINT reconcile_findings_kind_check;
ALTER TABLE reconcile_findings ADD CONSTRAINT reconcile_findings_kind_check
    CHECK (kind IN (
        'balance_mismatch', 'frozen_mismatch',
        'wallet_missing', 'malformed_account',
        'stale_reservation', 'plan_face_shortfall',
        'account_below_floor'));

-- 下界类的条数在运行记录上单独一列，与 malformed_accounts 那几列同理：
-- findings 修掉就删，运行记录是唯一的历史，少一列就等于这一轮少报一档。
ALTER TABLE reconcile_runs ADD COLUMN account_below_floors integer NOT NULL DEFAULT 0;

COMMENT ON COLUMN reconcile_runs.account_below_floors IS
    '这一轮报出的「科目余额低于所属大类下界」的科目条数。它不并入余额与冻结两项：那两项比的是同一个数的两处记录，这一项问的是单个科目的余额有没有越过下界。';

-- +goose Down
-- 先把这一类 finding 删掉再收回约束：留着它们的话，约束加不回去
-- （库里的行不满足新的（旧的）取值集合），down 会失败。
DELETE FROM reconcile_findings WHERE kind = 'account_below_floor';

ALTER TABLE reconcile_findings DROP CONSTRAINT reconcile_findings_kind_check;
ALTER TABLE reconcile_findings ADD CONSTRAINT reconcile_findings_kind_check
    CHECK (kind IN (
        'balance_mismatch', 'frozen_mismatch',
        'wallet_missing', 'malformed_account',
        'stale_reservation', 'plan_face_shortfall'));

ALTER TABLE reconcile_runs DROP COLUMN IF EXISTS account_below_floors;
