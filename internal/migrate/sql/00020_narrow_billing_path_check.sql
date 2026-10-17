-- +goose Up
-- 把 usage_events.billing_path 的 CHECK 从三档收窄成两档，去掉 'fallback'。
--
-- 这一档**没有任何产出点**。写入侧只有 internal/ledger/store 的 billingPathOf，
-- 它把 tokens.Source 翻成 'measured' 或 'estimated' 之一（SourceMeasured →
-- measured；SourceTokenized 与 SourceEstimated → estimated）。tokens.Source 一共
-- 三个取值，没有第三个映射到 'fallback'，全仓也没有第二处写这一列。
--
-- 于是它是一条**永远不会出现在数据里的取值**，却写在 CHECK 里：读的人会以为
-- 「上游没报用量、估算也没成」那类情形落成的是它，而那种情形今天根本不产生
-- 这一行（非流式那条路连「上游压根没报」与「上游报了 0」都还分不开，见
-- internal/ledger/store 里 ErrUnknownUsageSource 的注释）。
--
-- 处置方式是删掉这一档而不是接一个产出点，因为它**没有对应的语义**：现有三个
-- Source 已经把「这个数能不能拿去跟上游对账」分完了（能 → measured，不能 →
-- estimated），再来一档说不出它比 estimated 差在哪。真要区分「上游压根没报」，
-- 那是新增一个来源的事，取值该按那时的口径重新定，不该由这一档先占着位置。
--
-- # 分区表上的行为（PostgreSQL 16 上实测确认）
--
-- usage_events 按 created_at 分区，每个分区上都有一份继承来的同名约束
-- （pg_constraint 里 coninhcount = 1、conislocal = false）。实测：
--
--   * 在父表上 DROP CONSTRAINT 会**递归到每个分区**，包括 usage_events_default
--     兜底分区——执行完父表与各分区上都不再有这条约束；
--   * 在父表上 ADD CONSTRAINT 同样递归，并且**在每个分区上校验存量行**（真有
--     'fallback' 的行时，报出来的错误指名那个分区）。
--
-- 所以这一条迁移同时就是「存量数据里没有 'fallback'」的证明：真有那样一行，
-- ADD CONSTRAINT 会直接失败并指名分区，而不是把两档 CHECK 静默装上去。这里
-- 不再另写一段 SELECT 去数——那种检查与约束校验扫的是同一批行，而它报出来的
-- 是「有几行」，还得再查一次才知道是哪些行。
--
-- 另一件靠这条递归保证的事：**后来新建的分区自动继承父表的两档 CHECK**，
-- 所以不必给以后按月建出来的分区单独补约束。
--
-- # 为什么不改 00001_init.sql
--
-- 已应用的迁移文件不重写。goose 只记「这个版本跑过了」，不做校验和比对：改了
-- 00001 不会有任何报错，跑过 00001 的库留着三档 CHECK，从零建的库拿到两档，
-- 两者静默分叉。这一列的分叉方式还格外难发现——它是「一个库拒绝某种写入、
-- 另一个库接受」，不是查询报错。
ALTER TABLE usage_events DROP CONSTRAINT usage_events_billing_path_check;
ALTER TABLE usage_events ADD CONSTRAINT usage_events_billing_path_check
    CHECK (billing_path IN ('measured', 'estimated'));

-- +goose Down
-- 加回 'fallback'，回到 00001 里的三档。
--
-- 回滚后的状态是「CHECK 收三档、而仍然没有任何路径产出 'fallback'」——正是这
-- 一条迁移要消除的那件事。回滚之后要重新决定怎么处置这一档，而不是当它一直
-- 没问题。
ALTER TABLE usage_events DROP CONSTRAINT usage_events_billing_path_check;
ALTER TABLE usage_events ADD CONSTRAINT usage_events_billing_path_check
    CHECK (billing_path IN ('measured', 'estimated', 'fallback'));
