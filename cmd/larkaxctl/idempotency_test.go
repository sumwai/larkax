package main

import (
	"context"
	"database/sql"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/sumwai/larkax/cmd/larkaxctl/internal/cli"
)

// 缺子命令与未知子命令都是参数错（退出码 2），而不是「其他失败」（1）：
// 脚本按退出码分派处置，而这两种的下一步动作都是「改命令重发」。
func TestRunIdempotencyUnknownSubcommand(t *testing.T) {
	err := runCaptured(t, func() error { return runIdempotency(nil) })
	if err == nil || !strings.Contains(err.Error(), "需要子命令") {
		t.Fatalf("无子命令时应报错，得到 %v", err)
	}
	if got := cli.ExitCode(err); got != cli.ExitUsage {
		t.Errorf("缺子命令的退出码应当是 %d，得到 %d", cli.ExitUsage, got)
	}

	err = runCaptured(t, func() error { return runIdempotency([]string{"清理"}) })
	if err == nil || !strings.Contains(err.Error(), "未知的 idempotency 子命令") {
		t.Fatalf("未知子命令时应报错，得到 %v", err)
	}
	if got := cli.ExitCode(err); got != cli.ExitUsage {
		t.Errorf("未知子命令的退出码应当是 %d，得到 %d", cli.ExitUsage, got)
	}
}

func TestRunIdempotencyHelp(t *testing.T) {
	for _, arg := range []string{"help", "-h", "--help"} {
		if err := runCaptured(t, func() error { return runIdempotency([]string{arg}) }); err != nil {
			t.Errorf("idempotency %s 应当成功返回，实际为 %v", arg, err)
		}
	}
}

// cleanup --help 不是失败。flag 在返回 ErrHelp 之前已经把用法打印过了，
// 而 main 靠 helpRequested 认出它并以退出码 0 收尾（见 main.go）。
// 这条用例钉住的是「它仍然是那个能被认出来的 ErrHelp」——包了一层退出码
// 之后还要能透过 Unwrap 认出来。
func TestRunIdempotencyCleanupHelp(t *testing.T) {
	err := runCaptured(t, func() error { return runIdempotencyCleanup([]string{"--help"}) })
	if !cli.HelpRequested(err) {
		t.Errorf("--help 应当被认成「要帮助」（helpRequested），实际为 %v", err)
	}
}

// 不认识的标志是参数错，必须在连库之前报出来。
func TestRunIdempotencyCleanupRejectsUnknownFlag(t *testing.T) {
	err := runCaptured(t, func() error { return runIdempotencyCleanup([]string{"--bogus"}) })
	if err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("未知标志应当报错，得到 %v", err)
	}
	if got := cli.ExitCode(err); got != cli.ExitUsage {
		t.Errorf("参数错的退出码应当是 %d，得到 %d", cli.ExitUsage, got)
	}
}

// 位置参数是参数错，必须在连库之前拦下。
//
// 要挡的形状是「少写一个横线的 --dry-run」：flag 把它留在 NArg 里，`*dryRun`
// 仍是 false，命令会照着真删的路径往下走。所以这里同时钉三件事——退出码是 2、
// 提示点出 `--dry-run`、失败发生在 config.Load 之前（DSN 清空后报的仍是参数错
// 而不是配置错）。
func TestRunIdempotencyCleanupRejectsPositionalArgs(t *testing.T) {
	t.Setenv("LARKAX_DB_DSN", "")

	err := runCaptured(t, func() error { return runIdempotencyCleanup([]string{"dry-run"}) })
	if err == nil || !strings.Contains(err.Error(), "不接受位置参数") {
		t.Fatalf("位置参数应当报错，得到 %v", err)
	}
	if !strings.Contains(err.Error(), "--dry-run") {
		t.Errorf("提示里要点出 --dry-run，实际为 %v", err)
	}
	if got := cli.ExitCode(err); got != cli.ExitUsage {
		t.Errorf("位置参数的退出码应当是 %d，得到 %d", cli.ExitUsage, got)
	}
}

// --dry-run 与不带 --dry-run 都要能过 flag 解析。
//
// 判据是「失败发生在解析之后」：把 DSN 清空，config.Load 必然失败并点出
// LARKAX_DB_DSN。参数错会被标成退出码 2，而这里不是 2——于是这条用例既
// 不需要库，也不会在设了 DSN 的环境里真的去删数据（那样它会动真格）。
func TestRunIdempotencyCleanupParsesFlagsBeforeConnecting(t *testing.T) {
	t.Setenv("LARKAX_DB_DSN", "")

	for _, args := range [][]string{nil, {"--dry-run"}} {
		err := runCaptured(t, func() error { return runIdempotencyCleanup(args) })
		if err == nil {
			t.Fatalf("%v：没有 DSN 时应当失败", args)
		}
		if !strings.Contains(err.Error(), "LARKAX_DB_DSN") {
			t.Errorf("%v：失败应当发生在 config.Load（点出 LARKAX_DB_DSN），实际为 %v", args, err)
		}
		if got := cli.ExitCode(err); got == cli.ExitUsage {
			t.Errorf("%v：不该被当成参数错（退出码 %d），实际为 %v", args, cli.ExitUsage, err)
		}
	}
}

// 一条都没删时也要有明确输出。
//
// 「没有到期记录」与「命令没跑成」在安静的终端里长得一样（LESSONS.md 第 16 条），
// 所以这里断言的是**有一行说明是 0 条**，而不只是「没报错」。
func TestPrintCleanupEmptyIsExplicit(t *testing.T) {
	out := captureStdout(t, func() { printCleanup("已删除", 0) })
	if !strings.Contains(out, "0 条") {
		t.Errorf("空结果要说明是 0 条，实际为 %q", out)
	}
	if !strings.Contains(out, "没有到期记录") {
		t.Errorf("空结果要说明原因，实际为 %q", out)
	}
}

func TestPrintCleanupReportsCount(t *testing.T) {
	out := captureStdout(t, func() { printCleanup("已删除", 7) })
	if !strings.Contains(out, "7") {
		t.Errorf("输出里应当给出条数，实际为 %q", out)
	}
}

// TestRunIdempotencyCleanupDeletesExpired 钉住 dry-run 与真删数的是同一批行。
//
// 这是本命令最容易出错的地方：dry-run 与真删是两条 SQL，判据一旦漂移
// （把 `<` 写成 `<=`、把 interval 的单位从 second 换成 hour），干跑报出的
// 条数就与真删的条数对不上，而使用者正是拿前者决定要不要跑后者。
// 所以先 dry-run、再真删，断言两个数相等，最后落到行上：到期的那条没了，
// 没到期的那条还在。
func TestRunIdempotencyCleanupDeletesExpired(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	apiKeyID := seedAPIKey(t, db)
	expiredKey := uniqueName("expired")
	freshKey := uniqueName("fresh")
	// created_at 显式拨回去：「到期」是这条用例的被测对象本身，不能靠等。
	insertIdempotencyKey(t, db, apiKeyID, expiredKey, "48 hours")
	insertIdempotencyKey(t, db, apiKeyID, freshKey, "1 hour")

	dryOut, _ := captureStreams(t, func() {
		if err := runIdempotencyCleanup([]string{"--dry-run"}); err != nil {
			t.Fatalf("dry-run 失败: %v", err)
		}
	})
	if !strings.Contains(dryOut, "将删除") {
		t.Errorf("dry-run 的输出应当说明是干跑，实际为 %q", dryOut)
	}
	if !keyExists(t, db, apiKeyID, expiredKey) {
		t.Error("dry-run 不该真的删除")
	}
	dryCount := firstNumber(t, dryOut)

	out, _ := captureStreams(t, func() {
		if err := runIdempotencyCleanup(nil); err != nil {
			t.Fatalf("cleanup 失败: %v", err)
		}
	})
	if !strings.Contains(out, "已删除") {
		t.Errorf("cleanup 的输出应当说明删了多少，实际为 %q", out)
	}
	realCount := firstNumber(t, out)

	if dryCount != realCount {
		t.Errorf("dry-run 报 %d 条、真删 %d 条：两条 SQL 的判据不一致", dryCount, realCount)
	}
	if keyExists(t, db, apiKeyID, expiredKey) {
		t.Error("到期的那条应当被删掉")
	}
	if !keyExists(t, db, apiKeyID, freshKey) {
		t.Error("没到期的那条不该被删掉")
	}
}

// TestRunIdempotencyCleanupPositionalArgDeletesNothing 钉住的是后果，不只是退出码。
//
// 打错成 `dry-run` 时最坏的结果不是「命令报了个错」，而是它没报错却真的删了。
// 所以先造一条到期记录，再跑一次带位置参数的调用，最后落到行上：那条必须还在。
func TestRunIdempotencyCleanupPositionalArgDeletesNothing(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	apiKeyID := seedAPIKey(t, db)
	expiredKey := uniqueName("expired")
	insertIdempotencyKey(t, db, apiKeyID, expiredKey, "48 hours")

	var err error
	captureStreams(t, func() { err = runIdempotencyCleanup([]string{"dry-run"}) })
	if got := cli.ExitCode(err); got != cli.ExitUsage {
		t.Errorf("`dry-run` 的退出码应当是 %d，得到 %d（err=%v）", cli.ExitUsage, got, err)
	}
	if !keyExists(t, db, apiKeyID, expiredKey) {
		t.Error("位置参数写错时一行都不该删")
	}
}

// seedAPIKey 造一条 API Key 并返回它的 id。
//
// idempotency_keys.api_key_id 有外键指向 api_keys，所以不能只写一张表。
// 用裸 SQL 而不是走 runUserCreate / runAPIKeyCreate：这条用例要的是「有
// 一个合法的 api_key_id」，不是「那两条命令能不能跑」（它们各自有自己的用例）。
func seedAPIKey(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	ctx := context.Background()

	var userID int64
	if err := db.QueryRowContext(ctx,
		`INSERT INTO users (username) VALUES ($1) RETURNING id`,
		uniqueName("user")).Scan(&userID); err != nil {
		t.Fatalf("造用户失败: %v", err)
	}

	var keyID int64
	if err := db.QueryRowContext(ctx,
		`INSERT INTO api_keys (user_id, key_hash, key_prefix) VALUES ($1, $2, $3) RETURNING id`,
		userID, uniqueName("hash"), "larkax_test").Scan(&keyID); err != nil {
		t.Fatalf("造 API Key 失败: %v", err)
	}
	return keyID
}

// insertIdempotencyKey 插一条幂等记录，created_at 按 age 拨到过去。
func insertIdempotencyKey(t *testing.T, db *sql.DB, apiKeyID int64, key, age string) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO idempotency_keys (api_key_id, key, request_hash, state, created_at)
		VALUES ($1, $2, $3, 'completed', now() - $4::interval)`,
		apiKeyID, key, []byte("01234567890123456789012345678901"), age); err != nil {
		t.Fatalf("造幂等记录失败: %v", err)
	}
}

func keyExists(t *testing.T, db *sql.DB, apiKeyID int64, key string) bool {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM idempotency_keys WHERE api_key_id = $1 AND key = $2`,
		apiKeyID, key).Scan(&n); err != nil {
		t.Fatalf("查幂等记录失败: %v", err)
	}
	return n > 0
}

// firstNumber 从命令输出里取第一个整数。
//
// 输出有「已删除 3 条」与「已删除：0 条（没有到期记录）」两种形状，
// 按字段切分会因为全角冒号而错位，取第一个数则两种都对得上。
func firstNumber(t *testing.T, s string) int64 {
	t.Helper()
	m := regexp.MustCompile(`\d+`).FindString(s)
	if m == "" {
		t.Fatalf("输出里没有数字：%q", s)
	}
	n, err := strconv.ParseInt(m, 10, 64)
	if err != nil {
		t.Fatalf("解析数字 %q 失败: %v", m, err)
	}
	return n
}
