package main

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/auth"
	"github.com/sumwai/larkax/internal/ledger"
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
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runWalletOpen(tc.args)
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
	for _, want := range []string{"已为", "user-placeholder", "id=7", "CNY", "余额 0"} {
		if !strings.Contains(created.String(), want) {
			t.Errorf("新建的输出里没有 %q：%q", want, created.String())
		}
	}

	var existing bytes.Buffer
	printOpenedWallet(&existing, owner, ledger.AssetCNY, false)
	for _, want := range []string{"已有", "user-placeholder", "id=7", "CNY", "未改动"} {
		if !strings.Contains(existing.String(), want) {
			t.Errorf("已存在的输出里没有 %q：%q", want, existing.String())
		}
	}
	if strings.Contains(existing.String(), "已为") {
		t.Errorf("已存在不该说成「已为…开通」：%q", existing.String())
	}
}

// ── 以下要连库 ──────────────────────────────────────────────────────────

// 开户命令走完整条链路：解析用户 → 开户 → 打印结果，且第二次是空操作。
//
// 它不手工插任何钱包行——这正是 #337 要求的那条路径：新用户只有这一条路
// 能拿到第一行 user_wallets。
func TestWalletOpenCommand(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	userID := createTestUser(t)
	ref := strconv.FormatInt(userID, 10)

	var firstErr error
	first, _ := captureStreams(t, func() { firstErr = runWalletOpen([]string{"--user", ref}) })
	if firstErr != nil {
		t.Fatalf("开户失败: %v", firstErr)
	}
	for _, want := range []string{"已为", "id=" + ref, "CNY", "余额 0"} {
		if !strings.Contains(first, want) {
			t.Errorf("第一次开户的输出里没有 %q：%q", want, first)
		}
	}

	var secondErr error
	second, _ := captureStreams(t, func() { secondErr = runWalletOpen([]string{"--user", ref}) })
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

// 用户不存在时退出码是「要查的那一行不存在」，不是笼统的失败。
//
// 这条区分有实际用处：脚本对 1 与 4 的处置不同——一个是「去看环境」，
// 另一个是「先确认这个用户是存在的」。
func TestWalletOpenUnknownUserExitCode(t *testing.T) {
	testDB(t)
	setupEnv(t)

	err := runCaptured(t, func() error {
		return runWalletOpen([]string{"--user", uniqueName("nobody")})
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
