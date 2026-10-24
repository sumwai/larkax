-- +goose Up
-- 额度包期末按比例结转。
--
-- 到期时剩余面值按 settings.plans.carryover_bp 留下的一部分转进
-- `carryover:<源订阅 id>`，等用户下次领到同币种、同计量单位的新包时并进那条
-- 新订阅的 `prepaid:<新订阅 id>`。没结转的部分照旧归平台。
--
-- # 为什么是四个列
--
-- 一条订阅有两个与结转有关的时刻，两边的数含义相反：
--
--   - `carried_*`（进来的）：领到这条订阅时，从旧的待并入额度里并进来了多少。
--     只有活跃订阅上有意义，也是期末再结转时基数公式的减数——并进来的量不该
--     再结转第二次。
--   - `carryout_*`（出去的）：这条订阅期末结转出去、等下次领包时并入新订阅的
--     多少。只在终态且确实结转了的行上非零。
--
-- 合成两列、靠 status 分义也能存下，但任何不带状态过滤的查询都会把两个方向的
-- 数字加到一起，而那正是这个功能最容易被读错的地方。四个列各自只有一种含义。
--
-- # 金额的权威值在账本，量的权威值在这一行
--
-- 金额那一维以 `carryover:<源订阅 id>` 的**账本余额**为准，`carryout_amount`
-- 只是那一刻的镜像。量那一维（token / 次数）账本里没有对应字段——分录只有
-- asset，没有 unit——所以 `carryout_quota` 不是审计字段，并入时**必须**从它读。
--
-- # 约束
--
-- 四个数都不允许为负：结转的是「还剩多少」，负数没有任何业务含义，只可能是
-- 写错了方向。默认 0 让存量行（迁移之前的订阅）不必回填，它们的语义正是
-- 「这一边没有发生过结转」。
ALTER TABLE user_plans
    ADD COLUMN carried_quota numeric(20, 8) NOT NULL DEFAULT 0
        CHECK (carried_quota >= 0),
    ADD COLUMN carried_amount numeric(20, 8) NOT NULL DEFAULT 0
        CHECK (carried_amount >= 0),
    ADD COLUMN carryout_quota numeric(20, 8) NOT NULL DEFAULT 0
        CHECK (carryout_quota >= 0),
    ADD COLUMN carryout_amount numeric(20, 8) NOT NULL DEFAULT 0
        CHECK (carryout_amount >= 0);

COMMENT ON COLUMN user_plans.carried_quota IS
    '领到这条订阅时，从旧的待并入结转里并进来的可用量（token 数或次数）。'
    '期末再结转时是基数公式的减数：并进来的量不再结转第二次';
COMMENT ON COLUMN user_plans.carried_amount IS
    '领到这条订阅时并进来的结转面值，与 carried_quota 同时写。'
    '期末再结转时是面值基数公式的减数';
COMMENT ON COLUMN user_plans.carryout_quota IS
    '这条订阅期末结转出去、等下次领包时并入新订阅的可用量。'
    '账本没有 unit，并入时从这里读，所以它不是审计字段';
COMMENT ON COLUMN user_plans.carryout_amount IS
    '这条订阅期末结转出去的面值。金额的权威值是 carryover:<订阅 id> 的账本余额，'
    '本列只是那一刻的镜像';

-- 账本多两档分录：期末结转与结转并入。
--
-- 结转：贷 prepaid:<源订阅>、借 carryover:<源订阅>，两边的后缀是同一个订阅。
-- 并入：贷 carryover:<源订阅>（可以多条，每个源一条）、借 prepaid:<新订阅>。
--
-- 与 00049 同一个形状：扩取值，存量行全部满足新约束。约束名由 PostgreSQL
-- 按 `<表>_<列>_check` 自动生成，这里按它 DROP 再 ADD。
ALTER TABLE ledger_entries DROP CONSTRAINT ledger_entries_entry_type_check;
ALTER TABLE ledger_entries ADD CONSTRAINT ledger_entries_entry_type_check
    CHECK (entry_type IN (
        'topup', 'consume', 'refund', 'settle',
        'payout_request', 'payout_paid', 'payout_reject',
        'plan_grant', 'plan_void', 'plan_carry', 'plan_carry_merge', 'adjustment'));

-- 期末结转比例，万分点。0 表示不结转（整笔归平台，与这个功能出现之前一致），
-- 10000 表示全部结转。默认 40%：既让用户感到续费有回报，也不至于把一整份
-- 没用完的额度全带走。
--
-- 读点是 internal/plans 的 carryoverBasisPoints，只在期末作废时现读一次：
-- 改完下一次期末处置就生效，不需要重启。
INSERT INTO settings (key, value, description, editable)
VALUES ('plans.carryover_bp', '4000',
        '额度包期末按比例结转的万分点：0 = 不结转（整笔归平台），10000 = 全部结转。'
        '只在到期作废时生效；每次期末处置现读一次，改完下一次生效。',
        true);

-- +goose Down
-- 回滚前先确认库里没有结转留下的东西。有的话下面两条 DDL 会失败，但 PostgreSQL
-- 报的是「某些行违反约束」，看不出下一步该做什么——所以先自己查一遍。
--
-- 处置不是删约束：那会让账本里留下一批约束不认的类型，而四列里也留着没人读的
-- 数。唯一的路是把 plan_carry / plan_carry_merge 两类分录删掉（连同 carryover
-- 科目上的余额），并把四列清零，再回滚。
--
-- DO 块里有分号，要包在 StatementBegin/StatementEnd 里，否则 goose 会按分号
-- 把这一句切成两半。
-- +goose StatementBegin
DO $$
DECLARE
    entries bigint;
    carried bigint;
BEGIN
    SELECT count(*) INTO entries
      FROM ledger_entries WHERE entry_type IN ('plan_carry', 'plan_carry_merge');
    SELECT count(*) INTO carried
      FROM user_plans
     WHERE carried_quota <> 0 OR carried_amount <> 0
        OR carryout_quota <> 0 OR carryout_amount <> 0;
    IF entries > 0 OR carried > 0 THEN
        RAISE EXCEPTION '库里还有 % 条结转分录、% 条订阅带着结转数值，00065 不能回滚。'
            '把 plan_carry / plan_carry_merge 分录连同 carryover 科目的余额一起删掉，'
            '再把 user_plans 那四列清零，重跑回滚。', entries, carried
          USING HINT = 'SELECT id, txn_id, account, asset, amount, entry_type FROM ledger_entries '
                       'WHERE entry_type IN (''plan_carry'', ''plan_carry_merge'');';
    END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE ledger_entries DROP CONSTRAINT ledger_entries_entry_type_check;
ALTER TABLE ledger_entries ADD CONSTRAINT ledger_entries_entry_type_check
    CHECK (entry_type IN (
        'topup', 'consume', 'refund', 'settle',
        'payout_request', 'payout_paid', 'payout_reject',
        'plan_grant', 'plan_void', 'adjustment'));

DELETE FROM settings WHERE key = 'plans.carryover_bp';

ALTER TABLE user_plans
    DROP COLUMN carryout_amount,
    DROP COLUMN carryout_quota,
    DROP COLUMN carried_amount,
    DROP COLUMN carried_quota;
