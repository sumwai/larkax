package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/cmd/larkaxctl/internal/cli"
	"github.com/sumwai/larkax/internal/pricing"
)

// testCostExpr 是一条能编译过的成本表达式：一组单价。
//
// 表达式在写入前会被编译一次，编译不过的命令直接失败——所以用例里用的这条
// 必须是合法的，否则测的就不是命令那一层了。
const testCostExpr = `{"prompt": 0.0000012, "completion": 0.000006}`

// createCostChannel 建一条渠道并返回它的 ID。
//
// 成本规则挂在渠道上（外键要求那条渠道存在），所以每条用例先要一条。渠道名与
// 模型名都用 uniqueName：同一个库上跑第二次 `go test` 时纯计数会重名。
func createCostChannel(t *testing.T) (int64, string) {
	t.Helper()

	model := uniqueName("model")
	withStdin(t, testSecret)
	out, _ := captureStreams(t, func() {
		if err := runChannelAdd([]string{
			"--name", uniqueName("channel"), "--type", testChanType,
			"--base-url", testBaseURL, "--models", model, "--credential-stdin",
		}); err != nil {
			t.Fatalf("channel create 失败: %v", err)
		}
	})
	id, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		t.Fatalf("stdout 不是渠道 ID：%q（%v）", out, err)
	}
	return id, model
}

// addCostRule 建一条成本规则并返回它的 ID。
func addCostRule(t *testing.T, args []string) int64 {
	t.Helper()

	stdout, _ := captureStreams(t, func() {
		if err := runUpstreamCostCreate(args); err != nil {
			t.Fatalf("upstream-cost create 失败: %v", err)
		}
	})
	id, err := strconv.ParseInt(strings.TrimSpace(stdout), 10, 64)
	if err != nil {
		t.Fatalf("stdout 不是规则 ID：%q（%v）", stdout, err)
	}
	return id
}

// listCostRules 连库读一遍成本规则，用来断言落库的值。
func listCostRules(t *testing.T, db *sql.DB, channelID int64) []pricing.CostInfo {
	t.Helper()

	rules, err := pricing.NewCostAdmin(db).List(context.Background(), pricing.CostListQuery{ChannelID: channelID})
	if err != nil {
		t.Fatalf("列举成本规则失败: %v", err)
	}
	return rules
}

// firstCostRowID 取表里第一行数据的第一栏（规则 ID）。
//
// 按「第一栏是个整数」认数据行：表头与那两行说明的第一栏都不是数。
func firstCostRowID(t *testing.T, out string) int64 {
	t.Helper()

	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		id, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}
		return id
	}
	t.Fatalf("输出里一行数据都没有：\n%s", out)
	return 0
}

// TestUpstreamCostCreateRejectsBadFlags 走的是不碰数据库的那几条路径。
func TestUpstreamCostCreateRejectsBadFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"没有 --channel", []string{"--expr", testCostExpr}, "缺少 --channel"},
		{"--channel 是 0", []string{"--channel", "0", "--expr", testCostExpr}, "缺少 --channel"},
		{"--channel 是负数", []string{"--channel", "-1", "--expr", testCostExpr}, "无效"},
		{"没有 --expr", []string{"--channel", "1"}, "缺少 --expr"},
		{"--expr 是空串", []string{"--channel", "1", "--expr", "  "}, "缺少 --expr"},
		{"--effective-from 不是 RFC3339", []string{
			"--channel", "1", "--expr", testCostExpr, "--effective-from", "2026-01-01",
		}, "RFC3339"},
		{"多出位置参数", []string{
			"--channel", "1", "--expr", testCostExpr, "extra",
		}, "不接受位置参数"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runCaptured(t, func() error { return runUpstreamCostCreate(tc.args) })
			if err == nil {
				t.Fatalf("期望报错，却通过了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息 %q 里没有 %q", err.Error(), tc.want)
			}
			if got := cli.ExitCode(err); got != cli.ExitUsage {
				t.Errorf("参数错的退出码应当是 %d，得到 %d", cli.ExitUsage, got)
			}
		})
	}
}

// TestUpstreamCostCreateRejectsBadInput 钉住写入路径挡下的那几类。
//
// 它们都要连库才报得出来（渠道外键、表达式编译），而报出来的仍然是**输入错**：
// 改一下命令重发就能过。
func TestUpstreamCostCreateRejectsBadInput(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	channelID, _ := createCostChannel(t)
	channelArg := strconv.FormatInt(channelID, 10)

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"表达式编译不过", []string{"--channel", channelArg, "--expr", `{"prompt": `}, ""},
		{"通配的模型名", []string{
			"--channel", channelArg, "--expr", testCostExpr, "--models", "claude-*",
		}, "通配符"},
		{"渠道不在库里", []string{
			"--channel", strconv.FormatInt(channelID+1_000_000, 10), "--expr", testCostExpr,
		}, "违反数据库约束"},
		{"上界早于下界", []string{
			"--channel", channelArg, "--expr", testCostExpr,
			"--effective-from", "2026-05-01T00:00:00Z", "--effective-to", "2026-04-01T00:00:00Z",
		}, "必须晚于 effective_from"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runCaptured(t, func() error { return runUpstreamCostCreate(tc.args) })
			if err == nil {
				t.Fatalf("期望被拒，却建成了")
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息 %q 里没有 %q", err.Error(), tc.want)
			}
		})
	}

	// 被拒的那几次一条都不该落库。
	if rules := listCostRules(t, db, channelID); len(rules) != 0 {
		t.Errorf("被拒的创建不该落库，却看到 %d 条规则", len(rules))
	}
}

// TestUpstreamCostCreateUnknownChannelIsConflict 钉住「渠道不在库里」给的是
// 约束冲突那一档，而不是「其他失败」：退出码表把外键算作冲突，下一步是去看
// 那条渠道在不在，不是改命令重发。
func TestUpstreamCostCreateUnknownChannelIsConflict(t *testing.T) {
	testDB(t)
	setupEnv(t)

	err := runCaptured(t, func() error {
		return runUpstreamCostCreate([]string{
			"--channel", "999999999", "--expr", testCostExpr,
		})
	})
	if err == nil {
		t.Fatal("渠道不在库里时应当被拒")
	}
	if got := cli.ExitCode(err); got != cli.ExitConflict {
		t.Errorf("外键冲突的退出码应当是 %d，得到 %d（%v）", cli.ExitConflict, got, err)
	}
}

// TestUpstreamCostCreateThenList 钉住录入与出口合成一条链路。
func TestUpstreamCostCreateThenList(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	channelID, model := createCostChannel(t)
	channelArg := strconv.FormatInt(channelID, 10)

	id := addCostRule(t, []string{
		"--channel", channelArg, "--expr", testCostExpr,
		"--models", model, "--priority", "7",
	})

	rules := listCostRules(t, db, channelID)
	if len(rules) != 1 {
		t.Fatalf("这条渠道应当只有一条规则，得到 %d 条", len(rules))
	}
	got := rules[0]
	if got.ID != id {
		t.Errorf("规则 ID = %d，期望 %d", got.ID, id)
	}
	if got.Priority != 7 {
		t.Errorf("优先级 = %d，期望 7", got.Priority)
	}
	if len(got.ModelPatterns) != 1 || got.ModelPatterns[0] != model {
		t.Errorf("模型清单 = %v，期望 [%q]", got.ModelPatterns, model)
	}
	if strings.Join(got.DeclaredKeys, ",") != "completion,prompt" {
		t.Errorf("声明的单价键 = %v，期望 [completion prompt]", got.DeclaredKeys)
	}

	out, _ := captureStreams(t, func() {
		if err := runUpstreamCostList([]string{"--channel", channelArg}); err != nil {
			t.Fatalf("list 失败: %v", err)
		}
	})
	for _, want := range []string{strconv.FormatInt(id, 10), channelArg, model, "7", "completion,prompt"} {
		if !strings.Contains(out, want) {
			t.Errorf("清单里没有 %q：\n%s", want, out)
		}
	}
	// 不带 --model 时没有裁决那一栏：没有模型就谈不上「哪条被选中」。
	if strings.Contains(out, "裁决") {
		t.Errorf("不带 --model 的清单不该有裁决那一栏：\n%s", out)
	}
}

// TestUpstreamCostListPicksWinner 钉住出口那一侧：这条渠道这个模型按哪条算。
//
// 三条规则同时适用于这个模型，优先级与模型名单各不相同，判据是按**字面值**
// 断言的：选中的必须是优先级最高的那条，而且它排在表的第一行。
func TestUpstreamCostListPicksWinner(t *testing.T) {
	testDB(t)
	setupEnv(t)
	channelID, model := createCostChannel(t)
	channelArg := strconv.FormatInt(channelID, 10)

	fallback := addCostRule(t, []string{
		"--channel", channelArg, "--expr", testCostExpr, "--priority", "1",
	})
	low := addCostRule(t, []string{
		"--channel", channelArg, "--expr", testCostExpr, "--models", model, "--priority", "2",
	})
	high := addCostRule(t, []string{
		"--channel", channelArg, "--expr", testCostExpr, "--models", model, "--priority", "5",
	})

	out, _ := captureStreams(t, func() {
		if err := runUpstreamCostList([]string{
			"--channel", channelArg, "--model", model,
		}); err != nil {
			t.Fatalf("list 失败: %v", err)
		}
	})

	if !selectedRuleIs(out, high) {
		t.Errorf("被选中的应当是优先级 5 的那条 %d：\n%s", high, out)
	}
	if got := firstCostRowID(t, out); got != high {
		t.Errorf("表的第一行是 %d，期望就是被选中的 %d：\n%s", got, high, out)
	}
	// 另外两条也要在表里：它们各自的落选原因不同（优先级低、兜底），
	// 而这张表要回答的正是「为什么不是它」。
	for _, id := range []int64{fallback, low} {
		if !strings.Contains(out, strconv.FormatInt(id, 10)) {
			t.Errorf("落选的那条 %d 也该出现在表里：\n%s", id, out)
		}
	}
	if strings.Count(out, "被压住") != 2 {
		t.Errorf("另外两条都该标「被压住」：\n%s", out)
	}
}

// TestUpstreamCostListSkipsExpiredRule 钉住「已过期的规则不被选中」。
//
// 过期的那条优先级更高，仍然输给还在生效的那条；只剩过期的那条时，输出说
// 「没有适用的成本规则」而不是把它算进来。
func TestUpstreamCostListSkipsExpiredRule(t *testing.T) {
	testDB(t)
	setupEnv(t)
	channelID, model := createCostChannel(t)
	channelArg := strconv.FormatInt(channelID, 10)

	expired := addCostRule(t, []string{
		"--channel", channelArg, "--expr", testCostExpr, "--models", model, "--priority", "99",
		"--effective-from", "2026-01-01T00:00:00Z", "--effective-to", "2026-02-01T00:00:00Z",
	})
	// 用过去的一刻问：那一刻过期的那条还没结束，它是被选中的那条。
	past := "2026-01-15T00:00:00Z"
	out, _ := captureStreams(t, func() {
		if err := runUpstreamCostList([]string{
			"--channel", channelArg, "--model", model, "--at", past,
		}); err != nil {
			t.Fatalf("list 失败: %v", err)
		}
	})
	if !selectedRuleIs(out, expired) {
		t.Errorf("问 2026-01-15 那一刻时被选中的应当是 %d：\n%s", expired, out)
	}

	// 问现在：它已经过期，于是没有适用的规则。这不是失败。
	err := runCaptured(t, func() error {
		return runUpstreamCostList([]string{"--channel", channelArg, "--model", model})
	})
	if err != nil {
		t.Fatalf("没有适用的成本规则不是失败，得到 %v", err)
	}
	now, _ := captureStreams(t, func() {
		if listErr := runUpstreamCostList([]string{"--channel", channelArg, "--model", model}); listErr != nil {
			t.Fatalf("list 失败: %v", listErr)
		}
	})

	// 逐行比，不按 ID 做子串匹配：过期那条的 ID 是个小整数，时刻栏与渠道栏里
	// 到处都是数字，子串匹配会凭空命中（这正是「按字面值断言」要挡的那种假绿）。
	lines := strings.Split(strings.TrimRight(now, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("没有适用的规则时只该有两行（问的是哪条渠道哪个模型 + 结论），得到 %d 行：\n%s",
			len(lines), now)
	}
	if want := fmt.Sprintf("渠道 %d，模型 %s，", channelID, model); !strings.HasPrefix(lines[0], want) {
		t.Errorf("第一行应当说清问的是哪条渠道哪个模型，得到 %q", lines[0])
	}
	if want := "没有适用的成本规则。"; lines[1] != want {
		t.Errorf("第二行 = %q，想要 %q——过期的那条不该出现在「适用」那一档里", lines[1], want)
	}

	// 反过来：它没有被删掉，只是不适用了——不带 --model 的那张清单照旧列它。
	all, _ := captureStreams(t, func() {
		if listErr := runUpstreamCostList([]string{"--channel", channelArg}); listErr != nil {
			t.Fatalf("list 失败: %v", listErr)
		}
	})
	if got := firstCostRowID(t, all); got != expired {
		t.Errorf("不带 --model 的清单第一行应当是 %d，得到 %d：\n%s", expired, got, all)
	}
}

// TestUpstreamCostListRejectsBadFlags 钉住 list 的参数校验。
func TestUpstreamCostListRejectsBadFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"--model 没有配 --channel", []string{"--model", "m"}, "要同时给 --channel"},
		{"--at 没有配 --model", []string{"--channel", "1", "--at", "2026-01-01T00:00:00Z"}, "--at 要同时给 --model"},
		{"--channel 是负数", []string{"--channel", "-1"}, "无效"},
		{"--at 不是 RFC3339", []string{"--channel", "1", "--model", "m", "--at", "2026-01-01"}, "RFC3339"},
		{"多出位置参数", []string{"--channel", "1", "extra"}, "不接受位置参数"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runCaptured(t, func() error { return runUpstreamCostList(tc.args) })
			if err == nil {
				t.Fatalf("期望报错，却通过了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息 %q 里没有 %q", err.Error(), tc.want)
			}
			if got := cli.ExitCode(err); got != cli.ExitUsage {
				t.Errorf("参数错的退出码应当是 %d，得到 %d", cli.ExitUsage, got)
			}
		})
	}
}

// TestUpstreamCostListEmptyNote 钉住空清单那一句：它要说清「这一步是空的」
// 与「下一步敲哪条命令」。
func TestUpstreamCostListEmptyNote(t *testing.T) {
	testDB(t)
	setupEnv(t)
	channelID, _ := createCostChannel(t)

	out, _ := captureStreams(t, func() {
		if err := runUpstreamCostList([]string{"--channel", strconv.FormatInt(channelID, 10)}); err != nil {
			t.Fatalf("list 失败: %v", err)
		}
	})
	if !strings.Contains(out, "暂无成本规则") || !strings.Contains(out, "upstream-cost create") {
		t.Errorf("空清单那一句没说清下一步：\n%s", out)
	}
}

// TestUpstreamCostUpdateChangesOnlyGivenFields 钉住「只改真的给了的那几项」。
func TestUpstreamCostUpdateChangesOnlyGivenFields(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	channelID, model := createCostChannel(t)
	channelArg := strconv.FormatInt(channelID, 10)

	id := addCostRule(t, []string{
		"--channel", channelArg, "--expr", testCostExpr, "--models", model, "--priority", "3",
	})

	_, stderr := captureStreams(t, func() {
		if err := runUpstreamCostUpdate([]string{
			"--id", strconv.FormatInt(id, 10), "--priority", "8",
		}); err != nil {
			t.Fatalf("update 失败: %v", err)
		}
	})
	if !strings.Contains(stderr, "已更新成本规则") {
		t.Errorf("回显里没有说清改了哪条：%q", stderr)
	}

	rules := listCostRules(t, db, channelID)
	if len(rules) != 1 {
		t.Fatalf("这条渠道应当只有一条规则，得到 %d 条", len(rules))
	}
	got := rules[0]
	if got.Priority != 8 {
		t.Errorf("优先级 = %d，期望 8", got.Priority)
	}
	// 没提到的两项保持库里的值。
	if len(got.ModelPatterns) != 1 || got.ModelPatterns[0] != model {
		t.Errorf("没提到的模型清单被改掉了：%v", got.ModelPatterns)
	}
	if got.Expr != testCostExpr {
		t.Errorf("没提到的表达式被改掉了：%q", got.Expr)
	}

	// 「不再过期」是一次真实动作，不是「不改这一项」。
	if err := runCaptured(t, func() error {
		return runUpstreamCostUpdate([]string{
			"--id", strconv.FormatInt(id, 10),
			"--effective-from", "2026-01-01T00:00:00Z",
			"--effective-to", "2026-02-01T00:00:00Z",
		})
	}); err != nil {
		t.Fatalf("先设一个上界: %v", err)
	}
	if err := runCaptured(t, func() error {
		return runUpstreamCostUpdate([]string{"--id", strconv.FormatInt(id, 10), "--clear-effective-to"})
	}); err != nil {
		t.Fatalf("清成不过期: %v", err)
	}
	if rules = listCostRules(t, db, channelID); rules[0].EffectiveTo != nil {
		t.Errorf("清过之后上界应当是空的，得到 %v", rules[0].EffectiveTo)
	}
}

// TestUpstreamCostUpdateRejectsBadFlags 钉住 update 的参数校验。
func TestUpstreamCostUpdateRejectsBadFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"没有 --id", []string{"--priority", "1"}, "缺少 --id"},
		{"什么都没改", []string{"--id", "1"}, "至少要改一项"},
		{"--clear-effective-to 与 --effective-to 同时给", []string{
			"--id", "1", "--clear-effective-to", "--effective-to", "2026-02-01T00:00:00Z",
		}, "只能给一个"},
		{"--expr 是空串", []string{"--id", "1", "--expr", "  "}, "--expr 不能为空"},
		{"--effective-from 是空串", []string{"--id", "1", "--effective-from", ""}, "不能为空"},
		{"--channel 是 0", []string{"--id", "1", "--channel", "0"}, "不能是 0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runCaptured(t, func() error { return runUpstreamCostUpdate(tc.args) })
			if err == nil {
				t.Fatalf("期望报错，却通过了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息 %q 里没有 %q", err.Error(), tc.want)
			}
			if got := cli.ExitCode(err); got != cli.ExitUsage {
				t.Errorf("参数错的退出码应当是 %d，得到 %d", cli.ExitUsage, got)
			}
		})
	}
}

// TestUpstreamCostUpdateUnknownIDIsConflict 钉住「要改的那一行不在」给的是
// 约束冲突那一档，与 price 同一条哨兵分类。
func TestUpstreamCostUpdateUnknownIDIsConflict(t *testing.T) {
	testDB(t)
	setupEnv(t)

	err := runCaptured(t, func() error {
		return runUpstreamCostUpdate([]string{"--id", "999999999", "--priority", "1"})
	})
	if err == nil {
		t.Fatal("id 不在库里时应当报错")
	}
	if !errors.Is(err, pricing.ErrCostNotFound) {
		t.Fatalf("期望 ErrCostNotFound，得到 %v", err)
	}
	if got := cli.ExitCode(err); got != cli.ExitConflict {
		t.Errorf("「那一行不在」的退出码应当是 %d，得到 %d", cli.ExitConflict, got)
	}
}

// TestUpstreamCostDisableClosesWindow 钉住停用：它把生效区间收在当下。
func TestUpstreamCostDisableClosesWindow(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	channelID, model := createCostChannel(t)
	channelArg := strconv.FormatInt(channelID, 10)

	id := addCostRule(t, []string{
		"--channel", channelArg, "--expr", testCostExpr, "--models", model,
	})

	before := time.Now()
	_, stderr := captureStreams(t, func() {
		if err := runUpstreamCostDisable([]string{strconv.FormatInt(id, 10)}); err != nil {
			t.Fatalf("disable 失败: %v", err)
		}
	})
	after := time.Now()
	if !strings.Contains(stderr, "已停用") {
		t.Errorf("回显里没有说清停用了哪条：%q", stderr)
	}

	rules := listCostRules(t, db, channelID)
	if len(rules) != 1 || rules[0].EffectiveTo == nil {
		t.Fatalf("停用之后应当有一个上界：%+v", rules)
	}
	to := *rules[0].EffectiveTo
	if to.Before(before) || to.After(after) {
		t.Errorf("收在当下应当落在 [%s, %s] 之间，得到 %s",
			before.Format(time.RFC3339), after.Format(time.RFC3339), to.Format(time.RFC3339))
	}

	// 停下之后问不出它了。
	out, _ := captureStreams(t, func() {
		if err := runUpstreamCostList([]string{"--channel", channelArg, "--model", model}); err != nil {
			t.Fatalf("list 失败: %v", err)
		}
	})
	if !strings.Contains(out, "没有适用的成本规则") {
		t.Errorf("停用之后不该再选中它：\n%s", out)
	}

	// 再停一次：它已经失效了，照做会把上界重新拉长到此刻。
	err := runCaptured(t, func() error {
		return runUpstreamCostDisable([]string{strconv.FormatInt(id, 10)})
	})
	if err == nil {
		t.Fatal("对已经失效的规则再 disable 一次应当被拒")
	}
	if !strings.Contains(err.Error(), "就已经失效") {
		t.Errorf("错误信息 %q 没有点出「已经失效」", err.Error())
	}
}

// TestUpstreamCostDisableRefusesPlannedRule 钉住「还没生效的规则停不了」。
//
// 照做会写出 effective_to <= effective_from 的空窗口，而同一个形状在 create
// 与 update 上是被拒的——同一个包里对「一行合法的生效区间」只能有一个判据。
func TestUpstreamCostDisableRefusesPlannedRule(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	channelID, model := createCostChannel(t)
	channelArg := strconv.FormatInt(channelID, 10)

	future := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	id := addCostRule(t, []string{
		"--channel", channelArg, "--expr", testCostExpr, "--models", model,
		"--effective-from", future,
	})

	err := runCaptured(t, func() error {
		return runUpstreamCostDisable([]string{strconv.FormatInt(id, 10)})
	})
	if err == nil {
		t.Fatal("还没生效的规则应当停不了")
	}
	if !strings.Contains(err.Error(), "才生效") {
		t.Errorf("错误信息 %q 没有点出「还没生效」", err.Error())
	}

	// 一个字都没改：上界还是空的。
	if rules := listCostRules(t, db, channelID); rules[0].EffectiveTo != nil {
		t.Errorf("被拒之后上界不该被写上，得到 %v", rules[0].EffectiveTo)
	}
}

// TestUpstreamCostDisableRejectsBadID 钉住位置参数那一层。
func TestUpstreamCostDisableRejectsBadID(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"没有给 ID", nil, "需要一个规则 ID"},
		{"给了两个", []string{"1", "2"}, "需要一个规则 ID"},
		{"不是数", []string{"abc"}, "必须是正整数"},
		{"不是正数", []string{"0"}, "必须是正整数"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runCaptured(t, func() error { return runUpstreamCostDisable(tc.args) })
			if err == nil {
				t.Fatalf("期望报错，却通过了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息 %q 里没有 %q", err.Error(), tc.want)
			}
			if got := cli.ExitCode(err); got != cli.ExitUsage {
				t.Errorf("参数错的退出码应当是 %d，得到 %d", cli.ExitUsage, got)
			}
		})
	}
}

// TestUpstreamCostListDocIsMachineReadable 钉住机器可读的那一档。
func TestUpstreamCostListDocIsMachineReadable(t *testing.T) {
	from := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	to := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	rules := []pricing.CostInfo{
		{
			ID: 41, ChannelID: 9, ModelPatterns: []string{"m1", "m2"}, Priority: 3,
			EffectiveFrom: from, EffectiveTo: &to,
			DeclaredKeys: []string{"prompt", "completion"},
		},
		{
			// 不过期、模型名单为空（兜底）、表达式编译不过的一条：表格那一档的
			// `-`、「不过期」与「（表达式编译不过）」三个渲染一个都不该进 JSON。
			ID: 42, ChannelID: 9, Priority: 0,
			EffectiveFrom: from, ExprErr: errors.New("编译失败：未知变量"),
		},
	}

	q := pricing.CostListQuery{ChannelID: 9, CanonicalModel: "m1", At: from}
	got := jsonRoundTrip(t, costListDoc(q, rules, 41))
	assertJSONField(t, got, "status", cli.JSONOK)
	assertJSONField(t, jsonObjectField(t, got, "summary"), "total", float64(2))

	query := jsonObjectField(t, got, "query")
	assertJSONField(t, query, "channel_id", float64(9))
	assertJSONField(t, query, "canonical_model", "m1")
	assertJSONField(t, query, "at", "2026-01-02T03:04:05Z")

	list := jsonEntries(t, got, "costs")
	if len(list) != 2 {
		t.Fatalf("规则数 = %d，期望 2", len(list))
	}
	first := list[0].(map[string]any)
	assertJSONField(t, first, "id", float64(41))
	assertJSONField(t, first, "channel_id", float64(9))
	assertJSONField(t, first, "model_patterns", []any{"m1", "m2"})
	assertJSONField(t, first, "priority", float64(3))
	assertJSONField(t, first, "effective_from", "2026-01-02T03:04:05Z")
	assertJSONField(t, first, "effective_to", "2026-06-01T00:00:00Z")
	// 单价键是那一份原始清单，不是 declaredCostKeysLabel 渲染出来的那一栏。
	assertJSONField(t, first, "quota_keys", []any{"prompt", "completion"})
	assertJSONField(t, first, "expr_error", nil)
	// 裁决是英文的两个词：脚本按它分派。
	assertJSONField(t, first, "verdict", "selected")

	second := list[1].(map[string]any)
	assertJSONField(t, second, "effective_to", nil)
	assertJSONField(t, second, "model_patterns", []any{})
	assertJSONField(t, second, "quota_keys", []any{})
	assertJSONField(t, second, "expr_error", "编译失败：未知变量")
	assertJSONField(t, second, "verdict", "shadowed")

	t.Run("没给模型时裁决是 null", func(t *testing.T) {
		plain := jsonRoundTrip(t, costListDoc(pricing.CostListQuery{ChannelID: 9}, rules, 0))
		assertJSONField(t, jsonObjectField(t, plain, "query"), "canonical_model", nil)
		assertJSONField(t, jsonObjectField(t, plain, "query"), "at", nil)
		entries := jsonEntries(t, plain, "costs")
		assertJSONField(t, entries[0].(map[string]any), "verdict", nil)
	})

	t.Run("空清单是空数组，不是 null", func(t *testing.T) {
		empty := jsonRoundTrip(t, costListDoc(pricing.CostListQuery{}, nil, 0))
		assertJSONField(t, jsonObjectField(t, empty, "summary"), "total", float64(0))
		if len(jsonEntries(t, empty, "costs")) != 0 {
			t.Error("空清单应当是空数组")
		}
	})
}

// TestUpstreamCostWriteCommandsRejectJSONBeforeDoingAnything 钉住写入类命令在
// json 档下当场被拒，而不是做完再报不支持。
func TestUpstreamCostWriteCommandsRejectJSONBeforeDoingAnything(t *testing.T) {
	withFormat(t, cli.FormatJSON)

	cases := []struct {
		command string
		run     func() error
	}{
		{"upstream-cost create", func() error { return runUpstreamCostCreate(nil) }},
		{"upstream-cost update", func() error { return runUpstreamCostUpdate(nil) }},
		{"upstream-cost disable", func() error { return runUpstreamCostDisable(nil) }},
	}

	for _, tc := range cases {
		t.Run(tc.command, func(t *testing.T) {
			err := runCaptured(t, tc.run)
			if err == nil {
				t.Fatalf("%s 在 json 模式下应当被拒", tc.command)
			}
			if !strings.Contains(err.Error(), tc.command+" 还不支持 --format json") {
				t.Fatalf("错误信息应当指出 %s 不支持 json，得到 %q", tc.command, err.Error())
			}
			if got := cli.ExitCode(err); got != cli.ExitUsage {
				t.Errorf("退出码 = %d，期望参数错的 %d", got, cli.ExitUsage)
			}
		})
	}
}

// decodeJSONDoc 解析命令打到 stdout 上的那一份 JSON。
//
// 与 jsonRoundTrip 分开：那个喂的是一份 map（测的是「这一档打出来的形状」），
// 这个读的是命令真的写出去的那串字节（测的是「它出得来、路上没被别的东西
// 混进去」）。
func decodeJSONDoc(t *testing.T, raw string) map[string]any {
	t.Helper()

	var doc map[string]any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("stdout 不是一份 JSON：%v\n%s", err, raw)
	}
	return doc
}

// TestUpstreamCostListJSONEndToEnd 钉住 json 那一档真的从命令里出得来。
func TestUpstreamCostListJSONEndToEnd(t *testing.T) {
	testDB(t)
	setupEnv(t)
	// 切档放在夹具之后：json 档下写类命令直接报错（jsonUnsupported），而渠道与
	// 规则都得先躺进库里（见 cmd/larkaxctl 的 withFormat 用法说明）。
	channelID, model := createCostChannel(t)
	channelArg := strconv.FormatInt(channelID, 10)

	id := addCostRule(t, []string{
		"--channel", channelArg, "--expr", testCostExpr, "--models", model,
	})

	withFormat(t, cli.FormatJSON)

	out, _ := captureStreams(t, func() {
		if err := runUpstreamCostList([]string{"--channel", channelArg, "--model", model}); err != nil {
			t.Fatalf("list 失败: %v", err)
		}
	})
	doc := decodeJSONDoc(t, out)
	assertJSONField(t, doc, "status", cli.JSONOK)
	entries := jsonEntries(t, doc, "costs")
	if len(entries) != 1 {
		t.Fatalf("规则数 = %d，期望 1：\n%s", len(entries), out)
	}
	first := entries[0].(map[string]any)
	assertJSONField(t, first, "id", float64(id))
	assertJSONField(t, first, "channel_id", float64(channelID))
	assertJSONField(t, first, "verdict", "selected")
}
