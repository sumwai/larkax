package pricing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/sumwai/larkax/internal/migrate"
	"github.com/sumwai/larkax/internal/pipeline"
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

	dsn := os.Getenv("LARKAX_DB_DSN")
	if dsn == "" {
		t.Skip("未设置 LARKAX_DB_DSN，跳过集成测试（CI 里会跑）")
	}

	handle, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = handle.Close() })

	if err := handle.PingContext(context.Background()); err != nil {
		t.Skipf("连不上数据库，跳过集成测试: %v", err)
	}

	goose.SetBaseFS(migrate.FS())
	if err := migrate.SetDialect("postgres"); err != nil {
		t.Fatalf("设置方言失败: %v", err)
	}
	if err := goose.UpContext(context.Background(), handle, "."); err != nil {
		t.Fatalf("应用迁移失败: %v", err)
	}
	return handle
}

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
		{"两个都没选定（预留阶段），退到平台规则", 0, 0, platform},
		{"别的商家，退到平台规则", owner + 1, 0, platform},
		{"别的渠道，退到商家规则", owner, ch + 1, merchant},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.Match(ctx, pipeline.RuleQuery{
				CanonicalModel: model,
				ChannelID:      tc.channel,
				OwnerUserID:    tc.owner,
				At:             at,
			})
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

	q := pipeline.RuleQuery{CanonicalModel: model, At: time.Now()}

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
		return pipeline.RuleQuery{
			CanonicalModel: model,
			ChannelID:      ch,
			OwnerUserID:    owner,
			At:             time.Now(),
		}
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

	got, err := s.Match(ctx, pipeline.RuleQuery{CanonicalModel: model, At: now})
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
// 规则」。测试库是共享的：别的包会留下平台作用域、空 model_patterns 的规则
// （当前是 internal/ledger/store 的 TestReservationRecordsPricingRule 造的，
// 跑完没删），那种规则适用于任何模型，于是后一种构造在共享库上不可达——
// 一条属于别人的、跑完就留下的行不该让这条用例变红。
//
// 换的只是 WHERE 上的一个条件（`effective_from <= $1`），走的还是同一个分支：
// 零行 → ErrNoRule。本文件里凡是空 patterns 的规则都挂在渠道作用域下，
// 是为了不给别人添同样的麻烦。
func TestMatchReturnsErrNoRule(t *testing.T) {
	db := testDB(t)
	s := New(db)

	// 1970 早于库里任何一条规则的 effective_from（列默认 now()，迁移里不插规则），
	// 所以那一刻一条都不生效。
	_, err := s.Match(context.Background(), pipeline.RuleQuery{
		CanonicalModel: unique("model-nothing"),
		At:             time.Unix(0, 0).UTC(),
	})
	if !errors.Is(err, pipeline.ErrNoRule) {
		t.Fatalf("err = %v，期望 ErrNoRule", err)
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
	q := pipeline.RuleQuery{CanonicalModel: model, At: time.Now()}

	first, err := s.Match(ctx, q)
	if err != nil {
		t.Fatalf("匹配失败: %v", err)
	}
	if first.ID != id {
		t.Fatalf("规则 id = %d，期望 %d", first.ID, id)
	}

	// 直接改库里的表达式。没有触发器维护 updated_at，也没有人往
	// pricing_rule_versions 里插行——版本号因此必须由源码本身决定。
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
