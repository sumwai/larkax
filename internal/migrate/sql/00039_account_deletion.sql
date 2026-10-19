-- +goose Up
-- 账号销户：两步走，行永远不删。
--
-- 第一步（可撤）把 status 置成 deletion_requested，并记下它原来的状态；
-- 第二步（不可逆）置成 deleted、写下 deleted_at，并把这行上的个人身份信息
-- 换成占位。两档都在 status 这一列上，因为鉴权读的正是它（见
-- internal/auth/store.go 的 lookup）：非 active 的用户，手里所有 key 一起失效。
--
-- # 为什么要一列 deletion_prior_status
--
-- 撤销删除要求时要把账号「改回来」，而 status 只存得下一个值。没有这一列的话，
-- 一个 suspended 或 banned 的账号撤销之后会被放回 active——那是一次权限提升，
-- 而且从输出上看不出来。它只在删除要求期间有值，撤销或走进终态时清回 NULL。
--
-- # 为什么扩取值是安全的
--
-- 这是**扩**取值，不是收窄：库里已有的行全部满足新约束，ADD CONSTRAINT
-- 不会拒绝任何存量数据。旧二进制 + 新库照常工作（它不认识这两个取值，但也不会
-- 写出来）。**新二进制 + 旧库不行**：`deleted_at` 进了读侧的列清单，`user list`、
-- `user get` 与 `user update` 都会因为那一列不存在而失败，销户更是写不进去。
-- 这一条迁移必须与二进制一起发布，先跑迁移再换二进制。
--
-- 约束名 users_status_check 由 PostgreSQL 按 `<表>_<列>_check` 自动生成
-- （00001 里那个 CHECK 写在列定义上，没有起名），这里按它 DROP 再 ADD。
-- **不写 IF EXISTS**：名字写错时静默留下旧约束、新取值写不进去，那种失败
-- 要到线上第一次销户到达才现形。
ALTER TABLE users DROP CONSTRAINT users_status_check;
ALTER TABLE users ADD CONSTRAINT users_status_check
    CHECK (status IN (
        'active', 'suspended', 'banned',
        'deletion_requested', 'deleted'));

ALTER TABLE users ADD COLUMN deletion_prior_status text
    CHECK (deletion_prior_status IN ('active', 'suspended', 'banned'));

ALTER TABLE users ADD COLUMN deleted_at timestamptz;

-- 两条状态与列的配套关系交给库来判，而不是只写在应用层：直接改库、或者将来
-- 新加的一条写路径漏了其中一步，都会留下一行「到了终态却没有删除时刻」或者
-- 「停在删除要求上却回不去原来那一个状态」的数据，而鉴权与撤销都读这两列。
-- 还有一条不变式只在应用层（数据库没有触发器）：deleted 表示 key 已作废、插件
-- KV 已清、采集授权已撤销——这三件事由 PurgeDeletion 的一个事务保证，绕过它
-- 直接 UPDATE users 不会连带动那几张表。
ALTER TABLE users ADD CONSTRAINT users_deleted_at_required
    CHECK (status <> 'deleted' OR deleted_at IS NOT NULL);
ALTER TABLE users ADD CONSTRAINT users_deletion_prior_required
    CHECK (status <> 'deletion_requested' OR deletion_prior_status IS NOT NULL);

COMMENT ON COLUMN users.deletion_prior_status IS
    '打标之前的状态，撤销删除要求时回到它。NULL 表示当前不在「要求删除」态。';
COMMENT ON COLUMN users.deleted_at IS
    '匿名化完成的时刻。NULL 表示还没走到终态；行永远不删，账本与钱包按财务留存规则保留。';

-- +goose Down
-- 回到只有三个状态的那一版 schema。
--
-- 先给存量行找一个合法的落点，否则收紧之后那条 ADD CONSTRAINT 会失败：
--   * deletion_requested 回到打标之前的状态（那一列还在，取值也在允许之列）；
--   * deleted 落到 banned——销户是不可逆的，不能假装它还能回到 active。
-- 匿名化本身**不回滚**：邮箱、展示名与用户名已经换成了占位，原来的值没有第二份。
-- 先撤掉配套约束：它们引用的两列马上要被删掉。单独 DROP 而不是等 DROP COLUMN
-- 连坐，读迁移的人不必知道 PostgreSQL 会自动清掉依赖约束这件事。
ALTER TABLE users DROP CONSTRAINT users_deleted_at_required;
ALTER TABLE users DROP CONSTRAINT users_deletion_prior_required;

UPDATE users SET status = COALESCE(deletion_prior_status, 'active')
 WHERE status = 'deletion_requested';
UPDATE users SET status = 'banned' WHERE status = 'deleted';

ALTER TABLE users DROP COLUMN deleted_at;
ALTER TABLE users DROP COLUMN deletion_prior_status;

ALTER TABLE users DROP CONSTRAINT users_status_check;
ALTER TABLE users ADD CONSTRAINT users_status_check
    CHECK (status IN ('active', 'suspended', 'banned'));
