-- +goose Up
-- 每个协议可以有自己的上游地址：同一家上游常在不同路径上分别提供 OpenAI 与
-- Anthropic 两种格式，而今天一条渠道只有一条地址，于是只能拆成两条渠道、
-- 填两份凭证——轮换密钥漏一处就是半个站点的 401，QPS 与余额也被拆成两份。
--
-- # NULL 表示沿用渠道的地址，不是「还没填」
--
-- 地址只存一份：没有专属地址的协议读 channels.base_url。回填一份到这里会让
-- 同一个地址存两处，而渠道换地址之后两者就分岔了——改一处，另一处不动，
-- 而屏幕上两行都写着「地址」，谁也看不出哪一行才是真的。
--
-- 所以列上没有默认值，迁移也不回填：老行的这一列全是 NULL，含义是「这个协议
-- 与渠道用同一个地址」，与它迁移之前的行为逐字相同。
ALTER TABLE channel_protocols ADD COLUMN base_url text
    CHECK (base_url IS NULL OR base_url <> '');

COMMENT ON COLUMN channel_protocols.base_url IS
    '这个协议自己的上游地址。NULL 表示沿用 channels.base_url（不是「还没填」）——'
    '空值而不是回填，地址只存一份，渠道换地址时没有专属地址的协议跟着变。见 00069。';

-- +goose Down
-- 列里的值是每条协议各自的地址，删掉之后行为回到「一条渠道一条地址」。
ALTER TABLE channel_protocols DROP COLUMN base_url;
