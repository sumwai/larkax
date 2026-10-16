-- +goose Up
-- 对账任务：校验钱包快照与账本一致。
--
-- 分两部分：
--   1. settings 里的几个开关（怎么跑、发现不一致之后怎么办）
--   2. reconcile_runs：每次运行留下的一行记录。它同时承担两件事——
--      对账结果本身（比对了多少行、发现几条不一致），以及下一次增量
--      扫描的起点（watermark）。这两件事都**必须落库**：
--        * 只打日志的话，「账目不一致」在日志轮转之后就消失了，而它
--          恰恰是要在几周后回答「这笔钱什么时候开始少的」的问题；
--        * 起点记在内存或参数里的话，重启一次就退化成全表扫描。

-- 对账的开关与口径。
--
-- 默认值只在这里写一份，代码里不再写第二份（理由见 internal/settings）。
-- 默认**只报告不修正**：对账会读全表、并且可能改数据，它应当在被明确
-- 开启之后才生效。
INSERT INTO settings (key, value, description, editable) VALUES
    ('reconcile.auto_repair', 'false',
     '对账发现不一致时是否自动修正。关闭时只报告（默认）；打开后按 repair_mode 指定的方向修正。',
     true),
    ('reconcile.repair_mode', '"resync_snapshot"',
     '自动修正的方向。resync_snapshot=以账本为准，把快照重算成账本值（不写分录）；'
     'adjust_ledger=以快照为准，写一条调账分录把账本对齐到快照值，差额记入 reconcile.difference_account。',
     false),
    ('reconcile.difference_account', '"reconcile_difference"',
     'repair_mode=adjust_ledger 时修正分录的对家科目。默认记入 reconcile_difference 单独归集，'
     '不污染平台收入；查清来路后再写一笔人工调账把它转进 platform_revenue。',
     false),
    ('reconcile.shard_size', '1000',
     '一批比对的钱包行数上限。按 user_id 分片逐批处理，避免一次把大表读进内存。',
     true),
    ('reconcile.full_scan_interval_hours', '168',
     '距上一次全量扫描超过这个小时数就做一次全量。增量只看上次起点之后变动过的行，'
     '而「两边都早就错着」的那种不一致只有全量能发现。0 表示每次都全量。',
     true);

-- 每次对账运行的记录。
CREATE TABLE reconcile_runs (
    id                 bigserial PRIMARY KEY,
    started_at         timestamptz NOT NULL DEFAULT now(),
    finished_at        timestamptz,
    -- 本次比对的**起点**，而不是终点。
    --
    -- 下一轮增量从它开始：跑的过程中新写入的行，其 created_at 一定
    -- >= started_at（时间戳同样来自数据库时钟），于是「上一轮读到一半
    -- 才落库的那一行」会被下一轮再读一遍。宁可重复读（比对是幂等的），
    -- 不能漏读。
    watermark          timestamptz NOT NULL,
    full_scan          boolean NOT NULL DEFAULT false,
    checked_wallets    integer NOT NULL DEFAULT 0,
    balance_mismatches integer NOT NULL DEFAULT 0,
    frozen_mismatches  integer NOT NULL DEFAULT 0,
    repaired           integer NOT NULL DEFAULT 0,
    -- 畸形科目不计入上面两项（它既不是余额也不是冻结的差额，而是一笔
    -- 归不到任何用户头上的钱），单独一列，免得它被漏掉。
    malformed_accounts integer NOT NULL DEFAULT 0,
    status             text NOT NULL DEFAULT 'running'
                       CHECK (status IN ('running', 'done', 'failed')),
    error              text
);
CREATE INDEX reconcile_runs_started_idx ON reconcile_runs (started_at DESC);
-- 「上一次跑成的是哪一轮、是不是全量」每轮都要查，用一个部分索引直接命中。
CREATE INDEX reconcile_runs_done_idx
    ON reconcile_runs (started_at DESC) WHERE status = 'done';

COMMENT ON TABLE reconcile_runs IS
    '对账运行记录。既是结果留痕，也是下一轮增量扫描的起点。';

-- 尚未消除的不一致。
--
-- # 为什么必须有这张表
--
-- 增量扫描只看「上次起点之后变动过的行」。一条**没被修掉**的不一致
-- （默认就是只报告不修）本身不会再变动，于是下一轮增量根本不会再看它——
-- 表现为告警响一次就永久安静，而不一致还挂在那里。那比「不告警」更糟：
-- 它看起来像已经处理过了。
--
-- 所以不一致要落成一行，直到某一轮发现它消失了才删掉。有了它：
--   * 每一轮都会把它重新报出来，直到有人处理；
--   * first_seen_at 回答「这笔钱从什么时候开始不对的」；
--   * 候选用户里带上它们，于是增量扫描不会把它们跳过。
--
-- 主键用 (scope, kind) 而不是 (user_id, asset, kind)：畸形科目归不到任何
-- 用户头上，它只能用科目原文来标识，而那个值可能是 ``user :42`` 这种形态。
-- scope 对钱包类不一致是 ``user:<id>@<asset>``，对畸形科目是科目原文。
CREATE TABLE reconcile_findings (
    scope         text NOT NULL,
    kind          text NOT NULL CHECK (kind IN (
                      'balance_mismatch', 'frozen_mismatch',
                      'wallet_missing', 'malformed_account')),
    -- 有可解析的用户 id 时填它（畸形科目填 0），供候选查询与展示。
    user_id       bigint NOT NULL DEFAULT 0,
    asset         text NOT NULL DEFAULT '',
    snapshot      numeric(20, 8) NOT NULL DEFAULT 0,
    ledger        numeric(20, 8) NOT NULL DEFAULT 0,
    -- 与 Finding 的两个说明字段一一对应，原样保留：重新报出来时
    -- 要给出与首次相同的信息，否则第二次看的人得回去翻日志。
    unrepairable  text NOT NULL DEFAULT '',
    detail        text NOT NULL DEFAULT '',
    first_seen_at timestamptz NOT NULL DEFAULT now(),
    last_seen_at  timestamptz NOT NULL DEFAULT now(),
    last_run_id   bigint NOT NULL REFERENCES reconcile_runs(id),
    PRIMARY KEY (scope, kind)
);
-- 候选查询要按 user_id 把这些用户捞回来，与分片扫描的排序一致。
CREATE INDEX reconcile_findings_user_idx ON reconcile_findings (user_id);

COMMENT ON TABLE reconcile_findings IS
    '尚未消除的不一致。每轮重新报出来，直到某一轮发现它消失了。';

-- ── 支撑增量扫描的两条索引 ────────────────────────────────────────────
--
-- 增量扫描要回答的是「自上次起点以来，哪些钱包与用户科目变动过」。
-- 没有索引时这两次查询各是一次全表扫，而它们正是这个任务想避免的开销。
--
-- ledger_entries 上已有的 ledger_account_idx 是 (account, created_at DESC)：
-- 它对「某个科目的历史」有用，对「近期全体用户科目」用不上——查询里
-- account 是一个范围（所有 user:* ），不是等值。所以另建一条按时间、
-- 只覆盖 user 科目的部分索引。
--
-- 判据用 `LIKE 'user%'` 而不是 `LIKE 'user:%'`，与 reconcileMalformed
-- 的查询一致：后者漏掉 `user :42` 这类冒号前多空格的写法，而那条分支
-- 恰恰是为了看见它们。
CREATE INDEX ledger_user_created_idx ON ledger_entries (created_at)
    WHERE account LIKE 'user%';

-- user_wallets 上没有按时间的索引：写入路径每次都用主键更新，
-- 从不需要按 updated_at 扫。增量扫描需要。
CREATE INDEX user_wallets_updated_idx ON user_wallets (updated_at);

-- +goose Down
DROP INDEX IF EXISTS user_wallets_updated_idx;
DROP INDEX IF EXISTS ledger_user_created_idx;
DROP TABLE IF EXISTS reconcile_findings;
DROP TABLE IF EXISTS reconcile_runs;
DELETE FROM settings WHERE key IN (
    'reconcile.auto_repair',
    'reconcile.repair_mode',
    'reconcile.difference_account',
    'reconcile.shard_size',
    'reconcile.full_scan_interval_hours'
);
