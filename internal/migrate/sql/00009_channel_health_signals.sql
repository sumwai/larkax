-- +goose Up
-- 两个缺列：渠道的「连续失败」计数，与额度余量的单位。
--
-- 这两列都补在**写入侧**：在此之前，channel_health_rollup 与
-- channel_quota_state 没有任何生产写入方，读侧取到的永远是「没有行」，
-- 于是连续失败恒为 0（摘除渠道的判据从不触发，一个正在连续失败的渠道
-- 还拿满分健康分），额度恒为未知。写入方见 internal/channels 的 Recorder。

-- ── 一、连续失败 ────────────────────────────────────────────────────────
--
-- 小时桶里的 requests/successes/failures 重建不出「连续失败了 N 次」：
-- 桶内有过成功时，那段连续长度落在 0 与 failures 之间，无从判断。
-- 所以它是一个**逐次维护的计数**，不是从桶里算出来的统计量：
-- 调用成功清零、失败加一（见 internal/channels 的 RecordCall）。
--
-- 为什么不另建一张「最近 N 次结果」的表：这个信号的消费者只有一个
-- （route 的 DrainReason 与 scoreHealth），它要的是一个整数，
-- 而逐次明细没有别的用途。
ALTER TABLE channels ADD COLUMN consecutive_failures integer NOT NULL DEFAULT 0
    CHECK (consecutive_failures >= 0);

-- last_failure_at 与 consecutive_failures 成对维护：前者是最后一次**把计数
-- 加一**的时刻，计数为零时它必须是 NULL。
--
-- 它存在的理由是一个死循环：计数达到阈值 → 渠道被摘 → 没有流量 →
-- 计数不动 → 永远摘不掉。读取侧用这个时刻开一个冷却期（见
-- internal/channels 的 streakCooldown）：冷却期一过，渠道重新进候选集试一次，
-- 失败就再摘一轮，成功就清零。
--
-- 用时间戳而不是「一个定期清零计数的任务」：后者要额外有一个一直在跑的
-- 东西，而它停掉时的表现是「渠道再也回不来」。
ALTER TABLE channels ADD COLUMN last_failure_at timestamptz;

COMMENT ON COLUMN channels.consecutive_failures IS
    '连续失败次数：调用成功清零、失败加一，由调用收尾处维护。为 0 表示没有未结算的失败。';
COMMENT ON COLUMN channels.last_failure_at IS
    '最后一次失败（把 consecutive_failures 加一）的时刻，计数为 0 时为 NULL。读取侧据此判断冷却期是否已过。';

-- ── 二、额度单位 ────────────────────────────────────────────────────────
--
-- remaining_estimate 是个裸的 numeric，而候选渠道来自不同上游，各自的口径
-- 可能不同：按 token 计的套餐、按金额计的账户、按请求数计的配额。
-- 没有单位列时，route 会把「剩余 800000 token」与「剩余 5 元」放进同一个
-- 1/(1+x) 里比较——分差来自单位而不是余量。
--
-- 取值是**同一口径的判据**，不是展示用的标签：route 只对 unit 完全相同的
-- 渠道比较额度，跨 unit 给中性分（见 internal/route 的 scoreQuota）。
-- 拼错一个字母不会算错，只是把两个本可比的渠道判成不可比（偏保守）。
--
-- 允许 NULL：多数上游查不到额度，那种渠道根本没有行；而写进来的行若来自
-- 一个不知道自己量纲的来源，NULL 比猜一个单位诚实。
ALTER TABLE channel_quota_state ADD COLUMN unit text;

COMMENT ON COLUMN channel_quota_state.unit IS
    'remaining_estimate 的单位，例如 tokens / requests / money。只对 unit 相同的渠道比较额度，NULL 表示未知。';

-- +goose Down
-- 回滚撤销的是两列，不是已经写进去的值——重跑 Up 之后计数从 0 开始，
-- 而那是它该有的初值（下一次调用就会把它算对）。
ALTER TABLE channel_quota_state DROP COLUMN unit;
ALTER TABLE channels DROP COLUMN last_failure_at;
ALTER TABLE channels DROP COLUMN consecutive_failures;
