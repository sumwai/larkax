-- +goose Up
-- 额度探测的写入侧（用到时触发）需要表上还没有的两样东西。
--
-- # 为什么不能复用 last_probe_at
--
-- last_probe_at 只记**成功的实测**，它同时是 Routable 的判据：余量为 NULL 而
-- 这一列有值时，路由把该渠道判成「观测过却没拿到数值」并排除（见 internal/quota
-- 的 State.Routable）。而多数上游根本没有额度查询接口——「查不到」是正常结果，
-- 不能把它们的渠道因此排除掉，也不能拿 last_probe_at 当「多久该再探一次」的
-- 时钟：后者会让这些渠道的每一条请求都去问一次上游。
--
-- 所以另起一列记**尝试**的时刻，成功、查不到、出错都算；last_probe_at 的语义
-- 一个字节都不动。
--
-- # 为什么还要记结论
--
-- 查不到与出错都会更新尝试时刻，但排障时要能分开：前者是这家上游不支持，
-- 后者是这次探测真的失败了（探测失败不覆盖上一次有效值）。NULL 表示还没有
-- 探测过——自记账写出来的行就是这一档。
--
-- 两列都不参与路由判据：读侧今天不看它们，「能不能用」仍由
-- remaining_estimate / last_probe_at / last_error_code 决定。它们服务于触发
-- （TTL）与排障。
ALTER TABLE channel_quota_state
    ADD COLUMN last_probe_attempt_at timestamptz,
    ADD COLUMN last_probe_status text
        CHECK (last_probe_status IN ('ok', 'unsupported', 'error'));

COMMENT ON COLUMN channel_quota_state.last_probe_attempt_at IS
    '最近一次尝试探测额度的时刻，成功、查不到（上游没有查询接口）与出错都算。触发侧按它与 settings 的 quota.probe_interval_seconds 判断要不要再探。last_probe_at 只记成功的实测，不能当这个时钟。';

COMMENT ON COLUMN channel_quota_state.last_probe_status IS
    '最近一次探测的结论：ok（拿到实测值）/ unsupported（这家上游没有额度查询接口，是正常结果）/ error（探测本身失败，此时不覆盖上一次有效值）。NULL 表示还没有探测过。';

-- +goose Down
ALTER TABLE channel_quota_state DROP COLUMN last_probe_status;
ALTER TABLE channel_quota_state DROP COLUMN last_probe_attempt_at;
