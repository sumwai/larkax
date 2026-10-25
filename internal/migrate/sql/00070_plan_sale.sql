-- +goose Up
-- 额度包售卖：用户自己买一个包，付款通知到达时发放订阅。
--
-- # 三个列与一档分录
--
-- `plans.validity_days` 是这个包卖出去时给订阅的天数。空表示**不可售卖**——
-- 不是「永久有效」：运营发放走 `plan grant`，有效期是发放时给的，与包无关；
-- 而售卖要在下单那一刻就把「这个包卖几天」定下来，所以天数必须长在包上。
--
-- `payment_orders.plan_key` 让一张支付单记下它买的是哪个包。空表示充值，与这个
-- 功能出现之前逐字相同。外键指 `plans.key`：包删不掉（只有 disable），所以这张单
-- 永远能回答它买的是哪一件商品。
--
-- `plan_purchase` 是售卖那一笔的分录类型。它与 `plan_grant` 的科目**不同**：
-- 赠送的对方科目是平台收入（平台让出一份收入），售卖的对方科目是支付通道
-- （钱真的进来了）。合成一档会让「这份额度是送的还是卖的」在账本上消失，而
-- 收入确认的口径正落在这个区别上。
ALTER TABLE plans
    ADD COLUMN validity_days integer CHECK (validity_days > 0);
COMMENT ON COLUMN plans.validity_days IS
    '售卖时给订阅的有效期天数。空表示这个包不出售；运营发放的有效期与它无关';

ALTER TABLE payment_orders
    ADD COLUMN plan_key text REFERENCES plans(key);
COMMENT ON COLUMN payment_orders.plan_key IS
    '这张支付单买的额度包（plans.key）。空表示充值';

-- 下单那一刻记下的售卖天数。到期时刻由它与收款时刻算出，**入账时不再读包上的值**。
--
-- 为什么不直接记到期时刻：下单时钱还没付，`paid_at` 还不知道，算不出 `paid_at +
-- validity_days`。而天数在下单那一刻就定了，于是「用户按多少天付的钱」这件事从
-- 此刻起只由这张单回答。包后来被改成 365 天也不会让这一单多给 11 个月。
--
-- 充值的单子是 NULL；可售的包天数一定为正（plans.validity_days 上有 CHECK）。
ALTER TABLE payment_orders
    ADD COLUMN validity_days integer
    CONSTRAINT payment_orders_validity_days_check
    CHECK (validity_days IS NULL OR validity_days > 0);
COMMENT ON COLUMN payment_orders.validity_days IS
    '这张单买的额度包在下单时的售卖天数。空表示充值；到期时刻 = 收款时刻 + 它';

-- 与 00065 同一个形状：扩取值，存量行全部满足新约束。约束名由 PostgreSQL
-- 按 `<表>_<列>_check` 自动生成，这里按它 DROP 再 ADD。
ALTER TABLE ledger_entries DROP CONSTRAINT ledger_entries_entry_type_check;
ALTER TABLE ledger_entries ADD CONSTRAINT ledger_entries_entry_type_check
    CHECK (entry_type IN (
        'topup', 'consume', 'refund', 'settle',
        'payout_request', 'payout_paid', 'payout_reject',
        'plan_grant', 'plan_void', 'plan_carry', 'plan_carry_merge',
        'plan_purchase', 'adjustment'));

-- +goose Down
-- 回滚前先确认库里没有售卖留下的东西：有 plan_purchase 分录时，下面那条
-- 收窄的 CHECK 会失败，而 PostgreSQL 报的是「某些行违反约束」，看不出下一步
-- 该做什么。所以先自己查一遍。
--
-- DO 块里有分号，要包在 StatementBegin/StatementEnd 里，否则 goose 会按分号
-- 把这一句切成两半。
-- +goose StatementBegin
DO $$
DECLARE
    entries bigint;
BEGIN
    SELECT count(*) INTO entries
      FROM ledger_entries WHERE entry_type = 'plan_purchase';
    IF entries > 0 THEN
        RAISE EXCEPTION '库里还有 % 条 plan_purchase 分录，00070 不能回滚。'
            '把售卖产生的分录连同 user_plans 里对应的订阅一起删掉，重跑回滚。', entries
          USING HINT = 'SELECT id, txn_id, account, asset, amount FROM ledger_entries '
                       'WHERE entry_type = ''plan_purchase'';';
    END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE ledger_entries DROP CONSTRAINT ledger_entries_entry_type_check;
ALTER TABLE ledger_entries ADD CONSTRAINT ledger_entries_entry_type_check
    CHECK (entry_type IN (
        'topup', 'consume', 'refund', 'settle',
        'payout_request', 'payout_paid', 'payout_reject',
        'plan_grant', 'plan_void', 'plan_carry', 'plan_carry_merge', 'adjustment'));

ALTER TABLE payment_orders DROP COLUMN plan_key;
ALTER TABLE payment_orders DROP COLUMN validity_days;

ALTER TABLE plans DROP COLUMN validity_days;
