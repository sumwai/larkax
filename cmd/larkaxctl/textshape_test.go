package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/textshape"
)

// TestWriteCommandsRejectControlCharsInDisplayText 过命令、在连库之前。
//
// 这一组覆盖所有「写进库、随后原样打进终端表格或单条回显」的自由文本字段。
// 每条命令都调用真的入口函数：**参数检查必须排在连库之前**，所以这里不连库
// 也能走到判据；哪天有人把它挪到 withDB 里面，用例会先报「连不上库」而不是
// 静默通过（退出码也会从 2 变成 4）。
//
// 三样都要说清：是控制字符哪一类问题、哪个字符、改哪个旗标。第一个与第三个
// 分别在错误文案与字段名里，第二个按字面断言。
func TestWriteCommandsRejectControlCharsInDisplayText(t *testing.T) {
	cases := []struct {
		name string
		run  func([]string) error
		args func(bad string) []string
		want string // 报错里应当点名的旗标
	}{
		{
			name: "user update --display-name",
			run:  runUserUpdate,
			args: func(bad string) []string {
				return []string{"--user", "1", "--display-name", bad}
			},
			want: "--display-name",
		},
		{
			name: "apikey create --name",
			run:  runAPIKeyCreate,
			args: func(bad string) []string {
				return []string{"--user", "1", "--name", bad}
			},
			want: "--name",
		},
		{
			name: "apikey update --name",
			run:  runAPIKeyUpdate,
			args: func(bad string) []string {
				return []string{"--id", "1", "--name", bad}
			},
			want: "--name",
		},
		{
			name: "channel create --name",
			run:  runChannelCreate,
			args: func(bad string) []string {
				return []string{"--name", bad}
			},
			want: "--name",
		},
		{
			name: "channel create --models",
			run:  runChannelCreate,
			args: func(bad string) []string {
				return []string{
					"--name", "渠道", "--type", "openai",
					"--base-url", "https://api.example.com/v1",
					"--models", "claude-3," + bad,
				}
			},
			want: "--models",
		},
		{
			name: "channel update --name",
			run:  runChannelUpdate,
			args: func(bad string) []string {
				return []string{"--id", "1", "--name", bad}
			},
			want: "--name",
		},
		{
			name: "channel update --models",
			run:  runChannelUpdate,
			args: func(bad string) []string {
				return []string{"--id", "1", "--models", bad}
			},
			want: "--models",
		},
		{
			name: "model create --display",
			run:  runModelCreate,
			args: func(bad string) []string {
				return []string{"--model", "claude-3", "--display", bad}
			},
			want: "--display",
		},
		{
			name: "model create --vendor",
			run:  runModelCreate,
			args: func(bad string) []string {
				return []string{"--model", "claude-3", "--display", "Claude 3", "--vendor", bad}
			},
			want: "--vendor",
		},
		{
			name: "model create --family",
			run:  runModelCreate,
			args: func(bad string) []string {
				return []string{"--model", "claude-3", "--display", "Claude 3", "--family", bad}
			},
			want: "--family",
		},
		{
			name: "model create --input-modalities",
			run:  runModelCreate,
			args: func(bad string) []string {
				return []string{"--model", "claude-3", "--display", "Claude 3",
					"--input-modalities", "text," + bad}
			},
			want: "--input-modalities",
		},
		{
			name: "model create --output-modalities",
			run:  runModelCreate,
			args: func(bad string) []string {
				return []string{"--model", "claude-3", "--display", "Claude 3",
					"--output-modalities", bad}
			},
			want: "--output-modalities",
		},
		{
			name: "model update --display",
			run:  runModelUpdate,
			args: func(bad string) []string {
				return []string{"--model", "claude-3", "--display", bad}
			},
			want: "--display",
		},
		{
			name: "model update --vendor",
			run:  runModelUpdate,
			args: func(bad string) []string {
				return []string{"--model", "claude-3", "--vendor", bad}
			},
			want: "--vendor",
		},
		{
			name: "model update --family",
			run:  runModelUpdate,
			args: func(bad string) []string {
				return []string{"--model", "claude-3", "--family", bad}
			},
			want: "--family",
		},
		{
			name: "model update --input-modalities",
			run:  runModelUpdate,
			args: func(bad string) []string {
				return []string{"--model", "claude-3", "--input-modalities", bad}
			},
			want: "--input-modalities",
		},
		{
			name: "price create --name",
			run:  runPriceCreate,
			args: func(bad string) []string {
				return []string{"--scope", "platform", "--name", bad}
			},
			want: "--name",
		},
		{
			name: "price create --models",
			run:  runPriceCreate,
			args: func(bad string) []string {
				return []string{"--scope", "platform", "--name", "平台价", "--models", bad}
			},
			want: "--models",
		},
		{
			name: "price update --name",
			run:  runPriceUpdate,
			args: func(bad string) []string {
				return []string{"--id", "1", "--by", "ops", "--name", bad}
			},
			want: "--name",
		},
		{
			name: "price update --models",
			run:  runPriceUpdate,
			args: func(bad string) []string {
				return []string{"--id", "1", "--by", "ops", "--models", bad}
			},
			want: "--models",
		},
		{
			name: "upstream-cost create --models",
			run:  runUpstreamCostCreate,
			args: func(bad string) []string {
				return []string{"--channel", "1", "--expr", "1", "--models", bad}
			},
			want: "--models",
		},
		{
			name: "upstream-cost update --models",
			run:  runUpstreamCostUpdate,
			args: func(bad string) []string {
				return []string{"--id", "1", "--models", bad}
			},
			want: "--models",
		},
		{
			name: "alias create --upstream",
			run:  runAliasCreate,
			args: func(bad string) []string {
				return []string{"--model", "claude-3", "--upstream", bad}
			},
			want: "--upstream",
		},
		{
			name: "alias update --upstream",
			run:  runAliasUpdate,
			args: func(bad string) []string {
				return []string{"--id", "1", "--upstream", bad}
			},
			want: "--upstream",
		},
		{
			name: "consent grant --request-id",
			run:  runConsentGrant,
			args: func(bad string) []string {
				return []string{"--user", "1", "--scope", "single_request", "--request-id", bad}
			},
			want: "--request-id",
		},
		{
			name: "snapshot get --reason",
			run:  runSnapshotGet,
			args: func(bad string) []string {
				return []string{"req-1", "--actor", "ops", "--reason", bad}
			},
			want: "--reason",
		},
		{
			name: "plugin kv set --key",
			run:  runPluginKVSet,
			args: func(bad string) []string {
				return []string{"--plugin", "acme-vision", "--user", "1", "--key", bad, "--value", "v"}
			},
			want: "--key",
		},
		{
			name: "plugin kv get --key",
			run:  runPluginKVGet,
			args: func(bad string) []string {
				return []string{"--plugin", "acme-vision", "--user", "1", "--key", bad}
			},
			want: "--key",
		},
	}

	chars := []struct {
		name string
		bad  string
		want string // 文案里点名的那个字符
	}{
		{"换行", "a\nb", "换行（U+000A）"},
		{"制表符", "a\tb", "制表符（U+0009）"},
		{"ESC 序列", "a\x1b[31mb", "ESC（U+001B）"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, ch := range chars {
				t.Run(ch.name, func(t *testing.T) {
					r := runOnce(t, tc.run, tc.args(ch.bad))
					if r.err == nil {
						t.Fatalf("%q 应当被拒，但命令退出了 0", ch.bad)
					}
					if code := exitCode(r.err); code != exitUsage {
						t.Errorf("退出码 = %d，想要 %d（参数错那一档）", code, exitUsage)
					}
					for _, want := range []string{"控制字符", ch.want, tc.want} {
						if !strings.Contains(r.err.Error(), want) {
							t.Errorf("报错 %q 里没有 %q", r.err.Error(), want)
						}
					}
				})
			}
		})
	}
}

// TestPluginPublishRejectsControlCharsInMeta 走编译那一步。
//
// 插件元数据不是命令行旗标：它写在源码里，由 sandbox 编译后交给 ParseMeta。
// 因此这一条只能过真源码；key、name、version 都会进 `plugin list` / `plugin get`
// 的表格，判据在 registry.ParseMeta。
func TestPluginPublishRejectsControlCharsInMeta(t *testing.T) {
	const source = `
import { definePlugin } from "larkax";

export const meta = { key: "shape-780", name: "样例", version: "1.0\n0", apiVersion: 1 };
export default definePlugin({
  onUsage() { return { promptTokens: 1 }; },
});
`
	path := filepath.Join(t.TempDir(), "plugin.js")
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatalf("写临时插件源失败: %v", err)
	}

	r := runOnce(t, runPluginPublish, []string{path, "--by", "ops"})
	if r.err == nil {
		t.Fatal("meta.version 里的换行应当被拒，但命令退出了 0")
	}
	if code := exitCode(r.err); code != exitUsage {
		t.Errorf("退出码 = %d，想要 %d（参数错那一档）", code, exitUsage)
	}
	for _, want := range []string{"控制字符", "换行（U+000A）", "meta.version"} {
		if !strings.Contains(r.err.Error(), want) {
			t.Errorf("报错 %q 里没有 %q", r.err.Error(), want)
		}
	}
}

// TestUserUpdateRejectsControlCharDisplayNameLeavesNoChange 过真命令、连真库。
//
// 两件事一起钉：命令报参数错（退出码 2），以及库里那一栏**没动**。只断言
// 报错是不够的——先写库再报错的实现同样会打出那句话。
//
// 同一字段的正常值再跑一次：判据不该把中文与空格也挡掉。
func TestUserUpdateRejectsControlCharDisplayNameLeavesNoChange(t *testing.T) {
	setupEnv(t)
	db := testDB(t)
	userID := createTestUser(t)

	r := runOnce(t, runUserUpdate, []string{
		"--user", fmt.Sprint(userID),
		"--display-name", "a\nb\x1b[31mX",
	})
	if r.err == nil {
		t.Fatal("展示名里的控制字符应当被拒，但命令退出了 0")
	}
	if code := exitCode(r.err); code != exitUsage {
		t.Errorf("退出码 = %d，想要 %d（参数错那一档）", code, exitUsage)
	}
	for _, want := range []string{"控制字符", "换行（U+000A）", "--display-name"} {
		if !strings.Contains(r.err.Error(), want) {
			t.Errorf("报错 %q 里没有 %q", r.err.Error(), want)
		}
	}

	var display sql.NullString
	if err := db.QueryRowContext(t.Context(),
		`SELECT display_name FROM users WHERE id = $1`, userID).Scan(&display); err != nil {
		t.Fatalf("查库失败: %v", err)
	}
	if display.Valid {
		t.Errorf("被拒的那一次把 display_name 写成了 %q", display.String)
	}

	normal := runOnce(t, runUserUpdate, []string{
		"--user", fmt.Sprint(userID),
		"--display-name", "张 三",
	})
	if normal.err != nil {
		t.Fatalf("正常展示名被拒: %v", normal.err)
	}
	if err := db.QueryRowContext(t.Context(),
		`SELECT display_name FROM users WHERE id = $1`, userID).Scan(&display); err != nil {
		t.Fatalf("查库失败: %v", err)
	}
	if !display.Valid || display.String != "张 三" {
		t.Errorf("落库的展示名 = %+v，期望「张 三」", display)
	}
}

// TestExitCodeUsageForTextshapeControlChar 钉住哨兵的登记。
//
// 命令自己会在连库之前包一层 usageErr，所以**今天从命令行走不到这一格**；
// 别的调用方（将来的管理接口、导入脚本）拿到的是包直接返回的那个哨兵，
// 而它只有在 usageErrors 里登记过才有退出码 2——漏登记的那一天它会安静地
// 退回 1，而 1 与 2 在脚本里是两种处置。
func TestExitCodeUsageForTextshapeControlChar(t *testing.T) {
	if got := exitCode(textshape.ErrControlChar); got != exitUsage {
		t.Errorf("textshape.ErrControlChar 的退出码应当是 %d，得到 %d", exitUsage, got)
	}

	wrapped := fmt.Errorf("写入失败: %w", textshape.ErrControlChar)
	if got := exitCode(wrapped); got != exitUsage {
		t.Errorf("包了一层之后仍应当是 %d，得到 %d", exitUsage, got)
	}
	if !errors.Is(wrapped, textshape.ErrControlChar) {
		t.Error("errors.Is 认不出这条链，上一条断言就没有意义")
	}
}
