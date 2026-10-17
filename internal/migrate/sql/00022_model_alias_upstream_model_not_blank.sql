-- +goose Up
-- 把「上游模型名一定非空白」这条不变式写进 schema。
--
-- model_aliases.upstream_model 是 text NOT NULL，而 NOT NULL 拦不住空串：
-- '' 与 '   ' 都能存进去。写入侧唯一的把关是 Go 侧的管理面
-- （internal/catalog 的 normalizeUpstreamModel），schema 本身不拦。
--
-- # 为什么这条值得让 schema 也保证
--
-- 请求路径上有一个「它一定非空」的硬前提：internal/ledger/store 的
-- checkUpstreamModel 挡在 Save 开头，为空时报 ErrMissingUpstreamModel。
-- 有人用 SQL 直接写一个空串（或将来某条新的写入路径漏了校验）时，这条路
-- 是这样走下来的：
--
--   Catalog.UpstreamModel 返回 ("", nil) → 请求带着 "model": "" 发给上游 →
--   上游若照收，这次请求**已经成功、钱已经花掉**，走到 Save 才失败。
--
-- 那时账记不上，而钱花掉了——这是最贵的那个形状，且它没有任何东西会报出来。
-- 不变式写在 schema 里之后，「用 SQL 直接写」这条路径自动被覆盖。
--
-- # 判据为什么是 btrim
--
-- '   ' 与 '' 是同一类东西：两者都匹配不上任何上游的模型名，也都不该被当成
-- 「配了一个名字」。Go 侧的 normalizeUpstreamModel 正是按「首尾空白一定是
-- 笔误」把这种值挡在外面的，所以这里只拦空串等于把同一个错误放走一半。
--
-- # 它**不**取代 Go 侧那两道，判的不是同一件事
--
-- 这道 CHECK 只看「写进这一列的值不是空白」。normalizeUpstreamModel 做的还
-- 多一步：它 trim 之后再判，所以 '  x  ' 在那边被规整成 'x' 写进来；直接走
-- SQL 写 '  x  ' 则能通过这道 CHECK（它不是空白）。两者都要在：CHECK 管
-- 「不是空白」，normalize 管「写入的值已经规整过」。
--
-- 而 checkUpstreamModel 判的是**另一样东西**——这次结算带上来的那个值
-- （pipeline.Request.UpstreamModel，它才是落进 billing_requests 的那一列），
-- 不是这里的配置列。三者各管一段，见 store.go 那两段注释。
--
-- # 存量数据
--
-- ALTER TABLE ADD CONSTRAINT ... CHECK 会校验已有行，真有空白行时直接失败，
-- 不会静默装上。但那句报错不指名任何行，也不说有几行，人拿到它不知道该改
-- 哪几行——所以下面先自己查一遍并把它们列出来，与 00007_model_alias_unique.sql
-- 同一个形状。
--
-- 与 00020_narrow_billing_path_check.sql 的处置不同：那一条刻意不写前置检查，
-- 因为分区表上 ADD CONSTRAINT 的报错会**指名分区**；这里什么都不指名。
--
-- 违规行不由迁移处置：删掉或改写它们等于替数据选一个上游模型名，那正是这条
-- 迁移要消掉的东西。列出来让人处置完再重跑。

-- +goose StatementBegin
DO $$
DECLARE
    bad_rows integer;
    bad_sample text;
BEGIN
    SELECT count(*) INTO bad_rows
      FROM model_aliases
     WHERE btrim(upstream_model) = '';

    IF bad_rows = 0 THEN
        RETURN;
    END IF;

    SELECT string_agg(detail, '；' ORDER BY id)
      INTO bad_sample
      FROM (
        SELECT id,
               format('id=%s, channel_id=%s, canonical_name=%s',
                      id,
                      coalesce(channel_id::text, 'NULL（平台级）'),
                      coalesce(canonical_name, 'NULL')) AS detail
          FROM model_aliases
         WHERE btrim(upstream_model) = ''
         ORDER BY id
         LIMIT 5
      ) s;

    RAISE EXCEPTION USING MESSAGE = format(
        'model_aliases 里有 %s 行的 upstream_model 是空白（空串或纯空白），例如 %s。'
        '先人工处置这些行再重跑这条迁移：它们会让请求带着一个空的模型名发给上游，'
        '上游若照收就是「钱花了、账记不上」。',
        bad_rows, bad_sample);
END
$$;
-- +goose StatementEnd

ALTER TABLE model_aliases ADD CONSTRAINT model_aliases_upstream_model_not_blank
    CHECK (btrim(upstream_model) <> '');

-- +goose Down
-- 只删这条约束。回滚撤销的是约束，不是已经写进去的数据——被它挡住的行本来
-- 也没写进来。
--
-- 回滚之后回到「schema 不保证这一列非空白」的状态，这道保证只剩 Go 侧的
-- normalizeUpstreamModel 一道——正是这条迁移要消除的那件事。所以回滚之后要
-- 重新决定怎么处置它，而不是当它一直没问题。
ALTER TABLE model_aliases DROP CONSTRAINT model_aliases_upstream_model_not_blank;
