package main

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/cmd/larkaxctl/internal/cli"
	"github.com/sumwai/larkax/internal/ledger"
)

// 这一组用例管的是 --dry-run 的两条契约：
//
//  1. 干跑一条都不写：余额、账本行、订阅与提现单在它前后完全相同。
//  2. 干跑打出来的分录与真跑逐字相同：两边走同一条写路径与同一个渲染函数，
//     只有动词与尾注不同。
//
// 判据落在**库**上而不是回显上：回显写成「将入账」但真写进去了，与写成「未改动」
// 而其实什么都没写，在屏幕上一样好看。

// entryBlock 抽出回显里分录那一段。
//
// 干跑与真跑的表头不同（「将写入的分录：」与「分录：」），所以按后缀认；干跑的
// 尾注不是分录，跳过它——要比的正是两边共有的那几行。
func entryBlock(t *testing.T, out string) []string {
	t.Helper()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	start := -1
	for i, line := range lines {
		if strings.HasSuffix(line, "分录：") {
			start = i + 1
			break
		}
	}
	if start < 0 {
		t.Fatalf("回显里没有分录那一段：\n%s", out)
	}
	var block []string
	for _, line := range lines[start:] {
		if line == cli.DryRunNote {
			continue
		}
		block = append(block, line)
	}
	if len(block) == 0 {
		t.Fatalf("分录那一段是空的：\n%s", out)
	}
	return block
}

// assertLedgerMatchesEntries 核对库里那笔交易的分录都在预览里出现过。
//
// 光比「干跑与真跑的回显相同」不够：两份回显可能一起错（渲染函数改坏了，两边
// 一起改）。这一条落到库里的行上——科目与金额都必须在预览里找得到。
func assertLedgerMatchesEntries(t *testing.T, db *sql.DB, txnID ledger.TxnID, block []string) {
	t.Helper()
	rows := txnRows(t, db, txnID)
	if len(rows) == 0 {
		t.Fatalf("交易 %s 一条分录都没有", txnID)
	}
	for _, row := range rows {
		parts := strings.Split(row, "|")
		account, amount := parts[0], parts[1]
		magnitude := strings.TrimPrefix(amount, "-")
		found := false
		for _, line := range block {
			if strings.Contains(line, account) && strings.Contains(line, magnitude) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("库里的分录 %s = %s 没有出现在预览里：%v", account, amount, block)
		}
	}
}

// normalizePayoutIDs 把分录里的提现单号换成占位符。
//
// 干跑的那一行是在事务里插进去的，id 取自序列；事务回滚后这个号不会被真跑用到，
// 所以两边的 payout:<id> 必然差一个。除了这一个数，两边的分录逐字相同。
func normalizePayoutIDs(lines []string) []string {
	out := make([]string, len(lines))
	for i, line := range lines {
		if idx := strings.Index(line, "payout:"); idx >= 0 {
			rest := line[idx+len("payout:"):]
			end := strings.IndexAny(rest, " \t")
			if end < 0 {
				end = len(rest)
			}
			line = line[:idx+len("payout:")] + "<id>" + rest[end:]
		}
		out[i] = line
	}
	return out
}

// ── wallet credit / adjust / refund ─────────────────────────────────────

func TestWalletCreditDryRunKeepsLedgerAndMatchesRealRun(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	userID := createTestUser(t)
	user := itoa64(userID)
	mustRun(t, runWalletCreate, "--user", user)
	order := uniqueName("order")
	txnID := txnIDForOrder(order)

	dryOut := mustRun(t, runWalletCredit,
		"--user", user, "--amount", "100", "--ref", order, "--dry-run")
	assertHas(t, dryOut, "将入账", cli.DryRunNote)

	if balance := walletBalance(t, db, userID); balance != "0.00000000" {
		t.Errorf("干跑之后余额 = %s，期望 0.00000000", balance)
	}
	if rows := txnRows(t, db, txnID); len(rows) != 0 {
		t.Fatalf("干跑留下了 %d 条分录：%v", len(rows), rows)
	}

	realOut := mustRun(t, runWalletCredit,
		"--user", user, "--amount", "100", "--ref", order)
	assertHas(t, realOut, "已入账")

	if got, want := entryBlock(t, dryOut), entryBlock(t, realOut); !slices.Equal(got, want) {
		t.Errorf("预览的分录与真跑不同：\n预览 %v\n真跑 %v", got, want)
	}
	assertLedgerMatchesEntries(t, db, txnID, entryBlock(t, dryOut))
	if balance := walletBalance(t, db, userID); balance != "100.00000000" {
		t.Errorf("真跑之后余额 = %s，期望 100.00000000", balance)
	}
}

func TestWalletAdjustDryRunKeepsLedgerAndMatchesRealRun(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	userID := createTestUser(t)
	user := itoa64(userID)
	operator := createTestUserID(t)
	mustRun(t, runWalletCreate, "--user", user)
	mustRun(t, runWalletCredit, "--user", user, "--amount", "100", "--ref", uniqueName("order"))
	ref := uniqueName("adjust")
	txnID := txnIDForAdjustment(ref)

	dryOut := mustRun(t, runWalletAdjust,
		"--user", user, "--amount", "-40", "--memo", "误扣补回", "--ref", ref,
		"--by", operator, "--dry-run")
	assertHas(t, dryOut, "将调账用户", "余额", "干跑：以上内容没有写进数据库。")

	if balance := walletBalance(t, db, userID); balance != "100.00000000" {
		t.Errorf("干跑之后余额 = %s，期望 100.00000000", balance)
	}
	if rows := txnRows(t, db, txnID); len(rows) != 0 {
		t.Fatalf("干跑留下了 %d 条分录：%v", len(rows), rows)
	}

	realOut := mustRun(t, runWalletAdjust,
		"--user", user, "--amount", "-40", "--memo", "误扣补回", "--ref", ref, "--by", operator)
	assertHas(t, realOut, "已调账用户")

	if got, want := entryBlock(t, dryOut), entryBlock(t, realOut); !slices.Equal(got, want) {
		t.Errorf("预览的分录与真跑不同：\n预览 %v\n真跑 %v", got, want)
	}
	assertLedgerMatchesEntries(t, db, txnID, entryBlock(t, dryOut))
	if balance := walletBalance(t, db, userID); balance != "60.00000000" {
		t.Errorf("真跑之后余额 = %s，期望 60.00000000", balance)
	}
}

func TestWalletRefundDryRunKeepsLedgerAndMatchesRealRun(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	userID := createTestUser(t)
	user := itoa64(userID)
	mustRun(t, runWalletCreate, "--user", user)
	mustRun(t, runWalletCredit, "--user", user, "--amount", "50", "--ref", uniqueName("order"))
	ref := uniqueName("refund")
	txnID := txnIDForRefund(ref)

	dryOut := mustRun(t, runWalletRefund,
		"--user", user, "--amount", "20", "--ref", ref, "--memo", "请求失败", "--dry-run")
	assertHas(t, dryOut, "将退款给用户", "原因：请求失败", cli.DryRunNote)

	if balance := walletBalance(t, db, userID); balance != "50.00000000" {
		t.Errorf("干跑之后余额 = %s，期望 50.00000000", balance)
	}
	if rows := txnRows(t, db, txnID); len(rows) != 0 {
		t.Fatalf("干跑留下了 %d 条分录：%v", len(rows), rows)
	}

	realOut := mustRun(t, runWalletRefund,
		"--user", user, "--amount", "20", "--ref", ref, "--memo", "请求失败")
	assertHas(t, realOut, "已退款给用户")

	if got, want := entryBlock(t, dryOut), entryBlock(t, realOut); !slices.Equal(got, want) {
		t.Errorf("预览的分录与真跑不同：\n预览 %v\n真跑 %v", got, want)
	}
	assertLedgerMatchesEntries(t, db, txnID, entryBlock(t, dryOut))
	if balance := walletBalance(t, db, userID); balance != "70.00000000" {
		t.Errorf("真跑之后余额 = %s，期望 70.00000000", balance)
	}
}

// ── wallet payout ───────────────────────────────────────────────────────

func TestWalletPayoutRequestDryRunKeepsLedgerAndMatchesRealRun(t *testing.T) {
	setupEnv(t)
	db := testDB(t)
	merchant := createTestUserID(t)
	merchantID := mustUserID(t, merchant)
	fundMerchantPayable(t, db, merchantID, "12.5")
	ref := uniqueName("payout")
	payable := ledger.MerchantPayableAccount(merchantID)

	before := ledgerAccountTotal(t, db, payable.String())
	dryOut := mustRun(t, runWalletPayout, "request",
		"--user", merchant, "--amount", "12.5", "--ref", ref, "--dry-run")
	assertHas(t, dryOut, "将申请提现", ref, cli.DryRunNote)

	if got := ledgerAccountTotal(t, db, payable.String()); got != before {
		t.Errorf("干跑改动了分成科目：%s → %s", before, got)
	}
	var rows int
	if err := db.QueryRowContext(t.Context(),
		`SELECT count(*) FROM payout_requests WHERE merchant_user_id = $1`,
		merchantID).Scan(&rows); err != nil {
		t.Fatalf("数提现单失败: %v", err)
	}
	if rows != 0 {
		t.Fatalf("干跑留下了 %d 行提现单，期望 0 行", rows)
	}

	realOut := mustRun(t, runWalletPayout, "request",
		"--user", merchant, "--amount", "12.5", "--ref", ref)
	assertHas(t, realOut, "已申请提现")

	got := normalizePayoutIDs(entryBlock(t, dryOut))
	want := normalizePayoutIDs(entryBlock(t, realOut))
	if !slices.Equal(got, want) {
		t.Errorf("预览的分录与真跑不同（提现单号已归一）：\n预览 %v\n真跑 %v", got, want)
	}
}

func TestWalletPayoutSettleDryRunKeepsLedgerAndMatchesRealRun(t *testing.T) {
	setupEnv(t)
	db := testDB(t)
	merchant := createTestUserID(t)
	merchantID := mustUserID(t, merchant)
	operator := createTestUserID(t)
	fundMerchantPayable(t, db, merchantID, "20")
	ref := uniqueName("settle")
	payable := ledger.MerchantPayableAccount(merchantID)

	before := ledgerAccountTotal(t, db, payable.String())
	dryOut := mustRun(t, runWalletPayout, "settle",
		"--user", merchant, "--amount", "8", "--ref", ref,
		"--destination", `{"bank":"placeholder"}`, "--by", operator, "--dry-run")
	assertHas(t, dryOut, "将清算提现单", cli.DryRunNote)

	if got := ledgerAccountTotal(t, db, payable.String()); got != before {
		t.Errorf("干跑改动了分成科目：%s → %s", before, got)
	}
	var rows int
	if err := db.QueryRowContext(t.Context(),
		`SELECT count(*) FROM payout_requests WHERE merchant_user_id = $1`,
		merchantID).Scan(&rows); err != nil {
		t.Fatalf("数提现单失败: %v", err)
	}
	if rows != 0 {
		t.Fatalf("干跑留下了 %d 行提现单，期望 0 行", rows)
	}

	realOut := mustRun(t, runWalletPayout, "settle",
		"--user", merchant, "--amount", "8", "--ref", ref,
		"--destination", `{"bank":"placeholder"}`, "--by", operator)
	assertHas(t, realOut, "已清算提现单")

	got := normalizePayoutIDs(entryBlock(t, dryOut))
	want := normalizePayoutIDs(entryBlock(t, realOut))
	if !slices.Equal(got, want) {
		t.Errorf("预览的分录与真跑不同（提现单号已归一）：\n预览 %v\n真跑 %v", got, want)
	}
}

// ── wallet payment ──────────────────────────────────────────────────────

func TestWalletPaymentCreateDryRunWritesNothing(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	setupEnv(t)
	user := createTestUserID(t)
	mustRun(t, runWalletCreate, "--user", user)
	setPaymentFake(t, ctx, db, true)
	t.Cleanup(func() { setPaymentFake(t, ctx, db, false) })
	order := uniqueName("ORD")

	out := mustRun(t, runPaymentCreate,
		"--user", user, "--amount", "66.00", "--ref", order, "--dry-run")
	assertHas(t, out, "将下单", order, "66.00000000", "created（待支付）",
		"干跑：没有向通道下单，也没有写入数据库。")
	if strings.Contains(out, "支付地址") {
		t.Errorf("干跑不该有支付地址：\n%s", out)
	}

	var rows int
	if err := db.QueryRowContext(t.Context(),
		`SELECT count(*) FROM payment_orders WHERE order_no = $1`, order).Scan(&rows); err != nil {
		t.Fatalf("数支付单失败: %v", err)
	}
	if rows != 0 {
		t.Fatalf("干跑留下了 %d 行支付单，期望 0 行", rows)
	}

	// 真跑同一个单号：没有被预览占住，能正常下单。
	realOut := mustRun(t, runPaymentCreate,
		"--user", user, "--amount", "66.00", "--ref", order)
	assertHas(t, realOut, "已下单", order)
	if err := db.QueryRowContext(t.Context(),
		`SELECT count(*) FROM payment_orders WHERE order_no = $1`, order).Scan(&rows); err != nil {
		t.Fatalf("数支付单失败: %v", err)
	}
	if rows != 1 {
		t.Errorf("真跑之后库里 %d 行支付单，期望 1 行", rows)
	}
}

// ── plan grant / void ───────────────────────────────────────────────────

func TestPlanGrantDryRunWritesNothingAndMatchesRealRun(t *testing.T) {
	db := testDB(t)
	user := createTestUserID(t)
	planKey := uniqueName("dryrun-plan")
	_, stderr := mustStream(t, func() error {
		return runPlan([]string{
			"create", "--key", planKey, "--name", "干跑额度包",
			"--unit", "amount", "--quota", "5",
		})
	})
	if !strings.Contains(stderr, "已建额度包 "+planKey) {
		t.Fatalf("建包失败：%q", stderr)
	}

	ref := uniqueName("grant")
	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	args := []string{"grant", "--user", user, "--plan", planKey, "--ref", ref, "--expires-at", expires}

	_, dryErr := mustStream(t, func() error {
		return runPlan(append(append([]string{}, args...), "--dry-run"))
	})
	assertHas(t, dryErr, "将发放订阅", planKey)

	var rows int
	if err := db.QueryRowContext(t.Context(),
		`SELECT count(*) FROM user_plans WHERE ref = $1`, ref).Scan(&rows); err != nil {
		t.Fatalf("数订阅失败: %v", err)
	}
	if rows != 0 {
		t.Fatalf("干跑留下了 %d 行订阅，期望 0 行", rows)
	}

	_, realErr := mustStream(t, func() error { return runPlan(args) })
	assertHas(t, realErr, "已发放订阅", planKey)

	// 订阅 id 是序列号：干跑用掉一个之后真跑会拿到下一个，所以先把那句里的号
	// 归一，再把动词从「将发放」换成「已发放」，其余部分逐字比。
	dryText := strings.Replace(dryErr, "将发放", "已发放", 1)
	if got, want := stripSubscriptionID(t, dryText), stripSubscriptionID(t, realErr); got != want {
		t.Errorf("预览与真跑的发放回显不同（订阅号已归一）：\n预览 %q\n真跑 %q", got, want)
	}

	var upID int64
	if err := db.QueryRowContext(t.Context(),
		`SELECT id FROM user_plans WHERE ref = $1`, ref).Scan(&upID); err != nil {
		t.Fatalf("读订阅 id 失败: %v", err)
	}
	if got := ledgerAccountTotal(t, db, ledger.PrepaidAccount(upID).String()); got != "5.00000000" {
		t.Errorf("发放之后 prepaid 余额 = %s，期望 5.00000000", got)
	}
}

func TestPlanVoidDryRunWritesNothingAndMatchesRealRun(t *testing.T) {
	db := testDB(t)
	user := createTestUserID(t)
	planKey := uniqueName("dryrun-void-plan")
	_, stderr := mustStream(t, func() error {
		return runPlan([]string{
			"create", "--key", planKey, "--name", "干跑作废包",
			"--unit", "amount", "--quota", "7",
		})
	})
	if !strings.Contains(stderr, "已建额度包 "+planKey) {
		t.Fatalf("建包失败：%q", stderr)
	}

	grantRef := uniqueName("grant")
	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	_, grantErr := mustStream(t, func() error {
		return runPlan([]string{
			"grant", "--user", user, "--plan", planKey, "--ref", grantRef, "--expires-at", expires,
		})
	})
	assertHas(t, grantErr, "已发放订阅")

	var upID int64
	if err := db.QueryRowContext(t.Context(),
		`SELECT id FROM user_plans WHERE ref = $1`, grantRef).Scan(&upID); err != nil {
		t.Fatalf("读订阅 id 失败: %v", err)
	}
	prepaid := ledger.PrepaidAccount(upID).String()
	before := ledgerAccountTotal(t, db, prepaid)

	voidRef := uniqueName("void")
	idArg := itoa64(upID)
	_, dryErr := mustStream(t, func() error {
		return runPlan([]string{"void", "--id", idArg, "--ref", voidRef, "--dry-run"})
	})
	assertHas(t, dryErr, "将作废订阅")

	var status string
	if err := db.QueryRowContext(t.Context(),
		`SELECT status FROM user_plans WHERE id = $1`, upID).Scan(&status); err != nil {
		t.Fatalf("读订阅状态失败: %v", err)
	}
	if status != "active" {
		t.Errorf("干跑之后订阅状态 = %q，期望 active", status)
	}
	if got := ledgerAccountTotal(t, db, prepaid); got != before {
		t.Errorf("干跑改动了 prepaid 科目：%s → %s", before, got)
	}

	_, realErr := mustStream(t, func() error {
		return runPlan([]string{"void", "--id", idArg, "--ref", voidRef})
	})
	assertHas(t, realErr, "已作废订阅")
	if got, want := strings.Replace(dryErr, "将作废", "已作废", 1), realErr; got != want {
		t.Errorf("预览与真跑的作废回显不同：\n预览 %q\n真跑 %q", got, want)
	}
	if err := db.QueryRowContext(t.Context(),
		`SELECT status FROM user_plans WHERE id = $1`, upID).Scan(&status); err != nil {
		t.Fatalf("读订阅状态失败: %v", err)
	}
	if status != "canceled" {
		t.Errorf("真跑之后订阅状态 = %q，期望 canceled", status)
	}
}

// stripSubscriptionID 把「订阅 <id>」里的号换掉，供预览与真跑逐字比对。
func stripSubscriptionID(t *testing.T, text string) string {
	t.Helper()
	const marker = "订阅 "
	idx := strings.Index(text, marker)
	if idx < 0 {
		t.Fatalf("回显里没有「订阅 <id>」：%q", text)
	}
	rest := text[idx+len(marker):]
	end := strings.IndexAny(rest, "（，")
	if end < 0 {
		t.Fatalf("回显里的订阅号没有结束符：%q", text)
	}
	return text[:idx+len(marker)] + "<id>" + rest[end:]
}

// ── 参数错在干跑下同样报错、同样退出码 ─────────────────────────────────

// TestDryRunKeepsArgumentErrors 断言 --dry-run 不比真跑宽松。
//
// 每个用例把同一组参数跑两遍（带与不带 --dry-run），比的是**错误原文与退出码**：
// 预览放过了真跑会拒的那一格，等于把一次一定会失败的操作说成能行。
func TestDryRunKeepsArgumentErrors(t *testing.T) {
	cases := []struct {
		name string
		run  func([]string) error
		args []string
		want string
	}{
		{"credit 缺 --ref", runWalletCredit,
			[]string{"--user", "1", "--amount", "1"}, "缺少 --ref"},
		{"adjust 缺 --memo", runWalletAdjust,
			[]string{"--user", "1", "--amount", "-1", "--ref", "R", "--by", "1"}, "缺少 --memo"},
		{"refund 金额不是数字", runWalletRefund,
			[]string{"--user", "1", "--amount", "abc", "--ref", "R"}, "不是金额"},
		{"payout request 缺 --ref", runWalletPayout,
			[]string{"request", "--user", "1", "--amount", "1"}, "缺少 --ref"},
		{"payout settle 缺 --destination", runWalletPayout,
			[]string{"settle", "--user", "1", "--amount", "1", "--ref", "R", "--by", "1"}, "缺少 --destination"},
		{"payment create 缺 --ref", runPaymentCreate,
			[]string{"--user", "1", "--amount", "1"}, "缺少 --ref"},
		{"plan grant 没给到期时间", runPlan,
			[]string{"grant", "--user", "1", "--plan", "P", "--ref", "R"}, "缺少过期时间"},
		{"plan void 缺 --ref", runPlan,
			[]string{"void", "--id", "1"}, "缺少 --ref"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plain := runCaptured(t, func() error { return tc.run(tc.args) })
			if plain == nil {
				t.Fatalf("不带 --dry-run 就没有报错，这条用例的前提不成立")
			}
			if !strings.Contains(plain.Error(), tc.want) {
				t.Fatalf("错误里没有 %q：%v", tc.want, plain)
			}

			dry := runCaptured(t, func() error {
				return tc.run(append(append([]string{}, tc.args...), "--dry-run"))
			})
			if dry == nil {
				t.Fatal("干跑放过了真跑会拒的参数")
			}
			if dry.Error() != plain.Error() {
				t.Errorf("两边的错误原文不同：\n不带 --dry-run %q\n带 --dry-run   %q",
					plain.Error(), dry.Error())
			}
			if got, want := cli.ExitCode(dry), cli.ExitCode(plain); got != want {
				t.Errorf("退出码 = %d，期望 %d（与不带 --dry-run 相同）", got, want)
			}
		})
	}
}

// TestReadOnlyCommandsRejectDryRun 钉住「只读命令没有这个旗标」。
//
// 给读命令也接上 --dry-run 是在说「它可能会写」，而那会让真正需要预览的人分不清
// 哪几条命令有副作用。判据取「解析阶段就拒」，所以不需要库。
func TestReadOnlyCommandsRejectDryRun(t *testing.T) {
	cases := []struct {
		name string
		run  func([]string) error
	}{
		{"wallet list", runWalletList},
		{"wallet payment status", runWalletPayment},
		{"plan list", runPlan},
		{"plan subscriptions", runPlan},
		{"user wallet show", runUserWallet},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string][]string{
				"wallet list":           {"--dry-run"},
				"wallet payment status": {"status", "--order", "ORD-1", "--dry-run"},
				"plan list":             {"list", "--dry-run"},
				"plan subscriptions":    {"subscriptions", "--user", "1", "--dry-run"},
				"user wallet show":      {"show", "1", "--dry-run"},
			}[tc.name]
			err := runCaptured(t, func() error { return tc.run(args) })
			if err == nil {
				t.Fatal("只读命令接受了 --dry-run")
			}
			if !strings.Contains(err.Error(), "dry-run") {
				t.Errorf("错误里没点出 --dry-run：%v", err)
			}
			if got := cli.ExitCode(err); got != cli.ExitUsage {
				t.Errorf("退出码 = %d，期望 %d（参数错）", got, cli.ExitUsage)
			}
		})
	}
}
