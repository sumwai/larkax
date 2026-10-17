-- +goose Up
-- 把 billing_requests.client_cancelled_at 改名成 client_canceled_at。
--
-- 只改拼法，不改语义：同一列、同一类型、同样的可空性，值一个不动。
--
-- 为什么改：`.golangci.yml` 里 misspell 显式设了 locale: US，而 Go 侧全是美式
-- 拼法——字段叫 ClientCanceled、标准库叫 context.Canceled。这一列是唯一的不一致，
-- 于是它一旦出现在 Go 的注释或 SQL 字符串里（00015 之前的这一轮把它接通了），
-- misspell 就报 10 处。把列名对齐成美式，Go 侧与 schema 就同一种拼法。
--
-- 为什么不直接改 00001_init.sql：
--
-- **已应用的迁移文件不重写。** goose 只记「这个版本跑过了」，不做校验和比对，
-- 所以改了 00001 不会有任何报错：跑过 00001 的库留着旧列名，从零建的库拿到新列名，
-- 两者静默分叉，直到某次查询报「column does not exist」才暴露——而那时离原因
-- 已经很远了。一条 RENAME 迁移把两边的终态收敛到同一个名字，这是唯一能保证
-- 「从零建」与「逐个迁移升上来」结果一致的做法。
--
-- 00001 里另外两处英式拼法（payment_orders.status 与另一张表 CHECK 约束里的
-- 取值 'cancelled'）**这次不动**：那两张表属于尚未开建的功能，它们现在只出现在
-- SQL 里、不触发 lint；等那两个功能真做起来时，按同一条办法各加一条迁移。
--
-- 顺带一提，Go 里还有 identifier 形态的英式拼法（internal/plugin/sandbox 的
-- ErrClientCancelled）。misspell 按词切分，camelCase 的标识符是一个整词，
-- 不在它的词典里，所以从来不报——那是命名一致性的事，不是这一条迁移的事。
ALTER TABLE billing_requests RENAME COLUMN client_cancelled_at TO client_canceled_at;

-- +goose Down
-- 反向改名，回到 00001 里的那个拼法。
ALTER TABLE billing_requests RENAME COLUMN client_canceled_at TO client_cancelled_at;
