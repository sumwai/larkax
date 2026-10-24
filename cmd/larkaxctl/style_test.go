package main

import (
	"bytes"
	"database/sql"
	"fmt"
	"go/ast"
	"go/token"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/auth"
	calendarstore "github.com/sumwai/larkax/internal/calendar/store"
	"github.com/sumwai/larkax/internal/catalog"
	"github.com/sumwai/larkax/internal/channels"
	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/ledger/store"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/payment"
	pluginstore "github.com/sumwai/larkax/internal/plugin/store"
	"github.com/sumwai/larkax/internal/pricing"
	"github.com/sumwai/larkax/internal/settings"
	"github.com/sumwai/larkax/internal/snapshot"
	"github.com/sumwai/larkax/internal/upstream"
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
		ruleModels: doctorCheck{title: "规则模型名", subject: doctorSubjectRuleModels,
			ok: "2 条规则、3 个模型名都在目录里"},
		wallet:          doctorCheck{title: "钱包", subject: doctorSubjectWallet, ok: "用户 1 个都有钱包行"},
		inboundShadowed: doctorCheck{title: "入站别名遮蔽", subject: doctorSubjectInboundShadowed, ok: "1 条入站别名里没有被同名规范模型遮蔽的"},
		inboundDangling: doctorCheck{title: "入站别名悬空", subject: doctorSubjectInboundDangling, ok: "1 条入站别名的规范名都在目录里"},
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
// 八条命令原先各说各的（「还没有渠道。」「目录里还没有模型。」「没有 API Key。」），
// 而它们是同一件事。文案里带上下一步的命令：空清单是「还没配」，读者接下来
// 要做什么就写在那一句里。
//
// **提示语指向的是那条命令的正式动词。** price 那一格的正式动词是 add（create 是
// 它的旧名，不进补全），所以这里写 price add。其余几格仍写 create——add 作为全仓
// 正式动词是另一轮的事，那一轮会把这张表里凡是「建记录」的几格一起改；在那之前
// 两种写法并存是已知的中间态，不是漏改。
//
// doctor 的钱包体检是这张表之外的一处调用点（一个用户都没有），它的期望值在
// doctor_test.go 里——它打的是整份报告，不是单独一句清单。表外那几处的存在由
// renderpaths_test.go 按调用点计数钉住。
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
		{"计价规则", "暂无计价规则。可使用 larkaxctl price add 添加。",
			func(w io.Writer) { printRuleList(w, nil) }},
		{"阶梯档位", "暂无阶梯档位。可使用 larkaxctl price tier add 添加。",
			func(w io.Writer) { printTierList(w, nil) }},
		{"峰段", "暂无峰段。可使用 larkaxctl price peak add 添加。",
			func(w io.Writer) { printPeakList(w, nil) }},
		{"API Key", "暂无 API Key。可使用 larkaxctl apikey create 添加。",
			func(w io.Writer) { printKeyList(w, nil) }},
		{"别名", "暂无别名。可使用 larkaxctl alias create 添加。",
			func(w io.Writer) { printAliasList(w, nil, "别名", "alias create") }},
		{"映射", "暂无映射。可使用 larkaxctl channel map add 添加。",
			func(w io.Writer) { printAliasList(w, nil, "映射", "channel map add") }},
		{"用户", "暂无用户。可使用 larkaxctl user create 添加。",
			func(w io.Writer) { printUserList(w, nil) }},
		{"钱包", "暂无钱包。可使用 larkaxctl wallet create 添加。",
			func(w io.Writer) { printWalletList(w, nil) }},
		{"日历", "暂无日历。可使用 larkaxctl price calendar import 添加。",
			func(w io.Writer) { printCalendarList(w, nil, false) }},
	}

	// 与另一张样式表同一个做法：条数写死。少了一条就等于那条没人看，而
	// 「用例还在跑、只是不覆盖那条命令」在输出里和通过长得一样。
	if len(cases) != 12 {
		t.Fatalf("这张表里 %d 条清单命令，期望 12 条", len(cases))
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

// ── 帮助文本上色 ────────────────────────────────────────────────────────

// helpTextRounding 把样式码换成看得见的记号，报错时能一眼看出是哪一段被上了色。
//
// 只用在断言失败的输出里：期望值仍然写字面量（`\x1b[1m` 直接写），不引这个函数。
func helpTextRounding(s string) string {
	s = strings.ReplaceAll(s, "\x1b[1m", "«加粗»")
	s = strings.ReplaceAll(s, "\x1b[2m", "«置灰»")
	return strings.ReplaceAll(s, "\x1b[0m", "«/»")
}

// helpTextsForStyling 收齐包内全部帮助文本：根帮助页，加上每个 `*Usage` 常量，
// 以及带占位符的 `*UsageTemplate`（见下）。
//
// 按**常量名**扫而不是逐条罗列：帮助文本有四十来份，罗列的那一份与新加一条命令
// 时漏掉的那一份都看不出来（与 renderpaths_test.go 里那张豁免表同一个口径）。
// 扫常量而不是扫今天能走到的那几处调用点，比后者严：文本是包级的，将来多一条
// 打印路径不会绕过这几条用例。
//
// `*UsageTemplate` 也收：usage 的帮助要引用别处的清单（--status 的合法取值），
// 正文因此写成带上 `%s` 的模板常量、由 `fmt.Sprintf` 在包初始化时渲染。这不
// 是顺手的整理——只认 `*Usage` 的话，那两份正文会静默从这道门里消失（实测：
// 子用例少了两条，`ok` 上却一样）。扫描口径只多了这一个后缀，其余判据不动。
//
// 顺带扫进来几个不像帮助正文的 `…Usage`（例如某个 flag 的用法说明那一行）：
// 判据对它们同样成立，留着不影响什么。
func helpTextsForStyling(t *testing.T) []struct{ what, text string } {
	t.Helper()

	var texts []struct{ what, text string }
	forEachNonTestFile(t, func(_ string, file *ast.File) {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				values, ok := spec.(*ast.ValueSpec)
				if !ok || (!namesEndWith(values.Names, "Usage") &&
					!namesEndWith(values.Names, "UsageTemplate")) {
					continue
				}
				name := values.Names[0].Name
				for _, v := range values.Values {
					ast.Inspect(v, func(n ast.Node) bool {
						lit, ok := n.(*ast.BasicLit)
						if !ok || lit.Kind != token.STRING {
							return true
						}
						value, err := strconv.Unquote(lit.Value)
						if err != nil {
							return true
						}
						texts = append(texts, struct{ what, text string }{name, value})
						return true
					})
				}
			}
		}
	})

	// 条数写死：这份清单是好几条用例的覆盖面，少了十几条时它们全都还在跑，
	// 只是不再覆盖那些文本——那在输出里和通过长得一样。
	if len(texts) < 30 {
		t.Fatalf("只扫到 %d 份帮助文本，期望三十份以上：扫描器坏了，或者口径变了——两种都不算通过", len(texts))
	}
	// 根帮助页是**渲染出来的**（buildUsage），不在常量里，单独收一份。
	return append(texts, struct{ what, text string }{"根帮助页", usage})
}

// TestHelpTextStylesWithoutChangingContent 是这一组里最要紧的一条：上色那一档
// 必须真的上了色，而**剥掉样式码之后与不上色那一档逐字相同**。
//
// 后半句是 issue 里点名的判据，它比「每一栏的起点列不变」更严：连行尾空格、
// 空行、示例里的花括号一起比了——上色只许插样式码，一个字符都不许增删。
//
// 反过来也要防：某个文本没接上 palette 时只比「两档相同」是绿的，所以上色那
// 一档同时断言出现了样式码。
func TestHelpTextStylesWithoutChangingContent(t *testing.T) {
	for _, tc := range helpTextsForStyling(t) {
		t.Run(tc.what, func(t *testing.T) {
			withTerminal(t, false)
			colorEnv(t)
			var plain bytes.Buffer
			printUsage(&plain, tc.text)

			withTerminal(t, true)
			var styled bytes.Buffer
			printUsage(&styled, tc.text)

			if !strings.Contains(styled.String(), "\x1b[") {
				t.Fatalf("这一份没上色：它没走 palette\n%s", styled.String())
			}
			if got, want := stripSGR(styled.String()), plain.String(); got != want {
				t.Errorf("剥掉样式码之后与不上色那一档不同：\n得到 %s\n期望 %s",
					helpTextRounding(got), helpTextRounding(want))
			}
			if plain.String() != tc.text {
				t.Errorf("不上色那一档改了原文：\n得到 %q\n期望 %q", plain.String(), tc.text)
			}
		})
	}
}

// TestHelpColoursStructureOnly 钉住「哪些东西被上色」。
//
// 期望值写死成字面量（`\x1b[1m` 直接写在断言里），不引 palette 的常量、也不拿
// 被测实现算一遍：那样改坏了也照样通过（见 LESSONS.md 第 21 条）。
//
// 两份样本各管一半：version 的短版管段标题、首行、两栏行与指针行；根帮助页管
// 组标题、`全局选项…` 那一段的标题与命令清单——后者是**渲染出来的**（buildUsage
// 拼的），与前者的形状不同。
func TestHelpColoursStructureOnly(t *testing.T) {
	withTerminal(t, true)
	colorEnv(t)

	var short bytes.Buffer
	printUsage(&short, versionShortUsage)

	for _, want := range []string{
		"\x1b[1m用法：\x1b[0m",                                  // 段标题：整行加粗
		"\x1b[1mlarkaxctl version\x1b[0m",                    // 首行里的命令名
		"\x1b[2m 打印版本号与编译元数据。\x1b[0m",                        // 首行里的那句话
		"\x1b[1mlarkaxctl version -o json\x1b[0m",            // 两栏行的名字栏（用法）
		"\x1b[2m打成 {\"ok\":true,\"version\":\"...\"}\x1b[0m", // 它的说明栏
		"\x1b[2m更细的边界：larkaxctl help version。\x1b[0m",        // 指针行：整行置灰
	} {
		if !strings.Contains(short.String(), want) {
			t.Errorf("version 短版里没有 %q：\n%s", want, helpTextRounding(short.String()))
		}
	}
	// 说明文字不许加粗：加粗的是这一页的**结构**（命令名、段标题），不是
	// 「这句话重要」。少一条这样的反向断言，一个「整页加粗」的实现照样通过。
	if strings.Contains(short.String(), "\x1b[1m 打印版本号") {
		t.Errorf("说明文字被加粗了：\n%s", helpTextRounding(short.String()))
	}
	if strings.Contains(short.String(), "\x1b[2m用法：") {
		t.Errorf("段标题被置灰了：\n%s", helpTextRounding(short.String()))
	}

	var root bytes.Buffer
	printUsage(&root, usage)
	for _, want := range []string{
		"\x1b[1m核心与诊断 (Core & Diagnostics)：\x1b[0m", // 组标题
		"\x1b[1mdoctor\x1b[0m",                         // 清单里的命令名
		"\x1b[2m系统健康与链路连通性体检\x1b[0m",                   // 它后面那一句话
		"\x1b[1m用法：\x1b[0m",                            // 段标题：整行加粗
		"\x1b[1m全局选项（写在子命令前后都认）：\x1b[0m",               // usageNotes 里的段标题
		"\x1b[1m--env-file <路径>\x1b[0m",                // 全局选项的名字栏
		"\x1b[2m每条命令的详细用法：larkaxctl help <命令>。\x1b[0m", // 指针行
	} {
		if !strings.Contains(root.String(), want) {
			t.Errorf("根帮助页里没有 %q：\n%s", want, helpTextRounding(root.String()))
		}
	}
	// 与 version 页那条同一条反向断言：段标题被置灰，说明它不再满足段标题的
	// 判据（顶格 + 以「：」结尾 + 下一非空行缩进）。根页这一处最容易踩到——
	// 「用法：」与它的骨架合成一行时信息没少一个字，只是标题这一层没了，
	// 而少了这条断言时上面那些"有"的判据一条都不会响。
	if strings.Contains(root.String(), "\x1b[2m用法：") {
		t.Errorf("根帮助页的段标题被置灰了：\n%s", helpTextRounding(root.String()))
	}
}

// TestHelpLineJudgement 把逐行判据的几种边角钉死。
//
// 上面那条用例证明「上色不改内容」，这条证明**哪一段**被上了色。表里的输入都是
// 还能认出出处的最小片段：段标题引出的是缩进块还是顶格正文、名字栏里含宽字符时
// 说明栏落在哪一列、以及两种「宁可不上色」的续行（说明里换行下来的选项名、
// 示例里的 JSON）。续行那两条是回归：加粗它们会让半句话从一段置灰的说明里跳
// 出来，或者把示例里的花括号一起调暗。
func TestHelpLineJudgement(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{"空行与末尾的空行原样", "\n\n", "\n\n"},
		{"只有空格的行原样", "  \n", "  \n"},
		{"段标题引出一块缩进的内容：整行加粗",
			"用法：\n  larkaxctl version\n",
			"\x1b[1m用法：\x1b[0m\n  \x1b[1mlarkaxctl version\x1b[0m\n"},
		{"以冒号结尾但后面是顶格正文：当说明文字置灰",
			"下面两个写法仍然可用：\n\n顶格的正文\n",
			"\x1b[2m下面两个写法仍然可用：\x1b[0m\n\n\x1b[2m顶格的正文\x1b[0m\n"},
		{"首行：命令名加粗，那句话置灰",
			"larkaxctl version 打印版本号。\n",
			"\x1b[1mlarkaxctl version\x1b[0m\x1b[2m 打印版本号。\x1b[0m\n"},
		{"选项写在命令名前：整行置灰（不是一个命令名）",
			"larkaxctl -q help doctor\n",
			"\x1b[2mlarkaxctl -q help doctor\x1b[0m\n"},
		{"两栏行：说明栏按显示列补到原来那一列",
			"  a   b\n",
			"  \x1b[1ma\x1b[0m   \x1b[2mb\x1b[0m\n"},
		{"名字栏含宽字符时也补到同一列",
			"  模型  展示\n",
			"  \x1b[1m模型\x1b[0m  \x1b[2m展示\x1b[0m\n"},
		{"说明的续行原样（哪怕它以一个选项名开头）",
			"  说明的续行\n                     --env-file 要的是 .env 文件的路径。\n",
			"  说明的续行\n                     --env-file 要的是 .env 文件的路径。\n"},
		{"示例里的 JSON 原样", "  \"region\": \"CN\",\n", "  \"region\": \"CN\",\n"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withTerminal(t, true)
			colorEnv(t)

			var buf bytes.Buffer
			printUsage(&buf, tc.text)
			if got := buf.String(); got != tc.want {
				t.Errorf("上色后 =\n%s\n期望 =\n%s", helpTextRounding(got), helpTextRounding(tc.want))
			}
		})
	}
}

// TestHelpTextHasNoStyleWhenColourIsOff 钉住四种「一个 `\x1b` 都没有」的情形。
//
// 四种里有三种在 palette 那一层判（非终端、NO_COLOR 非空、TERM=dumb，见 style.go
// 的 newPalette），一种在 printUsage 那一层判（`--format json`：给管道下游的是
// 纯文本）。三条 palette 的判据终端判断被注入成「是终端」，所以它们只要失效就
// 当场现形——而在真实终端里失效时，人只会觉得「怎么又有颜色了」，不会去报 bug。
//
// 断言的是输出里没有 `\x1b`，不是「palette.on 是 false」：开关对了而某一处调用点
// 自己拼了一段转义序列，后者照样绿。顺带钉住「不上色不等于不输出」。
func TestHelpTextHasNoStyleWhenColourIsOff(t *testing.T) {
	cases := []struct {
		name  string
		apply func(t *testing.T)
	}{
		{"输出不是终端", func(t *testing.T) {
			withTerminal(t, false)
			colorEnv(t)
		}},
		{"NO_COLOR 非空", func(t *testing.T) {
			withTerminal(t, true)
			colorEnv(t, "NO_COLOR", "1")
		}},
		{"TERM=dumb", func(t *testing.T) {
			withTerminal(t, true)
			colorEnv(t, "TERM", "dumb")
		}},
		{"--format json", func(t *testing.T) {
			withTerminal(t, true)
			colorEnv(t)
			outputFormat = formatJSON
			t.Cleanup(func() { outputFormat = formatTable })
		}},
	}

	texts := helpTextsForStyling(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.apply(t)
			for _, one := range texts {
				var buf bytes.Buffer
				printUsage(&buf, one.text)
				if strings.Contains(buf.String(), "\x1b") {
					t.Errorf("%s：%s 里出现了转义序列：\n%q", tc.name, one.what, buf.String())
				}
				if buf.String() != one.text {
					t.Errorf("%s：%s 的输出与原文不同：\n得到 %q\n期望 %q",
						tc.name, one.what, buf.String(), one.text)
				}
			}
		})
	}
}

// TestHelpPathsAreColouredAndAgree 钉住三条打帮助的路都上色，且 `-h` 与
// `--help` 的行为一致。
//
// 三条路各有各的出口（`-h` / `--help` 走各命令自己的 flag 集、`larkaxctl help
// <命令>` 走分派），而它们都要经过 printUsage 这一个出口——这一条钉的就是
// 「都经过了」。`-h` 与 `--help` 逐字节比：flag 包把两者当同一个写法，自己接
// `-h` 的命令（version 就是）得跟上，不然脚本里两个写法给出不同的输出。
//
// `--quiet` 那一档在两个写法上都过一遍：它压的是「给人看」那一档的输出，而
// 帮助是使用者主动要的，照打、照上色。
func TestHelpPathsAreColouredAndAgree(t *testing.T) {
	withTerminal(t, true)
	colorEnv(t)

	// 根帮助页也有一条路：`larkaxctl help` 不给命令名。
	var rootOut, rootErr string
	if err := runQuietly(t, []string{"help"}, &rootOut, &rootErr); err != nil {
		t.Fatalf("larkaxctl help 出错: %v", err)
	}
	if !strings.Contains(rootOut, "\x1b[") || stripSGR(rootOut) != usage {
		t.Errorf("larkaxctl help 打的根帮助页没上色或内容变了：\n%s", helpTextRounding(rootOut))
	}

	for _, g := range commandGroups {
		for _, c := range g.commands {
			t.Run(c.name, func(t *testing.T) {
				dashH, dashHErr := helpOf(t, c, "-h")
				longH, longHErr := helpOf(t, c, "--help")
				if dashH != longH || dashHErr != longHErr {
					t.Errorf("-h 与 --help 的输出不同：\n-h 得到 %q / %q\n--help 得到 %q / %q",
						dashH, dashHErr, longH, longHErr)
				}
				if !strings.Contains(dashH+dashHErr, "\x1b[") {
					t.Errorf("%s -h 没上色：\n%s", c.name, helpTextRounding(dashH+dashHErr))
				}
				if out := stripSGR(dashH + dashHErr); !strings.Contains(out, c.help.short) {
					t.Errorf("%s -h 打的不是短版：\n%s", c.name, out)
				}

				var stdout, stderr string
				if err := runQuietly(t, []string{"help", c.name}, &stdout, &stderr); err != nil {
					t.Fatalf("larkaxctl help %s 出错: %v（stderr=%q）", c.name, err, stderr)
				}
				if !strings.Contains(stdout, "\x1b[") {
					t.Errorf("larkaxctl help %s 没上色：\n%s", c.name, helpTextRounding(stdout))
				}
				if out := stripSGR(stdout); !strings.Contains(out, c.help.long) {
					t.Errorf("larkaxctl help %s 打的不是长版：\n%s", c.name, out)
				}
			})
		}
	}
}

// TestQuietStillColoursHelp 钉住 `-q` 那一档：帮助照打、照上色。
//
// 「照打」由 usage_test.go 的 TestQuietDoesNotSuppressHelp 钉着，那一半很容易在
// 收口时丢掉（帮助一改走 humanStdout()，`larkaxctl -q help doctor` 就会安静地
// 什么都不打）。这一半钉的是别把 -q 顺手接进上色的判据里。
func TestQuietStillColoursHelp(t *testing.T) {
	withTerminal(t, true)
	colorEnv(t)

	quietOutput = true
	t.Cleanup(func() { quietOutput = false })

	var stdout, stderr string
	if err := runQuietly(t, []string{"help", "channel"}, &stdout, &stderr); err != nil {
		t.Fatalf("larkaxctl -q help channel 出错: %v", err)
	}
	if !strings.Contains(stdout, "\x1b[") {
		t.Errorf("-q 之下帮助没上色：\n%s", helpTextRounding(stdout))
	}
	if out := stripSGR(stdout); !strings.Contains(out, channelUsage) {
		t.Errorf("-q 之下帮助没打全：\n%s", out)
	}
}

// helpOf 跑一条命令的 `-h` / `--help`，返回它打出来的两股输出。
func helpOf(t *testing.T, c command, flag string) (string, string) {
	t.Helper()

	var err error
	stdout, stderr := captureStreams(t, func() { err = c.run([]string{flag}) })
	// flag 解析到 -h / --help 时返回 ErrHelp，而那时用法已经打过了：那不是失败，
	// main 也照这个判据收尾（见 helpRequested）。
	if err != nil && !helpRequested(err) {
		t.Fatalf("%s %s 报错了: %v", c.name, flag, err)
	}
	return stdout, stderr
}

// runQuietly 跑一次 dispatch，把两股输出收回来。
func runQuietly(t *testing.T, args []string, stdout, stderr *string) error {
	t.Helper()

	var err error
	out, errOut := captureStreams(t, func() { err = dispatch(args) })
	*stdout, *stderr = out, errOut
	return err
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
	// 健康那三张表的样本：一栏都不留空，于是「打不打得出」与「空值怎么排」两件事
	// 在这条用例里都看得到（后者的判据在 channel_test.go 的字段用例里）。
	healthP50, healthP95 := 80, 240
	healthCost := "1.50000000"
	healthRow := channels.ChannelHealth{
		ID: 1, Name: "示例渠道", Health: "degraded", LastProbeAt: &at,
		Requests: 40, Successes: 39, Failures: 1,
		CacheHitTokens: 300, CacheMissTokens: 700,
		LatencyP50ms: &healthP50, LatencyP95ms: &healthP95, Cost: &healthCost,
	}
	healthBucket := channels.HealthBucket{
		Bucket: at, Requests: 40, Successes: 39, Failures: 1,
		CacheHitTokens: 300, CacheMissTokens: 700,
		LatencyP95ms: &healthP95, Cost: &healthCost,
	}
	rules := []pricing.RuleInfo{{
		ID: 1, Scope: "platform", Name: "示例规则", ModelPatterns: []string{"model-x"},
		Expr: "input * 1", Priority: 1, EffectiveFrom: at, Enabled: true,
		DeclaredKeys: []string{"input"},
	}}
	costTo := at.Add(time.Hour)
	costRules := []pricing.CostInfo{{
		ID: 1, ChannelID: 3, ModelPatterns: []string{"model-x"},
		Expr: `{"prompt": 0.0000012}`, Priority: 2,
		EffectiveFrom: at, EffectiveTo: &costTo,
		DeclaredKeys: []string{"completion", "prompt"},
	}}

	// 适配器清单用真实的探测注册表：它由内置清单装配，内容固定，
	// 而这条用例要比的是排版（表头加粗、剥掉样式码后逐字相同），不是数据。
	adapterRegistry, regErr := upstream.ProbeRegistry()
	if regErr != nil {
		t.Fatalf("ProbeRegistry 出错: %v", regErr)
	}

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
		{"渠道详情", func(w io.Writer) {
			printChannelDetail(w, channel)
		}},
		{"渠道健康汇总", func(w io.Writer) {
			printChannelHealthList(w, []channels.ChannelHealth{healthRow}, false)
		}},
		{"渠道健康详情", func(w io.Writer) {
			printChannelHealthDetail(w, healthRow)
		}},
		{"渠道健康逐小时", func(w io.Writer) {
			printChannelHealthHours(w, []channels.HealthBucket{healthBucket})
		}},
		{"渠道上游模型清单", func(w io.Writer) {
			printChannelUpstreamModels(w, channels.ChannelModels{
				ChannelID: 1, ChannelName: "示例渠道", Status: channels.ModelSyncSynced,
				AttemptedAt: at, SyncedAt: at,
				Models: []channels.StoredModel{{
					UpstreamModel: "upstream-model-x", FirstSeenAt: at, LastSeenAt: at,
				}},
			})
		}},
		{"适配器清单", func(w io.Writer) { printChannelAdapters(w, adapterRegistry) }},
		{"模型清单", func(w io.Writer) {
			printModelList(w, []catalog.Model{{
				ID: 7, CanonicalName: "claude-x", DisplayName: "示例模型",
				Vendor: "example", ContextLimit: 200000,
			}})
		}},
		{"模型详情", func(w io.Writer) {
			printModelDetail(w, catalog.Model{ID: 7, CanonicalName: "claude-x", UpdatedAt: at})
		}},
		{"key 详情", func(w io.Writer) {
			printKeyDetail(w, auth.Key{
				ID: 3, Username: "user-placeholder", Prefix: "larkax_abcd",
				Name: "ops", Status: "active", CreatedAt: at,
			})
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
		{"阶梯清单", func(w io.Writer) {
			printTierList(w, []priceTierInfo{{
				segment: 2, above: 200000,
				prices: map[string]float64{"prompt": 2e-7, "completion": 8e-7},
			}})
		}},
		{"峰段清单", func(w io.Writer) {
			above := int64(100000)
			printPeakList(w, []pricePeakInfo{{
				segment: 3, window: "01:00-04:00,06:00-10:00",
				weekdays: []string{"mon", "tue", "wed", "thu", "fri"},
				dayKind:  "work", above: &above,
				prices: map[string]float64{"prompt": 3e-7, "completion": 1.2e-6},
			}})
		}},
		// 成本规则那两条与上面两条同形：同一张表的两档（不带裁决的清单、
		// 带裁决的说明），只是数据源换成 upstream_costs。
		{"成本规则", func(w io.Writer) { printCostTable(w, costRules, nil) }},
		{"成本规则带裁决", func(w io.Writer) {
			printCostTable(w, costRules, func(int) string { return "选中" })
		}},
		{"别名清单", func(w io.Writer) {
			printAliasList(w, []catalog.Alias{{
				ID: 1, UpstreamModel: "up-x", CanonicalName: "claude-x",
			}}, "别名", "alias create")
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
		// 下面四条是 #694 补上的单条详情与日历清单：与上面同一类，都是
		// 「主键加粗、表头加粗」的清单表。
		{"插件详情", func(w io.Writer) {
			printPluginDetail(w, pluginstore.Detail{
				Row: pluginstore.Row{
					Key: "demo", Version: "1.0.0", Name: "示例插件",
					Status: pluginstore.StatusActive, Generation: 1, Priority: 1,
				},
				CreatedAt: at, Versions: 1,
			})
		}},
		{"插件 KV 详情", func(w io.Writer) {
			printPluginKVEntry(w, kvTarget{plugin: "demo", user: "user-placeholder", key: "k"}, "v")
		}},
		{"配置详情", func(w io.Writer) {
			printSettingsDetail(w, settings.Entry{
				Key: "billing.default_asset", Value: []byte(`"CNY"`),
				Editable: true, UpdatedAt: at,
			}, false)
		}},
		{"日历清单", func(w io.Writer) {
			printCalendarList(w, []calendarstore.Year{{
				Region: "CN", Year: 2031, Status: "synced", DayCount: 365, SyncedAt: at,
			}}, false)
		}},
		// 下面四条是三条只读入口带出来的清单：#695 补上的用量事件、计费请求、
		// 对账运行记录与快照访问记录。与被截断时那一句同样，它们都是
		// 「主键加粗、表头加粗」的清单表。
		{"用量事件", func(w io.Writer) {
			printUsageEvents(w, []usageEventRow{{
				CreatedAt: at, UserID: 7, Username: "user-placeholder",
				CanonicalModel: "claude-x",
				PromptTokens:   1200, CompletionTokens: 800,
				CacheReadTokens: 300, CacheWriteTokens: 100,
				BillingPath: "measured", Cost: money.MustParse("0.01234500"),
			}}, false)
		}},
		{"计费请求", func(w io.Writer) {
			printUsageRequests(w, []usageRequestRow{{
				StartedAt: at, RequestID: "req-placeholder", UserID: 7,
				Username: "user-placeholder", CanonicalModel: "claude-x",
				Status:             "settled",
				ReservedAmount:     money.MustParse("0.02000000"),
				PlanReservedAmount: money.MustParse("0.01000000"),
				FinalAmount:        sql.NullString{String: "0.01234500", Valid: true},
			}}, false)
		}},
		{"用量汇总", func(w io.Writer) {
			printUsageSummary(w, at, at.Add(24*time.Hour), time.UTC, store.UsageSummary{
				Totals: store.UsageTotals{
					Cost: money.MustParse("2.50"), PlanReservedAmount: money.MustParse("3.25"),
					Requests:     3,
					PromptTokens: 137, CompletionTokens: 73,
					CacheReadTokens: 12, CacheWriteTokens: 6, ReasoningTokens: 8,
				},
				Paths: []store.UsagePathTotal{
					{Path: "measured", Cost: money.MustParse("1.75"), Requests: 2},
					{Path: "estimated", Cost: money.MustParse("0.75"), Requests: 1},
					{Path: "fallback"},
				},
			})
		}},
		{"对账运行记录", func(w io.Writer) {
			printLedgerRuns(w, []store.ReconcileRun{{
				ID: 3, StartedAt: at, Watermark: at, FullScan: true,
				Status: "done", CheckedWallets: 12,
			}}, false)
		}},
		{"快照访问记录", func(w io.Writer) {
			printSnapshotAccessLog(w, []snapshotAccessRow{{
				ID: 1, SnapshotID: 9, RequestID: "req-placeholder",
				ActorUserID: sql.NullInt64{Int64: 7, Valid: true},
				ActorKind:   "admin",
				Reason:      sql.NullString{String: "排查一次上游报错", Valid: true},
				AccessedAt:  at,
			}}, map[int64]string{7: "user-placeholder"}, false)
		}},
		// 下面一条是 wallet adjust 的回显：它不是清单，但同样是「主键加粗、
		// 一栏一行」的那张表（与 user update 的改动回显同一个形状）。
		{"调账回显", func(w io.Writer) {
			printAdjustedWallet(w, walletAdjustment{
				Owner:    auth.User{ID: 7, Username: "user-placeholder"},
				Operator: auth.User{ID: 8, Username: "ops-placeholder"},
				Asset:    ledger.AssetCNY,
				Before:   money.MustParse("100.00"),
				After:    money.MustParse("50.00"),
				Delta:    money.MustParse("-50.00"),
				Reason:   "误扣补回",
				Ref:      "ORD-20261019-1",
				Txn: ledger.Transaction{
					TxnID: "00000000-0000-8000-8000-000000000000",
					Entries: []ledger.Entry{{
						TxnID:   "00000000-0000-8000-8000-000000000000",
						Account: ledger.UserAccount(7), Asset: ledger.AssetCNY,
						Amount: money.MustParse("50.00"), EntryType: ledger.EntryAdjustment,
						Memo: "误扣补回（操作人 ops-placeholder，单据 ORD-20261019-1）",
					}},
				},
			}, true, false)
		}},
		// 下面一条是提现的回显：与调账回显同一类（一栏一行），只是几栏随状态走
		// ——审批人与审批时刻只有走过审批的单子才有。
		{"提现回显", func(w io.Writer) {
			reviewedBy := int64(8)
			printPayout(w, payoutReceipt{
				Verdict: "已批准提现单 3。",
				Payout: store.Payout{
					ID: 3, UserID: 7, Asset: ledger.AssetCNY, Amount: money.MustParse("88.00"),
					Status: store.PayoutApproved, ReviewedBy: &reviewedBy, ReviewedAt: &at,
					ReviewNote: "核对无误", CreatedAt: at,
				},
				User:     auth.User{ID: 7, Username: "user-placeholder"},
				Reviewer: &auth.User{ID: 8, Username: "ops-placeholder"},
			}, false)
		}},
		// 销户回显：与调账、提现同一类（一栏一行），只是几栏随处置结果走。
		{"销户回显", func(w io.Writer) {
			printDeletionReceipt(w, deletionPurgeReceipt(auth.DeletionResult{
				UserID:          7,
				OldUsername:     "user-placeholder",
				NewUsername:     "deleted-user-7",
				OldEmail:        "user@example.com",
				NewEmail:        "deleted-user-7@example.com",
				OldDisplayName:  "示例用户",
				NewDisplayName:  "已注销用户 #7",
				DeletedAt:       at,
				KeysRevoked:     2,
				KeysTotal:       3,
				PluginKVDeleted: 4,
				ConsentsRevoked: 1,
				LedgerEntries:   9,
				Balance:         money.MustParse("0"),
				Frozen:          money.MustParse("0"),
				WalletRows:      1,
			}))
		}},
		// 下面一条是支付单的回显：与提现回显同一类（一栏一行）。status 那一条会
		// 多出「通道状态」一栏，这里取能同时覆盖两栏的那一种。
		{"支付单回显", func(w io.Writer) {
			channelStatus := payment.StatusCreated
			printPayment(w, paymentReceipt{
				Verdict: "支付单 ORD-20261019-1 的现状。",
				Order: payment.Order{
					OrderNo: "ORD-20261019-1", UserID: 7, Gateway: "fake",
					Asset: ledger.AssetCNY, Amount: money.MustParse("66.00"),
					Status: payment.StatusPaid, GatewayTxnID: "TRADE-placeholder",
					Fee: money.MustParse("0.66"), PaidAt: &at, CreatedAt: at,
				},
				User:          auth.User{ID: 7, Username: "user-placeholder"},
				ChannelStatus: &channelStatus,
			}, false)
		}},
		// 管理令牌的回显：stdout 只有那一行令牌，说明去 stderr。它不随数据变长，
		// 但与上面几条回显同样「主键（这里是令牌）加粗」。
		{"管理令牌回显", func(w io.Writer) {
			printAdminToken(w, io.Discard, "token-placeholder")
		}},
	}

	if len(cases) != 37 {
		t.Fatalf("这张表里 %d 条渲染路径，期望 37 条——少了一条就等于那条没人看", len(cases))
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
