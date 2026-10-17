-- +goose Up
-- 给 usage_events 补一条表注释，指向分区维护的文档。
--
-- 表本身在 00001_init 里就建好了，这条只加一句话。需要它的原因是：分区不会
-- 自己长出来，而「谁来建」这件事在 schema 里看不见——`\d+ usage_events` 只会
-- 列出当下存在的分区，看不出有没有人在铺后面的月份。落进兜底分区的那条路
-- 是静默的（不报错、查询照常正确），所以线索要留在看得见的地方。
--
-- 00001_init 里那句「分区由 worker 定时预建」不能改（goose 不做校验和比对，
-- 重写已应用的迁移会让老库与新库静默分叉），所以把「那个 worker 是什么」写在
-- 这里：它就是 larkaxctl partition ensure，由外部调度器调用。

COMMENT ON TABLE usage_events IS
    '用量事件（计量层）。只追加，按月分区。'
    '分区由 larkaxctl partition ensure 预建（外部调度器调用），写入侧在结算前保证当月分区存在。'
    '没有预建时行会落进 usage_events_default——不报错，但兜底分区非空是告警：'
    '那些行没有到期日，而且会挡着那个月的分区建不出来。'
    '见 docs/partitions.md。';

-- +goose Down
-- 回到「没有注释」的状态。原状态就是没有注释，所以置 NULL 而不是写回别的文本。
COMMENT ON TABLE usage_events IS NULL;
