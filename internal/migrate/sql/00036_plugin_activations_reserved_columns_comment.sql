-- +goose Up
-- 给 plugin_activations 补一条表注释与一条列注释：说清按用户、按渠道启用还没接，
-- user_id / channel_id / config / enabled 四列是**预留**，不是废弃。
--
-- 这张表从 00001_init 建出来就没有 COMMENT ON（同一批迁移里 plugin_kv 有，
-- 见 00003）。打开库看见这四列的人，因此无从判断它们是「还没接」还是
-- 「已经废弃」：两者在库里长得一样——都没有人用，但一条要接着做，一条要删掉。
--
-- # 今天写到、读到这张表的地方
--
-- 只有 platform 这一档在跑（都在 internal/plugin/store）：
--
--   读  activeRows（加载器取源）、List 与 Get（管理面）三处查询，都取
--       MAX(a.priority) 并且只数 WHERE a.scope = 'platform' 那一档；
--   写  setPriority 只写 scope = 'platform' 那一行（先 UPDATE 后 INSERT，
--       两句都带这个条件），落的是 priority 一列；
--   删  Delete 按 plugin_key 删，不分作用域。
--
-- 于是 user_id、channel_id、config、enabled 四列零读零写：
-- 它们从没出现在任何一句生产 SQL 里——SELECT 不读，INSERT 与 UPDATE 也不写。
-- 用例里有 scope = 'channel' 的行（internal/plugin/store/store_test.go），
-- 那是核对「取优先级只数 platform 那一档」的夹具，不是这条路上的写入方。
--
-- # 为什么四列里只给 enabled 单独一条列注释
--
-- 「按用户、按渠道这两档还没接，四列是预留」是这张表整体的一件事，表注释里
-- 说一次就够：user_id 与 channel_id 各有外键指着 users 与 channels，config 是
-- 那一档自己的配置，读到表注释那句就分得清。
--
-- enabled 不一样：它不许为空、默认 true，看起来就是「这个插件跑不跑」的开关，
-- 而那个答案只在 plugins.status 上（见 internal/plugin/store 的包注释）。
-- 这一列得单独说一次，读的人才会知道不该拿它去关插件。
--
-- # 为什么不改 00001_init
--
-- 同 00017、00019、00023、00031、00033、00035：goose 不做校验和比对，重写一条
-- 已应用的迁移不会报错——已经跑过的库保持旧注释，新库拿到新注释，两者静默分叉。
-- 这条只追加的规矩见 docs/development.md 的「迁移能不能改」：分界线是有没有
-- 一个不在开发者本人机器上的库，而 staging 已经在了。
--
-- 注释里写「本迁移之后」而不是日期：每个环境这一条落地的时刻不同（同 00017）。

COMMENT ON TABLE plugin_activations IS
    '按作用域的启用记录。今天只有 scope = ''platform'' 这一档在跑：读它的是 '
    'internal/plugin/store 里加载器取源与管理面清单那三处查询（都取 '
    'MAX(priority)），写它的是 setPriority（发布时落 priority）。'
    '本迁移之后，scope 允许的另两档（''user'' 与 ''channel''）没有任何一处代码'
    '读写，user_id、channel_id、config、enabled 四列是预留——四列零读零写，'
    '但不表示它们已经废弃。';

COMMENT ON COLUMN plugin_activations.enabled IS
    '预留：本迁移之后恒为列默认值 true，没有任何一处代码读它或写它。'
    '它**不是**「这个插件跑不跑」的开关——那件事只有一个答案：plugins.status，'
    '两个开关并存会出现「界面上是启用的、实际没跑」，而不一致时没有任何一处会'
    '报错（见 internal/plugin/store 的包注释）。';

-- +goose Down
-- 这条迁移之前，plugin_activations 的表注释与列注释都是空的：00001_init 只建表，
-- 一句 COMMENT ON 也没有，此后没有一条迁移碰过这张表的注释。所以 Down 是把它们
-- 去掉，而不是写回一段别的话。
COMMENT ON COLUMN plugin_activations.enabled IS NULL;
COMMENT ON TABLE plugin_activations IS NULL;
