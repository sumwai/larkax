-- +goose Up
-- 内部监听上 /admin/v1 的管理令牌。
--
-- 这一行在**每个**部署里都要有：settings 的 Registry 与库里的行逐条对照，
-- 缺一个键就拒绝启动（理由见 internal/settings）。所以「从没配置过令牌」
-- 不能表现为「没有这一行」，只能是空串。
--
-- 它存的是令牌明文的 SHA-256（64 位十六进制），不是明文——明文只在
-- `larkaxctl admin token create` 那一次出现在终端上。空串表示没有配置过，
-- 此时 admin API 一律回 503；撤销之后写的是 64 个 0，请求得到 401。
-- 全零不是随手挑的常量，而是一个**约定**：没有任何 32 字节令牌能算出这个
-- SHA-256，所以它匹配不上任何令牌，只是「已撤销」的标记。两者的下一步
-- 不同：一个是先创建一枚，一个是换一枚。
--
-- editable 为 false：写入要记一个 users.id，而管理令牌不对应任何用户。
-- 这一行只由 `larkaxctl admin token create|revoke` 写，`settings set` 会被拒。
INSERT INTO settings (key, value, description, editable) VALUES
    ('admin.token_sha256', '""',
     '内部监听上 /admin/v1 的管理令牌的 SHA-256（小写十六进制）。空串表示没有配置，此时 admin API 一律回 503。由 larkaxctl admin token create / revoke 写入，不接受 settings set；改完下一次请求就生效，不必重启。', false);

-- +goose Down
-- 只删这一行：settings 表本身由 00002 建。
DELETE FROM settings WHERE key = 'admin.token_sha256';
