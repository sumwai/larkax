-- +goose Up
-- 把「同一个规范名对应多个上游模型名」的歧义挡在写入侧。
--
-- 表上原有的唯一键是 (channel_id, upstream_model)，它只保证
-- 「一个上游名不会在一个渠道里对应两个规范名」，**不保证反向**。反向出现时，
-- 按规范名查上游名会命中多条，而选哪一条没有依据——上游改名或换版本时，
-- 两个名字在上游侧的计价可能不同，选错就是差价，账单看上去还完全正常。
--
-- 所以这里让这种数据根本进不来，而不是在查询侧约定一条取舍规则：
-- 查询侧的取舍是替数据做决定，约束是让数据自己说清楚。
--
-- 需要两条**偏索引**，合成一条普通唯一键不行：SQL 里 NULL 不等于 NULL，
-- 普通唯一键对 channel_id IS NULL 的平台级别名一行都拦不住。
--
-- 索引建在 canonical_name 上，不是 upstream_model：要消掉的是
-- 「规范名 → 上游名」这个方向上的多义。
--
-- 还有一个方向不在本迁移范围内：平台级别名（channel_id IS NULL）在
-- upstream_model 上没有任何约束——原唯一键是 (channel_id, upstream_model)，
-- 对 NULL 那些行一行都管不到。它不产生这里要消的歧义：本包唯一的查询是从
-- 规范名找上游名，命中不了它。真要用到反方向（上游名反查规范名）时再补。

-- 建索引之前先查一遍已有的重复数据。
--
-- 重复是**数据问题**，迁移不该替人决定留哪一条：静默删掉一条等于替数据选了
-- 一个上游模型名，而那正是这条迁移要消除的东西。所以有重复就报错退出，
-- 把重复的组列出来，让人处置完再重跑。
--
-- CREATE UNIQUE INDEX 遇到重复同样会失败，但那时的报错只提第一组，也分不清
-- 是渠道级还是平台级，人拿到它不知道该改哪几行。
--
-- canonical_name IS NULL 的行不参与判断：它没有规范名，两个方向上都谈不上
-- 歧义，而唯一索引本来也把 NULL 当作互不相同。
--
-- +goose StatementBegin
DO $$
DECLARE
    dup_groups integer;
    dup_sample text;
BEGIN
    SELECT count(*) INTO dup_groups
      FROM (
        SELECT 1
          FROM model_aliases
         WHERE canonical_name IS NOT NULL
         GROUP BY channel_id, canonical_name
        HAVING count(*) > 1
      ) g;

    IF dup_groups = 0 THEN
        RETURN;
    END IF;

    SELECT string_agg(detail, '；' ORDER BY n DESC, channel_id NULLS FIRST, canonical_name)
      INTO dup_sample
      FROM (
        SELECT channel_id, canonical_name, count(*) AS n,
               format('channel_id=%s, canonical_name=%s, %s 条',
                      coalesce(channel_id::text, 'NULL（平台级）'), canonical_name, count(*)) AS detail
          FROM model_aliases
         WHERE canonical_name IS NOT NULL
         GROUP BY channel_id, canonical_name
        HAVING count(*) > 1
         ORDER BY count(*) DESC, channel_id NULLS FIRST, canonical_name
         LIMIT 5
      ) s;

    RAISE EXCEPTION USING MESSAGE = format(
        'model_aliases 里有 %s 组 (channel_id, canonical_name) 重复，例如 %s。'
        '先人工处置这些行再重跑这条迁移——静默删掉一条等于替数据选了一个上游模型名。',
        dup_groups, dup_sample);
END
$$;
-- +goose StatementEnd

-- 渠道级：同一个渠道下，一个规范名只能对应一个上游名。
CREATE UNIQUE INDEX model_aliases_channel_canonical_idx
    ON model_aliases (channel_id, canonical_name)
    WHERE channel_id IS NOT NULL;

-- 平台级：一个规范名只能有一个平台级别名。
CREATE UNIQUE INDEX model_aliases_platform_canonical_idx
    ON model_aliases (canonical_name)
    WHERE channel_id IS NULL;

-- +goose Down
-- 只删这两条索引。索引一删，之前被拦下的重复数据又能写进来；回滚撤销的是
-- 约束，不是已经写进去的数据——被约束挡住的那些行本来也没写进来。
DROP INDEX model_aliases_platform_canonical_idx;
DROP INDEX model_aliases_channel_canonical_idx;
