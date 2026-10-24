-- +goose Up
-- 一条渠道供哪些模型记在 channels.models（text[]）里，判断「这条渠道供不供
-- 某个模型」用的是 `models @> ARRAY[名]`（见 channels.suppliesModel）。没有
-- 索引时这条判据要把整张渠道表扫一遍，于是单模型页的供给查询随渠道总数增长。
--
-- # 为什么不用 CREATE INDEX CONCURRENTLY
--
-- CONCURRENTLY 不阻塞写入，但它不能跑在事务里；而迁移器（goose）默认把每条
-- 迁移包在事务里，绕开这一点要写它的 NO TRANSACTION 注解，那是本仓库从未
-- 用过的迁移写法。渠道表在十万行量级上构建这把索引约 1 秒、索引约 9.6 MB
-- （#1113 的实测），这 1 秒的写入锁对一张由后台操作写入的表可以接受，不值得
-- 为它引入一种新的迁移形态。普通写法跟着既有的事务包装走，up → down → up 的
-- 往返也天然被 internal/migrate 的集成用例覆盖。
--
-- # fastupdate 保持默认
--
-- 不写 `WITH (fastupdate = off)`：关掉它写入代价大约翻倍（#1113 实测插入
-- +120%、改 models +112%），而默认值下的代价已经够低（+5.4% / +11.5%）。
CREATE INDEX channels_models_gin_idx ON channels USING gin (models);

-- +goose Down
DROP INDEX channels_models_gin_idx;
