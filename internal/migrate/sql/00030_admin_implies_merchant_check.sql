-- +goose Up
-- 把「is_admin 为真时 is_merchant 也应为真」这条不变式写进 schema。
--
-- 它今天只靠两处维持：internal/auth 的 Role.Flags 在建用户时按称呼写两列，
-- cmd/larkaxctl 的 user update 挡住会把这一行写成反例的输入（--admin 与
-- --no-merchant 一起给、--no-merchant 打在读出来是 admin 的那一行上）。库里
-- 没有任何约束，于是绕过命令行的调用方直接调 auth.UpdateUser 就能写出一行
-- (is_merchant=false, is_admin=true)——它按列写，不替调用方补这条不变式。
--
-- 这样一行写进去之后**没有任何一处会报错**：roleOf 先看 is_admin，读出来仍是
-- admin，于是它在展示、筛选、折称呼上与正常写出来的 admin 没有分别，只在直接
-- 按列读时才看得出（手工 SQL、以及将来可能有的按列授权判断）。没有任何命令会
-- 因为它失败，也没有任何输出会提示它存在——落库就沉底了。
--
-- # 存量行先对齐
--
-- 反例行今天已经被 roleOf 当成 admin 读，把 is_merchant 补成真不改变任何既有
-- 语义，只是把那一列补成 roleOf 一直以为的样子。对齐要放在加约束之前：不先
-- 对齐，ADD CONSTRAINT 会在校验存量行时直接失败。
--
-- # 不加 NOT VALID
--
-- users 的行数小，加约束时顺带校验一遍存量行，比留一条没验过的约束省事：
-- 后者会让这条不变式的效力取决于「这一条迁移之前库里有没有反例行」，而那是
-- 每个环境各不相同的事实。
--
-- # 命令行的校验一条都不删
--
-- 违反约束时调用方拿到的是一句驱动原文（check constraint ... of relation
-- "users" is violated），而「先 --no-admin、或 --no-admin --no-merchant 一起
-- 给」这句出路只有命令层说得出来。两层各管一件事：库里那条保证反例行写不进
-- 去，命令那条告诉人怎么改。

UPDATE users SET is_merchant = true WHERE is_admin AND NOT is_merchant;

ALTER TABLE users ADD CONSTRAINT users_admin_implies_merchant
    CHECK (NOT is_admin OR is_merchant);

-- +goose Down
-- 只去掉约束。上面那条对齐**不回滚**：补过的行本来就是合法的 admin 行（roleOf
-- 按 is_admin 读，is_merchant=真 与 admin 这个称呼一致），把那一列改回假是亲手
-- 造出这条迁移要消除的反例行。
--
-- 回滚之后的语义是「schema 不再保证这条不变式」，那道保证又只剩 Go 侧的两处，
-- 也就是这条迁移之前的状态。
ALTER TABLE users DROP CONSTRAINT users_admin_implies_merchant;
