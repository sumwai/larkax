package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/catalog"
	"github.com/sumwai/larkax/internal/channels"
	"github.com/sumwai/larkax/internal/pricing"
)

// createOnboardChannel 建一条可用于上架的渠道，返回它的 ID。
//
// 渠道的 models 白名单先放一个别的名字：用例要断言「向导把规范名并进去、
// 不覆盖原有项」，白名单一开始就非空才测得出来。
func createOnboardChannel(t *testing.T) int64 {
	t.Helper()

	withStdin(t, testSecret)
	out, _ := captureStreams(t, func() {
		if err := runChannelAdd([]string{
			"--name", uniqueName("onboard-channel"), "--type", testChanType,
			"--base-url", testBaseURL, "--models", uniqueName("seed-model"),
			"--credential-stdin",
		}); err != nil {
			t.Fatalf("channel create 失败: %v", err)
		}
	})
	id, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		t.Fatalf("stdout 不是渠道 ID：%q（%v）", out, err)
	}
	return id
}

// runOnboard 跑一次向导，返回 stdout、stderr 与错误。
func runOnboard(t *testing.T, args ...string) (string, string, error) {
	t.Helper()

	var runErr error
	out, errOut := captureStreams(t, func() { runErr = runModelOnboard(args) })
	return out, errOut, runErr
}

// onboardArgs 拼出一次完整的上架参数。
func onboardArgs(canonical string, channelID int64, upstream string) []string {
	return []string{
		canonical,
		"--channel", strconv.FormatInt(channelID, 10),
		"--upstream", upstream,
		"--price-expr", testExpr,
		"--cost-expr", testCostExpr,
	}
}

func channelModels(t *testing.T, db *sql.DB, channelID int64) []string {
	t.Helper()

	info, err := channels.NewAdmin(db, nil, nil).Get(context.Background(), channelID)
	if err != nil {
		t.Fatalf("读渠道 %d 失败: %v", channelID, err)
	}
	return info.Models
}

// priceRulesFor 返回这个渠道上覆盖 canonical 的计价规则。
func priceRulesFor(t *testing.T, db *sql.DB, channelID int64, canonical string) []pricing.RuleInfo {
	t.Helper()

	rules, err := pricing.NewAdmin(db).List(context.Background())
	if err != nil {
		t.Fatalf("列举计价规则失败: %v", err)
	}
	out := make([]pricing.RuleInfo, 0, 1)
	for _, rule := range rules {
		if rule.Scope == pricing.ScopeChannel && rule.ChannelID == channelID &&
			slices.Contains(rule.ModelPatterns, canonical) {
			out = append(out, rule)
		}
	}
	return out
}

// costRulesFor 返回这个渠道上覆盖 canonical 的成本规则。
func costRulesFor(t *testing.T, db *sql.DB, channelID int64, canonical string) []pricing.CostInfo {
	t.Helper()

	rules, err := pricing.NewCostAdmin(db).List(context.Background(), pricing.CostListQuery{ChannelID: channelID})
	if err != nil {
		t.Fatalf("列举成本规则失败: %v", err)
	}
	out := make([]pricing.CostInfo, 0, 1)
	for _, rule := range rules {
		if slices.Contains(rule.ModelPatterns, canonical) {
			out = append(out, rule)
		}
	}
	return out
}

// TestModelOnboardWritesFivePlaces 是一条正向用例：五处都落库，且每一处都
// 按这单自己的名字查（共享库上断言整张表为空是错的）。
func TestModelOnboardWritesFivePlaces(t *testing.T) {
	setupEnv(t)
	db := testDB(t)
	channelID := createOnboardChannel(t)
	canonical := uniqueName("onboard-model")
	upstream := canonical + "-upstream"
	before := channelModels(t, db, channelID)

	out, _, err := runOnboard(t, onboardArgs(canonical, channelID, upstream)...)
	if err != nil {
		t.Fatalf("onboard 失败: %v", err)
	}
	if !strings.Contains(out, "模型目录：新建") || !strings.Contains(out, "成本规则：新建") {
		t.Fatalf("输出没有逐步报告新建：%q", out)
	}

	model, err := catalog.New(db).Model(context.Background(), canonical)
	if err != nil {
		t.Fatalf("模型目录里没有 %q: %v", canonical, err)
	}
	if model.DisplayName != canonical {
		t.Fatalf("没给 --display 时展示名应回落成规范名，得到 %q", model.DisplayName)
	}

	models := channelModels(t, db, channelID)
	if !slices.Contains(models, canonical) {
		t.Fatalf("渠道 %d 的 models 里没有 %q：%v", channelID, canonical, models)
	}
	for _, kept := range before {
		if !slices.Contains(models, kept) {
			t.Fatalf("原有模型 %q 被覆盖掉了：%v", kept, models)
		}
	}

	aliases, err := catalog.New(db).Aliases(context.Background(), channelID)
	if err != nil {
		t.Fatalf("读别名失败: %v", err)
	}
	found := false
	for _, alias := range aliases {
		if alias.CanonicalName == canonical {
			found = true
			if alias.UpstreamModel != upstream {
				t.Fatalf("上游名是 %q，期望 %q", alias.UpstreamModel, upstream)
			}
		}
	}
	if !found {
		t.Fatalf("渠道 %d 上没有 %q 的出站映射：%v", channelID, canonical, aliases)
	}

	if rules := priceRulesFor(t, db, channelID, canonical); len(rules) != 1 {
		t.Fatalf("覆盖 %q 的计价规则应有 1 条，得到 %d 条", canonical, len(rules))
	}
	if rules := costRulesFor(t, db, channelID, canonical); len(rules) != 1 {
		t.Fatalf("覆盖 %q 的成本规则应有 1 条，得到 %d 条", canonical, len(rules))
	}
}

// TestModelOnboardRollsBackWhenCostRuleFails 让最后一步失败，断言前四处一条
// 都没留下。
//
// 这条用例是事务边界的判据：把 NewAdminInTx 换回 NewAdmin(db)（即「外面开了
// 事务、里面各写各的」）时，前四处会各自提交，这里查表就会看到它们。
func TestModelOnboardRollsBackWhenCostRuleFails(t *testing.T) {
	setupEnv(t)
	db := testDB(t)
	channelID := createOnboardChannel(t)
	canonical := uniqueName("onboard-rollback")
	before := channelModels(t, db, channelID)

	args := onboardArgs(canonical, channelID, canonical+"-upstream")
	// 成本表达式故意编译不过：此时模型、渠道白名单、映射、计价规则都已经在
	// 同一个事务里写过了。
	args[len(args)-1] = "{"
	if _, _, err := runOnboard(t, args...); err == nil {
		t.Fatal("成本表达式编译不过，onboard 却成功了")
	}

	if _, err := catalog.New(db).Model(context.Background(), canonical); !errors.Is(err, catalog.ErrModelNotFound) {
		t.Fatalf("回滚后模型目录里仍有 %q（err=%v）", canonical, err)
	}
	after := channelModels(t, db, channelID)
	if !slices.Equal(before, after) {
		t.Fatalf("回滚后渠道 models 变了：之前 %v，之后 %v", before, after)
	}
	aliases, aliasErr := catalog.New(db).Aliases(context.Background(), channelID)
	if aliasErr != nil {
		t.Fatalf("读别名失败: %v", aliasErr)
	}
	if len(aliases) != 0 {
		t.Fatalf("回滚后还有出站映射：%v", aliases)
	}
	prices := priceRulesFor(t, db, channelID, canonical)
	if len(prices) != 0 {
		t.Fatalf("回滚后还有计价规则：%v", prices)
	}
	costs := costRulesFor(t, db, channelID, canonical)
	if len(costs) != 0 {
		t.Fatalf("回滚后还有成本规则：%v", costs)
	}
	t.Logf("回滚后四张表：model_catalog 查不到 %q；channels.models=%v；model_aliases=%d 条；pricing_rules=%d 条；upstream_costs=%d 条",
		canonical, after, len(aliases), len(prices), len(costs))
}

// TestModelOnboardRejectsExistingUnlessUpdate 覆盖「默认报错 / --update 覆盖 /
// 同一条命令重复跑报错稳定」三件事。
func TestModelOnboardRejectsExistingUnlessUpdate(t *testing.T) {
	setupEnv(t)
	db := testDB(t)
	channelID := createOnboardChannel(t)
	canonical := uniqueName("onboard-existing")
	args := onboardArgs(canonical, channelID, canonical+"-upstream")

	if _, _, err := runOnboard(t, args...); err != nil {
		t.Fatalf("首次 onboard 失败: %v", err)
	}

	_, _, firstErr := runOnboard(t, args...)
	if !errors.Is(firstErr, errOnboardExists) {
		t.Fatalf("重复跑应报 errOnboardExists，得到 %v", firstErr)
	}
	if got := exitCode(firstErr); got != exitConflict {
		t.Fatalf("「已存在」应退出 %d，得到 %d", exitConflict, got)
	}
	_, _, secondErr := runOnboard(t, args...)
	if secondErr == nil || secondErr.Error() != firstErr.Error() {
		t.Fatalf("同一条命令重复跑的报错不稳定：%v / %v", firstErr, secondErr)
	}
	t.Logf("重复跑报错（两次逐字相同，退出码 %d）：%v", exitCode(firstErr), firstErr)

	const newPriceExpr = `{"prompt": 0.0000020, "completion": 0.0000100}`
	const newCostExpr = `{"prompt": 0.0000010, "completion": 0.0000050}`
	updateArgs := slices.Clone(args)
	for i, arg := range updateArgs {
		switch arg {
		case testExpr:
			updateArgs[i] = newPriceExpr
		case testCostExpr:
			updateArgs[i] = newCostExpr
		}
	}
	updateArgs = append(updateArgs, "--update", "--display", "新展示名")
	out, _, err := runOnboard(t, updateArgs...)
	if err != nil {
		t.Fatalf("--update 失败: %v", err)
	}
	if !strings.Contains(out, "更新展示名") || !strings.Contains(out, "计价规则：更新") {
		t.Fatalf("--update 的输出没说清更新了什么：%q", out)
	}
	t.Logf("--update 输出：\n%s", out)

	model, err := catalog.New(db).Model(context.Background(), canonical)
	if err != nil {
		t.Fatalf("读模型失败: %v", err)
	}
	if model.DisplayName != "新展示名" {
		t.Fatalf("--update 没有改展示名，得到 %q", model.DisplayName)
	}
	rules := priceRulesFor(t, db, channelID, canonical)
	if len(rules) != 1 || rules[0].Expr != newPriceExpr {
		t.Fatalf("计价规则没有更新成新表达式：%v", rules)
	}
	costs := costRulesFor(t, db, channelID, canonical)
	if len(costs) != 1 || costs[0].Expr != newCostExpr {
		t.Fatalf("成本规则没有更新成新表达式：%v", costs)
	}
}

// TestModelOnboardDryRunWritesNothing 比对干跑前后五张表在这个名字/渠道上的
// 全部行：模型目录、渠道 models、出站映射、计价规则、成本规则。
func TestModelOnboardDryRunWritesNothing(t *testing.T) {
	setupEnv(t)
	db := testDB(t)
	channelID := createOnboardChannel(t)
	canonical := uniqueName("onboard-dryrun")

	before := onboardSnapshot(t, db, canonical, channelID)
	args := append(onboardArgs(canonical, channelID, canonical+"-upstream"), "--dry-run")
	out, _, err := runOnboard(t, args...)
	if err != nil {
		t.Fatalf("干跑失败: %v", err)
	}
	// 断共用常量而不是抄一句字面量：尾注只有 dryrun.go 那一份，抄一遍会在
	// 改文案时静默失配。
	if !strings.Contains(out, dryRunNote) {
		t.Fatalf("干跑输出没有写尾注 %q：%q", dryRunNote, out)
	}
	if after := onboardSnapshot(t, db, canonical, channelID); before != after {
		t.Fatalf("干跑改动了库：\n之前 %s\n之后 %s", before, after)
	}
	t.Logf("干跑前后五张表快照一致：%s", before)
}

// TestModelOnboardRefusesAmbiguousPriceRule：同一个渠道 + 模型上有两条计价
// 规则时，向导不猜要改哪一条，报错里列出两个候选 ID。
func TestModelOnboardRefusesAmbiguousPriceRule(t *testing.T) {
	setupEnv(t)
	db := testDB(t)
	channelID := createOnboardChannel(t)
	canonical := uniqueName("onboard-ambiguous")
	ctx := context.Background()

	var ids []string
	for i := 0; i < 2; i++ {
		id, err := pricing.NewAdmin(db).Create(ctx, pricing.CreateRequest{
			Scope:         pricing.ScopeChannel,
			ChannelID:     channelID,
			Name:          fmt.Sprintf("ambiguous-%s-%d", canonical, i),
			ModelPatterns: []string{canonical},
			Expr:          testExpr,
		})
		if err != nil {
			t.Fatalf("造第二条计价规则失败: %v", err)
		}
		ids = append(ids, strconv.FormatInt(id, 10))
	}

	args := append(onboardArgs(canonical, channelID, canonical+"-upstream"), "--update")
	_, _, err := runOnboard(t, args...)
	if err == nil {
		t.Fatal("两条候选规则时 onboard 应该报错")
	}
	for _, id := range ids {
		if !strings.Contains(err.Error(), id) {
			t.Fatalf("报错里没有列出候选 id %s：%v", id, err)
		}
	}
}

// onboardSnapshot 把五张表里与这单相关的状态拼成一个可比较的串。
func onboardSnapshot(t *testing.T, db *sql.DB, canonical string, channelID int64) string {
	t.Helper()

	ctx := context.Background()
	var b strings.Builder

	if _, err := catalog.New(db).Model(ctx, canonical); err == nil {
		b.WriteString("model=yes;")
	} else {
		b.WriteString("model=no;")
	}
	info, err := channels.NewAdmin(db, nil, nil).Get(ctx, channelID)
	if err != nil {
		t.Fatalf("读渠道失败: %v", err)
	}
	fmt.Fprintf(&b, "channel=%v;", info.Models)

	aliases, err := catalog.New(db).Aliases(ctx, channelID)
	if err != nil {
		t.Fatalf("读别名失败: %v", err)
	}
	for _, alias := range aliases {
		fmt.Fprintf(&b, "alias=%d:%s:%s;", alias.ID, alias.CanonicalName, alias.UpstreamModel)
	}

	rules, err := pricing.NewAdmin(db).List(ctx)
	if err != nil {
		t.Fatalf("列举计价规则失败: %v", err)
	}
	for _, rule := range rules {
		if rule.ChannelID == channelID {
			fmt.Fprintf(&b, "price=%d:%s:%v;", rule.ID, rule.Expr, rule.ModelPatterns)
		}
	}

	costs, err := pricing.NewCostAdmin(db).List(ctx, pricing.CostListQuery{ChannelID: channelID})
	if err != nil {
		t.Fatalf("列举成本规则失败: %v", err)
	}
	for _, rule := range costs {
		fmt.Fprintf(&b, "cost=%d:%s:%v;", rule.ID, rule.Expr, rule.ModelPatterns)
	}

	return b.String()
}
