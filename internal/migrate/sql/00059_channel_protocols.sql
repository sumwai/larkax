-- +goose Up
-- 一条渠道可以声明多套协议：同一家供应商常常在同一个 base URL 上用同一套
-- 凭证同时提供 OpenAI 与 Anthropic 两种端点。今天只能建两条渠道、填两份
-- 凭证，于是轮换密钥漏掉一处就是半个站点的 401，而这个供应商的 QPS、余额
-- 与健康度也被拆成两份。
--
-- # 为什么是新表，不是把 channels.type 改成逗号串
--
-- 逗号串加不了外键、也没法按协议建索引：外键让删渠道时协议行跟着走，
-- 索引让「哪些渠道说 anthropic」能走一次查找。
--
-- # channels.type 仍然保留
--
-- 它继续是这条渠道的**主协议**（写入时取协议集合里的第一个，或者调用方
-- 显式给的那一个）。读取侧今天按 type 算端点、`channel get` 按它打「类型」
-- 那一栏，这些行为一个字都不变——多协议是**附加**的一份声明，不是把单值
-- 改成多值。
--
-- # 老数据
--
-- 每条现有渠道按它的 type 补一行：迁移之前「一条渠道 = 一种协议」，那一行
-- 就是它的准确含义。
CREATE TABLE channel_protocols (
    channel_id  bigint NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    -- 取值是 channel.Protocol 的那几个（与适配器声明的 Type() 同名）。
    -- 表上不写 CHECK：协议清单的唯一来源是适配器注册表，写死一份枚举会在
    -- 加协议时变成第二个真相源，而它只会拦下新协议、不会报出自己过时了。
    protocol    text NOT NULL CHECK (protocol <> ''),
    -- 同一渠道下同一套协议只有一行：重复声明是无意义的，主键顺带挡住它。
    PRIMARY KEY (channel_id, protocol)
);

-- 主键是 (channel_id, protocol)：按协议反查（「哪些渠道声明了 anthropic」）
-- 走不了它，所以为那一列单独建一条索引。
CREATE INDEX channel_protocols_protocol_idx ON channel_protocols (protocol);

COMMENT ON TABLE channel_protocols IS
    '一条渠道声明支持的协议集合。channels.type 仍是主协议，单协议的渠道在这里只有一行且与 type 相同。见 00059 与 issue #1042。';

INSERT INTO channel_protocols (channel_id, protocol)
SELECT id, type FROM channels;

-- +goose Down
-- 表只存一份声明，删掉之后行为回到「一条渠道一种协议」（由 type 决定）。
DROP TABLE channel_protocols;
