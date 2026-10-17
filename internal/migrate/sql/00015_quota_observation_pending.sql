-- +goose Up
-- 把「这一组还没有写入方」写进 schema：quota_probes 整表，以及
-- channel_quota_state 的两列 last_error_code / last_error_at。
--
-- 这三样属于同一条还没开工的路——**从上游观测额度**，也就是
-- channel_quota_state.probe_kind 里 api_query 与 error_observed 那两个取值。
-- 今天活的只有自记账那一条（internal/channels 的 Recorder），它写回
-- probe_kind='self_accounting'。清单与理由见 docs/channel-health.md 的
-- 「还没接的部分」。
--
-- 为什么不删：它们不是「一版被取代的设计」，而是**读侧已经写好、等写入方**
-- 的形状，删掉会让已经接上的那一半无处可依。
--
--   - quota_probes 是探测结果指定的落点：channel_quota_state 的 probe_kind
--     取值里有两个指向它，表注释写明的用途（探针历史，用于回溯与对账）
--     是接上之后要兑现的承诺。删掉它，那条路落地时要自己再定一张表的形状。
--   - last_error_code 有**活着的读者**：channels 的 candidateQuery 会
--     SELECT 它，route 按 quota.State.Routable 用它区分「从没观测过」与
--     「观测过却没拿到数值」——后者要排除该渠道（见 docs/channel-health.md
--     的「余量未知有两种」一节）。删掉它等于把那一半读侧判据一起删掉。
--   - last_error_at 不一样：它今天**两头都没有**。库里那一列还没有写入方
--     （见下面的 COMMENT），candidateQuery 的 SELECT 列表里也没有它；
--     内存里的 quota.State.LastErrorAt 虽然会被 quota.Merge 在
--     KindErrorObserved 分支设、在 KindAPIQuery 分支清，但没有任何消费方
--     ——Routable 只看 LastProbeAt 与 LastErrorCode。留着它是因为它与
--     last_error_code 是同一次观测的两半，单独删一个会让那条路落地时
--     再补一次 DDL。**别照着「读侧已经写好」去理解它。**
--
-- 也**不接上**，因为接线前有一个必须先回答的问题：一次 429 会让
-- Merge 的 KindErrorObserved 分支设上 LastErrorCode，而它只在 KindAPIQuery
-- 分支被清——于是那个渠道会被 Routable **永久**排除，且不留任何持久痕迹。
-- 这件事要连同冷却期一起设计，见 docs/channel-health.md 同一节。
--
-- 注释让读 DDL 的人先看到事实，而不是自己发现「这一列一直是 NULL」。
COMMENT ON TABLE quota_probes IS
    '额度探测历史，用于回溯与对账。当前**没有写入方**：从上游观测额度（probe_kind 的 api_query / error_observed）尚未实现，今天只有自记账一个写入方，而它不写这张表。见 docs/channel-health.md 的「还没接的部分」。';

COMMENT ON COLUMN channel_quota_state.last_error_code IS
    '上游最近一次给出的错误码，用于区分「从没观测过」与「观测过却没拿到数值」（见 internal/quota 的 State.Routable）。当前**没有写入方**：error_observed 观测尚未实现，所以这一列一直是 NULL。接线前要先解决「一次 429 会让渠道被永久排除」，见 docs/channel-health.md 的「还没接的部分」。';

COMMENT ON COLUMN channel_quota_state.last_error_at IS
    '上面那个错误码的发生时刻。当前**没有写入方**，与 last_error_code 同因：error_observed 观测尚未实现。';

-- +goose Down
-- 注释不是数据，回滚就是把这几行拿掉，恢复成「没有注释」这个原始状态。
COMMENT ON COLUMN channel_quota_state.last_error_at IS NULL;
COMMENT ON COLUMN channel_quota_state.last_error_code IS NULL;
COMMENT ON TABLE quota_probes IS NULL;
