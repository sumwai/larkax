package main

import (
	"database/sql"
	"strconv"
	"strings"
	"testing"
)

// 这一组用例走的是**命令行的那一条完整路径**：解析旗标 → 账本 → store → 落库 →
// 回执。它要证明的与账本、store 两层不同：
//
//   - 回执上逐条分录的形状（金额是定点十进制、科目名、借/贷那一列）
//   - 幂等键是从哪来的、重跑同一条命令认不认得出来
//   - 「没经过 approved 就打款」在命令行这一层也是被拒的（退出码 4，不是 1）
//
// 金额一律按 money 的定点写法断言（8 位小数）：`10` 与 `10.00000000` 是同一个
// 数，但回执上打出来的是后者——按前者断言会写成一条恒不成立的判据。

// runCmd 跑一条命令并接住三个流。
//
// 本包共用的 runOnce 收的是一个 []string，而这里几乎每条调用都在原地列一串旗标，
// 多出来的那对花括号只剩噪音。
func runCmd(t *testing.T, run func([]string) error, args ...string) commandRun {
	t.Helper()
	var err error
	stdout, stderr := captureStreams(t, func() { err = run(args) })
	return commandRun{stdout: stdout, stderr: stderr, err: err}
}

// mustRun 跑一条命令并要求它成功，返回 stdout。
func mustRun(t *testing.T, run func([]string) error, args ...string) string {
	t.Helper()
	r := runCmd(t, run, args...)
	if r.err != nil {
		t.Fatalf("%v 失败: %v\nstderr=%s", args, r.err, r.stderr)
	}
	return r.stdout
}

// assertHas 断言输出里包含这几段。
//
// 报错时把整份输出打出来：回执是一屏文本，只报「少了哪一段」看不出它是被打错
// 了还是根本没打。
func assertHas(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(out, want) {
			t.Errorf("输出里没有 %q：\n%s", want, out)
		}
	}
}

// txnEntryCount 数某个交易在库里留下了几条分录。
//
// 「被拒的动作一个字节都没写」这类判据只有数库里的条数才判得了：被拒时回执上
// 什么都不打，而「什么都没打」与「这次根本没跑」在屏幕上是同一个样子。
func txnEntryCount(t *testing.T, db *sql.DB, txnID string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(t.Context(),
		`SELECT count(*) FROM ledger_entries WHERE txn_id = $1`, txnID).Scan(&n); err != nil {
		t.Fatalf("数分录失败: %v", err)
	}
	return n
}

// payoutStatus 读一条提现单现在的状态。
func payoutStatus(t *testing.T, db *sql.DB, id int64) string {
	t.Helper()
	var status string
	if err := db.QueryRowContext(t.Context(),
		`SELECT status FROM payout_requests WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatalf("读提现单 %d 失败: %v", id, err)
	}
	return status
}

// userBalance 读一个用户的钱包余额（定点十进制的文本）。
func userBalance(t *testing.T, db *sql.DB, userID int64) string {
	t.Helper()
	var balance string
	if err := db.QueryRowContext(t.Context(),
		`SELECT balance::text FROM user_wallets WHERE user_id = $1`, userID).Scan(&balance); err != nil {
		t.Fatalf("读余额失败: %v", err)
	}
	return balance
}

// mustUserID 把命令行的用户标识（这里是 createTestUserID 给的十进制形式）
// 转回 users.id，供直接查库。
func mustUserID(t *testing.T, ref string) int64 {
	t.Helper()
	id, err := strconv.ParseInt(ref, 10, 64)
	if err != nil {
		t.Fatalf("用例里的用户标识 %q 不是十进制 id: %v", ref, err)
	}
	return id
}

// payoutIDOfMerchant 取这个商户名下那一张提现单的 id。
//
// 只在这条用例给自己建了一张单子时用：多条时这条判据本身就不成立。
func payoutIDOfMerchant(t *testing.T, db *sql.DB, merchantID int64) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRowContext(t.Context(),
		`SELECT id FROM payout_requests WHERE merchant_user_id = $1`, merchantID).Scan(&id); err != nil {
		t.Fatalf("读提现单 id 失败: %v", err)
	}
	return id
}

// ── wallet refund ───────────────────────────────────────────────────────

// 退款：回执逐条列出分录，余额真的加上了，重跑同一条命令不改动任何东西。
func TestWalletRefundReceiptAndReplay(t *testing.T) {
	setupEnv(t)
	db := testDB(t)
	owner := createTestUserID(t)
	mustRun(t, runWalletCreate, "--user", owner)
	ref := uniqueName("refund")

	out := mustRun(t, runWalletRefund,
		"--user", owner, "--amount", "10", "--ref", ref, "--memo", "请求失败")
	want := []string{
		"已退款给用户",
		"10.00000000 CNY",
		"幂等键 " + ref,
		"原因：请求失败",
		"分录：",
		// 借用户、贷平台收入：退款把这一笔从平台收入里冲回去。
		"借 user:" + owner,
		"贷 platform_revenue",
	}
	assertHas(t, out, want...)

	if got := userBalance(t, db, mustUserID(t, owner)); got != "10.00000000" {
		t.Errorf("退款之后余额 = %s，期望 10.00000000", got)
	}

	// 重放：同一把幂等键、同样的参数，回执说「未改动」，余额一分不动。
	again := mustRun(t, runWalletRefund,
		"--user", owner, "--amount", "10", "--ref", ref, "--memo", "请求失败")
	assertHas(t, again, "幂等键 "+ref+" 已经退过款，本次未改动。", "此前写入的分录：", "借 user:"+owner)
	if got := userBalance(t, db, mustUserID(t, owner)); got != "10.00000000" {
		t.Errorf("重放之后余额 = %s，期望仍是 10.00000000（没有退第二次）", got)
	}

	// 同一把键配另一个金额不是重放，是冲突：退出码 4，余额不动。
	r := runCmd(t, runWalletRefund,
		"--user", owner, "--amount", "11", "--ref", ref)
	if r.err == nil {
		t.Fatal("同一把幂等键配另一个金额应当被拒")
	}
	if code := exitCode(r.err); code != exitConflict {
		t.Errorf("退出码 = %d，期望 %d（约束冲突）：%v", code, exitConflict, r.err)
	}
	// 报错要点名撞上的是**这一把幂等键**推出来的那笔交易：原文只说「交易号已存在
	// 但内容不同」，而交易号是哈希，操作者从它回不到自己写的那个 --ref。
	if want := string(txnIDForRefund(ref)); !strings.Contains(r.err.Error(), want) {
		t.Errorf("报错里没有幂等键 %s 推出来的交易号 %s：%v", ref, want, r.err)
	}
	if got := userBalance(t, db, mustUserID(t, owner)); got != "10.00000000" {
		t.Errorf("冲突之后余额 = %s，期望仍是 10.00000000", got)
	}
}

// 分账退款：给了商户与上游成本时，回执上是四条分录，平台收入那一笔是差额。
func TestWalletRefundWithSplit(t *testing.T) {
	setupEnv(t)
	owner := createTestUserID(t)
	merchant := createTestUserID(t)
	// 收款人要有个钱包：退款往 user 科目上记一笔，快照跟着加。
	mustRun(t, runWalletCreate, "--user", owner)

	out := mustRun(t, runWalletRefund,
		"--user", owner, "--amount", "10", "--ref", uniqueName("refund-split"),
		"--merchant", merchant, "--merchant-share", "3", "--upstream-cost", "2")

	assertHas(t, out,
		"借 user:"+owner, "10.00000000 CNY",
		"贷 merchant_payable:"+merchant, "3.00000000 CNY",
		"贷 upstream_cost", "2.00000000 CNY",
		"贷 platform_revenue", "5.00000000 CNY",
	)
}

// 分成那两个旗标必须成对出现：只给商户、不给金额是一次**静默丢弃**——
// 操作者以为冲回了那个商户的分成，账面上一分钱都没落到他身上。
func TestWalletRefundRejectsHalfGivenSplit(t *testing.T) {
	setupEnv(t)
	owner := createTestUserID(t)

	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			"给了 --merchant 没给 --merchant-share",
			[]string{"--user", owner, "--amount", "10", "--ref", "R1", "--merchant", "1"},
			"--merchant-share",
		},
		{
			"给了 --merchant-share 没给 --merchant",
			[]string{"--user", owner, "--amount", "10", "--ref", "R2", "--merchant-share", "3"},
			"--merchant",
		},
		{
			"分成与上游成本之和超过退款额",
			[]string{"--user", owner, "--amount", "10", "--ref", "R3",
				"--merchant", "1", "--merchant-share", "7", "--upstream-cost", "4"},
			"超过",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := runCmd(t, runWalletRefund, tc.args...)
			if r.err == nil {
				t.Fatal("这条旗标组合应当被拒")
			}
			if code := exitCode(r.err); code != exitUsage {
				t.Errorf("退出码 = %d，期望 %d（参数错，连库之前就该拒）", code, exitUsage)
			}
			if !strings.Contains(r.err.Error(), tc.want) {
				t.Errorf("报错里没有 %q：%v", tc.want, r.err)
			}
		})
	}
}

// ── wallet payout ───────────────────────────────────────────────────────

// 申请：回执列出冻结那一组分录，库里多一行 pending 的单子，重跑不再建单。
func TestWalletPayoutRequestReceiptAndReplay(t *testing.T) {
	setupEnv(t)
	db := testDB(t)
	merchant := createTestUserID(t)
	ref := uniqueName("payout")

	out := mustRun(t, runWalletPayout, "request",
		"--user", merchant, "--amount", "12.5", "--ref", ref)
	assertHas(t, out,
		"已申请提现。",
		ref,
		"12.50000000",
		"pending（待审批）",
		"分录：",
		// 钱从商户的分成挪进这条单子的待付，两边都挂着这条单子。
		"贷 merchant_payable:"+merchant,
		"借 payout:",
	)

	var id int64
	if err := db.QueryRowContext(t.Context(),
		`SELECT id FROM payout_requests WHERE merchant_user_id = $1`,
		mustUserID(t, merchant)).Scan(&id); err != nil {
		t.Fatalf("读提现单失败: %v", err)
	}
	if got := payoutStatus(t, db, id); got != "pending" {
		t.Errorf("状态 = %q，期望 pending", got)
	}

	again := mustRun(t, runWalletPayout, "request",
		"--user", merchant, "--amount", "12.5", "--ref", ref)
	assertHas(t, again, "申请单号 "+ref+" 已经建过单，本次未改动。", "分录：")
	var rows int
	if err := db.QueryRowContext(t.Context(),
		`SELECT count(*) FROM payout_requests WHERE merchant_user_id = $1`,
		mustUserID(t, merchant)).Scan(&rows); err != nil {
		t.Fatalf("数提现单失败: %v", err)
	}
	if rows != 1 {
		t.Errorf("重放之后库里 %d 行提现单，期望 1 行", rows)
	}
}

// --destination 不是合法 JSON 时在连库之前就退 2：那一栏要落进 jsonb 列，交给库去
// 报的话，回来的那句是「invalid input syntax for type json」，看不出是哪条命令的
// 哪个参数。这一条不碰库，所以有没有 LARKAX_DB_DSN 都该是红的。
func TestWalletPayoutRequestRejectsBadDestination(t *testing.T) {
	setupEnv(t)

	r := runCmd(t, runPayoutRequest,
		"--user", "1", "--amount", "1", "--ref", "R-dest", "--destination", "{bank:")
	if r.err == nil {
		t.Fatal("不是合法 JSON 的 --destination 应当被拒")
	}
	if code := exitCode(r.err); code != exitUsage {
		t.Errorf("退出码 = %d，期望 %d（参数错，连库之前就该拒）", code, exitUsage)
	}
	assertHas(t, r.err.Error(), "--destination", "JSON")
}

// 出账那道门在命令行这一层也关着：没批准的单子打不了款，而且一笔分录都不写。
func TestWalletPayoutPaidRefusesUnapproved(t *testing.T) {
	setupEnv(t)
	db := testDB(t)
	merchant := createTestUserID(t)

	mustRun(t, runWalletPayout, "request",
		"--user", merchant, "--amount", "9", "--ref", uniqueName("payout-gate"))
	id := payoutIDOfMerchant(t, db, mustUserID(t, merchant))

	txnID := string(txnIDForPayoutPaid(id))
	r := runCmd(t, runWalletPayout, "paid",
		"--id", itoa64(id), "--gateway", "manual", "--gateway-ref", "PAY-1")
	if r.err == nil {
		t.Fatal("没经过批准就打款应当被拒")
	}
	if code := exitCode(r.err); code != exitConflict {
		t.Errorf("退出码 = %d，期望 %d（约束冲突，不是参数错也不是其他失败）：%v",
			code, exitConflict, r.err)
	}
	// 报错要说清现在停在哪一步、这一步要哪一步：只说「不允许」看不出下一步该做什么。
	assertHas(t, r.err.Error(), "pending", "待审批", "approved", "已批准")
	if n := txnEntryCount(t, db, txnID); n != 0 {
		t.Errorf("被拒的打款写下了 %d 条分录", n)
	}
	if got := payoutStatus(t, db, id); got != "pending" {
		t.Errorf("被拒之后状态 = %q，期望仍是 pending", got)
	}

	// 批准 → 打款：回执上是出账那一组分录（贷待付、借通道）。
	approveOut := mustRun(t, runWalletPayout, "approve", "--id", itoa64(id), "--by", merchant)
	assertHas(t, approveOut, "已批准提现单", "approved（已批准）", "审批人")
	// 批准不打分录：那一行只是从待审批变成已批准。
	if got := payoutStatus(t, db, id); got != "approved" {
		t.Fatalf("批准之后状态 = %q，期望 approved", got)
	}
	if strings.Contains(approveOut, "分录：") {
		t.Errorf("批准不该列出分录（钱在申请那一步就冻住了）：\n%s", approveOut)
	}

	paidOut := mustRun(t, runWalletPayout, "paid",
		"--id", itoa64(id), "--gateway", "manual", "--gateway-ref", "PAY-1")
	assertHas(t, paidOut,
		"已打款提现单",
		"paid（已打款）",
		"PAY-1",
		"分录：",
		"贷 payout:",
		"借 payment_gateway:manual",
		"9.00000000 CNY",
	)
	if got := payoutStatus(t, db, id); got != "paid" {
		t.Errorf("打款之后状态 = %q，期望 paid", got)
	}

	// 打款重放：同一把键（这条单子）再来一次，说「未改动」，分录还是那两条。
	paidAgain := mustRun(t, runWalletPayout, "paid",
		"--id", itoa64(id), "--gateway", "manual", "--gateway-ref", "PAY-1")
	assertHas(t, paidAgain, "已经打过款，本次未改动。")
	if n := txnEntryCount(t, db, txnID); n != 2 {
		t.Errorf("重放之后这个交易有 %d 条分录，期望 2 条", n)
	}

	// 已打款的单子驳不回去：终态就是终态。
	r = runCmd(t, runWalletPayout, "reject", "--id", itoa64(id), "--by", merchant)
	if r.err == nil {
		t.Fatal("驳回一条已经打款的单子应当被拒")
	}
	if code := exitCode(r.err); code != exitConflict {
		t.Errorf("退出码 = %d，期望 %d：%v", code, exitConflict, r.err)
	}
	if got := payoutStatus(t, db, id); got != "paid" {
		t.Errorf("被拒的驳回把状态改成了 %q，期望仍是 paid", got)
	}
}

// 驳回：解除冻结的那一组镜像分录，重跑不改动，驳回过之后也打不了款。
func TestWalletPayoutRejectReleasesFreeze(t *testing.T) {
	setupEnv(t)
	db := testDB(t)
	merchant := createTestUserID(t)

	mustRun(t, runWalletPayout, "request",
		"--user", merchant, "--amount", "4", "--ref", uniqueName("payout-reject"))
	id := payoutIDOfMerchant(t, db, mustUserID(t, merchant))

	out := mustRun(t, runWalletPayout, "reject",
		"--id", itoa64(id), "--by", merchant, "--note", "收款信息不全")
	assertHas(t, out,
		"已驳回提现单",
		"rejected（已驳回）",
		"收款信息不全",
		"分录：",
		// 申请那一笔的镜像：欠商户的分成恢复、这条单子的待付核销。
		"借 merchant_payable:"+merchant,
		"贷 payout:",
		"4.00000000 CNY",
	)
	if got := payoutStatus(t, db, id); got != "rejected" {
		t.Fatalf("驳回之后状态 = %q，期望 rejected", got)
	}

	// 待付科目被核销干净：解冻之后这条单子不再欠任何钱。
	var left string
	if err := db.QueryRowContext(t.Context(),
		`SELECT COALESCE(SUM(amount), 0)::numeric(20,8)::text FROM ledger_entries WHERE account = $1`,
		"payout:"+itoa64(id)).Scan(&left); err != nil {
		t.Fatalf("汇总待付科目失败: %v", err)
	}
	if left != "0.00000000" {
		t.Errorf("驳回之后待付 = %s，期望 0.00000000（冻结已解除）", left)
	}

	again := mustRun(t, runWalletPayout, "reject",
		"--id", itoa64(id), "--by", merchant, "--note", "收款信息不全")
	assertHas(t, again, "提现单 "+itoa64(id)+" 已经驳回过，本次未改动。")

	r := runCmd(t, runWalletPayout, "paid",
		"--id", itoa64(id), "--gateway", "manual", "--gateway-ref", "PAY-2")
	if r.err == nil {
		t.Fatal("给一张驳回了的单子打款应当被拒")
	}
	if code := exitCode(r.err); code != exitConflict {
		t.Errorf("退出码 = %d，期望 %d：%v", code, exitConflict, r.err)
	}
	if n := txnEntryCount(t, db, string(txnIDForPayoutPaid(id))); n != 0 {
		t.Errorf("被拒的打款写下了 %d 条分录", n)
	}
}

// 打款的幂等键是**单子自己的 id**，不是通道给的凭据号。
//
// 键取凭据号的话，同一张单换一个号会被算成一笔新交易，待付科目被扣成负数——
// 那是重复打款，而它一路都不会报错。换一个号必须撞成冲突（退出码 4）。
func TestWalletPayoutRefChangeIsNotANewPayment(t *testing.T) {
	setupEnv(t)
	db := testDB(t)
	merchant := createTestUserID(t)

	mustRun(t, runWalletPayout, "request",
		"--user", merchant, "--amount", "5", "--ref", uniqueName("payout-ref"))
	id := payoutIDOfMerchant(t, db, mustUserID(t, merchant))
	mustRun(t, runWalletPayout, "approve", "--id", itoa64(id), "--by", merchant)
	mustRun(t, runWalletPayout, "paid",
		"--id", itoa64(id), "--gateway", "manual", "--gateway-ref", "PAY-A")

	r := runCmd(t, runWalletPayout, "paid",
		"--id", itoa64(id), "--gateway", "manual", "--gateway-ref", "PAY-B")
	if r.err == nil {
		t.Fatal("同一张单换一个凭据号应当撞成冲突，而不是再打一次款")
	}
	if code := exitCode(r.err); code != exitConflict {
		t.Errorf("退出码 = %d，期望 %d：%v", code, exitConflict, r.err)
	}
	var left string
	if err := db.QueryRowContext(t.Context(),
		`SELECT COALESCE(SUM(amount), 0)::numeric(20,8)::text FROM ledger_entries WHERE account = $1`,
		"payout:"+itoa64(id)).Scan(&left); err != nil {
		t.Fatalf("汇总待付科目失败: %v", err)
	}
	if left != "0.00000000" {
		t.Errorf("待付 = %s，期望 0.00000000（没有被扣第二次）", left)
	}
	if n := txnEntryCount(t, db, string(txnIDForPayoutPaid(id))); n != 2 {
		t.Errorf("这个交易有 %d 条分录，期望还是那 2 条", n)
	}
}

// --gateway-ref 可选：线下打款（银行转账、现金）拿不到通道流水号，不给这一项照样要能打款，
// 只是分录备注里没有凭据号那一截。要填就只能编一个，编出来的号进了备注反而更难对账。
func TestWalletPayoutPaidWithoutGatewayRef(t *testing.T) {
	setupEnv(t)
	db := testDB(t)
	merchant := createTestUserID(t)

	mustRun(t, runWalletPayout, "request",
		"--user", merchant, "--amount", "6", "--ref", uniqueName("payout-noref"))
	id := payoutIDOfMerchant(t, db, mustUserID(t, merchant))
	mustRun(t, runWalletPayout, "approve", "--id", itoa64(id), "--by", merchant)

	out := mustRun(t, runWalletPayout, "paid", "--id", itoa64(id), "--gateway", "manual")
	assertHas(t, out, "已打款提现单", "6.00000000 CNY", "借 payment_gateway:manual")
	if got := payoutStatus(t, db, id); got != "paid" {
		t.Errorf("打款之后状态 = %q，期望 paid", got)
	}

	txnID := string(txnIDForPayoutPaid(id))
	if n := txnEntryCount(t, db, txnID); n != 2 {
		t.Fatalf("这个交易有 %d 条分录，期望 2 条", n)
	}
	var memo string
	if err := db.QueryRowContext(t.Context(),
		`SELECT memo FROM ledger_entries WHERE txn_id = $1 AND account = $2`,
		txnID, "payout:"+itoa64(id)).Scan(&memo); err != nil {
		t.Fatalf("读打款那条分录的备注失败: %v", err)
	}
	if memo != "提现打款" {
		t.Errorf("没给 --gateway-ref，备注 = %q，期望 %q（只有事由，没有凭据号）", memo, "提现打款")
	}
	// 用法里那一项要带方括号：说明书把它写成必填时，运维会以为不给就发不出去。
	if help := mustRun(t, runWalletPayout, "help"); !strings.Contains(help, "[--gateway-ref") {
		t.Error("用法里 --gateway-ref 没标成可选（可选的那一项要带方括号）")
	}
}

// 找不到单子时退出码是 4，而不是 1：那一行不在，下一步是先看清库里现在有什么。
func TestWalletPayoutUnknownIDIsAConflict(t *testing.T) {
	setupEnv(t)
	// --by 必须是一个真实存在的用户：审批那条路先解析身份、再去找单子，
	// 随手编一个标识会先撞在「找不到用户」上，而这条用例要测的是找不到**单子**。
	by := createTestUserID(t)
	cases := []struct {
		name string
		run  func([]string) error
		args []string
	}{
		{"approve", runPayoutApprove, []string{"--id", "1099511627776", "--by", by}},
		{"reject", runPayoutReject, []string{"--id", "1099511627776", "--by", by}},
		{"paid", runPayoutPaid, []string{"--id", "1099511627776", "--gateway", "manual"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := runCmd(t, tc.run, tc.args...)
			if r.err == nil {
				t.Fatal("一条不存在的提现单应当被拒")
			}
			if code := exitCode(r.err); code != exitConflict {
				t.Errorf("退出码 = %d，期望 %d：%v", code, exitConflict, r.err)
			}
			if !strings.Contains(r.err.Error(), "1099511627776") {
				t.Errorf("报错里没有那个 id：%v", r.err)
			}
		})
	}
}

// ── 用法文本 ────────────────────────────────────────────────────────────

// `wallet refund help` 与 `wallet payout help` 打长版说明书，退出码是 0。
//
// 这两条是**唯一**能拿到长版的路：笼统的 `larkaxctl help wallet refund` 只认
// 第一层，到不了这里。所以用法文本里那句指向自己的话必须真的走得通——它是给
// 读的人照着敲的。
func TestWalletRefundAndPayoutHelpPrintLongUsage(t *testing.T) {
	cases := []struct {
		name string
		run  func([]string) error
		args []string
		want []string
	}{
		{
			"wallet refund help",
			runWalletRefund, []string{"help"},
			[]string{"larkaxctl wallet refund", "--merchant-share", "--upstream-cost"},
		},
		{
			"wallet payout help",
			runWalletPayout, []string{"help"},
			[]string{"larkaxctl wallet payout", "approve", "reject", "paid", "--gateway"},
		},
		{
			"wallet payout request help",
			runPayoutRequest, []string{"help"},
			[]string{"larkaxctl wallet payout", "--destination"},
		},
		{
			"wallet payout approve help",
			runPayoutApprove, []string{"help"},
			[]string{"larkaxctl wallet payout", "--by"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := runCmd(t, tc.run, tc.args...)
			if r.err != nil {
				t.Fatalf("打说明书不该报错: %v", r.err)
			}
			if r.stderr != "" {
				t.Errorf("说明书走 stdout，stderr 应当为空，得到 %q", r.stderr)
			}
			assertHas(t, r.stdout, tc.want...)
		})
	}
}

// 长版说明书要说清这一族最要紧的那件事：**打款只放行已批准的单子**。
//
// 这一句是给运维看的唯一一处说明，写在别处（源码注释、issue）都到不了他眼前。
func TestWalletPayoutUsageStatesTheApprovalGate(t *testing.T) {
	r := runCmd(t, runWalletPayout, "help")
	if r.err != nil {
		t.Fatalf("打说明书不该报错: %v", r.err)
	}
	// 三件事：状态怎么走、paid 只认已批准、驳回会把冻结解除。
	assertHas(t, r.stdout,
		"pending", "approved", "paid", "rejected",
		"只放行已批准",
		"冻结",
	)
}
