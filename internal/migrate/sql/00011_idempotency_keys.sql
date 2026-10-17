-- +goose Up
-- 请求级幂等键。
--
-- 没有它的时候，网络超时、连接被中间设备切断、客户端进程被杀这三种情况
-- 都只能靠重试，而重试 = 两次预留 + 两次结算 + 两次真实上游调用。
-- 两条分录各自都平、账目合计也没错，所以**对账看不出来**——
-- 而错误的方向是多收，多收是最难被发现、也最伤信任的那一种。

CREATE TABLE idempotency_keys (
    id                      bigserial PRIMARY KEY,

    -- 作用域是 (api_key_id, key)，不是全局唯一：不同租户各自用自己的键空间。
    -- 全局唯一只会让一个用户能用掉另一个用户的键。
    --
    -- 用 api_key_id 而不是 user_id：一个用户可以有多个 key，而「重试」是
    -- 客户端拿同一把 key 再发一次。
    --
    -- 级联删除跟着 key 走：key 没了，属于它的幂等记录也就没有意义了。
    api_key_id              bigint NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
    key                     text NOT NULL,

    -- 请求体的指纹（sha256，32 字节）。同键不同体一律拒绝，所以它必须
    -- 覆盖影响结果的每一部分。进指纹的字段清单与逐字段理由写在
    -- internal/idempotency 的 hashInput 上——那里是唯一的定义处。
    request_hash            bytea NOT NULL,

    -- in_progress / completed / failed，与 internal/idempotency 的 State 一一对应。
    --
    -- 「走完」是重放的前提：completed 才能重放，另外两个都回 409。
    -- 重放一段截断的响应比不重放更坏——调用方会把它当成一次成功的完整回答。
    state                   text NOT NULL
                            CHECK (state IN ('in_progress', 'completed', 'failed')),

    -- 第一次请求的完整响应。**存字节**而不是「结构化响应」：重放要的是
    -- 逐字节相同，重新渲染一次会引入第二处渲染逻辑，而两处迟早不一致，
    -- 表现为「重试拿到的回答与第一次不一样」。
    --
    -- 流式请求存的就是那串 SSE 字节，连同 Content-Type 一起原样再发一遍。
    response_status         integer,
    response_content_type   text,
    response_body           bytea,

    -- 这次请求在 billing_requests 里的那一行。它与本条记录由**同一个事务**
    -- 写入（见 internal/ledger/store 的 SaveReservation），所以它非零就说明
    -- 预留真的落了库。
    --
    -- 排查「客户端说重试被拒，但这次请求到底扣没扣钱」时，这是唯一的入口。
    billing_request_id      bigint REFERENCES billing_requests(id),

    -- 留存期按 created_at 算，不按 updated_at：后者会被「写回响应」推到请求
    -- 结束那一刻，两者差一次上游调用的时长，拿它当起点会让留存期随上游快慢漂移。
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now()
);

-- 唯一键既是约束也是热路径上的索引：入口按 (api_key_id, key) 定位，
-- 而「同键并发」这件事就靠它来判——插入影响 0 行即说明键已被占住。
ALTER TABLE idempotency_keys ADD CONSTRAINT idempotency_keys_scope_key
    UNIQUE (api_key_id, key);

-- 清理线的索引。没有它，`larkaxctl idempotency cleanup` 每次都要全表扫，
-- 而这张表按设计会一直长——它就是那条「只写不删的表在一年后是一块石头」
-- 的防线，防线自己不能是慢的。
CREATE INDEX idempotency_keys_created_idx ON idempotency_keys (created_at);

COMMENT ON TABLE idempotency_keys IS
    '请求级幂等记录，作用域是 (api_key_id, key)。入口按请求头 Idempotency-Key 读写：'
    '记录在调上游之前、与预留同一个事务里建好；走完了写回完整响应供重放，'
    '没走完标成 failed 且不重放。到期由 larkaxctl idempotency cleanup 按 '
    'settings.idempotency.retention_hours 删除。';

COMMENT ON COLUMN idempotency_keys.request_hash IS
    '请求体指纹（sha256）。同键不同指纹一律拒绝——回第一次的响应等于对一个新请求撒谎。'
    '进指纹的字段清单与理由见 internal/idempotency 的 hashInput。';

COMMENT ON COLUMN idempotency_keys.state IS
    'in_progress：已受理，第一次请求还在跑；completed：走完了，响应完整可重放；'
    'failed：没走完（上游失败、客户端中途断开、进程没了），不重放也不重新执行。';

COMMENT ON COLUMN idempotency_keys.response_body IS
    '第一次请求的响应体原文。流式请求存的是那串 SSE 字节，重放时连同 '
    'response_content_type 一起原样再发一遍。';

COMMENT ON COLUMN idempotency_keys.billing_request_id IS
    '这次请求在 billing_requests 里的那一行。与幂等记录由同一个事务写入，'
    '非零即说明预留落了库。';

-- ── 留存期配置 ──────────────────────────────────────────────────────────
--
-- 默认 24 小时：留太短等于没有（客户端隔一会儿重试就漏过去了），
-- 留太长会让键空间无限增长。
--
-- 必须同时有清理线：settings 里的这个值只有 larkaxctl idempotency cleanup
-- 一个消费者，没人跑它这张表就只增不减。
INSERT INTO settings (key, value, description, editable) VALUES
    ('idempotency.retention_hours', '24',
     '幂等记录的留存小时数。到期由 larkaxctl idempotency cleanup 删除。', true);

-- +goose Down
-- 先删配置行再删表：反过来会让「回滚了但配置还在」——而 settings 里多一个
-- 未声明的键会让服务拒绝启动（见 internal/settings 的 Load）。
DELETE FROM settings WHERE key = 'idempotency.retention_hours';
DROP TABLE IF EXISTS idempotency_keys;
