-- +goose Up
-- 假支付通道的开关。
--
-- # 为什么默认关、而且要在装配期读
--
-- 这条通道没有真实的资金与凭证：它的通知用的是一个写在源码里的占位密钥，
-- 任何人读到这个仓库都能伪造一笔「已支付」。所以它不能顺手打开，默认值必须是
-- false，装配期读到 false 时连回调入口都不挂——那条路径于是与其余未知路径
-- 一样回 501。
--
-- 它不需要在连上数据库之前拿到（见 AGENTS.md 的「配置的分界线」），所以放
-- settings 表，不放环境变量：备份恢复、换机器时它会跟着数据一起回来。
--
-- 打开它的部署必须是明确知道自己在做本地联调的那种；接真实流量的部署上
-- 打开它，等于让任何人只要知道这个仓库就能给任意用户入账。
INSERT INTO settings (key, value, description, editable) VALUES
    ('payment.fake_enabled', 'false',
     '假支付通道的开关。打开后才会挂上支付回调入口，并允许 wallet payment 下单；它只用于本地联调，密钥是公开的占位值，绝不要在接真实流量的部署上打开。', true);

-- +goose Down
-- 只删这一行：settings 表本身由 00002 建。
DELETE FROM settings WHERE key = 'payment.fake_enabled';
