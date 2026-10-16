package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/auth"
)

// TestAPIKeyCreateRejectsBadFlags 走的是**不碰数据库**的那几条路径。
//
// 参数检查必须发生在连库之前：一个写错的命令不该先连上生产库再报错，
// 而「连上库之后才发现参数不对」还有一个更坏的可能——它没报错。
func TestAPIKeyCreateRejectsBadFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"没有 --user", []string{}, "缺少 --user"},
		{"--user 为空", []string{"--user", ""}, "缺少 --user"},
		{"多出位置参数", []string{"--user", "1", "extra"}, "不接受位置参数"},
		{"--expires-at 不是时间", []string{"--user", "1", "--expires-at", "明天"}, "RFC3339"},
		{"--expires-at 在过去", []string{"--user", "1", "--expires-at", "2020-01-01T00:00:00Z"}, "已经过去"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runAPIKeyCreate(tc.args)
			if err == nil {
				t.Fatalf("期望报错，却通过了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息 %q 里没有 %q", err.Error(), tc.want)
			}
		})
	}
}

func TestAPIKeyUnknownSubcommand(t *testing.T) {
	err := runAPIKey([]string{})
	if err == nil || !strings.Contains(err.Error(), "需要子命令") {
		t.Fatalf("无子命令时应报错，得到 %v", err)
	}

	err = runAPIKey([]string{"bogus"})
	if err == nil || !strings.Contains(err.Error(), "未知的 apikey 子命令") {
		t.Fatalf("未知子命令时应报错，得到 %v", err)
	}
}

// TestPrintCreatedKeySplitsStreams 钉住输出分流这件事。
//
// `larkaxctl apikey create ... > key.txt` 是保存一把 key 最自然的动作。
// 元信息一旦混进 stdout，那个文件就没法被程序直接读走——粘贴到客户端配置里
// 多一行就少一个能用的 key。所以「stdout 只有明文」是一条契约，不是排版偏好。
func TestPrintCreatedKeySplitsStreams(t *testing.T) {
	var stdout, stderr bytes.Buffer
	expires := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)

	printCreatedKey(&stdout, &stderr,
		auth.User{ID: 7, Username: "user-placeholder"},
		auth.Created{ID: 3, Prefix: "larkax_abcd", Plaintext: "larkax_abcd_plaintext"},
		&expires,
	)

	if got, want := stdout.String(), "larkax_abcd_plaintext\n"; got != want {
		t.Errorf("stdout = %q，期望只有明文加一个换行 %q", got, want)
	}

	// 提示语必须在：明文只返回一次，不说明的话它就是一次静默的数据丢失。
	for _, want := range []string{"明文只打印这一次", "larkax_abcd", "2030-01-02T03:04:05Z", "user-placeholder"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr 里没有 %q，实际内容：\n%s", want, stderr.String())
		}
	}
}
