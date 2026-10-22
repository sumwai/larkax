# 47 · `INSERT ... SELECT` 的 select 列表里，参数类型推断不出来，会按 text 处理

**编号** 47 · **日期** 2026-10-22 · **摘要**：把一条 `INSERT ... VALUES ($1, ...)`
改写成 `INSERT ... SELECT $1, ...` 之后，`$1` 在 select 列表里没有任何上下文，
Postgres 把它判成 `unknown` 再按 `text` 处理，往 `bigint` / `timestamptz` 列里
一塞就报 42804。

**现象**

```
ERROR: column "channel_id" is of type bigint but expression is of type text
```

同一句 SQL 在 `INSERT ... VALUES` 的写法下是对的：

```sql
-- 对：目标列的类型会推给 $1
INSERT INTO channel_health_rollup (channel_id, bucket, ...) VALUES ($1, $2, ...)

-- 错：select 列表里的 $1 没有上下文
INSERT INTO channel_health_rollup (channel_id, model, bucket, ...)
SELECT DISTINCT $1, t.model, $3, 1, $4, $5
  FROM (VALUES ($2::text), (''::text)) AS t(model)
```

**原因**

`INSERT ... VALUES` 的表达式直接对应目标列，解析时目标列的类型会被用来定参数类型；
`INSERT ... SELECT` 不同——那边的 SELECT 先被当成一条**普通查询**分析，分析完才知道
往哪儿插，所以 select 列表里裸着的 `$n` 一律是 `unknown`，最终回落成 `text`。
`WHERE 列 = $n` 里的参数没事，因为它有比较的上下文。

**解决**

按目标列的类型给每个参数显式转型：

```sql
SELECT DISTINCT $1::bigint, t.model, $3::timestamptz, 1, $4::bigint, $5::bigint
```

**预防**

- 改写语句形状（`VALUES` → `SELECT`、搬进 CTE）时，**参数的类型来源会跟着变**，
  这不是「等价改写」。
- 写完一句带参数的 SQL，对每个 `$n` 说一句「它的类型是从哪儿推出来的」：
  目标列、显式 `::`、还是 `WHERE 列 = $n`。说不出来就是 `unknown`。
- 同第 16 条：这一类错只在真库上现形，需要库的用例整包跳过时本地永远是绿的。
