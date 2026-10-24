-- +goose Up
-- 同一个通道流水号只能落在一张支付单上。
--
-- 通道的通知里带着它自己那一侧的流水号（gateway_txn_id）。正常情形下一笔付款
-- 只有这一个号，重复的通知带着同一个号回到同一张单上；两份通知把同一个号打到
-- 两张不同的单上时（通道重号、通知发错单号），两张单都会记成「已支付」，账本
-- 就多入一笔，而事后分不出哪一笔是错的。
--
-- 这道判据放在库里而不是 Go 侧：通知按单号加锁（Store.WithOrderLock），它只把
-- 同一张单的通知串起来，两张不同的单并发入账时谁也不挡谁——真正守住这一条的
-- 只有下面这条约束。
--
-- # 为什么是部分索引
--
-- gateway_txn_id 为空表示「通道还没有给流水号」：下单那一刻写 NULL，有的通道
-- 付款后也可能不给。空值不参与判重——否则第一张还没付的单子就会挡住之后所有
-- 同样没有流水的单子。部分索引（WHERE gateway_txn_id IS NOT NULL）让这一类行
-- 完全不进索引；也因此 `gateway_txn_id IS NULL` 的行在这个索引里查不到，它
-- 顶替不了下面那条被删掉的普通索引（见下一节）。
--
-- # 同时删掉 payment_orders_gateway_idx
--
-- 00001 里那条普通索引 (gateway, gateway_txn_id) 建在同一组列上。全树（含测试
-- 辅助与脚本）扫过一遍：没有只按 gateway 过滤 payment_orders 的查询，也没有按
-- gateway_txn_id 查这一列的地方——它只在读回整行时出现，以及被这条新索引判重
-- 时出现。留着它就是同一组列的第二份索引，只增加写入与磁盘开销。
--
-- 将来若真的出现「按 gateway 取这张表的全部单子」的查询，它落不进这个部分
-- 索引（部分索引不含 NULL 行，而那种查询要看全部行）。那时应当另建一条只按
-- gateway 的索引，而不是把这一条恢复回来。
--
-- # 为什么不用 CREATE INDEX CONCURRENTLY
--
-- 同 00063：CONCURRENTLY 不能跑在事务里，而迁移器默认把每条迁移包在事务里。
-- 普通写法拿的 ShareLock 挡写不挡读。这张表的写只有下单与通知入账两件事
-- （后者含转 paid 与转 failed / expired 两次状态转换），都是单行写，也都不在
-- 推理请求路径上；构建期间它们短暂排队，不影响调用方拿到的结论。
--
-- # 上线前的一次性核对
--
-- 库里已经有重复的 (gateway, gateway_txn_id) 时，这条迁移会失败而不是静默
-- 绕过。部署前先查一遍：
--
--   SELECT gateway, gateway_txn_id, count(*),
--          array_agg(order_no ORDER BY order_no) AS orders
--     FROM payment_orders
--    WHERE gateway_txn_id IS NOT NULL
--    GROUP BY 1, 2 HAVING count(*) > 1;
--
-- 有结果就先人工核那几张单子（多半是重复入账），再跑迁移。

CREATE UNIQUE INDEX payment_orders_gateway_txn_uniq_idx
    ON payment_orders (gateway, gateway_txn_id)
    WHERE gateway_txn_id IS NOT NULL;

DROP INDEX payment_orders_gateway_idx;

-- +goose Down
CREATE INDEX payment_orders_gateway_idx ON payment_orders (gateway, gateway_txn_id);
DROP INDEX payment_orders_gateway_txn_uniq_idx;
