-- +goose Up
-- 已吊销的控制台会话保留多久，由 larkaxctl maintenance session 兑现。
--
-- 这一列管的是「已吊销、但 expires_at 还没到」的那些行：过期会话到点即删，
-- 不需要窗口。已吊销的为什么还要留一阵——排查「这个会话是什么时候被踢的」
-- 要看它们，删早了那份证据就没了（见 #862）。
--
-- 与 idempotency.retention_hours 同一个形状：留存期只从库里读，命令行上
-- 不另设参数。
INSERT INTO settings (key, value, description, editable) VALUES
    ('console.session_retention_days', '30',
     '已吊销的控制台会话在吊销之后保留多少天，到期由 larkaxctl maintenance session 删除。已过期的会话到点即删，不受这个窗口影响；窗口按 revoked_at 算。', true);

-- +goose Down
DELETE FROM settings WHERE key = 'console.session_retention_days';
