-- +goose Up
-- 额度包面值：一类新的对账 finding。
--
-- 背景：00053 把额度包覆盖的请求从钱包那条路上分了出来——那一类请求不冻
-- 钱包（`reserved_amount` 是 0），另记 `plan_reserved_amount` 占订阅的面值。
-- 于是对账原有的两条不变量对它都成立而不报任何东西：`frozen` 与
-- `reserved_amount` 之和都是 0，两边相等。缺的是包这一侧的不变量：
--
--     prepaid:<订阅 id> 的余额 ≥ 该订阅所有 pending 行的 plan_reserved_amount 之和
--
-- 违反它意味着某个订阅承诺出去的额度已经超过它剩下的面值，结算时
-- `prepaid:<订阅 id>` 会被扣成负数（额度包透支）。
--
-- 这一类 finding 的两列与其他几类的关系不同：snapshot 是 `prepaid:<订阅 id>`
-- 的余额（分录之和），ledger 是该订阅的在途预留之和。它们不是「同一笔钱的
-- 两种记录」，而是同一条不等式的两边，所以二者都可能非零，且
-- `ledger > snapshot` 才是报出来的条件（detail 里带订阅 id）。
--
-- 判据、取数为什么必须在一条语句里、以及为什么只报不修，见
-- internal/ledger/store 的 FindingPlanFaceShortfall、readPlanFaces 与
-- comparePlanFaces。

-- kind 的合法取值写在列上的 CHECK 约束里，加一个取值就得换掉这条约束。
--
-- 约束名沿用 PostgreSQL 按 <表>_<列>_check 自动生成的那个，与 00006 同一条
-- 理由：另起一个名字会让两条约束同时存在，而旧的那条照样把新取值挡在外面
-- ——表现为「报出来了，但落库时报约束冲突」，比不报更难查。
ALTER TABLE reconcile_findings DROP CONSTRAINT reconcile_findings_kind_check;
ALTER TABLE reconcile_findings ADD CONSTRAINT reconcile_findings_kind_check
    CHECK (kind IN (
        'balance_mismatch', 'frozen_mismatch',
        'wallet_missing', 'malformed_account',
        'stale_reservation', 'plan_face_shortfall'));

-- 面值类的条数在运行记录上单独一列，与 malformed_accounts / stale_reservations
-- 同理：它既不是余额差额也不是冻结差额（那两类比的是同一个数的两处记录，这一
-- 类比的是同一条不等式的两边），混进那两列里等于把它藏起来。findings 修掉就删，
-- 运行记录是唯一的历史——少一列就等于这一轮少报一档。
ALTER TABLE reconcile_runs ADD COLUMN plan_face_shortfalls integer NOT NULL DEFAULT 0;

COMMENT ON COLUMN reconcile_runs.plan_face_shortfalls IS
    '这一轮报出的「在途预留超过订阅面值」的订阅条数。它不计入余额与冻结两项：那两项比的是同一个数的两处记录，这一类比的是同一条不等式的两边。';

-- +goose Down
-- 先把这一类 finding 删掉再收回约束：留着它们的话，约束加不回去
-- （库里的行不满足新的（旧的）取值集合），down 会失败。
DELETE FROM reconcile_findings WHERE kind = 'plan_face_shortfall';

ALTER TABLE reconcile_findings DROP CONSTRAINT reconcile_findings_kind_check;
ALTER TABLE reconcile_findings ADD CONSTRAINT reconcile_findings_kind_check
    CHECK (kind IN (
        'balance_mismatch', 'frozen_mismatch',
        'wallet_missing', 'malformed_account',
        'stale_reservation'));

ALTER TABLE reconcile_runs DROP COLUMN IF EXISTS plan_face_shortfalls;
