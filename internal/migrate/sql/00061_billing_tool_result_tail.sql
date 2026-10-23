-- +goose Up
-- 请求日志要能分开「用户在对话」与「agent 在跑工具循环」：同一行两种读法，
-- 而区分的判据就是这次提交的最后一句是不是工具结果的回填。
--
-- 判定在入口做（relay.ParseRequest 已把三条 chat 入口的工具结果归一化成
-- tool_result 块，见 channel.Request.EndsWithToolResult），这里只存结论。
-- systemone 的决策表面没有消息序列，恒为 false。
ALTER TABLE billing_requests
    ADD COLUMN tool_result_tail boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE billing_requests
    DROP COLUMN tool_result_tail;
