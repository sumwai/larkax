-- +goose Up
-- 给 billing_requests 加一列 computed_amount：这次结算**按实际用量算出来**的金额，
-- 未截断。它与 final_amount 的关系是
--
--     final_amount = min(computed_amount, reserved_amount)
--
-- 于是「这次结算撞上了预留上界」这件事在库里第一次有了判据：
-- computed_amount > reserved_amount。撞上界时两者差出来的那一截，正是被
-- 预留截住、由平台承担掉的金额。
--
-- # 为什么不只记一个 capped boolean
--
-- 布尔只回答「有没有撞」，答不出「撞了多少」——而后者才是判断这次上界错得
-- 离谱还是擦边所需要的信息：差 0.000001 说明预留估得刚好，差一个数量级说明
-- 上界的构成里漏了一整项（例如某条规则把 cache_read 写成 prompt 的几倍）。
-- 两种情况该做的处置完全不同，而 boolean 把它们记成同一个值。
--
-- 反过来，computed_amount 与 reserved_amount 的**比较结果**就是那个 boolean，
-- 所以再加一列是同一件事记两遍：两列同义信息迟早会不一致，而不一致时没有
-- 任何东西会报错。留一列就够。
--
-- # 为什么可空、没有默认值
--
-- 与 final_amount 同一个口径：NULL 表示「这次请求还没有结算金额」，不是 0。
-- 写成 NOT NULL DEFAULT 0 会让每一行 pending 的预留看起来都「算出过 0 元」，
-- 而 0 ≤ reserved_amount，于是一行还没结算的记录会被判成「没有撞上界」——
-- 一个看起来像答案的错答案。
--
-- # 本迁移之前落库的行补不回来
--
-- 那批行的 computed_amount 一律为 NULL，且无法回填。回填 final_amount 是错的：
-- 撞上界时 final_amount **就是** reserved_amount，回填等于把一次撞上界记成
-- 「没撞上界」。而只回填 final_amount < reserved_amount 的那些行（那些确实
-- 没撞上界，可以证明）也不行——NULL 会因此同时表示「没结算」与「结算了但撞
-- 过界」，一个取值两种含义，读的人分不出来。
--
-- 所以这批行是「没有记录」。按 computed_amount 判「有没有撞上界」时，它们
-- 会落到「判不出来」而不是「没撞上界」——这正是本列口径要求的行为，不要为了
-- 让统计好看而把它们并进任何一边。时间点按迁移落地的顺序说（「本迁移之前」），
-- 不写具体日期：每个环境的这一条迁移落地时刻不同。

ALTER TABLE billing_requests ADD COLUMN computed_amount numeric(20, 8);

COMMENT ON COLUMN billing_requests.computed_amount IS
    '这次结算按实际用量算出的金额（未截断）。final_amount = min(computed_amount, reserved_amount)。'
    'computed_amount > reserved_amount 即「这次结算撞上了预留上界」，差额由平台承担。'
    'NULL 表示这一行还没有结算金额（status = ''pending''），或它在本次迁移之前就已结算——'
    '后者补不回来：那时 final_amount 一被截断，未截断的数就没有留下任何痕迹。'
    '所以 NULL 是「没有记录」，判「有没有撞上界」时必须把它算作「判不出来」，不能并进「没撞上界」。'
    '写入点是 internal/ledger/store 的 markSettled，与 final_amount 同一个 UPDATE。';

-- +goose Down
-- 去掉这一列，回到「库里没有未截断结算金额」的状态。列上的注释随列一起消失，
-- 不必单独置 NULL。
--
-- 回滚之后，「这次结算有没有撞上预留上界」重新变成只能在进程内看到（一条
-- CappedSettlements 计数，见 internal/pipeline 与 internal/observe），事后
-- 从库里查不出来。回滚这一列时要想清楚这一点，而不是当它一直没问题。
ALTER TABLE billing_requests DROP COLUMN computed_amount;
