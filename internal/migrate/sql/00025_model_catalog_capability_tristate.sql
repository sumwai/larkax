-- +goose Up
-- model_catalog 的六个能力列加出「未知」这一态：改成可空。
--
-- # 三种状态
--
--   NULL   未知：这一项从没配过，**不参与**任何判据
--   false  明确声明不支持：请求用到这一项时这条候选出局
--   true   支持
--
-- # 为什么要有这一态
--
-- 这六列原本是 NOT NULL DEFAULT false，于是「从没配过」与「配成不支持」在
-- 库里是同一个值。选路（#366）按这些列筛候选，口径是「有记录就按记录判」，
-- 于是一条只填了名字的记录在库里等于「六项都不支持」——带工具的请求从此
-- 选不到它，哪怕那个上游其实支持工具。同一张表的 context_limit / output_limit
-- 是可空的，读出来的「未知」不参与判据；能力这一侧当时没有这一态。
--
-- # 存量一行都不动
--
-- 这条迁移只有 DDL：六个 DROP NOT NULL 加六个 DROP DEFAULT，一条 UPDATE 都没有。
-- 既有行的 false 保持 false——**不对存量做猜测**。把存量的 false 改写成 NULL
-- 会让所有已经声明过「不支持」的模型重新参与判据，而那是一次静默的放行，
-- 方向正是「把请求发给一个声明做不了这件事的模型」。#485 定的是：存量仍旧是
-- 「不支持」，要改哪一条由运维显式地改。
--
-- # 默认值也一起去掉
--
-- 留着 DEFAULT false 等于把这条三态废掉一半：写入侧漏一列就又落回 false，
-- 而「不知道」与「不支持」的区别正是这条迁移要的东西。写入侧
-- （internal/catalog 的 Create）本来就显式写这六列。
--
-- # 顺序：先 DROP DEFAULT 还是先 DROP NOT NULL 都行
--
-- 两者互不依赖，写成一组只是少跑几遍 ALTER TABLE。
ALTER TABLE model_catalog
    ALTER COLUMN supports_reasoning DROP NOT NULL,
    ALTER COLUMN supports_reasoning DROP DEFAULT,
    ALTER COLUMN supports_tools     DROP NOT NULL,
    ALTER COLUMN supports_tools     DROP DEFAULT,
    ALTER COLUMN supports_vision    DROP NOT NULL,
    ALTER COLUMN supports_vision    DROP DEFAULT,
    ALTER COLUMN supports_audio     DROP NOT NULL,
    ALTER COLUMN supports_audio     DROP DEFAULT,
    ALTER COLUMN supports_cache     DROP NOT NULL,
    ALTER COLUMN supports_cache     DROP DEFAULT,
    ALTER COLUMN supports_batch     DROP NOT NULL,
    ALTER COLUMN supports_batch     DROP DEFAULT;

-- 六列一个意思，注释也写同一句：在 psql 里看这一行的人要能立刻分清
-- NULL 与 false，而不是去翻代码。
COMMENT ON COLUMN model_catalog.supports_reasoning IS
    'NULL = 未知（没配过，不参与选路判据）；false = 明确不支持；true = 支持。见 00025。';
COMMENT ON COLUMN model_catalog.supports_tools IS
    'NULL = 未知（没配过，不参与选路判据）；false = 明确不支持；true = 支持。见 00025。';
COMMENT ON COLUMN model_catalog.supports_vision IS
    'NULL = 未知（没配过，不参与选路判据）；false = 明确不支持；true = 支持。见 00025。';
COMMENT ON COLUMN model_catalog.supports_audio IS
    'NULL = 未知（没配过，不参与选路判据）；false = 明确不支持；true = 支持。见 00025。';
COMMENT ON COLUMN model_catalog.supports_cache IS
    'NULL = 未知（没配过，不参与选路判据）；false = 明确不支持；true = 支持。见 00025。';
COMMENT ON COLUMN model_catalog.supports_batch IS
    'NULL = 未知（没配过，不参与选路判据）；false = 明确不支持；true = 支持。见 00025。';

-- +goose Down
-- 回滚要把「未知」折成 false：旧 schema 里没有别的地方放这一态。
--
-- 折成 false 是**保守**的那一侧——回滚之后一条没配过能力的记录不再放行，
-- 用到那几项能力的请求被判成「这个模型做不了」，而不是发给一个可能真做不了
-- 的上游。信息是有损的（哪些行本来是「未知」，回滚后看不出来），但那是旧
-- schema 的容量决定的，不是这里能选出来的；不折的话 SET NOT NULL 直接失败，
-- 于是这条迁移在任何一个用过三态的库上都回滚不了。
--
-- 先 UPDATE 再 SET NOT NULL：反过来会在遇到第一行 NULL 时就报错。
UPDATE model_catalog SET supports_reasoning = false WHERE supports_reasoning IS NULL;
UPDATE model_catalog SET supports_tools     = false WHERE supports_tools     IS NULL;
UPDATE model_catalog SET supports_vision    = false WHERE supports_vision    IS NULL;
UPDATE model_catalog SET supports_audio     = false WHERE supports_audio     IS NULL;
UPDATE model_catalog SET supports_cache     = false WHERE supports_cache     IS NULL;
UPDATE model_catalog SET supports_batch     = false WHERE supports_batch     IS NULL;

ALTER TABLE model_catalog
    ALTER COLUMN supports_reasoning SET DEFAULT false,
    ALTER COLUMN supports_reasoning SET NOT NULL,
    ALTER COLUMN supports_tools     SET DEFAULT false,
    ALTER COLUMN supports_tools     SET NOT NULL,
    ALTER COLUMN supports_vision    SET DEFAULT false,
    ALTER COLUMN supports_vision    SET NOT NULL,
    ALTER COLUMN supports_audio     SET DEFAULT false,
    ALTER COLUMN supports_audio     SET NOT NULL,
    ALTER COLUMN supports_cache     SET DEFAULT false,
    ALTER COLUMN supports_cache     SET NOT NULL,
    ALTER COLUMN supports_batch     SET DEFAULT false,
    ALTER COLUMN supports_batch     SET NOT NULL;

-- 回到 00001_init 的状态：那六列本来没有注释。
COMMENT ON COLUMN model_catalog.supports_reasoning IS NULL;
COMMENT ON COLUMN model_catalog.supports_tools     IS NULL;
COMMENT ON COLUMN model_catalog.supports_vision    IS NULL;
COMMENT ON COLUMN model_catalog.supports_audio     IS NULL;
COMMENT ON COLUMN model_catalog.supports_cache     IS NULL;
COMMENT ON COLUMN model_catalog.supports_batch     IS NULL;
