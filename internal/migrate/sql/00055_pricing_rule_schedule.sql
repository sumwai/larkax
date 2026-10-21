-- +goose Up
-- 结构化定价表。NULL 表示这条规则手写表达式。
--
-- # 为什么 expr 照旧存一份
--
-- 表只解决「可枚举」与「写起来不用手写 2^N 支」；求值、预留、读取路径
-- 一行都不改，仍然读 expr。所以写入时把表编译成表达式一起落库，而不变式是：
--
--     schedule IS NOT NULL  ⟹  expr = 由 schedule 编译出的那条表达式
--
-- 它由写入路径保证（见 internal/pricing 的 Schedule.Validate 与 admin.go），
-- 数据库这一层只加一列。不在库里做 CHECK：编译要跑一遍计价引擎的类型检查，
-- SQL 里没有第二份实现，而「两份判据」正是这张表要避免的东西。
--
-- # 为什么是 jsonb 而不是拆表
--
-- 表是**一条规则的内部结构**，不是可以独立查询的实体：没有任何读取路径会
-- 问「哪些规则用了 weekdays 覆盖」。拆成 schedule_entries 会多出一组与规则
-- 同生共死的行、多一次级联删除、多一份「顺序靠 position 而非主键」的约定，
-- 换来的只有「能按覆盖查询」这个今天没人问的能力。
ALTER TABLE pricing_rules ADD COLUMN schedule jsonb;
COMMENT ON COLUMN pricing_rules.schedule IS
    '结构化定价表；NULL 表示手写表达式。非 NULL 时 expr 恰好等于它编译出的结果，'
    '由写入路径保证（读取与求值仍走 expr，没有第二条求值路径）';

-- +goose Down
-- 回滚不丢价：expr 那一栏留着，规则照旧能计价。丢掉的是**可枚举的那一份**
-- 与「这条规则是表写出来的」这个事实，所以回滚前按下面这句导出过一次。
-- +goose StatementBegin
DO $$
DECLARE
    affected bigint;
BEGIN
    SELECT count(*) INTO affected FROM pricing_rules WHERE schedule IS NOT NULL;
    IF affected > 0 THEN
        RAISE NOTICE '有 % 条规则带结构化定价表，回滚后只剩编译结果 expr。', affected
          USING HINT = '需要留档就先导出：SELECT id, name, schedule FROM pricing_rules '
                       'WHERE schedule IS NOT NULL;';
    END IF;
END
$$;
-- +goose StatementEnd

ALTER TABLE pricing_rules DROP COLUMN schedule;
