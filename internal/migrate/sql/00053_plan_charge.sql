-- +goose Up
-- 请求路径从额度包扣费所需要的三列。
--
-- # 两个维度各记一个数
--
-- 额度包「用完」的判据是**任一维度耗尽**：可用量归零，或者面值归零
-- （见 #860）。这两个维度存在两处，各自只有一条写入路径：
--
--   - `user_plans.quota_used` 记**已经消费掉**的可用量（按包自己的单位：
--     金额包不记账、token 包记 token 数、次数包记次数）。它只增不减，
--     由结算在扣钱的那个事务里加一次。已消费与「预留中」必须分开记：
--     合成一个数之后，一笔在途请求结束时无法区分「它本来就占着这些量」
--     与「它是新消费的」，释放与结算就分不清该减多少。
--   - `billing_requests.reserved_quota` 记这笔预留**占着**多少可用量。
--     它跟着预留行走：行从 pending 变成 settled / released 时就自然不再
--     计入「预留中」，不需要另一次扣减。
--
-- 可用量 = `plans.quota_total` − `quota_used` − 该订阅所有 pending 行的
-- `reserved_quota` 之和。这是请求路径判断「这个包还装不装得下本次预留」的
-- 唯一口径，不写成视图：三处（判断、插入、结算）在同一个事务里，口径写在
-- 一处 SQL 常量里比多一个库对象更好追。
--
-- # 面值这一侧为什么要单独一列
--
-- `billing_requests.reserved_amount` 的含义是「从**钱包**冻住了多少钱」，
-- 对账拿它与 `user_wallets.frozen` 对（见 reconcile 的 readPendingReservations）。
-- 额度包覆盖的请求**不动钱包**：动钱包的话，一个余额为零、只有额度包的用户
-- 会被「可用余额不足」挡在门外。所以那一列必须保持 0，而这笔请求同样占着
-- 包的面值——用 `plan_reserved_amount` 记它，对账的两个不变量因此都还成立：
--
--   - 钱包：`frozen == Σ pending.reserved_amount`（额度包的行贡献 0）；
--   - 包面值：`prepaid 科目余额 ≥ Σ pending.plan_reserved_amount`（结算把
--     实扣金额写成分录，预留行同时不再 pending，两边同进同退）。
--
-- 面值可用量 = prepaid 科目余额 − 该订阅所有 pending 行的
-- `plan_reserved_amount` 之和。
--
-- # 快照
--
-- `billing_requests.user_plan_id` 是**预留那一刻**选中的订阅，结算用它，
-- 不重新选：在途期间订阅到期、用户新增一个优先级更高的包，都不影响这笔账。
-- 为空表示这笔预留走的是账户余额。
ALTER TABLE user_plans
    ADD COLUMN quota_used numeric(20, 8) NOT NULL DEFAULT 0
        CHECK (quota_used >= 0);
COMMENT ON COLUMN user_plans.quota_used IS
    '已经消费掉的可用量，按包的单位计（金额包恒为 0：面值那一侧由账本记）。只增不减，'
    '由结算在扣钱的同一个事务里加上；预留中的量不在这里，见 billing_requests.reserved_quota';

ALTER TABLE billing_requests
    ADD COLUMN user_plan_id bigint REFERENCES user_plans(id),
    ADD COLUMN reserved_quota numeric(20, 8) NOT NULL DEFAULT 0
        CHECK (reserved_quota >= 0),
    ADD COLUMN plan_reserved_amount numeric(20, 8) NOT NULL DEFAULT 0
        CHECK (plan_reserved_amount >= 0);
COMMENT ON COLUMN billing_requests.user_plan_id IS
    '预留时选中的额度包订阅；为空表示这次从账户余额扣。结算按它记 prepaid 科目，不重新选';
COMMENT ON COLUMN billing_requests.reserved_quota IS
    '这笔预留占着的可用量（token 数或次数）。可用量 = quota_total − quota_used − 本列之和';
COMMENT ON COLUMN billing_requests.plan_reserved_amount IS
    '这笔预留占着的额度包面值。它**不在** user_wallets.frozen 里：额度包覆盖的请求不动钱包';

-- 「这个订阅还有多少在途」是请求路径每次选源都要算的，而它只关心 pending 行。
-- 部分索引把它收得只剩在途的那些；谓词写成 status = 'pending' 与查询里的条件
-- 逐字一致，规划器才用得上。
CREATE INDEX billing_requests_pending_plan_idx
    ON billing_requests (user_plan_id)
    WHERE status = 'pending' AND user_plan_id IS NOT NULL;

-- +goose Down
-- 回滚前确认库里没有额度包覆盖过的请求：那几行把 user_plan_id 与两个预留数
-- 写进去了，回滚会一起丢掉。丢掉之后这些行仍然是 settled / released，账本分录
-- 还在，但「这笔钱是从哪个订阅扣的」就再也读不出来了。先按下面的提示核对。
-- +goose StatementBegin
DO $$
DECLARE
    leftover bigint;
BEGIN
    SELECT count(*) INTO leftover
      FROM billing_requests WHERE user_plan_id IS NOT NULL;
    IF leftover > 0 THEN
        RAISE EXCEPTION '有 % 条账单行是从额度包扣的，00053 不能回滚。', leftover
          USING HINT = '这些行回滚后会失去「从哪个订阅扣」的关联。先按 request_id 导出'
                       '（SELECT request_id, user_plan_id, reserved_quota, plan_reserved_amount '
                       'FROM billing_requests WHERE user_plan_id IS NOT NULL），再回滚。';
    END IF;
END
$$;
-- +goose StatementEnd

DROP INDEX billing_requests_pending_plan_idx;
ALTER TABLE billing_requests
    DROP COLUMN plan_reserved_amount,
    DROP COLUMN reserved_quota,
    DROP COLUMN user_plan_id;
ALTER TABLE user_plans DROP COLUMN quota_used;
