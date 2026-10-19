package pricing

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

// 成本规则的挑选与售价规则一样是 SQL 的活（窗口、jsonb 数组的元素匹配、
// 优先级的裁决次序），所以这一层也要真的连库。用一份内存实现去测，测的是
// 「我以为数据库会怎么做」。

// costSpec 描述一条待插入的成本规则，零值表示用默认。
type costSpec struct {
	channelID int64
	patterns  []string
	expr      string
	priority  int
	from      *time.Time
	to        *time.Time
}

// seedCostChannel 插一条渠道，供成本规则挂靠（外键要求它存在）。
//
// 渠道要有一个存在的 owner（那一栏是外键），所以顺带建一个用户；这一条不参与
// 匹配之外的任何事，本包只读成本规则。
func seedCostChannel(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	return seedChannel(t, db, seedUser(t, db))
}

// insertCost 直接插一条成本规则并返回 id。
//
// 走裸 SQL 而不是 CostAdmin.Create：用例要构造的是**读取路径看到的那些行**，
// 包括写入路径会拒掉的行（过期的、编译不过的）；用写入路径造夹具会让
// 「写入路径挡住了什么」与「读取路径看见了什么」两件事缠在一起。
func insertCost(t *testing.T, ctx context.Context, db *sql.DB, spec costSpec) int64 {
	t.Helper()

	if spec.expr == "" {
		spec.expr = `{"prompt": 0.000001}`
	}
	// 空清单要显式给成空数组：nil 的 []string 会当成 NULL 发过去，而那一栏
	// 非空（而且「适用于全部模型」正是空数组这个取值）。
	patterns := spec.patterns
	if patterns == nil {
		patterns = []string{}
	}
	from := time.Now().Add(-time.Hour)
	if spec.from != nil {
		from = *spec.from
	}
	var id int64
	err := db.QueryRowContext(ctx, `
		INSERT INTO upstream_costs
			(channel_id, model_patterns, expr, priority, effective_from, effective_to)
		VALUES ($1, to_jsonb($2::text[]), $3, $4, $5, $6)
		RETURNING id`,
		spec.channelID, patterns, spec.expr, spec.priority, from, nullableTime(spec.to),
	).Scan(&id)
	if err != nil {
		t.Fatalf("插成本规则失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM upstream_costs WHERE id = $1`, id)
	})
	return id
}

// matchCost 问一次选择器，返回选中那条的 id；没有适用规则时返回 0。
func matchCost(t *testing.T, ctx context.Context, db *sql.DB, channel int64, model string, at time.Time) int64 {
	t.Helper()

	rule, err := NewCostSource(db).Match(ctx, CostQuery{
		ChannelID: channel, CanonicalModel: model, At: at,
	})
	if errors.Is(err, ErrNoCostRule) {
		return 0
	}
	if err != nil {
		t.Fatalf("Match 失败: %v", err)
	}
	return rule.ID
}

// TestCostMatchPrefersHigherPriority 钉住第一条裁决依据。
//
// 两条规则都适用、模型名单同样具体时，priority 大的赢。
func TestCostMatchPrefersHigherPriority(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	model := unique("cost-model")
	ch := seedCostChannel(t, db)
	low := insertCost(t, ctx, db, costSpec{channelID: ch, patterns: []string{model}, priority: 1})
	high := insertCost(t, ctx, db, costSpec{channelID: ch, patterns: []string{model}, priority: 5})

	if got := matchCost(t, ctx, db, ch, model, time.Now()); got != high {
		t.Errorf("选中的是 %d，期望高优先级的 %d（低的是 %d）", got, high, low)
	}
}

// TestCostMatchTieBreaksOnIDDescending 钉住「同优先级取哪条」。
//
// 两条规则的优先级与模型名单都相同，只有 id 不同：取 id 大的那条。没有这一条，
// 同一份数据两次查询可能给出不同的价——谁被选中取决于行的物理顺序。
func TestCostMatchTieBreaksOnIDDescending(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	model := unique("cost-model")
	ch := seedCostChannel(t, db)
	first := insertCost(t, ctx, db, costSpec{channelID: ch, patterns: []string{model}, priority: 3})
	second := insertCost(t, ctx, db, costSpec{channelID: ch, patterns: []string{model}, priority: 3})

	if second <= first {
		t.Fatalf("夹具的 id 没有递增：%d 之后是 %d", first, second)
	}
	if got := matchCost(t, ctx, db, ch, model, time.Now()); got != second {
		t.Errorf("选中的是 %d，期望后加的那条 %d（先加的是 %d）", got, second, first)
	}
}

// TestCostMatchPrefersModelSpecificOverFallback 钉住第二条裁决依据。
//
// 兜底规则（空的 model_patterns）与模型专属规则同优先级时，模型专属的那条赢，
// **即使兜底那条是后加的**（id 更大）。
func TestCostMatchPrefersModelSpecificOverFallback(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	model := unique("cost-model")
	ch := seedCostChannel(t, db)
	specific := insertCost(t, ctx, db, costSpec{channelID: ch, patterns: []string{model}, priority: 2})
	fallback := insertCost(t, ctx, db, costSpec{channelID: ch, priority: 2})

	if fallback <= specific {
		t.Fatalf("夹具的 id 没有递增：%d 之后是 %d", specific, fallback)
	}
	if got := matchCost(t, ctx, db, ch, model, time.Now()); got != specific {
		t.Errorf("选中的是 %d，期望模型专属的 %d（兜底的是 %d）", got, specific, fallback)
	}

	// priority 压过具体程度：想让兜底赢就把它的优先级调高，那是一次显式的改价。
	withPriority := insertCost(t, ctx, db, costSpec{channelID: ch, priority: 9})
	if got := matchCost(t, ctx, db, ch, model, time.Now()); got != withPriority {
		t.Errorf("兜底那条优先级更高时应当选中它 %d，得到 %d", withPriority, got)
	}
}

// TestCostMatchPrefersNonEmptyModelList 钉住「模型名单里有它」这一个条件：
// 名单里没有这个模型时那条规则不适用，即使它优先级更高。
func TestCostMatchPrefersNonEmptyModelList(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	model := unique("cost-model")
	other := unique("cost-model")
	ch := seedCostChannel(t, db)
	insertCost(t, ctx, db, costSpec{channelID: ch, patterns: []string{other}, priority: 99})
	want := insertCost(t, ctx, db, costSpec{channelID: ch, patterns: []string{model}, priority: 1})

	if got := matchCost(t, ctx, db, ch, model, time.Now()); got != want {
		t.Errorf("选中的是 %d，期望只认这个模型的那条 %d", got, want)
	}
}

// TestCostMatchWindowBoundaries 钉住生效区间的两个端点。
//
// 左闭右开：effective_from <= at < effective_to。两个端点各是一个判据——
// 开始时点那一刻算适用，结束时点那一刻**不算**。
func TestCostMatchWindowBoundaries(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	model := unique("cost-model")
	ch := seedCostChannel(t, db)
	from := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	id := insertCost(t, ctx, db, costSpec{
		channelID: ch, patterns: []string{model}, from: &from, to: &to,
	})

	cases := []struct {
		name string
		at   time.Time
		want int64
	}{
		{"开始时点整点：算适用", from, id},
		{"结束时点整点：不算适用", to, 0},
		{"结束时点往前一纳秒：算适用", to.Add(-time.Nanosecond), id},
		{"开始时点往前一纳秒：不算适用", from.Add(-time.Nanosecond), 0},
		{"区间正中", from.Add(24 * time.Hour), id},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchCost(t, ctx, db, ch, model, tc.at); got != tc.want {
				t.Errorf("在 %s 问一次，选中 %d，期望 %d",
					tc.at.Format(time.RFC3339Nano), got, tc.want)
			}
		})
	}
}

// TestCostMatchSkipsExpiredRule 钉住「已经失效的规则不被选中」。
//
// 一条优先级更高的规则只要过期了就不参与：选中另一条还生效的。库里只剩过期
// 的那条时是 ErrNoCostRule，而不是「按它算」。
func TestCostMatchSkipsExpiredRule(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	model := unique("cost-model")
	ch := seedCostChannel(t, db)
	past := time.Now().Add(-24 * time.Hour)
	older := time.Now().Add(-48 * time.Hour)
	insertCost(t, ctx, db, costSpec{
		channelID: ch, patterns: []string{model}, priority: 99, from: &older, to: &past,
	})

	if got := matchCost(t, ctx, db, ch, model, time.Now()); got != 0 {
		t.Errorf("只剩过期的规则时应当问不出结果，得到 %d", got)
	}

	live := insertCost(t, ctx, db, costSpec{channelID: ch, patterns: []string{model}, priority: 1})
	if got := matchCost(t, ctx, db, ch, model, time.Now()); got != live {
		t.Errorf("过期那条优先级更高也不该被选中：选中的是 %d，期望 %d", got, live)
	}
}

// TestCostMatchIsChannelScoped 钉住「成本规则只在同一条渠道内竞争」。
//
// 同一个模型在两条渠道上是两个上游、两个价：另一条渠道上的规则再高的优先级也
// 不会影响这一条。
func TestCostMatchIsChannelScoped(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	model := unique("cost-model")
	mine := seedCostChannel(t, db)
	theirs := seedCostChannel(t, db)
	mineID := insertCost(t, ctx, db, costSpec{channelID: mine, patterns: []string{model}, priority: 1})
	theirsID := insertCost(t, ctx, db, costSpec{channelID: theirs, patterns: []string{model}, priority: 99})

	if got := matchCost(t, ctx, db, mine, model, time.Now()); got != mineID {
		t.Errorf("渠道 %d 选中的是 %d，期望 %d", mine, got, mineID)
	}
	if got := matchCost(t, ctx, db, theirs, model, time.Now()); got != theirsID {
		t.Errorf("渠道 %d 选中的是 %d，期望 %d", theirs, got, theirsID)
	}

	// 另一条渠道没有规则时，问不出结果——不跨渠道兜底。
	third := seedCostChannel(t, db)
	if got := matchCost(t, ctx, db, third, model, time.Now()); got != 0 {
		t.Errorf("没有规则的那条渠道应当问不出结果，得到 %d", got)
	}
}

// TestCostListHeadIsTheSelectedRule 钉住「列举里排第一的那条就是 Match 选中的那条」。
//
// 两处共用同一段 ORDER BY（见 costOrderBy），所以这条性质是**能读出来的**，
// 不是靠人记得同步两处排序。同一份数据连问两次也要给同一个答案。
func TestCostListHeadIsTheSelectedRule(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	model := unique("cost-model")
	ch := seedCostChannel(t, db)
	insertCost(t, ctx, db, costSpec{channelID: ch, priority: 1})
	insertCost(t, ctx, db, costSpec{channelID: ch, patterns: []string{model}, priority: 4})
	want := insertCost(t, ctx, db, costSpec{channelID: ch, patterns: []string{model}, priority: 4})

	at := time.Now()
	rules, err := NewCostAdmin(db).List(ctx, CostListQuery{
		ChannelID: ch, CanonicalModel: model, At: at,
	})
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	// 适用的有三条：优先级 4 的两条，加上优先级 1 的兜底那条（它适用于全部模型）。
	if len(rules) != 3 {
		t.Fatalf("适用的规则应当有 3 条，得到 %d 条: %+v", len(rules), rules)
	}
	if rules[0].ID != want {
		t.Errorf("清单第一条是 %d，期望 %d", rules[0].ID, want)
	}
	for i := 1; i < len(rules); i++ {
		if rules[i-1].Priority < rules[i].Priority {
			t.Errorf("清单没有按优先级降序：第 %d 条 priority=%d，第 %d 条 priority=%d",
				i, rules[i-1].Priority, i+1, rules[i].Priority)
		}
	}

	for i := range 3 {
		got := matchCost(t, ctx, db, ch, model, at)
		if got != rules[0].ID {
			t.Fatalf("第 %d 次 Match 得到 %d，与清单第一条 %d 不同", i+1, got, rules[0].ID)
		}
	}
}

// TestCostListThreeModes 钉住 list 的三档：全部、按渠道、按渠道与模型。
func TestCostListThreeModes(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	admin := NewCostAdmin(db)

	model := unique("cost-model")
	ch := seedCostChannel(t, db)
	other := seedCostChannel(t, db)
	fallback := insertCost(t, ctx, db, costSpec{channelID: ch})
	specific := insertCost(t, ctx, db, costSpec{channelID: ch, patterns: []string{model}})
	elsewhere := insertCost(t, ctx, db, costSpec{channelID: other, patterns: []string{model}})

	// 全部：三条都在，按渠道分组。
	all, err := admin.List(ctx, CostListQuery{})
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	got := map[int64]bool{}
	for _, r := range all {
		got[r.ID] = true
	}
	for _, id := range []int64{fallback, specific, elsewhere} {
		if !got[id] {
			t.Errorf("不带条件列举时缺了规则 %d", id)
		}
	}

	// 按渠道：只有这条渠道的两条，兜底那条排在后面（模型名单非空的优先）。
	onlyChannel, err := admin.List(ctx, CostListQuery{ChannelID: ch})
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(onlyChannel) != 2 || onlyChannel[0].ID != specific || onlyChannel[1].ID != fallback {
		t.Errorf("按渠道列举得到 %v，期望 [%d %d]", onlyChannel, specific, fallback)
	}

	// 按渠道与模型：只列适用的——兜底那条也适用（适用于全部模型），
	// 另一条渠道的那条不在里面。
	applicable, err := admin.List(ctx, CostListQuery{
		ChannelID: ch, CanonicalModel: model, At: time.Now(),
	})
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(applicable) != 2 || applicable[0].ID != specific || applicable[1].ID != fallback {
		t.Errorf("按渠道与模型列举得到 %v，期望 [%d %d]", applicable, specific, fallback)
	}

	// 过期的规则不出现在「适用」那一档里。
	over := time.Now().Add(-time.Hour)
	if err = admin.Update(ctx, CostUpdateRequest{ID: fallback, EffectiveTo: &over}); err != nil {
		t.Fatalf("把兜底那条改成过期失败: %v", err)
	}
	applicable, err = admin.List(ctx, CostListQuery{
		ChannelID: ch, CanonicalModel: model, At: time.Now(),
	})
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(applicable) != 1 || applicable[0].ID != specific {
		t.Errorf("兜底那条过期之后只剩下 %d 一条，得到 %v", specific, applicable)
	}
}

// TestCostCreateValidatesShape 钉住写入路径挡下的那几类「配了却不生效」。
func TestCostCreateValidatesShape(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	admin := NewCostAdmin(db)
	ch := seedCostChannel(t, db)
	from := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	before := from.Add(-time.Hour)

	cases := []struct {
		name string
		req  CostCreateRequest
		want string
	}{
		{"没有渠道", CostCreateRequest{Expr: `{"prompt": 1}`}, "渠道 ID 0 无效"},
		{"表达式为空", CostCreateRequest{ChannelID: ch, Expr: "   "}, "表达式不能为空"},
		{
			"表达式编译不过",
			CostCreateRequest{ChannelID: ch, Expr: `{"prompt": `},
			"",
		},
		{
			"通配的模型名",
			CostCreateRequest{ChannelID: ch, Expr: `{"prompt": 1}`, ModelPatterns: []string{"claude-*"}},
			"通配符",
		},
		{
			"空清单里有一项是空的",
			CostCreateRequest{ChannelID: ch, Expr: `{"prompt": 1}`, ModelPatterns: []string{""}},
			"模型名为空",
		},
		{
			"上界不晚于下界",
			CostCreateRequest{ChannelID: ch, Expr: `{"prompt": 1}`, EffectiveFrom: from, EffectiveTo: &before},
			"必须晚于 effective_from",
		},
		{
			"上界与下界同一时刻",
			CostCreateRequest{ChannelID: ch, Expr: `{"prompt": 1}`, EffectiveFrom: from, EffectiveTo: &from},
			"必须晚于 effective_from",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := admin.Create(ctx, tc.req)
			if err == nil {
				t.Fatalf("期望被拒，却建成了")
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息 %q 里没有 %q", err.Error(), tc.want)
			}
		})
	}
}

// TestCostCreateThenMatch 钉住写入与读取合成一条链路：建出来的规则真的会被选中，
// 落库的字段与请求逐项对得上。
func TestCostCreateThenMatch(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	admin := NewCostAdmin(db)
	src := NewCostSource(db)

	model := unique("cost-model")
	ch := seedCostChannel(t, db)
	expr := `{"prompt": 0.000002, "completion": 0.000006}`
	id, err := admin.Create(ctx, CostCreateRequest{
		ChannelID: ch, ModelPatterns: []string{model}, Expr: expr, Priority: 7,
	})
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM upstream_costs WHERE id = $1`, id)
	})

	rule, err := src.Match(ctx, CostQuery{ChannelID: ch, CanonicalModel: model})
	if err != nil {
		t.Fatalf("Match 失败: %v", err)
	}
	if rule.ID != id {
		t.Errorf("选中的是 %d，期望刚建的 %d", rule.ID, id)
	}
	if rule.Expr != expr {
		t.Errorf("表达式 = %q，期望 %q", rule.Expr, expr)
	}

	rules, err := admin.List(ctx, CostListQuery{ChannelID: ch})
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("这条渠道应当只有刚建的一条规则，得到 %d 条", len(rules))
	}
	got := rules[0]
	if got.ChannelID != ch || got.Priority != 7 {
		t.Errorf("落库的渠道/优先级 = %d/%d，期望 %d/7", got.ChannelID, got.Priority, ch)
	}
	if len(got.ModelPatterns) != 1 || got.ModelPatterns[0] != model {
		t.Errorf("模型清单 = %v，期望 [%q]", got.ModelPatterns, model)
	}
	if got.ExprErr != nil {
		t.Errorf("表达式应当能编译：%v", got.ExprErr)
	}
	if strings.Join(got.DeclaredKeys, ",") != "completion,prompt" {
		t.Errorf("声明的单价键 = %v，期望 [completion prompt]", got.DeclaredKeys)
	}
	// 没给下界时是「从现在起」：建完立刻问得出来。
	if got.EffectiveFrom.IsZero() {
		t.Error("没给 --effective-from 时应当落一个当前时刻，得到零值")
	}
	if got.EffectiveTo != nil {
		t.Errorf("没给上界时应当落 NULL（不过期），得到 %s", got.EffectiveTo.Format(time.RFC3339))
	}
}

// TestCostUpdateChangesOnlyGivenFields 钉住「只改真的给了的那几项」。
func TestCostUpdateChangesOnlyGivenFields(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	admin := NewCostAdmin(db)

	model := unique("cost-model")
	ch := seedCostChannel(t, db)
	id := insertCost(t, ctx, db, costSpec{channelID: ch, patterns: []string{model}, priority: 3})

	newPriority := 8
	if err := admin.Update(ctx, CostUpdateRequest{ID: id, Priority: &newPriority}); err != nil {
		t.Fatalf("Update 失败: %v", err)
	}

	rules, err := admin.List(ctx, CostListQuery{ChannelID: ch})
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("这条渠道应当只有一条规则，得到 %d 条", len(rules))
	}
	got := rules[0]
	if got.Priority != newPriority {
		t.Errorf("优先级 = %d，期望 %d", got.Priority, newPriority)
	}
	if len(got.ModelPatterns) != 1 || got.ModelPatterns[0] != model {
		t.Errorf("没提到的模型清单被改掉了：%v", got.ModelPatterns)
	}
	if got.ChannelID != ch {
		t.Errorf("没提到的渠道被改掉了：%d", got.ChannelID)
	}

	// 一条都没提到时是 ErrCostNoChanges，而不是一次「成功」的调用。
	if err = admin.Update(ctx, CostUpdateRequest{ID: id}); !errors.Is(err, ErrCostNoChanges) {
		t.Errorf("没给任何字段时期望 ErrCostNoChanges，得到 %v", err)
	}
	// 要改的那条不在库里时是 ErrCostNotFound。
	if err = admin.Update(ctx, CostUpdateRequest{ID: id + 1_000_000, Priority: &newPriority}); !errors.Is(err, ErrCostNotFound) {
		t.Errorf("id 不在库里时期望 ErrCostNotFound，得到 %v", err)
	}
}

// TestCostUpdateValidatesMergedRow 钉住「校验的对象是改完之后的那一行」。
//
// 单独看这次给的字段都合法（上界是一个时刻），但落在库里那一行上就不合法了：
// 上界早于下界。只看「这次给了什么」的写法会把它放进去。
func TestCostUpdateValidatesMergedRow(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	admin := NewCostAdmin(db)

	ch := seedCostChannel(t, db)
	from := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	id := insertCost(t, ctx, db, costSpec{channelID: ch, from: &from})

	before := from.Add(-time.Hour)
	err := admin.Update(ctx, CostUpdateRequest{ID: id, EffectiveTo: &before})
	if err == nil {
		t.Fatal("上界早于库里那一行的下界时应当被拒")
	}
	if !strings.Contains(err.Error(), "必须晚于 effective_from") {
		t.Errorf("错误信息 %q 没有点出窗口那一栏", err.Error())
	}

	// 清成不过期之后它又适用了：--clear-effective-to 表达的是「不再过期」，
	// 不是「不改这一项」。
	after := from.Add(time.Hour)
	if err = admin.Update(ctx, CostUpdateRequest{ID: id, EffectiveTo: &after}); err != nil {
		t.Fatalf("先设一个合法的上界: %v", err)
	}
	if err = admin.Update(ctx, CostUpdateRequest{ID: id, ClearEffectiveTo: true}); err != nil {
		t.Fatalf("清成不过期: %v", err)
	}
	rules, err := admin.List(ctx, CostListQuery{ChannelID: ch})
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(rules) != 1 || rules[0].EffectiveTo != nil {
		t.Errorf("清过之后上界应当是 nil，得到 %+v", rules)
	}
}

// TestCostDisableClosesWindow 钉住停用的语义：把生效区间收在当下。
func TestCostDisableClosesWindow(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	admin := NewCostAdmin(db)

	model := unique("cost-model")
	ch := seedCostChannel(t, db)
	id := insertCost(t, ctx, db, costSpec{channelID: ch, patterns: []string{model}, priority: 5})

	before := time.Now()
	if err := admin.Disable(ctx, id); err != nil {
		t.Fatalf("Disable 失败: %v", err)
	}
	after := time.Now()

	rules, err := admin.List(ctx, CostListQuery{ChannelID: ch})
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("这条渠道应当只有一条规则，得到 %d 条", len(rules))
	}
	to := rules[0].EffectiveTo
	if to == nil {
		t.Fatal("停用之后应当有一个上界，得到不过期")
	}
	if to.Before(before) || to.After(after) {
		t.Errorf("收在当下应当落在 [%s, %s] 之间，得到 %s",
			before.Format(time.RFC3339Nano), after.Format(time.RFC3339Nano), to.Format(time.RFC3339Nano))
	}

	// 停下之后问不出它了，而且**只**是它：同一渠道另一条还生效的规则照旧。
	if got := matchCost(t, ctx, db, ch, model, time.Now()); got != 0 {
		t.Errorf("停用之后不该再选中它，得到 %d", got)
	}
	other := insertCost(t, ctx, db, costSpec{channelID: ch, patterns: []string{model}})
	if got := matchCost(t, ctx, db, ch, model, time.Now()); got != other {
		t.Errorf("停用那条之后应当选中还在生效的 %d，得到 %d", other, got)
	}
}

// TestCostDisableRefusesTwoShapes 钉住停用拒绝的两种情形。
//
//   - 还没生效的规则：照做会写出 effective_to <= effective_from 的空窗口，
//     而同一个形状在 create / update 上是拒的。
//   - 已经失效的规则：把上界改成此刻会把它**拉长**，与「停用」正好相反。
func TestCostDisableRefusesTwoShapes(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	admin := NewCostAdmin(db)

	ch := seedCostChannel(t, db)

	future := time.Now().Add(24 * time.Hour)
	planned := insertCost(t, ctx, db, costSpec{channelID: ch, from: &future})
	err := admin.Disable(ctx, planned)
	if err == nil {
		t.Fatal("还没生效的规则应当被拒")
	}
	if !strings.Contains(err.Error(), "才生效") {
		t.Errorf("错误信息 %q 没有点出「还没生效」", err.Error())
	}

	past := time.Now().Add(-24 * time.Hour)
	older := time.Now().Add(-48 * time.Hour)
	expired := insertCost(t, ctx, db, costSpec{channelID: ch, from: &older, to: &past})
	err = admin.Disable(ctx, expired)
	if err == nil {
		t.Fatal("已经失效的规则应当被拒")
	}
	if !strings.Contains(err.Error(), "就已经失效") {
		t.Errorf("错误信息 %q 没有点出「已经失效」", err.Error())
	}

	// 两种情形都一个字都没改：上界还是原来的那个。
	rules, err := admin.List(ctx, CostListQuery{ChannelID: ch})
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	for _, r := range rules {
		switch r.ID {
		case planned:
			if r.EffectiveTo != nil {
				t.Errorf("被拒之后计划中的那条不该有上界，得到 %s", r.EffectiveTo.Format(time.RFC3339))
			}
		case expired:
			// 截到微秒再比：库里那一栏是微秒精度，而夹具那个时刻带纳秒。
			if r.EffectiveTo == nil || !r.EffectiveTo.Truncate(time.Microsecond).Equal(past.Truncate(time.Microsecond)) {
				t.Errorf("被拒之后失效那条的上界应当还是 %s，得到 %v",
					past.Format(time.RFC3339), r.EffectiveTo)
			}
		}
	}
}

// TestCostListReportsBrokenExpr 钉住「一行表达式坏了不影响别的行」。
//
// 编译不过的表达式只写在那一行的 ExprErr 上，不跳过也不整次报错：库里有一条
// 坏规则时整个清单一条都看不到，而那正是最需要看清单的时候。
func TestCostListReportsBrokenExpr(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	model := unique("cost-model")
	ch := seedCostChannel(t, db)
	broken := insertCost(t, ctx, db, costSpec{channelID: ch, patterns: []string{model}, expr: `{"prompt":`})
	good := insertCost(t, ctx, db, costSpec{channelID: ch, patterns: []string{model}})

	rules, err := NewCostAdmin(db).List(ctx, CostListQuery{ChannelID: ch})
	if err != nil {
		t.Fatalf("一条坏表达式不该让整次列举失败: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("两条都应当列出来，得到 %d 条", len(rules))
	}
	byID := map[int64]CostInfo{}
	for _, r := range rules {
		byID[r.ID] = r
	}
	if byID[broken].ExprErr == nil {
		t.Error("编译不过的那一行应当在 ExprErr 上说明原因")
	}
	if len(byID[broken].DeclaredKeys) != 0 {
		t.Errorf("编译不过时不该有单价键，得到 %v", byID[broken].DeclaredKeys)
	}
	if byID[good].ExprErr != nil {
		t.Errorf("另一行不该被牵连：%v", byID[good].ExprErr)
	}
}

// TestCostSourceMatchValidates 钉住选择器的输入校验。
//
// 渠道是必填的（成本规则按渠道挂，没有「未选定」这一档），模型名不能空——
// 空模型名在 SQL 里匹配不到任何一行，而那时返回 ErrNoCostRule 会把调用方的
// 参数错说成「没配成本」。
func TestCostSourceMatchValidates(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	src := NewCostSource(db)

	if _, err := src.Match(ctx, CostQuery{CanonicalModel: "m"}); err == nil {
		t.Error("渠道 ID 为 0 时应当报错")
	} else if errors.Is(err, ErrNoCostRule) {
		t.Errorf("渠道 ID 无效不是「没有成本规则」，得到 %v", err)
	}
	if _, err := src.Match(ctx, CostQuery{ChannelID: 1}); err == nil {
		t.Error("模型名为空时应当报错")
	} else if errors.Is(err, ErrNoCostRule) {
		t.Errorf("模型名为空不是「没有成本规则」，得到 %v", err)
	}
}
