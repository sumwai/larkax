package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

	enableOut, _ := captureStreams(t, func() {
		if err := runPluginSetEnabled([]string{key}, true); err != nil {
			t.Errorf("启用失败: %v", err)
		}
	})
	if !strings.Contains(enableOut, "已启用") {
		t.Errorf("启用之后的输出里没有「已启用」：%q", enableOut)
	}
	if !hasActive(t, db, key) {
		t.Error("启用之后该参与注册")
	}

	disableOut, _ := captureStreams(t, func() {
		if err := runPluginSetEnabled([]string{key}, false); err != nil {
			t.Errorf("停用失败: %v", err)
		}
	})
	if !strings.Contains(disableOut, "已停用") {
		t.Errorf("停用之后的输出里没有「已停用」：%q", disableOut)
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
