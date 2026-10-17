package main

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/sumwai/larkax/internal/auth"
	"github.com/sumwai/larkax/internal/dberr"
	"github.com/sumwai/larkax/internal/ledger/store"
)

// 退出码是给脚本用的对外契约，所以这里逐个钉住**具体的数**，而不是拿常量
// 去比常量——帮助文本里写的也是这几个数，改一个就会让那些脚本改变行为。
//
//	2  参数错
//	3  连不上数据库
//	4  约束冲突
//
// 三类必须互不相同：脚本对它们的处置不同（改命令 / 看环境 / 看数据），
// 混成一个码就只能去匹配错误文本，而错误文本是给人看的。

// TestExitCodeUsageIsTwo 取一次参数错。
func TestExitCodeUsageIsTwo(t *testing.T) {
	cases := map[string]func() error{
		"缺字段":   func() error { return runChannelCreate(nil) },
		"未知子命令": func() error { return runChannel([]string{"bogus"}) },
		"未知标志":  func() error { return runChannelList([]string{"--nope"}) },
	}

	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			err := runCaptured(t, run)
			if err == nil {
				t.Fatal("期望报错，却通过了")
			}
			if got := exitCode(err); got != 2 {
				t.Errorf("参数错的退出码应当是 2，得到 %d（%v）", got, err)
			}
		})
	}
}

// TestExitCodeDBUnreachableIsThree 取一次连不上库。
//
// 指向一个没有监听者的端口：这一条不需要真库，正是它要测的那种失败。
// 它与参数错必须分得开——「命令写错了」与「库连不上」的下一步动作完全不同。
func TestExitCodeDBUnreachableIsThree(t *testing.T) {
	setupEnv(t)
	t.Setenv("LARKAX_DB_DSN", "postgres://larkax:placeholder@127.0.0.1:1/larkax?sslmode=disable")

	err := runCaptured(t, func() error { return runChannelList(nil) })
	if err == nil {
		t.Fatal("连不上库时应当报错")
	}
	if got := exitCode(err); got != 3 {
		t.Errorf("连不上库的退出码应当是 3，得到 %d（%v）", got, err)
	}
}

// TestExitCodeConflictIsFour 取一次约束冲突。
//
// 要改的那一行不在库里属于这一类：它的下一步动作与撞唯一键一样，都是
// 「先去看数据里现在是什么」。
func TestExitCodeConflictIsFour(t *testing.T) {
	testDB(t)
	setupEnv(t)

	err := runCaptured(t, func() error {
		return runChannel([]string{"enable", "--id", "999999999"})
	})
	if err == nil {
		t.Fatal("启用一个不存在的渠道应当报错")
	}
	if got := exitCode(err); got != 4 {
		t.Errorf("约束冲突的退出码应当是 4，得到 %d（%v）", got, err)
	}
}

// TestExitCodeConflictForUnknownUser 取一次「要查的那一行不存在」。
//
// 它与上面那条是**两个不同的来源**：上面那条是写入路径撞上约束，这条是解析
// 路径认不出一个标识（auth.ErrUserNotFound 进了 conflictErrors）。两者共用
// 退出码 4，而这个哨兵是全局的：`apikey` / `settings` / `snapshot` / `owner` /
// `wallet` 几条命令的解析路径都靠它。改它的判据（把它从 conflictErrors 里拿
// 掉、或让某个包不再返回它）会让这些命令的退出码一起从 4 退回 1，而没有任何
// 一条路径会喊——这条用例就是那个会喊的东西。
//
// 挑 apikey create 钉住，是因为它是最先被外部脚本用起来的一条（拿 key 打请求
// 的第一步），退出码静默变化最容易咬到它。
func TestExitCodeConflictForUnknownUser(t *testing.T) {
	testDB(t)
	setupEnv(t)

	err := runCaptured(t, func() error {
		return runAPIKeyCreate([]string{"--user", uniqueName("nobody")})
	})
	if err == nil {
		t.Fatal("用户不存在时应当报错")
	}
	if !errors.Is(err, auth.ErrUserNotFound) {
		t.Errorf("错误链里没有 auth.ErrUserNotFound：%v", err)
	}
	if got := exitCode(err); got != 4 {
		t.Errorf("要查的那一行不存在时退出码应当是 4，得到 %d（%v）", got, err)
	}
}

// TestExitCodeConflictForGenericConstraint 钉住兜底哨兵也落 4。
//
// 这个哨兵覆盖的是**没被逐个认出来**的唯一键与外键。它不经过哪条具体命令
// 就能构造出来，所以这里直接喂一个带哨兵的错误：要钉的是「它被登记进
// conflictErrors」这件事——漏登记的表现与「这条路径还没人走过」一模一样，
// 没有任何一条真实路径会喊。
func TestExitCodeConflictForGenericConstraint(t *testing.T) {
	err := fmt.Errorf("%w: 写预留记录失败: %w",
		store.ErrConstraintViolation, errors.New("driver error"))
	if got := exitCode(err); got != 4 {
		t.Errorf("通用约束冲突的退出码应当是 4，得到 %d（%v）", got, err)
	}
}

// TestExitCodeConflictForUnnamedForeignKey 从**包的写路径**造一个真库上的
// 外键冲突，断言它落 4 而不是 1。
//
// 造法就是 issue 里举的那个场景：`apikey create` 先把标识解析成用户，插入
// 之前那个用户被删了。命令这一层没法确定性地把删除塞进解析与插入之间，
// 所以直接调 `auth.Store.Create` 并给一个 users 里没有的 id——撞上的约束
// 是同一条（api_keys_user_id_fkey），判据与造法无关。
//
// 改之前这条错误从 auth 原样上抛，链里没有任何哨兵，exitCode 落到 1；
// 改之后它带着 dberr.ErrConstraintViolation。这条用例钉的正是那个数。
func TestExitCodeConflictForUnnamedForeignKey(t *testing.T) {
	db := testDB(t)

	_, err := auth.New(db).Create(context.Background(), auth.CreateParams{
		UserID: 900000000000000,
	})
	if err == nil {
		t.Fatal("用户不存在时建 key 应当报错")
	}
	if !errors.Is(err, dberr.ErrConstraintViolation) {
		t.Fatalf("错误链里没有 dberr.ErrConstraintViolation：%v", err)
	}
	if got := exitCode(err); got != 4 {
		t.Errorf("没被具名认出来的外键冲突，退出码应当是 4，得到 %d（%v）", got, err)
	}
}

// TestHelpIsNotAFailure 钉住 -h 不是失败。
//
// 子命令这一层自己认 -h 并直接打印用法（那时它不经过 flag 包）；叶子命令
// 由 flag 包返回 ErrHelp。两条路都要是「成功」，否则
// `larkaxctl channel list -h` 在脚本里看起来像一次错误，还会多打一行
// 「flag: help requested」。
func TestHelpIsNotAFailure(t *testing.T) {
	if err := runCaptured(t, func() error { return runChannel([]string{"-h"}) }); err != nil {
		t.Errorf("子命令那一层的 -h 应当直接成功，得到 %v", err)
	}

	err := runCaptured(t, func() error { return runChannelList([]string{"-h"}) })
	if err == nil {
		t.Fatal("叶子命令的 -h 应当由 flag 包返回 ErrHelp")
	}
	if !helpRequested(err) {
		t.Errorf("-h 应当被认成「要帮助」，得到 %v", err)
	}
}

// TestExitCodeOfNilIsZero 钉住成功。
func TestExitCodeOfNilIsZero(t *testing.T) {
	if got := exitCode(nil); got != 0 {
		t.Errorf("没有错误时退出码应当是 0，得到 %d", got)
	}
}
