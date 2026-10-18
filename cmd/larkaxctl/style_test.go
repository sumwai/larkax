package main

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/auth"
	"github.com/sumwai/larkax/internal/catalog"
	"github.com/sumwai/larkax/internal/channels"
	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/ledger/store"
	"github.com/sumwai/larkax/internal/money"
	pluginstore "github.com/sumwai/larkax/internal/plugin/store"
	"github.com/sumwai/larkax/internal/pricing"
	"github.com/sumwai/larkax/internal/settings"
	"github.com/sumwai/larkax/internal/snapshot"
)

// 上色与不上色是同一份代码的两条路，而**默认那条是不上色**：命令的输出多数
// 会被接进管道、写进文件、被别的程序读走。这几条用例钉的就是那个默认。
//
// 期望值一律写**字面量**（`\x1b[` 直接写在断言里），不引被测的常量：拿被测
// 实现算期望值，改坏了也照样通过（见 LESSONS.md 第 21 条）。

// withTerminal 把「输出目标是不是交互终端」换成给定的答案。
//
// 真实的 TTY 在 `go test` 里拿不到（stdout 是管道），所以这个判断本身是变量
// （见 style.go 的 terminalProbe）。用例是唯一读它的地方。
func withTerminal(t *testing.T, isTerminal bool) {
	t.Helper()
	saved := terminalProbe
	terminalProbe = func(io.Writer) bool { return isTerminal }
	t.Cleanup(func() { terminalProbe = saved })
}

// colorEnv 把两个「别上色」的开关显式清干净，再按需设其中一个。
//
// 不这么做的话，开发机上恰好设了 NO_COLOR 会让「该上色」的用例假红，
// 而那种红看起来像代码坏了。
func colorEnv(t *testing.T, extra ...string) {
	t.Helper()
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	if len(extra) == 2 {
		t.Setenv(extra[0], extra[1])
	}
}

// styledKeys 是样式用例共用的一份清单。
func styledKeys() []auth.Key {
	rpm := 60
	quota := money.MustParse("100")
	return []auth.Key{{
		ID: 1, UserID: 7, Username: "user-placeholder", Name: "示例",
		Prefix: "larkax_abcd", Status: "active",
		RateLimitRPM: &rpm, QuotaLimit: &quota,
		CreatedAt: time.Date(2026, 10, 17, 4, 25, 15, 0, time.UTC),
	}}
}

// styledDoctorReport 是一份「好坏都有」的报告：三档状态符都要出现，
// 否则一条只给通过项上色的实现也能过。
func styledDoctorReport() doctorReport {
	return doctorReport{
		database: doctorCheck{title: "数据库", subject: doctorSubjectDatabase, ok: "连接正常（迁移 v23）"},
		channel:  doctorCheck{title: "渠道", subject: doctorSubjectChannel, ok: "enabled 且 models 非空的渠道 1 条"},
		catalog: doctorCheck{title: "模型目录", subject: doctorSubjectCatalog,
			findings: []doctorProblem{{
				headline: `渠道 1（channel-a）的 models 里有 "model-x"，但目录里没有这个名字`,
				detail:   "多半是拼写差了一个字符，或者那条模型已经被删掉了",
			}}},
		alias:   doctorCheck{title: "别名", subject: doctorSubjectAlias, skip: "没有可服务的规范名"},
		pricing: doctorCheck{title: "计价规则", subject: doctorSubjectPricing, skip: "没有可服务的规范名"},
		wallet:  doctorCheck{title: "钱包", subject: doctorSubjectWallet, ok: "用户 1 个都有钱包行"},
	}
}

// TestNoStyleWhenNotATerminal 是这一层最要紧的一条：输出目标不是终端时
// （重定向到文件、接进管道 `| grep`、被别的程序读走），输出里**一个 ANSI
// 转义序列都没有**。
//
// 断言的是输出里没有 `\x1b[`，不是「palette.on 是 false」：开关对了而某一处
// 调用点自己拼了一段转义序列，前者照样绿。
func TestNoStyleWhenNotATerminal(t *testing.T) {
	withTerminal(t, false)
	colorEnv(t)

	var buf bytes.Buffer
	printKeyList(&buf, styledKeys())
	printDoctorReport(&buf, styledDoctorReport(), false)
	printDoctorReport(&buf, styledDoctorReport(), true)

	if strings.Contains(buf.String(), "\x1b[") {
		t.Errorf("非终端输出里出现了 ANSI 转义序列：\n%q", buf.String())
	}
	// 顺带钉住「重定向到文件时输出仍然可读」：不上色不等于不输出。
	for _, want := range []string{"✓ 数据库", "✗ 模型目录", "! 别名", "larkax_abcd"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("非终端输出里没有 %q：\n%s", want, buf.String())
		}
	}
}

// TestNoStyleWhenDisabledByEnv 钉住 NO_COLOR 与 TERM=dumb。
//
// 终端判断被注入成「是终端」，所以这两个开关只要失效就会当场现形——而它们
// 在真实终端里失效时，人只会觉得「怎么又有颜色了」，不会去报一个 bug。
func TestNoStyleWhenDisabledByEnv(t *testing.T) {
	cases := []struct {
		name  string
		env   string
		value string
	}{
		{"NO_COLOR=1", "NO_COLOR", "1"},
		{"NO_COLOR 非空即生效", "NO_COLOR", "0"},
		{"TERM=dumb", "TERM", "dumb"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withTerminal(t, true)
			colorEnv(t, tc.env, tc.value)

			var buf bytes.Buffer
			printKeyList(&buf, styledKeys())
			printDoctorReport(&buf, styledDoctorReport(), false)

			if strings.Contains(buf.String(), "\x1b[") {
				t.Errorf("%s=%s 时不该上色：\n%q", tc.env, tc.value, buf.String())
			}
		})
	}
}

// TestStyledTableStillAligns 是上色那一半：带样式码时表格仍然对齐。
//
// 一行是全角中文（`模型规范名`，10 列）、一行是 ASCII（`claude-x`，8 列），
// 第二栏必须**都**从第 12 列开始（栏宽取 10，加两列栏间距）。
//
// 期望值写死成 12 而不是「两行彼此一致」：一个把汉字按一列算的实现对所有行
// 都算错、错得一样，彼此照样一致（见 table_test.go 的 assertColumnStarts）。
func TestStyledTableStillAligns(t *testing.T) {
	withTerminal(t, true)
	colorEnv(t)

	var buf bytes.Buffer
	p := newPalette(&buf)
	tw := newTableWriter(&buf)
	_, _ = fmt.Fprintf(tw, "%s\t%s\n", p.bold("模型规范名"), p.bold("单价"))
	_, _ = fmt.Fprintf(tw, "%s\t%s\n", p.bold("claude-x"), p.dim("1.00"))
	_ = tw.Flush()

	out := buf.String()
	if !strings.Contains(out, "\x1b[") {
		t.Fatalf("注入成终端之后该有样式码：\n%q", out)
	}

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("打印了 %d 行，期望 2 行：\n%q", len(lines), out)
	}
	for i, cell := range []string{"单价", "1.00"} {
		if got := cellColumn(t, lines[i], cell); got != 12 {
			t.Errorf("第 %d 行 %q 的起点 = %d 列，期望 12（样式码不占列）：\n%q",
				i, cell, got, lines[i])
		}
	}
}

// TestDoctorCarriesTheOldSentencesIntoVerbose 钉住「长句没删」这件事。
//
// 默认输出收敛成一行，代价必须是**默认那一屏**变短，而不是措辞消失：
// 那些句子（后果、排查方向）是 -v 存在的理由。删掉它们的人会让这条用例红。
func TestDoctorCarriesTheOldSentencesIntoVerbose(t *testing.T) {
	rep := styledDoctorReport()

	var plain bytes.Buffer
	printDoctorReport(&plain, rep, false)
	const monologue = "多半是拼写差了一个字符，或者那条模型已经被删掉了"
	if strings.Contains(plain.String(), monologue) {
		t.Errorf("默认输出里不该出现那句推导：\n%s", plain.String())
	}

	var verbose bytes.Buffer
	printDoctorReport(&verbose, rep, true)
	if !strings.Contains(verbose.String(), monologue) {
		t.Errorf("-v 里该原样保留那句推导：\n%s", verbose.String())
	}
	// 默认那一行不是「什么都没说」：对象与根因要在。
	if !strings.Contains(plain.String(), "渠道 1（channel-a）") {
		t.Errorf("默认那一行要点名是哪个渠道：\n%s", plain.String())
	}
}

// TestEmptyListMessages 把空清单的文案逐条钉住。
//
// 六条命令原先各说各的（「还没有渠道。」「目录里还没有模型。」「没有 API Key。」），
// 而它们是同一件事。文案里带上下一步的命令：空清单是「还没配」，读者接下来
// 要做什么就写在那一句里。
//
// doctor 的钱包体检是第七处调用点（一个用户都没有），它的期望值在 doctor_test.go
// 里——它打的是整份报告，不是单独一句清单。第七处的存在由 renderpaths_test.go
// 按调用点计数钉住。
//
// settings list 不在这张表里：settings 表是空的与「还没有配过」不是一回事
// （键来自 Registry，加不了），那句文案留着它自己的说法。
func TestEmptyListMessages(t *testing.T) {
	cases := []struct {
		name  string
		want  string
		print func(w io.Writer)
	}{
		{"渠道", "暂无渠道。可使用 larkaxctl channel create 添加。",
			func(w io.Writer) { printChannelList(w, nil, false) }},
		{"插件", "暂无插件。可使用 larkaxctl plugin publish 添加。",
			func(w io.Writer) { printPluginList(w, nil) }},
		{"模型", "暂无模型。可使用 larkaxctl model create 添加。",
			func(w io.Writer) { printModelList(w, nil) }},
		{"计价规则", "暂无计价规则。可使用 larkaxctl price create 添加。",
			func(w io.Writer) { printRuleList(w, nil) }},
		{"API Key", "暂无 API Key。可使用 larkaxctl apikey create 添加。",
			func(w io.Writer) { printKeyList(w, nil) }},
		{"别名", "暂无别名。可使用 larkaxctl alias create 添加。",
			func(w io.Writer) { printAliasList(w, nil) }},
	}

	// 与另一张样式表同一个做法：条数写死。少了一条就等于那条没人看，而
	// 「用例还在跑、只是不覆盖那条命令」在输出里和通过长得一样。
	if len(cases) != 6 {
		t.Fatalf("这张表里 %d 条清单命令，期望 6 条", len(cases))
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			tc.print(&buf)
			if got, want := buf.String(), tc.want+"\n"; got != want {
				t.Errorf("空清单 = %q，期望 %q", got, want)
			}
		})
	}
}

// ── 用例自己的宽度折算 ──────────────────────────────────────────────────

// stripSGR 去掉样式码——**测试自己的**剥离器，刻意不复用被測的 ansiSeqLen：
// 两边共用时，「它没剥干净」与「宽度没算对」会互相掩护，一起错也照样绿。
func stripSGR(s string) string {
	for {
		start := strings.Index(s, "\x1b[")
		if start < 0 {
			return s
		}
		end := strings.IndexByte(s[start:], 'm')
		if end < 0 {
			return s
		}
		s = s[:start] + s[start+end+1:]
	}
}

// cellColumn 返回 cell 在 line 里起始的**显示列**（样式码不占列）。
//
// 列宽用 table_test.go 的 terminalCols（ASCII 一列、其余两列）。
func cellColumn(t *testing.T, line, cell string) int {
	t.Helper()
	i := strings.Index(line, cell)
	if i < 0 {
		t.Fatalf("这一行里没有 %q：%q", cell, line)
	}
	return terminalCols(stripSGR(line[:i]))
}

// ── 每张清单表都上色，且上色不改排版 ────────────────────────────────────

// TestListTablesStyleWithoutChangingLayout 把每条清单渲染路径逐个过一遍：
// **上色那一档必须真的上了色，而剥掉样式码之后与不上色那一档逐字相同**。
//
// 这一条同时钉住 issue 里的两条判据：
//
//   - 「六张表的表头都加粗，且上色前后每一栏的起点列不变」——后半句就是
//     「剥掉样式码逐字相同」，它比逐栏比起点更严：连空行、行尾空格、列间距
//     一起比了。
//   - 「非 TTY / NO_COLOR / TERM=dumb 下输出与现在逐字相同」——这里比的是
//     同一份数据的另一条路，正是那个「现在」。
//
// 反过来也要防：如果某个 print 根本没接 palette，只比「两条路相同」是绿的，
// 所以上色那一档必须同时断言出现了样式码。
//
// 表里是**调用点**，与 renderpaths_test.go 的 tableRenderPaths 登记表一一对应
// （那张表按 AST 扫出来的调用点计数，少了谁都会红）：这里给每条路径喂一份样本，
// 那张表管「有没有漏掉一条路径」。两处都要，是因为它们判的不是同一件事——
// 一条路径可能被登记了却没人喂样本，也可能有人喂了样本却没登记。
func TestListTablesStyleWithoutChangingLayout(t *testing.T) {
	at := time.Date(2026, 10, 17, 4, 25, 15, 0, time.UTC)
	channel := channels.ChannelInfo{
		ID: 1, Name: "示例渠道", Type: "openai", BaseURL: "https://api.example.com/v1",
		Status: "enabled", Models: []string{"model-x"}, Weight: 1, Priority: 1,
		CredentialKeys: []string{"k1"}, CreatedAt: at, UpdatedAt: at,
	}
	rules := []pricing.RuleInfo{{
		ID: 1, Scope: "platform", Name: "示例规则", ModelPatterns: []string{"model-x"},
		Expr: "input * 1", Priority: 1, EffectiveFrom: at, Enabled: true,
		DeclaredKeys: []string{"input"},
	}}

	cases := []struct {
		name  string
		print func(w io.Writer)
	}{
		{"渠道清单", func(w io.Writer) {
			printChannelList(w, []channels.ChannelInfo{channel}, false)
		}},
		{"渠道清单 --all", func(w io.Writer) {
			printChannelList(w, []channels.ChannelInfo{channel}, true)
		}},
		{"模型清单", func(w io.Writer) {
			printModelList(w, []catalog.Model{{
				ID: 7, CanonicalName: "claude-x", DisplayName: "示例模型",
				Vendor: "example", ContextLimit: 200000,
			}})
		}},
		{"模型详情", func(w io.Writer) {
			printModelDetail(w, catalog.Model{ID: 7, CanonicalName: "claude-x", UpdatedAt: at})
		}},
		{"插件清单", func(w io.Writer) {
			printPluginList(w, []pluginstore.Row{{
				Key: "demo", Version: "1.0.0", Name: "示例插件",
				Status: "published", Generation: 1, Priority: 1,
			}})
		}},
		{"计价规则", func(w io.Writer) { printRuleTable(w, rules, nil) }},
		{"计价规则带裁决", func(w io.Writer) {
			printRuleTable(w, rules, func(int) string { return "选中" })
		}},
		{"别名清单", func(w io.Writer) {
			printAliasList(w, []catalog.Alias{{
				ID: 1, UpstreamModel: "up-x", CanonicalName: "claude-x",
			}})
		}},
		{"配置清单", func(w io.Writer) {
			printSettingsList(w, []settings.Entry{{
				Key: "billing.default_asset", Value: []byte(`"CNY"`),
				Editable: true, UpdatedAt: at,
			}}, false)
		}},
		// 下面三条是这一轮补上的清单路径：原先只有渠道、模型、插件、计价规则、
		// 别名、配置这几条，余额汇总与允许表是「本来就在打清单、只是没走表格」，
		// 快照详情是「本来就不是列联表、只该有视觉重心」。
		{"科目余额表", func(w io.Writer) {
			printLedgerBalances(w, ledger.Asset("CNY"), &store.AccountBalancesReport{
				ByAccount: map[ledger.Asset]map[ledger.Account]money.Amount{
					"CNY": {
						ledger.PlatformRevenueAccount(): money.MustParse("12.34"),
						ledger.UserAccount(7):           money.MustParse("-1.00"),
					},
				},
			})
		}},
		{"快照允许表", func(w io.Writer) {
			printAllowlists(w, snapshot.Surfaces(), snapshot.Roots())
		}},
		{"快照详情", func(w io.Writer) {
			printSnapshot(w, io.Discard, 1, snapshotRow{
				Direction: "response", CreatedAt: at, SkeletonVersion: 2,
				CaptureMode: "full", Skeleton: map[string]any{"model": "model-x"},
			})
		}},
	}

	if len(cases) != 12 {
		t.Fatalf("这张表里 %d 条渲染路径，期望 12 条——少了一条就等于那条没人看", len(cases))
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withTerminal(t, false)
			colorEnv(t)
			var plain bytes.Buffer
			tc.print(&plain)

			withTerminal(t, true)
			var styled bytes.Buffer
			tc.print(&styled)

			if !strings.Contains(styled.String(), "\x1b[") {
				t.Errorf("这一档没上色：这条渲染路径根本没接 palette\n%s", styled.String())
			}
			if got, want := stripSGR(styled.String()), plain.String(); got != want {
				t.Errorf("剥掉样式码之后与不上色那一档不同：\n得到 %q\n期望 %q", got, want)
			}
		})
	}
}
