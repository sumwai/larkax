package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/auth"
	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/ledger/store"
	"github.com/sumwai/larkax/internal/money"
)

func TestWalletUnknownSubcommand(t *testing.T) {
	err := runWallet(nil)
	if err == nil || !strings.Contains(err.Error(), "需要子命令") {
		t.Fatalf("无子命令时应报错，得到 %v", err)
	}

	err = runWallet([]string{"bogus"})
	if err == nil || !strings.Contains(err.Error(), "未知的 wallet 子命令") {
		t.Fatalf("未知子命令时应报错，得到 %v", err)
	}
}

// 参数检查发生在**连库之前**：一个写错的命令不该先连上生产库再报错。
//
// 这些用例因此不需要库，在没有 DSN 的机器上也真的跑了——不会变成
// 「本地通过、CI 跳过」的那种测试。
func TestWalletOpenRejectsBadArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"没有 --user", nil, "缺少 --user"},
		{"--user 为空", []string{"--user", ""}, "缺少 --user"},
		{"--user 只有空白", []string{"--user", "   "}, "缺少 --user"},
		{"多出位置参数", []string{"--user", "1", "extra"}, "不接受位置参数"},
		{"资产不认识", []string{"--user", "1", "--asset", "USDT"}, "不受支持"},
		// 「给了 --asset 但值是空白」不是「没给」：`--asset "$VAR"` 而 VAR
		// 没设时静默回落成默认资产，会让脚本拿到一个它没指定的品种，而品种
		// 是钱包主键的一部分。这两条钉住它按参数错处置。
		{"--asset 给了但为空", []string{"--user", "1", "--asset", ""}, "--asset 不能为空"},
		{"--asset 只有空白", []string{"--user", "1", "--asset", "   "}, "--asset 不能为空"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runWalletCreate(tc.args)
			if err == nil {
				t.Fatal("期望报错，却通过了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息 %q 里没有 %q", err.Error(), tc.want)
			}
			if code := exitCode(err); code != exitUsage {
				t.Errorf("退出码 = %d，期望 %d（参数错）", code, exitUsage)
			}
		})
	}
}

// 「刚建出来」与「本来就有」必须在输出上分得开：两者都退出 0，
// 而一次静默的空操作与一次真的建了行不是同一个结果。
func TestPrintOpenedWallet(t *testing.T) {
	owner := auth.User{ID: 7, Username: "user-placeholder"}

	var created bytes.Buffer
	printOpenedWallet(&created, owner, ledger.AssetCNY, true)
	for _, want := range []string{"已为", "user-placeholder", "CNY", "余额 0"} {
		if !strings.Contains(created.String(), want) {
			t.Errorf("新建的输出里没有 %q：%q", want, created.String())
		}
	}
	// 归属人只点一次身份（见 docs/cli-output.md 的「实体只点一次身份」）：
	// 用户名后面不再挂一个用户 ID。
	if strings.Contains(created.String(), "id=") {
		t.Errorf("用户名后面不该再挂一个 ID：%q", created.String())
	}

	var existing bytes.Buffer
	printOpenedWallet(&existing, owner, ledger.AssetCNY, false)
	for _, want := range []string{"已有", "user-placeholder", "CNY", "未改动"} {
		if !strings.Contains(existing.String(), want) {
			t.Errorf("已存在的输出里没有 %q：%q", want, existing.String())
		}
	}
	if strings.Contains(existing.String(), "已为") {
		t.Errorf("已存在不该说成「已为…开通」：%q", existing.String())
	}
}

// ── 以下要连库 ──────────────────────────────────────────────────────────

// defaultAssetKey 是新用户钱包默认品种那一行。
//
// 它在迁移里的 editable 是 false，`settings set` 改不动，所以用例只能直接
// 改库——而这条命令正好读它，改库等价于「换一个部署的默认值」。
const defaultAssetKey = "billing.default_asset"

// setSetting 直接改一行 settings，并在用例结束时还原。
//
// 走库而不是走 `settings set`：这个键不允许命令行改（editable=false），
// 而这里要构造的正是「部署时写成了别的值」这个状态。
func setSetting(t *testing.T, db *sql.DB, key, value string) {
	t.Helper()
	ctx := t.Context()

	var before string
	if err := db.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key = $1`, key).Scan(&before); err != nil {
		t.Fatalf("读 settings.%s 失败: %v", key, err)
	}
	t.Cleanup(func() {
		// 用 WithoutCancel：用例结束时 ctx 已经被取消，那时再拿它去执行
		// 还原语句会直接失败，而库会留在被改过的状态上，污染后面的用例。
		if _, err := db.ExecContext(context.WithoutCancel(ctx),
			`UPDATE settings SET value = $2 WHERE key = $1`, key, before); err != nil {
			t.Errorf("还原 settings.%s 失败: %v", key, err)
		}
	})

	if _, err := db.ExecContext(ctx,
		`UPDATE settings SET value = $2 WHERE key = $1`, key, value); err != nil {
		t.Fatalf("改 settings.%s 失败: %v", key, err)
	}
}

// walletRows 数一个用户有几行钱包。
func walletRows(t *testing.T, db *sql.DB, userID int64) int {
	t.Helper()
	var rows int
	if err := db.QueryRowContext(t.Context(),
		`SELECT count(*) FROM user_wallets WHERE user_id = $1`, userID).Scan(&rows); err != nil {
		t.Fatalf("数钱包行失败: %v", err)
	}
	return rows
}

// 开户命令走完整条链路：解析用户 → 开户 → 打印结果，且第二次是空操作。
//
// 它不手工插任何钱包行——这正是 #337 要求的那条路径：新用户只有这一条路
// 能拿到第一行 user_wallets。
func TestWalletOpenCommand(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	userID := createTestUser(t)
	ref := strconv.FormatInt(userID, 10)
	// 输出里点名的是用户名而不是 ID（见 docs/cli-output.md 的「实体只点一次
	// 身份」），所以这里把它查出来，断言才有得比。
	owner, err := auth.New(db).ResolveUser(t.Context(), ref)
	if err != nil {
		t.Fatalf("解析用户失败: %v", err)
	}

	var firstErr error
	first, _ := captureStreams(t, func() { firstErr = runWalletCreate([]string{"--user", ref}) })
	if firstErr != nil {
		t.Fatalf("开户失败: %v", firstErr)
	}
	for _, want := range []string{"已为", "用户 " + owner.Username, "CNY", "余额 0"} {
		if !strings.Contains(first, want) {
			t.Errorf("第一次开户的输出里没有 %q：%q", want, first)
		}
	}

	var secondErr error
	second, _ := captureStreams(t, func() { secondErr = runWalletCreate([]string{"--user", ref}) })
	if secondErr != nil {
		t.Fatalf("重复开户应当成功（幂等）: %v", secondErr)
	}
	for _, want := range []string{"已有", "未改动"} {
		if !strings.Contains(second, want) {
			t.Errorf("第二次开户的输出里没有 %q：%q", want, second)
		}
	}

	var balance, frozen string
	if err := db.QueryRowContext(t.Context(),
		`SELECT balance, frozen FROM user_wallets WHERE user_id = $1 AND asset = 'CNY'`,
		userID).Scan(&balance, &frozen); err != nil {
		t.Fatalf("读钱包失败: %v", err)
	}
	if balance != "0.00000000" || frozen != "0.00000000" {
		t.Errorf("余额=%s 冻结=%s，期望都是 0", balance, frozen)
	}
}

// 显式 --asset 真的压过 settings 的默认值——它正是那条错误信息推荐的逃生门。
//
// 只能这么测：默认值合法时（就是 CNY）两种写法开出来的都是 CNY，分不出
// 「读了显式值」与「没读、回落成默认」。所以先把 billing.default_asset 改成
// 一个不受支持的值，再传 --asset CNY——能成就说明显式值确实压过了配置。
//
// 同一轮里顺带钉住失败那一侧：默认值非法且不给 --asset 时报错、且**一行钱包
// 都不留**（报错发生在开户之前）。
func TestWalletOpenExplicitAssetOverridesSetting(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	userID := createTestUser(t)
	ref := strconv.FormatInt(userID, 10)

	setSetting(t, db, defaultAssetKey, `"USDT"`)

	err := runCaptured(t, func() error {
		return runWalletCreate([]string{"--user", ref})
	})
	if err == nil {
		t.Fatal("默认资产不受支持时应当报错")
	}
	if !strings.Contains(err.Error(), defaultAssetKey) {
		t.Errorf("错误信息 %q 里没有点出 %s", err, defaultAssetKey)
	}
	if !strings.Contains(err.Error(), "--asset") {
		t.Errorf("错误信息 %q 里没有给出 --asset 这条逃生门", err)
	}
	if rows := walletRows(t, db, userID); rows != 0 {
		t.Fatalf("被拒的开户留下了 %d 行钱包", rows)
	}

	var explicitErr error
	out, _ := captureStreams(t, func() {
		explicitErr = runWalletCreate([]string{"--user", ref, "--asset", "CNY"})
	})
	if explicitErr != nil {
		t.Fatalf("显式 --asset CNY 应当压过非法的默认值，却失败: %v", explicitErr)
	}
	if !strings.Contains(out, "CNY") {
		t.Errorf("输出里没有 CNY：%q", out)
	}

	var asset string
	if scanErr := db.QueryRowContext(t.Context(),
		`SELECT asset FROM user_wallets WHERE user_id = $1`, userID).Scan(&asset); scanErr != nil {
		t.Fatalf("读钱包失败: %v", scanErr)
	}
	if asset != "CNY" {
		t.Errorf("开出来的品种 = %q，期望 CNY（显式值）", asset)
	}
}

// 用户不存在时退出码是「要查的那一行不存在」，不是笼统的失败。
//
// 这条区分有实际用处：脚本对 1 与 4 的处置不同——一个是「去看环境」，
// 另一个是「先确认这个用户是存在的」。
func TestWalletOpenUnknownUserExitCode(t *testing.T) {
	testDB(t)
	setupEnv(t)

	err := runCaptured(t, func() error {
		return runWalletCreate([]string{"--user", uniqueName("nobody")})
	})
	if err == nil {
		t.Fatal("不存在的用户应当报错")
	}
	if !errors.Is(err, auth.ErrUserNotFound) {
		t.Errorf("错误链里没有 auth.ErrUserNotFound：%v", err)
	}
	if code := exitCode(err); code != exitConflict {
		t.Errorf("退出码 = %d，期望 %d（要查的那一行不存在）", code, exitConflict)
	}
}

// 写入路径上「用户不存在」是**另一个哨兵**（store.ErrNoSuchUser），它也要落 4。
//
// 与上一条是同一个事实的两条路径：上一条在**解析**时认不出标识（auth 那边），
// 这一条在**写入**时被外键挡下（store 那边，用户解析完到插入之间被删）。
// 两条路径给出两个退出码，脚本就得按两套处置写；所以两个哨兵都进 conflictErrors。
//
// 不走 runWalletCreate 去构造那条路径：正常命令行先解析用户，要走到这里得让
// 「解析完到插入之间」真的发生一次删除——那是个竞态，钉不稳（造出来的用例要么
// 靠调度恰好，要么靠一次阻塞式的加锁）。**store 层的那条路径由 store 自己的
// 用例直接调 OpenWallet 钉住**（internal/ledger/store 的 TestOpenWalletUnknownUser）；
// 这里钉的是「它冒到命令行这一层时算不算冲突」——也就是 exit.go 里那条登记，
// 而那正是这条用例能唯一喊出来的东西：把 store.ErrNoSuchUser 从 conflictErrors
// 里拿掉，它立刻红。
func TestWalletOpenStoreMissingUserExitCode(t *testing.T) {
	// 造一个与 store 返回的形状一致的错误：哨兵包在一条带用户 id 的消息里，
	// 驱动的原文留在链上（见 store.missingUserError）。
	err := fmt.Errorf("%w: 用户 %d 在 users 里没有对应的一行: %w",
		store.ErrNoSuchUser, int64(900000000000000), errors.New("SQLSTATE 23503"))

	if !errors.Is(err, store.ErrNoSuchUser) {
		t.Fatalf("错误链里没有 store.ErrNoSuchUser：%v", err)
	}
	if code := exitCode(err); code != exitConflict {
		t.Errorf("退出码 = %d，期望 %d（要写的那一行引用的用户不存在）", code, exitConflict)
	}
}

// ── wallet credit ──────────────────────────────────────────────────────

// 参数检查同样发生在**连库之前**：这些用例不需要库。
func TestWalletCreditRejectsBadArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"没有 --user", []string{"--amount", "1", "--idempotency-key", "o"}, "缺少 --user"},
		{"--user 只有空白", []string{"--user", "  ", "--amount", "1", "--idempotency-key", "o"}, "缺少 --user"},
		{"没有 --amount", []string{"--user", "1", "--idempotency-key", "o"}, "缺少 --amount"},
		// 外部单据号必填且没有默认值：给了默认值，一次「忘了写 --ref」就会变成
		// 一笔与上一次互不相干的入账，而两笔在账面上都合法。
		//
		// 这两条用**规范名**写，写不出键名的那条错误说的也是规范名，所以两处
		// 对得上；下面那些用 --idempotency-key 的走的是同一条路（别名），
		// 顺带证明它仍然可用。
		{"没有 --ref", []string{"--user", "1", "--amount", "1"}, "缺少 --ref"},
		{"--ref 只有空白", []string{"--user", "1", "--amount", "1", "--ref", "  "}, "缺少 --ref"},
		// 控制字符：键既算交易号又进备注，而备注会把 \r\n\t 换成空格——
		// `ORD\nX` 与 `ORD X` 因此是两笔各自合法、备注却一模一样的入账。
		{"--idempotency-key 含换行", []string{"--user", "1", "--amount", "1", "--idempotency-key", "ORD\nX"}, "控制字符"},
		{"--idempotency-key 含制表符", []string{"--user", "1", "--amount", "1", "--idempotency-key", "ORD\tX"}, "控制字符"},
		// 备注那边只清洗 \r\n\t，这一个会原样落进备注，所以判据要比它宽。
		{"--idempotency-key 含其它控制字符", []string{"--user", "1", "--amount", "1", "--idempotency-key", "ORD\x07X"}, "控制字符"},
		{"金额不是数字", []string{"--user", "1", "--amount", "abc", "--idempotency-key", "o"}, "不是金额"},
		{"金额为零", []string{"--user", "1", "--amount", "0", "--idempotency-key", "o"}, "必须为正数"},
		{"金额为负", []string{"--user", "1", "--amount", "-1", "--idempotency-key", "o"}, "必须为正数"},
		// 小数位比列多：money.Parse 会四舍五入，而入账金额是「敲进去多少就
		// 记多少」的数，静默改掉最后一位会让账本与单据对不上。
		{"小数位过多", []string{"--user", "1", "--amount", "1.000000001", "--idempotency-key", "o"}, "最多 8 位小数"},
		{"资产不认识", []string{"--user", "1", "--amount", "1", "--idempotency-key", "o", "--asset", "USDT"}, "不受支持"},
		{"--asset 给了但为空", []string{"--user", "1", "--amount", "1", "--idempotency-key", "o", "--asset", ""}, "--asset 不能为空"},
		{"--gateway 给了但为空", []string{"--user", "1", "--amount", "1", "--idempotency-key", "o", "--gateway", ""}, "--gateway 不能为空"},
		{"通道标识含冒号", []string{"--user", "1", "--amount", "1", "--idempotency-key", "o", "--gateway", "bad:name"}, "不是合法的通道标识"},
		{"通道标识含空格", []string{"--user", "1", "--amount", "1", "--idempotency-key", "o", "--gateway", "bad name"}, "不是合法的通道标识"},
		{"多出位置参数", []string{"--user", "1", "--amount", "1", "--idempotency-key", "o", "extra"}, "不接受位置参数"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runWalletCredit(tc.args)
			if err == nil {
				t.Fatal("期望报错，却通过了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息 %q 里没有 %q", err.Error(), tc.want)
			}
			if code := exitCode(err); code != exitUsage {
				t.Errorf("退出码 = %d，期望 %d（参数错）", code, exitUsage)
			}
		})
	}
}

// 小数位判据看的是**有效位数**，不是小数点后的字符个数。
//
// `1.000000000` 与 `1` 是同一个数，列存得下，拒它会让文案里那句
// 「多出来的位存不进去」变成一句不成立的话。
func TestParseCreditAmountTrailingZeros(t *testing.T) {
	amount, err := parseCreditAmount("1.000000000")
	if err != nil {
		t.Fatalf("末尾是 0 的小数位不该被拒: %v", err)
	}
	if amount.String() != "1.00000000" {
		t.Errorf("解析结果 = %s，期望 1.00000000", amount)
	}

	// 第 9 位起还有非零：仍然拒，且仍然是参数错。
	for _, plain := range []string{"1.000000001", "1.0000000010", "0.000000001"} {
		bad, badErr := parseCreditAmount(plain)
		if badErr == nil {
			t.Errorf("%q 应当被拒，却得到 %s", plain, bad)
			continue
		}
		if !strings.Contains(badErr.Error(), "最多 8 位小数") {
			t.Errorf("%q 的错误信息 %q 里没有「最多 8 位小数」", plain, badErr)
		}
		if code := exitCode(badErr); code != exitUsage {
			t.Errorf("%q 的退出码 = %d，期望 %d（参数错）", plain, code, exitUsage)
		}
	}
}

// 来源名归一成小写：科目按字符串比对，MANUAL 与 manual 不能是两个科目。
func TestParseGatewayFlagLowercases(t *testing.T) {
	for _, given := range []string{"manual", "MANUAL", "Manual", "  MANUAL  "} {
		fs := newFlagSet("wallet credit", flag.ContinueOnError)
		gateway := fs.String("gateway", manualGateway, "")
		if err := fs.Parse([]string{"--gateway", given}); err != nil {
			t.Fatalf("解析 %q 失败: %v", given, err)
		}
		got, err := parseGatewayFlag(fs, *gateway)
		if err != nil {
			t.Fatalf("--gateway %q 被拒: %v", given, err)
		}
		if got != manualGateway {
			t.Errorf("--gateway %q 归一成 %q，期望 %q", given, got, manualGateway)
		}
	}
}

// 幂等键到交易号的映射必须**稳定**，钉死到一个字面量上。
//
// 换掉这个映射（加盐、换哈希、改域分隔前缀）不会让任何用例变红，却会让
// 已经入过账的幂等键算出另一个交易号——于是同一条命令重跑时会**再入一次
// 账**，而那正是 --idempotency-key 要防的事。所以这里钉住字面量：改了映射就得改这
// 一行，而改这一行的人必须解释旧键怎么办。
func TestTxnIDForOrderIsPinned(t *testing.T) {
	const order = "ORD-1"
	const want = "31a99512-14f5-8d96-81c8-05a21eb582bb"

	got := txnIDForOrder(order)
	if string(got) != want {
		t.Errorf("txnIDForOrder(%q) = %s，期望 %s（改了映射就要说清旧幂等键怎么办）", order, got, want)
	}
	if txnIDForOrder(order) != got {
		t.Error("同一个幂等键两次算出的交易号不同")
	}
	if txnIDForOrder("ORD-2") == got {
		t.Error("不同的幂等键算出了同一个交易号")
	}
	if _, err := ledger.ParseTxnID(string(got)); err != nil {
		t.Errorf("算出来的交易号不是合法 uuid: %v", err)
	}
}

// walletBalance 读一个用户 CNY 钱包的快照余额。
func walletBalance(t *testing.T, db *sql.DB, userID int64) string {
	t.Helper()
	var balance string
	if err := db.QueryRowContext(t.Context(),
		`SELECT balance FROM user_wallets WHERE user_id = $1 AND asset = 'CNY'`,
		userID).Scan(&balance); err != nil {
		t.Fatalf("读钱包余额失败: %v", err)
	}
	return balance
}

// txnRows 读一个交易写下的分录，每条压成「科目|金额|类型|备注」。
func txnRows(t *testing.T, db *sql.DB, txnID ledger.TxnID) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `
		SELECT account || '|' || amount::text || '|' || entry_type || '|' || coalesce(memo, '')
		  FROM ledger_entries WHERE txn_id = $1 ORDER BY account`, string(txnID))
	if err != nil {
		t.Fatalf("读分录失败: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("扫描分录失败: %v", err)
		}
		out = append(out, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历分录失败: %v", err)
	}
	return out
}

// 入账命令走完整条链路：开户 → 入账 → 重放 → 同键异参。
//
// 金额、科目、备注与余额逐条钉死：这一层动钱，而「写成功了但写反了方向」
// 「加了两次」都不会报错。
func TestWalletCreditCommand(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	userID := createTestUser(t)
	ref := strconv.FormatInt(userID, 10)
	order := uniqueName("order")
	txnID := txnIDForOrder(order)
	// 输出里点名的是用户名而不是 ID（见 docs/cli-output.md 的「实体只点一次
	// 身份」），所以这里把它查出来，断言才有得比。
	owner, err := auth.New(db).ResolveUser(t.Context(), ref)
	if err != nil {
		t.Fatalf("解析用户失败: %v", err)
	}

	// 还没开户：入账要被拒，且一条分录都不留。缺钱包行与余额不足是两件事
	// （前者先跑 wallet open，后者要用户充值），所以判据是哨兵而不是「报错了」。
	noWalletErr := runCaptured(t, func() error {
		return runWalletCredit([]string{"--user", ref, "--amount", "100", "--idempotency-key", order})
	})
	if !errors.Is(noWalletErr, store.ErrNoWallet) {
		t.Fatalf("没有钱包时应当返回 store.ErrNoWallet，得到 %v", noWalletErr)
	}
	if code := exitCode(noWalletErr); code != exitConflict {
		t.Errorf("退出码 = %d，期望 %d（要改的那一行不存在）", code, exitConflict)
	}
	if rows := txnRows(t, db, txnID); len(rows) != 0 {
		t.Fatalf("被拒的入账留下了 %d 条分录：%v", len(rows), rows)
	}

	if openErr := runCaptured(t, func() error {
		return runWalletCreate([]string{"--user", ref})
	}); openErr != nil {
		t.Fatalf("开户失败: %v", openErr)
	}

	var creditErr error
	out, _ := captureStreams(t, func() {
		creditErr = runWalletCredit([]string{"--user", ref, "--amount", "100", "--idempotency-key", order})
	})
	if creditErr != nil {
		t.Fatalf("入账失败: %v", creditErr)
	}
	for _, want := range []string{
		"已入账", "用户 " + owner.Username, "100.00000000", "幂等键 " + order, string(txnID),
		"借 " + ledger.UserAccount(userID).String(), "贷 " + ledger.PaymentGatewayAccount("manual").String(),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("入账输出里没有 %q：\n%s", want, out)
		}
	}

	if balance := walletBalance(t, db, userID); balance != "100.00000000" {
		t.Errorf("余额 = %s，期望 100.00000000", balance)
	}
	wantRows := []string{
		ledger.PaymentGatewayAccount("manual").String() + "|-100.00000000|topup|充值（单据 " + order + "）",
		ledger.UserAccount(userID).String() + "|100.00000000|topup|",
	}
	if got := txnRows(t, db, txnID); !slices.Equal(got, wantRows) {
		t.Errorf("分录 = %v，期望 %v", got, wantRows)
	}

	// 快照与账本两侧都是 100：入账走的是账本，快照只是跟着动。
	authoritative, err := store.New(db).AccountBalance(
		t.Context(), ledger.UserAccount(userID), ledger.AssetCNY)
	if err != nil {
		t.Fatalf("读权威余额失败: %v", err)
	}
	if authoritative.String() != "100.00000000" {
		t.Errorf("账本侧余额 = %s，期望 100.00000000", authoritative)
	}

	// 重放同一条命令：退出 0，但一个字都不改，且输出必须说清是重放。
	var replayErr error
	replay, _ := captureStreams(t, func() {
		replayErr = runWalletCredit([]string{"--user", ref, "--amount", "100", "--idempotency-key", order})
	})
	if replayErr != nil {
		t.Fatalf("重放不该报错: %v", replayErr)
	}
	for _, want := range []string{"已经入过账", "未改动", "幂等键 " + order} {
		if !strings.Contains(replay, want) {
			t.Errorf("重放输出里没有 %q：\n%s", want, replay)
		}
	}
	if strings.Contains(replay, "已入账：") {
		t.Errorf("重放不该说成「已入账」：\n%s", replay)
	}
	if balance := walletBalance(t, db, userID); balance != "100.00000000" {
		t.Errorf("重放之后余额 = %s，期望仍是 100.00000000", balance)
	}
	if got := txnRows(t, db, txnID); !slices.Equal(got, wantRows) {
		t.Errorf("重放之后分录 = %v，期望仍是 %v", got, wantRows)
	}

	// 同一个幂等键配不同的金额不是重放，是冲突：退出 4，一笔都不写。
	conflictErr := runCaptured(t, func() error {
		return runWalletCredit([]string{"--user", ref, "--amount", "50", "--idempotency-key", order})
	})
	if !errors.Is(conflictErr, store.ErrTxnConflict) {
		t.Fatalf("同键异参应当返回 store.ErrTxnConflict，得到 %v", conflictErr)
	}
	if code := exitCode(conflictErr); code != exitConflict {
		t.Errorf("退出码 = %d，期望 %d（去看库里那笔是什么）", code, exitConflict)
	}
	if balance := walletBalance(t, db, userID); balance != "100.00000000" {
		t.Errorf("冲突之后余额 = %s，期望仍是 100.00000000", balance)
	}
	if got := txnRows(t, db, txnID); !slices.Equal(got, wantRows) {
		t.Errorf("冲突之后分录 = %v，期望仍是 %v", got, wantRows)
	}

	// 另一个幂等键 + 显式通道：钱加到同一个余额上，对家换一个科目。
	secondOrder := uniqueName("order")
	if err := runCaptured(t, func() error {
		return runWalletCredit([]string{
			"--user", ref, "--amount", "50", "--idempotency-key", secondOrder, "--gateway", "alipay",
		})
	}); err != nil {
		t.Fatalf("第二笔入账失败: %v", err)
	}
	if balance := walletBalance(t, db, userID); balance != "150.00000000" {
		t.Errorf("两笔之后余额 = %s，期望 150.00000000", balance)
	}
	secondRows := txnRows(t, db, txnIDForOrder(secondOrder))
	wantSecond := []string{
		ledger.PaymentGatewayAccount("alipay").String() + "|-50.00000000|topup|充值（单据 " + secondOrder + "）",
		ledger.UserAccount(userID).String() + "|50.00000000|topup|",
	}
	if !slices.Equal(secondRows, wantSecond) {
		t.Errorf("第二笔的分录 = %v，期望 %v", secondRows, wantSecond)
	}

	// 来源名大小写归一：`MANUAL` 落进的仍是 `payment_gateway:manual`，
	// 不会在它旁边再开一个只差大小写的科目。
	upperOrder := uniqueName("order")
	if err := runCaptured(t, func() error {
		return runWalletCredit([]string{
			"--user", ref, "--amount", "5", "--idempotency-key", upperOrder, "--gateway", "MANUAL",
		})
	}); err != nil {
		t.Fatalf("--gateway MANUAL 的入账失败: %v", err)
	}
	upperRows := txnRows(t, db, txnIDForOrder(upperOrder))
	if len(upperRows) != 2 || !strings.HasPrefix(upperRows[0], ledger.PaymentGatewayAccount("manual").String()+"|") {
		t.Errorf("--gateway MANUAL 的分录 = %v，期望对家是 payment_gateway:manual", upperRows)
	}
	var upperAccounts int
	if err := db.QueryRowContext(t.Context(),
		`SELECT count(*) FROM ledger_entries WHERE account = 'payment_gateway:MANUAL'`).Scan(&upperAccounts); err != nil {
		t.Fatalf("数大小写科目失败: %v", err)
	}
	if upperAccounts != 0 {
		t.Errorf("库里多出 %d 条 payment_gateway:MANUAL 的分录", upperAccounts)
	}
	if balance := walletBalance(t, db, userID); balance != "155.00000000" {
		t.Errorf("三笔之后余额 = %s，期望 155.00000000", balance)
	}
}

// 用户不存在时退出码是「要查的那一行不存在」，与 wallet open 一致。
func TestWalletCreditUnknownUserExitCode(t *testing.T) {
	testDB(t)
	setupEnv(t)

	err := runCaptured(t, func() error {
		return runWalletCredit([]string{
			"--user", uniqueName("nobody"), "--amount", "1", "--idempotency-key", uniqueName("order"),
		})
	})
	if err == nil {
		t.Fatal("不存在的用户应当报错")
	}
	if !errors.Is(err, auth.ErrUserNotFound) {
		t.Errorf("错误链里没有 auth.ErrUserNotFound：%v", err)
	}
	if code := exitCode(err); code != exitConflict {
		t.Errorf("退出码 = %d，期望 %d（要查的那一行不存在）", code, exitConflict)
	}
}

// ── wallet adjust ──────────────────────────────────────────────────────

// 参数检查同样发生在**连库之前**：这些用例不需要库。
func TestWalletAdjustRejectsBadArgs(t *testing.T) {
	// 一条「别的都给全」的基线：缺哪一项就把它拿掉。挨着写五遍全量参数，
	// 改动一处旗标名时要改的地方会多到看不住。
	base := map[string]string{
		"--user": "1", "--amount": "-1", "--memo": "误扣补回", "--ref": "ADJ-1", "--by": "ops",
	}
	names := []string{"--user", "--amount", "--memo", "--ref", "--by"}
	args := func(drop string, extra ...string) []string {
		out := make([]string, 0, 2*len(names)+len(extra))
		for _, name := range names {
			if name == drop {
				continue
			}
			out = append(out, name, base[name])
		}
		return append(out, extra...)
	}

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"没有 --user", args("--user"), "缺少 --user"},
		{"--user 只有空白", args("--user", "--user", "   "), "缺少 --user"},
		{"没有 --amount", args("--amount"), "缺少 --amount"},
		{"--amount 只有空白", args("--amount", "--amount", "  "), "缺少 --amount"},
		// 原因与操作人都必填，且都在连库之前判：调账是唯一没有业务依据的分录，
		// 一条不记名、不写原因的调账事后谁也解释不了（见 runWalletAdjust）。
		{"没有 --memo", args("--memo"), "缺少 --memo"},
		{"--memo 只有空白", args("--memo", "--memo", "  "), "缺少 --memo"},
		{"没有 --ref", args("--ref"), "缺少 --ref"},
		{"--ref 只有空白", args("--ref", "--ref", "  "), "缺少 --ref"},
		{"没有 --by", args("--by"), "缺少 --by"},
		{"--by 只有空白", args("--by", "--by", "  "), "缺少 --by"},
		{"金额不是数字", args("--amount", "--amount", "abc"), "不是金额"},
		{"小数位过多", args("--amount", "--amount", "1.000000001"), "最多 8 位小数"},
		{"--ref 含换行", args("--ref", "--ref", "ADJ\n1"), "控制字符"},
		{"--ref 含制表符", args("--ref", "--ref", "ADJ\t1"), "控制字符"},
		{"--ref 含其它控制字符", args("--ref", "--ref", "ADJ\x071"), "控制字符"},
		// 原因也会进回显，而落库那一份过了 sanitizeMemo：带控制字符时两边不是
		// 同一个串，换行还会把「一栏一行」的回显折出一行没有栏名的续行。
		{"--memo 含换行", args("--memo", "--memo", "误扣\n补回"), "控制字符"},
		{"--memo 含制表符", args("--memo", "--memo", "误扣\t补回"), "控制字符"},
		{"--memo 含 ESC", args("--memo", "--memo", "误扣\x1b补回"), "控制字符"},
		// 这三个旗标是别的命令上的：调账只认自己那六个。判据落在「认不认这个
		// 旗标」上，不是落在文案上——flag 包那句是英文的。
		{"--asset 在 adjust 上不存在", args("", "--asset", "CNY"), "asset"},
		{"--order 只在 credit 上", args("", "--order", "ORD-1"), "order"},
		{"--idempotency-key 只在 credit 上", args("", "--idempotency-key", "ORD-1"), "idempotency-key"},
		{"多出位置参数", args("", "extra"), "不接受位置参数"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runWalletAdjust(tc.args)
			if err == nil {
				t.Fatal("期望报错，却通过了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息 %q 里没有 %q", err.Error(), tc.want)
			}
			if code := exitCode(err); code != exitUsage {
				t.Errorf("退出码 = %d，期望 %d（参数错）", code, exitUsage)
			}
		})
	}
}

// 控制字符那条错误要点名这条命令真的认的旗标。
//
// credit 与 adjust 的幂等键各是一个旗标，而 credit 上还挂着一个旧名
// --idempotency-key。判据过去把那个旧名写死在文案里；adjust 上没有这个旧名，
// 报出来就是一条照着重敲走不通的路。这一条钉住那个名字来自调用点。
func TestWalletAdjustControlCharErrorNamesTheFlag(t *testing.T) {
	err := runWalletAdjust([]string{
		"--user", "1", "--amount", "-1", "--memo", "m", "--ref", "ADJ\x071", "--by", "ops",
	})
	if err == nil {
		t.Fatal("键里含控制字符时应当报错")
	}
	if !strings.Contains(err.Error(), "--ref") {
		t.Errorf("错误信息 %q 里没有 --ref", err.Error())
	}
	if strings.Contains(err.Error(), "--idempotency-key") {
		t.Errorf("adjust 上没有 --idempotency-key 这个写法，错误里不该出现它：%q", err.Error())
	}
}

// credit 上写旧名时，那条错误仍然点名规范名 --ref。
//
// 与上一条是一对：方向都是「报出能照着敲的那个名字」，而 credit 上挂着旧名
// --idempotency-key。这是对外可见的行为（脚本照着错误信息改参数），所以把它钉住，
// 免得以后被人当成写错了名字顺手「修」掉。
func TestWalletCreditControlCharErrorNamesCanonicalFlag(t *testing.T) {
	err := runWalletCredit([]string{
		"--user", "1", "--amount", "1", "--idempotency-key", "ORD\nX",
	})
	if err == nil {
		t.Fatal("键里含控制字符时应当报错")
	}
	if !strings.Contains(err.Error(), "--ref") {
		t.Errorf("错误信息 %q 里没有规范名 --ref", err.Error())
	}
	if strings.Contains(err.Error(), "--idempotency-key") {
		t.Errorf("错误信息该报规范名，不该出现旧名：%q", err.Error())
	}
}

// 调账的幂等键到交易号的映射必须**稳定**，钉死到一个字面量上。
//
// 与 TestTxnIDForOrderIsPinned 同一条理由，另外还多一件：两条命令的键**不能**
// 映射到同一个交易号。单据号来自同一个物理来源（银行流水号、回单号），撞上了
// 就意味着「先 credit 再 adjust 同一个单号」里的第二笔会被判成冲突或重放，
// 而它本该是一笔新的调账。
func TestTxnIDForAdjustmentIsPinned(t *testing.T) {
	const ref = "ADJ-1"
	const want = "6b409e53-e1c2-870e-879a-ad20804381f5"

	got := txnIDForAdjustment(ref)
	if string(got) != want {
		t.Errorf("txnIDForAdjustment(%q) = %s，期望 %s（改了映射就要说清旧幂等键怎么办）", ref, got, want)
	}
	if txnIDForAdjustment(ref) != got {
		t.Error("同一个幂等键两次算出的交易号不同")
	}
	if txnIDForAdjustment("ADJ-2") == got {
		t.Error("不同的幂等键算出了同一个交易号")
	}
	if txnIDForOrder(ref) == got {
		t.Error("调账与入账对同一个单据号算出了同一个交易号：两条命令会认成同一笔交易")
	}
	if _, err := ledger.ParseTxnID(string(got)); err != nil {
		t.Errorf("算出来的交易号不是合法 uuid: %v", err)
	}
}

// 用户不存在时退出码是「要查的那一行不存在」——两个用户旗标各自都算。
//
// --by 与 --user 走的是同一套解析，但报错要说得出是哪一个没对上：两个都认
// 「ID 或用户名」的命令里，只报一句「找不到用户」的话，操作者得自己猜是钱包
// 的主人写错了还是操作人写错了。
func TestWalletAdjustUnknownUserExitCode(t *testing.T) {
	testDB(t)
	setupEnv(t)

	ref := strconv.FormatInt(createTestUser(t), 10)
	by := createTestUserID(t)

	byErr := runCaptured(t, func() error {
		return runWalletAdjust([]string{
			"--user", ref, "--amount", "-1", "--memo", "m",
			"--ref", uniqueName("adj"), "--by", uniqueName("nobody"),
		})
	})
	if !errors.Is(byErr, auth.ErrUserNotFound) {
		t.Fatalf("--by 认不出用户时应当返回 auth.ErrUserNotFound，得到 %v", byErr)
	}
	if !strings.Contains(byErr.Error(), "--by") {
		t.Errorf("错误信息 %q 里没有点出 --by", byErr)
	}
	if code := exitCode(byErr); code != exitConflict {
		t.Errorf("退出码 = %d，期望 %d（要查的那一行不存在）", code, exitConflict)
	}

	ownerErr := runCaptured(t, func() error {
		return runWalletAdjust([]string{
			"--user", uniqueName("nobody"), "--amount", "-1", "--memo", "m",
			"--ref", uniqueName("adj"), "--by", by,
		})
	})
	if !errors.Is(ownerErr, auth.ErrUserNotFound) {
		t.Fatalf("--user 认不出用户时应当返回 auth.ErrUserNotFound，得到 %v", ownerErr)
	}
	if !strings.Contains(ownerErr.Error(), "--user") {
		t.Errorf("错误信息 %q 里没有点出 --user", ownerErr)
	}
	if code := exitCode(ownerErr); code != exitConflict {
		t.Errorf("退出码 = %d，期望 %d（要查的那一行不存在）", code, exitConflict)
	}
}

// 调账命令走完整条链路：开户 → 入账 → 调减 → 重放 → 同键异参。
//
// 金额、科目、方向、备注与余额逐条钉死：这一层动钱，而「写成功了但方向写反了」
// 「加了两次」都不会报错。
func TestWalletAdjustCommand(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	userID := createTestUser(t)
	ref := strconv.FormatInt(userID, 10)
	operatorID := createTestUserID(t)
	adjustRef := uniqueName("adj")
	txnID := txnIDForAdjustment(adjustRef)

	// 原因里特意带一对全角括号：备注末尾那段「（操作人 …，单据 …）」是**锚在串尾**
	// 的固定后缀，原因里出现同样形状的括号是允许的，而这个用例把「原样进账本」
	// 钉在这一串上——清洗（只换 \r\n\t）与拼接都不许动它内部一个字符。
	const adjustMemo = "误扣补回（第二次）"

	// 输出里点名的是用户名而不是 ID（见 docs/cli-output.md 的「实体只点一次
	// 身份」），两个身份各查一遍，断言才有得比。
	owner, err := auth.New(db).ResolveUser(t.Context(), ref)
	if err != nil {
		t.Fatalf("解析用户失败: %v", err)
	}
	operator, err := auth.New(db).ResolveUser(t.Context(), operatorID)
	if err != nil {
		t.Fatalf("解析操作人失败: %v", err)
	}
	if owner.Username == operator.Username {
		t.Fatal("被调用户与操作人撞成了同一个人：这一条要证明两个身份分得开")
	}

	adjustArgs := func(amount string) []string {
		return []string{
			"--user", ref, "--amount", amount, "--memo", adjustMemo,
			"--ref", adjustRef, "--by", operatorID,
		}
	}

	// 还没开户：调账要被拒，且一条分录都不留。缺钱包行与余额不足是两件事
	// （前者先跑 wallet create，后者要先把余额调够），所以判据是哨兵。
	noWalletErr := runCaptured(t, func() error { return runWalletAdjust(adjustArgs("-40")) })
	if !errors.Is(noWalletErr, store.ErrNoWallet) {
		t.Fatalf("没有钱包时应当返回 store.ErrNoWallet，得到 %v", noWalletErr)
	}
	if code := exitCode(noWalletErr); code != exitConflict {
		t.Errorf("退出码 = %d，期望 %d（要改的那一行不存在）", code, exitConflict)
	}
	if rows := txnRows(t, db, txnID); len(rows) != 0 {
		t.Fatalf("被拒的调账留下了 %d 条分录：%v", len(rows), rows)
	}

	if openErr := runCaptured(t, func() error {
		return runWalletCreate([]string{"--user", ref})
	}); openErr != nil {
		t.Fatalf("开户失败: %v", openErr)
	}
	if creditErr := runCaptured(t, func() error {
		return runWalletCredit([]string{
			"--user", ref, "--amount", "100", "--ref", uniqueName("order"),
		})
	}); creditErr != nil {
		t.Fatalf("入账失败: %v", creditErr)
	}

	// 调减 40：余额从 100 到 60，分录是 贷 user / 借 platform_revenue
	// （负数是「把记多的钱扣回来」，方向与调增相反）。
	var adjustErr error
	out, _ := captureStreams(t, func() { adjustErr = runWalletAdjust(adjustArgs("-40")) })
	if adjustErr != nil {
		t.Fatalf("调账失败: %v", adjustErr)
	}
	for _, want := range []string{
		"已调账用户 " + owner.Username,
		"余额", "100.00000000 → 60.00000000",
		"调整", "-40.00000000",
		"原因", adjustMemo,
		"操作人", operator.Username,
		"幂等键", adjustRef,
		string(txnID),
		"贷 " + ledger.UserAccount(userID).String(),
		"借 " + ledger.PlatformRevenueAccount().String(),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("调账输出里没有 %q：\n%s", want, out)
		}
	}

	if balance := walletBalance(t, db, userID); balance != "60.00000000" {
		t.Errorf("余额 = %s，期望 60.00000000", balance)
	}
	wantRows := []string{
		ledger.PlatformRevenueAccount().String() + "|40.00000000|adjustment|" + adjustMemo + "（操作人 " + operator.Username + "，单据 " + adjustRef + "）",
		ledger.UserAccount(userID).String() + "|-40.00000000|adjustment|" + adjustMemo + "（操作人 " + operator.Username + "，单据 " + adjustRef + "）",
	}
	if got := txnRows(t, db, txnID); !slices.Equal(got, wantRows) {
		t.Errorf("分录 = %v，期望 %v", got, wantRows)
	}

	// 快照与账本两侧都是 60：调账走的是账本，快照只是跟着动。
	authoritative, err := store.New(db).AccountBalance(
		t.Context(), ledger.UserAccount(userID), ledger.AssetCNY)
	if err != nil {
		t.Fatalf("读权威余额失败: %v", err)
	}
	if authoritative.String() != "60.00000000" {
		t.Errorf("账本侧余额 = %s，期望 60.00000000", authoritative)
	}

	// 重放同一条命令：退出 0，一个字都不改，且**不打**余额那一对——那两行会让
	// 人以为这一次真的动了钱，而它一分钱都没动（LESSONS 第 16 条）。
	var replayErr error
	replay, _ := captureStreams(t, func() { replayErr = runWalletAdjust(adjustArgs("-40")) })
	if replayErr != nil {
		t.Fatalf("重放不该报错: %v", replayErr)
	}
	for _, want := range []string{"已经调过账", "未改动", "幂等键 " + adjustRef} {
		if !strings.Contains(replay, want) {
			t.Errorf("重放输出里没有 %q：\n%s", want, replay)
		}
	}
	if strings.Contains(replay, "已调账用户") || strings.Contains(replay, "余额") {
		t.Errorf("重放不该打一份「余额从多少到多少」：\n%s", replay)
	}
	if balance := walletBalance(t, db, userID); balance != "60.00000000" {
		t.Errorf("重放之后余额 = %s，期望仍是 60.00000000", balance)
	}
	if got := txnRows(t, db, txnID); !slices.Equal(got, wantRows) {
		t.Errorf("重放之后分录 = %v，期望仍是 %v", got, wantRows)
	}

	// 同一个幂等键配不同的金额不是重放，是冲突：退出 4，一笔都不写。
	conflictErr := runCaptured(t, func() error { return runWalletAdjust(adjustArgs("-20")) })
	if !errors.Is(conflictErr, store.ErrTxnConflict) {
		t.Fatalf("同键异参应当返回 store.ErrTxnConflict，得到 %v", conflictErr)
	}
	if code := exitCode(conflictErr); code != exitConflict {
		t.Errorf("退出码 = %d，期望 %d（去看库里那笔是什么）", code, exitConflict)
	}
	if balance := walletBalance(t, db, userID); balance != "60.00000000" {
		t.Errorf("冲突之后余额 = %s，期望仍是 60.00000000", balance)
	}
	if got := txnRows(t, db, txnID); !slices.Equal(got, wantRows) {
		t.Errorf("冲突之后分录 = %v，期望仍是 %v", got, wantRows)
	}
}

// 调增、零金额与「结果会变负」三档：都是 ledger.Adjustment 与该写路径的判据，
// 命令行一条都不重复实现——所以它们的退出码不是参数错。
//
// 零金额那条尤其要说清：命令行**不判**零，拒它的是 Adjustment 那句「调账金额
// 为零，没有需要修正的东西」。这不是漏了，是口径（同一件事只在一处判）。
func TestWalletAdjustAmountEdges(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	userID := createTestUser(t)
	ref := strconv.FormatInt(userID, 10)
	by := createTestUserID(t)
	if openErr := runCaptured(t, func() error {
		return runWalletCreate([]string{"--user", ref})
	}); openErr != nil {
		t.Fatalf("开户失败: %v", openErr)
	}

	adjust := func(amount string) (string, error) {
		var err error
		out, _ := captureStreams(t, func() {
			err = runWalletAdjust([]string{
				"--user", ref, "--amount", amount, "--memo", "按线下对账结果平掉差额",
				"--ref", uniqueName("adj"), "--by", by,
			})
		})
		return out, err
	}

	// 调增：空钱包上也能做，余额从 0 到 10。分录方向与调减相反
	// （借 user / 贷 platform_revenue）——对家是平台认下的这笔差额。
	out, err := adjust("10")
	if err != nil {
		t.Fatalf("调增失败: %v", err)
	}
	for _, want := range []string{"0.00000000 → 10.00000000", "借 " + ledger.UserAccount(userID).String()} {
		if !strings.Contains(out, want) {
			t.Errorf("调增输出里没有 %q：\n%s", want, out)
		}
	}
	if balance := walletBalance(t, db, userID); balance != "10.00000000" {
		t.Errorf("调增之后余额 = %s，期望 10.00000000", balance)
	}

	// 结果为负：拒绝，一笔都不写。库里那道 `UPDATE … AND balance + delta >= 0`
	// 拦的，命令行只把它那句「余额不足以扣」换成调账这一侧的说法。
	shortRef := uniqueName("adj")
	negativeErr := runCaptured(t, func() error {
		return runWalletAdjust([]string{
			"--user", ref, "--amount", "-11", "--memo", "多扣了要退回去",
			"--ref", shortRef, "--by", by,
		})
	})
	if !errors.Is(negativeErr, store.ErrInsufficientBalance) {
		t.Fatalf("调成负数应当返回 store.ErrInsufficientBalance，得到 %v", negativeErr)
	}
	if !strings.Contains(negativeErr.Error(), "不允许把余额调成负数") {
		t.Errorf("错误信息 %q 里没有「不允许把余额调成负数」", negativeErr)
	}
	if rows := txnRows(t, db, txnIDForAdjustment(shortRef)); len(rows) != 0 {
		t.Errorf("被拒的调账留下了 %d 条分录：%v", len(rows), rows)
	}
	if balance := walletBalance(t, db, userID); balance != "10.00000000" {
		t.Errorf("被拒之后余额 = %s，期望仍是 10.00000000", balance)
	}

	// 结果正好是零：允许。余额为 0 不违反那条 CHECK，而「把钱正好扣光」是
	// 一次合法的修正——把它一起拒掉会让人以为得留一分钱。
	if _, err := adjust("-10"); err != nil {
		t.Fatalf("把余额调成正好为零应当允许: %v", err)
	}
	if balance := walletBalance(t, db, userID); balance != "0.00000000" {
		t.Errorf("调成零之后余额 = %s，期望 0.00000000", balance)
	}

	// 零金额：由 Adjustment 拒，退出码因此是「其它失败」而不是参数错。
	zeroErr := runCaptured(t, func() error {
		return runWalletAdjust([]string{
			"--user", ref, "--amount", "0", "--memo", "什么都没改",
			"--ref", uniqueName("adj"), "--by", by,
		})
	})
	if zeroErr == nil {
		t.Fatal("金额为零时应当报错")
	}
	if !strings.Contains(zeroErr.Error(), "为零") {
		t.Errorf("错误信息 %q 里没有提到金额为零", zeroErr)
	}
	if code := exitCode(zeroErr); code != exitFailure {
		t.Errorf("退出码 = %d，期望 %d（这条判据在账本那一层，不是参数错）", code, exitFailure)
	}
}

// 余额贴着金额表示上界时的那条早退，连同它的代价。
//
// `after, fits := money.SumChecked(...)` 判在**重放判定之前**：重放判定在
// PostTransaction 里面，命令行这一层看不到它。于是余额顶到上界之后，重放同一条调账
// 拿到的不是「已经调过账」，而是参数错。这是已知代价（见 runWalletAdjust 里那段
// 注释），不是回归——这一条把它钉住，顺带证明那条早退一笔都不写。
func TestWalletAdjustOverflowAtAmountBoundary(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	userID := createTestUser(t)
	ref := strconv.FormatInt(userID, 10)
	by := createTestUserID(t)
	if openErr := runCaptured(t, func() error {
		return runWalletCreate([]string{"--user", ref})
	}); openErr != nil {
		t.Fatalf("开户失败: %v", openErr)
	}

	adjust := func(amount, memo, key string) error {
		return runCaptured(t, func() error {
			return runWalletAdjust([]string{
				"--user", ref, "--amount", amount, "--memo", memo,
				"--ref", key, "--by", by,
			})
		})
	}
	credit := func(amount, key string) error {
		return runCaptured(t, func() error {
			return runWalletCredit([]string{
				"--user", ref, "--amount", amount, "--ref", key,
			})
		})
	}

	// 写完的东西收尾时删掉：这一格最后留下的余额放不进 money.Amount，而包里那些
	// 「列出全部钱包」「扫全表」的用例会把全库的余额逐个解析一遍——留着它，它们会
	// 跟着红（与 TestLedgerBalanceReportsOverflowInsteadOfClamping 同一条理由）。
	// 测试库跑完即丢，这里按交易号把这次造出来的删干净。
	created := []ledger.TxnID{}
	t.Cleanup(func() {
		for _, id := range created {
			if _, err := db.ExecContext(context.Background(),
				`DELETE FROM ledger_entries WHERE txn_id = $1`, string(id)); err != nil {
				t.Fatalf("清理用例的分录失败: %v", err)
			}
		}
		if _, err := db.ExecContext(context.Background(),
			`DELETE FROM user_wallets WHERE user_id = $1`, userID); err != nil {
			t.Fatalf("清理用例的钱包行失败: %v", err)
		}
	})

	// 先落一笔正常的调增，它的键下面要重放。
	const firstAmount = "1"
	replayRef := uniqueName("adj")
	if err := adjust(firstAmount, "先调一笔", replayRef); err != nil {
		t.Fatalf("第一次调增失败: %v", err)
	}
	created = append(created, txnIDForAdjustment(replayRef))

	// 把余额顶到上界：调增那 1 已经在里面了，所以补的是「上界减去现值」，
	// 补 92233720367.54775807 —— 1 + 92233720367.54775807 = 92233720368.54775807，
	// 也就是 int64 能表示的最后一个最小单位（money.Amount 是 int64 存最小单位的）。
	// 顺带断言一次现值：没顶到的话，下面两条判据都会变成空转。
	const boundary = "92233720368.54775807"
	topUpRef := uniqueName("order")
	if creditErr := credit("92233720367.54775807", topUpRef); creditErr != nil {
		t.Fatalf("把余额顶到上界失败: %v", creditErr)
	}
	created = append(created, txnIDForOrder(topUpRef))
	if balance := walletBalance(t, db, userID); balance != boundary {
		t.Fatalf("余额 = %s，期望顶到 %s：没顶到上界这一条就白测了", balance, boundary)
	}

	// 新键 + 正金额：现值加上它就超出 int64，在碰库之前拦下。
	overRef := uniqueName("adj")
	overErr := adjust("0.00000001", "把余额顶出上界", overRef)
	if overErr == nil {
		t.Fatal("调后余额超出金额可表示的范围时应当报错")
	}
	if !strings.Contains(overErr.Error(), "超出金额可表示的范围") {
		t.Errorf("错误信息 %q 里没有说明是超出表示范围", overErr)
	}
	if code := exitCode(overErr); code != exitUsage {
		t.Errorf("退出码 = %d，期望 %d（这一条判的是参数量级）", code, exitUsage)
	}
	if rows := txnRows(t, db, txnIDForAdjustment(overRef)); len(rows) != 0 {
		t.Errorf("被拒的调账留下了 %d 条分录：%v", len(rows), rows)
	}

	// 重放同一个键：这一条拿到的也是参数错，而不是「已经调过账」——早退排在
	// 重放判定前面。库里不该因此多出任何一行。
	replayErr := adjust(firstAmount, "先调一笔", replayRef)
	if replayErr == nil {
		t.Fatal("贴着上界重放时应当报错（重放判定排在早退之后，这是已知代价）")
	}
	if !strings.Contains(replayErr.Error(), "超出金额可表示的范围") {
		t.Errorf("贴着重放时的错误不是那条早退：%v", replayErr)
	}
	if rows := txnRows(t, db, txnIDForAdjustment(replayRef)); len(rows) != 2 {
		t.Errorf("重放之后第一次那笔的分录 = %d 条，期望仍是 2 条：%v", len(rows), rows)
	}
	if balance := walletBalance(t, db, userID); balance != boundary {
		t.Errorf("两次被拒之后余额 = %s，期望仍是 %s", balance, boundary)
	}

	// 顶**过**上界：账本的和已经放不进 money.Amount（库里的 numeric(20, 8) 存得
	// 下它，放不下的是 money），于是读余额那一步先失败——这是同一类问题的另一道
	// 门，而且它连金额都不用挑：任何金额、连重放都到不了重放判定。
	pastRef := uniqueName("order")
	if creditErr := credit("0.00000001", pastRef); creditErr != nil {
		t.Fatalf("顶过上界那一笔入账应当成功（和进的是 numeric，只在读回来时才放不下）: %v", creditErr)
	}
	created = append(created, txnIDForOrder(pastRef))
	pastErr := adjust("1", "顶过上界之后随便调一笔", uniqueName("adj"))
	if pastErr == nil {
		t.Fatal("余额的和放不进 money.Amount 时应当报错")
	}
	if !errors.Is(pastErr, money.ErrOverflow) {
		t.Errorf("错误链里没有 money.ErrOverflow：%v", pastErr)
	}
	// 退出码 1 而不是 2：这一条不是参数错，是库里的数读不出来。
	if code := exitCode(pastErr); code != exitFailure {
		t.Errorf("退出码 = %d，期望 %d（读余额失败，不是参数错）", code, exitFailure)
	}
	// 连重放也走不到重放判定：它同样在这一步失败，库里一行都不该多出来。
	pastReplayErr := adjust(firstAmount, "先调一笔", replayRef)
	if pastReplayErr == nil {
		t.Fatal("这一格上重放也应当报错（读余额排在重放判定之前）")
	}
	if rows := txnRows(t, db, txnIDForAdjustment(replayRef)); len(rows) != 2 {
		t.Errorf("重放之后第一次那笔的分录 = %d 条，期望仍是 2 条：%v", len(rows), rows)
	}
}

// ── wallet list ─────────────────────────────────────────────────────────

// TestWalletListRejectsPositionalArgs 走的是**不碰数据库**的那条路径（同
// TestWalletOpenRejectsBadArgs 的理由）。
func TestWalletListRejectsPositionalArgs(t *testing.T) {
	err := runWalletList([]string{"alice"})
	if err == nil {
		t.Fatal("list 收到位置参数时应当报错")
	}
	if !strings.Contains(err.Error(), "不接受位置参数") {
		t.Fatalf("错误信息 %q 里没有「不接受位置参数」", err.Error())
	}
	if code := exitCode(err); code != exitUsage {
		t.Errorf("退出码 = %d，想要 %d（参数错）", code, exitUsage)
	}
}

// walletRow 在清单里按用户 ID 找到那一行，返回它的各栏。
func walletListRow(t *testing.T, stdout, userID string) []string {
	t.Helper()
	for _, line := range strings.Split(stdout, "\n") {
		cells := strings.Fields(line)
		if len(cells) >= 5 && cells[0] == userID {
			return cells
		}
	}
	t.Fatalf("清单里没有用户 %s 那一行：\n%s", userID, stdout)
	return nil
}

// TestWalletListShowsBalanceAndFrozen 过真命令、连真库：入账之后那一行是
// 库里两个快照列的**原值**，用户名那一栏也填上了。
//
// 冻结额没有命令行写入路径（它由结算那一路写），所以直接用 SQL 造一个非零值：
// 不给它一个非零的输入，「冻结那一栏真的读了 frozen 列」这句话证明不了——
// 一个恒打 0 的实现照样通过。
func TestWalletListShowsBalanceAndFrozen(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	userID := createTestUser(t)
	ref := strconv.FormatInt(userID, 10)
	owner, err := auth.New(db).ResolveUser(t.Context(), ref)
	if err != nil {
		t.Fatalf("解析用户失败: %v", err)
	}

	if r := runOnce(t, runWallet, []string{"create", "--user", ref}); r.err != nil {
		t.Fatalf("开户失败: %v", r.err)
	}
	if r := runOnce(t, runWallet, []string{
		"credit", "--user", ref, "--amount", "12.5", "--ref", uniqueName("ref"),
	}); r.err != nil {
		t.Fatalf("入账失败: %v", r.err)
	}
	if _, err := db.ExecContext(t.Context(),
		`UPDATE user_wallets SET frozen = '3.25' WHERE user_id = $1 AND asset = 'CNY'`,
		userID); err != nil {
		t.Fatalf("造冻结额失败: %v", err)
	}

	r := runOnce(t, runWallet, []string{"list", "--user", ref})
	if r.err != nil {
		t.Fatalf("wallet list 失败: %v\nstderr=%s", r.err, r.stderr)
	}

	got := walletListRow(t, r.stdout, ref)
	want := []string{ref, owner.Username, "CNY", "12.50000000", "3.25000000"}
	if !slices.Equal(got, want) {
		t.Errorf("这一行是 %q，想要 %q", got, want)
	}
}

// TestWalletListWithoutFilterNamesEveryUser 钉住不筛时的那条路：用户名那一栏
// 由 users 取来，而不是空着。
//
// 它问的是「这一栏真的去查了名字吗」：一个只打 ID 的实现同样能让
// TestWalletListShowsBalanceAndFrozen 过（那一栏只是没断言到），而这一条会在
// 名字对不上时就红。
func TestWalletListWithoutFilterNamesEveryUser(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	userID := createTestUser(t)
	ref := strconv.FormatInt(userID, 10)
	owner, err := auth.New(db).ResolveUser(t.Context(), ref)
	if err != nil {
		t.Fatalf("解析用户失败: %v", err)
	}
	if r := runOnce(t, runWallet, []string{"create", "--user", ref}); r.err != nil {
		t.Fatalf("开户失败: %v", r.err)
	}

	r := runOnce(t, runWallet, []string{"list"})
	if r.err != nil {
		t.Fatalf("wallet list 失败: %v", r.err)
	}
	// 不带筛选时清单里不止这一行（别的用例也建过钱包），所以按行找而不是比全量。
	got := walletListRow(t, r.stdout, ref)
	if got[1] != owner.Username {
		t.Errorf("用户名那一栏是 %q，想要 %q：%q", got[1], owner.Username, got)
	}
}

// TestWalletListForUserWithoutWalletIsEmpty 钉住「用户存在但没有钱包行」这一档。
//
// 它与「用户不存在」是两件事（后者退出 4）：一条命令里走错一步的人看到的
// 应当是「这个用户还没开户」，而不是一个空表头或一句「找不到用户」。
func TestWalletListForUserWithoutWalletIsEmpty(t *testing.T) {
	requireDB(t)
	setupEnv(t)

	ref := strconv.FormatInt(createTestUser(t), 10)
	r := runOnce(t, runWallet, []string{"list", "--user", ref})
	if r.err != nil {
		t.Fatalf("没开户的用户上也应当成功（列出来是空的）: %v", r.err)
	}
	if want := emptyListNote("钱包", "wallet create"); !strings.Contains(r.stdout, want) {
		t.Errorf("空清单的输出里没有 %q：%q", want, r.stdout)
	}
}

// TestWalletListUnknownUserIsAConflict 钉住「要查的那一行不在」的退出码。
func TestWalletListUnknownUserIsAConflict(t *testing.T) {
	requireDB(t)
	setupEnv(t)

	r := runOnce(t, runWallet, []string{"list", "--user", uniqueName("nobody")})
	if r.err == nil {
		t.Fatal("筛一个不存在的用户应当报错")
	}
	if !errors.Is(r.err, auth.ErrUserNotFound) {
		t.Errorf("错误 %v 不是 ErrUserNotFound", r.err)
	}
	if code := exitCode(r.err); code != exitConflict {
		t.Errorf("退出码 = %d，想要 %d", code, exitConflict)
	}
}

// TestWalletListJSONShape 断言机器可读那一档：金额是**字符串**（过一遍 float64
// 会丢精度），用户 ID 是数字，没筛到东西时是 `[]` 而不是 null。
func TestWalletListJSONShape(t *testing.T) {
	requireDB(t)
	setupEnv(t)

	userID := createTestUser(t)
	ref := strconv.FormatInt(userID, 10)
	if r := runOnce(t, runWallet, []string{"create", "--user", ref}); r.err != nil {
		t.Fatalf("开户失败: %v", r.err)
	}
	// 另一个用户不建钱包：它是下面「空清单」那一档的输入。
	// 两个用户都在切到 json 之前建好——写类的命令会拒掉 --format json
	// （见 format.go 的 jsonUnsupported），在那一档里建不了。
	withoutWallet := strconv.FormatInt(createTestUser(t), 10)

	withFormat(t, formatJSON)
	r := runOnce(t, runWallet, []string{"list", "--user", ref})
	if r.err != nil {
		t.Fatalf("wallet list -o json 失败: %v", r.err)
	}
	assertOnlyJSON(t, r.stdout)

	var got struct {
		Status  string `json:"status"`
		Summary struct {
			Total int `json:"total"`
		} `json:"summary"`
		Wallets []struct {
			UserID   int64  `json:"user_id"`
			Username string `json:"username"`
			Asset    string `json:"asset"`
			Balance  string `json:"balance"`
			Frozen   string `json:"frozen"`
		} `json:"wallets"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &got); err != nil {
		t.Fatalf("解不回来: %v\n%s", err, r.stdout)
	}
	if got.Status != jsonOK || got.Summary.Total != 1 || len(got.Wallets) != 1 {
		t.Fatalf("形状不对：status=%q total=%d 行数=%d\n%s",
			got.Status, got.Summary.Total, len(got.Wallets), r.stdout)
	}

	w := got.Wallets[0]
	if w.UserID != userID || w.Asset != "CNY" {
		t.Errorf("这一行是 %+v，想要 user_id=%d asset=CNY", w, userID)
	}
	if w.Balance != "0.00000000" || w.Frozen != "0.00000000" {
		t.Errorf("金额是 %q / %q，想要两个 0.00000000（字符串，不是数字）",
			w.Balance, w.Frozen)
	}

	// 空清单：用户存在、钱包不存在的那一档。
	empty := runOnce(t, runWallet, []string{"list", "--user", withoutWallet})
	if empty.err != nil {
		t.Fatalf("空清单那次失败: %v", empty.err)
	}
	var emptyDoc struct {
		Wallets []map[string]any `json:"wallets"`
		Total   int              `json:"total"`
	}
	if err := json.Unmarshal([]byte(empty.stdout), &emptyDoc); err != nil {
		t.Fatalf("解不回来: %v\n%s", err, empty.stdout)
	}
	if emptyDoc.Wallets == nil {
		t.Errorf("空清单编成了 null，想要 []：%s", empty.stdout)
	}
	if emptyDoc.Total != 0 {
		t.Errorf("空清单的 total 是 %d，想要 0", emptyDoc.Total)
	}
}

// ── wallet list：某一格读不出来（#777）────────────────────────────────────

// overRangeWalletBalance 是构造「这一格读不出来」用的余额。
//
// 两个数各超一边：小数点前面 11 位已经到 int64 的上界（金额以 10^-8 为单位存，
// 上界是 92233720368.54775807），末两位 08 再往上顶一格。库里那一列是
// numeric(20, 8)，装得下它——于是「列里存着、命令读不出来」这个现场是真的
// （见 docs/cli-reference.md 的 wallet 一节与 #442）。
const overRangeWalletBalance = "92233720368.54775808"

// seedOverRangeWallet 把一个用户钱包的某一列改成超范围的数，并挂上清理。
//
// column 只接 user_wallets 上那两列（balance / frozen），且**各自写成一句 SQL**，
// 不拼串：用例写错列名时当场失败，而不是拼出一句跑不通的语句。
//
// # 为什么直接改快照，而不是照 ledger 那条用例写分录
//
// 写路径自己就挡住这种余额：applyToWallets 按**真实和**判（money.SumChecked），
// 超范围时整笔拒绝，所以从账本那条路造不出这个现场。而 wallet list 读的正是
// user_wallets 里的快照——库里真出现这个数，只可能来自更早的版本、迁移或对账
// 修正，改快照是对现场的忠实复现。
//
// # 必须自己清掉
//
// 本包的库是全包共用的。留着一行读不出来的余额，后面每一条不筛 --user 的清单
// 用例都会跟着退 1（TestWalletListWithoutFilterNamesEveryUser 就是一条），
// 而那种连带失效看起来像别处坏了。同一条教训见 ledger_test.go 里造溢出的那处。
func seedOverRangeWallet(t *testing.T, db *sql.DB, userID int64, column string) {
	t.Helper()

	var stmt string
	switch column {
	case "balance":
		stmt = `UPDATE user_wallets SET balance = $2::numeric WHERE user_id = $1`
	case "frozen":
		stmt = `UPDATE user_wallets SET frozen = $2::numeric WHERE user_id = $1`
	default:
		t.Fatalf("用例写错了列名 %q：只有 balance 与 frozen 两列可造", column)
	}
	if _, err := db.ExecContext(t.Context(), stmt, userID, overRangeWalletBalance); err != nil {
		t.Fatalf("造超范围的 %s 失败: %v", column, err)
	}
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.Background(),
			`DELETE FROM user_wallets WHERE user_id = $1`, userID); err != nil {
			t.Fatalf("清理用例的钱包失败: %v", err)
		}
	})
}

// 一格读不出来时：**其余行照打**，那一格有标记，命令退出 1，错误里点出是哪一行、
// 库里的原文是什么。
//
// 这是 #777 的现场：从前整条命令失败，别人的钱包一个也看不到——调用方看不出是
// 哪一行的问题，也绕不过去（只能一条一条 --user 试，而那一条恰好也失败）。
//
// 两行坏数据各占一列（一行坏余额、一行坏冻结额）：两格是**各判各的**，
// 只让余额坏的话，「冻结额那一格也照常打」这句话没有输入能证明——一个把两格
// 绑在一起判的实现照样绿。
func TestWalletListKeepsRowsWhenOneBalanceIsUnreadable(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	goodID := createTestUser(t)
	good := strconv.FormatInt(goodID, 10)
	if r := runOnce(t, runWallet, []string{"create", "--user", good}); r.err != nil {
		t.Fatalf("开户失败: %v", r.err)
	}
	badBalanceID := createTestUser(t)
	badBalance := strconv.FormatInt(badBalanceID, 10)
	if r := runOnce(t, runWallet, []string{"create", "--user", badBalance}); r.err != nil {
		t.Fatalf("开户失败: %v", r.err)
	}
	badFrozenID := createTestUser(t)
	badFrozen := strconv.FormatInt(badFrozenID, 10)
	if r := runOnce(t, runWallet, []string{"create", "--user", badFrozen}); r.err != nil {
		t.Fatalf("开户失败: %v", r.err)
	}
	seedOverRangeWallet(t, db, badBalanceID, "balance")
	seedOverRangeWallet(t, db, badFrozenID, "frozen")

	owner, err := auth.New(db).ResolveUser(t.Context(), badBalance)
	if err != nil {
		t.Fatalf("解析用户失败: %v", err)
	}

	r := runOnce(t, runWallet, []string{"list"})

	// 退出码非零：这一屏不是一次成功的调用，机器读的一方也要能据此告警。
	if r.err == nil {
		t.Fatalf("有一格读不出来时命令应当退出非零，stdout=\n%s", r.stdout)
	}
	if code := exitCode(r.err); code != exitFailure {
		t.Errorf("退出码 = %d，想要 %d", code, exitFailure)
	}

	// 好行照打：这正是 #777 要的那件事。用户名那一栏也照旧来自 users 表。
	goodOwner, err := auth.New(db).ResolveUser(t.Context(), good)
	if err != nil {
		t.Fatalf("解析用户失败: %v", err)
	}
	gotGood := walletListRow(t, r.stdout, good)
	if want := []string{good, goodOwner.Username, "CNY", "0.00000000", "0.00000000"}; !slices.Equal(gotGood, want) {
		t.Errorf("好行是 %q，想要 %q", gotGood, want)
	}

	// 坏行也在，不是被跳过：坏的是余额那一格，冻结额照常打。
	gotBad := walletListRow(t, r.stdout, badBalance)
	if len(gotBad) < 5 {
		t.Fatalf("坏行只有 %d 栏：%q", len(gotBad), gotBad)
	}
	if gotBad[3] != walletUnreadableCell {
		t.Errorf("余额那一格 = %q，想要 %q", gotBad[3], walletUnreadableCell)
	}
	if gotBad[4] != "0.00000000" {
		t.Errorf("同一行的冻结额 = %q，想要照常打 0.00000000（两格各判各的）", gotBad[4])
	}

	// 镜像：坏的是冻结额那一格，余额照常打。
	gotFrozen := walletListRow(t, r.stdout, badFrozen)
	if gotFrozen[3] != "0.00000000" {
		t.Errorf("同一行的余额 = %q，想要照常打 0.00000000（两格各判各的）", gotFrozen[3])
	}
	if gotFrozen[4] != walletUnreadableCell {
		t.Errorf("冻结额那一格 = %q，想要 %q", gotFrozen[4], walletUnreadableCell)
	}

	// 那一句话点出是哪一行、哪一格、原文是什么：表格里那一格放不下原文，
	// 所以这不是把表里的话说第二遍。两行都要点出来。
	msg := r.err.Error()
	for _, want := range []string{owner.Username, overRangeWalletBalance, "余额", "冻结额"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息 %q 里没有 %q", msg, want)
		}
	}

	// 带 --user 时口径不变：窄查询问的就是那一格的值，读不出来就是没答上来，
	// 一个字都不打（与这份清单的宽查询相反，见 runWalletList）。
	narrow := runOnce(t, runWallet, []string{"list", "--user", badBalance})
	if narrow.err == nil {
		t.Fatal("--user 指向那一行时应当失败")
	}
	if narrow.stdout != "" {
		t.Errorf("--user 失败时 stdout 应当是空的，实际 %q", narrow.stdout)
	}

	// 而筛一个读得出来的用户不受影响：别的行坏掉不牵连同一次调用，退出码仍是 0。
	other := runOnce(t, runWallet, []string{"list", "--user", good})
	if other.err != nil {
		t.Fatalf("筛一个读得出来的用户不该失败: %v", other.err)
	}
	if code := exitCode(other.err); code != exitOK {
		t.Errorf("没有坏行时退出码 = %d，想要 %d", code, exitOK)
	}
}

// 机器可读那一档里，读不出来的那一格是 `null`，那一行多一个 `unreadable` 说出是
// 哪一栏，状态是 failed、exit_code 与进程那个码是同一个数。
//
// 单看 null 分不出「这一栏没有值」与「值读不出来」，而钱包那两栏是非空列。
// 库里的原文**不在这一档里**：它在 cause 那句话上，两种格式下都经 main 打到
// stderr（见 walletListDoc）。
func TestWalletListJSONMarksUnreadableCells(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	goodID := createTestUser(t)
	good := strconv.FormatInt(goodID, 10)
	if r := runOnce(t, runWallet, []string{"create", "--user", good}); r.err != nil {
		t.Fatalf("开户失败: %v", r.err)
	}
	badBalanceID := createTestUser(t)
	badBalance := strconv.FormatInt(badBalanceID, 10)
	if r := runOnce(t, runWallet, []string{"create", "--user", badBalance}); r.err != nil {
		t.Fatalf("开户失败: %v", r.err)
	}
	badFrozenID := createTestUser(t)
	badFrozen := strconv.FormatInt(badFrozenID, 10)
	if r := runOnce(t, runWallet, []string{"create", "--user", badFrozen}); r.err != nil {
		t.Fatalf("开户失败: %v", r.err)
	}
	seedOverRangeWallet(t, db, badBalanceID, "balance")
	seedOverRangeWallet(t, db, badFrozenID, "frozen")

	withFormat(t, formatJSON)
	r := runOnce(t, runWallet, []string{"list"})
	if r.err == nil {
		t.Fatal("有读不出来的格子时应当退出非零")
	}
	if code := exitCode(r.err); code != exitFailure {
		t.Errorf("退出码 = %d，想要 %d", code, exitFailure)
	}
	// 清单照打：报告比退出码更先被看到，而「退 1」不该同时意味着「stdout 是空的」。
	assertOnlyJSON(t, r.stdout)

	var got struct {
		Status  string `json:"status"`
		Exit    int    `json:"exit_code"`
		Summary struct {
			Total          int `json:"total"`
			UnreadableRows int `json:"unreadable_rows"`
		} `json:"summary"`
		Wallets []struct {
			UserID     int64    `json:"user_id"`
			Balance    *string  `json:"balance"`
			Frozen     *string  `json:"frozen"`
			Unreadable []string `json:"unreadable"`
		} `json:"wallets"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &got); err != nil {
		t.Fatalf("解不回来: %v\n%s", err, r.stdout)
	}
	if got.Status != jsonFailed {
		t.Errorf("status = %q，想要 %q", got.Status, jsonFailed)
	}
	// exit_code 与进程真正退出的那个码必须是同一个数（见 docs/cli-output.md）。
	if got.Exit != exitFailure {
		t.Errorf("exit_code = %d，想要 %d", got.Exit, exitFailure)
	}
	if got.Summary.UnreadableRows != 2 {
		t.Errorf("unreadable_rows = %d，想要 2（一行坏余额、一行坏冻结额）", got.Summary.UnreadableRows)
	}

	seen := map[int64]bool{}
	for _, w := range got.Wallets {
		switch w.UserID {
		case badBalanceID:
			seen[badBalanceID] = true
			if w.Balance != nil {
				t.Errorf("读不出来的余额应当是 null，实际 %q", *w.Balance)
			}
			if !slices.Equal(w.Unreadable, []string{"balance"}) {
				t.Errorf("unreadable = %v，想要 [balance]", w.Unreadable)
			}
			if w.Frozen == nil || *w.Frozen != "0.00000000" {
				t.Errorf("同一行的冻结额 = %v，想要照常打 0.00000000", w.Frozen)
			}
		case badFrozenID:
			seen[badFrozenID] = true
			if w.Balance == nil || *w.Balance != "0.00000000" {
				t.Errorf("同一行的余额 = %v，想要照常打 0.00000000", w.Balance)
			}
			if w.Frozen != nil {
				t.Errorf("读不出来的冻结额应当是 null，实际 %q", *w.Frozen)
			}
			if !slices.Equal(w.Unreadable, []string{"frozen"}) {
				t.Errorf("unreadable = %v，想要 [frozen]", w.Unreadable)
			}
		case goodID:
			seen[goodID] = true
			if w.Balance == nil || *w.Balance != "0.00000000" {
				t.Errorf("好行的余额 = %v，想要 0.00000000", w.Balance)
			}
			// 好行的 unreadable 是空数组而不是 null 或省略：键在、值是空，
			// 「这一栏有，一个都没有」（见 format.go 的 jsonList）。
			if w.Unreadable == nil || len(w.Unreadable) != 0 {
				t.Errorf("好行的 unreadable = %v，想要空数组", w.Unreadable)
			}
		}
	}
	if !seen[badBalanceID] || !seen[badFrozenID] || !seen[goodID] {
		t.Fatalf("清单里少了行：%v\n%s", seen, r.stdout)
	}
}

// 与上一条互补的那个组合：机器可读那一档**再加上**窄查询，走的是 early return，
// 连 JSON 文档都不生成，所以这里要钉的不是「哪一栏是 null」而是「stdout 一字节
// 不打、退 1」。这条路径今天没有别处覆盖——窄查询那条用例是表格档的（见上面
// TestWalletListKeepsRowsWhenOneBalanceIsUnreadable 的末段）。
func TestWalletListJSONNarrowQueryOnUnreadableRow(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	badID := createTestUser(t)
	bad := strconv.FormatInt(badID, 10)
	if r := runOnce(t, runWallet, []string{"create", "--user", bad}); r.err != nil {
		t.Fatalf("开户失败: %v", r.err)
	}
	seedOverRangeWallet(t, db, badID, "balance")

	withFormat(t, formatJSON)
	r := runOnce(t, runWallet, []string{"list", "--user", bad})
	if r.err == nil {
		t.Fatal("窄查询撞上读不出来的那一格时应当失败")
	}
	if code := exitCode(r.err); code != exitFailure {
		t.Errorf("退出码 = %d，想要 %d", code, exitFailure)
	}
	// 半个 JSON 文档比没有更难用：解析器拿到的是截断的输入，而该说的那件事
	// （哪一行、哪一栏、原文是什么）本来也不在文档里。
	if r.stdout != "" {
		t.Errorf("窄查询失败时 stdout 应当是空的，实际 %q", r.stdout)
	}
	if !strings.Contains(r.err.Error(), overRangeWalletBalance) {
		t.Errorf("错误信息里要带上库里的原文：%v", r.err)
	}
}

// 那条错误把「是哪几行」写全，行数多时折起来并说出折了几行——不是静默地只报前几条。
//
// 纯函数，不连库：喂进去的行自己造。
func TestWalletUnreadableCauseCapsTheNamedRows(t *testing.T) {
	if cause := walletUnreadableCause(nil); cause != nil {
		t.Errorf("一行都没有时应当是 nil，得到 %v", cause)
	}
	if cause := walletUnreadableCause([]walletRow{{UserID: 1, Asset: ledger.AssetCNY}}); cause != nil {
		t.Errorf("都读得出来时应当是 nil，得到 %v", cause)
	}

	cellErr := errors.New(`解析余额 "1" 失败: 金额超出可表示范围`)
	rows := make([]walletRow, 0, maxUnreadableInReport+2)
	for i := 0; i < maxUnreadableInReport+2; i++ {
		rows = append(rows, walletRow{
			UserID: int64(i + 1), Username: "u" + strconv.Itoa(i+1),
			Asset: ledger.AssetCNY, BalanceErr: cellErr,
		})
	}

	cause := walletUnreadableCause(rows)
	if cause == nil {
		t.Fatal("有坏行时应当给出错误")
	}
	msg := cause.Error()
	if got := strings.Count(msg, "钱包："); got != maxUnreadableInReport {
		t.Errorf("点名的行数 = %d，想要 %d：%s", got, maxUnreadableInReport, msg)
	}
	// 折掉的行数要照写：少了它，读的人会以为坏的只有点到的这几行。
	if want := fmt.Sprintf("另有 %d 行", 2); !strings.Contains(msg, want) {
		t.Errorf("错误信息里没有 %q：%s", want, msg)
	}
}

// 那一格的标记只有一处来源：帮助文本里写的必须是同一个串。
//
// 短用法与长帮助都是手写的原始字符串，插不进常量，所以改标记时最容易漏掉它们，
// 而漏掉的表现是帮助文本安静地说着一个已经不成立的形状（与 snapshot 那个上限
// 是同一类用例）。
func TestWalletUsageStatesTheUnreadableCellRule(t *testing.T) {
	for _, tc := range []struct{ name, text string }{
		{"短用法", walletShortUsage},
		{"长帮助", walletUsage},
	} {
		if !strings.Contains(tc.text, walletUnreadableCell) {
			t.Errorf("%s 里没有 %q：改标记时帮助文本也要跟着改", tc.name, walletUnreadableCell)
		}
		if !strings.Contains(tc.text, "退出 1") {
			t.Errorf("%s 里没说清那一格读不出来时命令退出几", tc.name)
		}
	}
}

// walletCell 的三种输入各一种写法：读得出来是那个数；读出不来给标记；只有原因是
// 溢出时标记才带上原因。
//
// 第三种今天到不了（见 walletUnreadableCellGeneric 的注释），留着是因为标记说错
// 原因比说得笼统更坏——那会把读的人送去查一个不存在的边界。所以这里也钉住它。
func TestWalletCellLabelsUnreadableAmounts(t *testing.T) {
	if got := walletCell(money.MustParse("12.34"), nil); got != "12.34000000" {
		t.Errorf("读得出来的那一格 = %q，想要 12.34000000", got)
	}
	if got := walletCell(money.Zero, money.ErrOverflow); got != walletUnreadableCell {
		t.Errorf("溢出那一格 = %q，想要 %q", got, walletUnreadableCell)
	}
	if got := walletCell(money.Zero, errors.New("别的错")); got != walletUnreadableCellGeneric {
		t.Errorf("别的原因那一格 = %q，想要 %q", got, walletUnreadableCellGeneric)
	}
}
