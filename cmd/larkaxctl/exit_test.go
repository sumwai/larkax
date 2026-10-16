package main

import (
	"testing"
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
