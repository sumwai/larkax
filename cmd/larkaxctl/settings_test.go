package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/settings"
)

func TestSettingsUnknownSubcommand(t *testing.T) {
	err := runSettings([]string{})
	if err == nil || !strings.Contains(err.Error(), "需要子命令") {
		t.Fatalf("无子命令时应报错，得到 %v", err)
	}

	err = runSettings([]string{"bogus"})
	if err == nil || !strings.Contains(err.Error(), "未知的 settings 子命令") {
		t.Fatalf("未知子命令时应报错，得到 %v", err)
	}
}

// TestSettingsSetRejectsBadArgs 走的是**不碰数据库**的那几条路径。
//
// 键名与值的类型都在连库之前校验（见 settings.EncodeValue）：
// 一个写错的命令不该先连上生产库再报错，而「连上库之后才发现参数不对」
// 还有一个更坏的可能——它没报错。
func TestSettingsSetRejectsBadArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"没有 --key", []string{"--value", "true", "--by", "ops"}, "缺少 --key"},
		{"没有 --by", []string{"--key", "plugin.enabled", "--value", "true"}, "缺少 --by"},
		{"--by 为空",
			[]string{"--key", "plugin.enabled", "--value", "true", "--by", ""}, "缺少 --by"},
		{"位置参数",
			[]string{"plugin.enabled", "--key", "plugin.enabled", "--by", "ops"}, "不接受位置参数"},
		{"未声明的键",
			[]string{"--key", "typo.retentoin_days", "--value", "7", "--by", "ops"}, "Registry"},
		{"值的类型不对",
			[]string{"--key", "plugin.enabled", "--value", "1", "--by", "ops"}, "true 或 false"},
		{"整数键给了文字",
			[]string{"--key", "request_snapshot.retention_days", "--value", "七天", "--by", "ops"}, "整数"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runSettingsSet(tc.args)
			if err == nil {
				t.Fatalf("期望报错，却通过了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息 %q 里没有 %q", err.Error(), tc.want)
			}
		})
	}
}

func TestSettingsListRejectsPositional(t *testing.T) {
	err := runSettingsList([]string{"extra"})
	if err == nil || !strings.Contains(err.Error(), "不接受位置参数") {
		t.Fatalf("期望报错，得到 %v", err)
	}
}

// TestPrintSettingsList 钉住每一栏的取值。
//
// 值打的是 jsonb 原文：字符串带引号、布尔与整数不带。这不是排版偏好——
// 这一列里到底存了什么正是核对时要看的东西，而一个漏了引号的字符串配置
// 解码之后与正常的看起来一模一样，可它会让服务起不来。
func TestPrintSettingsList(t *testing.T) {
	by := int64(7)
	at := time.Date(2026, 10, 17, 6, 10, 0, 0, time.UTC)

	var buf bytes.Buffer
	printSettingsList(&buf, []settings.Entry{
		{
			Key: "plugin.enabled", Value: []byte(`false`),
			Description: "是否启用插件系统。", Editable: true,
			UpdatedBy: &by, UpdatedAt: at,
		},
		{
			Key: "billing.default_asset", Value: []byte(`"CNY"`),
			Description: "新用户钱包的默认资产品种。", Editable: false,
			UpdatedAt: at,
		},
		{
			// 未声明的键：它是「服务加载不起来」的直接原因，要在类型那一栏
			// 标出来，而不是靠人记得去比对 Registry。
			Key: "typo.key", Value: []byte(`1`),
			Editable: true, UpdatedAt: at,
		},
	})

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	// 一行表头；plugin.enabled 三行（元数据、说明、尚未接线）；
	// billing.default_asset 两行；typo.key 一行（没说明，也不是未接线的键）。
	if len(lines) != 1+3+2+1 {
		t.Fatalf("打印了 %d 行，期望 7 行：\n%s", len(lines), buf.String())
	}

	if !strings.Contains(lines[0], "jsonb 原文") {
		t.Errorf("表头没有说明值那一列是什么：%q", lines[0])
	}

	first := lines[1]
	for _, want := range []string{
		"plugin.enabled = false", "[bool]", "可改 是", "改自 7", "改于 2026-10-17T06:10:00Z",
	} {
		if !strings.Contains(first, want) {
			t.Errorf("第一行里没有 %q：\n%s", want, first)
		}
	}
	if !strings.Contains(lines[2], "说明：是否启用插件系统。") {
		t.Errorf("第二行没有说明：%q", lines[2])
	}

	// 还没有读者的键要在清单里当场说出来。这一行取自 Registry（见
	// settings.Spec.Unwired），所以它是一道真的门：谁把 Unwired 清空了，
	// 这条就挂——而清空正意味着「接上了」或「标错了」，两种都该看一眼。
	//
	// 断言写死字面量，不拿 Registry 里的值当期望：那等于让被测对象给自己
	// 出题，把文案改掉也照样通过。
	if !strings.Contains(lines[3], "尚未接线") {
		t.Errorf("未接线的键没有被标出来：%q", lines[3])
	}
	if !strings.Contains(lines[3], "插件系统尚未接线（#216），改了不会有任何效果") {
		t.Errorf("未接线那一行没有说清后果：%q", lines[3])
	}

	// 字符串配置的值带引号：这就是它在 jsonb 里的样子。
	second := lines[4]
	for _, want := range []string{
		`billing.default_asset = "CNY"`, "[string]", "可改 否", "改自 -",
	} {
		if !strings.Contains(second, want) {
			t.Errorf("第三行里没有 %q：\n%s", want, second)
		}
	}
	// 接线正常的键不该带上那一行：否则「标出来了」就不再是一个信号。
	if strings.Contains(strings.Join(lines[4:6], "\n"), "尚未接线") {
		t.Errorf("已接线的键被标成了未接线：\n%s", strings.Join(lines[4:6], "\n"))
	}

	third := lines[6]
	if !strings.Contains(third, "typo.key = 1") || !strings.Contains(third, "[未声明]") {
		t.Errorf("未声明的键没有被标出来：%q", third)
	}
}

func TestPrintSettingsListEmpty(t *testing.T) {
	var buf bytes.Buffer
	printSettingsList(&buf, nil)
	if got, want := buf.String(), "settings 表是空的。\n"; got != want {
		t.Errorf("空清单 = %q，期望 %q", got, want)
	}
}
