package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/sumwai/larkax/cmd/larkaxctl/internal/cli"
)

// --env-file 是**全局**选项：它必须与任何子命令一起用，写在子命令前后都认。
//
// 摘掉再分派（而不是给每条子命令各接一个同名选项）的做法由这一组用例钉住：
// 二十来条子命令各接一遍的话，漏掉一条的表现是那条命令认不出这个选项。
func TestTakeEnvFileFlag(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantFile string
		wantRest []string
	}{
		{
			name:     "没有这个选项",
			args:     []string{"doctor", "--live"},
			wantRest: []string{"doctor", "--live"},
		},
		{
			name:     "写在子命令前面",
			args:     []string{"--env-file", ".env.staging", "doctor"},
			wantFile: ".env.staging",
			wantRest: []string{"doctor"},
		},
		{
			name:     "写在子命令后面",
			args:     []string{"doctor", "--env-file", ".env.staging"},
			wantFile: ".env.staging",
			wantRest: []string{"doctor"},
		},
		{
			name:     "等号写法",
			args:     []string{"doctor", "--env-file=.env.staging"},
			wantFile: ".env.staging",
			wantRest: []string{"doctor"},
		},
		{
			name:     "单横线写法",
			args:     []string{"-env-file", "x.env", "doctor"},
			wantFile: "x.env",
			wantRest: []string{"doctor"},
		},
		{
			name:     "夹在子命令与它的位置参数之间",
			args:     []string{"migrate", "--env-file", "x.env", "up"},
			wantFile: "x.env",
			wantRest: []string{"migrate", "up"},
		},
		{
			name:     "出现多次取最后一个",
			args:     []string{"--env-file", "a.env", "doctor", "--env-file", "b.env"},
			wantFile: "b.env",
			wantRest: []string{"doctor"},
		},
		{
			name:     "其余参数一个不动",
			args:     []string{"price", "explain", "--model", "claude-x", "--message", "-1"},
			wantRest: []string{"price", "explain", "--model", "claude-x", "--message", "-1"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			file, rest, err := takeEnvFileFlag(tc.args)
			if err != nil {
				t.Fatalf("不该报错: %v", err)
			}
			if file != tc.wantFile {
				t.Errorf("文件 = %q，期望 %q", file, tc.wantFile)
			}
			if !slices.Equal(rest, tc.wantRest) {
				t.Errorf("剩余参数 = %q，期望 %q", rest, tc.wantRest)
			}
		})
	}
}

// 取值缺失或为空都是参数错（退出码 2），而且错误里要点到那个选项名——
// 报错时不点名的表现是使用者不知道是哪一处写错了。
func TestTakeEnvFileFlagRejectsEmptyPath(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"选项在最后没有取值", []string{"doctor", "--env-file"}},
		{"等号后面是空的", []string{"doctor", "--env-file="}},
		{"取值是空串", []string{"--env-file", "", "doctor"}},
		{"单横线写法同理", []string{"doctor", "-env-file"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := takeEnvFileFlag(tc.args)
			if err == nil {
				t.Fatal("应报错")
			}
			if code := cli.ExitCode(err); code != cli.ExitUsage {
				t.Errorf("退出码 = %d，期望 %d（参数错）", code, cli.ExitUsage)
			}
			if !strings.Contains(err.Error(), "env-file") {
				t.Errorf("错误信息未点到那个选项: %v", err)
			}
		})
	}
}
