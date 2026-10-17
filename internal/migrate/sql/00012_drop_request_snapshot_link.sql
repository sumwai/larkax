-- +goose Up
-- 删掉 billing_requests.request_snapshot_id。
--
-- 它的用途是「请求与快照的关联」，而**这层关联已经由两边共有的 request_id
-- 承载**：request_snapshots 自己就有 request_id 列，且索引是
-- request_snapshots_request_idx (request_id, created_at)。用那个 id 查，
-- 比用这一列查更差：request_snapshots 按 created_at 分区，单凭一个 id
-- 定位不到分区，查询要扫遍所有分区。
--
-- 另一半是它接不上：request_snapshots 的 INSERT 没有 RETURNING id，落库是
-- 独立 goroutine 里异步做的、返回值当场丢弃（见 internal/gateway 的
-- snapshotAsync），而结算发生在它之前——结算时那个 id 不一定已经拿到。
-- 要接上得改三层接口，换来的却是一个「看运气」的关联值。
--
-- 列自己的注释也承认它会永久悬空（分区 DROP 之后读不到）。用 request_id
-- 做关联时这件事自然成立：快照过期之后 join 不到，不需要读的人先判空。
--
-- 这是 00010 那类处置——**已经有一版更省事的做法取代它**。
ALTER TABLE billing_requests DROP COLUMN request_snapshot_id;

-- +goose Down
-- 按 00001 的原样加回来：可空、无默认值，不影响任何现有行。
--
-- 00001 里那三行说明（「关联的结构快照……读取方需自行判空并接受查不到」）
-- 随列一起回到 DDL 里，见 git 历史。
--
-- 回滚之后这一列仍然是 NULL——它本来就没有写入方。要真的接上，先解决
-- 「异步落库拿不回 id」这件事，见上面 Up 的第二段。
ALTER TABLE billing_requests ADD COLUMN request_snapshot_id bigint;
