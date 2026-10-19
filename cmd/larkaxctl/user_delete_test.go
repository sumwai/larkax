package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/auth"
)

// 销户命令那一族：参数门、退出码与回执。
//
// 状态机本身在 internal/auth 的集成用例里逐项证明；这里盯的是命令那一层——
// 哪些错在连库之前就报、哪一档给哪个退出码，以及回执上真的打出了可核对的值。

// TestUserDeleteRejectsBadFlags 走不碰库的那几条路径。
func TestUserDeleteRejectsBadFlags(t *testing.T) {
	cases := []struct {
		name string
		run  func([]string) error
		args []string
		want string
	}{
		{"delete 没有子命令", runUserDelete, nil, "delete 需要子命令"},
		{"delete 未知子命令", runUserDelete, []string{"erase"}, "未知的 user delete 子命令"},
		{"request 没有 --user", runUserDeleteRequest, nil, "缺少 --user"},
		{"request 多出位置参数", runUserDeleteRequest, []string{"--user", "1", "extra"},
			"不接受位置参数"},
		{"revert 没有 --user", runUserDeleteRevert, nil, "缺少 --user"},
		{"purge 没有 --user", runUserDeletePurge, nil, "缺少 --user"},
		{"purge 没有 --confirm", runUserDeletePurge, []string{"--user", "1"}, "缺少 --confirm"},
		{"purge 的 --confirm 只有空白", runUserDeletePurge,
			[]string{"--user", "1", "--confirm", "  "}, "缺少 --confirm"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run(tc.args)
			if err == nil {
				t.Fatal("期望报错，却通过了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息 %q 里没有 %q", err.Error(), tc.want)
			}
			if code := exitCode(err); code != exitUsage {
				t.Errorf("退出码 = %d，想要 %d（参数错）", code, exitUsage)
			}
		})
	}
}

// TestUserDeleteLifecycle 过真命令走完两步，并在每一步之后核对库里的值。
//
// 它同时验证几件事在**命令这一层**接通了：--user 按用户名与 ID 都认、回执上
// 打出来的是占位之后的值、user get 能看到终态、撤销之后原来的 key 还能用。
func TestUserDeleteLifecycle(t *testing.T) {
	setupEnv(t)
	db := testDB(t)
	ctx := t.Context()

	name := uniqueName("delete")
	email := name + "@example.com"
	created := runOnce(t, runUserCreate, []string{"--name", name, "--email", email})
	if created.err != nil {
		t.Fatalf("user create 失败: %v\nstderr=%s", created.err, created.stderr)
	}
	id, err := strconv.ParseInt(strings.TrimSpace(created.stdout), 10, 64)
	if err != nil {
		t.Fatalf("user create 的 stdout 不是 ID: %q", created.stdout)
	}
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.WithoutCancel(ctx),
			`DELETE FROM users WHERE id = $1`, id); err != nil {
			t.Logf("清理用户 %d 失败: %v", id, err)
		}
	})
	if r := runOnce(t, runUserUpdate, []string{
		"--user", strconv.FormatInt(id, 10), "--display-name", "要销户的人",
	}); r.err != nil {
		t.Fatalf("user update --display-name 失败: %v", r.err)
	}

	// 钱包、账本、插件 KV 与一把真 key：销户之后它们各自的处置都能核对。
	if _, err := db.ExecContext(ctx, `
		INSERT INTO user_wallets (user_id, asset, balance, frozen)
		VALUES ($1, 'CNY', 0, 0)`, id); err != nil {
		t.Fatalf("建钱包失败: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO ledger_entries (txn_id, account, amount, entry_type, memo)
		VALUES ('00000000-0000-8000-8000-000000000003', $1, 1, 'adjustment', '销户用例')`,
		"user:"+strconv.FormatInt(id, 10)); err != nil {
		t.Fatalf("写账本分录失败: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO plugin_kv (plugin_key, user_id, key, value) VALUES ('demo', $1, 'k', 'v')`,
		id); err != nil {
		t.Fatalf("写插件 KV 失败: %v", err)
	}
	key, err := auth.New(db).Create(ctx, auth.CreateParams{UserID: id})
	if err != nil {
		t.Fatalf("创建 key 失败: %v", err)
	}
	authOK := func() error {
		_, err := auth.New(db).Authenticate(ctx, requestWithKey(ctx, key.Plaintext))
		return err
	}

	// ── 第一步：打标（可撤） ───────────────────────────────────────
	r := runOnce(t, runUser, []string{"delete", "request", "--user", name})
	if r.err != nil {
		t.Fatalf("delete request 失败: %v\nstderr=%s", r.err, r.stderr)
	}
	for _, want := range []string{"已标记用户 " + name, "active → deletion_requested", "delete revert"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("打标回执里没有 %q:\n%s", want, r.stdout)
		}
	}
	if err := authOK(); err == nil {
		t.Fatal("打标之后 key 还能用")
	}

	// ── 撤销：原来的 key 还能用 ───────────────────────────────────
	r = runOnce(t, runUser, []string{"delete", "revert", "-u", name})
	if r.err != nil {
		t.Fatalf("delete revert 失败: %v\nstderr=%s", r.err, r.stderr)
	}
	if !strings.Contains(r.stdout, "deletion_requested → active") {
		t.Errorf("撤销回执里没有状态变化:\n%s", r.stdout)
	}
	if err := authOK(); err != nil {
		t.Fatalf("撤销之后原来的 key 不能用: %v", err)
	}

	// ── 再打标；--confirm 写错时什么都不写 ─────────────────────────
	if r := runOnce(t, runUser, []string{"delete", "request", "--user", name}); r.err != nil {
		t.Fatalf("第二次打标失败: %v", r.err)
	}
	r = runOnce(t, runUser, []string{
		"delete", "purge", "--user", strconv.FormatInt(id, 10), "--confirm", "另一个名字",
	})
	if r.err == nil {
		t.Fatal("--confirm 写错却通过了")
	}
	if !errors.Is(r.err, auth.ErrDeletionConfirmMismatch) {
		t.Errorf("错误 %v 不是 ErrDeletionConfirmMismatch", r.err)
	}
	if code := exitCode(r.err); code != exitUsage {
		t.Errorf("退出码 = %d，想要 %d", code, exitUsage)
	}
	var status string
	if err := db.QueryRowContext(ctx,
		`SELECT status FROM users WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatalf("读状态失败: %v", err)
	}
	if status != auth.UserStatusDeletionRequested {
		t.Errorf("确认失败之后状态变成了 %q", status)
	}

	// ── 第二步：匿名化 ─────────────────────────────────────────────
	r = runOnce(t, runUser, []string{
		"delete", "purge", "--user", strconv.FormatInt(id, 10), "--confirm", name,
	})
	if r.err != nil {
		t.Fatalf("delete purge 失败: %v\nstderr=%s", r.err, r.stderr)
	}
	newName := "deleted-user-" + strconv.FormatInt(id, 10)
	for _, want := range []string{
		"已匿名化用户 " + name,
		name + " → " + newName,
		email + " → " + newName + "@example.com",
		"要销户的人 → 已注销用户 #" + strconv.FormatInt(id, 10),
		"deletion_requested → deleted",
		"1 把置为 revoked（共 1 把，行都保留）",
		"1 个键已删除",
		"1 条分录保留",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("销户回执里没有 %q:\n%s", want, r.stdout)
		}
	}

	// user get 看得到终态与删除时刻，而且按新用户名也读得到这一行。
	r = runOnce(t, runUser, []string{"get", "--user", strconv.FormatInt(id, 10)})
	if r.err != nil {
		t.Fatalf("销户之后 user get 失败: %v", r.err)
	}
	for _, want := range []string{auth.UserStatusDeleted, "删除时间"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("user get 里没有 %q:\n%s", want, r.stdout)
		}
	}
	if r := runOnce(t, runUser, []string{"get", "--user", newName}); r.err != nil {
		t.Fatalf("按新用户名读不到: %v", r.err)
	}

	// 终态不可逆：没有第二条命令能把身份写回去。这里试的是 update——它是唯一
	// 一组能改 users 身份列的入口，不挡住的话占位值就只是「暂时」的。
	r = runOnce(t, runUser, []string{
		"update", "--user", strconv.FormatInt(id, 10), "--email", email,
	})
	if r.err == nil {
		t.Fatal("终态账号却可以改邮箱")
	}
	if !strings.Contains(r.err.Error(), "已经匿名化") {
		t.Errorf("报错里没有「已经匿名化」: %v", r.err)
	}
	if code := exitCode(r.err); code != exitConflict {
		t.Errorf("退出码 = %d，想要 %d（终态冲突）", code, exitConflict)
	}
	r = runOnce(t, runUser, []string{"get", "--user", strconv.FormatInt(id, 10)})
	if r.err != nil {
		t.Fatalf("拒绝之后 user get 失败: %v", r.err)
	}
	if strings.Contains(r.stdout, email) {
		t.Errorf("被拒的 update 却把邮箱写了回去:\n%s", r.stdout)
	}
	if !strings.Contains(r.stdout, newName+"@example.com") {
		t.Errorf("占位邮箱不见了:\n%s", r.stdout)
	}
}

// TestUserDeleteRefusesNonZeroBalance 过真命令验证前置条件的退出码与文案。
func TestUserDeleteRefusesNonZeroBalance(t *testing.T) {
	setupEnv(t)
	db := testDB(t)
	ctx := t.Context()
	name := uniqueName("balance")
	id := createUserWithRole(t, name, "", string(auth.RoleUser))
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.WithoutCancel(ctx),
			`DELETE FROM users WHERE id = $1`, id); err != nil {
			t.Logf("清理用户 %d 失败: %v", id, err)
		}
	})
	if _, err := db.ExecContext(ctx, `
		INSERT INTO user_wallets (user_id, asset, balance, frozen)
		VALUES ($1, 'CNY', 1.23, 0)`, id); err != nil {
		t.Fatalf("建钱包失败: %v", err)
	}

	r := runOnce(t, runUser, []string{"delete", "request", "--user", name})
	if r.err == nil {
		t.Fatal("余额不是 0 却打标成功")
	}
	if !strings.Contains(r.err.Error(), "可用余额必须为 0") {
		t.Errorf("报错里没有那句判据: %v", r.err)
	}
	if !strings.Contains(r.err.Error(), "1.23") {
		t.Errorf("报错里没有当前余额: %v", r.err)
	}
	if code := exitCode(r.err); code != exitConflict {
		t.Errorf("退出码 = %d，想要 %d（约束冲突）", code, exitConflict)
	}
}

// requestWithKey 造一个带 x-api-key 的最小请求。
//
// cmd 包自己的用例要么走命令、要么走 auth 的公共入口；这里要回答的是「撤销之后
// 这一把 key 还能不能鉴权」，所以从公共入口走一遍，而不是直接读状态列。
func requestWithKey(ctx context.Context, plaintext string) *http.Request {
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("x-api-key", plaintext)
	return r
}
