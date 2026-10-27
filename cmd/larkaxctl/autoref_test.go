package main

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/cmd/larkaxctl/internal/cli"
)

// 这一组管的是 --auto-ref 的三条契约：
//
//  1. 生成的单号形状固定（前缀、UTC 日期、随机段、长度），不依赖数据库，
//     也不依赖执行者所在时区。
//  2. 只给 --auto-ref 才生成；两个都不给、两个一起给都是参数错。
//  3. 生成出来的单号会打进输出，--dry-run 预览的那一个与拿它真跑出来的是同一个。
//
// 判据落在命令的完整路径上（解析旗标 → 账本 → 落库 → 回显），因为「默认不生成」
// 这条取舍只在整条命令上看得出来：单看生成器，它当然不会自己跑。

// autoRefPattern 是生成出来的单号该有的样子：MANUAL_<UTC 日期>_<12 位小写十六进制>。
var autoRefPattern = regexp.MustCompile(`^MANUAL_\d{8}_[0-9a-f]{12}$`)

// TestNewAutoRefShape 钉住单号的形状与 28 个字符的长度。
func TestNewAutoRefShape(t *testing.T) {
	// 本地时间是 10 月 24 日凌晨 2 点，UTC 还是 10 月 23 日：日期段必须取 UTC，
	// 否则同一个时刻在不同时区的机器上会生成两个日期。
	at := time.Date(2026, 10, 24, 2, 0, 0, 0, time.FixedZone("UTC+8", 8*60*60))
	ref, err := cli.NewAutoRef(at)
	if err != nil {
		t.Fatalf("生成自动单号失败: %v", err)
	}
	const wantPrefix = "MANUAL_20261023_"
	if !strings.HasPrefix(ref, wantPrefix) {
		t.Errorf("自动单号 = %q，期望以 %q 开头（日期按 UTC 算）", ref, wantPrefix)
	}
	if len(ref) != 28 {
		t.Errorf("自动单号 %q 长 %d 个字符，期望 28（MANUAL_ + 8 位日期 + _ + 12 位随机段）",
			ref, len(ref))
	}
}

// TestNewAutoRefCharsetAndVariation 扫一批生成结果：字符集、长度恒定，且随机段
// 真的在变。
//
// 「真的在变」只用两个样本判会偶发误报，所以取一批看有多少种不同的值——随机段
// 被写成常量时这里会只剩一种。
func TestNewAutoRefCharsetAndVariation(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	seen := make(map[string]bool)
	for i := 0; i < 256; i++ {
		ref, err := cli.NewAutoRef(now)
		if err != nil {
			t.Fatalf("第 %d 次生成失败: %v", i, err)
		}
		if !autoRefPattern.MatchString(ref) {
			t.Fatalf("自动单号 %q 不满足 %s", ref, autoRefPattern)
		}
		seen[ref] = true
	}
	if len(seen) < 2 {
		t.Errorf("256 次生成只得到 %d 种单号：随机段是固定的", len(seen))
	}
}

// TestResolveOrderRefChoices 钉住「--ref / --auto-ref 二选一」的三种结果。
func TestResolveOrderRefChoices(t *testing.T) {
	t.Run("只给 --ref 就用它", func(t *testing.T) {
		got, err := cli.ResolveOrderRef("  ORD-1  ", false, cli.MissingRefError("测试单号"))
		if err != nil {
			t.Fatalf("给了 --ref 还报错: %v", err)
		}
		if got != "ORD-1" {
			t.Errorf("单号 = %q，期望 trim 之后的 ORD-1", got)
		}
	})

	t.Run("只给 --auto-ref 就生成", func(t *testing.T) {
		got, err := cli.ResolveOrderRef("", true, cli.MissingRefError("测试单号"))
		if err != nil {
			t.Fatalf("--auto-ref 报错: %v", err)
		}
		if !autoRefPattern.MatchString(got) {
			t.Errorf("生成出来的单号 %q 不满足 %s", got, autoRefPattern)
		}
	})

	t.Run("两个都没给是参数错", func(t *testing.T) {
		_, err := cli.ResolveOrderRef("", false, cli.MissingRefError("测试单号"))
		if err == nil {
			t.Fatal("两个都没给却成功了：这就是那条「静默重复扣款」的路")
		}
		if got := cli.ExitCode(err); got != cli.ExitUsage {
			t.Errorf("退出码 = %d，期望 %d（参数错）", got, cli.ExitUsage)
		}
		if !strings.Contains(err.Error(), "缺少 --ref 或 --auto-ref") {
			t.Errorf("报错没有点出这两个旗标: %v", err)
		}
	})

	t.Run("两个一起给是参数错", func(t *testing.T) {
		_, err := cli.ResolveOrderRef("ORD-1", true, cli.MissingRefError("测试单号"))
		if err == nil {
			t.Fatal("--ref 与 --auto-ref 一起给却成功了")
		}
		if got := cli.ExitCode(err); got != cli.ExitUsage {
			t.Errorf("退出码 = %d，期望 %d（参数错）", got, cli.ExitUsage)
		}
	})
}

// writeCommandNeedingRef 是一条收单号的写命令：名字、处理函数，以及「走到单号那
// 一步」所需的其他旗标（单号之外都不必合法——判据在连库与业务校验之前）。
type writeCommandNeedingRef struct {
	name string
	run  func([]string) error
	args []string
}

// writeCommandsNeedingRef 是范围本身：所有收 --ref 的写命令，一条不漏。
//
// 用例按这张表逐条跑，新加一条收 --ref 的写命令时往这里补一行——漏掉的那条不会
// 自己冒出来，所以这张表也是「范围有没有扫全」的答案。
var writeCommandsNeedingRef = []writeCommandNeedingRef{
	{"plan grant", runPlanGrant, []string{"--user", "1", "--plan", "p"}},
	{"plan void", runPlanVoid, []string{"--id", "1"}},
	{"wallet credit", runWalletCredit, []string{"--user", "1", "--amount", "1"}},
	{"wallet adjust", runWalletAdjust, []string{"--user", "1", "--amount", "1"}},
	{"wallet refund", runWalletRefund, []string{"--user", "1", "--amount", "1"}},
	{"wallet payout request", runPayoutRequest, []string{"--user", "1", "--amount", "1"}},
	{"wallet payout settle", runPayoutSettle, []string{"--user", "1", "--amount", "1"}},
	{"wallet payment create", runPaymentCreate, []string{"--user", "1", "--amount", "1"}},
}

// TestWriteCommandsRejectMissingRef 断言每一条写命令在既没给 --ref、也没给
// --auto-ref 时都按参数错停下，而且停在连库之前。
//
// 这一条是整条 issue 的核心取舍：--ref 是幂等锚点，默默生成会把「重跑 = 幂等
// 命中」变成「真的又执行一次」。用例不连库，所以它证明的不只是报错，还有「没有
// 悄悄去写」——走到连库那一步的话，这里会拿到配置错误而不是退出码 2。
func TestWriteCommandsRejectMissingRef(t *testing.T) {
	for _, c := range writeCommandsNeedingRef {
		t.Run(c.name, func(t *testing.T) {
			err := c.run(c.args)
			if err == nil {
				t.Fatal("既没给 --ref 也没给 --auto-ref，命令却成功了")
			}
			if got := cli.ExitCode(err); got != cli.ExitUsage {
				t.Fatalf("退出码 = %d，期望 %d（参数错），错误是 %v", got, cli.ExitUsage, err)
			}
			if !strings.Contains(err.Error(), "--auto-ref") {
				t.Errorf("报错没有提到 --auto-ref，读的人不知道还有这条路: %v", err)
			}
		})
	}
}

// TestWriteCommandsRejectRefWithAutoRef 断言 --ref 与 --auto-ref 同时出现时同样
// 按参数错停下：它们是同一个问题的两种回答，挑哪一个都可能不是操作者要的。
func TestWriteCommandsRejectRefWithAutoRef(t *testing.T) {
	for _, c := range writeCommandsNeedingRef {
		t.Run(c.name, func(t *testing.T) {
			args := append(append([]string{}, c.args...), "--ref", "ORD-1", "--auto-ref")
			err := c.run(args)
			if err == nil {
				t.Fatal("--ref 与 --auto-ref 一起给，命令却成功了")
			}
			if got := cli.ExitCode(err); got != cli.ExitUsage {
				t.Fatalf("退出码 = %d，期望 %d（参数错），错误是 %v", got, cli.ExitUsage, err)
			}
			if !strings.Contains(err.Error(), "只能给一个") {
				t.Errorf("报错没有说清两个旗标互斥: %v", err)
			}
		})
	}
}

// TestHelpMentionsAutoRef 按常量名扫各条帮助：收单号的写命令都要提到 --auto-ref，
// 只读命令的用法行不能出现它。
//
// 按常量名而不是按「整份帮助里有没有这个词」：wallet payment 的同屏既有 create
// 也有 status，前者的用法行该写它、后者的不该。所以断言精确到行。
func TestHelpMentionsAutoRef(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
	}{
		{"userPlanShortUsage", userPlanShortUsage},
		{"userPlanUsage", userPlanUsage},
		{"userWalletShortUsage", userWalletShortUsage},
		{"userWalletUsage", userWalletUsage},
		{"walletShortUsage", walletShortUsage},
		{"walletUsage", walletUsage},
		{"walletRefundShortUsage", walletRefundShortUsage},
		{"walletRefundUsage", walletRefundUsage},
		{"walletPayoutShortUsage", walletPayoutShortUsage},
		{"walletPayoutUsage", walletPayoutUsage},
		{"walletPaymentShortUsage", walletPaymentShortUsage},
		{"walletPaymentUsage", walletPaymentUsage},
		{"planUsage", planUsage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(tc.text, "--auto-ref") {
				t.Errorf("%s 里没有 --auto-ref：收单号的写命令不该只提 --ref", tc.name)
			}
		})
	}

	// 只读命令的用法行不带这个旗标。plan 的短版只剩商品定义，整份都不该出现它。
	if strings.Contains(planShortUsage, "--auto-ref") {
		t.Error("planShortUsage 出现了 --auto-ref：plan 的短版只有 create/list/get/enable")
	}
	for _, tc := range []struct {
		what  string
		text  string
		lines []string
	}{
		{"userPlanShortUsage", userPlanShortUsage, []string{
			"larkaxctl user plan list", "larkaxctl user plan order show",
			"larkaxctl user plan order set",
		}},
		{"userWalletShortUsage", userWalletShortUsage, []string{"larkaxctl user wallet show"}},
		{"walletShortUsage", walletShortUsage, []string{
			"larkaxctl wallet payout approve", "larkaxctl wallet payout paid",
			"larkaxctl wallet payment status",
		}},
		{"walletPayoutShortUsage", walletPayoutShortUsage, []string{
			"larkaxctl wallet payout approve", "larkaxctl wallet payout reject",
			"larkaxctl wallet payout paid",
		}},
		{"walletPaymentShortUsage", walletPaymentShortUsage, []string{
			"larkaxctl wallet payment status",
		}},
	} {
		for _, line := range strings.Split(tc.text, "\n") {
			trimmed := strings.TrimSpace(line)
			for _, readOnly := range tc.lines {
				if strings.HasPrefix(trimmed, readOnly) && strings.Contains(trimmed, "--auto-ref") {
					t.Errorf("%s 的只读命令用法行带了 --auto-ref：%q", tc.what, trimmed)
				}
			}
		}
	}
}

// ── 连库的那一半：生成出来的单号真的落到账上 ────────────────────────────

// refFromOutput 从回显里取出这条命令用的单号。
//
// credit 的回显是「幂等键 <ref>，交易 <txn>。」；plan 的是「…发放单号 <ref>。」
// 两种形状各扫一次，取到的值再按 autoRefPattern 核一遍。
func refFromOutput(t *testing.T, out string) string {
	t.Helper()
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`幂等键 (\S+?)，`),
		regexp.MustCompile(`(?:发放|作废)单号 (MANUAL_\S+?)。`),
	} {
		if m := re.FindStringSubmatch(out); m != nil {
			return m[1]
		}
	}
	t.Fatalf("回显里找不到单号：\n%s", out)
	return ""
}

// txnIDFromOutput 从回显里取出交易号。
func txnIDFromOutput(t *testing.T, out string) string {
	t.Helper()
	m := regexp.MustCompile(`交易 (\S+?)。`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("回显里找不到交易号：\n%s", out)
	}
	return m[1]
}

// TestWalletCreditAutoRefTwoRunsAreTwoEntries 钉住 issue 的第一条判据：带
// --auto-ref 连跑两次产生两笔，各打各的单号，两次不同。
func TestWalletCreditAutoRefTwoRunsAreTwoEntries(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	userID := createTestUser(t)
	user := itoa64(userID)
	mustRun(t, runWalletCreate, "--user", user)

	first := mustRun(t, runWalletCredit, "--user", user, "--amount", "10", "--auto-ref")
	second := mustRun(t, runWalletCredit, "--user", user, "--amount", "10", "--auto-ref")
	assertHas(t, first, "已入账")
	assertHas(t, second, "已入账")

	firstRef, secondRef := refFromOutput(t, first), refFromOutput(t, second)
	for _, ref := range []string{firstRef, secondRef} {
		if !autoRefPattern.MatchString(ref) {
			t.Errorf("打完的单号 %q 不满足 %s", ref, autoRefPattern)
		}
	}
	if firstRef == secondRef {
		t.Fatalf("两次 --auto-ref 打出了同一个单号 %q：第二次会被当成幂等重放", firstRef)
	}

	// 两笔都真的落了账：余额翻倍，两个交易号各有一组分录。
	if got := walletBalance(t, db, userID); got != "20.00000000" {
		t.Errorf("两次入账之后余额 = %s，期望 20.00000000", got)
	}
	for _, ref := range []string{firstRef, secondRef} {
		if rows := txnRows(t, db, txnIDForOrder(ref)); len(rows) == 0 {
			t.Errorf("单号 %q 对应的交易一笔分录都没有", ref)
		}
	}
}

// TestWalletCreditAutoRefDryRunMatchesRealRun 钉住 issue 点名的用法：
// --dry-run --auto-ref 预览里打出来的单号，拿它真跑一次，写下的账本行与预览
// 逐字相等。
func TestWalletCreditAutoRefDryRunMatchesRealRun(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	userID := createTestUser(t)
	user := itoa64(userID)
	mustRun(t, runWalletCreate, "--user", user)

	dryOut := mustRun(t, runWalletCredit, "--user", user, "--amount", "100", "--auto-ref", "--dry-run")
	assertHas(t, dryOut, "将入账", cli.DryRunNote)

	ref := refFromOutput(t, dryOut)
	if !autoRefPattern.MatchString(ref) {
		t.Fatalf("预览打出来的单号 %q 不满足 %s", ref, autoRefPattern)
	}
	// 预览里的单号就是这次真跑要用的那一个：干跑没写库，余额仍是 0。
	if balance := walletBalance(t, db, userID); balance != "0.00000000" {
		t.Fatalf("干跑之后余额 = %s，期望 0.00000000", balance)
	}

	realOut := mustRun(t, runWalletCredit, "--user", user, "--amount", "100", "--ref", ref)
	assertHas(t, realOut, "已入账")

	// 同一个单号 → 同一个交易号；两条回显的分录段逐字相等。
	if dryTxn, realTxn := txnIDFromOutput(t, dryOut), txnIDFromOutput(t, realOut); dryTxn != realTxn {
		t.Errorf("预览的交易 %s 与真跑的 %s 不是同一个：单号没有原样用上", dryTxn, realTxn)
	}
	if got, want := entryBlock(t, dryOut), entryBlock(t, realOut); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("预览的分录与真跑不同：\n预览 %v\n真跑 %v", got, want)
	}
	// 库里那几行也带着同一个单号：备注里的「单据 <ref>」是事后对账的落点。
	rows := txnRows(t, db, txnIDForOrder(ref))
	if len(rows) == 0 {
		t.Fatalf("真跑之后单号 %q 的交易没有分录", ref)
	}
	if !strings.Contains(strings.Join(rows, "\n"), ref) {
		t.Errorf("分录备注里没有单号 %q：%v", ref, rows)
	}
}

// TestPlanGrantAutoRefPrintsAndDiffers 钉住 plan grant 那一侧：新增的「发放单号」
// 一行真的打了出来，两次 --auto-ref 得到两条订阅、两个不同的单号。
func TestPlanGrantAutoRefPrintsAndDiffers(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	userID := createTestUser(t)
	user := itoa64(userID)
	planKey := uniqueName("autoref-plan")
	mustStream(t, func() error {
		return runPlan([]string{
			"create", "--key", planKey, "--name", "自动单号额度包",
			"--unit", "amount", "--quota", "5",
		})
	})
	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)

	grant := func() string {
		_, stderr := mustStream(t, func() error {
			return runPlan([]string{
				"grant", "--user", user, "--plan", planKey,
				"--expires-at", expires, "--auto-ref",
			})
		})
		assertHas(t, stderr, "已发放订阅", "发放单号")
		return refFromOutput(t, stderr)
	}
	firstRef, secondRef := grant(), grant()
	for _, ref := range []string{firstRef, secondRef} {
		if !autoRefPattern.MatchString(ref) {
			t.Errorf("发放单号 %q 不满足 %s", ref, autoRefPattern)
		}
	}
	if firstRef == secondRef {
		t.Fatalf("两次 --auto-ref 发放用了同一个单号 %q", firstRef)
	}

	var rows int
	if err := db.QueryRowContext(t.Context(),
		`SELECT count(*) FROM user_plans WHERE user_id = $1 AND ref IN ($2, $3)`,
		userID, firstRef, secondRef).Scan(&rows); err != nil {
		t.Fatalf("数订阅失败: %v", err)
	}
	if rows != 2 {
		t.Errorf("库里按两个单号查到 %d 行订阅，期望 2 行", rows)
	}
}

// TestWalletRefundAutoRefUsesGeneratedRef 走一遍退款这条写路径：--auto-ref 生成的
// 单号进了回显、也进了账本，余额按退款额涨回来。
func TestWalletRefundAutoRefUsesGeneratedRef(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	userID := createTestUser(t)
	user := itoa64(userID)
	mustRun(t, runWalletCreate, "--user", user)
	mustRun(t, runWalletCredit, "--user", user, "--amount", "100", "--ref", uniqueName("fund"))

	out := mustRun(t, runWalletRefund, "--user", user, "--amount", "10", "--auto-ref")
	assertHas(t, out, "已退款给用户")
	ref := refFromOutput(t, out)
	if !autoRefPattern.MatchString(ref) {
		t.Fatalf("退款回显里的单号 %q 不满足 %s", ref, autoRefPattern)
	}
	if rows := txnRows(t, db, txnIDForRefund(ref)); len(rows) == 0 {
		t.Errorf("单号 %q 对应的退款交易没有分录", ref)
	}
	if got := walletBalance(t, db, userID); got != "110.00000000" {
		t.Errorf("退款之后余额 = %s，期望 110.00000000", got)
	}
}

// TestWalletPaymentAutoRefCreatesTwoOrders 走一遍支付下单：两次 --auto-ref 各自
// 生成单号，库里落下两张单子。
func TestWalletPaymentAutoRefCreatesTwoOrders(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	userID := createTestUser(t)
	user := itoa64(userID)
	mustRun(t, runWalletCreate, "--user", user)
	setPaymentFake(t, t.Context(), db, true)

	orderNo := regexp.MustCompile(`订单号\s+(MANUAL_\S+)`)
	create := func() string {
		out := mustRun(t, runPaymentCreate, "--user", user, "--amount", "5", "--auto-ref")
		m := orderNo.FindStringSubmatch(out)
		if m == nil {
			t.Fatalf("回显里没有自动单号：\n%s", out)
		}
		return m[1]
	}
	first, second := create(), create()
	for _, no := range []string{first, second} {
		if !autoRefPattern.MatchString(no) {
			t.Errorf("支付单号 %q 不满足 %s", no, autoRefPattern)
		}
	}
	if first == second {
		t.Fatalf("两次 --auto-ref 下单用了同一个单号 %q", first)
	}
	var rows int
	if err := db.QueryRowContext(t.Context(),
		`SELECT count(*) FROM payment_orders WHERE order_no IN ($1, $2)`,
		first, second).Scan(&rows); err != nil {
		t.Fatalf("数支付单失败: %v", err)
	}
	if rows != 2 {
		t.Errorf("库里按两个单号查到 %d 张支付单，期望 2 张", rows)
	}
}

// TestPlanGrantAutoRefDryRunMatchesRealRun 钉住发放这一侧的预览：--dry-run
// --auto-ref 打出来的单号，拿它真跑，写下的分录与预览相同。
func TestPlanGrantAutoRefDryRunMatchesRealRun(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	user := createTestUserID(t)
	planKey := uniqueName("autoref-dryrun-plan")
	mustStream(t, func() error {
		return runPlan([]string{
			"create", "--key", planKey, "--name", "自动单号干跑额度包",
			"--unit", "amount", "--quota", "7",
		})
	})
	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)

	_, dryErr := mustStream(t, func() error {
		return runPlan([]string{
			"grant", "--user", user, "--plan", planKey,
			"--expires-at", expires, "--auto-ref", "--dry-run",
		})
	})
	assertHas(t, dryErr, "将发放订阅", "发放单号")
	ref := refFromOutput(t, dryErr)
	if !autoRefPattern.MatchString(ref) {
		t.Fatalf("预览打出来的发放单号 %q 不满足 %s", ref, autoRefPattern)
	}

	_, realErr := mustStream(t, func() error {
		return runPlan([]string{
			"grant", "--user", user, "--plan", planKey,
			"--expires-at", expires, "--ref", ref,
		})
	})
	assertHas(t, realErr, "已发放订阅", "发放单号 "+ref)

	// 订阅 id 是序列号，干跑用掉一个之后真跑会拿到下一个：先把它归一，再把动词
	// 从「将发放」换成「已发放」，其余部分逐字比。
	dryText := strings.Replace(dryErr, "将发放", "已发放", 1)
	if got, want := stripSubscriptionID(t, dryText), stripSubscriptionID(t, realErr); got != want {
		t.Errorf("预览与真跑的发放回显不同（订阅号已归一）：\n预览 %q\n真跑 %q", got, want)
	}
	if rows := txnRows(t, db, txnIDFromKey("larkax:plan-grant:v1", ref)); len(rows) == 0 {
		t.Errorf("真跑之后单号 %q 的交易没有分录", ref)
	}
}
