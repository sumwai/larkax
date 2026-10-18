-- +goose Up
-- 给 usage_events.local_hour 与 is_off_day 补两条列注释：说清这两格是什么、
-- 什么时候是空的。
--
-- 两列在 00001_init 里就建好了（可空）。它们上方那句
-- `-- 分时计价的判定依据，事后可复算` 是建表语句里的 SQL 注释，
-- `\d+ usage_events` 看不到它，而这两列此前也没有别的列注释。
--
-- 在此之前 INSERT 里那两个位置写的是字面量 NULL，所以每一行都是空的。
-- 列本来就可空，接上写入点不需要迁移——需要迁移的是这一句话：不写清楚 NULL
-- 在这里是什么意思，后来的人会把「这次求值没用当地口径」读成「求值算出来
-- 就是当地 0 点、不是休息日」。列注释是唯一能跟着列走的地方。
--
-- # 为什么不改 00001
--
-- 同 00017、00019、00023、00031：goose 不做校验和比对，重写一条已应用的迁移
-- 不会报错——已经跑过的库保持旧状态，新库拿到新状态，两者静默分叉。
--
-- 注释里写「本迁移之后」而不是日期：每个环境这一条落地的时刻不同。

COMMENT ON COLUMN usage_events.local_hour IS
    '这次计价用的当地钟点（0–23）。当地 = 地区时区，即 Env.Region 那把时区'
    '（地区由 settings.calendar.default_region 给，见 internal/billing/expr 的 Env）。'
    '它与 is_off_day、priced_at、unit_prices 出自**同一次**求值：求值在选分时档位时'
    '把「当地」解析成一份（时区 → 当地钟点 → 当地那一天 → 那一天的日历属性），这两列取的就是那一份。'
    '本迁移之后，它由结算路径写入（见 internal/ledger/store 的 insertUsageEvent）。'
    '所以事后拿 priced_at 与这两列复算，复出来的是当时那一档的价。'
    '规则没有用到当地口径（isWorkday / isHoliday / isPeak 一个都没走）时为 NULL——'
    '读作「这次计价没有用到当地钟点」，不是「当地 0 点」。';

COMMENT ON COLUMN usage_events.is_off_day IS
    '那一天在当地日历上算不算休息日：周末与法定节假日算，调休补班日不算'
    '（对应 calendar_days.is_off_day）。它是规则里 isWorkday() 的相反数，'
    '与 local_hour 取自同一次求值的同一份解析——两者要么都有值，要么都为 NULL。'
    '规则没有用到当地口径时为 NULL——读作「没有用到」，不是「不是休息日」。';

-- +goose Down
-- 回到「没有列注释」的状态。原状态就是没有注释，所以置 NULL 而不是写回别的文本。
COMMENT ON COLUMN usage_events.local_hour IS NULL;
COMMENT ON COLUMN usage_events.is_off_day IS NULL;
