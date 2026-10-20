-- +goose Up
-- 控制台的账户凭证：口令、登录失败计数与会话。
--
-- users.password_hash 是迁移 00012 删掉的那一列。那一次的注释写着：它是一
-- 个凭证字段，而没有配套（口令强度、失败锁定、重置流程、会话表），所以删掉，
-- 将来要做口令登录时按 Down 段加回来、并「先定配套」。这一条迁移兑现的就是
-- 后半句——配套在 internal/auth 的 password.go 与 session.go，表在下面。
ALTER TABLE users
    -- 自带参数的哈希串：$pbkdf2-sha256$i=600000,l=32$<salt>$<hash>。
    -- 参数随串走，所以调轮数或换算法时旧口令仍然验证得出来（见 auth.KDFParams）。
    -- NULL 表示运维还没设口令：登录回 account_not_ready，不是 401。
    ADD COLUMN password_hash        text,
    ADD COLUMN password_updated_at  timestamptz,
    -- 按账号的登录节流（跨进程有效）：从第 5 次连续失败起，locked_until 前推
    -- 一段指数增长的时长，成功登录清零。按 IP 的那一层在进程内，不落库。
    ADD COLUMN failed_login_count   integer NOT NULL DEFAULT 0,
    ADD COLUMN locked_until         timestamptz;

-- 展示用的后 6 位。只存哈希的话，列表里那把 key 的后 6 位无从恢复。
-- 老行（larkax_ 方案的 key）这里为 NULL：迁移不猜后缀，展示退化成 prefix + …。
ALTER TABLE api_keys ADD COLUMN key_suffix text;

-- 控制台会话。令牌明文只在 Cookie 里，库里存它的 SHA-256（bytea，32 字节）。
--
-- csrf_token 存明文是**有意的**：它能做的只有「配合这个 HttpOnly Cookie 发
-- 请求」，单看不构成凭证；而能读到这张表的人本来就能直接造一个会话。
CREATE TABLE user_sessions (
    id           bigserial PRIMARY KEY,
    user_id      bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash   bytea NOT NULL UNIQUE,
    csrf_token   text  NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    -- 空闲窗（7 天）与绝对窗（30 天）里来得早的那一个，在每次刷新时重算。
    expires_at   timestamptz NOT NULL,
    revoked_at   timestamptz,
    user_agent   text,
    ip           inet
);
-- 按用户吊销（改口令、运维重置）走这两条偏索引；过期清理走第二条。
CREATE INDEX user_sessions_user_idx
    ON user_sessions (user_id) WHERE revoked_at IS NULL;
CREATE INDEX user_sessions_expiry_idx
    ON user_sessions (expires_at) WHERE revoked_at IS NULL;

-- +goose Down
-- 只回滚这一步是干净的。跨多步回滚（一路撤到 00012 之前）会撞上
-- 「users.password_hash 已存在」——00012 的 Down 段也要加这一列。要跨过
-- 00012 的话先手工删掉这一列，再继续回滚。
DROP TABLE user_sessions;
ALTER TABLE api_keys DROP COLUMN key_suffix;
ALTER TABLE users
    DROP COLUMN locked_until,
    DROP COLUMN failed_login_count,
    DROP COLUMN password_updated_at,
    DROP COLUMN password_hash;
