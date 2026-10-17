-- +goose Up
-- channels 加一列 deleted_at：渠道改成**软删**。
--
-- # 为什么是软删而不是 DELETE
--
-- 指向 channels 的八条外键里，七条是 ON DELETE CASCADE（额度状态、探针历史、
-- 健康汇总、别名、计价规则、上游成本、插件激活），真删会把它们一起带走；
-- 唯一没有 ON DELETE 的 billing_requests.channel_id 会让真删直接撞外键。
-- 软删保住「历史行认得这条渠道」，而那正是对账与快照可追溯性的前提。
--
-- # 代价：每个读 channels 的地方都要带上 deleted_at IS NULL
--
-- 判据只有一份字面量（internal/channels 的 channelAlive），读侧都拼它。
-- 漏掉一处不会报错，只会让一条已经删除的渠道继续参与选路或出现在清单上——
-- 所以那道片段是这条迁移的配套，不是可选的整洁。
--
-- # 删除时连 status 一起置成 disabled
--
-- 这一列不是第二份判据，是给**读侧漏网**留的一道兜底：选路要求
-- status = 'enabled'，所以即便某条读路径忘了新的条件，被删的渠道也选不中。
-- 它同时让已有的 channels_status_idx（部分索引，只收 enabled 的行）自动
-- 不再为已删的行留位置。
--
-- # 部分索引
--
-- 默认清单（不带 --all）只列没删的行并按 id 排序，这条索引服务的就是它：
-- 按 id 顺序走索引、跳过已删的行，不必先取回再过滤。选路那条查询用不上它
-- （它按 status 与 models 过滤，走 channels_status_idx），列在这里只是说明
-- 为什么是 (id) 而不是别的列。
ALTER TABLE channels ADD COLUMN deleted_at timestamptz;
CREATE INDEX channels_live_idx ON channels (id) WHERE deleted_at IS NULL;

COMMENT ON COLUMN channels.deleted_at IS
    '软删时刻。非 NULL 表示这条渠道已删除：所有读路径都过滤它，管理面 --all 才看得见。'
    '删除不清凭证密文，也不删从属的行——billing_requests / usage_events 仍按 channel_id 引用它。';

-- +goose Down
-- DROP COLUMN 会连同依赖这一列的部分索引一起删掉（索引的谓词引用了它），
-- 这里的 DROP INDEX 是显式写出来给人看的：回滚之后 channels 回到
-- 「没有任何删除状态」的样子，已删除的行也一并变回普通行。
DROP INDEX channels_live_idx;
ALTER TABLE channels DROP COLUMN deleted_at;
