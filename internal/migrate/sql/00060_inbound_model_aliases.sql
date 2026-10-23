-- +goose Up
-- 入站别名：调用方请求里写的名字 → 规范模型名。
--
-- # 为什么是新表
--
-- model_aliases 的语义是「规范名 → 某个渠道上的上游名」，方向是**出站**。
-- 把反方向的映射塞进同一张表，下一次读它的人分不清一个字段说的是哪个方向；
-- 两者的唯一键也不一样：出站映射按渠道分范围，入站别名全局只有一条。
--
-- # 为什么不加外键到 model_catalog
--
-- 目录行可以被删，而「别名指向一个已经不在的规范名」正是 model trace 要报的
-- 断链。外键会把它变成写入侧不可能——一个可诊断的问题就变成了不可表达的
-- 问题。写入时由 catalog.Admin 校验规范名存在；读路径允许断链，并把它报出来。
CREATE TABLE inbound_model_aliases (
    alias          text PRIMARY KEY,
    canonical_name text NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE inbound_model_aliases IS
    '入站别名：调用方请求里的模型名 → 规范模型名。解析只发生在 API 入口一次，之后全链路按规范名走。见 00060。';

-- +goose Down
DROP TABLE inbound_model_aliases;
