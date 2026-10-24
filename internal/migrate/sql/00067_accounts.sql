-- +goose Up
-- 凭证从渠道行拆成账号实体。
--
-- 一家上游常常开多条渠道，而这几条渠道用的是同一把 key。此前凭证密文直接
-- 挂在 channels 行上，于是同一把 key 存好几份、换 key 要逐条 update，而
-- 「这几条渠道其实共用一个账号」这个事实在库里根本不存在。
--
-- # 两张表
--
--   accounts         一个凭证本体：名字、密文、末四位提示。
--   channel_accounts 渠道 × 账号的多对多关联，带 weight。
--
-- 一条渠道可以挂零个、一个或多个账号：零个是合法的中间态（先建渠道、后接
-- 账号），多个是同一个上游开了好几把 key 的池子。选路读第一条（按账号 id
-- 升序），池内的调度由后续工作接手。
--
-- # 主密钥引用在哪
--
-- 不另设 master_key 列：密文是自描述的信封，头部记着「这份数据密钥被哪几把
-- 主密钥包过」（见 internal/credential）。主密钥轮换改写的就是那个头部，
-- 再存一列等于把同一个事实写两处，而两处只会在轮换之后分岔——读的人不知道
-- 该信哪一边。
--
-- # 存量数据
--
-- 每条渠道现有的凭证密文原样搬进一个新账号，再建一条关联。**复制密文而不是
-- 解密重加密**：信封是自包含的，同一个字节串在 accounts 里与在 channels 里
-- 含义完全相同，而重加密要先把主密钥交给迁移、再把明文在进程里过一遍——
-- 两件都不该发生在一次 schema 迁移里。
--
-- 账号名取 `channel-<渠道 id>`：渠道名没有唯一约束（软删之后重建同名渠道是
-- 常见操作），而账号名必须唯一。带上 id 之后，运维仍能凭 `channel list` 里的
-- 渠道 id 找回它对应的账号。
--
-- # 可重入性
--
-- 这段搬数据的 INSERT 在一次成功之后**无法重入**：它的源列在同一个迁移末尾
-- 被删掉了。迁移器把每条迁移包在一个事务里，任何一步失败都整体回滚，所以
-- 不存在「搬了一半」的中间态；成功之后重跑也没有源可搬。两条 INSERT 因此
-- 不需要 ON CONFLICT 之类的幂等写法。
CREATE TABLE accounts (
    id              bigserial PRIMARY KEY,
    -- 运维用来指代这个账号的名字，命令行里按它定位。迁移生成的叫
    -- `channel-<渠道 id>`，之后新建的缺省是 `account-` 加六位随机串。
    name            text NOT NULL UNIQUE CHECK (name <> ''),
    -- 凭证密文。格式与旧的 channels.credential_enc 完全相同（信封加密，
    -- 唯一解密点是 internal/channels）。
    credential_enc  bytea NOT NULL,
    -- 末四位，供界面辨识。它不是凭证的一部分，也不足以还原凭证。
    credential_hint text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    -- 「换 key 的时刻」。账号改名或改权重不动它。
    updated_at      timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE accounts IS
    '上游凭证的独立实体。一条渠道通过 channel_accounts 挂一个或多个账号，'
    '换 key 改这里，同池渠道一起生效。见 00067。';
COMMENT ON COLUMN accounts.credential_enc IS
    '信封加密的凭证密文，头部自描述「被哪几把主密钥包过」，'
    '不另设主密钥列，避免轮换时两处漂移';

CREATE TABLE channel_accounts (
    channel_id  bigint NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    account_id  bigint NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    -- 池内权重，默认与其他渠道权重同为 1。0 是合法取值（等价于「先别用」），
    -- 负数没有含义。
    weight      integer NOT NULL DEFAULT 1 CHECK (weight >= 0),
    -- 同一渠道下的同一个账号只有一行：重复关联没有意义，主键顺带挡住它。
    PRIMARY KEY (channel_id, account_id)
);

-- 主键是 (channel_id, account_id)，按账号反查「它还挂在哪些渠道上」走不了
-- 它，所以为 account_id 单独建一条索引。
CREATE INDEX channel_accounts_account_idx ON channel_accounts (account_id);

COMMENT ON TABLE channel_accounts IS
    '渠道与账号的多对多关联。渠道零账号是合法中间态，选路跳过它。见 00067。';

-- 存量：一条渠道一个账号，密文与提示原样搬过去。
-- 回滚过的库里可能有零账号渠道（Down 把它们留成了 NULL）；Up 再跑一次时
-- 不为它们建账号——零账号本来就是它们在那个世界里的意思。
INSERT INTO accounts (name, credential_enc, credential_hint, created_at, updated_at)
SELECT 'channel-' || c.id, c.credential_enc, c.credential_hint,
       c.created_at, c.updated_at
  FROM channels c
 WHERE c.credential_enc IS NOT NULL
 ORDER BY c.id;

INSERT INTO channel_accounts (channel_id, account_id, weight)
SELECT c.id, a.id, 1
  FROM channels c
  JOIN accounts a ON a.name = 'channel-' || c.id
 ORDER BY c.id;

-- 凭证从此只有 accounts 一处。留着旧列会让两条写路径各自写一份，而它们
-- 只在换 key 之后分岔。
ALTER TABLE channels
    DROP COLUMN credential_enc,
    DROP COLUMN credential_hint;

-- +goose Down
-- 回滚把凭证搬回渠道行，然后删掉两张表。
--
-- # 有损的两处，都是旧模型表达不了的
--
--   - 一条渠道挂了多个账号时只有**账号 id 最小**的那一个能搬回来：旧模型里
--     一条渠道只有一份凭证。其余账号随 accounts 表一起消失。
--   - 零账号的渠道搬不回来。这里**不**替它编一份密文（那会写进一份解不开的
--     假凭证），也**不**删掉这一行（回滚不该销毁数据），所以 credential_enc
--     在回滚态是**可空**的，比 00001 的定义松一格。旧版二进制读到这种行会
--     解不开——但这类渠道在旧模型里本来就不存在。
ALTER TABLE channels
    ADD COLUMN credential_enc bytea,
    ADD COLUMN credential_hint text;

UPDATE channels c
   SET credential_enc = picked.credential_enc,
       credential_hint = picked.credential_hint
  FROM (
        SELECT DISTINCT ON (ca.channel_id)
               ca.channel_id, a.credential_enc, a.credential_hint
          FROM channel_accounts ca
          JOIN accounts a ON a.id = ca.account_id
         ORDER BY ca.channel_id, a.id
       ) picked
 WHERE picked.channel_id = c.id;

DROP TABLE channel_accounts;
DROP TABLE accounts;
