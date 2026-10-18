-- +goose Up
-- plugins 加三列，记**加载期**那一次尝试的结果。
--
-- # 为什么另记一处，而不是复用已有的两列
--
--   - status 说的是「这一代参不参与注册」，是「要不要」那一档，不是「装上了没有」。
--     一个 active 的插件在某次启动里可能被跳过（源码编译不过，或者它申请的能力
--     这次装配没提供），而它的 status 仍是 active——跳过不改状态是有意的：下次
--     启动照样会再试一次。把 status 写成 failed 的话，「这次装配没提供」就被记成
--     了一个永久的事实：那个插件再也不会被重试。
--   - compile_error 是**发布期**那一列：publish 拒绝编译不过的插件，所以它只在
--     手工改库的行上有值。把加载期的原因也写进去，读的人就分不清「发布时没编译过」
--     与「上一次启动时没装成」——前者去改源码重发，后者往往是重启一次就好。
--
-- # 三列同一次写，不清理历史
--
-- 都可空，而 NULL 有两种来路：加载器没读到过这一行（新发布的、没被取中的、库刚加上
-- 这三列之后还没重启的），或者它试过了而**写回没落地**（连不上库、写超时）——写回失败
-- 只记日志，不影响插件运行（见 store 的 recordLoadResults）。后一种情形下，如果这一行
-- 之前写过，留下的是**上一次**那一份而不再是 NULL，所以这三列要说「最近一次成功写下的
-- 尝试」，读的时候按时刻那一列判它有多旧。加载器每启动一次就把参与注册的那些行刷一
-- 遍，多实例部署时各自写、最后一次赢，可以接受。
-- 没有清理路径：旧的原文被下一次尝试覆盖就够了，留着一份原文也没有第二条读它的路。
--
-- 三列一起写、一起清（成功时把 last_load_error 置回 NULL）：留着上一次的原文，
-- 读的人会以为这一次也失败了。

ALTER TABLE plugins ADD COLUMN last_load_result text
    CHECK (last_load_result IN ('ok', 'failed'));
ALTER TABLE plugins ADD COLUMN last_load_error text;
ALTER TABLE plugins ADD COLUMN last_load_at timestamptz;

COMMENT ON COLUMN plugins.last_load_result IS
    '加载期那一次尝试的结果：ok = 这一代被装上了，failed = 被跳过（原因在 last_load_error）。'
    'NULL 有两种来路：加载器还没读到这一行（它不是那个 key 里取中的一代，或者库刚加上这三列之后还没重启），'
    '或者它试过了而这次写回没落地（写回失败只记日志）。'
    '这一行之前写过的话，写回失败时留下的是上一次那一份，看 last_load_at 判它有多旧。'
    '写它的是加载器（见 internal/plugin/store 的 LoadRegistry），每启动一次把参与注册的行重新刷一遍，'
    '所以它说的是最近一次尝试，不是历史。';
COMMENT ON COLUMN plugins.last_load_error IS
    '加载期被跳过时的错误原文。last_load_result 为 ok 时是 NULL——留着上一次的原文，'
    '读的人会以为这一次也失败了。它与 compile_error 不是一回事：那一列记的是发布期编译不过。';
COMMENT ON COLUMN plugins.last_load_at IS
    '上一次加载期尝试的时刻，取自库里的时钟。三列同一次写，所以这个时刻就是 last_load_result 那一次。'
    '写回落地时才有值：三列都写，而写回失败时三列一起不动，于是它记的是最近一次**写成功**的那一次。'
    '手工改库的行可以只填 last_load_result 而把它留空——读的人不要拿零值当时刻。';

-- +goose Down
-- 三列都是可空的附加记录，回滚只是让管理面重新看不见加载期的结果，没有数据要迁移。
ALTER TABLE plugins DROP COLUMN last_load_at;
ALTER TABLE plugins DROP COLUMN last_load_error;
ALTER TABLE plugins DROP COLUMN last_load_result;
