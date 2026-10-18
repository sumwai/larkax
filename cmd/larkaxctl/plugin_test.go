package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/plugin"
	"github.com/sumwai/larkax/internal/plugin/sandbox"
	pluginstore "github.com/sumwai/larkax/internal/plugin/store"
)

// ── 不碰数据库的那几条 ────────────────────────────────────────────────

func TestPluginUnknownSubcommand(t *testing.T) {
	err := runCaptured(t, func() error { return runPlugin(nil) })
	if err == nil || !strings.Contains(err.Error(), "需要子命令") {
		t.Fatalf("无子命令时应报错，得到 %v", err)
	}
	if got := exitCode(err); got != exitUsage {
		t.Errorf("退出码 = %d，期望 %d", got, exitUsage)
	}

	err = runCaptured(t, func() error { return runPlugin([]string{"bogus"}) })
	if err == nil || !strings.Contains(err.Error(), "未知的 plugin 子命令") {
		t.Fatalf("未知子命令时应报错，得到 %v", err)
	}
	if got := exitCode(err); got != exitUsage {
		t.Errorf("退出码 = %d，期望 %d", got, exitUsage)
	}
}

// 参数校验全部发生在连库之前。
//
// 这一条不设 LARKAX_DB_DSN：一个写错的命令不该先连上生产库再报错，
// 而「连上库之后才发现参数不对」还有一个更坏的可能——它没报错。
func TestPluginRejectsBadArgs(t *testing.T) {
	cases := []struct {
		name string
		fn   func() error
		want string
	}{
		{"list 带位置参数",
			func() error { return runPluginList([]string{"extra"}) }, "不接受位置参数"},
		{"publish 没有文件",
			func() error { return runPluginPublish(nil) }, "缺少插件源文件路径"},
		{"publish 两个文件",
			func() error { return runPluginPublish([]string{"a.js", "b.js"}) }, "只接受一个源文件"},
		{"enable 没有 key",
			func() error { return runPluginSetEnabled(nil, true) }, "需要一个插件 key"},
		{"enable 空 key",
			func() error { return runPluginSetEnabled([]string{"  "}, true) }, "不能为空"},
		{"disable 两个 key",
			func() error { return runPluginSetEnabled([]string{"a", "b"}, false) }, "需要一个插件 key"},
		{"publish 文件不存在",
			func() error { return runPluginPublish([]string{"/nonexistent/plugin.js"}) }, "读取插件源文件失败"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runCaptured(t, tc.fn)
			if err == nil {
				t.Fatal("期望报错，却通过了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息 %q 里没有 %q", err.Error(), tc.want)
			}
			if got := exitCode(err); got != exitUsage {
				t.Errorf("退出码 = %d，期望 %d（参数问题）", got, exitUsage)
			}
		})
	}
}

// 判据：编译不过的插件以非零退出，且**库里不会多出一行**。
//
// 这一条的重点是「先编译再落库」这个顺序。反过来写（先落库再编译）时，
// 这一条会挂在「库里多了一行」上，而它在命令行上只表现为一句「编译失败」。
//
// 语法错的那段源码里没有可用的 meta，所以这里显式给 --key：这样断言才能
// 落到「这个 key 下有没有行」上，而不是「随便哪个 key 下有没有行」。
func TestPluginPublishRejectsBrokenSourceWithoutWriting(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	key := uniqueName("broken")
	path := writePluginFile(t, "broken.js", "import { definePlugin } from \"larkax\";\nexport default { onUsage() { ")

	err := runCaptured(t, func() error {
		return runPluginPublish([]string{path, "--key", key})
	})
	if err == nil {
		t.Fatal("编译不过的插件该被拒绝")
	}
	if !strings.Contains(err.Error(), "插件编译失败") {
		t.Errorf("错误信息 %q 里没有「插件编译失败」", err.Error())
	}
	// 错误里要有**分类**，不能只有引擎的原文：上传的人要据此判断改哪儿。
	if !strings.Contains(err.Error(), "（") || !strings.Contains(err.Error(), "）") {
		t.Errorf("错误信息 %q 里没有分类标签", err.Error())
	}
	if got := exitCode(err); got == exitOK {
		t.Errorf("退出码 = %d，期望非零", got)
	}

	if n := countPlugins(t, db, key); n != 0 {
		t.Errorf("编译不过的插件在库里留下了 %d 行，该是 0 行", n)
	}
}

// 编译失败的分类要能分辨「元数据缺 key」与「源码语法错」。
//
// 分类来自哨兵错误，不是去匹配引擎的文案——文案会随引擎版本变，而分类
// 决定上传的人去改哪一处。这里直接喂哨兵，所以它测的是映射表本身。
func TestCompileErrorKind(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{plugin.ErrMetaMissingKey, "元数据缺少 key"},
		{plugin.ErrMetaAPIVersion, "契约版本不受支持"},
		{plugin.ErrMetaCapability, "申请了宿主未开放的能力"},
		{sandbox.ErrNoDefaultExport, "没有 export default"},
		{sandbox.ErrMemoryLimit, "求值顶层超出内存上限"},
		{sandbox.ErrCallTimeout, "求值顶层超时"},
		{errors.New("Unexpected token }"), "源码编译不过"},
	}

	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			// 包一层：真实路径上错误是从沙箱里 wrap 出来的，不是裸的哨兵。
			wrapped := fmt.Errorf("编译插件 %q: %w", "p", tc.err)
			if got := compileErrorKind(wrapped); got != tc.want {
				t.Errorf("compileErrorKind = %q，期望 %q", got, tc.want)
			}
		})
	}
}

// meta 里没有 version 的插件不发：它在库里会与别的版本挤成同一个 (key, ”)，
// 第二次发布就变成覆盖第一代，而那种「怎么发都是同一行」要盯着 generation
// 才看得出来。
func TestPluginPublishRejectsMissingVersion(t *testing.T) {
	setupEnv(t)

	path := writePluginFile(t, "noversion.js",
		"import { definePlugin } from \"larkax\";\n"+
			"export const meta = { key: \"noversion\", apiVersion: 1 };\n"+
			"export default definePlugin({ onUsage() { return {}; } });\n")

	err := runCaptured(t, func() error { return runPluginPublish([]string{path}) })
	if err == nil {
		t.Fatal("meta 里没有 version 时该被拒绝")
	}
	if !strings.Contains(err.Error(), "version") {
		t.Errorf("错误信息 %q 里没有提到 version", err.Error())
	}
	if got := exitCode(err); got != exitUsage {
		t.Errorf("退出码 = %d，期望 %d（参数问题：源码里少写了一个字段）", got, exitUsage)
	}
}

// printPluginList 把 meta 有问题的行标出来，而不是崩掉或跳过它。
//
// 这一条测的是**渲染**：库那一半在 internal/plugin/store 的集成用例里。
// 分开是有意的——这里的输入是构造出来的 Row，所以三种坏形状都能摆出来，
// 不必真去改库。
func TestPrintPluginListFlagsBadMeta(t *testing.T) {
	var buf bytes.Buffer
	printPluginList(&buf, []pluginstore.Row{
		{Key: "good", Version: "1.0.0", Status: "active", Generation: 0, Priority: 3, Name: "好的"},
		{Key: "bad", Version: "2.0.0", Status: "pending", Generation: 1, Name: "坏的",
			MetaProblem: "meta 里的 key \"另一个名字\" 与行上的 key \"bad\" 不一致"},
	})

	out := buf.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	// 表头 + 两行 + 一行说明。说明**在表之后**：先给全貌，再给异常。
	if len(lines) != 4 {
		t.Fatalf("打印了 %d 行，期望 4 行：\n%s", len(lines), out)
	}
	if !strings.Contains(lines[0], "KEY") || !strings.Contains(lines[0], "GENERATION") {
		t.Errorf("表头用的是库里的列名：%q", lines[0])
	}
	// 坏掉的那一行**仍然要出现在表里**：整次报错或跳过它，都会让最需要看
	// 清单的那一刻看不到东西。
	for _, want := range []string{"good", "bad", "active", "pending"} {
		if !strings.Contains(out, want) {
			t.Errorf("清单里没有 %q：\n%s", want, out)
		}
	}
	if !strings.Contains(lines[3], "插件 bad 的 meta 有问题") {
		t.Errorf("坏 meta 没有被标出来：%q", lines[3])
	}
	// 好的那条不该被标：标了就不再是信号。
	if strings.Contains(out, "插件 good 的 meta 有问题") {
		t.Errorf("meta 正常的插件被标成了有问题：\n%s", out)
	}
}

// 空清单要说一句话，而不是什么都不输出。
//
// 「暂无插件」与「命令因为某个原因没跑起来」在安静的终端里长得一样
// （见 LESSONS.md 第 16 条）。文案与其余清单命令统一：见 style_test.go 的
// TestEmptyListMessages。
func TestPrintPluginListEmpty(t *testing.T) {
	var buf bytes.Buffer
	printPluginList(&buf, nil)
	if got, want := buf.String(), "暂无插件。可使用 larkaxctl plugin publish 添加。\n"; got != want {
		t.Errorf("空清单 = %q，期望 %q", got, want)
	}
}

// 判据：「在跑」那一栏按四档算，而文本、json 与文档里的推导式说的是同一套。
//
// 四档各造一个 key（见 docs/cli-reference.md 的「plugin」一节那张表）：
//
//   - 取中的那一代、上一次加载装上了 → 是 / 装上了；
//   - 取中的那一代、上一次加载跳过了它 → 否 / 没装成，原文在表后；
//   - 取中的那一代、还没有加载记录 → **还没试**。这一档与「是」混起来最贵：三列由
//     加载器写下来，它还没写过，而「取中」不等于「装过」；
//   - 这个 key 没有取中的一代 → 否（代次 0 也是合法值，不能拿它当「没有」）。
//
// 另外三个 key 各钉一件事：两代同代次时只有取中的那一行是「是」（代次会撞车，
// 见 store.IsRunning）；手工改库的行结果有值而时刻没有，打的不能是零值时刻；同一个
// key 的旧代身上不显示取中那一代的加载结果。
//
// 这一条测的是**渲染**：库那一半（Row 上那几个字段从哪几列来）在
// internal/plugin/store 的集成用例里。
func TestPluginOutputsShowLoadResultBesideSelectedGeneration(t *testing.T) {
	const loadErr = "申请了未开放的能力: kv"
	at := time.Date(2026, 10, 19, 3, 4, 5, 0, time.UTC)

	// 加载结果按 key 给：取中的那一代被跳过之后，这个 key 的每一行都带着它。
	demoPicked := &pluginstore.Generation{Version: "2.0.0", Number: 1}
	demoLoad := &pluginstore.LoadAttempt{Result: pluginstore.LoadResultFailed, Error: loadErr, At: at}
	newest := pluginstore.Row{
		Key: "demo", Version: "2.0.0", Status: pluginstore.StatusActive, Generation: 1,
		Name: "演示", Latest: true, Running: demoPicked, RunningLoad: demoLoad,
	}
	rows := []pluginstore.Row{
		newest,
		{
			// 同一个 key 的旧一代：它不显示取中那一代的加载结果。
			Key: "demo", Version: "1.0.0", Status: pluginstore.StatusActive, Generation: 0,
			Name: "演示", Running: demoPicked, RunningLoad: demoLoad,
		}, {
			Key: "fine", Version: "3.0.0", Status: pluginstore.StatusActive, Generation: 0,
			Name: "顺利", Latest: true,
			Running:     &pluginstore.Generation{Version: "3.0.0", Number: 0},
			RunningLoad: &pluginstore.LoadAttempt{Result: pluginstore.LoadResultOK, At: at},
		}, {
			Key: "quiet", Version: "4.0.0", Status: pluginstore.StatusActive, Generation: 0,
			Name: "还没轮到", Latest: true,
			Running: &pluginstore.Generation{Version: "4.0.0", Number: 0},
		}, {
			Key: "idle", Version: "5.0.0", Status: pluginstore.StatusPending, Generation: 0,
			Name: "没启用", Latest: true,
		}, {
			// 两代同代次（#754 之前写下的行：那时覆盖发布把代次原地加一，旧行不搬）：
			// 取中的是按版本名定下来的那一行，而「取中的那一行」只能有一行。
			Key: "twice", Version: "7.0.0", Status: pluginstore.StatusActive, Generation: 1,
			Name: "两代", Latest: true,
			Running:     &pluginstore.Generation{Version: "7.0.0", Number: 1},
			RunningLoad: &pluginstore.LoadAttempt{Result: pluginstore.LoadResultOK, At: at},
		}, {
			Key: "twice", Version: "6.0.0", Status: pluginstore.StatusActive, Generation: 1,
			Name: "两代", Latest: true,
			Running:     &pluginstore.Generation{Version: "7.0.0", Number: 1},
			RunningLoad: &pluginstore.LoadAttempt{Result: pluginstore.LoadResultOK, At: at},
		}, {
			// 手工改库的行：结果那一列有值而时刻没有。
			Key: "stale", Version: "8.0.0", Status: pluginstore.StatusActive, Generation: 0,
			Name: "没有时刻", Latest: true,
			Running:     &pluginstore.Generation{Version: "8.0.0", Number: 0},
			RunningLoad: &pluginstore.LoadAttempt{Result: pluginstore.LoadResultFailed, Error: loadErr},
		},
	}

	// 每一行该看到的那两栏。八行恰好把四档走过一遍。
	wantMarks := map[string][2]string{
		"demo 2.0.0":  {"否", "没装成"},
		"demo 1.0.0":  {"否", "-"},
		"fine 3.0.0":  {"是", "装上了"},
		"quiet 4.0.0": {"还没试", "-"},
		"idle 5.0.0":  {"否", "-"},
		"twice 7.0.0": {"是", "装上了"},
		"twice 6.0.0": {"否", "-"},
		"stale 8.0.0": {"否", "没装成"},
	}

	// ── list：两栏 + 表后那两句原文 ──
	var list bytes.Buffer
	printPluginList(&list, rows)
	listOut := list.String()

	head := strings.Split(listOut, "\n")[0]
	for _, col := range []string{"在跑", "上次加载"} {
		if !strings.Contains(head, col) {
			t.Errorf("清单的表头该有「%s」那一栏：%q", col, head)
		}
	}
	// 表格按 key + 版本取最后两栏，不按下标：多一栏、少一栏时这里会漏掉，而漏掉
	// 会被下面那条「每一行都要出现」抓住。表后那几句说明的字段数正好也是八个，
	// 所以只认夹具里那些 key。
	gotMarks := map[string][2]string{}
	for _, line := range strings.Split(listOut, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 8 {
			continue
		}
		mark := fields[0] + " " + fields[1]
		if _, ok := wantMarks[mark]; !ok {
			continue
		}
		gotMarks[mark] = [2]string{fields[6], fields[7]}
	}
	for mark, want := range wantMarks {
		got, ok := gotMarks[mark]
		if !ok {
			t.Errorf("清单里没有 %s 那一行（或者它不再是八栏）", mark)
			continue
		}
		if got != want {
			t.Errorf("%s 的最后两栏 = %v，想要 %v", mark, got, want)
		}
	}

	// 表后逐条给出原文与时刻：失败的那两行各一句，装上了、还没试过、没启用的都不打。
	wantNote := "插件 demo 的第 1 代（2.0.0）没装成，现在这个 key 哪一代都没在跑：" +
		loadErr + "（2026-10-19T03:04:05Z）"
	if row := lineEndingWith(t, listOut, "插件 demo"); row != wantNote {
		t.Errorf("表后那一句 = %q，想要 %q", row, wantNote)
	}
	// 手工改库的行没有时刻：打「0001-01-01T00:00:00Z」会像一个真时刻。
	wantStale := "插件 stale 的第 0 代（8.0.0）没装成，现在这个 key 哪一代都没在跑：" +
		loadErr + "（没有记下时刻）"
	if row := lineEndingWith(t, listOut, "插件 stale"); row != wantStale {
		t.Errorf("没有时刻的那一句 = %q，想要 %q", row, wantStale)
	}
	for _, marker := range []string{"插件 fine", "插件 quiet", "插件 idle", "插件 twice"} {
		if strings.Contains(listOut, marker) {
			t.Errorf("没有失败的 key 不该被说明（%s）：\n%s", marker, listOut)
		}
	}

	// ── get：四档各打一遍 ──
	detail := func(d pluginstore.Detail) string {
		var buf bytes.Buffer
		printPluginDetail(&buf, d)
		return buf.String()
	}
	// 「上次加载」那一栏按标签在行首找：同一个页面里还有别的行带着「加载」两个字。
	loadRow := func(t *testing.T, out string) string {
		t.Helper()
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "上次加载") {
				return strings.TrimRight(line, " ")
			}
		}
		t.Fatalf("详情里没有「上次加载」那一行：\n%s", out)
		return ""
	}

	getOut := detail(pluginstore.Detail{Row: newest, CreatedAt: at, Versions: 2})
	if row := lineEndingWith(t, getOut, "在跑"); !strings.HasSuffix(row, "没有：取的是第 1 代（2.0.0），上一次加载没装成") {
		t.Errorf("「在跑」那一行 = %q，该说清取中但没装成", row)
	}
	if row := loadRow(t, getOut); !strings.HasSuffix(row, "没装成（2026-10-19T03:04:05Z）") {
		t.Errorf("「上次加载」那一行 = %q，该带上结果与时刻", row)
	}
	wantGetNote := "第 1 代（2.0.0）上次加载没装成：" + loadErr
	if row := lineEndingWith(t, getOut, "上次加载没装成："); row != wantGetNote {
		t.Errorf("表后那一句 = %q，想要 %q", row, wantGetNote)
	}

	// 取中但还没有加载记录：说成「还没试」，并给两条来路各自的下一步——这个设置
	// 关着时加载器不读表，重启多少次都不会有记录。
	quietOut := detail(pluginstore.Detail{Row: rows[3], CreatedAt: at, Versions: 1})
	if row := lineEndingWith(t, quietOut, "在跑"); !strings.HasSuffix(row, "没有：取的是第 0 代（4.0.0），但它还没有加载记录") {
		t.Errorf("「在跑」那一行 = %q，该说清取中但还没试过", row)
	}
	if row := loadRow(t, quietOut); !strings.HasSuffix(row,
		"没有记录：加载器还没试过它——发布或启用之后还没重启过，或者 plugin.enabled 是关着的"+
			"（那时加载器不读表，重启也不会有记录）") {
		t.Errorf("「上次加载」那一行 = %q，该把两条来路分开说", row)
	}
	if strings.Contains(quietOut, "上次加载没装成") {
		t.Errorf("还没试过的不该被说成没装成：\n%s", quietOut)
	}

	// 这个 key 哪一代都没取中：两栏各说一句，不是留白。
	idleOut := detail(pluginstore.Detail{Row: rows[4], CreatedAt: at, Versions: 1})
	if row := lineEndingWith(t, idleOut, "在跑"); !strings.HasSuffix(row, "没有：这个 key 里哪一代都没在跑") {
		t.Errorf("「在跑」那一行 = %q，该说清没有取中的一代", row)
	}
	if row := loadRow(t, idleOut); !strings.HasSuffix(row, "不适用：这个 key 里没有取中的一代（见「在跑」那一栏）") {
		t.Errorf("「上次加载」那一行 = %q，该说清没东西可试", row)
	}

	// 手工改库的行：结果有值而时刻没有。
	staleOut := detail(pluginstore.Detail{Row: rows[7], CreatedAt: at, Versions: 1})
	if row := loadRow(t, staleOut); !strings.HasSuffix(row, "没装成（没有记下时刻）") {
		t.Errorf("没有时刻的那一行 = %q，不该打零值时刻", row)
	}
	if strings.Contains(staleOut, "0001-01-01") {
		t.Errorf("零值时刻不该出现在输出里：\n%s", staleOut)
	}

	// ── 文本那一栏与 json 那几栏是同一套 ──
	var listDoc struct {
		Plugins []pluginEntryDoc `json:"plugins"`
	}
	decodeDoc(t, pluginListDoc(rows), &listDoc)
	if len(listDoc.Plugins) != len(rows) {
		t.Fatalf("清单编出来 %d 条，想要 %d 条", len(listDoc.Plugins), len(rows))
	}
	// 按文档里那条推导式从 json 算一遍，再与表格那一栏比：两边分开算，改了任一边
	// 而另一边没跟上都会红。这一条是「四处同一套」在测试里的落点。
	for _, entry := range listDoc.Plugins {
		mark := entry.Key + " " + entry.Version
		want, ok := wantMarks[mark]
		if !ok {
			t.Fatalf("json 里多了一条 %s", mark)
		}
		if got := runningFromDoc(entry.RunningGeneration, entry.Version, entry.Generation, entry.RunningLoad); got != want[0] {
			t.Errorf("%s 的 json 推出「%s」，表格那一栏是「%s」", mark, got, want[0])
		}
	}
	// 具体那几栏也点一遍：推导式对得上，字段本身还得是原来那几样。
	entryFor := func(mark string) pluginEntryDoc {
		for _, entry := range listDoc.Plugins {
			if entry.Key+" "+entry.Version == mark {
				return entry
			}
		}
		t.Fatalf("json 里没有 %s", mark)
		return pluginEntryDoc{}
	}
	if got := entryFor("demo 2.0.0"); got.RunningGeneration == nil ||
		got.RunningGeneration.Version != "2.0.0" || got.RunningGeneration.Generation != 1 {
		t.Errorf("这一条要说得出取中的是哪一代：%+v", got.RunningGeneration)
	}
	if got := entryFor("demo 2.0.0"); got.RunningLoad == nil ||
		got.RunningLoad.Result != pluginstore.LoadResultFailed ||
		got.RunningLoad.Error != loadErr || got.RunningLoad.At != "2026-10-19T03:04:05Z" {
		t.Errorf("这一条要说得出那一次加载的结果与原文：%+v", got.RunningLoad)
	}
	if got := entryFor("fine 3.0.0"); got.RunningLoad == nil || got.RunningLoad.Result != pluginstore.LoadResultOK ||
		got.RunningLoad.Error != "" {
		t.Errorf("装上了那一条的原文该是空串（编成 null）：%+v", got.RunningLoad)
	}
	if got := entryFor("quiet 4.0.0"); got.RunningLoad != nil {
		t.Errorf("还没被加载过的那一条该是 null：%+v", got.RunningLoad)
	}
	if got := entryFor("idle 5.0.0"); got.RunningGeneration != nil {
		t.Errorf("没有取中的一代时该是 null：%+v", got.RunningGeneration)
	}
	// 手工改库的行没有时刻：json 那一栏也是 null，不是 0001-01-01。
	if got := entryFor("stale 8.0.0"); got.RunningLoad == nil || got.RunningLoad.At != "" {
		t.Errorf("没有时刻时 at 该是 null：%+v", got.RunningLoad)
	}

	var getDoc struct {
		Plugin pluginEntryDoc `json:"plugin"`
	}
	decodeDoc(t, pluginGetDoc(pluginstore.Detail{Row: newest, CreatedAt: at, Versions: 2}), &getDoc)
	if getDoc.Plugin.RunningLoad == nil || getDoc.Plugin.RunningLoad.Result != pluginstore.LoadResultFailed {
		t.Errorf("详情那一条该带上 running_load：%+v", getDoc.Plugin.RunningLoad)
	}
	if getDoc.Plugin.RunningGeneration == nil || getDoc.Plugin.RunningGeneration.Version != "2.0.0" {
		t.Errorf("详情那一条要说得出取中的是哪一代：%+v", getDoc.Plugin.RunningGeneration)
	}
}

// pluginEntryDoc / pluginGenerationDoc / pluginLoadDoc 是 json 档在测试里的形状。
//
// 具名而不是就地声明：推导式那一条要拿着它们与表格比，而两处要用同一个类型。
type pluginGenerationDoc struct {
	Version    string `json:"version"`
	Generation int64  `json:"generation"`
}

type pluginLoadDoc struct {
	Result string `json:"result"`
	Error  string `json:"error"`
	At     string `json:"at"`
}

type pluginEntryDoc struct {
	Key               string               `json:"key"`
	Version           string               `json:"version"`
	Generation        int64                `json:"generation"`
	Status            string               `json:"status"`
	Latest            bool                 `json:"latest"`
	CompileError      *string              `json:"compile_error"`
	RunningGeneration *pluginGenerationDoc `json:"running_generation"`
	RunningLoad       *pluginLoadDoc       `json:"running_load"`
}

// runningFromDoc 按 docs/cli-reference.md 里那条推导式算出 json 这一条是哪一档。
//
// 照抄一遍那条规则，是为了让「文本档与 json 档是同一套」变成一条能挂的断言：两边
// 分开算、再比一次，改了任一边而另一边没跟上都会红。规则本身写在文档里，这里不
// 另立一份口径——它要是变了，这条用例与文档一起改。
//
// 两份判断都不能少：running_generation 与 running_load 都按 key 给，同一个 key 的
// 每一条带着同一份，所以要判的是**这一条自己**是不是取中的那一条——version 与
// generation 都比（代次会撞车，只比代次会在同一个 key 里撞出两行来）。
func runningFromDoc(run *pluginGenerationDoc, version string, generation int64, load *pluginLoadDoc) string {
	if run == nil || run.Version != version || run.Generation != generation {
		return "否"
	}
	switch {
	case load == nil:
		return "还没试"
	case load.Result == "failed":
		return "否"
	default:
		return "是"
	}
}

// ── 真库上的那几条 ────────────────────────────────────────────────────

// 发布 → 清单 → 启用 → 停用，走一遍完整的手工流程。
func TestPluginPublishEnableDisable(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	key := uniqueName("cli")
	path := writePluginFile(t, "cli.js", pluginSource(key, "1.0.0", 0))

	out, _ := captureStreams(t, func() {
		if err := runPluginPublish([]string{path, "--priority", "4"}); err != nil {
			t.Errorf("发布失败: %v", err)
		}
	})
	if !strings.Contains(out, "已发布") {
		t.Errorf("发布之后的输出里没有「已发布」：%q", out)
	}
	// 新发布的插件是 pending，输出要**当场说出来**并给出下一步，
	// 否则使用者会以为发布完就生效了。
	if !strings.Contains(out, "还没有生效") || !strings.Contains(out, "plugin enable "+key) {
		t.Errorf("新发布的插件没有被提示「还没生效」及怎么启用：%q", out)
	}

	// 清单里能看到它，状态是 pending，优先级是刚给的那个。
	listOut, _ := captureStreams(t, func() {
		if err := runPluginList(nil); err != nil {
			t.Errorf("列清单失败: %v", err)
		}
	})
	if !strings.Contains(listOut, key) {
		t.Fatalf("清单里没有刚发布的插件 %s：\n%s", key, listOut)
	}
	if !strings.Contains(listOut, pluginstore.StatusPending) {
		t.Errorf("清单里没有状态那一栏：\n%s", listOut)
	}

	// 加载器这时还不该把它装上。
	if hasActive(t, db, key) {
		t.Error("pending 的插件不该参与注册")
	}

	// 回显走 stderr：stdout 只放能被脚本接走的值，而这条命令没有新值要给出去。
	enableOut, enableNote := captureStreams(t, func() {
		if err := runPluginSetEnabled([]string{key}, true); err != nil {
			t.Errorf("启用失败: %v", err)
		}
	})
	if !strings.Contains(enableNote, "已启用") {
		t.Errorf("启用之后的回显里没有「已启用」：%q", enableNote)
	}
	if strings.Contains(enableOut, "已启用") {
		t.Errorf("回显不该混进 stdout：%q", enableOut)
	}
	if !hasActive(t, db, key) {
		t.Error("启用之后该参与注册")
	}

	disableOut, disableNote := captureStreams(t, func() {
		if err := runPluginSetEnabled([]string{key}, false); err != nil {
			t.Errorf("停用失败: %v", err)
		}
	})
	if !strings.Contains(disableNote, "已停用") {
		t.Errorf("停用之后的回显里没有「已停用」：%q", disableNote)
	}
	if strings.Contains(disableOut, "已停用") {
		t.Errorf("回显不该混进 stdout：%q", disableOut)
	}
	if hasActive(t, db, key) {
		t.Error("停用之后不该参与注册")
	}
}

// 同一个 (key, version) 重复发布是覆盖，且输出里说得明白。
//
// 「已发布」与「已覆盖」必须是两句不同的话：前者是新建了一代，后者是改了
// 同一代的内容，而它们在库里都表现为「命令成功了」。
func TestPluginPublishSameVersionSaysOverwritten(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	key := uniqueName("cliover")
	path := writePluginFile(t, "cliover.js", pluginSource(key, "1.0.0", 0))

	first, _ := captureStreams(t, func() {
		if err := runPluginPublish([]string{path}); err != nil {
			t.Errorf("首次发布失败: %v", err)
		}
	})
	if strings.Contains(first, "已覆盖") {
		t.Errorf("第一次发布不该说「已覆盖」：%q", first)
	}

	second, _ := captureStreams(t, func() {
		if err := runPluginPublish([]string{path}); err != nil {
			t.Errorf("重复发布失败: %v", err)
		}
	})
	if !strings.Contains(second, "已覆盖") {
		t.Errorf("同一版本重新发布该说「已覆盖」：%q", second)
	}
	if n := countPlugins(t, db, key); n != 1 {
		t.Errorf("覆盖之后该只有 1 行，实际 %d 行", n)
	}
}

// --key 与源码 meta 里的 key 不一致时拒绝。
//
// 两边都能决定注册名，而它们不一致时没有一处会报错：库里记的是 A、请求里
// 认的是 B，表现为「启用了但没生效」。
func TestPluginPublishRejectsKeyMismatch(t *testing.T) {
	setupEnv(t)

	path := writePluginFile(t, "mismatch.js", pluginSource("source-key", "1.0.0", 0))

	err := runCaptured(t, func() error {
		return runPluginPublish([]string{path, "--key", "flag-key"})
	})
	if err == nil {
		t.Fatal("--key 与 meta 不一致时该被拒绝")
	}
	for _, want := range []string{"flag-key", "source-key"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息 %q 里没有点出 %q：两个名字都要出现才看得出哪里对不上",
				err.Error(), want)
		}
	}
	if got := exitCode(err); got == exitOK {
		t.Errorf("退出码 = %d，期望非零", got)
	}
}

// 启用一个库里没有的 key：按「约束冲突」返回 4，不是 1。
//
// 两者的下一步动作不同：4 是「去看数据里现在是什么」，1 是「去看日志」。
func TestPluginEnableUnknownKeyExitCode(t *testing.T) {
	testDB(t)
	setupEnv(t)

	err := runCaptured(t, func() error {
		return runPluginSetEnabled([]string{uniqueName("nope")}, true)
	})
	if err == nil {
		t.Fatal("启用不存在的 key 该报错")
	}
	if got := exitCode(err); got != exitConflict {
		t.Errorf("退出码 = %d，期望 %d（约束冲突）", got, exitConflict)
	}
}

// 换版本会插入新的一代，而不是覆盖旧的：老版本留在库里，回退时还在。
func TestPluginPublishNewVersionAddsGeneration(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	key := uniqueName("cligen")
	v1 := writePluginFile(t, "v1.js", pluginSource(key, "1.0.0", 0))
	v2 := writePluginFile(t, "v2.js", pluginSource(key, "2.0.0", 0))

	for _, path := range []string{v1, v2} {
		out, _ := captureStreams(t, func() {
			if err := runPluginPublish([]string{path}); err != nil {
				t.Errorf("发布 %s 失败: %v", path, err)
			}
		})
		if strings.Contains(out, "已覆盖") {
			t.Errorf("换版本该是新建一代，不该说「已覆盖」：%q", out)
		}
	}
	if n := countPlugins(t, db, key); n != 2 {
		t.Errorf("两个版本该留下 2 行，实际 %d 行", n)
	}
}

// ── 助手 ──────────────────────────────────────────────────────────────

// pluginSource 造一段能编译过的插件源码。
func pluginSource(key, version string, _ int) string {
	return fmt.Sprintf(`import { definePlugin } from "larkax";
export const meta = { key: %q, version: %q, apiVersion: 1 };
export default definePlugin({ onUsage() { return { promptTokens: 1 }; } });
`, key, version)
}

func writePluginFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写插件文件失败: %v", err)
	}
	return path
}

func countPlugins(t *testing.T, db *sql.DB, key string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM plugins WHERE key = $1`, key).Scan(&n); err != nil {
		t.Fatalf("统计插件行失败: %v", err)
	}
	return n
}

// hasActive 问的是「加载器会不会把它装上」——与 store.Active 同一口径。
func hasActive(t *testing.T, db *sql.DB, key string) bool {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM plugins WHERE key = $1 AND status = $2`,
		key, pluginstore.StatusActive).Scan(&n); err != nil {
		t.Fatalf("统计插件行失败: %v", err)
	}
	return n > 0
}

// ── 机器可读输出（--format json） ───────────────────────────────────────

// 清单里的每一栏都要能读出来，坏掉的那一行尤其要**留在这条自己身上**。
//
// 表格那一档把坏 meta 折成表后的一行说明；脚本问的却是「哪几个插件的 meta 有
// 问题」，所以这一档必须有 meta_problem 这一栏，否则它只能去解析给人看的那句话。
func TestPluginListDocIsMachineReadable(t *testing.T) {
	doc := pluginListDoc([]pluginstore.Row{
		{Key: "good", Version: "1.0.0", Status: "active", Generation: 0, Priority: 3, Name: "好的"},
		{Key: "bad", Version: "2.0.0", Status: "pending", Generation: 1, Name: "坏的",
			MetaProblem: "meta 里的 key \"另一个名字\" 与行上的 key \"bad\" 不一致"},
	})

	var got struct {
		Status  string `json:"status"`
		Summary struct {
			Total int `json:"total"`
		} `json:"summary"`
		Plugins []struct {
			Key         string  `json:"key"`
			Version     string  `json:"version"`
			Status      string  `json:"status"`
			Generation  int64   `json:"generation"`
			Priority    int     `json:"priority"`
			Name        string  `json:"name"`
			MetaProblem *string `json:"meta_problem"`
		} `json:"plugins"`
	}
	decodeDoc(t, doc, &got)

	if got.Status != jsonOK {
		t.Errorf("status = %q，想要 %q", got.Status, jsonOK)
	}
	if got.Summary.Total != 2 || len(got.Plugins) != 2 {
		t.Fatalf("summary.total = %d、plugins 有 %d 条，都想要 2",
			got.Summary.Total, len(got.Plugins))
	}

	first := got.Plugins[0]
	if first.Key != "good" || first.Version != "1.0.0" || first.Status != "active" ||
		first.Generation != 0 || first.Priority != 3 || first.Name != "好的" {
		t.Errorf("第一条对不上：%+v", first)
	}
	// 这一条没有 meta 问题 → null（不是空串：空串读起来像一个值）。
	if first.MetaProblem != nil {
		t.Errorf("meta 正常的插件，meta_problem = %q，想要 null", *first.MetaProblem)
	}

	second := got.Plugins[1]
	if second.Key != "bad" || second.Status != "pending" || second.Generation != 1 {
		t.Errorf("第二条对不上：%+v", second)
	}
	// 坏掉的那一行**照样在清单里**：整次报错或跳过它，都会让最需要看清单的
	// 那一刻看不到东西。
	if second.MetaProblem == nil || !strings.Contains(*second.MetaProblem, "与行上的 key") {
		t.Errorf("坏 meta 的原文没打出来：%s", showPtr(second.MetaProblem))
	}
	// 计数与代次是 JSON 数字，不是字符串。
	var generic map[string]any
	raw := decodeDoc(t, doc, &generic)
	entries, ok := generic["plugins"].([]any)
	if !ok || len(entries) != 2 {
		t.Fatalf("plugins 不是两条的数组：%v", generic["plugins"])
	}
	if _, ok := entries[1].(map[string]any)["generation"].(float64); !ok {
		t.Errorf("generation 应当是 JSON 数字，得到 %#v", entries[1].(map[string]any)["generation"])
	}
	for i, item := range entries {
		entry, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("第 %d 条不是对象：%v", i, item)
		}
		assertDocKeys(t, entry, []string{
			"key", "version", "status", "generation", "priority", "name", "meta_problem",
			"compile_error", "latest", "running_generation", "running_load",
		})
	}
	if strings.Contains(string(raw), "插件 good 的 meta 有问题") {
		t.Errorf("表格那一档的那句话混进了 JSON：\n%s", raw)
	}
}

// 空清单是 `[]`，不是 null，也不是那句「暂无插件。」。
func TestPluginListDocEmptyListIsEmptyArray(t *testing.T) {
	var got struct {
		Plugins *[]map[string]any `json:"plugins"`
	}
	decodeDoc(t, pluginListDoc(nil), &got)

	if got.Plugins == nil {
		t.Fatal("空清单编成了 null，要的是 []")
	}
	if len(*got.Plugins) != 0 {
		t.Errorf("空清单里有 %d 条", len(*got.Plugins))
	}
}

// 写类命令在 json 模式下必须在连库之前被拦下，且命名的正是自己那一条。
//
// enable 与 disable 共用 runPluginSetEnabled：命令名写死一个的话，另一条命令
// 在不支持 json 时会报出兄弟命令的名字——而照着那句话去改的人会改错命令。
// 表里不写条数：这类清单每加一条命令就要跟着长，写死的数字只会变成一句
// 随表变旧的谎。
func TestPluginWriteCommandsRejectJSONFormatBeforeTouchingTheDB(t *testing.T) {
	withFormat(t, formatJSON)
	// 连不上的库：真的去连就会以 exitDBUnreach 收场。
	t.Setenv("LARKAX_DB_DSN", "postgres://larkax:placeholder@127.0.0.1:1/larkax?sslmode=disable")

	cases := []struct {
		name string
		run  func() error
		want string
	}{
		{"publish", func() error {
			return runPluginPublish([]string{"plugin.js"})
		}, "plugin publish"},
		{"enable", func() error { return runPluginSetEnabled([]string{"k"}, true) }, "plugin enable"},
		{"disable", func() error { return runPluginSetEnabled([]string{"k"}, false) }, "plugin disable"},
		{"delete", func() error { return runPluginDelete([]string{"k"}) }, "plugin delete"},
		{"kv set", func() error {
			return runPluginKVSet([]string{"--plugin", "p", "--user", "u", "--key", "k", "--value", "v"})
		}, "plugin kv set"},
		{"kv delete", func() error {
			return runPluginKVDelete([]string{"--plugin", "p", "--user", "u", "--key", "k"})
		}, "plugin kv delete"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			stdout, _ := captureStreams(t, func() { err = tc.run() })
			if err == nil {
				t.Fatal("json 模式下应当把写类命令拦下来")
			}
			if !strings.Contains(err.Error(), "不支持 --format json") {
				t.Fatalf("错误信息 %q 里没有「不支持 --format json」——是不是先连库了", err.Error())
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息里要点名 %q：%v", tc.want, err)
			}
			if code := exitCode(err); code != exitUsage {
				t.Errorf("退出码 = %d，想要 %d（参数错）", code, exitUsage)
			}
			if stdout != "" {
				t.Errorf("被拦下时 stdout 应当为空，得到 %q", stdout)
			}
		})
	}
}

// 命令级：发布一个插件再列出来，stdout 整体是一份合法 JSON。
//
// 用真库是因为这一档的键值几乎全来自库里那几列（代次、优先级、名称），而
// 「优先级取自 platform 作用域那条启用记录」这类口径只有真库能给。
func TestPluginListJSONStdoutIsMachineReadable(t *testing.T) {
	testDB(t)
	setupEnv(t)

	key := uniqueName("jsoncli")
	path := writePluginFile(t, "cli.js", pluginSource(key, "1.0.0", 0))
	if err := runCaptured(t, func() error { return runPluginPublish([]string{path}) }); err != nil {
		t.Fatalf("发布插件失败: %v", err)
	}

	withFormat(t, formatJSON)
	var err error
	stdout, _ := captureStreams(t, func() { err = runPluginList(nil) })
	if err != nil {
		t.Fatalf("plugin list 失败: %v", err)
	}
	assertOnlyJSON(t, stdout)

	var got struct {
		Status   string `json:"status"`
		ExitCode int    `json:"exit_code"`
		Plugins  []struct {
			Key      string  `json:"key"`
			Version  string  `json:"version"`
			Status   string  `json:"status"`
			Name     string  `json:"name"`
			Problem  *string `json:"meta_problem"`
			Priority int     `json:"priority"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("解不回来: %v\n%s", err, stdout)
	}
	if got.Status != jsonOK || got.ExitCode != exitOK {
		t.Errorf("status=%q exit_code=%d，想要 %q / %d",
			got.Status, got.ExitCode, jsonOK, exitOK)
	}

	var found bool
	for _, p := range got.Plugins {
		if p.Key != key {
			continue
		}
		found = true
		if p.Version != "1.0.0" || p.Status != pluginstore.StatusPending {
			t.Errorf("刚发布的那一条对不上：%+v", p)
		}
		if p.Problem != nil {
			t.Errorf("刚发布的插件不该有 meta 问题：%q", *p.Problem)
		}
	}
	if !found {
		t.Fatalf("清单里没有刚发布的 %s：\n%s", key, stdout)
	}
}

// ── plugin get / delete / kv（#694） ────────────────────────────────
//
// 这三组补的是「库里已经有、命令行够不着」的面。判据与同组其余子命令一致：
// 参数错在连库之前报出来、读类给表格与 -o json 两条路、写类的回显只在 stderr、
// 找不到那一行落退出码 4。

// 参数错全部发生在连库之前：不设 LARKAX_DB_DSN 也跑得通。
func TestPluginGetAndDeleteRejectBadArgs(t *testing.T) {
	cases := []struct {
		name string
		fn   func() error
		want string
	}{
		{"get 没有 key", func() error { return runPluginGet(nil) }, "需要一个插件 key"},
		{"get 两个 key", func() error { return runPluginGet([]string{"a", "b"}) }, "需要一个插件 key"},
		{"get 空 key", func() error { return runPluginGet([]string{"  "}) }, "不能为空"},
		{"delete 没有 key", func() error { return runPluginDelete(nil) }, "需要一个插件 key"},
		{"delete 两个 key", func() error { return runPluginDelete([]string{"a", "b"}) }, "需要一个插件 key"},
		{"delete 空 key", func() error { return runPluginDelete([]string{"  "}) }, "不能为空"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runCaptured(t, tc.fn)
			if err == nil {
				t.Fatal("期望报错，却通过了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息 %q 里没有 %q", err.Error(), tc.want)
			}
			if got := exitCode(err); got != exitUsage {
				t.Errorf("退出码 = %d，期望 %d（参数问题）", got, exitUsage)
			}
		})
	}
}

// kv 的三段身份（插件、用户、键）缺一段都不往下走。
//
// 缺 --user 尤其不能放过：那一段决定读的是谁的键，而漏掉它之后命令照样能跑出
// 一份输出——只是那份输出可能不是这个人的。
func TestPluginKVRejectsBadArgs(t *testing.T) {
	cases := []struct {
		name string
		fn   func() error
		want string
	}{
		{"没有子命令", func() error { return runPluginKV(nil) }, "需要子命令"},
		{"未知子命令", func() error { return runPluginKV([]string{"bogus"}) }, "未知的 plugin kv 子命令"},
		{"get 缺 --plugin", func() error {
			return runPluginKVGet([]string{"--user", "alice", "--key", "k"})
		}, "缺少 --plugin"},
		{"get 缺 --user", func() error {
			return runPluginKVGet([]string{"--plugin", "p", "--key", "k"})
		}, "缺少 --user"},
		{"get 缺 --key", func() error {
			return runPluginKVGet([]string{"--plugin", "p", "--user", "alice"})
		}, "缺少 --key"},
		{"get 不收位置参数", func() error {
			return runPluginKVGet([]string{"--plugin", "p", "--user", "alice", "--key", "k", "extra"})
		}, "不接受位置参数"},
		{"set 负 ttl", func() error {
			return runPluginKVSet([]string{
				"--plugin", "p", "--user", "alice", "--key", "k", "--ttl", "-1s"})
		}, "--ttl 不接受负数"},
		{"set 不收位置参数", func() error {
			return runPluginKVSet([]string{"--plugin", "p", "--user", "alice", "--key", "k", "x"})
		}, "不接受位置参数"},
		{"delete 缺 --plugin", func() error {
			return runPluginKVDelete([]string{"--user", "alice", "--key", "k"})
		}, "缺少 --plugin"},
		{"delete 缺 --user", func() error {
			return runPluginKVDelete([]string{"--plugin", "p", "--key", "k"})
		}, "缺少 --user"},
		{"delete 缺 --key", func() error {
			return runPluginKVDelete([]string{"--plugin", "p", "--user", "alice"})
		}, "缺少 --key"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runCaptured(t, tc.fn)
			if err == nil {
				t.Fatal("期望报错，却通过了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息 %q 里没有 %q", err.Error(), tc.want)
			}
			if got := exitCode(err); got != exitUsage {
				t.Errorf("退出码 = %d，期望 %d（参数问题）", got, exitUsage)
			}
		})
	}
}

// 详情页把这一代的事实都打出来，并把「还有更老的版本」与 meta 问题说清楚。
func TestPrintPluginDetail(t *testing.T) {
	at := time.Date(2026, 10, 18, 4, 25, 15, 0, time.UTC)
	seven := int64(7)

	var buf bytes.Buffer
	printPluginDetail(&buf, pluginstore.Detail{
		Row: pluginstore.Row{
			Key: "demo", Version: "2.0.0", Status: pluginstore.StatusActive,
			Generation: 3, Priority: 5, Name: "示例插件",
			MetaProblem: "meta 是空的",
		},
		CreatedAt: at, CreatedBy: &seven, Versions: 2,
	})
	out := buf.String()

	for _, want := range []string{
		"demo", "2.0.0", pluginstore.StatusActive, "3", "5", "示例插件",
		at.Format(time.RFC3339),
		"这个插件的 meta 有问题：meta 是空的",
		// 只打最新的一代，所以必须说出「还有别的」：不说的话，「只有一个
		// 版本」与「有好几代、你看到的是最上面那一个」在这份输出上一样。
		"这个 key 下还有 1 个更老的版本",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("详情里没有 %q：\n%s", want, out)
		}
	}
	if row := lineEndingWith(t, out, "写自"); !strings.HasSuffix(row, "7") {
		t.Errorf("「写自」那一行 = %q，期望以 7 结尾", row)
	}

	// 没有记录时打 `-`，不是 0：0 读起来像「写自用户 0」。
	var nilBuf bytes.Buffer
	printPluginDetail(&nilBuf, pluginstore.Detail{
		Row: pluginstore.Row{
			Key: "demo", Version: "1.0.0", Status: pluginstore.StatusPending, Name: "示例",
		},
		CreatedAt: at,
	})
	if row := lineEndingWith(t, nilBuf.String(), "写自"); !strings.HasSuffix(row, "-") {
		t.Errorf("没有创建者时「写自」那一行 = %q，期望以 - 结尾", row)
	}
	// 只有一代时不打「更老的版本」：那一句是条件成立时才有的附注。
	if strings.Contains(nilBuf.String(), "更老的版本") {
		t.Errorf("只有一代时不该提更老的版本：\n%s", nilBuf.String())
	}
}

// lineEndingWith 找出含 marker 的那一行（报错时这一行就是上下文）。
func lineEndingWith(t *testing.T, out, marker string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, marker) {
			return strings.TrimRight(line, " ")
		}
	}
	t.Fatalf("输出里没有 %q 那一行：\n%s", marker, out)
	return ""
}

// get 的 JSON 是「list 的条目 + 详情才有的三栏」，不是另一套字段名。
func TestPluginGetDocIsMachineReadable(t *testing.T) {
	at := time.Date(2026, 10, 18, 4, 25, 15, 0, time.UTC)
	nine := int64(9)

	doc := pluginGetDoc(pluginstore.Detail{
		Row: pluginstore.Row{
			Key: "demo", Version: "1.2.3", Status: pluginstore.StatusActive,
			Generation: 4, Priority: 2, Name: "示例", MetaProblem: "坏了",
		},
		CreatedAt: at, CreatedBy: &nine, Versions: 3,
	})

	var got struct {
		Status string `json:"status"`
		Plugin struct {
			Key         string  `json:"key"`
			Version     string  `json:"version"`
			Status      string  `json:"status"`
			Generation  int64   `json:"generation"`
			Priority    int     `json:"priority"`
			Name        string  `json:"name"`
			MetaProblem *string `json:"meta_problem"`
			Versions    int64   `json:"versions"`
			CreatedAt   string  `json:"created_at"`
			CreatedBy   *int64  `json:"created_by"`
		} `json:"plugin"`
	}
	decodeDoc(t, doc, &got)

	if got.Status != jsonOK {
		t.Errorf("status = %q，想要 %q", got.Status, jsonOK)
	}
	p := got.Plugin
	if p.Key != "demo" || p.Version != "1.2.3" || p.Status != pluginstore.StatusActive ||
		p.Generation != 4 || p.Priority != 2 || p.Name != "示例" {
		t.Errorf("清单那几栏对不上：%+v", p)
	}
	if p.MetaProblem == nil || *p.MetaProblem != "坏了" {
		t.Errorf("meta_problem = %q，想要「坏了」", showPtr(p.MetaProblem))
	}
	if p.Versions != 3 {
		t.Errorf("versions = %d，想要 3", p.Versions)
	}
	// 时刻恒 UTC、完整精度（见 jsonTime）：同一行在两台机器上必须编出同一个串。
	if p.CreatedAt != "2026-10-18T04:25:15Z" {
		t.Errorf("created_at = %q，想要 UTC 的 RFC3339", p.CreatedAt)
	}
	if p.CreatedBy == nil || *p.CreatedBy != 9 {
		t.Errorf("created_by = %q，想要 9", showPtr(p.CreatedBy))
	}
}

// 没有创建者时 created_by 是 null，不是 0。
func TestPluginGetDocCreatedByNil(t *testing.T) {
	doc := pluginGetDoc(pluginstore.Detail{
		Row:       pluginstore.Row{Key: "demo", Version: "1.0.0", Status: pluginstore.StatusPending},
		CreatedAt: time.Date(2026, 10, 18, 4, 25, 15, 0, time.UTC),
	})

	var got map[string]any
	decodeDoc(t, doc, &got)
	entry, ok := got["plugin"].(map[string]any)
	if !ok {
		t.Fatalf("plugin 不是一个对象：%v", got["plugin"])
	}
	if v, present := entry["created_by"]; !present || v != nil {
		t.Errorf("created_by = %v（在不在：%v），想要 null", v, present)
	}
}

// 卸载的回显把三样一起说清楚：版本、启用记录、私有存储。
//
// 只复述一句「已删除」的话，把 KV 一起删掉这件事在输出上完全看不出来。
func TestPrintPluginDeleteResult(t *testing.T) {
	var buf bytes.Buffer
	printPluginDeleteResult(&buf, pluginstore.Deleted{Key: "demo", Versions: 2, Activations: 1, KVKeys: 5})
	want := "已删除插件 demo：2 个版本、1 条启用记录、5 个私有存储的键。重启后生效。\n"
	if got := buf.String(); got != want {
		t.Errorf("卸载回显 = %q，期望 %q", got, want)
	}
}

// kv get 的 JSON 照抄三段身份，并把值原样交出去。
func TestPluginKVGetDocIsMachineReadable(t *testing.T) {
	doc := pluginKVGetDoc(kvTarget{plugin: "demo", user: "alice", key: "k"}, "raw\tvalue")

	var got struct {
		Status string `json:"status"`
		Entry  struct {
			Plugin string `json:"plugin"`
			User   string `json:"user"`
			Key    string `json:"key"`
			Value  string `json:"value"`
		} `json:"entry"`
	}
	decodeDoc(t, doc, &got)

	if got.Status != jsonOK {
		t.Errorf("status = %q，想要 %q", got.Status, jsonOK)
	}
	if got.Entry.Plugin != "demo" || got.Entry.User != "alice" || got.Entry.Key != "k" {
		t.Errorf("身份那几栏对不上：%+v", got.Entry)
	}
	if got.Entry.Value != "raw\tvalue" {
		t.Errorf("value = %q，期望原样搬过去", got.Entry.Value)
	}
}

// get 看的是最新一代：加载器只认代次最高的那一行。
func TestPluginGetShowsNewestGeneration(t *testing.T) {
	setupEnv(t)

	key := uniqueName("cliget")
	for _, version := range []string{"1.0.0", "2.0.0"} {
		path := writePluginFile(t, "v"+version+".js", pluginSource(key, version, 0))
		if err := runCaptured(t, func() error { return runPluginPublish([]string{path}) }); err != nil {
			t.Fatalf("发布 %s 失败: %v", version, err)
		}
	}

	out, _ := captureStreams(t, func() {
		if err := runPluginGet([]string{key}); err != nil {
			t.Errorf("看详情失败: %v", err)
		}
	})
	for _, want := range []string{key, "2.0.0", "这个 key 下还有 1 个更老的版本"} {
		if !strings.Contains(out, want) {
			t.Errorf("详情里没有 %q：\n%s", want, out)
		}
	}
	// 老版本那一行不该出现：这一页回答的是「现在生效的是哪一个」。
	if strings.Contains(out, "1.0.0") {
		t.Errorf("详情里出现了更老的版本：\n%s", out)
	}

	// JSON 那一档同样只给最新的一代，并且整份能解出来、不含转义码。
	withFormat(t, formatJSON)
	jsonOut, _ := captureStreams(t, func() {
		if err := runPluginGet([]string{key}); err != nil {
			t.Errorf("json 档看详情失败: %v", err)
		}
	})
	assertOnlyJSON(t, jsonOut)
	var doc struct {
		Plugin struct {
			Version  string `json:"version"`
			Versions int64  `json:"versions"`
		} `json:"plugin"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &doc); err != nil {
		t.Fatalf("解不回来: %v\n%s", err, jsonOut)
	}
	if doc.Plugin.Version != "2.0.0" || doc.Plugin.Versions != 2 {
		t.Errorf("version=%q versions=%d，想要 2.0.0 / 2", doc.Plugin.Version, doc.Plugin.Versions)
	}
}

// 看一个库里没有的插件：按「约束冲突」返回 4。
func TestPluginGetUnknownKeyExitsFour(t *testing.T) {
	testDB(t)
	setupEnv(t)

	err := runCaptured(t, func() error { return runPluginGet([]string{uniqueName("nope")}) })
	if err == nil {
		t.Fatal("看不存在的插件该报错")
	}
	if !errors.Is(err, pluginstore.ErrPluginNotFound) {
		t.Errorf("错误链里没有 ErrPluginNotFound：%v", err)
	}
	if got := exitCode(err); got != exitConflict {
		t.Errorf("退出码 = %d，期望 %d（约束冲突）", got, exitConflict)
	}
}

// 卸载把三张表里属于这个插件的行一起删掉，别人的数据不动。
func TestPluginDeleteRemovesPluginActivationsAndKV(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	ctx := context.Background()

	key := uniqueName("clidel")
	path := writePluginFile(t, "del.js", pluginSource(key, "1.0.0", 0))
	if err := runCaptured(t, func() error { return runPluginPublish([]string{path, "--priority", "3"}) }); err != nil {
		t.Fatalf("发布失败: %v", err)
	}
	if err := runCaptured(t, func() error { return runPluginSetEnabled([]string{key}, true) }); err != nil {
		t.Fatalf("启用失败: %v", err)
	}
	userID := createTestUser(t)
	if err := pluginstore.NewKV(db).Set(ctx, key, userID, "state", "1", 0); err != nil {
		t.Fatalf("种一个 KV 键失败: %v", err)
	}

	// 另起一个插件，卸载不该碰到它。
	other := uniqueName("clidel-other")
	otherPath := writePluginFile(t, "other.js", pluginSource(other, "1.0.0", 0))
	if err := runCaptured(t, func() error { return runPluginPublish([]string{otherPath}) }); err != nil {
		t.Fatalf("发布对照组失败: %v", err)
	}
	if err := pluginstore.NewKV(db).Set(ctx, other, userID, "state", "1", 0); err != nil {
		t.Fatalf("种对照组的 KV 键失败: %v", err)
	}

	stdout, stderr := captureStreams(t, func() {
		if err := runPluginDelete([]string{key}); err != nil {
			t.Errorf("卸载失败: %v", err)
		}
	})
	// 回显只走 stderr：stdout 上这条命令没有能被接走的值。
	if strings.Contains(stdout, "已删除插件") {
		t.Errorf("卸载回显混进了 stdout：%q", stdout)
	}
	for _, want := range []string{
		"已删除插件 " + key,
		"1 个版本", "1 条启用记录", "1 个私有存储的键",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("回显里没有 %q：%q", want, stderr)
		}
	}

	if n := countPlugins(t, db, key); n != 0 {
		t.Errorf("plugins 里还剩 %d 行，该是 0 行", n)
	}
	if n := countQueryRows(t, db, `SELECT count(*) FROM plugin_activations WHERE plugin_key = $1`, key); n != 0 {
		t.Errorf("plugin_activations 里还剩 %d 行，该是 0 行", n)
	}
	if n := countQueryRows(t, db, `SELECT count(*) FROM plugin_kv WHERE plugin_key = $1`, key); n != 0 {
		t.Errorf("plugin_kv 里还剩 %d 行，该是 0 行", n)
	}

	// 对照组一行都不能少。
	if n := countPlugins(t, db, other); n != 1 {
		t.Errorf("对照组的插件行 = %d，想要 1", n)
	}
	if n := countQueryRows(t, db, `SELECT count(*) FROM plugin_kv WHERE plugin_key = $1`, other); n != 1 {
		t.Errorf("对照组的 KV 行 = %d，想要 1", n)
	}
}

// 卸载一个不存在的 key：报「插件不存在」，而且**一行都不许删**。
//
// plugin_activations 与 plugin_kv 按 plugin_key 存，与 plugins 之间没有外键。
// 先删 plugins、删不到就收手，是这两张表的行不被一个拼错的 key 扫掉的那道闸。
func TestPluginDeleteUnknownKeyLeavesOtherTablesAlone(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	key := uniqueName("clidel-nope")
	// 只往从属表里塞行，plugins 里没有这个 key。
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO plugin_activations (plugin_key, scope, priority) VALUES ($1, 'platform', 0)`,
		key); err != nil {
		t.Fatalf("种启用记录失败: %v", err)
	}
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.Background(),
			`DELETE FROM plugin_activations WHERE plugin_key = $1`, key); err != nil {
			t.Errorf("清理启用记录失败: %v", err)
		}
	})

	err := runCaptured(t, func() error { return runPluginDelete([]string{key}) })
	if err == nil {
		t.Fatal("卸载不存在的插件该报错")
	}
	if !errors.Is(err, pluginstore.ErrPluginNotFound) {
		t.Errorf("错误链里没有 ErrPluginNotFound：%v", err)
	}
	if got := exitCode(err); got != exitConflict {
		t.Errorf("退出码 = %d，期望 %d", got, exitConflict)
	}
	if n := countQueryRows(t, db, `SELECT count(*) FROM plugin_activations WHERE plugin_key = $1`, key); n != 1 {
		t.Errorf("启用记录被删掉了 %d 行，它不该动", 1-n)
	}
}

// set → get → delete 走一遍；删完之后 get 与再删都是「没有这个键」（退出码 4）。
func TestPluginKVSetGetDelete(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	key := uniqueName("clikv")
	path := writePluginFile(t, "kv.js", pluginSource(key, "1.0.0", 0))
	if err := runCaptured(t, func() error { return runPluginPublish([]string{path}) }); err != nil {
		t.Fatalf("发布失败: %v", err)
	}
	user := uniqueName("kvuser")
	if err := runCaptured(t, func() error { return runUserCreate([]string{"--name", user}) }); err != nil {
		t.Fatalf("建用户失败: %v", err)
	}

	setOut, setNote := captureStreams(t, func() {
		if err := runPluginKVSet([]string{
			"--plugin", key, "--user", user, "--key", "greeting", "--value", "你好",
		}); err != nil {
			t.Errorf("写键失败: %v", err)
		}
	})
	if setOut != "" {
		t.Errorf("写键的 stdout 该是空的，实际 %q", setOut)
	}
	// 「你好」是 6 个字节、2 个字：报的是字节数，插件配额按字节算。
	for _, want := range []string{"已写入插件 " + key, "greeting", user, "不过期", "6 字节"} {
		if !strings.Contains(setNote, want) {
			t.Errorf("写键回显里没有 %q：%q", want, setNote)
		}
	}

	getOut, _ := captureStreams(t, func() {
		if err := runPluginKVGet([]string{
			"--plugin", key, "--user", user, "--key", "greeting",
		}); err != nil {
			t.Errorf("读键失败: %v", err)
		}
	})
	if !strings.Contains(getOut, "你好") {
		t.Errorf("读回的值里没有「你好」：%q", getOut)
	}

	delOut, delNote := captureStreams(t, func() {
		if err := runPluginKVDelete([]string{
			"--plugin", key, "--user", user, "--key", "greeting",
		}); err != nil {
			t.Errorf("删键失败: %v", err)
		}
	})
	if delOut != "" {
		t.Errorf("删键的 stdout 该是空的，实际 %q", delOut)
	}
	// 开头是「已删除 <key> 的键」而不是「已删除插件 <key>」：那后一种开头
	// 是卸载那条命令的，两条回显分不开就会让人以为插件被删了。
	if !strings.Contains(delNote, "已删除 "+key+" 的键 greeting") {
		t.Errorf("删键回显对不上：%q", delNote)
	}
	if n := countQueryRows(t, db, `SELECT count(*) FROM plugin_kv WHERE plugin_key = $1`, key); n != 0 {
		t.Errorf("删完之后还剩 %d 个键", n)
	}

	for _, tc := range []struct {
		name string
		fn   func() error
	}{
		{"删完再读", func() error {
			return runPluginKVGet([]string{"--plugin", key, "--user", user, "--key", "greeting"})
		}},
		{"删完再删", func() error {
			return runPluginKVDelete([]string{"--plugin", key, "--user", user, "--key", "greeting"})
		}},
	} {
		err := runCaptured(t, tc.fn)
		if err == nil {
			t.Fatalf("%s：期望报错，却通过了", tc.name)
		}
		if !errors.Is(err, errPluginKVKeyNotFound) {
			t.Errorf("%s：错误链里没有 errPluginKVKeyNotFound：%v", tc.name, err)
		}
		if got := exitCode(err); got != exitConflict {
			t.Errorf("%s：退出码 = %d，期望 %d", tc.name, got, exitConflict)
		}
	}
}

// 过期的键按不存在处置；而删除**不动**它（清理在写入那一侧）。
func TestPluginKVExpiredKeyLooksAbsent(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	ctx := context.Background()

	key := uniqueName("clikv-exp")
	path := writePluginFile(t, "kvexp.js", pluginSource(key, "1.0.0", 0))
	if err := runCaptured(t, func() error { return runPluginPublish([]string{path}) }); err != nil {
		t.Fatalf("发布失败: %v", err)
	}
	userID := createTestUser(t)
	user := strconv.FormatInt(userID, 10)

	if _, err := db.ExecContext(ctx, `
		INSERT INTO plugin_kv (plugin_key, user_id, key, value, expires_at, updated_at)
		VALUES ($1, $2, 'stale', 'v', now() - interval '1 hour', now())`,
		key, userID); err != nil {
		t.Fatalf("种一个过期键失败: %v", err)
	}
	t.Cleanup(func() {
		if _, err := db.ExecContext(ctx,
			`DELETE FROM plugin_kv WHERE plugin_key = $1`, key); err != nil {
			t.Errorf("清理过期键失败: %v", err)
		}
	})

	for _, tc := range []struct {
		name string
		fn   func() error
	}{
		{"读", func() error {
			return runPluginKVGet([]string{"--plugin", key, "--user", user, "--key", "stale"})
		}},
		{"删", func() error {
			return runPluginKVDelete([]string{"--plugin", key, "--user", user, "--key", "stale"})
		}},
	} {
		err := runCaptured(t, tc.fn)
		if err == nil {
			t.Fatalf("%s过期的键：期望报错，却通过了", tc.name)
		}
		if got := exitCode(err); got != exitConflict {
			t.Errorf("%s过期的键：退出码 = %d，期望 %d", tc.name, got, exitConflict)
		}
	}

	// 过期的行仍躺在表里：删除只清「可见」的那些，收拾过期行是 set 那一步的事。
	if n := countQueryRows(t, db, `SELECT count(*) FROM plugin_kv WHERE plugin_key = $1`, key); n != 1 {
		t.Errorf("过期的行被删掉了（剩 %d 行），它该留着等 set 清", n)
	}
}

// 插件或用户写错时，KV 一行都不落。
//
// plugin_kv 没有外键指着 plugins：不拦这一下的话，一个拼错的 --plugin 会留下
// 一行谁也读不到、谁也清不掉的数据。
func TestPluginKVRejectsUnknownPluginAndUser(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	key := uniqueName("clikv-guard")
	path := writePluginFile(t, "kvguard.js", pluginSource(key, "1.0.0", 0))
	if err := runCaptured(t, func() error { return runPluginPublish([]string{path}) }); err != nil {
		t.Fatalf("发布失败: %v", err)
	}
	user := uniqueName("kvguard-user")
	if err := runCaptured(t, func() error { return runUserCreate([]string{"--name", user}) }); err != nil {
		t.Fatalf("建用户失败: %v", err)
	}

	unknownPlugin := uniqueName("clikv-noplugin")
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.Background(),
			`DELETE FROM plugin_kv WHERE plugin_key = ANY($1)`,
			[]string{unknownPlugin, key}); err != nil {
			t.Errorf("清理 KV 失败: %v", err)
		}
	})

	cases := []struct {
		name string
		args []string
	}{
		{"插件不存在", []string{
			"--plugin", unknownPlugin, "--user", user, "--key", "k", "--value", "v",
		}},
		{"用户不存在", []string{
			"--plugin", key, "--user", uniqueName("kvguard-nouser"), "--key", "k", "--value", "v",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runCaptured(t, func() error { return runPluginKVSet(tc.args) })
			if err == nil {
				t.Fatal("期望报错，却通过了")
			}
			if got := exitCode(err); got != exitConflict {
				t.Errorf("退出码 = %d，期望 %d（要查的那一行不存在）", got, exitConflict)
			}
		})
	}

	for _, k := range []string{unknownPlugin, key} {
		if n := countQueryRows(t, db, `SELECT count(*) FROM plugin_kv WHERE plugin_key = $1`, k); n != 0 {
			t.Errorf("%s 名下落了 %d 个键，该是 0", k, n)
		}
	}
}

// 改库的那三条要主密钥，只读那三条不要。
//
// 「要」这一档的判据是命令自己报出来：没有 LARKAX_MASTER_KEYS 时它在连库之前
// 就停下。只读那三条在同样的环境里照常跑——排查时手上往往只有库口令。
func TestPluginWriteVerbsRequireMasterKey(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	key := uniqueName("clikv-keys")
	path := writePluginFile(t, "keys.js", pluginSource(key, "1.0.0", 0))
	if err := runCaptured(t, func() error { return runPluginPublish([]string{path}) }); err != nil {
		t.Fatalf("发布失败: %v", err)
	}
	userID := createTestUser(t)
	user := strconv.FormatInt(userID, 10)

	t.Setenv("LARKAX_MASTER_KEYS", "")

	writes := []struct {
		name string
		fn   func() error
	}{
		{"delete", func() error { return runPluginDelete([]string{key}) }},
		{"kv set", func() error {
			return runPluginKVSet([]string{"--plugin", key, "--user", user, "--key", "k", "--value", "v"})
		}},
		{"kv delete", func() error {
			return runPluginKVDelete([]string{"--plugin", key, "--user", user, "--key", "k"})
		}},
	}
	for _, tc := range writes {
		t.Run(tc.name+" 没有主密钥", func(t *testing.T) {
			err := runCaptured(t, tc.fn)
			if err == nil {
				t.Fatal("没有主密钥时该被拒绝")
			}
			if !strings.Contains(err.Error(), "LARKAX_MASTER_KEYS") {
				t.Errorf("错误信息 %q 里没有点出 LARKAX_MASTER_KEYS", err.Error())
			}
		})
	}
	if n := countPlugins(t, db, key); n != 1 {
		t.Errorf("被拒绝的卸载动了库：plugins 剩 %d 行", n)
	}

	// 只读那三条照样能跑。
	reads := []struct {
		name string
		fn   func() error
	}{
		{"get", func() error { return runPluginGet([]string{key}) }},
		{"list", func() error { return runPluginList(nil) }},
		{"kv get（键不在也照样走到了库）", func() error {
			return runPluginKVGet([]string{"--plugin", key, "--user", user, "--key", "absent"})
		}},
	}
	for _, tc := range reads {
		t.Run(tc.name+" 没有主密钥", func(t *testing.T) {
			err := runCaptured(t, tc.fn)
			if err != nil && strings.Contains(err.Error(), "LARKAX_MASTER_KEYS") {
				t.Errorf("%s 不该要主密钥：%v", tc.name, err)
			}
		})
	}
}

// countQueryRows 跑一条带实参的 count 查询。
func countQueryRows(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("统计行数失败: %v", err)
	}
	return n
}

// 值排在表外、原样打。
//
// 它是插件自己写进去的任意文本，换行与制表符都可能带（写 JSON 时最常见）。
// 当成表格最后一栏的话，带换行的值会顶出一行、带制表符的值会多出一栏——而
// 「多出来那一栏」在终端上看着就像另一个字段。
func TestPrintPluginKVEntryKeepsValueOutsideTheTable(t *testing.T) {
	value := "first\nsecond\tthird"

	var buf bytes.Buffer
	printPluginKVEntry(&buf, kvTarget{plugin: "demo", user: "alice", key: "k"}, value)
	out := buf.String()

	if !strings.Contains(out, "值：first\nsecond\tthird\n") {
		t.Errorf("值该原样排在表外一行：\n%q", out)
	}
	// 值自己那几行不许沾上表里的栏名：沾上就说明它又被当成一栏了，
	// 而带制表符的那一段会把那一行多顶出一栏。
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "first") && !strings.Contains(line, "second") {
			continue
		}
		for _, label := range []string{"插件", "用户", "键"} {
			if strings.Contains(line, label) {
				t.Errorf("值那一行沾上了表里的栏名 %q：%q", label, line)
			}
		}
	}
	// 三栏身份仍在表里。
	for _, want := range []string{"插件", "用户", "键"} {
		if !strings.Contains(out, want) {
			t.Errorf("表里少了 %q 那一栏：\n%s", want, out)
		}
	}
}

// 卸载一个多版本的 key：回显里的三个数都是可核对的，所以按字面量钉住。
//
// 只走「一个版本」的用例钉不住这件事：版本数写成 RowsAffected、启用记录数与
// KV 键数写成常量 1，在只有一个版本的库上照样全对。
func TestPluginDeleteCountsEveryVersionAndDependentRow(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	ctx := context.Background()

	key := uniqueName("clidel-multi")
	for _, version := range []string{"1.0.0", "2.0.0"} {
		path := writePluginFile(t, "v"+version+".js", pluginSource(key, version, 0))
		if err := runCaptured(t, func() error { return runPluginPublish([]string{path}) }); err != nil {
			t.Fatalf("发布 %s 失败: %v", version, err)
		}
	}
	if err := runCaptured(t, func() error { return runPluginSetEnabled([]string{key}, true) }); err != nil {
		t.Fatalf("启用失败: %v", err)
	}
	userID := createTestUser(t)
	if err := pluginstore.NewKV(db).Set(ctx, key, userID, "state", "1", 0); err != nil {
		t.Fatalf("种 KV 键失败: %v", err)
	}

	// 先确认夹具确实是「两代、一条启用记录、一个键」——否则下面的回显断言
	// 只是把夹具的数字抄了一遍。
	if n := countPlugins(t, db, key); n != 2 {
		t.Fatalf("夹具里 plugins 有 %d 行，想要两代", n)
	}
	if n := countQueryRows(t, db, `SELECT count(*) FROM plugin_activations WHERE plugin_key = $1`, key); n != 1 {
		t.Fatalf("夹具里启用记录有 %d 行，想要 1", n)
	}
	if n := countQueryRows(t, db, `SELECT count(*) FROM plugin_kv WHERE plugin_key = $1`, key); n != 1 {
		t.Fatalf("夹具里 KV 有 %d 个键，想要 1", n)
	}

	_, stderr := captureStreams(t, func() {
		if err := runPluginDelete([]string{key}); err != nil {
			t.Errorf("卸载失败: %v", err)
		}
	})
	want := fmt.Sprintf("已删除插件 %s：%d 个版本、%d 条启用记录、%d 个私有存储的键。重启后生效。\n",
		key, 2, 1, 1)
	if !strings.Contains(stderr, want) {
		t.Errorf("回显里没有这一个数都没错的整句：\n得到 %q\n想要含 %q", stderr, want)
	}
}

// KV 按用户隔离：同一个键名，换个用户就是另一个键。
//
// 隔离在存储层由主键保证，但命令行这一层要自己把 --user 解析对——解析错成
// 别人的 ID 时，读出来的是别人的值、删掉的是别人的键，而两条命令都会成功。
func TestPluginKVIsolatesUsers(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	ctx := context.Background()

	key := uniqueName("clikv-iso")
	path := writePluginFile(t, "iso.js", pluginSource(key, "1.0.0", 0))
	if err := runCaptured(t, func() error { return runPluginPublish([]string{path}) }); err != nil {
		t.Fatalf("发布失败: %v", err)
	}
	alice := createTestUser(t)
	bob := createTestUser(t)
	if err := pluginstore.NewKV(db).Set(ctx, key, alice, "shared-name", "给 alice 的值", 0); err != nil {
		t.Fatalf("种 alice 的键失败: %v", err)
	}

	// bob 读同一个键名：读不到。
	err := runCaptured(t, func() error {
		return runPluginKVGet([]string{
			"--plugin", key, "--user", strconv.FormatInt(bob, 10), "--key", "shared-name",
		})
	})
	if err == nil {
		t.Fatal("bob 名下没有这个键，不该读到")
	}
	if !errors.Is(err, errPluginKVKeyNotFound) {
		t.Errorf("错误链里没有 errPluginKVKeyNotFound：%v", err)
	}
	if got := exitCode(err); got != exitConflict {
		t.Errorf("退出码 = %d，期望 %d", got, exitConflict)
	}

	// bob 删同一个键名：删不掉，而 alice 那一行还在。
	err = runCaptured(t, func() error {
		return runPluginKVDelete([]string{
			"--plugin", key, "--user", strconv.FormatInt(bob, 10), "--key", "shared-name",
		})
	})
	if err == nil {
		t.Fatal("bob 名下没有这个键，不该删得掉")
	}
	if got := exitCode(err); got != exitConflict {
		t.Errorf("删除的退出码 = %d，期望 %d", got, exitConflict)
	}
	if n := countQueryRows(t, db,
		`SELECT count(*) FROM plugin_kv WHERE plugin_key = $1 AND user_id = $2`, key, alice); n != 1 {
		t.Errorf("alice 的键被 bob 删掉了（剩 %d 行）", n)
	}

	// alice 自己读得到——证明上面那两条不是因为键名写错了。
	out, _ := captureStreams(t, func() {
		if err := runPluginKVGet([]string{
			"--plugin", key, "--user", strconv.FormatInt(alice, 10), "--key", "shared-name",
		}); err != nil {
			t.Errorf("alice 读自己的键失败: %v", err)
		}
	})
	if !strings.Contains(out, "给 alice 的值") {
		t.Errorf("alice 没读回自己的值：\n%s", out)
	}

	if _, err := db.ExecContext(ctx, `DELETE FROM plugin_kv WHERE plugin_key = $1`, key); err != nil {
		t.Errorf("清理 KV 失败: %v", err)
	}
}

// 带换行与制表符的值走一遍真库：写进去、读回来，形状不变。
//
// 上面那条只钉渲染，这条钉「库里那一列没被改写」——值原样进出，命令不解析它。
func TestPluginKVValueWithNewlineRoundTrips(t *testing.T) {
	testDB(t)
	setupEnv(t)

	key := uniqueName("clikv-multiline")
	path := writePluginFile(t, "multi.js", pluginSource(key, "1.0.0", 0))
	if err := runCaptured(t, func() error { return runPluginPublish([]string{path}) }); err != nil {
		t.Fatalf("发布失败: %v", err)
	}
	user := strconv.FormatInt(createTestUser(t), 10)
	value := "first\nsecond\tthird"

	if err := runCaptured(t, func() error {
		return runPluginKVSet([]string{
			"--plugin", key, "--user", user, "--key", "multi", "--value", value,
		})
	}); err != nil {
		t.Fatalf("写多行值失败: %v", err)
	}

	out, _ := captureStreams(t, func() {
		if err := runPluginKVGet([]string{
			"--plugin", key, "--user", user, "--key", "multi",
		}); err != nil {
			t.Errorf("读多行值失败: %v", err)
		}
	})
	if !strings.Contains(out, "值：first\nsecond\tthird\n") {
		t.Errorf("多行的值没有原样读回来：\n%q", out)
	}
}
