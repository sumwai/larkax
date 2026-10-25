-- +goose Up
-- 账号粒度的运行状态：一份凭证被上游拒绝时，该摘掉的是这个账号，不是整条渠道。
--
-- # 为什么单独一张表
--
-- accounts 是**配置**（这份凭证是什么），这张表是**运行状态**（它此刻怎么样）。
-- 与 channels / channel_quota_state 的分工同一个理由：配置改了与状态变了两件事
-- 混在一行里，读的人和写的人都会在对方改过之后分不清自己读到的算什么。
--
-- 状态挂在账号上而不是 channel_accounts 关联上：一份凭证被上游拒绝，坏的
-- 是这份凭证本身，而它与渠道的关系（挂在几条渠道上、各条上权重多少）不受影响。
-- 权重在关联上，健康在账号上，各归各的。
--
-- # 一行都没有 = 还没被用过
--
-- 新账号没有这一行。读侧把「没有行」与「行上写着 unknown」当同一件事：两者都
-- 说不出这个账号好不好，选路照常让它参选。所以写入方不需要在建账号时预插一行。
--
-- # 取值与状态机
--
-- health 复用渠道侧那套词：
--
--   unknown   还没被调用过（没有行时也读作它）
--   healthy   最近一次调用成功
--   degraded  最近连着失败过，但还没到摘除的阈值
--   drained   不参与选路（凭证被上游拒绝，或连续失败到了阈值）
--
-- drained 带一个冷却期：距 last_failure_at 超过冷却时长之后它重新参选一次
-- （见 internal/channels 的账号选路）。没有这条出口的话，被摘掉的账号没有流量，
-- 状态也就永远不会自己变回去。
CREATE TABLE account_state (
    account_id           bigint PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    health               text NOT NULL DEFAULT 'unknown'
        CHECK (health IN ('healthy', 'degraded', 'drained', 'unknown')),
    consecutive_failures integer NOT NULL DEFAULT 0 CHECK (consecutive_failures >= 0),
    -- 最近一次失败的时刻。健康结论与连续失败计数都以它为冷却期的时钟；
    -- 成功时清回 NULL。
    last_failure_at      timestamptz,
    -- 最近一次结局的一句话，给人看。写入方按自己的词表拼，不照抄上游响应体。
    last_conclusion      text NOT NULL DEFAULT '',
    updated_at           timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE account_state IS
    '账号粒度的运行状态（健康结论、连续失败、最近结论）。与配置表 accounts 分开放：'
    '一份凭证被上游拒绝时摘的是这个账号，不是整条渠道。见 00068。';
COMMENT ON COLUMN account_state.last_failure_at IS
    '最近一次失败的时刻；drained 的冷却期内不参与选路，超过冷却期重新参选一次。'
    '成功时清回 NULL。';

-- +goose Down
-- 纯新增：这张表里的每一列都是本迁移之后才有的运行状态，回滚直接丢掉。
DROP TABLE account_state;
