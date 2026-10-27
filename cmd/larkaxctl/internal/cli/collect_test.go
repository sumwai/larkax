package cli

import (
	"errors"
	"slices"
	"testing"

	"github.com/sumwai/larkax/internal/config"
)

// 这一组用例测的是**收集会话**这一条契约本身（见 collect.go），不借道任何
// 导出名：它就说清「会话进行中，那四个读点各自让路」。
//
// 为什么要有它：收集期让路是补全脚本正确性的地基。`HelpOnly` 若在收集期返回
// true，`dispatch(<路径> -h)` 会当场打一页帮助返回，那条路径的旗标集根本注册
// 不出来——补全脚本安静地少掉一片旗标，而所有用例照样绿。这一条把它变成红的。

// TestHelpOnlyYieldsDuringCollection 证明收集期 HelpOnly 一律返回 false。
//
// 注入方法：把 collect.go 的 HelpOnly 里那三行 `if CollectingFlags() { return
// false }` 删掉，这条用例立刻红：
//
//	helpOnly 在收集期应当返回 false，得到 true
func TestHelpOnlyYieldsDuringCollection(t *testing.T) {
	outside := HelpOnly([]string{"-h"})
	if !outside {
		t.Fatalf("会话之外 HelpOnly([-h]) 应当 true，得到 false——" +
			"这条用例自身的前提不成立，先看 HelpOnly 的实现")
	}

	var inside bool
	CollectFlags([][]string{{"probe"}}, func([]string) {
		inside = HelpOnly([]string{"-h"})
	})
	if inside {
		t.Errorf("helpOnly 在收集期应当返回 false，得到 true：" +
			"这会让 dispatch(<路径> -h) 当场打帮助返回，那条路径一个旗标都注册不出来")
	}
}

// TestCollectingFlagsIsOnlyTrueInsideSession 证明会话状态只在 CollectFlags
// 的调用期间为真，跑完之后一定要复位。
//
// 复位的价值：`buildCompletionData` 之后还有别的代码在同一个进程里跑（用例、
// 命令本身）。状态漏在那里，后面每一次 HelpOnly / OpenDB / 位置参数解析都会
// 走收集期那一支——而这些症状与「收集期行为写错了」完全不同，排查会往错的方向走。
func TestCollectingFlagsIsOnlyTrueInsideSession(t *testing.T) {
	if CollectingFlags() {
		t.Fatal("会话之外 CollectingFlags() 应当 false")
	}
	var inside []bool
	CollectFlags([][]string{{"a"}, {"a", "b"}}, func([]string) {
		inside = append(inside, CollectingFlags())
	})
	for i, v := range inside {
		if !v {
			t.Errorf("第 %d 条路径重放时会话应当在进行中，得到 false", i)
		}
	}
	if CollectingFlags() {
		t.Error("会话结束之后 CollectingFlags() 应当 false：状态漏在了进程里")
	}
}

// TestCollectFlagsRecordsByPath 证明记录按命令路径分组，并且记下「吃不吃取值」。
//
// 分组是补全生成物的键：合成一张表的话，`channel list` 会拿到 `channel create`
// 的 --name 之类。吃不吃取值决定 fish 生成物给不给选项加 `-r`。
func TestCollectFlagsRecordsByPath(t *testing.T) {
	paths := [][]string{{"one"}, {"two", "three"}}
	got := CollectFlags(paths, func(path []string) {
		if len(path) == 1 {
			NewFlagSet("probe", 0).String("name", "", "")
			NewFlagSet("probe", 0).Bool("force", false, "")
			return
		}
		NewFlagSet("probe", 0).Int("count", 0, "")
	})

	one := got["one"]
	if len(one) != 2 {
		t.Fatalf("路径 one 记下 %d 个选项，想要 2 个：%+v", len(one), one)
	}
	if one[0].Name != "name" || !one[0].TakesValue {
		t.Errorf("第一个选项应当是 name（吃取值），得到 %+v", one[0])
	}
	if one[1].Name != "force" || one[1].TakesValue {
		t.Errorf("第二个选项应当是 force（不吃取值），得到 %+v", one[1])
	}
	if len(got["two three"]) != 1 || got["two three"][0].Name != "count" {
		t.Errorf("路径 two three 的记录不对：%+v", got["two three"])
	}
}

// TestRegisteringOutsideSessionRecordsNothing 证明会话之外注册选项不留痕。
//
// 命令照常跑的时候，每一次 NewFlagSet 都走 record；会话之外它必须是空操作，
// 否则那条路径的旗标清单会在一次正常调用之后被悄悄改掉。
func TestRegisteringOutsideSessionRecordsNothing(t *testing.T) {
	NewFlagSet("probe", 0).String("name", "", "")
	got := CollectFlags([][]string{{"one"}}, func([]string) {})
	if len(got["one"]) != 0 {
		t.Errorf("会话之外注册的选项跑进了记录里：%+v", got["one"])
	}
}

// TestOpenDBSkipsConnectionDuringCollection 证明收集期 OpenDB 直接返回、
// 不去碰 DSN。
//
// 直接传一个**解析不了**的 DSN：真去连的话它会报「打开数据库连接失败」，而
// 收集期必须返回 errFlagIntrospection。两者都是错误，所以这条用例比的是
// 「哪一条」而不是「有没有错」——只看有没有错的话，改动把这里改成真连库，
// 用例照样绿。
func TestOpenDBSkipsConnectionDuringCollection(t *testing.T) {
	var err error
	CollectFlags([][]string{{"probe"}}, func([]string) {
		// 配置本身是满的，DSN 故意写成一个解析不了的值：真去连的话报的是
		// 「打开数据库连接失败」，与这道让路的返回值不同。
		_, err = OpenDB(t.Context(), &config.OpsConfig{DBDSN: "not-a-dsn"})
	})
	if err == nil {
		t.Fatal("收集期 OpenDB 应当返回错误（不连库），得到 nil")
	}
	if !errors.Is(err, errFlagIntrospection) {
		t.Fatalf("收集期 OpenDB 应当返回 errFlagIntrospection，得到 %v", err)
	}
}

// TestLeadingOperandYieldsDuringCollection 证明收集期不摘位置参数、也不报错。
//
// 四条输入都试，是因为它们分别区分出守卫的两半语义：
//
//   - `{-h}` 与 `{}`：**去掉守卫也是同样的结果**——前者被后面的
//     `HasPrefix(args[0], "-")` 拦住，后者被 `len(args) == 0` 拦住。留着它们
//     是为了钉住「收集期的常见输入照旧放行」，不是为了证明守卫有用。
//   - `{alice, -h}`：去掉守卫会摘走 alice，返回 `("alice", {-h})`。这才是
//     「不摘位置参数」这半句话的可区分输入。
//   - `{""}`：去掉守卫会走到空串那一支，返回 `UsageErr`。这是「也不报错」
//     那半句话的可区分输入。
//
// 为什么后果严重：摘走一个词意味着那条路径后面的 flag 集少注册一片，补全脚本
// 安静地缺一块；而报错会让 dispatch 提前返回，同样一整个 flag 集都注册不出来。
// 两种都是「屏幕上什么都没变」的坏法（见 LESSONS.md 第 16 条）。
func TestLeadingOperandYieldsDuringCollection(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"只有求助旗标", []string{"-h"}},
		{"空参数", nil},
		{"位置参数在求助旗标前", []string{"alice", "-h"}},
		{"空串位置参数", []string{""}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string(nil), tc.args...)

			var operand string
			var rest []string
			var err error
			CollectFlags([][]string{{"probe"}}, func([]string) {
				operand, rest, err = TakeLeadingOperand(args, OperandSpec{What: "名字"})
			})

			if err != nil {
				t.Fatalf("收集期不该报错（报错会让 dispatch 提前返回、一整个 flag 集注册不出来），得到 %v", err)
			}
			if operand != "" {
				t.Errorf("收集期摘走了位置参数 %q：那条路径后面的 flag 集会少注册一片", operand)
			}
			if !slices.Equal(rest, tc.args) {
				t.Errorf("收集期应当原样返回参数，得到 %q，想要 %q", rest, tc.args)
			}
		})
	}
}
