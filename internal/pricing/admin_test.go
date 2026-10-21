package pricing

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sumwai/larkax/internal/billing/expr"
	"github.com/sumwai/larkax/internal/pipeline"
)

// ── 测试台 ──────────────────────────────────────────────────────────────

// createRule 通过管理面建一条规则，并登记用完删掉。
//
// 规则表是**全局**的：一条平台作用域、空 model_patterns 的规则会让
// 「一条都匹配不上」这个状态在整库范围内消失（见 insertRule 的注释），
// 所以本文件建的每一条规则都要删干净。
func createRule(t *testing.T, admin *Admin, req CreateRequest) int64 {
	t.Helper()

	if req.Name == "" {
		req.Name = unique("rule")
	}
	if req.Expr == "" {
		req.Expr = `{"prompt": 0.00001}`
	}

	id, err := admin.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("Create 出错: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.db.ExecContext(context.Background(),
			`DELETE FROM pricing_rules WHERE id = $1`, id)
	})
	return id
}

// ruleVersions 按版本号顺序读出这条规则的表达式历史。
//
// 只取 expr 一列：这张表承诺的就是「成交价的依据留痕」。
func ruleVersions(t *testing.T, db *sql.DB, ruleID int64) []string {
	t.Helper()

	rows, err := db.QueryContext(context.Background(),
		`SELECT expr FROM pricing_rule_versions WHERE rule_id = $1 ORDER BY version`, ruleID)
	if err != nil {
		t.Fatalf("读版本历史失败: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var expr string
		if err := rows.Scan(&expr); err != nil {
			t.Fatalf("读版本历史失败: %v", err)
		}
		out = append(out, expr)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历版本历史失败: %v", err)
	}
	return out
}

// listAll 列举全部规则，出错就结束用例。
func listAll(t *testing.T, admin *Admin) []RuleInfo {
	t.Helper()

	infos, err := admin.List(context.Background())
	if err != nil {
		t.Fatalf("List 出错: %v", err)
	}
	return infos
}

// findRule 从列举结果里挑出指定 id 的那条。
func findRule(t *testing.T, infos []RuleInfo, id int64) RuleInfo {
	t.Helper()

	for _, info := range infos {
		if info.ID == id {
			return info
		}
	}
	t.Fatalf("列举里没有规则 %d（共 %d 条）", id, len(infos))
	return RuleInfo{}
}

// rankOf 返回一条规则在裁决次序里的位置。
//
// 断言「谁排在谁前面」用它而不是直接用下标：测试库是共享的，别的用例与别的
// 包都会留下规则，绝对位置不由本用例决定。
func rankOf(t *testing.T, infos []RuleInfo, id int64) int {
	t.Helper()

	for i, info := range infos {
		if info.ID == id {
			return i
		}
	}
	t.Fatalf("结果里没有规则 %d（共 %d 条）", id, len(infos))
	return -1
}

// countRuleByName 数库里叫这个名字的规则有几条，用来断言「被拒的请求一行
// 都没留下」。
func countRuleByName(t *testing.T, db *sql.DB, name string) int {
	t.Helper()

	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM pricing_rules WHERE name = $1`, name).Scan(&n); err != nil {
		t.Fatalf("按名字计数失败: %v", err)
	}
	return n
}

// rawRuleOwner 直接读库里那一栏，用来断言「平台自营存的是 NULL 而不是 0」。
func rawRuleOwner(t *testing.T, db *sql.DB, id int64) sql.NullInt64 {
	t.Helper()

	var owner sql.NullInt64
	if err := db.QueryRowContext(context.Background(),
		`SELECT owner_user_id FROM pricing_rules WHERE id = $1`, id).Scan(&owner); err != nil {
		t.Fatalf("读 owner_user_id 失败: %v", err)
	}
	return owner
}

// requireValidationError 断言这次失败是**写入路径自己拦下的**，而不是数据库
// 拦下的。
//
// 两者都表现为一个 error，但只有前者说明校验真的在跑：交给数据库拦的话，
// 错误要到 INSERT 那一刻才出现，而那时事务、连接都已经用上了，报出来的也是
// SQLSTATE 而不是「哪个字段不对」。
func requireValidationError(t *testing.T, err error) {
	t.Helper()

	if err == nil {
		t.Fatal("非法输入应当被拒")
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		t.Errorf("这次失败来自数据库（SQLSTATE %s），说明写入路径没有先拦住它: %v",
			pgErr.Code, err)
	}
}

// ── 创建 ────────────────────────────────────────────────────────────────

// 通过管理面建的规则要能被**现有读取路径**选中，而不只是「行插进去了」。
//
// 这一条证明写入侧与读取侧对同一套列（作用域、时间窗口、模型名单、表达式）
// 的假设一致。
func TestCreateThenMatchReadsItBack(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)
	ctx := context.Background()

	owner := seedUser(t, db)
	ch := seedChannel(t, db, owner)
	model := unique("model-create")
	source := `{"prompt": 0.00001, "completion": 0.00002}`
	from := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	id := createRule(t, admin, CreateRequest{
		Scope:         ScopeChannel,
		ChannelID:     ch,
		Name:          "channel-price-" + model,
		ModelPatterns: []string{model},
		Expr:          source,
		Priority:      4,
		EffectiveFrom: from,
		EffectiveTo:   &to,
	})

	got, err := New(db).Match(ctx, singleCandidate(model, ch, owner, from.Add(time.Hour)))
	if err != nil {
		t.Fatalf("Match 出错: %v", err)
	}
	if got.ID != id {
		t.Errorf("选中的规则 id = %d，期望刚建出来的 %d", got.ID, id)
	}
	if got.Source != source {
		t.Errorf("规则源码 = %q，期望 %q", got.Source, source)
	}

	info := findRule(t, listAll(t, admin), id)
	if info.Scope != ScopeChannel {
		t.Errorf("Scope = %q，期望 %q", info.Scope, ScopeChannel)
	}
	if info.ChannelID != ch {
		t.Errorf("ChannelID = %d，期望 %d", info.ChannelID, ch)
	}
	if info.OwnerUserID != 0 {
		t.Errorf("OwnerUserID = %d，期望 0（channel 作用域不挂 owner）", info.OwnerUserID)
	}
	if info.Priority != 4 || !info.Enabled {
		t.Errorf("Priority/Enabled = %d/%v，期望 4/true", info.Priority, info.Enabled)
	}
	if !reflect.DeepEqual(info.ModelPatterns, []string{model}) {
		t.Errorf("ModelPatterns = %v，期望 %v", info.ModelPatterns, []string{model})
	}
	if !info.EffectiveFrom.Equal(from) {
		t.Errorf("EffectiveFrom = %s，期望 %s", info.EffectiveFrom, from)
	}
	if info.EffectiveTo == nil || !info.EffectiveTo.Equal(to) {
		t.Errorf("EffectiveTo = %v，期望 %s", info.EffectiveTo, to)
	}
}

// 表达式编译不过就整次拒绝，且一行都不留。
//
// 这是写入路径最要紧的一条：一条编译不过的规则进了库，表现是每个用到它的
// 请求都失败，而失败点在热路径上——排查的人会先去怀疑请求。
func TestCreateRejectsUncompilableExpr(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)

	cases := []struct {
		name   string
		source string
		reason expr.Reason
	}{
		{"语法错", `{"prompt": }`, expr.ReasonSyntax},
		{"未知名字", `{"prompt": nope}`, expr.ReasonSyntax},
		{"结果不是一张单价表", `1.5`, expr.ReasonSyntax},
		{"用量变量出现在价格位置", `{"prompt": p * 0.5}`, expr.ReasonShape},
		{"未知的键", `{"promt": 1.0}`, expr.ReasonShape},
		{"嵌套过深", strings.Repeat("(", 30) + `{"prompt": 1.0}` + strings.Repeat(")", 30), expr.ReasonScale},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name := unique("rule-badexpr")
			_, err := admin.Create(context.Background(), CreateRequest{
				Scope: ScopePlatform, Name: name, Expr: tc.source,
			})
			if err == nil {
				t.Fatal("编译不过的表达式应当被拒")
			}

			// 报错要能定位到出错的那一条规则：写规则的人手里只有它。
			var compileErr *expr.CompileError
			if !errors.As(err, &compileErr) {
				t.Fatalf("err = %v，期望 *expr.CompileError", err)
			}
			if compileErr.Reason != tc.reason {
				t.Errorf("Reason = %q，期望 %q（err: %v）", compileErr.Reason, tc.reason, err)
			}
			if n := countRuleByName(t, db, name); n != 0 {
				t.Errorf("被拒的请求留下了 %d 行", n)
			}
		})
	}
}

// 作用域与两栏 id 的组合：写了读取路径不看的那一栏就是一条不生效的配置，
// 所以整次拒绝，而不是悄悄存下来。
func TestCreateRejectsInvalidFields(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)
	owner := seedUser(t, db)
	ch := seedChannel(t, db, owner)
	from := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	before := from.Add(-time.Hour)
	same := from

	cases := []struct {
		name   string
		mutate func(*CreateRequest)
	}{
		{"作用域为空", func(r *CreateRequest) { r.Scope = "" }},
		{"作用域是别的字符串", func(r *CreateRequest) { r.Scope = "global" }},
		{"platform 挂了商家", func(r *CreateRequest) { r.OwnerUserID = owner }},
		{"platform 挂了渠道", func(r *CreateRequest) { r.ChannelID = ch }},
		{"merchant 没给商家", func(r *CreateRequest) { r.Scope = ScopeMerchant }},
		{"merchant 挂了渠道", func(r *CreateRequest) {
			r.Scope, r.OwnerUserID, r.ChannelID = ScopeMerchant, owner, ch
		}},
		{"channel 没给渠道", func(r *CreateRequest) { r.Scope = ScopeChannel }},
		{"channel 挂了商家", func(r *CreateRequest) {
			r.Scope, r.OwnerUserID, r.ChannelID = ScopeChannel, owner, ch
		}},
		{"规则名为空", func(r *CreateRequest) { r.Name = "   " }},
		{"表达式为空", func(r *CreateRequest) { r.Expr = "" }},
		{"model_patterns 里有空项", func(r *CreateRequest) {
			r.ModelPatterns = []string{"a", " "}
		}},
		{"model_patterns 里有通配符", func(r *CreateRequest) {
			r.ModelPatterns = []string{"claude-*"}
		}},
		{"上界早于下界", func(r *CreateRequest) {
			r.EffectiveFrom, r.EffectiveTo = from, &before
		}},
		{"上界等于下界", func(r *CreateRequest) {
			r.EffectiveFrom, r.EffectiveTo = from, &same
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := CreateRequest{
				Scope:         ScopePlatform,
				Name:          unique("rule-invalid"),
				ModelPatterns: []string{unique("model-invalid")},
				Expr:          `{"prompt": 0.00001}`,
			}
			tc.mutate(&req)

			_, err := admin.Create(context.Background(), req)
			requireValidationError(t, err)
			if n := countRuleByName(t, db, req.Name); n != 0 {
				t.Errorf("被拒的请求留下了 %d 行", n)
			}
		})
	}
}

// 名单里**第几项**不合法要报出来：名单可以很长，只说「有个模型名不合法」
// 等于让人自己一个个试。
//
// 变异：normalizePatterns 里两处 `i+1`（把下标换成从 1 起的序号）各自翻成
// `i-1`，位置就报错了。取「前两项合法、第三项不合法」的名单分辨：正确版说
// 第 3 个，变异版说第 1 个。两处各管一条分支，所以两条都要取到这个位置。
func TestCreateReportsWhichPatternIsInvalid(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)

	cases := []struct {
		name     string
		patterns []string
		want     string
	}{
		{"空项", []string{"a", "b", " "}, "第 3 个模型名为空"},
		{"通配符", []string{"a", "b", "c*"}, "第 3 个模型名含通配符"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := admin.Create(context.Background(), CreateRequest{
				Scope: ScopePlatform, Name: unique("rule-position"),
				ModelPatterns: tc.patterns, Expr: `{"prompt": 0.00001}`,
			})
			if err == nil {
				t.Fatal("不合法的模型名单应当被拒")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q，期望里面有 %q", err, tc.want)
			}
		})
	}
}

// 空 model_patterns 是「适用于全部模型」，是正常写法而不是漏填。
func TestCreateAllowsEmptyModelPatterns(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)
	model := unique("model-any")

	// priority 给一个非零值：测试库是共享的，库里可能留着一条「适用于全部模型」
	// 的平台规则（残留窗口的来源见 TestMatchReturnsErrNoRule 的注释），没有
	// 优先级就只剩 id 决胜，而 id 的大小不由本用例决定。
	id := createRule(t, admin, CreateRequest{
		Scope: ScopePlatform, Name: unique("rule-any"), ModelPatterns: []string{},
		Expr: `{"prompt": 1}`, Priority: 50,
	})

	info := findRule(t, listAll(t, admin), id)
	if len(info.ModelPatterns) != 0 {
		t.Errorf("ModelPatterns = %v，期望空数组", info.ModelPatterns)
	}
	// 空数组真的适用于任意模型名。
	got, err := New(db).Match(context.Background(),
		singleCandidate(model, 0, 0, time.Now().Add(time.Hour)))
	if err != nil {
		t.Fatalf("Match 出错: %v", err)
	}
	if got.ID != id {
		t.Errorf("选中的规则 id = %d，期望不限定模型的那条 %d", got.ID, id)
	}
}

// 平台作用域与渠道作用域的规则，库里那两栏存的是 NULL 而不是 0：
// 0 会去匹配一个不存在的行，外键直接拒绝。
func TestCreateStoresNullForUnsetIDs(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)
	owner := seedUser(t, db)
	ch := seedChannel(t, db, owner)

	platform := createRule(t, admin, CreateRequest{Scope: ScopePlatform, Name: unique("rule-null")})
	if got := rawRuleOwner(t, db, platform); got.Valid {
		t.Errorf("platform 规则的 owner_user_id = %d，期望 NULL", got.Int64)
	}
	channel := createRule(t, admin, CreateRequest{
		Scope: ScopeChannel, ChannelID: ch, Name: unique("rule-null"),
	})
	if got := rawRuleOwner(t, db, channel); got.Valid {
		t.Errorf("channel 规则的 owner_user_id = %d，期望 NULL", got.Int64)
	}
}

// 不写 Enabled 时落成启用：零值 false 会让「忘了写这个字段」变成一条静默
// 停用的规则。
func TestCreateEnabledDefaultsToTrue(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)

	id := createRule(t, admin, CreateRequest{Scope: ScopePlatform, Name: unique("rule-on")})
	if !findRule(t, listAll(t, admin), id).Enabled {
		t.Error("没写 Enabled 的规则应当是启用的")
	}

	off := false
	disabled := createRule(t, admin, CreateRequest{
		Scope: ScopePlatform, Name: unique("rule-off"), Enabled: &off,
	})
	if findRule(t, listAll(t, admin), disabled).Enabled {
		t.Error("Enabled 显式给 false 的规则不该是启用的")
	}
}

// 不写 EffectiveFrom 时从当前时刻起算，而不是公元 1 年——后者会让这条规则
// 立刻生效，而「立刻生效」与「从现在起生效」在账单上不是一回事。
func TestCreateDefaultsEffectiveFromToNow(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)
	model := unique("model-from")

	before := time.Now().Add(-time.Minute)
	id := createRule(t, admin, CreateRequest{
		Scope: ScopePlatform, Name: unique("rule-from"), ModelPatterns: []string{model},
	})

	info := findRule(t, listAll(t, admin), id)
	if info.EffectiveFrom.Before(before) {
		t.Errorf("EffectiveFrom = %s，期望在当前时刻附近（不早于 %s）",
			info.EffectiveFrom, before)
	}
	if info.EffectiveTo != nil {
		t.Errorf("EffectiveTo = %v，期望 nil（不过期）", info.EffectiveTo)
	}
}

// 外键约束（owner / channel 指向不存在的行）要原样报出来，不能被吞掉。
func TestCreateForeignKeyViolationSurfaces(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)

	cases := []struct {
		name string
		req  CreateRequest
	}{
		{"商家不存在", CreateRequest{Scope: ScopeMerchant, OwnerUserID: 1 << 40}},
		{"渠道不存在", CreateRequest{Scope: ScopeChannel, ChannelID: 1 << 40}},
	}

	for i := range cases {
		cases[i].req.Expr = `{"prompt": 0.00001}`
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.req
			req.Name = unique("rule-fk")
			_, err := admin.Create(context.Background(), req)
			if err == nil {
				t.Fatal("不存在的引用应当被外键拒掉")
			}
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) {
				t.Fatalf("err = %v，期望 *pgconn.PgError", err)
			}
			if pgErr.Code != "23503" {
				t.Errorf("SQLSTATE = %s（%s），期望 23503 foreign_key_violation",
					pgErr.Code, pgErr.Message)
			}
			if n := countRuleByName(t, db, req.Name); n != 0 {
				t.Errorf("被拒的请求留下了 %d 行", n)
			}
		})
	}
}

// ── 修改 ────────────────────────────────────────────────────────────────

// 每一个可改字段都要能改，并且改完从读取路径与列举路径看得到。
func TestUpdateEveryFieldReadsBack(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)
	owner := seedUser(t, db)
	ch := seedChannel(t, db, owner)
	oldModel := unique("model-old")
	newModel := unique("model-new")

	id := createRule(t, admin, CreateRequest{
		Scope: ScopeChannel, ChannelID: ch, Name: unique("rule-fields"),
		ModelPatterns: []string{oldModel}, Expr: `{"prompt": 0.00001}`,
		Priority: 1,
	})

	newName := unique("rule-renamed")
	newPatterns := []string{newModel}
	newExpr := `{"prompt": 0.00003, "completion": 0.00004}`
	newPriority := 0 // 零值是合法修改，必须与「没提到」区分开
	from := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	off := false

	err := admin.Update(context.Background(), UpdateRequest{
		ID: id,
		// 作用域从 channel 改成 merchant：两栏 id 一起给，改完之后那一行
		// 才是读取路径认得的组合。
		Scope:         ptr(ScopeMerchant),
		OwnerUserID:   ptr(owner),
		ChannelID:     ptr(int64(0)),
		Name:          &newName,
		ModelPatterns: &newPatterns,
		Expr:          &newExpr,
		Priority:      &newPriority,
		EffectiveFrom: &from,
		EffectiveTo:   &to,
		Enabled:       &off,
	})
	if err != nil {
		t.Fatalf("Update 出错: %v", err)
	}

	info := findRule(t, listAll(t, admin), id)
	if info.Scope != ScopeMerchant || info.OwnerUserID != owner || info.ChannelID != 0 {
		t.Errorf("作用域/id = %s/%d/%d，期望 %s/%d/0",
			info.Scope, info.OwnerUserID, info.ChannelID, ScopeMerchant, owner)
	}
	if info.Name != newName || info.Priority != newPriority || info.Enabled != off {
		t.Errorf("Name/Priority/Enabled = %q/%d/%v，期望 %q/%d/%v",
			info.Name, info.Priority, info.Enabled, newName, newPriority, off)
	}
	if !reflect.DeepEqual(info.ModelPatterns, newPatterns) {
		t.Errorf("ModelPatterns = %v，期望 %v", info.ModelPatterns, newPatterns)
	}
	if info.Expr != newExpr {
		t.Errorf("Expr = %q，期望 %q", info.Expr, newExpr)
	}
	if !info.EffectiveFrom.Equal(from) {
		t.Errorf("EffectiveFrom = %s，期望 %s", info.EffectiveFrom, from)
	}
	if info.EffectiveTo == nil || !info.EffectiveTo.Equal(to) {
		t.Errorf("EffectiveTo = %v，期望 %s", info.EffectiveTo, to)
	}

	// 模型名单是整列替换：旧模型名不该再命中。
	if _, err = New(db).Match(context.Background(),
		singleCandidate(oldModel, 0, owner, from.Add(time.Hour))); !errors.Is(err, pipeline.ErrNoRule) {
		t.Errorf("按旧模型名匹配的 err = %v，期望 ErrNoRule（model_patterns 要整列替换）", err)
	}
}

// 表达式改了之后，读取路径拿到的是新的那一份。
func TestUpdateExprReadsBack(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)
	ctx := context.Background()
	model := unique("model-update-expr")

	before := `{"prompt": 0.00001}`
	after := `{"prompt": 0.00009}`
	id := createRule(t, admin, CreateRequest{
		Scope: ScopePlatform, Name: unique("rule-expr"), ModelPatterns: []string{model},
		Expr: before, Priority: 1,
	})

	// 时刻在规则之后取：规则默认的生效时刻是创建那一刻，先取时刻的话
	// 这条规则在那一刻还没生效。
	at := time.Now()

	got, err := New(db).Match(ctx, singleCandidate(model, 0, 0, at))
	if err != nil {
		t.Fatalf("Match 出错: %v", err)
	}
	if got.Source != before {
		t.Fatalf("改动前的源码 = %q，期望 %q", got.Source, before)
	}

	if err = admin.Update(ctx, UpdateRequest{ID: id, Expr: &after}); err != nil {
		t.Fatalf("Update 出错: %v", err)
	}

	got, err = New(db).Match(ctx, singleCandidate(model, 0, 0, at))
	if err != nil {
		t.Fatalf("Match 出错: %v", err)
	}
	if got.Source != after {
		t.Errorf("改动后的源码 = %q，期望 %q", got.Source, after)
	}
	if got.Version == 0 {
		t.Error("Version 应当由源码算出，不该是 0")
	}
}

// 版本历史：建规则留第一版，改表达式再留一版。
//
// 钉的是**每一版的表达式本身**，不是「有几行」：只数行数的话，把每一版都写成
// 同一个（例如都写当前值）也能过，而这张表的全部用途就是事后回答
// 「这个价是什么时候被改成这样的」——写成同一个值等于没记。
func TestRuleVersionsRecordExpressionHistory(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)
	ctx := context.Background()

	before := `{"prompt": 0.00001}`
	after := `{"prompt": 0.00007}`
	id := createRule(t, admin, CreateRequest{
		Scope: ScopePlatform, Name: unique("rule-version"), Expr: before,
	})

	if got := ruleVersions(t, db, id); !reflect.DeepEqual(got, []string{before}) {
		t.Fatalf("建规则后的版本历史 = %v，期望只有第一版 %q", got, before)
	}

	if err := admin.Update(ctx, UpdateRequest{ID: id, Expr: &after}); err != nil {
		t.Fatalf("Update 出错: %v", err)
	}
	if got := ruleVersions(t, db, id); !reflect.DeepEqual(got, []string{before, after}) {
		t.Errorf("改表达式后的版本历史 = %v，期望 [%q %q]", got, before, after)
	}
}

// 表达式没变就不留版本。
//
// 这张表记的是**成交价的依据**：改名字、改优先级、改生效时间都不改变任何
// 一笔已经算出来的钱。为它们插行会让版本历史里塞满「其实没变」的噪声，
// 而它的读者正是要在差额出现时一眼看出「价是什么时候被改的」。
//
// 传一个与现值**相同**的表达式同样不算一次改价——判据是「值变没变」，
// 不是「这次有没有传这个字段」。
func TestRuleVersionsSkipUnchangedExpression(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)
	ctx := context.Background()

	expr := `{"prompt": 0.00003}`
	id := createRule(t, admin, CreateRequest{
		Scope: ScopePlatform, Name: unique("rule-version-skip"), Expr: expr,
	})

	name := unique("rule-version-renamed")
	if err := admin.Update(ctx, UpdateRequest{ID: id, Name: &name}); err != nil {
		t.Fatalf("Update 出错: %v", err)
	}
	same := expr
	if err := admin.Update(ctx, UpdateRequest{ID: id, Expr: &same}); err != nil {
		t.Fatalf("Update 出错: %v", err)
	}

	if got := ruleVersions(t, db, id); !reflect.DeepEqual(got, []string{expr}) {
		t.Errorf("版本历史 = %v，期望仍只有第一版 %q", got, expr)
	}
}

// 编译不过的表达式在修改路径上同样被拒，且**库里那一行不动**。
func TestUpdateRejectsUncompilableExpr(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)
	model := unique("model-badupdate")

	good := `{"prompt": 0.00001}`
	id := createRule(t, admin, CreateRequest{
		Scope: ScopePlatform, Name: unique("rule-badupdate"), ModelPatterns: []string{model},
		Expr: good, Priority: 1,
	})
	at := time.Now()

	bad := `{"prompt": nope}`
	err := admin.Update(context.Background(), UpdateRequest{ID: id, Expr: &bad})
	if err == nil {
		t.Fatal("编译不过的表达式应当被拒")
	}
	var compileErr *expr.CompileError
	if !errors.As(err, &compileErr) {
		t.Fatalf("err = %v，期望 *expr.CompileError", err)
	}

	info := findRule(t, listAll(t, admin), id)
	if info.Expr != good {
		t.Errorf("被拒的修改还是改了库：Expr = %q，期望 %q", info.Expr, good)
	}
	got, err := New(db).Match(context.Background(), singleCandidate(model, 0, 0, at))
	if err != nil {
		t.Fatalf("Match 出错: %v", err)
	}
	if got.Source != good {
		t.Errorf("读取路径拿到的源码 = %q，期望 %q", got.Source, good)
	}
}

// 修改路径上的字段校验与创建路径是同一套：改出来的名字与模型名单同样要过。
func TestUpdateRejectsInvalidFields(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)
	ctx := context.Background()

	id := createRule(t, admin, CreateRequest{Scope: ScopePlatform, Name: unique("rule-upd-invalid")})

	cases := []struct {
		name   string
		mutate func(*UpdateRequest)
	}{
		{"名字改成空白", func(r *UpdateRequest) { r.Name = ptr("   ") }},
		{"模型名单里有空项", func(r *UpdateRequest) { r.ModelPatterns = ptr([]string{"a", " "}) }},
		{"模型名单里有通配符", func(r *UpdateRequest) { r.ModelPatterns = ptr([]string{"claude-*"}) }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := UpdateRequest{ID: id}
			tc.mutate(&req)
			requireValidationError(t, admin.Update(ctx, req))
		})
	}

	// 被拒的修改一个字段都没动。
	info := findRule(t, listAll(t, admin), id)
	if len(info.ModelPatterns) != 0 {
		t.Errorf("被拒的修改还是改了库：ModelPatterns = %v", info.ModelPatterns)
	}
	if info.Name == "" {
		t.Error("被拒的修改把名字清掉了")
	}
}

// 名字为空的规则改不动：它不能被指认，而校验的对象是改完之后的那一行。
//
// 库里那一行是绕过写入路径写进去的（写入路径不给空名字），要改它就先把
// 名字补上——同一次调用里给就行。
func TestUpdateRequiresNameOnUnnamedRow(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)
	ctx := context.Background()

	// name 是 NOT NULL，但空字符串在数据库那一侧是合法的。
	var id int64
	if err := db.QueryRowContext(ctx, `
		INSERT INTO pricing_rules (scope, name, expr)
		VALUES ('platform', '', '{"prompt": 1}') RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("造规则失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM pricing_rules WHERE id = $1`, id)
	})

	off := false
	err := admin.Update(ctx, UpdateRequest{ID: id, Enabled: &off})
	requireValidationError(t, err)

	name := unique("rule-named")
	if err = admin.Update(ctx, UpdateRequest{ID: id, Name: &name, Enabled: &off}); err != nil {
		t.Fatalf("补上名字之后应当能改: %v", err)
	}
	info := findRule(t, listAll(t, admin), id)
	if info.Name != name || info.Enabled {
		t.Errorf("Name/Enabled = %q/%v，期望 %q/false", info.Name, info.Enabled, name)
	}
}

// 校验的对象是**改完之后的那一行**：只改作用域而不动两栏 id，改出来的组合
// 读取路径是不认的。
func TestUpdateValidatesMergedRow(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)
	ctx := context.Background()
	owner := seedUser(t, db)
	ch := seedChannel(t, db, owner)

	id := createRule(t, admin, CreateRequest{
		Scope: ScopeChannel, ChannelID: ch, Name: unique("rule-merged"),
	})

	// 只把作用域改成 platform：owner 是 NULL 但 channel_id 还挂着，而
	// platform 的读取判据不看 channel_id。
	err := admin.Update(ctx, UpdateRequest{ID: id, Scope: ptr(ScopePlatform)})
	requireValidationError(t, err)

	// 只把作用域改成 merchant：channel_id 还挂着，owner 还是空的。
	err = admin.Update(ctx, UpdateRequest{ID: id, Scope: ptr(ScopeMerchant)})
	requireValidationError(t, err)

	info := findRule(t, listAll(t, admin), id)
	if info.Scope != ScopeChannel || info.ChannelID != ch {
		t.Errorf("被拒的修改还是改了库：Scope/ChannelID = %s/%d", info.Scope, info.ChannelID)
	}

	// 两栏 id 一起给，改完之后那一行才是合法的。
	if err = admin.Update(ctx, UpdateRequest{
		ID: id, Scope: ptr(ScopeMerchant), OwnerUserID: ptr(owner), ChannelID: ptr(int64(0)),
	}); err != nil {
		t.Fatalf("合法的修改被拒了: %v", err)
	}
	info = findRule(t, listAll(t, admin), id)
	if info.Scope != ScopeMerchant || info.OwnerUserID != owner || info.ChannelID != 0 {
		t.Errorf("改完之后 Scope/Owner/Channel = %s/%d/%d，期望 %s/%d/0",
			info.Scope, info.OwnerUserID, info.ChannelID, ScopeMerchant, owner)
	}
}

// 时间窗口是左闭右开，而修改可能只动一端——所以判据要用**库里现在那一端**
// 与这次给的那一端合起来看。
//
// 这一条钉的是读-改-写：只按请求里给的字段校验的话，「把上界改到下界之前」
// 会顺利通过，而那条规则从此一条都匹配不上。
func TestUpdateValidatesWindowAgainstStoredBound(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)
	ctx := context.Background()

	from := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	id := createRule(t, admin, CreateRequest{
		Scope: ScopePlatform, Name: unique("rule-window"),
		EffectiveFrom: from, EffectiveTo: &to,
	})

	// 上界改成与下界同一个时刻：区间为空，这条规则一条都匹配不上。
	same := from
	err := admin.Update(ctx, UpdateRequest{ID: id, EffectiveTo: &same})
	requireValidationError(t, err)

	// 上界改到下界之前：同样是空区间。
	before := from.Add(-time.Hour)
	err = admin.Update(ctx, UpdateRequest{ID: id, EffectiveTo: &before})
	requireValidationError(t, err)

	info := findRule(t, listAll(t, admin), id)
	if info.EffectiveTo == nil || !info.EffectiveTo.Equal(to) {
		t.Errorf("被拒的修改还是改了库：EffectiveTo = %v，期望 %s", info.EffectiveTo, to)
	}

	// 反方向：下界推到上界之后。
	late := to.Add(time.Hour)
	err = admin.Update(ctx, UpdateRequest{ID: id, EffectiveFrom: &late})
	requireValidationError(t, err)

	// 只动一端而仍然合法：上界往后挪。
	later := to.Add(24 * time.Hour)
	if err = admin.Update(ctx, UpdateRequest{ID: id, EffectiveTo: &later}); err != nil {
		t.Fatalf("合法的修改被拒了: %v", err)
	}
	info = findRule(t, listAll(t, admin), id)
	if info.EffectiveTo == nil || !info.EffectiveTo.Equal(later) {
		t.Errorf("EffectiveTo = %v，期望 %s", info.EffectiveTo, later)
	}
	if !info.EffectiveFrom.Equal(from) {
		t.Errorf("下界被改动了：EffectiveFrom = %s，期望 %s", info.EffectiveFrom, from)
	}
}

// 上界可以清成「不过期」：指针表达不了「置空」，所以单独一个字段。
func TestUpdateClearsEffectiveTo(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)
	ctx := context.Background()
	model := unique("model-clear")

	// 时刻相对当前取，不写死日期：库里别的平台规则（见 TestMatchReturnsErrNoRule
	// 的注释）从各自创建那一刻起生效，写死的日期会随墙上时钟漂到它们的窗口里。
	from := time.Now().Add(-time.Hour)
	to := time.Now().Add(time.Hour)
	outside := to.Add(time.Hour)

	// priority 给得比别的规则高：这条规则只要在窗口内就一定胜出，于是
	// 「窗口外选中的不是它」是一个精确的判据，而不是「恰好没有别的规则」。
	id := createRule(t, admin, CreateRequest{
		Scope: ScopePlatform, Name: unique("rule-clear"), ModelPatterns: []string{model},
		EffectiveFrom: from, EffectiveTo: &to, Priority: 99,
	})

	if got, err := New(db).Match(ctx, singleCandidate(model, 0, 0, outside)); err == nil && got.ID == id {
		t.Fatalf("窗口外的规则被选中了（窗口 %s ~ %s，查询时刻 %s）", from, to, outside)
	}

	if err := admin.Update(ctx, UpdateRequest{ID: id, ClearEffectiveTo: true}); err != nil {
		t.Fatalf("Update 出错: %v", err)
	}
	if got := findRule(t, listAll(t, admin), id); got.EffectiveTo != nil {
		t.Errorf("EffectiveTo = %v，期望 nil", got.EffectiveTo)
	}

	// 清掉之后，原来窗口外的时刻就轮到它了。
	got, err := New(db).Match(ctx, singleCandidate(model, 0, 0, outside))
	if err != nil {
		t.Fatalf("清掉上界之后应当能匹配上: %v", err)
	}
	if got.ID != id {
		t.Errorf("规则 id = %d，期望 %d", got.ID, id)
	}
}

// 「改成某个时刻」与「清成不过期」是两件事，同时给是调用方的错。
func TestUpdateRejectsConflictingEffectiveTo(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)
	id := createRule(t, admin, CreateRequest{Scope: ScopePlatform, Name: unique("rule-conflict")})

	to := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	err := admin.Update(context.Background(), UpdateRequest{
		ID: id, EffectiveTo: &to, ClearEffectiveTo: true,
	})
	requireValidationError(t, err)
}

// 零值时刻是「公元 1 年」，没人想要它，多半是调用方少赋了一次。
func TestUpdateRejectsZeroEffectiveFrom(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)
	id := createRule(t, admin, CreateRequest{Scope: ScopePlatform, Name: unique("rule-zero")})

	err := admin.Update(context.Background(), UpdateRequest{ID: id, EffectiveFrom: &time.Time{}})
	requireValidationError(t, err)
}

// 一个字段都没提到时直接报错：发一条只动 updated_at 的 UPDATE 会让
// 「结构体拼错了」看起来像一次成功的修改。
func TestUpdateWithoutChanges(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)

	if err := admin.Update(context.Background(), UpdateRequest{ID: 1}); !errors.Is(err, ErrNoChanges) {
		t.Errorf("err = %v，期望 ErrNoChanges", err)
	}
}

// 只给一个字段也算一次「要改的请求」——哪怕这一次改不成，报的也该是改不成
// 的那条错，而不是「没有要改的字段」。
//
// 变异：changesAnything 是一条十个操作数的 `||` 链，其中两处各自翻成 `&&`
// （INVERT_LOGICAL 默认是关的，本仓在 scripts/check-mutation.sh 里显式打开）。
// Go 里 `&&` 比 `||` 紧，于是链子变成 `(a && b) || c || …` 或
// `(a || b) && c || …`：前者在「只给了 a」或「只给了 b」时为假，后者在
// 「给了 a 或 b 而没给 c」时为假，两版都报 ErrNoChanges——而正确版会往下走，
// 走到真正该报的那条错、或者真的把这一行改掉。
//
// 两个子用例各取一格：
//   - 只给 Scope：正确版走到校验，报的是「platform 作用域不挂商家」；
//   - 只给 OwnerUserID：正确版真的改掉归属。
//
// 一条「字段提到了没提到」的判据，错的方向都是**把一次真实的改动说成没改**。
func TestUpdateSingleFieldCountsAsAChange(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)
	ctx := context.Background()

	// 只给 Scope：channel 规则改成 platform，channel_id 还挂着，校验要拦下它。
	// 拦下它的**不是** ErrNoChanges——这里给了一个字段，只是这个改动本身不成立。
	chRule := createRule(t, admin, CreateRequest{
		Scope: ScopeChannel, ChannelID: seedChannel(t, db, seedUser(t, db)),
		Name: unique("rule-one-field"),
	})
	err := admin.Update(ctx, UpdateRequest{ID: chRule, Scope: ptr(ScopePlatform)})
	if err == nil {
		t.Fatal("只改作用域而两栏 id 不动，改出来的组合读取路径不认，应当被拒")
	}
	if errors.Is(err, ErrNoChanges) {
		t.Errorf("err = %v，被当成了「没有要改的字段」——这次给的是作用域", err)
	}

	// 只给 OwnerUserID：merchant 规则换个归属，改完之后那一行仍然合法。
	ownerA := seedUser(t, db)
	ownerB := seedUser(t, db)
	merchantRule := createRule(t, admin, CreateRequest{
		Scope: ScopeMerchant, OwnerUserID: ownerA, Name: unique("rule-one-field"),
	})
	if err = admin.Update(ctx, UpdateRequest{ID: merchantRule, OwnerUserID: ptr(ownerB)}); err != nil {
		t.Fatalf("只改归属应当能改: %v", err)
	}
	if info := findRule(t, listAll(t, admin), merchantRule); info.OwnerUserID != ownerB {
		t.Errorf("OwnerUserID = %d，期望改后的 %d", info.OwnerUserID, ownerB)
	}
}

// 改一个不存在的 id 要报 ErrRuleNotFound，不能静默成功。
func TestUpdateUnknownRule(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)

	name := unique("rule-nowhere")
	err := admin.Update(context.Background(), UpdateRequest{ID: 1 << 40, Name: &name})
	if !errors.Is(err, ErrRuleNotFound) {
		t.Errorf("err = %v，期望 ErrRuleNotFound", err)
	}
	if n := countRuleByName(t, db, name); n != 0 {
		t.Errorf("改不存在的规则留下了 %d 行", n)
	}

	// id 为 0 或负数在**查库之前**就被拒：那是调用方没填 id，与「这条规则
	// 不在库里」是两回事——后者要去查那条规则是不是被删了，前者要去看调用方。
	//
	// 变异：`if req.ID <= 0` 的边界翻成 `< 0`（CONDITIONALS_BOUNDARY），
	// 0 于是漏过这一层落进 loadRuleForUpdate，报的变成 ErrRuleNotFound。
	// 区分两者的输入就是 id = 0（负数那一格同上）这一次调用。
	for _, id := range []int64{0, -1} {
		err = admin.Update(context.Background(), UpdateRequest{ID: id, Name: &name})
		if err == nil {
			t.Fatalf("id = %d 应当被拒", id)
		}
		if errors.Is(err, ErrRuleNotFound) {
			t.Errorf("id = %d 的 err = %v，被报成了「规则不存在」——"+
				"这两个诊断指向的地方不一样", id, err)
		}
	}
}

// 库里那条本来就编译不过的行（绕过写入路径写进去的）仍然要能被停用。
//
// 出故障时最需要的动作就是「关掉它」，而它不该因为另一个字段坏着而失败。
// 代价是「结构上的非法组合」不受这条豁免——那只会让规则多一栏被忽略或者
// 一条都匹配不上，都不会让它在线上算错钱。
func TestUpdateCanDisableRuleWithBrokenExpr(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)
	ctx := context.Background()
	model := unique("model-broken")

	// 直接写进库，绕过写入路径的编译检查。
	id := insertRule(t, ctx, db, ruleSpec{patterns: []string{model}, expr: `{"prompt": nope}`, priority: 99})

	// 读取路径**不编译**，所以这条规则照样会被选中——编译发生在求值那一步。
	// 这正是它危险的地方：故障出现在热路径上，而不是在写入的时候。
	got, err := New(db).Match(ctx, singleCandidate(model, 0, 0, time.Now()))
	if err != nil {
		t.Fatalf("Match 出错: %v", err)
	}
	if got.ID != id {
		t.Fatalf("选中的规则是 %d，期望 %d", got.ID, id)
	}

	off := false
	if err = admin.Update(ctx, UpdateRequest{ID: id, Enabled: &off}); err != nil {
		t.Fatalf("停用一条表达式坏掉的规则不该失败: %v", err)
	}
	info := findRule(t, listAll(t, admin), id)
	if info.Enabled {
		t.Error("停用没生效")
	}
	if info.ExprErr == nil {
		t.Error("列举应当指出这条规则的表达式编译不过")
	}

	// 停用之后读取路径不再选它。
	after, err := New(db).Match(ctx, singleCandidate(model, 0, 0, time.Now()))
	if err == nil && after.ID == id {
		t.Error("停用之后仍然选中了它")
	}

	// 但把它的表达式改成另一份坏的，照样要被拒。
	bad := `{"prompt": }`
	if err := admin.Update(ctx, UpdateRequest{ID: id, Expr: &bad}); err == nil {
		t.Error("写入路径不该放行编译不过的表达式")
	}
}

// ── 列举 ────────────────────────────────────────────────────────────────

// 列举的次序就是裁决次序：作用域具体程度 → priority → 具体 > 兜底 → id。
//
// 于是这个清单有一个可以直接读出来的性质：适用于某次请求的第一条，就是
// Match 会选中的那条。
func TestListIsOrderedByAdjudication(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)
	owner := seedUser(t, db)
	ch := seedChannel(t, db, owner)
	model := unique("model-order")

	platform := createRule(t, admin, CreateRequest{
		Scope: ScopePlatform, Name: unique("rule-order"), ModelPatterns: []string{model},
		Expr: `{"prompt": 1}`, Priority: 9,
	})
	merchant := createRule(t, admin, CreateRequest{
		Scope: ScopeMerchant, OwnerUserID: owner, Name: unique("rule-order"),
		ModelPatterns: []string{model}, Expr: `{"prompt": 2}`, Priority: 1,
	})
	channel := createRule(t, admin, CreateRequest{
		Scope: ScopeChannel, ChannelID: ch, Name: unique("rule-order"),
		ModelPatterns: []string{model}, Expr: `{"prompt": 3}`, Priority: 1,
	})

	infos := listAll(t, admin)
	// 作用域压过 priority：平台那条的 priority 是 9，仍然排在最后。
	if rankOf(t, infos, channel) > rankOf(t, infos, merchant) {
		t.Error("channel 作用域应当排在 merchant 之前")
	}
	if rankOf(t, infos, merchant) > rankOf(t, infos, platform) {
		t.Error("merchant 作用域应当排在 platform 之前")
	}

	// 同一作用域内按 priority 决胜。
	high := createRule(t, admin, CreateRequest{
		Scope: ScopeChannel, ChannelID: ch, Name: unique("rule-order"),
		ModelPatterns: []string{model}, Expr: `{"prompt": 4}`, Priority: 5,
	})
	infos = listAll(t, admin)
	if rankOf(t, infos, high) > rankOf(t, infos, channel) {
		t.Error("同作用域下 priority 大的应当排在前面")
	}

	// 同作用域同优先级时按 id 取最新——这一条不是偏好，是确定性。
	newer := createRule(t, admin, CreateRequest{
		Scope: ScopeChannel, ChannelID: ch, Name: unique("rule-order"),
		ModelPatterns: []string{model}, Expr: `{"prompt": 5}`, Priority: 5,
	})
	infos = listAll(t, admin)
	if rankOf(t, infos, newer) > rankOf(t, infos, high) {
		t.Error("同作用域同优先级下 id 大的应当排在前面")
	}

	// 同作用域同优先级时，模型名单非空的排在兜底的前面——与 id 的大小无关：
	// 兜底那条是后建的，序号更大。
	catchAll := createRule(t, admin, CreateRequest{
		Scope: ScopeChannel, ChannelID: ch, Name: unique("rule-order"),
		ModelPatterns: []string{}, Expr: `{"prompt": 6}`, Priority: 5,
	})
	infos = listAll(t, admin)
	if rankOf(t, infos, catchAll) < rankOf(t, infos, newer) {
		t.Error("同作用域同优先级下兜底的规则应当排在模型专属的后面")
	}
}

// 列举不藏着一行坏规则，也不让一行坏规则遮住别的行。
func TestListReportsBrokenExprPerRow(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)

	// 直接写进库，绕过写入路径。
	broken := insertRule(t, t.Context(), db, ruleSpec{expr: `{"prompt": nope}`})
	good := createRule(t, admin, CreateRequest{Scope: ScopePlatform, Name: unique("rule-good")})

	infos := listAll(t, admin)

	badRow := findRule(t, infos, broken)
	if badRow.ExprErr == nil {
		t.Error("编译不过的那一行没有在 ExprErr 上说明")
	}
	if len(badRow.DeclaredKeys) != 0 {
		t.Errorf("编译不过的那一行给出了键清单：%v", badRow.DeclaredKeys)
	}

	okRow := findRule(t, infos, good)
	if okRow.ExprErr != nil {
		t.Errorf("能编译的那一行被报了错: %v", okRow.ExprErr)
	}
	if !reflect.DeepEqual(okRow.DeclaredKeys, []string{"prompt"}) {
		t.Errorf("DeclaredKeys = %v，期望 [prompt]", okRow.DeclaredKeys)
	}
}

// 列举顺带说出这条规则收了哪几项的钱。
//
// 单价漏写的后果是按 0 元计，而结算侧的判据要到结算时才拦——列举是唯一
// 能在写完之后、出账之前看见它的地方。
func TestListDeclaresPriceKeys(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)

	full := createRule(t, admin, CreateRequest{
		Scope: ScopePlatform, Name: unique("rule-keys"),
		Expr: `{"prompt": 1.5, "completion": 7.5, "units": {"search": 0.01}}`,
	})
	unitsOnly := createRule(t, admin, CreateRequest{
		Scope: ScopePlatform, Name: unique("rule-keys"),
		Expr: `{"units": {"search": 0.01}}`,
	})

	infos := listAll(t, admin)
	want := []string{"completion", "prompt", "units"}
	if got := findRule(t, infos, full).DeclaredKeys; !reflect.DeepEqual(got, want) {
		t.Errorf("DeclaredKeys = %v，期望 %v", got, want)
	}
	// 只写计量项、一个 token 单价都没写：这正是要在列举里看得见的那种规则。
	if got := findRule(t, infos, unitsOnly).DeclaredKeys; !reflect.DeepEqual(got, []string{"units"}) {
		t.Errorf("DeclaredKeys = %v，期望 [units]", got)
	}
}

// 管理面返回的字段集合是固定的。
//
// 改这里之前先问「这个新字段该不该出现在管理面的返回值里」——这条用例就是
// 为了让那个问题被问一次。
func TestRuleInfoFieldsArePinned(t *testing.T) {
	want := []string{
		"ID", "Scope", "OwnerUserID", "ChannelID", "Name", "ModelPatterns",
		"Expr", "Schedule", "Priority", "EffectiveFrom", "EffectiveTo", "Enabled",
		"CreatedBy", "CreatedAt", "UpdatedAt", "DeclaredKeys", "ExprErr",
	}

	typ := reflect.TypeOf(RuleInfo{})
	names := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		names = append(names, typ.Field(i).Name)
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("RuleInfo 的字段 = %v，期望 %v", names, want)
	}
}

// ── 裁决说明 ────────────────────────────────────────────────────────────

// Explain 的判据与次序必须与 Match 逐字相同。
//
// 这一条不读代码、只比对两个入口的答案：把同一个场景同时交给它们，答案必须
// 是同一条规则。两处的 SQL 片段分开写的话，这个用例会先挂。
func TestExplainAgreesWithMatch(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)
	src := New(db)
	ctx := context.Background()

	owner := seedUser(t, db)
	otherOwner := seedUser(t, db)
	ch := seedChannel(t, db, owner)
	model := unique("model-explain")

	platform := createRule(t, admin, CreateRequest{
		Scope: ScopePlatform, Name: unique("rule-explain"), ModelPatterns: []string{model},
		Expr: `{"prompt": 1}`, Priority: 9,
	})
	merchant := createRule(t, admin, CreateRequest{
		Scope: ScopeMerchant, OwnerUserID: owner, Name: unique("rule-explain"),
		ModelPatterns: []string{model}, Expr: `{"prompt": 2}`, Priority: 9,
	})
	channel := createRule(t, admin, CreateRequest{
		Scope: ScopeChannel, ChannelID: ch, Name: unique("rule-explain"),
		ModelPatterns: []string{model}, Expr: `{"prompt": 3}`, Priority: 1,
	})
	disabled := false
	disabledRule := createRule(t, admin, CreateRequest{
		Scope: ScopeChannel, ChannelID: ch, Name: unique("rule-explain"),
		ModelPatterns: []string{model}, Expr: `{"prompt": 4}`, Priority: 99,
		Enabled: &disabled,
	})
	future := time.Now().Add(24 * time.Hour)
	createRule(t, admin, CreateRequest{
		Scope: ScopeChannel, ChannelID: ch, Name: unique("rule-explain"),
		ModelPatterns: []string{model}, Expr: `{"prompt": 5}`, Priority: 99,
		EffectiveFrom: future,
	})

	// 时刻在规则之后取：规则默认的生效时刻是创建那一刻。
	at := time.Now()

	cases := []struct {
		name    string
		channel int64
		owner   int64
		want    int64
	}{
		{"渠道与商家都在场，选渠道规则", ch, owner, channel},
		{"只有商家在场，选商家规则", 0, owner, merchant},
		{"别的商家与别的渠道，退到平台规则", ch + 1, otherOwner, platform},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			matched, err := src.Match(ctx, singleCandidate(model, tc.channel, tc.owner, at))
			if err != nil {
				t.Fatalf("Match 出错: %v", err)
			}
			if matched.ID != tc.want {
				t.Errorf("Match 选中的是 %d，期望 %d", matched.ID, tc.want)
			}

			rules, err := admin.Explain(ctx, ExplainQuery{
				CanonicalModel: model, OwnerUserID: tc.owner, ChannelID: tc.channel, At: at,
			})
			if err != nil {
				t.Fatalf("Explain 出错: %v", err)
			}
			if rules[0].ID != matched.ID {
				t.Errorf("Explain 的第一条是 %d，Match 选中的是 %d——两处的裁决次序不一致",
					rules[0].ID, matched.ID)
			}
			if rules[0].ID != tc.want {
				t.Errorf("Explain 的第一条是 %d，期望 %d", rules[0].ID, tc.want)
			}

			// 停用的那条与还没生效的那条不在候选里：它们的 priority 是 99，
			// 一旦被放进来就会排在最前面。
			for _, r := range rules {
				if r.ID == disabledRule {
					t.Error("停用的规则出现在了适用清单里")
				}
				if !r.Enabled {
					t.Errorf("规则 %d 未启用却出现在了适用清单里", r.ID)
				}
			}
		})
	}
}

// Explain 的清单按裁决次序排列，于是排在胜出那条后面的每一条都在回答
// 「为什么不是它」。
func TestExplainOrdersLosersAfterWinner(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)
	owner := seedUser(t, db)
	ch := seedChannel(t, db, owner)
	model := unique("model-explain-order")

	platform := createRule(t, admin, CreateRequest{
		Scope: ScopePlatform, Name: unique("rule-explain-order"), ModelPatterns: []string{model},
		Expr: `{"prompt": 1}`, Priority: 5,
	})
	merchant := createRule(t, admin, CreateRequest{
		Scope: ScopeMerchant, OwnerUserID: owner, Name: unique("rule-explain-order"),
		ModelPatterns: []string{model}, Expr: `{"prompt": 2}`, Priority: 5,
	})
	channel := createRule(t, admin, CreateRequest{
		Scope: ScopeChannel, ChannelID: ch, Name: unique("rule-explain-order"),
		ModelPatterns: []string{model}, Expr: `{"prompt": 3}`, Priority: 5,
	})

	at := time.Now()
	rules, err := admin.Explain(context.Background(), ExplainQuery{
		CanonicalModel: model, OwnerUserID: owner, ChannelID: ch, At: at,
	})
	if err != nil {
		t.Fatalf("Explain 出错: %v", err)
	}

	if rules[0].ID != channel {
		t.Errorf("第一条是 %d，期望渠道规则 %d", rules[0].ID, channel)
	}
	if rankOf(t, rules, merchant) > rankOf(t, rules, platform) {
		t.Error("商家规则应当排在平台规则之前")
	}
}

// 一条都不适用时报 pipeline.ErrNoRule，与 Match 同一个错误。
func TestExplainWithoutApplicableRule(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)

	// 1970 早于库里任何一条规则的 effective_from（见 TestMatchReturnsErrNoRule）。
	_, err := admin.Explain(context.Background(), ExplainQuery{
		CanonicalModel: unique("model-nothing-explain"),
		At:             time.Unix(0, 0).UTC(),
	})
	if !errors.Is(err, pipeline.ErrNoRule) {
		t.Fatalf("err = %v，期望 ErrNoRule", err)
	}

	if _, err = admin.Explain(context.Background(), ExplainQuery{}); err == nil {
		t.Error("模型名为空应当被拒")
	}
}

// ptr 取一个值的地址，供 UpdateRequest 的指针字段用。
func ptr[T any](v T) *T { return &v }

// TestOwnedRuleScope 钉住商家规则的读改边界：只看得到、只改得动自己的
// merchant 规则；平台规则（owner 为 NULL）与别人的规则越权一律 ErrRuleNotFound。
//
// 列表这一条尤其重要：漏掉 owner 条件不是「改错一行」，而是把全平台的价
// 摊给每一个商家看。
func TestOwnedRuleScope(t *testing.T) {
	db := testDB(t)
	admin := NewAdmin(db)
	owner := seedUser(t, db)
	other := seedUser(t, db)
	model := unique("owned-model")

	ownID := createRule(t, admin, CreateRequest{
		Scope: ScopeMerchant, OwnerUserID: owner, ModelPatterns: []string{model},
		Expr: `{"prompt": 0.1}`,
	})
	otherID := createRule(t, admin, CreateRequest{
		Scope: ScopeMerchant, OwnerUserID: other, ModelPatterns: []string{model},
		Expr: `{"prompt": 0.2}`,
	})
	platformID := createRule(t, admin, CreateRequest{
		Scope: ScopePlatform, ModelPatterns: []string{model},
		Expr: `{"prompt": 0.3}`,
	})

	// 库里允许「channel 作用域 + owner_user_id」这种行：唯一的约束只管
	// platform 不得挂 owner，channel 与 merchant 两栏怎么写它不管。所以这里
	// 直接写库造一条，用来钉住商家清单靠的是 scope = 'merchant'，而不是只按
	// owner 过滤——只按 owner 过滤时这一条会漏进商家自己的清单。
	channelScopedID := insertChannelScopedRule(t, db, owner, seedChannel(t, db, owner))

	rules, err := admin.ListOwned(context.Background(), owner)
	if err != nil {
		t.Fatalf("ListOwned 出错: %v", err)
	}
	seen := make(map[int64]bool, len(rules))
	for _, info := range rules {
		seen[info.ID] = true
		if info.Scope != ScopeMerchant {
			t.Errorf("商家清单里出现了作用域 %q 的规则 %d", info.Scope, info.ID)
		}
	}
	if !seen[ownID] {
		t.Errorf("自己的规则 %d 不在清单里", ownID)
	}
	if seen[otherID] {
		t.Errorf("清单里出现了别人的规则 %d", otherID)
	}
	if seen[platformID] {
		t.Errorf("清单里出现了平台规则 %d", platformID)
	}
	if seen[channelScopedID] {
		t.Errorf("清单里出现了 channel 作用域、owner 是商家的规则 %d", channelScopedID)
	}

	if _, getErr := admin.GetOwned(context.Background(), owner, ownID); getErr != nil {
		t.Errorf("读自己的规则失败: %v", getErr)
	}
	for _, id := range []int64{otherID, platformID, channelScopedID, 999999999} {
		if _, getErr := admin.GetOwned(context.Background(), owner, id); !errors.Is(getErr, ErrRuleNotFound) {
			t.Errorf("GetOwned(%d) 的错误 = %v，期望 ErrRuleNotFound", id, getErr)
		}
		priority := 99
		if setErr := admin.UpdateOwned(context.Background(), owner, UpdateRequest{
			ID: id, Priority: &priority,
		}); !errors.Is(setErr, ErrRuleNotFound) {
			t.Errorf("UpdateOwned(%d) 的错误 = %v，期望 ErrRuleNotFound", id, setErr)
		}
	}

	// 零值 id 不是「不限制」。
	if _, getErr := admin.GetOwned(context.Background(), 0, ownID); getErr == nil {
		t.Error("GetOwned(owner=0) 没有报错")
	}
	if _, getErr := admin.ListOwned(context.Background(), 0); getErr == nil {
		t.Error("ListOwned(0) 没有报错")
	}

	// 自己的可以改。
	name := unique("renamed")
	if setErr := admin.UpdateOwned(context.Background(), owner, UpdateRequest{
		ID: ownID, Name: &name,
	}); setErr != nil {
		t.Fatalf("改自己的规则失败: %v", setErr)
	}
	got, readErr := admin.GetOwned(context.Background(), owner, ownID)
	if readErr != nil {
		t.Fatalf("改完再读失败: %v", readErr)
	}
	if got.Name != name {
		t.Errorf("改完的名字 = %q，期望 %q", got.Name, name)
	}

	// 商家不能把规则改成平台作用域或改归属：那是一次合法的校验，但越权。
	scope := ScopePlatform
	zero := int64(0)
	for _, req := range []UpdateRequest{
		{ID: ownID, Scope: &scope, OwnerUserID: &zero},
		{ID: ownID, OwnerUserID: &other},
		{ID: ownID, ChannelID: ptrID(1)},
	} {
		if setErr := admin.UpdateOwned(context.Background(), owner, req); setErr == nil {
			t.Errorf("商家改作用域/归属没有报错: %+v", req)
		}
	}
}

func ptrID(v int64) *int64 { return &v }

// insertChannelScopedRule 直接写库造一条 channel 作用域、owner_user_id 也填上
// 的规则，并登记用完删掉。
//
// 写入路径（Admin.Create / Admin.Update）不接受这种组合，但数据库约束允许它
// （唯一的约束只管 platform 不得挂 owner）。读路径上「商家只看自己的 merchant
// 规则」这个判据要靠它才钉得住：只按 owner 过滤时这一条会漏进商家清单。
func insertChannelScopedRule(t *testing.T, db *sql.DB, owner, channelID int64) int64 {
	t.Helper()

	var id int64
	if err := db.QueryRowContext(context.Background(), `
		INSERT INTO pricing_rules
			(scope, owner_user_id, channel_id, name, model_patterns, expr)
		VALUES ('channel', $1, $2, $3, '[]'::jsonb, '{"prompt": 0.4}')
		RETURNING id`, owner, channelID, unique("rule-channel-owner")).Scan(&id); err != nil {
		t.Fatalf("插入 channel 作用域夹具失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM pricing_rules WHERE id = $1`, id)
	})
	return id
}
