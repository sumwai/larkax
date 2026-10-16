package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/auth"
)

// TestUserCreateRejectsBadFlags 走的是**不碰数据库**的那几条路径。
//
// 与 apikey create 同一条约定：参数检查必须发生在连库之前。
// 一个写错的命令不该先连上生产库再报错，而「连上库之后才发现参数不对」
// 还有一个更坏的可能——它没报错。
//
// 「不碰库」也是这些用例能在没有 DSN 的机器上跑起来的原因：
// 它们一旦开始碰库，就会在本地通过、在 CI 上跳过，而「跳过」在输出里
// 和「通过」长得一样。
func TestUserCreateRejectsBadFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"没有 --name", []string{}, "缺少 --name"},
		{"--name 为空", []string{"--name", ""}, "缺少 --name"},
		{"--name 只有空白", []string{"--name", "   "}, "缺少 --name"},
		{"多出位置参数", []string{"--name", "alice", "extra"}, "不接受位置参数"},
		{"--role 不认识", []string{"--name", "alice", "--role", "root"}, "未知的角色"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runUserCreate(tc.args)
			if err == nil {
				t.Fatalf("期望报错，却通过了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息 %q 里没有 %q", err.Error(), tc.want)
			}
		})
	}
}

func TestUserUnknownSubcommand(t *testing.T) {
	err := runUser([]string{})
	if err == nil || !strings.Contains(err.Error(), "需要子命令") {
		t.Fatalf("无子命令时应报错，得到 %v", err)
	}

	err = runUser([]string{"bogus"})
	if err == nil || !strings.Contains(err.Error(), "未知的 user 子命令") {
		t.Fatalf("未知子命令时应报错，得到 %v", err)
	}
}

// TestPrintCreatedUserWritesOnlyIDToStdout 钉住输出分流这件事。
//
// `larkaxctl apikey create --user "$(larkaxctl user create --name alice)"`
// 是造完用户之后最自然的下一步。stdout 里一旦混进提示语，命令替换拿到的
// 就不是用户标识，而那条命令会报「找不到用户」——断点离真正的原因很远。
// 所以「stdout 只有 ID」是一条契约，不是排版偏好。
func TestPrintCreatedUserWritesOnlyIDToStdout(t *testing.T) {
	var stdout, stderr bytes.Buffer

	printCreatedUser(&stdout, &stderr, auth.User{ID: 7, Username: "user-placeholder"})

	if got, want := stdout.String(), "7\n"; got != want {
		t.Errorf("stdout = %q，期望只有 ID 加一个换行 %q", got, want)
	}
	if !strings.Contains(stderr.String(), "user-placeholder") {
		t.Errorf("stderr 里没有用户名，实际内容：\n%s", stderr.String())
	}
}
