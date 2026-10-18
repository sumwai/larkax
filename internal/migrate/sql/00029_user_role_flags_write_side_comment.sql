-- +goose Up
-- users.is_merchant / users.is_admin 的注释里，「写侧只有 CreateUser」这句话在
-- `larkaxctl user update` 之后就不成立了，这里把它改准。
--
-- 00023 写的是
--
--   is_merchant：'写侧只有 internal/auth 的 CreateUser（按 --role 落值）；'
--   is_admin   ：'admin 落库时同时打上 is_merchant（见 internal/auth 的 Role.flags）。'
--
-- 变的是这两处：写入侧多了一条路（`user update` 的 --merchant / --no-merchant /
-- --admin / --no-admin 按旗标逐列写这两列，命中哪几列写哪几列），以及那条不变式
-- 所在的方法改名并导出了（Role.flags → Role.Flags：现在有两处调用方，两处各写
-- 一份的话，建出来的 admin 与改出来的 admin 会静默变成两行不同的数据）。
--
-- 读侧一个字没动，原来那句仍然成立：全仓没有任何**鉴权判断**读这两列，读它们
-- 的只有展示与筛选（`user list` 的角色栏与 `--role`）。所以「现在能改」不等于
-- 「有人按它放行」——能改只是把称呼记下来这件事多了一个入口，管理面的鉴权
-- （#158）还没有。
--
-- # 为什么不改 00023
--
-- 同 00023 不改 00001_init 的理由：goose 不做校验和比对，重写一条已应用的迁移
-- 不会报错——已经跑过的库保持旧注释，新库拿到新注释，两者静默分叉。
--
-- 注释里写「本迁移之后」而不是日期：每个环境这一条落地的时刻不同（同 00017）。

COMMENT ON COLUMN users.is_merchant IS
    'merchant 的称呼，不是独立实体。'
    '预留：本迁移之后，全仓没有任何生产代码读这一列——它今天不参与任何判断，'
    '不表示「这个用户被允许挂载上游渠道」。'
    '写侧有两条路，都在 internal/auth 里：CreateUser 按 --role 整体落值，'
    'UpdateUser 供 `user update` 按 --merchant / --no-merchant / --admin 逐列写；'
    '读它来判断权限的入口是管理面的鉴权，那个入口还不存在（#158）。';

COMMENT ON COLUMN users.is_admin IS
    'admin 的称呼，不是独立实体。'
    '预留：本迁移之后，全仓没有任何生产代码读这一列——它今天不参与任何判断，'
    '不表示「这个用户能进管理接口」（管理面还不存在，#158）。'
    'admin 落库时同时打上 is_merchant（见 internal/auth 的 Role.Flags）：建与改都从'
    '那一处取，所以 `user update --admin` 改出来的 admin 与建出来的 admin 是同一种'
    '数据。';

-- +goose Down
-- 写回 00023 的原文，不是置 NULL：那些注释本来就在，Down 要回到这条迁移之前的
-- 状态。写回的是**旧说法**（写侧只有 CreateUser、方法还叫 Role.flags），也就是
-- 这次要消掉的那一份——回滚这一条时要知道，从这一刻起注释又会与代码不符。
COMMENT ON COLUMN users.is_merchant IS
    'merchant 的称呼，不是独立实体。'
    '预留：本迁移之后，全仓没有任何生产代码读这一列——它今天不参与任何判断，'
    '不表示「这个用户被允许挂载上游渠道」。'
    '写侧只有 internal/auth 的 CreateUser（按 --role 落值）；'
    '读它来判断权限的入口是管理面的鉴权，那个入口还不存在（#158）。';

COMMENT ON COLUMN users.is_admin IS
    'admin 的称呼，不是独立实体。'
    '预留：本迁移之后，全仓没有任何生产代码读这一列——它今天不参与任何判断，'
    '不表示「这个用户能进管理接口」（管理面还不存在，#158）。'
    'admin 落库时同时打上 is_merchant（见 internal/auth 的 Role.flags）。';
