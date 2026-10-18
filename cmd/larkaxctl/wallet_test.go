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
		fs := flag.NewFlagSet("wallet credit", flag.ContinueOnError)
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
