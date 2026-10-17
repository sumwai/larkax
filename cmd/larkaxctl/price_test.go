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

// TestPriceAddRejectsBadFlags 走的是不碰数据库的那几条路径。
func TestPriceAddRejectsBadFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"没有 --scope", []string{"--name", "n1", "--expr", testExpr}, "缺少 --scope"},
		{"没有 --name", []string{"--scope", "platform", "--expr", testExpr}, "缺少 --name"},
		{"没有 --expr", []string{"--scope", "platform", "--name", "n1"}, "缺少 --expr"},
		{"--channel 是负数", []string{
			"--scope", "channel", "--name", "n1", "--expr", testExpr, "--channel", "-1",
		}, "无效"},
		{"--effective-from 不是 RFC3339", []string{
			"--scope", "platform", "--name", "n1", "--expr", testExpr,
			"--effective-from", "2026-01-01",
		}, "RFC3339"},
		{"多出位置参数", []string{
			"--scope", "platform", "--name", "n1", "--expr", testExpr, "extra",
		}, "不接受位置参数"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runCaptured(t, func() error { return runPriceAdd(tc.args) })
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

// TestPriceAddThenList 钉住计价规则的写入与列举。
func TestPriceAddThenList(t *testing.T) {
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

// TestPriceAddDisabledAndUnlimited 钉住两个「零值不是默认值」的地方。
//
// --disabled 是「明确要一条停用的规则」，不给它时是启用；--models 留空是
// 「适用于全部模型」（兜底规则），与 channels 的 models 正好相反。
// 这两处都是「没填」与「填了」必须分得开的地方。
func TestPriceAddDisabledAndUnlimited(t *testing.T) {
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

// TestPriceAddRejectsBadExpression 钉住「编译不过的表达式进不了库」。
//
// 一条编译不过的规则进了库，表现是每个用到它的请求都失败，而失败点在请求
// 路径上——排查的人会先去怀疑请求。所以它必须被写入路径当场拒掉。
func TestPriceAddRejectsBadExpression(t *testing.T) {
	testDB(t)
	setupEnv(t)

	err := runCaptured(t, func() error {
		return runPriceAdd([]string{
			"--scope", "platform", "--name", uniqueName("rule"), "--expr", "p * ",
		})
	})
	if err == nil {
		t.Fatal("编译不过的表达式应当被拒")
	}
	if !strings.Contains(err.Error(), "编译失败") {
		t.Errorf("错误信息应当指出是编译不过，得到 %v", err)
	}
}

// TestPriceAddRejectsWildcardModel 钉住通配符模型名。
//
// 读取路径按元素逐字匹配，`claude-*` 只是一个普通字符串——配了通配的规则
// 一条都匹配不上，而它不报错，只是静默退到别的价上。
func TestPriceAddRejectsWildcardModel(t *testing.T) {
	testDB(t)
	setupEnv(t)

	err := runCaptured(t, func() error {
		return runPriceAdd([]string{
			"--scope", "platform", "--name", uniqueName("rule"), "--expr", testExpr,
			"--models", "claude-*",
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
		{"没有 --id", []string{"--priority", "3"}, "缺少 --id"},
		{"没有要改的字段", []string{"--id", "1"}, "至少要改一项"},
		{"--enable 与 --disable 一起", []string{"--id", "1", "--enable", "--disable"}, "只能给一个"},
		{"--clear-effective-to 与 --effective-to 一起", []string{
			"--id", "1", "--clear-effective-to", "--effective-to", "2027-01-01T00:00:00Z",
		}, "只能给一个"},
		{"--owner 是空串", []string{"--id", "1", "--owner", ""}, "空串"},
		{"--channel 是负数", []string{"--id", "1", "--channel", "-1"}, "无效"},
		{"--expr 是空串", []string{"--id", "1", "--expr", ""}, "不能为空"},
		{"--effective-from 是空串", []string{"--id", "1", "--effective-from", ""}, "不能为空"},
		{"--effective-to 是空串", []string{"--id", "1", "--effective-to", ""}, "不能为空"},
		{"--effective-from 不是 RFC3339", []string{
			"--id", "1", "--effective-from", "2026-01-01",
		}, "RFC3339"},
		{"多出位置参数", []string{"--id", "1", "--priority", "3", "extra"}, "不接受位置参数"},
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
	name := uniqueName("rule")
	model := uniqueName("model")
	id := addPriceRule(t, []string{
		"--scope", "merchant", "--owner", strconv.FormatInt(ownerID, 10),
		"--name", name, "--expr", testExpr,
		"--models", model, "--priority", "3",
	})
	idArg := strconv.FormatInt(id, 10)

	_, _ = captureStreams(t, func() {
		if err := runPriceUpdate([]string{"--id", idArg, "--priority", "9", "--disable"}); err != nil {
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
		if err := runPriceUpdate([]string{"--id", idArg, "--models", "", "--enable"}); err != nil {
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
	id := addPriceRule(t, []string{
		"--scope", "merchant", "--name", uniqueName("rule"), "--expr", testExpr,
		"--owner", strconv.FormatInt(ownerID, 10), "--models", uniqueName("model"),
	})
	idArg := strconv.FormatInt(id, 10)

	// 只改作用域：owner 还挂着，改完之后那一行是无效组合。
	err := runCaptured(t, func() error {
		return runPriceUpdate([]string{"--id", idArg, "--scope", "platform"})
	})
	if err == nil {
		t.Fatal("merchant 改成 platform 时留着 owner 应当被拒")
	}
	if !strings.Contains(err.Error(), "platform 作用域的规则不挂商家") {
		t.Errorf("错误信息应当说清是哪一栏不合法，得到 %v", err)
	}

	// 把 owner 清掉（--owner 0 就是「改成不挂商家」）之后改得动。
	_, _ = captureStreams(t, func() {
		if err := runPriceUpdate([]string{"--id", idArg, "--scope", "platform", "--owner", "0"}); err != nil {
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
		if err := runChannelCreate([]string{
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
func addPriceRule(t *testing.T, args []string) int64 {
	t.Helper()
	stdout, _ := captureStreams(t, func() {
		if err := runPriceAdd(args); err != nil {
			t.Fatalf("price add 失败: %v", err)
		}
	})
	id, err := strconv.ParseInt(strings.TrimSpace(stdout), 10, 64)
	if err != nil {
		t.Fatalf("stdout 不是规则 ID：%q（%v）", stdout, err)
	}
	return id
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
