package main

import (
	"regexp"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/auth"
)

// 本文件钉住 `user password` 的三段纯逻辑：参数怎么解析、生成的口令长什么
// 样、标准输入怎么读。它们都不连库，所以不需要 LARKAX_DB_DSN。
//
// 连库那一半（找用户、写哈希、吊销会话）在 user_password.go 的
// runUserPassword 里，只有真库能测；这里不拿替身假装它。

func TestParseUserPassword(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		user     string
		generate bool
		stdin    bool
		wantErr  bool
	}{
		{
			name: "只给 --user 与 --stdin",
			args: []string{"--user", "alice", "--stdin"},
			user: "alice", stdin: true,
		},
		{
			name: "短选项 -u 与 --generate",
			args: []string{"-u", "alice", "--generate"},
			user: "alice", generate: true,
		},
		{
			name: "帮助页的顺序：用户在前、flag 在后",
			args: []string{"42", "--generate"},
			user: "42", generate: true,
		},
		{
			name: "帮助页的顺序：用户在前、--stdin 在后",
			args: []string{"42", "--stdin"},
			user: "42", stdin: true,
		},
		{
			name: "短选项在前、位置参数在中间、flag 在最后",
			args: []string{"-u", "42", "--generate"},
			user: "42", generate: true,
		},
		{
			name: "flag 在前、位置参数在后",
			args: []string{"--generate", "42"},
			user: "42", generate: true,
		},
		{
			name: "位置参数紧跟在 flag 之后",
			args: []string{"--stdin", "alice"},
			user: "alice", stdin: true,
		},
		{
			name:    "用户在前、--user 在后",
			args:    []string{"42", "--user", "7", "--generate"},
			wantErr: true,
		},
		{
			name:    "-- 之后全部是位置参数",
			args:    []string{"--", "42", "--generate"},
			wantErr: true,
		},
		{
			name:    "位置参数与 --user 同时给",
			args:    []string{"--user", "alice", "bob", "--generate"},
			wantErr: true,
		},
		{
			name:    "两个位置参数",
			args:    []string{"--generate", "a", "b"},
			wantErr: true,
		},
		{
			name:    "缺用户",
			args:    []string{"--generate"},
			wantErr: true,
		},
		{
			name:    "只有用户、两种来源都没给",
			args:    []string{"alice"},
			wantErr: true,
		},
		{
			name:    "--generate 与 --stdin 同时给",
			args:    []string{"--generate", "--stdin", "alice"},
			wantErr: true,
		},
		{
			name:    "只有 --user、没有来源",
			args:    []string{"--user", "alice"},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			user, generate, stdin, err := parseUserPassword(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望报错，得到 user=%q generate=%v stdin=%v",
						user, generate, stdin)
				}
				return
			}
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if user != tc.user || generate != tc.generate || stdin != tc.stdin {
				t.Errorf("解析结果 = %q / %v / %v，期望 %q / %v / %v",
					user, generate, stdin, tc.user, tc.generate, tc.stdin)
			}
		})
	}
}

// base64urlPattern 是 generatePassword 该产出的形状。
//
// 直接拿 base64.RawURLEncoding 去 Decode 也能判字符集，但它同时收一些边角
// 写法；用一条锚定的正则把「24 个字符」与「只含这一组字符」一起说完。
var base64urlPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{24}$`)

func TestGeneratePassword(t *testing.T) {
	first, err := generatePassword()
	if err != nil {
		t.Fatalf("生成口令失败: %v", err)
	}

	if len(first) != 24 {
		t.Errorf("长度 = %d，期望 24（18 字节 base64url）", len(first))
	}
	if !base64urlPattern.MatchString(first) {
		t.Errorf("口令 %q 不是 24 个 base64url 字符", first)
	}
	// 生成的东西必须过得了命令自己随后要过的同一道策略。
	if policyErr := auth.ValidatePassword("jdoe", first); policyErr != nil {
		t.Errorf("生成的口令过不了 ValidatePassword: %v", policyErr)
	}

	second, err := generatePassword()
	if err != nil {
		t.Fatalf("第二次生成失败: %v", err)
	}
	if first == second {
		t.Error("两次生成得到了同一个口令：熵源没在起作用")
	}
}

func TestReadPasswordInput(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{
			name:  "行尾换行被去掉",
			input: "correct horse battery staple\n",
			want:  "correct horse battery staple",
		},
		{
			name:  "CRLF 一起去掉",
			input: "secret\r\n",
			want:  "secret",
		},
		{
			name:  "开头与中间的空白原样保留",
			input: "  pass word  \n",
			want:  "  pass word  ",
		},
		{
			name:    "空输入报错",
			input:   "",
			wantErr: true,
		},
		{
			name:    "只有一个换行也算空",
			input:   "\n",
			wantErr: true,
		},
		{
			name:  "恰好 4096 字节通过",
			input: strings.Repeat("a", maxPasswordInput),
			want:  strings.Repeat("a", maxPasswordInput),
		},
		{
			name:    "4097 字节报错",
			input:   strings.Repeat("a", maxPasswordInput+1),
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			usePasswordInput(t, strings.NewReader(tc.input))
			got, err := readPasswordInput()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望报错，得到 %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("读取失败: %v", err)
			}
			if got != tc.want {
				t.Errorf("读到的口令 = %q，期望 %q", got, tc.want)
			}
		})
	}
}

// usePasswordInput 把一个读来源换进包级变量，用例结束换回去。
//
// 不并行、不用 t.Setenv：这里换的是一条命令的输入来源，不是进程环境。
func usePasswordInput(t *testing.T, reader *strings.Reader) {
	t.Helper()
	old := passwordInput
	passwordInput = reader
	t.Cleanup(func() { passwordInput = old })
}
