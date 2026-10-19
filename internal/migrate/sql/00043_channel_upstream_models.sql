-- +goose Up
-- 落库「某条渠道的上游模型清单」，以及它是什么时候、以什么状态同步来的。
--
-- # 这不是 channels.models
--
-- channels.models 存的是**规范模型名**，读取路径拿它当选路白名单。上游清单
-- 给的是上游自己的模型名（`gpt-x`、`claude-x-20260101` 这类），两者之间隔着
-- model_aliases 那张映射表。把前者写进后者，会让一批根本调不动的名字变成
-- 「这条渠道声称支持」——失败要等到第一次真打才现形。所以清单落在这张新表里，
-- 路由与展示都不读它；它是给人看的一份**事实**，要不要把它变成别名由人决定。
--
-- # 清单来自一次显式同步
--
-- 写入方只有 `larkaxctl channel sync-models`。启动时不拉、请求路径上不调用、
-- 也没有后台轮询，所以这张表记的是**上一次同步那一刻**的上游清单，会过时。
-- 时间戳一起落库，读的人才能判断它值不值得信。
--
-- # 两条表分开的理由
--
-- channel_model_syncs 每条渠道至多一行，记「同步过没有、上次是什么结论」；
-- channel_upstream_models 每个模型一行，记「哪些名字、各自什么时候第一次和
-- 最近一次出现在清单里」。两者更新的时机不同：上游不支持拉清单时，只能写前
-- 者，后者一条都不该动——「不支持」不构成「清单是空的」的证据。
CREATE TABLE channel_model_syncs (
    channel_id      bigint PRIMARY KEY REFERENCES channels(id) ON DELETE CASCADE,
    -- synced      上次同步成功，清单是 channel_upstream_models 里那些行
    -- unsupported 上游不提供模型列表；这是正常情况，不是失败
    --
    -- 「从未同步过」不在这张表里：没有这一行就是没同步过。为它存一行
    -- 'never' 会让「没配过」与「配过、结果是空」在库里长得一样。
    status          text NOT NULL CHECK (status IN ('synced', 'unsupported')),
    -- attempted_at 是最近一次尝试的时刻，成功与否都更新
    attempted_at    timestamptz NOT NULL,
    -- synced_at 是最近一次**成功**拉取的时刻；unsupported 时可以为空，也可以
    -- 留着更早那一次成功的时间——那一份清单还在 channel_upstream_models 里。
    synced_at       timestamptz,
    -- synced 必须有同步时刻；unsupported 两种都允许。
    CONSTRAINT channel_model_syncs_synced_at_check
        CHECK (status <> 'synced' OR synced_at IS NOT NULL)
);
COMMENT ON TABLE channel_model_syncs IS
    '渠道上游模型清单的同步状态。没有行 = 从未同步过。只由 larkaxctl channel sync-models 写，见 00043。';

CREATE TABLE channel_upstream_models (
    channel_id      bigint NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    -- 上游自己的模型名，不是规范名。原样保存，不做大小写或前后空白归一：
    -- 请求里要填的就是上游认的那个字符串。
    upstream_model  text NOT NULL,
    first_seen_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at    timestamptz NOT NULL DEFAULT now(),
    -- 同一渠道下同一个名字只有一行：重复同步不会产生重复行，只会刷新
    -- last_seen_at。这也让「一行 = 一个当前可用模型」成立。
    PRIMARY KEY (channel_id, upstream_model)
);
COMMENT ON TABLE channel_upstream_models IS
    '某条渠道上一次成功同步时上游给出的模型名。不是选路用的 channels.models（那里是规范名），读取路径不读本表。见 00043。';

-- +goose Down
-- 两张表都只存「一次同步的快照」，丢了可以重新同步，不回填。
DROP TABLE channel_upstream_models;
DROP TABLE channel_model_syncs;
