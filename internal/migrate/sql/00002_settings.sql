-- +goose Up
-- 运行期配置。
--
-- 为什么放在数据库而不是环境变量：
--   环境变量必须跟着数据一起走。备份恢复、换机器、切环境时，
--   如果有一批变量没同步过去，数据就不可用了——而这类错误往往
--   在几周后才被发现。放进库里，配置与数据天然同生共死。
--
-- 什么可以放这里：拿不到数据库时不需要的值。留存期、默认模型、
--   额度策略、功能开关、通知模板。
-- 什么不能放这里：连上数据库本身所需的值（DSN、主密钥），
--   以及每个实例可以不同的值（监听地址、日志级别）。那些仍是环境变量。
--
-- 密钥类配置（上游 API Key、支付通道凭证）不放这里，
--   这里的值会明文落盘并进入备份；那些走 channels 表的信封加密。

CREATE TABLE settings (
    key         text PRIMARY KEY,
    value       jsonb NOT NULL,
    -- 一行说明这个键控制什么，避免日久之后没人知道它是干嘛的
    description text NOT NULL DEFAULT '',
    -- 是否允许在控制台修改。false 表示只能由迁移或运维命令写入，
    -- 用于那些改错会造成严重后果、不欢迎随手调整的项
    editable    boolean NOT NULL DEFAULT true,
    updated_by  bigint,
    updated_at  timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE settings IS '运行期配置。与数据同生共死，不随环境变量一起丢失。';

-- 初始默认值。加配置时在这里写默认值，代码里不再写一份，
-- 避免两处默认值不一致。
INSERT INTO settings (key, value, description, editable) VALUES
    ('request_snapshot.retention_days', '30',
     '请求结构快照的留存天数。到期按分区整块删除。', true),
    ('request_snapshot.enabled', 'true',
     '是否采集请求结构快照。关闭后不再写入新快照，已有的按留存期清理。', true),
    ('request_snapshot.max_bytes', '65536',
     '单条结构骨架的字节上限。超限时降级为只保留顶层标量的摘要层。', true),
    ('capture.consent_max_window_hours', '24',
     '用户授权原样采集时，单次授权的最长有效时长。', true),
    ('quota.probe_interval_seconds', '300',
     '额度探测的默认间隔。单个渠道可在 channels.setting 里覆盖。', true),
    ('billing.default_asset', '"CNY"',
     '新用户钱包的默认资产品种。', false),
    ('calendar.default_region', '"CN"',
     '分时计价默认使用的节假日地区。', false);

-- +goose Down
DELETE FROM settings WHERE key IN (
    'request_snapshot.retention_days',
    'request_snapshot.enabled',
    'request_snapshot.max_bytes',
    'capture.consent_max_window_hours',
    'quota.probe_interval_seconds',
    'billing.default_asset',
    'calendar.default_region'
);
DROP TABLE IF EXISTS settings;
