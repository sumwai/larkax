package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/pipeline"
	"github.com/sumwai/larkax/internal/pricing"
)

// testExpr 是一条能编译过的计价表达式：一组单价。
//
// 表达式在写入前会被编译一次，编译不过的命令直接失败——所以用例里用的这条
// 必须是合法的，否则测的就不是命令那一层了。
const testExpr = `{"prompt": 0.0000015, "completion": 0.0000075}`

// TestPriceCreateRejectsBadFlags 走的是不碰数据库的那几条路径。
func TestPriceCreateRejectsBadFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"没有 --scope", []string{"--name", "n1", "--expr", testExpr}, "缺少 --scope"},
		{"没有规则名", []string{"--scope", "platform", "--expr", testExpr}, "缺少规则名"},
		{"没有 --expr", []string{"--scope", "platform", "--name", "n1"}, "缺少 --expr"},
		// --by 必填：它记的是这条价是谁定的，而一条不记名的写入等于没有审计。
		// 判据取「缺了」与「给了空串」两种写法——空串多半是一个没展开的变量，
		// 静默地当成「没给这一项」会让它一路走到落库那一句。
		{"没有 --by", []string{
			"--scope", "platform", "--name", "n1", "--expr", testExpr,
		}, "缺少 --by"},
		{"--by 是空串", []string{
			"--scope", "platform", "--name", "n1", "--expr", testExpr, "--by", "  ",
		}, "缺少 --by"},
		{"--channel 是负数", []string{
			"--scope", "channel", "--name", "n1", "--expr", testExpr, "--by", "ops",
			"--channel", "-1",
		}, "无效"},
		{"--effective-from 不是 RFC3339", []string{
			"--scope", "platform", "--name", "n1", "--expr", testExpr, "--by", "ops",
			"--effective-from", "2026-01-01",
		}, "RFC3339"},
		{"规则名写在旗标后面", []string{
			"--scope", "platform", "--name", "n1", "--expr", testExpr, "extra",
		}, "不接受多余的参数"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runCaptured(t, func() error { return runPriceCreate(tc.args) })
			if err == nil {
				t.Fatalf("期望报错，却通过了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息 %q 里没有 %q", err.Error(), tc.want)
			}
			if got := exitCode(err); got != exitUsage {
				t.Errorf("参数错的退出码应当是 %d，得到 %d", exitUsage, got)
			}
		})
	}
}

// TestPriceCreateThenList 钉住计价规则的写入与列举。
func TestPriceCreateThenList(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	name := uniqueName("rule")
	model := uniqueName("model")

	id := addPriceRule(t, []string{
		"--scope", "platform", "--name", name, "--expr", testExpr,
		"--models", model, "--priority", "7",
	})

	got := findRule(t, listRules(t, db), id)
	if got.Name != name {
		t.Errorf("规则名 = %q，期望 %q", got.Name, name)
	}
	if got.Scope != pricing.ScopePlatform {
		t.Errorf("作用域 = %q，期望 %q", got.Scope, pricing.ScopePlatform)
	}
	if got.Priority != 7 {
		t.Errorf("优先级 = %d，期望 7", got.Priority)
	}
	if len(got.ModelPatterns) != 1 || got.ModelPatterns[0] != model {
		t.Errorf("模型清单 = %v，期望 [%q]", got.ModelPatterns, model)
	}
	if !got.Enabled {
		t.Error("没给 --disabled 时规则应当是启用的")
	}
	if got.ExprErr != nil {
		t.Errorf("表达式应当能编译：%v", got.ExprErr)
	}
	if len(got.DeclaredKeys) != 2 || got.DeclaredKeys[0] != "completion" || got.DeclaredKeys[1] != "prompt" {
		t.Errorf("声明的单价键 = %v，期望 [completion prompt]", got.DeclaredKeys)
	}

	listOut, _ := captureStreams(t, func() {
		if listErr := runPriceList(nil); listErr != nil {
			t.Fatalf("list 失败: %v", listErr)
		}
	})
	for _, want := range []string{name, model, "platform", "启用", "completion,prompt"} {
		if !strings.Contains(listOut, want) {
			t.Errorf("清单里没有 %q：\n%s", want, listOut)
		}
	}
}

// TestPriceCreateDisabledAndUnlimited 钉住两个「零值不是默认值」的地方。
//
// --disabled 是「明确要一条停用的规则」，不给它时是启用；--models 留空是
// 「适用于全部模型」（兜底规则），与 channels 的 models 正好相反。
// 这两处都是「没填」与「填了」必须分得开的地方。
func TestPriceCreateDisabledAndUnlimited(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	id := addPriceRule(t, []string{
		"--scope", "platform", "--name", uniqueName("rule"), "--expr", testExpr, "--disabled",
	})

	got := findRule(t, listRules(t, db), id)
	if got.Enabled {
		t.Error("给了 --disabled 的规则应当是停用的")
	}
	if len(got.ModelPatterns) != 0 {
		t.Errorf("没给 --models 时应当适用于全部模型（空清单），得到 %v", got.ModelPatterns)
	}
}

// TestPriceCreateRejectsBadExpression 钉住「编译不过的表达式进不了库」。
//
// 一条编译不过的规则进了库，表现是每个用到它的请求都失败，而失败点在请求
// 路径上——排查的人会先去怀疑请求。所以它必须被写入路径当场拒掉。
func TestPriceCreateRejectsBadExpression(t *testing.T) {
	testDB(t)
	setupEnv(t)

	by := createTestUserID(t)
	err := runCaptured(t, func() error {
		return runPriceCreate([]string{
			"--scope", "platform", "--name", uniqueName("rule"), "--expr", "p * ",
			"--by", by,
		})
	})
	if err == nil {
		t.Fatal("编译不过的表达式应当被拒")
	}
	if !strings.Contains(err.Error(), "编译失败") {
		t.Errorf("错误信息应当指出是编译不过，得到 %v", err)
	}
}

// TestPriceCreateRejectsWildcardModel 钉住通配符模型名。
//
// 读取路径按元素逐字匹配，`claude-*` 只是一个普通字符串——配了通配的规则
// 一条都匹配不上，而它不报错，只是静默退到别的价上。
func TestPriceCreateRejectsWildcardModel(t *testing.T) {
	testDB(t)
	setupEnv(t)

	by := createTestUserID(t)
	err := runCaptured(t, func() error {
		return runPriceCreate([]string{
			"--scope", "platform", "--name", uniqueName("rule"), "--expr", testExpr,
			"--models", "claude-*", "--by", by,
		})
	})
	if err == nil {
		t.Fatal("带通配符的模型名应当被拒")
	}
	if !strings.Contains(err.Error(), "通配符") {
		t.Errorf("错误信息应当指出通配符的问题，得到 %v", err)
	}
}

// TestPriceUpdateRejectsBadFlags 走的是不碰数据库的那几条路径。
//
// 「清不掉的那两栏」单独钉住：--effective-from 是 NOT NULL，--effective-to 要
// 清成不过期得用 --clear-effective-to。给了空串却被静默当成「不改这一项」时，
// 一次只想改这一栏的调用会变成「什么都没改」，而它看起来像一次成功的修改。
func TestPriceUpdateRejectsBadFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"没有定位符", []string{"--priority", "3"}, "缺少要改的规则"},
		{"没有 --by", []string{"--id", "1", "--priority", "3"}, "缺少 --by"},
		{"--by 是空串", []string{"--id", "1", "--priority", "3", "--by", "  "}, "缺少 --by"},
		{"没有要改的字段", []string{"--id", "1", "--by", "ops"}, "至少要改一项"},
		{"--enable 与 --disable 一起", []string{
			"--id", "1", "--by", "ops", "--enable", "--disable",
		}, "只能给一个"},
		{"--clear-effective-to 与 --effective-to 一起", []string{
			"--id", "1", "--by", "ops", "--clear-effective-to",
			"--effective-to", "2027-01-01T00:00:00Z",
		}, "只能给一个"},
		{"--user 是空串", []string{"--id", "1", "--by", "ops", "--user", ""}, "空串"},
		{"--channel 是负数", []string{"--id", "1", "--by", "ops", "--channel", "-1"}, "无效"},
		{"--expr 是空串", []string{"--id", "1", "--by", "ops", "--expr", ""}, "不能为空"},
		{"--effective-from 是空串", []string{
			"--id", "1", "--by", "ops", "--effective-from", "",
		}, "不能为空"},
		{"--effective-to 是空串", []string{
			"--id", "1", "--by", "ops", "--effective-to", "",
		}, "不能为空"},
		{"--effective-from 不是 RFC3339", []string{
			"--id", "1", "--by", "ops", "--effective-from", "2026-01-01",
		}, "RFC3339"},
		{"多出位置参数", []string{"--id", "1", "--priority", "3", "extra"}, "不接受多余的参数"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runCaptured(t, func() error { return runPriceUpdate(tc.args) })
			if err == nil {
				t.Fatalf("期望报错，却通过了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息 %q 里没有 %q", err.Error(), tc.want)
			}
			if got := exitCode(err); got != exitUsage {
				t.Errorf("参数错的退出码应当是 %d，得到 %d", exitUsage, got)
			}
		})
	}
}

// TestPriceUpdateChangesOnlyGivenFields 钉住「只改真的给了的那几项」。
//
// # 这条规则为什么必须是商家作用域
//
// 规则表是**全局**的，而这条用例中间有一步会把它改成「覆盖全部模型」
// （`--models ”`，兜底规则的正常写法）。平台作用域 + 空 model_patterns 的
// **启用**规则对每一个模型都生效，而 CI 里 `go test ./...` 把所有包跑在同一个
// 库上、包之间是并行的：那一步之后到本用例结束之前，别的包（例如
// internal/assembly 的端到端用例）查规则表时会命中它，于是它们的余额断言全错。
// 本地串行跑不出来——`go test -p 1 ./cmd/larkaxctl/` 没有别的包落在那个窗口里。
//
// 商家作用域只对这个商家的请求生效（见 internal/pricing 的 ruleApplicable：
// `r.scope = 'merchant' AND r.owner_user_id = $3`），而这个商家是本用例自己
// 造的，别的包不会拿它去查询。所以窗口期再长也打不到别人。
// internal/pricing/pricing_integration_test.go 里那条约定是同一个意思——
// 那里凡是空 model_patterns 的规则都挂在渠道作用域下。
func TestPriceUpdateChangesOnlyGivenFields(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	ownerID := createTestUser(t)
	by := createTestUserID(t)
	name := uniqueName("rule")
	model := uniqueName("model")
	id := addPriceRule(t, []string{
		"--scope", "merchant", "--user", strconv.FormatInt(ownerID, 10),
		"--name", name, "--expr", testExpr,
		"--models", model, "--priority", "3",
	})
	idArg := strconv.FormatInt(id, 10)

	_, _ = captureStreams(t, func() {
		if err := runPriceUpdate([]string{
			"--id", idArg, "--priority", "9", "--disable", "--by", by,
		}); err != nil {
			t.Fatalf("update 失败: %v", err)
		}
	})

	got := findRule(t, listRules(t, db), id)
	if got.Priority != 9 {
		t.Errorf("优先级 = %d，期望 9", got.Priority)
	}
	if got.Enabled {
		t.Error("给了 --disable 之后应当停用")
	}
	// 没提到的字段一个都不该动。
	if got.Name != name {
		t.Errorf("规则名 = %q，期望 %q", got.Name, name)
	}
	if got.Expr != testExpr {
		t.Errorf("表达式 = %q，期望 %q", got.Expr, testExpr)
	}
	if len(got.ModelPatterns) != 1 || got.ModelPatterns[0] != model {
		t.Errorf("模型清单 = %v，期望 [%q]", got.ModelPatterns, model)
	}

	// --models '' 是「适用于全部模型」（兜底规则），与 channels 的 models 相反。
	// --enable 顺带钉住启用那一侧。
	//
	// 这一步也是本用例最危险的一步：它造出一条**启用的、覆盖全部模型的**规则。
	// 这里不靠「跑完再停用」关门——那样门在用例跑着的那段时间是开着的，
	// 而那正是别的包会读到它的时间。挡住跨包影响的是商家作用域（见函数注释），
	// 所以跑完之后它可以一直是启用的。
	_, _ = captureStreams(t, func() {
		if err := runPriceUpdate([]string{
			"--id", idArg, "--models", "", "--enable", "--by", by,
		}); err != nil {
			t.Fatalf("update 失败: %v", err)
		}
	})
	got = findRule(t, listRules(t, db), id)
	if len(got.ModelPatterns) != 0 {
		t.Errorf("--models '' 应当适用于全部模型，得到 %v", got.ModelPatterns)
	}
	if !got.Enabled {
		t.Error("给了 --enable 之后应当启用")
	}
	if got.OwnerUserID != ownerID {
		t.Errorf("商家 = %d，期望 %d（这条规则的作用域是它挡住跨包影响的依据）",
			got.OwnerUserID, ownerID)
	}
}

// TestPriceUpdateScopeChangeNeedsClearing 钉住「校验的是改完之后的那一行」。
//
// 把作用域从 merchant 改成 platform 时，owner 那一栏必须一起清掉：读取路径
// 按 owner_user_id IS NULL 匹配 platform 规则，留着那一栏的规则一条都匹配
// 不上，而它看起来是一条正常的规则。写入路径据此拒掉这种组合，命令这一层
// 要把错误原样报出来，而不是替使用者猜他想清哪一栏。
func TestPriceUpdateScopeChangeNeedsClearing(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	ownerID := createTestUser(t)
	by := createTestUserID(t)
	id := addPriceRule(t, []string{
		"--scope", "merchant", "--name", uniqueName("rule"), "--expr", testExpr,
		"--user", strconv.FormatInt(ownerID, 10), "--models", uniqueName("model"),
	})
	idArg := strconv.FormatInt(id, 10)

	// 只改作用域：owner 还挂着，改完之后那一行是无效组合。
	err := runCaptured(t, func() error {
		return runPriceUpdate([]string{"--id", idArg, "--scope", "platform", "--by", by})
	})
	if err == nil {
		t.Fatal("merchant 改成 platform 时留着 owner 应当被拒")
	}
	if !strings.Contains(err.Error(), "platform 作用域的规则不挂商家") {
		t.Errorf("错误信息应当说清是哪一栏不合法，得到 %v", err)
	}

	// 把 owner 清掉（--user 0 就是「改成不挂商家」）之后改得动。
	_, _ = captureStreams(t, func() {
		if err := runPriceUpdate([]string{
			"--id", idArg, "--scope", "platform", "--user", "0", "--by", by,
		}); err != nil {
			t.Fatalf("清掉 owner 之后应当改得动: %v", err)
		}
	})
	got := findRule(t, listRules(t, db), id)
	if got.Scope != pricing.ScopePlatform {
		t.Errorf("作用域 = %q，期望 %q", got.Scope, pricing.ScopePlatform)
	}
	if got.OwnerUserID != 0 {
		t.Errorf("商家 = %d，期望 0（不挂商家）", got.OwnerUserID)
	}
}

// create 的 --by 落到 pricing_rules.created_by 上，第一版历史里也是同一个人。
//
// 第一版那一行不是顺带的：它的操作人取自规则的 created_by，而版本历史正是事后
// 回答「这个价什么时候、被谁改成这样的」的那张表。
func TestPriceCreateRecordsCreatedBy(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	by := createTestUserID(t)

	// 模型名单给一个只属于这条用例的名字：规则表是全局的，而一条**启用的、
	// 覆盖全部模型**的平台规则会被别的包读到（见
	// TestPriceUpdateChangesOnlyGivenFields 开头那一段）。
	stdout, _ := captureStreams(t, func() {
		if err := runPriceCreate([]string{
			"--scope", "platform", "--name", uniqueName("rule"), "--expr", testExpr,
			"--models", uniqueName("model"), "--by", by,
		}); err != nil {
			t.Errorf("price create 失败: %v", err)
		}
	})
	id, err := strconv.ParseInt(strings.TrimSpace(stdout), 10, 64)
	if err != nil {
		t.Fatalf("stdout 不是规则 ID：%q（%v）", stdout, err)
	}
	want, err := strconv.ParseInt(by, 10, 64)
	if err != nil {
		t.Fatalf("用例造的用户 ID 不是整数：%v", err)
	}

	if got := ruleCreatedBy(t, db, id); !got.Valid || got.Int64 != want {
		t.Errorf("pricing_rules.created_by = %v，期望 %d", got, want)
	}
	if got := ruleVersionActor(t, db, id, 1); !got.Valid || got.Int64 != want {
		t.Errorf("第一版的 changed_by = %v，期望 %d", got, want)
	}
}

// update 的 --by 落到新那一版历史的 changed_by 上，规则行自己的 created_by 不动。
//
// 两栏记的不是同一件事：created_by 是这条价是谁**定**的，而改价的人可能根本没
// 参与过定它。把这个人当成改动人写下来，审计会读到一个没做过这件事的人。
func TestPriceUpdateRecordsChangedBy(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	creator, editor := createTestUserID(t), createTestUserID(t)

	id := addPriceRule(t, []string{
		"--scope", "platform", "--name", uniqueName("rule"), "--expr", testExpr,
		"--models", uniqueName("model"), "--by", creator,
	})
	idArg := strconv.FormatInt(id, 10)

	// 改表达式才留一版（见 internal/pricing 的 Update）：改名字、改优先级不
	// 改变任何一笔已经算出来的钱，不为它们插一行。
	_, _ = captureStreams(t, func() {
		if err := runPriceUpdate([]string{
			"--id", idArg, "--expr", `{"prompt": 0.000003}`, "--by", editor,
		}); err != nil {
			t.Fatalf("price update 失败: %v", err)
		}
	})

	creatorID, err := strconv.ParseInt(creator, 10, 64)
	if err != nil {
		t.Fatalf("用例造的用户 ID 不是整数：%v", err)
	}
	editorID, err := strconv.ParseInt(editor, 10, 64)
	if err != nil {
		t.Fatalf("用例造的用户 ID 不是整数：%v", err)
	}

	if got := ruleVersionActor(t, db, id, 2); !got.Valid || got.Int64 != editorID {
		t.Errorf("第二版的 changed_by = %v，期望 %d", got, editorID)
	}
	if got := ruleCreatedBy(t, db, id); !got.Valid || got.Int64 != creatorID {
		t.Errorf("改价之后 created_by = %v，期望 %d 不动", got, creatorID)
	}
}

// ruleCreatedBy 读一条规则的创建人（pricing_rules.created_by）。
//
// 直接查库：这条命令的读路径（price list / explain）不打这一栏，而要钉住的正是
// 「写入侧有没有把它落下去」。
func ruleCreatedBy(t *testing.T, db *sql.DB, ruleID int64) sql.NullInt64 {
	t.Helper()
	var by sql.NullInt64
	if err := db.QueryRowContext(context.Background(),
		`SELECT created_by FROM pricing_rules WHERE id = $1`, ruleID).Scan(&by); err != nil {
		t.Fatalf("读规则 %d 的创建人失败: %v", ruleID, err)
	}
	return by
}

// ruleVersionActor 读某一版历史的操作人（pricing_rule_versions.changed_by）。
//
// 这张表今天只有写入方、没有读取方（见 internal/pricing 的 Update），所以只能
// 直接查它。
func ruleVersionActor(t *testing.T, db *sql.DB, ruleID int64, version int) sql.NullInt64 {
	t.Helper()
	var by sql.NullInt64
	if err := db.QueryRowContext(context.Background(),
		`SELECT changed_by FROM pricing_rule_versions WHERE rule_id = $1 AND version = $2`,
		ruleID, version).Scan(&by); err != nil {
		t.Fatalf("读规则 %d 第 %d 版的操作人失败: %v", ruleID, version, err)
	}
	return by
}

// TestPriceExplainPicksWinner 钉住 explain 回答的是「按哪条计价」。
//
// 它要把**全部适用**的规则按裁决次序列出来，第一条就是会被选中的那条：
// 只给胜出的那条不够，运维还要知道有没有一条更该生效的规则被压住了。
// 次序与 Match 是同一段 SQL，所以「渠道自己的价压过平台价」在这里看得见。
func TestPriceExplainPicksWinner(t *testing.T) {
	testDB(t)
	setupEnv(t)

	model := uniqueName("model")
	platformName := uniqueName("rule")
	channelName := uniqueName("rule")

	platformID := addPriceRule(t, []string{
		"--scope", "platform", "--name", platformName, "--expr", testExpr,
		"--models", model,
	})

	withStdin(t, testSecret)
	channelOut, _ := captureStreams(t, func() {
		if err := runChannelAdd([]string{
			"--name", uniqueName("channel"), "--type", testChanType,
			"--base-url", testBaseURL, "--models", model, "--credential-stdin",
		}); err != nil {
			t.Fatalf("channel create 失败: %v", err)
		}
	})
	channelID, err := strconv.ParseInt(strings.TrimSpace(channelOut), 10, 64)
	if err != nil {
		t.Fatalf("stdout 不是渠道 ID：%q（%v）", channelOut, err)
	}
	channelIDArg := strconv.FormatInt(channelID, 10)
	channelRuleID := addPriceRule(t, []string{
		"--scope", "channel", "--name", channelName, "--expr", testExpr,
		"--channel", channelIDArg, "--models", model,
	})

	// 有渠道时：渠道自己的价赢，平台价被压住——两条都要出现在输出里。
	out, _ := captureStreams(t, func() {
		if err := runPriceExplain([]string{"--model", model, "--channel", channelIDArg}); err != nil {
			t.Fatalf("explain 失败: %v", err)
		}
	})
	for _, want := range []string{platformName, channelName, "选中", "被压住"} {
		if !strings.Contains(out, want) {
			t.Errorf("explain 的输出里没有 %q：\n%s", want, out)
		}
	}
	if !selectedRuleIs(out, channelRuleID) {
		t.Errorf("被选中的应当是渠道规则 %d：\n%s", channelRuleID, out)
	}

	// 不给渠道时：平台价兜底。
	out, _ = captureStreams(t, func() {
		if err := runPriceExplain([]string{"--model", model}); err != nil {
			t.Fatalf("explain 失败: %v", err)
		}
	})
	if !selectedRuleIs(out, platformID) {
		t.Errorf("不给 --channel 时被选中的应当是平台规则 %d：\n%s", platformID, out)
	}
}

// TestPriceExplainNoRule 钉住「一条都不适用」是一个该被处置的结果。
//
// 请求路径上它是拒绝服务，不是按 0 元计（见 pricing.Admin.Explain），所以
// 这里也不能退化成一个空清单——一张空表与「命令没跑起来」长得一样。
func TestPriceExplainNoRule(t *testing.T) {
	testDB(t)
	setupEnv(t)

	err := runCaptured(t, func() error {
		return runPriceExplain([]string{"--model", uniqueName("model")})
	})
	if err == nil {
		t.Fatal("没有适用规则时应当报错，而不是按 0 元算")
	}
	if !errors.Is(err, pipeline.ErrNoRule) {
		t.Errorf("期望 pipeline.ErrNoRule，得到 %v", err)
	}
	if got := exitCode(err); got != exitConflict {
		t.Errorf("退出码应当是 %d，得到 %d", exitConflict, got)
	}
}

// TestPrintRuleExplainWithoutRules 钉住「一条都没有」时不是一张空表。
//
// 这条路径从命令走不到：pricing.Admin.Explain 对「一条都不适用」返回
// ErrNoRule，而不是一个空清单。留着那个分支是因为 printRuleExplain 是一个
// 可以直接调的函数，而一张只有表头的空表与「命令没跑起来」长得一样——
// 所以这里直接调它，把那一行钉住（一个没人钉住的守卫等于没被证明在工作）。
func TestPrintRuleExplainWithoutRules(t *testing.T) {
	out := captureStdout(t, func() {
		printRuleExplain(os.Stdout, pricing.ExplainQuery{
			CanonicalModel: "m1",
			At:             time.Now(),
		}, nil)
	})
	if !strings.Contains(out, "没有适用的计价规则") {
		t.Errorf("一条规则都没有时应当明说：\n%s", out)
	}
	if strings.Contains(out, "裁决") {
		t.Errorf("一条规则都没有时不该打出表头：\n%s", out)
	}
}

// selectedRuleIs 判断 explain 的输出里带「选中」那一行是不是指定的规则。
//
// 按裁决那一栏找而不是按行号找：断言要钉的是「谁赢」这个结论，不是它恰好
// 排在第几行——行号是排版，而排序由 SQL 决定，两者不是同一件事。
func selectedRuleIs(out string, id int64) bool {
	want := strconv.FormatInt(id, 10)
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "选中") {
			continue
		}
		fields := strings.Fields(line)
		return len(fields) > 0 && fields[0] == want
	}
	return false
}

// addPriceRule 建一条规则并返回它的 ID。
//
// --by 必填，而多数用例不比这一栏：这里补上一个现建的用户。已经自己给了
// --by 的用例原样保留——那一栏正是它要断言的东西（见 hasFlagArg）。
func addPriceRule(t *testing.T, args []string) int64 {
	t.Helper()
	if !hasFlagArg(args, "--by") {
		args = append(args, "--by", createTestUserID(t))
	}
	stdout, _ := captureStreams(t, func() {
		if err := runPriceCreate(args); err != nil {
			t.Fatalf("price add 失败: %v", err)
		}
	})
	id, err := strconv.ParseInt(strings.TrimSpace(stdout), 10, 64)
	if err != nil {
		t.Fatalf("stdout 不是规则 ID：%q（%v）", stdout, err)
	}
	return id
}

// hasFlagArg 判断参数清单里有没有给这一个旗标。
//
// 只认 `--名字` 这一种写法：这一组用例都用长名写操作人，而短名与别名的存在
// 由 short_flags_test.go 那一张表管。
func hasFlagArg(args []string, name string) bool {
	for _, arg := range args {
		if arg == name {
			return true
		}
	}
	return false
}

// listRules 列出全部规则。
func listRules(t *testing.T, db *sql.DB) []pricing.RuleInfo {
	t.Helper()
	rules, err := pricing.NewAdmin(db).List(context.Background())
	if err != nil {
		t.Fatalf("列举规则失败: %v", err)
	}
	return rules
}

func findRule(t *testing.T, rules []pricing.RuleInfo, id int64) pricing.RuleInfo {
	t.Helper()
	for _, rule := range rules {
		if rule.ID == id {
			return rule
		}
	}
	t.Fatalf("清单里没有规则 %d", id)
	return pricing.RuleInfo{}
}

// ── 机器可读输出（--format json）─────────────────────────────────────────

// TestPriceListDocIsMachineReadable 钉住 price list 的机器可读形状。
func TestPriceListDocIsMachineReadable(t *testing.T) {
	from := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	to := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	rules := []pricing.RuleInfo{
		{
			ID: 41, Scope: "platform", Name: "默认价",
			ModelPatterns: []string{"m1", "m2"}, Priority: 3, Enabled: true,
			EffectiveFrom: from, EffectiveTo: &to,
			DeclaredKeys: []string{"prompt", "completion"},
		},
		{
			// 不挂商家、不挂渠道（两栏都是 null）、不过期、表达式编译不过的
			// 一条：表格那一档的 `-`、「不过期」与「（表达式编译不过）」三个
			// 渲染一个都不该进 JSON。
			ID: 42, Scope: "platform", Name: "坏规则",
			Enabled: false, EffectiveFrom: from,
			ExprErr: errors.New("编译失败：未知变量"),
		},
	}

	got := jsonRoundTrip(t, priceListDoc(rules))
	assertJSONField(t, got, "status", jsonOK)
	assertJSONField(t, jsonObjectField(t, got, "summary"), "total", float64(2))

	list := jsonEntries(t, got, "rules")
	if len(list) != 2 {
		t.Fatalf("规则数 = %d，期望 2", len(list))
	}
	first := list[0].(map[string]any)
	assertJSONField(t, first, "id", float64(41))
	assertJSONField(t, first, "scope", "platform")
	assertJSONField(t, first, "owner_user_id", nil)
	assertJSONField(t, first, "channel_id", nil)
	assertJSONField(t, first, "name", "默认价")
	assertJSONField(t, first, "model_patterns", []any{"m1", "m2"})
	assertJSONField(t, first, "priority", float64(3))
	// 状态是布尔，不是表格那一栏的「启用/停用」。
	assertJSONField(t, first, "enabled", true)
	assertJSONField(t, first, "effective_from", "2026-01-02T03:04:05Z")
	assertJSONField(t, first, "effective_to", "2026-06-01T00:00:00Z")
	// 单价键是那一份原始清单，不是 declaredKeysLabel 渲染出来的那一栏。
	assertJSONField(t, first, "quota_keys", []any{"prompt", "completion"})
	assertJSONField(t, first, "expr_error", nil)

	second := list[1].(map[string]any)
	assertJSONField(t, second, "enabled", false)
	assertJSONField(t, second, "effective_to", nil)
	assertJSONField(t, second, "model_patterns", []any{})
	assertJSONField(t, second, "quota_keys", []any{})
	assertJSONField(t, second, "expr_error", "编译失败：未知变量")

	t.Run("空清单是空数组，不是 null", func(t *testing.T) {
		empty := jsonRoundTrip(t, priceListDoc(nil))
		assertJSONField(t, jsonObjectField(t, empty, "summary"), "total", float64(0))
		if len(jsonEntries(t, empty, "rules")) != 0 {
			t.Error("空清单应当是空数组")
		}
	})
}

// TestPriceExplainDocIsMachineReadable 钉住 price explain 的机器可读形状。
func TestPriceExplainDocIsMachineReadable(t *testing.T) {
	at := time.Date(2026, 7, 8, 9, 10, 11, 0, time.UTC)
	q := pricing.ExplainQuery{
		CanonicalModel: "m1", OwnerUserID: 5, ChannelID: 9, At: at,
	}
	rules := []pricing.RuleInfo{
		{
			ID: 41, Scope: "channel", OwnerUserID: 5, ChannelID: 9, Name: "选中",
			ModelPatterns: []string{"m1"}, Enabled: true, EffectiveFrom: at,
			DeclaredKeys: []string{"prompt"},
		},
		{
			ID: 42, Scope: "platform", Name: "被压住",
			Enabled: true, EffectiveFrom: at,
			DeclaredKeys: []string{"prompt"},
		},
	}

	got := jsonRoundTrip(t, priceExplainDoc(q, rules))
	assertJSONField(t, got, "status", jsonOK)

	query := jsonObjectField(t, got, "query")
	assertJSONField(t, query, "canonical_model", "m1")
	assertJSONField(t, query, "owner_user_id", float64(5))
	assertJSONField(t, query, "channel_id", float64(9))
	assertJSONField(t, query, "at", "2026-07-08T09:10:11Z")

	list := jsonEntries(t, got, "rules")
	if len(list) != 2 {
		t.Fatalf("规则数 = %d，期望 2", len(list))
	}
	// 排第一的就是会被计价的那条：裁决那一栏是给脚本分派用的，所以用英文
	// 的取值，而不是表格那一档的「选中 / 被压住」。
	assertJSONField(t, list[0].(map[string]any), "verdict", "selected")
	assertJSONField(t, list[1].(map[string]any), "verdict", "shadowed")

	t.Run("没选定商家与渠道时是 null", func(t *testing.T) {
		bare := jsonRoundTrip(t, priceExplainDoc(pricing.ExplainQuery{
			CanonicalModel: "m1", At: at,
		}, nil))
		query := jsonObjectField(t, bare, "query")
		assertJSONField(t, query, "owner_user_id", nil)
		assertJSONField(t, query, "channel_id", nil)
		// 一条都不适用时写入路径报错而不是给空清单（见 pricing.Admin.Explain），
		// 但组装函数这一层仍然要给 `[]`——空数组与 null 是两件事。
		if len(jsonEntries(t, bare, "rules")) != 0 {
			t.Error("没有规则时应当是空数组")
		}
	})
}

// TestPriceWriteCommandsRejectJSONBeforeDoingAnything 钉住写类命令的守卫位置。
func TestPriceWriteCommandsRejectJSONBeforeDoingAnything(t *testing.T) {
	withFormat(t, formatJSON)

	cases := []struct {
		command string
		run     func() error
	}{
		{"price add", func() error { return runPriceCreate(nil) }},
		{"price update", func() error { return runPriceUpdate(nil) }},
	}

	for _, tc := range cases {
		t.Run(tc.command, func(t *testing.T) {
			err := tc.run()
			if err == nil {
				t.Fatalf("%s 在 json 模式下应当被拒", tc.command)
			}
			if !strings.Contains(err.Error(), tc.command+" 还不支持 --format json") {
				t.Fatalf("错误信息应当指出 %s 不支持 json，得到 %q", tc.command, err.Error())
			}
			if got := exitCode(err); got != exitUsage {
				t.Errorf("退出码 = %d，期望参数错的 %d", got, exitUsage)
			}
		})
	}
}

// ── 位置参数、缺省写 0、按名字定位 ────────────────────────────────────────

// TestPriceAddPositionalNameLandsTheSameExpression 钉住「规则名走位置参数」与
// 「单价缺省写 0」两件事落到库里的结果是同一份。
//
// 两种写法建两条规则，逐字比库里的表达式：
//
//	<规则名> --in 1.5                 位置参数 + 短旗标
//	--name <规则名> --prompt 1.5      旧写法：--name + 旧长名
//
// 期望值是写死的字面量。**这条用例不叫「旧写法行为不变」**：库里那一栏本来就
// 会变——旧写法以前落的是只有 prompt 一项的表，现在五项齐全、其余写 0。变的是
// 「漏项」这件事的处理方式（见 priceUsage 里那段），不是单位换算。
func TestPriceAddPositionalNameLandsTheSameExpression(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	by := createTestUserID(t)
	model := uniqueName("model")

	positionalName := uniqueName("rule")
	positionalID := addPriceRule(t, []string{
		positionalName, "--scope", "platform",
		"--models", model, "--in", "1.5", "--by", by,
	})
	legacyName := uniqueName("rule")
	legacyID := addPriceRule(t, []string{
		"--scope", "platform", "--name", legacyName,
		"--models", model, "--prompt", "1.5", "--by", by,
	})

	const want = `{"prompt": 0.0000015, "completion": 0, "cache_read": 0, ` +
		`"cache_write": 0, "reasoning": 0}`
	rules := listRules(t, db)
	if got := findRule(t, rules, positionalID).Expr; got != want {
		t.Errorf("位置参数写法落库的表达式 = %s\n期望 %s", got, want)
	}
	if got := findRule(t, rules, legacyID).Expr; got != want {
		t.Errorf("--name + --prompt 写法落库的表达式 = %s\n期望 %s", got, want)
	}
	if got := findRule(t, rules, positionalID).Name; got != positionalName {
		t.Errorf("规则名 = %q，期望 %q", got, positionalName)
	}
}

// TestPriceAddNameAndOperandMustAgree 钉住位置参数与 --name 的两种给法：
// 同值取位置参数，异值报错。
//
// 异值时静默取其中一个的表现是「我明明改了名字，落库的还是旧的」，而那是一次
// 成功退出、库里没有那条新名字的写入。
func TestPriceAddNameAndOperandMustAgree(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	by := createTestUserID(t)
	model := uniqueName("model")

	same := uniqueName("rule")
	id := addPriceRule(t, []string{
		same, "--scope", "platform", "--name", same,
		"--models", model, "--in", "1", "--by", by,
	})
	if got := findRule(t, listRules(t, db), id).Name; got != same {
		t.Errorf("同值两处给时规则名 = %q，期望 %q", got, same)
	}

	r := runOnce(t, runPrice, []string{
		"add", uniqueName("rule"), "--scope", "platform", "--name", uniqueName("rule"),
		"--models", model, "--in", "1", "--by", by,
	})
	if r.err == nil {
		t.Fatalf("位置参数与 --name 给两个不同的名字应当报错，实际通过")
	}
	if !strings.Contains(r.err.Error(), "两个不同的值") {
		t.Errorf("错误信息里该点出两处给了不同的值：%v", r.err)
	}
	if got := exitCode(r.err); got != exitUsage {
		t.Errorf("退出码 = %d，期望参数错的 %d", got, exitUsage)
	}
}

// TestPriceUpdateLocatesByName 钉住 update 按名字定位。
//
// 只改一条自己的规则：别的用例建的规则名与它不同，命中唯一。
func TestPriceUpdateLocatesByName(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	by := createTestUserID(t)
	name := uniqueName("rule")
	id := addPriceRule(t, []string{
		name, "--scope", "platform", "--models", uniqueName("model"),
		"--expr", testExpr, "--by", by,
	})

	_, _ = captureStreams(t, func() {
		if err := runPriceUpdate([]string{name, "--priority", "7", "--by", by}); err != nil {
			t.Fatalf("按名字 update 失败: %v", err)
		}
	})
	if got := findRule(t, listRules(t, db), id).Priority; got != 7 {
		t.Errorf("按名字定位之后优先级 = %d，期望 7", got)
	}
}

// TestPriceUpdateLocatorAmbiguityListsCandidates 钉住重名时列候选、不猜。
//
// 三条同名规则分别落在两个作用域上：挑一条改等于在三条里随便选一条改价，
// 而命令照样成功退出。所以命中多条时必须报错，并把作用域、商家、渠道、
// 优先级一起打出来——运营看到三条同名的要能挑。
func TestPriceUpdateLocatorAmbiguityListsCandidates(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	by := createTestUserID(t)
	ownerA := createTestUser(t)
	ownerB := createTestUser(t)
	// 名字带上 base 与序号：同一个库上跑第二次也不会撞上上一次留下的同名规则。
	name := uniqueName("dup")

	platformID := addPriceRule(t, []string{
		name, "--scope", "platform", "--models", uniqueName("model"), "--expr", testExpr,
		"--priority", "3", "--by", by,
	})
	merchantA := addPriceRule(t, []string{
		name, "--scope", "merchant", "--user", strconv.FormatInt(ownerA, 10),
		"--models", uniqueName("model"), "--expr", testExpr, "--by", by,
	})
	merchantB := addPriceRule(t, []string{
		name, "--scope", "merchant", "--user", strconv.FormatInt(ownerB, 10),
		"--models", uniqueName("model"), "--expr", testExpr, "--by", by,
	})

	r := runOnce(t, runPrice, []string{"update", name, "--priority", "5", "--by", by})
	if r.err == nil {
		t.Fatalf("同名三条时按名字定位应当报错，实际通过")
	}
	msg := r.err.Error()
	for _, want := range []string{
		"3 条", name,
		// 候选里要能看到作用域与商家：只有 ID 时运营还得自己去 list 里找。
		"platform", "merchant",
		strconv.FormatInt(ownerA, 10), strconv.FormatInt(ownerB, 10),
		// 优先级那一栏：三条同名规则里要挑一条，光有 ID 不够。
		"优先级",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("候选清单里该有 %q：%v", want, r.err)
		}
	}
	for _, id := range []int64{platformID, merchantA, merchantB} {
		if !strings.Contains(msg, "ID="+strconv.FormatInt(id, 10)) {
			t.Errorf("候选清单里该有规则 %d：%v", id, r.err)
		}
	}
	// 一条都没被改：报错发生在写入之前。
	for _, id := range []int64{platformID, merchantA, merchantB} {
		if got := findRule(t, listRules(t, db), id).Priority; got == 5 {
			t.Errorf("规则 %d 的优先级被改成了 5：重名时不该改任何一条", id)
		}
	}

	// 给 --scope 收窄到唯一一条之后能改：那正是「同名跨作用域」的处置办法。
	_, _ = captureStreams(t, func() {
		if err := runPriceUpdate([]string{
			name, "--scope", "platform", "--priority", "5", "--by", by,
		}); err != nil {
			t.Fatalf("带 --scope 收窄之后 update 失败: %v", err)
		}
	})
	if got := findRule(t, listRules(t, db), platformID).Priority; got != 5 {
		t.Errorf("平台那条的优先级 = %d，期望 5", got)
	}
	for _, id := range []int64{merchantA, merchantB} {
		if got := findRule(t, listRules(t, db), id).Priority; got == 5 {
			t.Errorf("商家规则 %d 也被改了：--scope 没起到收窄的作用", id)
		}
	}
}

// TestPriceUpdateIDAndLocatorMustAgree 钉住 --id 与位置参数只能给一处。
func TestPriceUpdateIDAndLocatorMustAgree(t *testing.T) {
	requireDB(t)
	setupEnv(t)

	r := runOnce(t, runPrice, []string{"update", "7", "--id", "9", "--priority", "3", "--by", "ops"})
	if r.err == nil {
		t.Fatalf("位置参数与 --id 给两个不同的 ID 应当报错，实际通过")
	}
	if !strings.Contains(r.err.Error(), "两个不同的值") {
		t.Errorf("错误信息里该点出两处给了不同的值：%v", r.err)
	}
}
