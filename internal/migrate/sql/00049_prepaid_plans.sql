-- +goose Up
-- 额度包：一份预存的服务量 + 一个有效期 + 一组可用的模型。
--
-- # 三张表里两张已经存在
--
-- 00001 建过 `plans` 与 `plan_subscriptions`，那是「套餐目录」的草稿，全仓零读零写。
-- 这次按额度包的需要改它们，而不是再建一套同名的新表：同一个概念在库里只留一份
--（见 LESSONS.md 第 40 条）。`plan_models` 是新的。
--
-- # 三种计量单位
--
-- `unit` 决定 `quota_total` 怎么读：金额、token 数、调用次数。定价永远只有一套
--（按 token 的单价），计量单位只决定这个包装了多少、还剩多少。
--
-- # 账本上每个包只有一个数：钱
--
-- `face_value` 是进 `prepaid:<订阅 id>` 的面值，也是这个包值多少钱。金额包的可用量
-- 与面值是同一个数（`plans_amount_quota_matches_face_value`），token / 次数包的两个
-- 数各自独立：1 万 token 在贵模型与便宜模型上值不同的钱，所以运营要分别填。
--
-- 两列不合并成「金额包就用 face_value 当可用量」：可用量按包自己的单位记（token、
-- 次数），面值永远是钱，二者在金额包上恰好相等，在另外两种包上必须分开。合并了
-- 之后一加 token / 次数包就得再把它们拆开，拆的代价是重写所有读这两列的地方。
--
-- `quota_total` 与 `face_value` 都不给默认值：0 必然违反各自的 CHECK，一个用不上的
-- 默认值只会让后来人以为「不给值也能建包」。列注释在下面（COMMENT ON COLUMN）。
ALTER TABLE plans
    ADD COLUMN unit text NOT NULL DEFAULT 'amount'
        CHECK (unit IN ('amount', 'token', 'request')),
    ADD COLUMN quota_total numeric(20, 8) NOT NULL
        CHECK (quota_total > 0),
    ADD COLUMN face_value numeric(20, 8) NOT NULL
        CHECK (face_value > 0);
ALTER TABLE plans ADD CONSTRAINT plans_amount_quota_matches_face_value
    CHECK (unit <> 'amount' OR quota_total = face_value);
COMMENT ON COLUMN plans.quota_total IS
    '按 unit 计的可用量。金额包与 face_value 同值（由 plans_amount_quota_matches_face_value 保证）；'
    'token / 次数包与 face_value 各自独立，不要互相推导';
COMMENT ON COLUMN plans.face_value IS
    '进 prepaid 科目的面值（这个包值多少钱）。金额包与 quota_total 同值；'
    'token / 次数包另有按单位计的可用量，两者不相等';

-- 每个额度包允许使用的模型，按规范模型名精确匹配（与 pricing_rules.model_patterns
-- 同一个键）。token / 次数包至少要有一行，金额包可以为空（空 = 不限模型）。
-- 「至少一行」这条不写成 CHECK：它跨表，CHECK 看不见；由写入路径校验，见
-- internal/plans 的 validateScope。
CREATE TABLE plan_models (
    plan_id         bigint NOT NULL REFERENCES plans(id) ON DELETE CASCADE,
    model           text NOT NULL CHECK (model <> ''),
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (plan_id, model)
);
CREATE INDEX plan_models_model_idx ON plan_models (model);

-- 订阅：用户在某段时间拥有某个额度包。表名沿用任务里的 user_plans，
-- 形状沿用 plan_subscriptions 的 status / period_start / period_end。
ALTER TABLE plan_subscriptions RENAME TO user_plans;
ALTER INDEX plan_subscriptions_user_idx RENAME TO user_plans_user_idx;

-- `ref` 是外部发放单号，发放的幂等键；`void_ref` 是作废单号，作废的幂等键。
-- 两者都唯一：同一个单号重放时返回同一行，而不是再发一份。
--
-- `voided_at` 是作废发生的时刻。剩余面值为零时作废不写分录，状态也只有一个
-- 终态值——没有这个时间戳，事后只能翻日志才知道是什么时候作废的。
--
-- 「用掉了多少」那一列不在这里：它随扣费路径一起落（见 #860），今天没有任何代码
-- 写它，先建出来就是一条恒为空的列。
ALTER TABLE user_plans
    ADD COLUMN ref text NOT NULL,
    ADD COLUMN void_ref text,
    ADD COLUMN voided_at timestamptz;
CREATE UNIQUE INDEX user_plans_ref_key ON user_plans (ref);
CREATE UNIQUE INDEX user_plans_void_ref_key ON user_plans (void_ref);

-- `status` 的取值把 cancelled 换成 canceled：本仓已经为这个拼写分歧改过一次
--（00016 的 client_canceled_at）。这张表此前是草稿、一行数据都没有，所以这里
-- 可以直接换，不必留兼容值——写入端只有 internal/plans 一处。
ALTER TABLE user_plans DROP CONSTRAINT plan_subscriptions_status_check;
ALTER TABLE user_plans ADD CONSTRAINT plan_subscriptions_status_check
    CHECK (status IN ('active', 'expired', 'canceled'));

-- 00001 里同批建的草稿列：今天没有任何代码读写它们。保留原样（删列是另一件事），
-- 但要让读库的人一眼看出来，而不是以为它们已经生效。
COMMENT ON COLUMN plans.dimensions IS
    '草稿列：00001 建的套餐目录遗留，今天没有任何代码读写';
COMMENT ON COLUMN plans.price IS
    '草稿列：00001 建的套餐目录遗留，今天没有任何代码读写；额度包的价格在 face_value';
COMMENT ON COLUMN plans.billing_period IS
    '草稿列：00001 建的套餐目录遗留，今天没有任何代码读写；额度包没有计费周期';
COMMENT ON COLUMN plans.owner_user_id IS
    '草稿列：00001 建的套餐目录遗留，今天没有任何代码读写；额度包没有归属用户';
COMMENT ON COLUMN user_plans.quota_state IS
    '草稿列：00001 建的套餐目录遗留，今天没有任何代码读写';
COMMENT ON COLUMN user_plans.billing_preference IS
    '草稿列：00001 建的套餐目录遗留，今天没有任何代码读写';

-- 账本多两档分录：额度包授予、额度包作废（到期或人工作废）。
--
-- 授予：借 prepaid:<订阅 id>、贷 platform_revenue。
-- 作废：借 platform_revenue、贷 prepaid:<订阅 id>。
--
-- 消费不新增一档：它与既有的 consume 只差用户侧的科目（user → prepaid），
-- 商户分成与平台收入那两条照旧，见 ledger.Consume。
--
-- 与 00037 同一个形状：这是扩取值，存量行全部满足新约束。约束名由 PostgreSQL
-- 按 `<表>_<列>_check` 自动生成，这里按它 DROP 再 ADD。
ALTER TABLE ledger_entries DROP CONSTRAINT ledger_entries_entry_type_check;
ALTER TABLE ledger_entries ADD CONSTRAINT ledger_entries_entry_type_check
    CHECK (entry_type IN (
        'topup', 'consume', 'refund', 'settle',
        'payout_request', 'payout_paid', 'payout_reject',
        'plan_grant', 'plan_void', 'adjustment'));

-- +goose Down
-- 回滚前先确认库里没有 plan_grant / plan_void 的分录。有的话下面那条
-- ADD CONSTRAINT 会失败，但 PostgreSQL 报的是「某些行违反约束」，看不出下一步
-- 该做什么——所以先自己查一遍，报出行数与处置办法。
--
-- 处置不是删约束：那会让账本里留下一批约束不认的类型。**作废订阅并不够**——
-- Void 只新增 plan_void 分录，plan_grant 那几行永远留着。所以唯一的路是把
-- plan_grant / plan_void 两类分录都删掉（连同对应的 prepaid 科目余额一起，
-- 否则账本的科目余额会对不上），再回滚。
--
-- DO 块里有分号，要包在 StatementBegin/StatementEnd 里，否则 goose 会按分号
-- 把这一句切成两半。
-- +goose StatementBegin
DO $$
DECLARE
    leftover bigint;
BEGIN
    SELECT count(*) INTO leftover
      FROM ledger_entries WHERE entry_type IN ('plan_grant', 'plan_void');
    IF leftover > 0 THEN
        RAISE EXCEPTION '库里还有 % 条 plan_grant / plan_void 分录，00049 不能回滚。'
            '把这些分录连同 prepaid 科目的余额一起删掉，再重跑回滚——作废订阅不够，'
            'plan_grant 那几行仍在。', leftover
          USING HINT = 'SELECT id, txn_id, account, asset, amount, entry_type FROM ledger_entries '
                       'WHERE entry_type IN (''plan_grant'', ''plan_void'');';
    END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE ledger_entries DROP CONSTRAINT ledger_entries_entry_type_check;
ALTER TABLE ledger_entries ADD CONSTRAINT ledger_entries_entry_type_check
    CHECK (entry_type IN (
        'topup', 'consume', 'refund', 'settle',
        'payout_request', 'payout_paid', 'payout_reject', 'adjustment'));

COMMENT ON COLUMN plans.dimensions IS NULL;
COMMENT ON COLUMN plans.price IS NULL;
COMMENT ON COLUMN plans.billing_period IS NULL;
COMMENT ON COLUMN plans.owner_user_id IS NULL;
COMMENT ON COLUMN user_plans.quota_state IS NULL;
COMMENT ON COLUMN user_plans.billing_preference IS NULL;

ALTER TABLE user_plans DROP CONSTRAINT plan_subscriptions_status_check;
ALTER TABLE user_plans ADD CONSTRAINT plan_subscriptions_status_check
    CHECK (status IN ('active', 'expired', 'cancelled'));

DROP INDEX user_plans_void_ref_key;
DROP INDEX user_plans_ref_key;
ALTER TABLE user_plans
    DROP COLUMN voided_at,
    DROP COLUMN void_ref,
    DROP COLUMN ref;
ALTER INDEX user_plans_user_idx RENAME TO plan_subscriptions_user_idx;
ALTER TABLE user_plans RENAME TO plan_subscriptions;

DROP INDEX plan_models_model_idx;
DROP TABLE plan_models;

ALTER TABLE plans DROP CONSTRAINT plans_amount_quota_matches_face_value;
ALTER TABLE plans
    DROP COLUMN face_value,
    DROP COLUMN quota_total,
    DROP COLUMN unit;
