-- +goose Up
-- 插件系统的存储与配置。
--
-- 两张表分开：plugins 已经在初始迁移里建好（源码、元数据、代次），
-- 这里补的是插件**运行期**需要的东西——它自己的私有 KV，以及四个限额配置。

-- 插件私有 KV。
--
-- 按 (plugin_key, user_id, key) 三段隔离，而不是只按 key：
--   插件 A 与插件 B 用同一个键名不该互相看见；
--   同一个插件在不同用户下也不该互相看见（插件会拿它存租户级的状态）。
--
-- 隔离放在主键里而不是靠查询时的 WHERE：靠 WHERE 的话，漏写一处条件
-- 就是一次跨租户读，而那种错误在测试里通常看不出来。
CREATE TABLE plugin_kv (
    plugin_key  text NOT NULL,
    user_id     bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    key         text NOT NULL,
    value       text NOT NULL,
    -- 到期时间。NULL 表示不过期——显式区分「永不过期」与「已经过期」，
    -- 用一个很远的未来时刻代替 NULL 会让清理逻辑分不清这两种。
    expires_at  timestamptz,
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (plugin_key, user_id, key)
);
COMMENT ON TABLE plugin_kv IS '插件私有键值存储。按插件与用户两段隔离，插件之间与用户之间互不可见。';

-- 过期清理按到期时间扫。部分索引把「永不过期」的行排除在外，
-- 而那些行通常占大多数。
CREATE INDEX plugin_kv_expiry_idx ON plugin_kv (expires_at) WHERE expires_at IS NOT NULL;

-- 插件的运行限额。
--
-- 这三个值都放这里而不是环境变量：它们不需要在连上数据库之前拿到，
-- 而环境变量必须跟着数据一起走——换机器时漏一个，插件就会以另一套
-- 限额运行，而那种差异只有在出问题时才看得出来（见 AGENTS.md 的配置分界线）。
INSERT INTO settings (key, value, description, editable) VALUES
    ('plugin.enabled', 'false',
     '是否启用插件系统。关闭时插件不注册、不执行，请求按没有插件处理。', true),
    ('plugin.call_timeout_ms', '5000',
     '单次插件 hook 调用的时限（毫秒）。超时按插件失败处理，不拖垮请求。', true),
    ('plugin.memory_limit_bytes', '67108864',
     '单个插件运行时的内存上限（字节）。超限中断无法被脚本捕获。', true),
    ('plugin.max_payload_bytes', '4194304',
     '插件单次调用的入参与出参字节上限。内存上限管不住宿主塞进去的值，这一项才管得住。', true);

-- +goose Down
DROP TABLE IF EXISTS plugin_kv;
DELETE FROM settings WHERE key IN (
    'plugin.enabled',
    'plugin.call_timeout_ms',
    'plugin.memory_limit_bytes',
    'plugin.max_payload_bytes'
);
