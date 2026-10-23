-- +goose Up
-- 请求日志要回答两个新问题：「上游多久吐出第一个 token」与「调用方是谁」。
--
-- # first_token_ms 为什么在 billing_requests，而不是 usage_events
--
-- 它是**一次请求**的形状，不是一次用量：usage_events 按分区分批清理，
-- 而 billing_requests 的行就是控制台请求日志读的那一行——读侧不必再
-- LATERAL 一次。非流式请求整个响应一次到达，TTFT 没有意义，落 NULL。
--
-- 口径：从受理（预留落库）到**第一个内容块**（正文 / 工具调用 / 标注）
-- 到达网关的毫秒数。由网关在流式循环里量，结算时随 Outcome 带下来。
--
-- # user_agent 为什么整串存
--
-- 截断会让「同一个客户端」对不上号：客户端库里同一家的标识符五花八门，
-- 截到第一个斜杠反而丢了版本号，而版本正是排查兼容性问题时要看的。
-- 这一列只进请求日志一行，体量不是问题。
ALTER TABLE billing_requests
    ADD COLUMN first_token_ms integer,
    ADD COLUMN user_agent text;

-- +goose Down
ALTER TABLE billing_requests
    DROP COLUMN first_token_ms,
    DROP COLUMN user_agent;
