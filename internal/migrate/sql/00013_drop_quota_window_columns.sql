-- +goose Up
-- 删掉 channel_quota_state 的四列滚动窗口：window_kind / window_started_at /
-- window_budget / window_used。
--
-- 这四列是「按周期给量的上游」（「每天 1000 次」那类）的口径落点，而那条
-- 路没有开工，也没有排期：**读侧从来没有要过它们**（internal/route 的注释
-- 明写「窗口那几列不填」，候选上的 quota.State 也不填），写入侧
-- （internal/channels 的 Recorder）的 UPSERT 列清单里也没有它们。
--
-- 真正被需要的那一件事——「这个额度什么时候重置」——已经由同一张表的
-- resets_at 承担，而它是活的：candidateQuery 会读它，route 的 scoreQuota
-- 按它给「越接近到期越该先用掉」的分。
--
-- 与之配套的还有 internal/quota 里三个死字段（State.WindowKind /
-- WindowBudget / WindowUsed）：全仓没有一处给它们赋值，也没有一处读它们。
-- 那一版一并删掉了，否则会留下「内存模型有窗口、表里没有」的错位。
--
-- 这不是「将来要用所以先摆着」：真做按周期给量时，那几列该按当时的口径
-- 重新定（周期是自然月还是滚动窗口、预算与已用是数值还是可空的观测），
-- 照现在这四列填反而会被一个没验证过的形状绑住。原 DDL 留在 git 历史里的
-- 00001_init.sql，需要时可以照着取。
ALTER TABLE channel_quota_state DROP COLUMN window_kind;
ALTER TABLE channel_quota_state DROP COLUMN window_started_at;
ALTER TABLE channel_quota_state DROP COLUMN window_budget;
ALTER TABLE channel_quota_state DROP COLUMN window_used;

-- +goose Down
-- 按 00001 的原样加回来。**原有的值恢复不了**（本来也没有写入方，
-- 四列全是 NULL 或默认值，所以这里没有实际的数据损失）。
--
-- 注意回滚后的状态是「四列在表里、而仍然零读零写」——正是这一条迁移要
-- 消除的那件事。回滚之后要重新决定怎么处置它们，而不是当它一直没问题。
ALTER TABLE channel_quota_state ADD COLUMN window_kind text
    CHECK (window_kind IN ('fixed_clock', 'from_first_use'));
ALTER TABLE channel_quota_state ADD COLUMN window_started_at timestamptz;
ALTER TABLE channel_quota_state ADD COLUMN window_budget numeric(20, 8);
ALTER TABLE channel_quota_state ADD COLUMN window_used numeric(20, 8) NOT NULL DEFAULT 0;
