-- +goose Up
-- 一把 key 能用用户的哪些额度包订阅，以及一次请求按什么次序挑从哪儿扣。
--
-- # 空集合 = 全部
--
-- `api_key_plans` 里没有这把 key 的行，就是「这把 key 用得了用户名下全部订阅」。
-- 这一条不引入哨兵行，也不加「是否受限」的开关列：多一个哨兵会让「没有行」与
-- 「有行但为空」讲两件事，而它们在本仓的语义里是同一件事（见 #860）。读侧按
-- 「没有行就是全部」解释，见 internal/plans 的 APIKeyPlans。
--
-- # 两个外键都随宿主行删除
--
-- 删掉 key 或删掉订阅之后，关联行跟着没，读侧不会读到悬空行。归属不在这张表上
-- 抄一份 user_id：它会与 api_keys.user_id 分家，而写侧校验时那份副本恰好是
-- 「这个订阅是不是这个用户的」的唯一判据。
CREATE TABLE api_key_plans (
    api_key_id   bigint NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
    user_plan_id bigint NOT NULL REFERENCES user_plans(id) ON DELETE CASCADE,
    created_at   timestamptz NOT NULL DEFAULT now(),
    -- 同一对写两次没有意义；主键同时是「一把 key 用了哪些订阅」的索引。
    PRIMARY KEY (api_key_id, user_plan_id)
);
-- 反过来那半边：删订阅时级联要走的索引，也是「这个订阅被哪些 key 引用」的入口。
CREATE INDEX api_key_plans_user_plan_idx ON api_key_plans (user_plan_id);

-- 用户级的扣费顺序。NULL = 跟随平台默认（settings 的 plans.charge_order）。
--
-- 取值与 internal/plans 的 ChargeOrder 一致：先额度包后余额、先余额后额度包、
-- 只走余额。写入侧校验在 plans.SetUserChargeOrder，库里这一条 CHECK 是第二道
-- 守卫——绕过写入路径的手工修数也写不进认不出的值。约束显式起名：读侧用例要摘掉
-- 它才能造出「库里已经有未知取值」的状态（见 internal/plans 的 chargeorder_test.go）。
ALTER TABLE users ADD COLUMN charge_order text
    CONSTRAINT users_charge_order_check
    CHECK (charge_order IN ('plan_first', 'balance_first', 'balance_only'));
COMMENT ON COLUMN users.charge_order IS
    '一次请求按什么次序挑从哪儿扣。NULL = 跟随平台默认（settings.plans.charge_order）';

-- 平台默认的扣费顺序。它不需要在连上数据库之前拿到（见 AGENTS.md 的
-- 「配置的分界线」），所以放 settings 表，不放环境变量：备份恢复、换机器时
-- 它会跟着数据一起回来。
--
-- 取值的含义见 internal/plans：plan_first 先额度包后余额（平台默认，与
-- #860 里「先到期的先用 → 赠送先于付费 → 额度包先于账户余额」一致），
-- balance_first 先余额后额度包，balance_only 只走余额。
INSERT INTO settings (key, value, description, editable) VALUES
    ('plans.charge_order', '"plan_first"',
     '平台默认的扣费顺序：plan_first 先额度包后余额、balance_first 先余额后额度包、balance_only 只走余额。用户可以在 users.charge_order 上覆盖，NULL 表示跟随这里，用户那一行认不出时也回落到这里；这一行本身缺失、不是 JSON 或取值认不出时才回落到兜底值 plan_first。两种回落都记一条日志。', true);

-- +goose Down
DELETE FROM settings WHERE key = 'plans.charge_order';
ALTER TABLE users DROP COLUMN charge_order;
DROP INDEX api_key_plans_user_plan_idx;
DROP TABLE api_key_plans;
