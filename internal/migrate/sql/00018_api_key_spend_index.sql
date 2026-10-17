-- +goose Up
-- 给「这把 key 累计花了多少」那条查询补一条索引。
--
-- 额度判定要按 api_key_id 汇总 billing_requests（见 internal/keyquota 的
-- spentSQL），而它落在**每次预留**上。api_key_id 上原本没有索引，于是这条
-- 查询是整表顺序扫描，扫的是全表而不是这一把 key 的行——表越大越慢，而它
-- 在热路径上，一次请求都躲不掉。
--
-- INCLUDE 里的三列正是那条查询读的列（status 用于分支，final_amount 与
-- reserved_amount 是求和的两项）。带上它们，PostgreSQL 能只走索引
-- （index-only scan）而不回表取行——这张表的写入量不大，多维护三列副本的
-- 代价远小于每次预留都回表。
--
-- 它**不解决**的是：汇总仍然是「这把 key 的全部历史行」，随这把 key 用过的
-- 次数线性增长。真要扛住那种量级，得改成一个随预留/结算/释放一起维护的
-- 累计值（那要额外保证它和这里的求和永远相等）。现在不做，是因为那种做法
-- 的失效方式是静默的——漏掉一处维护点，额度就永远算少，而没有任何东西会
-- 报出来。口径与代价写在 internal/keyquota 的包注释里。
CREATE INDEX billing_requests_api_key_idx
    ON billing_requests (api_key_id)
    INCLUDE (status, final_amount, reserved_amount);

-- +goose Down
-- 回到「api_key_id 上没有索引」的状态。回退之后那条查询会退化成顺序扫描，
-- 功能不受影响。
DROP INDEX billing_requests_api_key_idx;
