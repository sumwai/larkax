-- +goose Up
-- 删掉 channels.protocol。
--
-- 它是一条独立落库、独立校验、却没有消费者的第二真相源：上游协议族由
-- channels.type 对应的适配器决定（Adapter.Protocol()），这一列的值对每条
-- 已注册的 type 都是可推导的。于是它只会填错而无人发现——「type 是 openai
-- 适配器、protocol 写 anthropic」照样通过校验，而执行器分派只看 type。
--
-- 「大体像 OpenAI 但有几处不一样」这类差别不进这一列：它属于该渠道的私有
-- 配置（channels.setting），那里是真的会被读的地方。
ALTER TABLE channels DROP COLUMN protocol;

-- +goose Down
-- 把列加回来，但**恢复不了原值**：列一删，值就没了，任何重建都只能填一个
-- 凭空写进去的默认值。
--
-- 所以这里填 'openai'，并且**只用来填充已有行**：填完立刻摘掉默认值，
-- 让新插入的行必须自己声明协议——与这一列原本的定义一致。回滚迁移的人
-- 必须知道：执行这一步之后，**所有**渠道的 protocol 都是 'openai'，
-- 而不是它们各自原本的值。
ALTER TABLE channels ADD COLUMN protocol text NOT NULL DEFAULT 'openai'
    CHECK (protocol IN ('openai', 'anthropic', 'custom'));
ALTER TABLE channels ALTER COLUMN protocol DROP DEFAULT;
