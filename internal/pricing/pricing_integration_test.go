package pricing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/pipeline"
	"github.com/sumwai/larkax/internal/testdb"
)

// 规则的匹配是 SQL 的活，所以这一层要真的连库。
//
// # 拿内存假装测不出什么
//
// 这里要钉住的性质全都在 SQL 里：作用域的排序、优先级、jsonb 数组的
// 元素匹配、时间窗口的边界、以及「没有任何一行匹配」时返回 ErrNoRule。
// 用一份内存实现去测，测的是「我以为数据库会怎么做」。
//
// 没有 LARKAX_DB_DSN 时跳过而不是失败。**注意「跳过」与「通过」在输出里
// 长得一样**，所以 CI 里有一道专门确认它真的跑了的门。
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	return testdb.Open(t)
}

// TestMain 给这个包建一个独立的库，跑完删掉（见 internal/testdb）。
func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

// 每个用例用独立的标识，避免彼此干扰：同一个库上跑第二次 `go test` 时
// 纯计数会从头开始，撞上上一次留下的行。
var (
	seqMu   sync.Mutex
	seqN    int64
	seqBase = time.Now().UnixNano() % 1_000_000_000
)

func unique(prefix string) string {
	seqMu.Lock()
	defer seqMu.Unlock()
	seqN++
	return fmt.Sprintf("%s-%d-%d", prefix, seqBase, seqN)
}

// singleCandidate 是结算那一次的条件：**恰好一个**候选，也就是实际选中的
// 那个渠道（0 表示未选定，退到平台规则）。
func singleCandidate(model string, channelID, ownerID int64, at time.Time) pipeline.RuleQuery {
	return pipeline.RuleQuery{
		CanonicalModel: model,
		Candidates:     []pipeline.RuleCandidate{{ChannelID: channelID, OwnerUserID: ownerID}},
		At:             at,
	}
}

// ruleSpec 描述一条待插入的规则，零值表示用默认。
type ruleSpec struct {
	scope     string
	ownerID   any // nil 表示不挂商家
	channelID any // nil 表示不挂渠道
	patterns  []string
	expr      string
	priority  int
	enabled   *bool
	from      *time.Time
	to        *time.Time
}

// insertRule 插一条规则并返回 id。
func insertRule(t *testing.T, db *sql.DB, spec ruleSpec) int64 {
	t.Helper()

	if spec.scope == "" {
		spec.scope = "platform"
	}
	if spec.expr == "" {
		spec.expr = `{"prompt": 0.00001}`
	}
	enabled := true
	if spec.enabled != nil {
		enabled = *spec.enabled
	}
	from := time.Now().Add(-24 * time.Hour)
	if spec.from != nil {
		from = *spec.from
	}

	var to any
	if spec.to != nil {
		to = *spec.to
	}

	// 空数组与 NULL 在库里的意思不同：model_patterns 是 NOT NULL，而
	// 「适用于全部模型」的写法是空数组。传 nil 会撞上非空约束，所以这里
	// 明确换成空切片——它是「全部」，不是「没写」。
	patterns := spec.patterns
	if patterns == nil {
		patterns = []string{}
	}

	var id int64
	err := db.QueryRowContext(context.Background(), `
		INSERT INTO pricing_rules
			(scope, owner_user_id, channel_id, name, model_patterns, expr, priority,
			 enabled, effective_from, effective_to)
		VALUES ($1, $2, $3, $4, to_jsonb($5::text[]), $6, $7, $8, $9, $10)
		RETURNING id`,
		spec.scope, spec.ownerID, spec.channelID, unique("rule"),
		patterns, spec.expr, spec.priority, enabled, from, to,
	).Scan(&id)
	if err != nil {
		t.Fatalf("插规则失败: %v", err)
	}

	// 用完就删。这不只是整洁：规则表是**全局**的，一条平台作用域的
	// 「适用全部模型」规则会让「一条都匹配不上」这个状态在整库范围内
	// 消失，而那条分支必须能被测到（见 TestMatchReturnsErrNoRule）。
	// 本包不产生任何指向 pricing_rules 的外键，所以删得掉。
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(),
			`DELETE FROM pricing_rules WHERE id = $1`, id)
	})
	return id
}

// seedChannel 插一条渠道，供渠道作用域的规则挂靠（外键要求它存在）。
//
// 这一条不参与匹配之外的任何事：本包只读规则，渠道的内容（地址、凭证）
// 是候选来源那一侧的事，这里只需要一个合法的 id。
func seedChannel(t *testing.T, db *sql.DB, owner int64) int64 {
	t.Helper()

	var id int64
	err := db.QueryRowContext(context.Background(), `
		INSERT INTO channels (owner_user_id, name, type, base_url, credential_enc)
		VALUES ($1, $2, 'openai', 'http://127.0.0.1:1/v1', '\x00'::bytea)
		RETURNING id`,
		owner, unique("pricing-channel"),
	).Scan(&id)
	if err != nil {
		t.Fatalf("插渠道失败: %v", err)
	}
	return id
}

// seedUser 建一个用户，供商家作用域的规则挂靠（外键要求它存在）。
func seedUser(t *testing.T, db *sql.DB) int64 {
	t.Helper()

	var id int64
	if err := db.QueryRowContext(context.Background(),
		`INSERT INTO users (username) VALUES ($1) RETURNING id`, unique("pricing-user"),
	).Scan(&id); err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	return id
}

// 作用域越具体越优先：渠道价盖过商家价，商家价盖过平台价。
//
// 这条次序是「谁定的价」的次序，而不是优先级的次序——让 priority 压过
// 作用域，等于让平台随时可以悄悄改掉商家的价。
func TestMatchPrefersMostSpecificScope(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	model := unique("model-scope")
	owner := seedUser(t, db)
	ch := seedChannel(t, db, owner)

	// 三条规则同时适用于这个模型，只有作用域不同。
	platform := insertRule(t, db, ruleSpec{patterns: []string{model}, expr: `{"prompt": 1}`})
	merchant := insertRule(t, db, ruleSpec{scope: "merchant", ownerID: owner, patterns: []string{model}, expr: `{"prompt": 2}`})
	channel := insertRule(t, db, ruleSpec{scope: "channel", channelID: ch, patterns: []string{model}, expr: `{"prompt": 3}`})

	at := time.Now()
	cases := []struct {
		name    string
		owner   int64
		channel int64
		want    int64
	}{
		{"渠道与商家都在场，选渠道规则", owner, ch, channel},
		{"只有商家在场，选商家规则", owner, 0, merchant},
		{"两个都是 0（未选定），退到平台规则", 0, 0, platform},
		{"别的商家，退到平台规则", owner + 1, 0, platform},
		{"别的渠道，退到商家规则", owner, ch + 1, merchant},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.Match(ctx, singleCandidate(model, tc.channel, tc.owner, at))
			if err != nil {
				t.Fatalf("匹配失败: %v", err)
			}
			if got.ID != tc.want {
				t.Errorf("规则 id = %d，期望 %d", got.ID, tc.want)
			}
		})
	}
}

// 同一作用域内按 priority 决胜，同优先级按 id 取最新。
//
// 最后一条不是偏好，是确定性：没有它，两条同作用域同优先级的规则谁被
// 选中由行的物理顺序决定，同一份数据两次查询可能给出不同的价。
func TestMatchBreaksTiesWithinScope(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	model := unique("model-tie")
	low := insertRule(t, db, ruleSpec{patterns: []string{model}, priority: 1, expr: `{"prompt": 1}`})
	high := insertRule(t, db, ruleSpec{patterns: []string{model}, priority: 9, expr: `{"prompt": 2}`})

	q := singleCandidate(model, 0, 0, time.Now())

	got, err := s.Match(ctx, q)
	if err != nil {
		t.Fatalf("匹配失败: %v", err)
	}
	if got.ID != high {
		t.Errorf("规则 id = %d，期望 priority 更高的 %d（低的那个是 %d）", got.ID, high, low)
	}

	// 同优先级下再插一条：应当是新的那条胜出。
	newer := insertRule(t, db, ruleSpec{patterns: []string{model}, priority: 9, expr: `{"prompt": 3}`})
	got, err = s.Match(ctx, q)
	if err != nil {
		t.Fatalf("匹配失败: %v", err)
	}
	if got.ID != newer {
		t.Errorf("规则 id = %d，期望同优先级里最新的 %d", got.ID, newer)
	}
}

// 模型过滤：空数组适用于全部模型，非空数组要求逐字匹配。
func TestMatchFiltersByModel(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	owner := seedUser(t, db)
	ch := seedChannel(t, db, owner)

	anyModel := unique("model-any")
	only := unique("model-only")
	other := unique("model-other")

	// 空 patterns 的那条挂在**渠道**作用域下，不是平台作用域。这是刻意的：
	// 平台级的「适用全部模型」一旦存在，ErrNoRule 在这个库上就再也构造不出来了，
	// 而那条分支必须能被测到（见 TestMatchReturnsErrNoRule）。
	anyRule := insertRule(t, db, ruleSpec{scope: "channel", channelID: ch, expr: `{"prompt": 1}`})
	onlyRule := insertRule(t, db, ruleSpec{scope: "channel", channelID: ch, patterns: []string{only}, priority: 5, expr: `{"prompt": 2}`})

	query := func(model string) pipeline.RuleQuery {
		return singleCandidate(model, ch, owner, time.Now())
	}

	// 空数组那条适用于它自己的模型。
	got, err := s.Match(ctx, query(anyModel))
	if err != nil {
		t.Fatalf("匹配失败: %v", err)
	}
	if got.ID != anyRule {
		t.Errorf("规则 id = %d，期望不限定模型的那条 %d", got.ID, anyRule)
	}

	// 指名的那条命中，且它的优先级更高（说明它确实参与了排序，
	// 而不是「恰好只有它匹配」）。
	got, err = s.Match(ctx, query(only))
	if err != nil {
		t.Fatalf("匹配失败: %v", err)
	}
	if got.ID != onlyRule {
		t.Errorf("规则 id = %d，期望指名这个模型的那条 %d", got.ID, onlyRule)
	}

	// 名单外的模型不会命中那条指名规则，只会退到不限定模型的那条。
	got, err = s.Match(ctx, query(other))
	if err != nil {
		t.Fatalf("匹配失败: %v", err)
	}
	if got.ID != anyRule {
		t.Errorf("规则 id = %d，期望退到不限定模型的那条 %d——指名别的模型的规则"+
			"不该命中，那是按元素匹配，不是按子串", got.ID, anyRule)
	}
}

// 停用与时间窗口之外的规则都不参与匹配。
//
// 时间窗口是左闭右开：上界取开区间，于是「把旧规则的 effective_to 设成
// 新规则的 effective_from」不会让两条规则在同一瞬间同时生效。
func TestMatchSkipsDisabledAndOutOfWindow(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	now := time.Now()
	disabled := false
	past := now.Add(-2 * time.Hour)
	future := now.Add(2 * time.Hour)

	model := unique("model-window")
	live := insertRule(t, db, ruleSpec{patterns: []string{model}, expr: `{"prompt": 1}`})
	insertRule(t, db, ruleSpec{patterns: []string{model}, enabled: &disabled, priority: 9, expr: `{"prompt": 2}`})
	insertRule(t, db, ruleSpec{patterns: []string{model}, from: &future, priority: 9, expr: `{"prompt": 3}`})         // 还没生效
	insertRule(t, db, ruleSpec{patterns: []string{model}, to: &past, priority: 9, expr: `{"prompt": 4}`})             // 已过期
	insertRule(t, db, ruleSpec{patterns: []string{model}, from: &past, to: &now, priority: 9, expr: `{"prompt": 5}`}) // 刚好在上界

	got, err := s.Match(ctx, singleCandidate(model, 0, 0, now))
	if err != nil {
		t.Fatalf("匹配失败: %v", err)
	}
	if got.ID != live {
		t.Errorf("规则 id = %d，期望唯一在窗口内且启用的 %d", got.ID, live)
	}
}

// 一条都匹配不上时返回 ErrNoRule，而不是某条兜底规则。
//
// 兜底成零价会让「没配价」表现为「免费」，而免费是最不容易被发现的错账。
//
// 「零行」这个状态是用**时刻**构造的，不是靠「库里恰好没有适用于这个模型的
// 规则」。测试库是共享的，而 pricing_rules 是全局表：平台作用域、空
// model_patterns 的规则适用于**任何**模型，只要它在库里，后一种构造就不成立。
//
// 残留窗口来自**本包自己**：admin_test.go 里有两条平台作用域、空
// model_patterns 的规则——一条绕过写入路径插进去（`INSERT INTO pricing_rules
// (scope, name, expr)`，没写 model_patterns，而那一列的默认值是空数组），
// 一条由 TestCreateAllowsEmptyModelPatterns 正常创建。它们都只在 t.Cleanup 里
// 删，而清理只保证用例**结束之后**库里干净，跑着的那一刻它仍然在；
// `go test ./...` 并行跑包，别的包正好落在那个窗口里就会读到它。
//
// 换的只是 WHERE 上的一个条件（`effective_from <= $1`），走的还是同一个分支：
// 零行 → ErrNoRule。本文件里凡是空 patterns 的规则都挂在渠道作用域下，
// 是为了不给别人添同样的麻烦。
func TestMatchReturnsErrNoRule(t *testing.T) {
	db := testDB(t)
	s := New(db)

	// 1970 早于库里任何一条规则的 effective_from（列默认 now()，迁移里不插规则），
	// 所以那一刻一条都不生效。
	_, err := s.Match(context.Background(),
		singleCandidate(unique("model-nothing"), 0, 0, time.Unix(0, 0).UTC()))
	if !errors.Is(err, pipeline.ErrNoRule) {
		t.Fatalf("err = %v，期望 ErrNoRule", err)
	}
}

// MatchAll 给候选集里**每个候选各取一条**规则，裁决次序与 Match 相同。
//
// 这是预留取上界的输入：候选里最贵的那条规则决定预留，而候选之间的规则
// 差别正来自作用域——渠道自己的价、商家定的价、平台兜底。
func TestMatchAllPicksRulePerCandidate(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	model := unique("model-all")
	ownerA := seedUser(t, db)
	ownerB := seedUser(t, db)
	ownerC := seedUser(t, db)

	// 三个候选，规则各自不同：
	//   chA —— 有自己的渠道级规则（最具体）
	//   chB —— 没有渠道级规则，退到商家 B 的规则
	//   chC —— 商家与渠道都没有规则，退到平台规则
	chA := seedChannel(t, db, ownerA)
	chB := seedChannel(t, db, ownerB)
	chC := seedChannel(t, db, ownerC)

	platform := insertRule(t, db, ruleSpec{patterns: []string{model}, expr: `{"prompt": 1}`})
	merchantB := insertRule(t, db, ruleSpec{scope: "merchant", ownerID: ownerB, patterns: []string{model}, expr: `{"prompt": 2}`})
	channelA := insertRule(t, db, ruleSpec{scope: "channel", channelID: chA, patterns: []string{model}, expr: `{"prompt": 3}`})

	cands := []pipeline.RuleCandidate{
		{ChannelID: chA, OwnerUserID: ownerA},
		{ChannelID: chB, OwnerUserID: ownerB},
		{ChannelID: chC, OwnerUserID: ownerC},
	}
	wantByChannel := map[int64]int64{chA: channelA, chB: merchantB, chC: platform}

	// 结果按 (渠道, 商家) 排序，与查询里的 ORDER BY 一致。先把候选排好，
	// 于是断言不必依赖三个 id 的生成顺序。
	sort.Slice(cands, func(i, j int) bool { return cands[i].ChannelID < cands[j].ChannelID })

	rules, err := s.MatchAll(ctx, pipeline.RuleQuery{
		CanonicalModel: model,
		Candidates:     cands,
		At:             time.Now(),
	})
	if err != nil {
		t.Fatalf("匹配失败: %v", err)
	}
	if len(rules) != len(cands) {
		t.Fatalf("规则 %d 条，期望每个候选各一条（%d 条）", len(rules), len(cands))
	}
	for i, c := range cands {
		if want := wantByChannel[c.ChannelID]; rules[i].ID != want {
			t.Errorf("渠道 %d 的规则 = %d，期望 %d", c.ChannelID, rules[i].ID, want)
		}
	}
}

// 一个候选都匹配不上时返回空，而不是零价的兜底规则。
//
// 「零行」这个状态是用**时刻**构造的（见 TestMatchReturnsErrNoRule）：
// 测试库是共享的，库里可能有一条适用于任何模型的平台规则，靠「库里恰好
// 没有适用的规则」构造不出来。
func TestMatchAllReturnsEmptyWhenNothingMatches(t *testing.T) {
	db := testDB(t)
	s := New(db)

	rules, err := s.MatchAll(context.Background(), pipeline.RuleQuery{
		CanonicalModel: unique("model-nothing-all"),
		Candidates: []pipeline.RuleCandidate{
			{ChannelID: 1, OwnerUserID: 1},
			{ChannelID: 2, OwnerUserID: 2},
		},
		At: time.Unix(0, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("匹配失败: %v", err)
	}
	if len(rules) != 0 {
		t.Errorf("规则 = %+v，期望空——匹配不上不等于零价", rules)
	}
}

// 空候选集返回空，且不去查库。
//
// 「空」在调用方那一侧是有意义的：候选集为空算不出预留上界（见
// pipeline.Reserve），而不是「按平台价算」。
func TestMatchAllWithNoCandidates(t *testing.T) {
	db := testDB(t)
	s := New(db)

	rules, err := s.MatchAll(context.Background(), pipeline.RuleQuery{CanonicalModel: unique("model-no-cand")})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if len(rules) != 0 {
		t.Errorf("规则 = %+v，期望空", rules)
	}
}

// 结算只接受一个候选：把整个候选集交给它，会让「按哪条规则结算」取决于
// 谁排在第一。
func TestMatchRejectsMultipleCandidates(t *testing.T) {
	db := testDB(t)
	s := New(db)

	_, err := s.Match(context.Background(), pipeline.RuleQuery{
		CanonicalModel: unique("model-multi"),
		Candidates: []pipeline.RuleCandidate{
			{ChannelID: 1, OwnerUserID: 1},
			{ChannelID: 2, OwnerUserID: 2},
		},
		At: time.Now(),
	})
	if err == nil {
		t.Fatal("候选多于一个时应报错")
	}
}

// 表达式改了，版本号就要跟着变。
//
// Version 唯一的用途是编译缓存的键：同一 id 的规则改了而版本没变，
// 取到的还是旧表达式的编译产物，表现为「改了规则但没生效」。
func TestMatchVersionTracksExpression(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	model := unique("model-version")
	before := `{"prompt": 0.00001}`
	after := `{"prompt": 0.00002}`

	id := insertRule(t, db, ruleSpec{patterns: []string{model}, expr: before})
	q := singleCandidate(model, 0, 0, time.Now())

	first, err := s.Match(ctx, q)
	if err != nil {
		t.Fatalf("匹配失败: %v", err)
	}
	if first.ID != id {
		t.Fatalf("规则 id = %d，期望 %d", first.ID, id)
	}

	// 直接改库里的表达式，**绕过管理面**。这条路径上没有任何东西维护版本：
	// 没有触发器动 updated_at，也不会有人往 pricing_rule_versions 里插行
	// （插行的是 Admin.Create / Admin.Update，而这里走的是裸 SQL）。
	// 版本号因此必须由源码本身决定，而不是由「有没有人记得写版本表」决定。
	if _, execErr := db.ExecContext(ctx,
		`UPDATE pricing_rules SET expr = $2 WHERE id = $1`, id, after,
	); execErr != nil {
		t.Fatalf("改规则失败: %v", execErr)
	}

	second, err := s.Match(ctx, q)
	if err != nil {
		t.Fatalf("匹配失败: %v", err)
	}
	if second.Source != after {
		t.Fatalf("规则源码 = %q，期望改后的 %q", second.Source, after)
	}
	if second.Version == first.Version {
		t.Error("表达式改了而版本号没变——编译缓存会继续用旧表达式的产物")
	}

	// 改回去，版本号也要回到原值：它是源码的函数，不是计数器。
	if _, execErr := db.ExecContext(ctx,
		`UPDATE pricing_rules SET expr = $2 WHERE id = $1`, id, before,
	); execErr != nil {
		t.Fatalf("改回规则失败: %v", execErr)
	}
	third, err := s.Match(ctx, q)
	if err != nil {
		t.Fatalf("匹配失败: %v", err)
	}
	if third.Version != first.Version {
		t.Error("同一份源码应当得到同一个版本号——它是内容哈希，不是递增计数")
	}
}
