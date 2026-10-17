package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Calcium-Ion/moejs"
)

// 本文件钉编译、链接、元数据，以及插件与宿主之间的数据类型边界。
// 每条测试的注释说明它在钉什么，改动实现时先看注释判断这条还成不成立。

// ---------- 编译与链接 ----------

// 语法错误必须在编译期被拒，而且错误里要带文件与行列——
// 插件作者拿到「编译失败」而不知道哪一行时，只能靠猜。
func Test语法错误报出文件与行列(t *testing.T) {
	_, err := Compile("bad.js", "export default {\n  onRequest(x { return 1; }\n};\n")
	if err == nil {
		t.Fatal("语法错误的源码应该编译失败")
	}
	var se *moejs.SyntaxError
	if !errors.As(err, &se) {
		t.Fatalf("错误里应能找到 *moejs.SyntaxError，实际 %T: %v", err, err)
	}
	if se.File != "bad.js" {
		t.Errorf("文件名 = %q，想要 bad.js", se.File)
	}
	if se.Line != 2 {
		t.Errorf("行号 = %d，想要 2（'x {' 那一行）", se.Line)
	}
	if se.Column <= 0 {
		t.Errorf("列号 = %d，应当是 1 起的正数", se.Column)
	}
}

// 插件只能从虚拟 SDK "larkax" 导入。`node:fs` 这种说明符必须在**链接期**就被
// resolver 拒掉，而不是等运行时拿到一个 undefined。
// 这条钉住「关掉文件系统与网络」是在编译期完成的。
func Test只放行虚拟SDK的导入(t *testing.T) {
	_, err := Compile("imp.js", "import fs from \"node:fs\";\nexport default { onRequest() {} };\n")
	if err == nil {
		t.Fatal("从 node:fs 导入应当失败")
	}
	if !strings.Contains(err.Error(), "node:fs") {
		t.Errorf("错误信息里应提到被拒的说明符 node:fs，实际: %v", err)
	}
	var re *moejs.ResolveError
	if !errors.As(err, &re) {
		t.Fatalf("应能取到 *moejs.ResolveError（说明失败发生在解析导入的环节），实际 %T", err)
	}
	if re.Specifier != "node:fs" {
		t.Errorf("被拒的说明符 = %q，想要 node:fs", re.Specifier)
	}
	if re.File != "imp.js" {
		t.Errorf("报错位置的文件 = %q，想要 imp.js", re.File)
	}
	if re.Line != 1 {
		t.Errorf("报错位置的行 = %d，想要 1", re.Line)
	}
}

// 虚拟 SDK 本身必须能导入，否则插件写什么都编译不过。
func Test导入虚拟SDK成功(t *testing.T) {
	src := "import { definePlugin } from \"larkax\";\nexport default definePlugin({ onRequest() {} });\n"
	m, err := Compile("ok.js", src)
	if err != nil {
		t.Fatalf("从 larkax 导入应当成功，实际: %v", err)
	}
	if !m.Has("onRequest") {
		t.Errorf("Hooks() = %v，应含 onRequest", m.Hooks())
	}
}

// SDK 的导出就是插件的**契约面**：能力清单变了，这里会先动。
// 多出名字意味着插件多了一条宿主没想到的路。
func Test虚拟SDK的导出面(t *testing.T) {
	sm, err := sdk()
	if err != nil {
		t.Fatalf("虚拟 SDK 应当能编译: %v", err)
	}
	got := sm.Exports()
	want := []string{"calendar", "definePlugin", "kv", "log"}
	if len(got) != len(want) {
		t.Fatalf("SDK 导出 = %v，想要 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("SDK 导出 = %v，想要 %v", got, want)
		}
	}
}

// 没有 export default 时要报出专门的那一种错误，而不是「找不到 onRequest」——
// 后者会把插件作者引到错误的方向上去。
func Test没有默认导出报ErrNoDefaultExport(t *testing.T) {
	_, err := Compile("p", "export const meta = { name: \"x\" };\n")
	if err == nil {
		t.Fatal("没有 export default 应当编译失败")
	}
	if !errors.Is(err, ErrNoDefaultExport) {
		t.Errorf("errors.Is(err, ErrNoDefaultExport) = false，实际: %v", err)
	}
}

// 有 export default 但一个 hook 都没导出，同样要失败：
// 这种插件会被注册、每次请求跑一遍、然后什么都不做，没人看得出来。
func Test有默认导出但没有hook时报错(t *testing.T) {
	_, err := Compile("p", "export default { wat() {} };\n")
	if err == nil {
		t.Fatal("一个 hook 都没有应当编译失败")
	}
	if !strings.Contains(err.Error(), "没有导出任何 hook") {
		t.Errorf("错误信息应说明是没有 hook，实际: %v", err)
	}
}

// export default 存在但不是对象（数字、null、函数）时，
// 它的成员一个都不是函数，等价于「没有 hook」。
// 这条钉住这类插件不会被误判成「合法但什么都不做」。
func Test默认导出不是对象时也按没有hook处理(t *testing.T) {
	for _, src := range []string{
		"export default 42;",
		"export default null;",
		"export default () => {};",
	} {
		_, err := Compile("p", src)
		if err == nil {
			t.Errorf("%q 应当编译失败", src)
			continue
		}
		if errors.Is(err, ErrNoDefaultExport) {
			t.Errorf("%q 有 default 导出，不该报 ErrNoDefaultExport，实际: %v", src, err)
		}
	}
}

// hook 名来自真实的求值（rt.Has），不是「成员键存在」。
// 注意 moejs 的 Hook() 对任何成员名都解析成功，所以编译期就用它做判据会出错。
func TestHook名的判定来自实际求值(t *testing.T) {
	m, err := Compile("p", "export default { onUsage() {}, onRequest: 42 };\n")
	if err != nil {
		t.Fatal(err)
	}
	// onRequest 是 42，不是函数，不算数。
	if m.Has("onRequest") {
		t.Errorf("onRequest 不是函数，Has 应为 false，Hooks() = %v", m.Hooks())
	}
	if !m.Has("onUsage") {
		t.Errorf("Hooks() = %v，应含 onUsage", m.Hooks())
	}
	if got := m.Hooks(); len(got) != 1 || got[0] != "onUsage" {
		t.Errorf("Hooks() = %v，想要 [onUsage]", got)
	}
}

// Hooks() 与 Has() 必须一致，且返回的切片是副本——
// 调用方改它不该改到 Module 自己的状态。
func TestHooks与Has一致且返回副本(t *testing.T) {
	m, err := Compile("p", "export default { onUsage() {}, onStream() {} };\n")
	if err != nil {
		t.Fatal(err)
	}
	h := m.Hooks()
	if len(h) != 2 || h[0] != "onStream" || h[1] != "onUsage" {
		t.Fatalf("Hooks() = %v，想要 [onStream onUsage]（按字母序）", h)
	}
	for _, name := range HookNames {
		if got, want := m.Has(name), (name == "onStream" || name == "onUsage"); got != want {
			t.Errorf("Has(%s) = %v，想要 %v", name, got, want)
		}
	}
	h[0] = "改掉了"
	if m.Has("改掉了") {
		t.Error("改 Hooks() 的返回值影响到了 Module：返回的应当是副本")
	}
	if !m.Has("onStream") {
		t.Error("改 Hooks() 的返回值影响到了 Module 的 hook 表")
	}
}

// ---------- 调用与数据类型 ----------

// 插件只返回部分字段时，没返回的字段必须保持零值，指针字段保持 nil——
// 这是「没给」与「给了零」的唯一区分方式。
// 上游靠它决定「要不要动这次请求的参数」，把没给当成给了零会静默改掉默认值。
func Test部分字段保持零值且指针为nil(t *testing.T) {
	m, err := Compile("p", "export default { onRequest() { return { temperature: 0.5, max_tokens: 0 }; } };\n")
	if err != nil {
		t.Fatal(err)
	}
	rt, err := m.NewRuntime(context.Background(), Host{}, nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	var out struct {
		Temperature *float64 `json:"temperature"`
		MaxTokens   *int64   `json:"max_tokens"`
		TopP        *float64 `json:"top_p"`
		Stop        []string `json:"stop"`
	}
	f := rt.Call(context.Background(), "onRequest", map[string]any{"a": 1}, &out)
	if f != nil {
		t.Fatalf("调用不该失败: %v", f)
	}
	if out.Temperature == nil || *out.Temperature != 0.5 {
		t.Errorf("temperature = %s，想要指针指向 0.5", showPtr(out.Temperature))
	}
	// 给了 0：指针非 nil，指向 0。
	if out.MaxTokens == nil || *out.MaxTokens != 0 {
		t.Errorf("max_tokens = %s，想要指针指向 0（给了零 ≠ 没给）", showPtr(out.MaxTokens))
	}
	// 没给：指针 nil、切片 nil。
	if out.TopP != nil {
		t.Errorf("top_p = %v，没返回的字段应当是 nil", *out.TopP)
	}
	if out.Stop != nil {
		t.Errorf("stop = %v，没返回的字段应当是 nil", out.Stop)
	}
}

// 显式返回 null 与「没返回」都落到指针 nil——这是同一件事：
// 两者在 JSON 语义上都是「这里没有值」。
func Test返回null与没返回都得到nil指针(t *testing.T) {
	m, err := Compile("p", "export default { onRequest() { return { temperature: null }; } };\n")
	if err != nil {
		t.Fatal(err)
	}
	rt, err := m.NewRuntime(context.Background(), Host{}, nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	out := struct {
		Temperature *float64 `json:"temperature"`
		Absent      *float64 `json:"absent"`
	}{}
	if f := rt.Call(context.Background(), "onRequest", map[string]any{"a": 1}, &out); f != nil {
		t.Fatalf("调用不该失败: %v", f)
	}
	if out.Temperature != nil {
		t.Errorf("显式返回的 null 应当是 nil 指针，实际 %v", *out.Temperature)
	}
	if out.Absent != nil {
		t.Errorf("没返回的字段应当是 nil 指针，实际 %v", *out.Absent)
	}
}

// 插件什么都不返回（undefined / null / 空 return）是合法的：
// 大多数 hook 只在自己的条件下才给出补丁。
// 这条同时钉住「out 完全不被改动」——半截写入比不写更危险。
func Test什么都不返回时不改动出参(t *testing.T) {
	m, err := Compile("p", `
export default {
  onRequest() { return; },
  onRoute()   { return null; },
  onStream()  { return undefined; },
};
`)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := m.NewRuntime(context.Background(), Host{}, nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	for _, hook := range []string{"onRequest", "onRoute", "onStream"} {
		out := struct {
			A int      `json:"a"`
			B string   `json:"b"`
			C *int     `json:"c"`
			D []int    `json:"d"`
			E []string `json:"e"`
		}{A: 7, B: "保留", D: []int{1, 2}}
		f := rt.Call(context.Background(), hook, map[string]any{"a": 1}, &out)
		if f != nil {
			t.Fatalf("%s: 什么都不返回不该是失败: %v", hook, f)
		}
		if out.A != 7 || out.B != "保留" || out.C != nil || len(out.D) != 2 || out.E != nil {
			t.Errorf("%s: 出参被改动了: %+v", hook, out)
		}
	}
}

// 插件返回的值解不进出参结构体时是**契约违规**，与「插件抛异常」不是一回事：
// 前者说明插件理解错了返回值形状，上层的处置不同。
func Test返回值类型不符是契约违规(t *testing.T) {
	m, err := Compile("p", "export default { onRequest() { return { max_tokens: \"不是数字\" }; } };\n")
	if err != nil {
		t.Fatal(err)
	}
	rt, err := m.NewRuntime(context.Background(), Host{}, nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	var out struct {
		MaxTokens *int64 `json:"max_tokens"`
	}
	f := rt.Call(context.Background(), "onRequest", map[string]any{"a": 1}, &out)
	if f == nil {
		t.Fatal("类型不符应当返回 *Failure")
	}
	if !errors.Is(f, ErrBadResult) {
		t.Errorf("errors.Is(f, ErrBadResult) = false，实际 kind = %v", f.Kind)
	}
	if errors.Is(f, ErrPluginThrew) {
		t.Errorf("契约违规不该被当成插件抛异常")
	}
	if errors.Is(f, ErrTooLarge) {
		t.Errorf("契约违规不该被当成超出字节上限")
	}
	if f.Plugin != "p" || f.Hook != "onRequest" {
		t.Errorf("失败里应带上插件与 hook，实际 Plugin=%q Hook=%q", f.Plugin, f.Hook)
	}
}

// 插件抛异常与返回坏值是两种失败，靠 errors.Is 分得开。
func Test插件抛异常是另一种失败(t *testing.T) {
	m, err := Compile("p", "export default { onRequest() { throw new TypeError(\"炸了\"); } };\n")
	if err != nil {
		t.Fatal(err)
	}
	rt, err := m.NewRuntime(context.Background(), Host{}, nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	f := rt.Call(context.Background(), "onRequest", map[string]any{"a": 1}, nil)
	if f == nil {
		t.Fatal("抛异常应当返回 *Failure")
	}
	if !errors.Is(f, ErrPluginThrew) {
		t.Errorf("errors.Is(f, ErrPluginThrew) = false，实际 kind = %v", f.Kind)
	}
	if errors.Is(f, ErrBadResult) {
		t.Errorf("抛异常不该被当成契约违规")
	}
	var exc *moejs.Exception
	if !errors.As(f.Cause, &exc) {
		t.Fatalf("Cause 里应能取到 *moejs.Exception，实际 %T", f.Cause)
	}
	if exc.Name() != "TypeError" {
		t.Errorf("异常名 = %q，想要 TypeError", exc.Name())
	}
}

// 没导出的 hook 与不存在的名字都返回 nil：这是「插件不关心这个环节」，
// 不是失败。上层因此可以无脑地对五个 hook 各调一次。
func Test没导出的hook返回nil而不是失败(t *testing.T) {
	m, err := Compile("p", "export default { onUsage() { return {}; } };\n")
	if err != nil {
		t.Fatal(err)
	}
	if m.Has("onRequest") {
		t.Fatalf("这个插件没导出 onRequest，Hooks() = %v", m.Hooks())
	}
	rt, err := m.NewRuntime(context.Background(), Host{}, nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	for _, hook := range []string{"onRequest", "onRoute", "onStream", "onResponse", "名字根本不存在"} {
		if f := rt.Call(context.Background(), hook, map[string]any{"a": 1}, nil); f != nil {
			t.Errorf("Call(%s) = %v，没导出的 hook 应当返回 nil", hook, f)
		}
	}
	if rt.Unusable() {
		t.Error("问一个没导出的 hook 不该把 runtime 弄成不可用")
	}
	// 问过之后，导出的那个仍然能跑。
	var out map[string]any
	if f := rt.Call(context.Background(), "onUsage", map[string]any{"a": 1}, &out); f != nil {
		t.Errorf("onUsage 仍应可用: %v", f)
	}
}

// ---------- 沙箱边界 ----------

// 默认全局里不能有网络、文件、进程、定时器、终端。
// 这条是硬约束：多一个名字就多一条插件能绕开宿主的路。
// 「有 Date」是刻意的（日历类插件要用它），所以不在这里断言它不存在。
func Test默认全局没有危险的名字(t *testing.T) {
	m, err := Compile("p", `
export default {
  onRequest() {
    const 禁止的 = ["fetch", "require", "process", "setTimeout", "setInterval", "setImmediate",
      "console", "Intl", "Temporal", "XMLHttpRequest", "WebSocket", "Deno", "Bun"];
    const o = {};
    for (const n of 禁止的) { o[n] = typeof globalThis[n]; }
    return o;
  },
};
`)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := m.NewRuntime(context.Background(), Host{}, nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	var out map[string]any
	if f := rt.Call(context.Background(), "onRequest", map[string]any{"a": 1}, &out); f != nil {
		t.Fatalf("调用不该失败: %v", f)
	}
	if len(out) == 0 {
		t.Fatal("插件没返回任何字段，这条测试没验到东西")
	}
	for _, k := range []string{"fetch", "require", "process", "setTimeout", "setInterval", "setImmediate", "console", "Intl", "Temporal", "XMLHttpRequest", "WebSocket", "Deno", "Bun"} {
		if out[k] != "undefined" {
			t.Errorf("typeof %s = %v，应当是 \"undefined\"", k, out[k])
		}
	}
}

// 有 Date 是刻意的：日历类能力要按本地时间判断，插件自己也要能读时间。
// 单独钉一条，免得将来有人为了「更严的沙箱」把它一起关掉。
func Test默认全局有Date(t *testing.T) {
	m, err := Compile("p", `export default { onRequest() { return { d: typeof Date, n: typeof Date.now }; } };`)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := m.NewRuntime(context.Background(), Host{}, nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	var out map[string]any
	if f := rt.Call(context.Background(), "onRequest", map[string]any{"a": 1}, &out); f != nil {
		t.Fatalf("调用不该失败: %v", f)
	}
	if out["d"] != "function" || out["n"] != "function" {
		t.Errorf("Date 应当可用，实际 %v", out)
	}
}

// import.meta 是一个**空对象**，既没有 url 也没有别的字段。
// 宿主没给 Options.Importer，所以任何一个 url 都是空的。
// 这条钉住「插件拿 import.meta 读不出任何东西」，别让人以为它能带信息进来。
func TestImportMeta是空对象(t *testing.T) {
	m, err := Compile("p", `
export default {
  onRequest() {
    return {
      t: typeof import.meta,
      keys: Object.getOwnPropertyNames(import.meta).length,
      url: typeof import.meta.url,
    };
  },
};
`)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := m.NewRuntime(context.Background(), Host{}, nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	var out map[string]any
	if f := rt.Call(context.Background(), "onRequest", map[string]any{"a": 1}, &out); f != nil {
		t.Fatalf("调用不该失败: %v", f)
	}
	if out["t"] != "object" {
		t.Errorf("typeof import.meta = %v，想要 object", out["t"])
	}
	if out["keys"] != float64(0) {
		t.Errorf("import.meta 的属性数 = %v，想要 0", out["keys"])
	}
	if out["url"] != "undefined" {
		t.Errorf("typeof import.meta.url = %v，想要 undefined（宿主没给 Importer）", out["url"])
	}
}

// 静态 import 在链接期被 resolver 挡住（见 Test只放行虚拟SDK的导入），
// 动态 import() 走的是另一条路：Options.Importer 没设时，**任何** specifier
// 都被拒——包括虚拟 SDK 自己。这条钉住「动态导入不是绕过静态检查的口子」。
func Test动态import任何说明符都被拒(t *testing.T) {
	for _, spec := range []string{`"node:fs"`, `"larkax"`} {
		// 结果记在模块变量里：async hook 的返回值会被解成空对象，
		// 而 .then 的回调在 Call 返回前就跑了（moejs 会把 job 跑完）。
		m, err := Compile("p", `
let 结果 = "未开始";
import(`+spec+`).then(
  (v) => { 结果 = "解析成功: " + typeof v; },
  (e) => { 结果 = "被拒: " + e.name; },
);
export default { onRequest() { return { 结果: 结果 }; } };
`)
		if err != nil {
			t.Fatalf("import(%s) 应当能编译，实际: %v", spec, err)
		}
		rt, err := m.NewRuntime(context.Background(), Host{}, nil, Limits{})
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		if f := rt.Call(context.Background(), "onRequest", map[string]any{"a": 1}, &out); f != nil {
			t.Fatalf("import(%s): 调用不该失败: %v", spec, f)
		}
		if 结果, _ := out["结果"].(string); 结果 != "被拒: TypeError" {
			t.Errorf("import(%s) 的结果 = %v，想要「被拒: TypeError」（宿主没给 Importer）", spec, out["结果"])
		}
		rt.Close()
	}
}

// 插件把 hook 写成 async 时，Call 拿到的是一个 Promise 而不是补丁对象。
//
// **它必须报错，不能静默成功。** Promise 走正常解组路径会解成一个空对象，
// 于是插件作者看到的是「我明明 return 了东西，宿主却说没给」——
// 那种静默是排查里最费时间的一类：插件自己测不出问题，宿主日志里也没有异常。
//
// 契约里顶层 await 与异步 hook 都不支持，所以这里直接拒绝，
// 而不是等那个 Promise 落地（宿主不驱动 job 队列，等也等不到）。
func Test异步hook被显式拒绝(t *testing.T) {
	m, err := Compile("p", `
export default {
  async onRequest() { return { temperature: 0.5 }; },
  onRoute() { return { temperature: 0.5 }; },
};
`)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := m.NewRuntime(context.Background(), Host{}, nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	var asyncOut struct {
		Temperature *float64 `json:"temperature"`
	}
	f := rt.Call(context.Background(), "onRequest", map[string]any{"a": 1}, &asyncOut)
	if f == nil {
		t.Fatalf("async hook 该被拒绝，实际成功了，解出 %+v", asyncOut)
	}
	if !errors.Is(f, ErrAsyncHook) {
		t.Errorf("Kind = %v，想要 ErrAsyncHook", f.Kind)
	}
	if asyncOut.Temperature != nil {
		t.Errorf("被拒绝时不该改动出参，实际 %v", *asyncOut.Temperature)
	}

	// 同一个结构体、同一个返回值，同步写法就能读到——两个 hook 的差别只在 async。
	var syncOut struct {
		Temperature *float64 `json:"temperature"`
	}
	if f := rt.Call(context.Background(), "onRoute", map[string]any{"a": 1}, &syncOut); f != nil {
		t.Fatalf("调用不该失败: %v", f)
	}
	if syncOut.Temperature == nil || *syncOut.Temperature != 0.5 {
		t.Errorf("同步 hook 的返回值没解出来: %s", showPtr(syncOut.Temperature))
	}
}

// DisableDynamicCode 让 eval 与 Function 构造器直接抛 EvalError。
// 插件源码在编译期就解析完了，运行时不需要动态生成代码；
// 关掉它意味着插件不能靠拼字符串绕过任何东西。
func TestEval与Function构造器被关掉(t *testing.T) {
	m, err := Compile("p", `
export default {
  onRequest() {
    const out = {};
    try { eval("1+1"); out.evalName = "没抛"; } catch (e) { out.evalName = e.name; }
    try { new Function("return 1"); out.fnName = "没抛"; } catch (e) { out.fnName = e.name; }
    try { globalThis.Function("return 1"); out.gFnName = "没抛"; } catch (e) { out.gFnName = e.name; }
    return out;
  },
};
`)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := m.NewRuntime(context.Background(), Host{}, nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	var out map[string]any
	if f := rt.Call(context.Background(), "onRequest", map[string]any{"a": 1}, &out); f != nil {
		t.Fatalf("调用不该失败: %v", f)
	}
	for _, k := range []string{"evalName", "fnName", "gFnName"} {
		if out[k] != "EvalError" {
			t.Errorf("%s = %v，想要 EvalError", k, out[k])
		}
	}
}

// 内建原型被深度冻结并跨 runtime 共享，写它抛 TypeError。
// 更要紧的是第二个 runtime 看到的原型是**干净的**：
// 冻结把「一个插件污染所有插件」这条路堵死了。
func Test内建原型被冻结且跨runtime保持干净(t *testing.T) {
	m, err := Compile("p", `
export default {
  onRequest() {
    const out = {};
    try { Object.prototype.污染 = 1; out.写 = "没抛"; } catch (e) { out.写 = e.name; }
    out.冻结 = Object.isFrozen(Object.prototype);
    out.污染读回 = typeof ({}).污染;
    out.原型属性数 = Object.getOwnPropertyNames(Object.prototype).length;
    return out;
  },
};
`)
	if err != nil {
		t.Fatal(err)
	}

	// 同一个插件在两个 runtime 里各跑一次：两次看到的都必须是干净的。
	for i := 0; i < 2; i++ {
		rt, err := m.NewRuntime(context.Background(), Host{}, nil, Limits{})
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		if f := rt.Call(context.Background(), "onRequest", map[string]any{"a": 1}, &out); f != nil {
			t.Fatalf("第 %d 个 runtime 调用失败: %v", i, f)
		}
		if out["写"] != "TypeError" {
			t.Errorf("第 %d 个 runtime: 写 Object.prototype 得到 %v，想要 TypeError", i, out["写"])
		}
		if out["冻结"] != true {
			t.Errorf("第 %d 个 runtime: Object.isFrozen(Object.prototype) = %v，想要 true", i, out["冻结"])
		}
		if out["污染读回"] != "undefined" {
			t.Errorf("第 %d 个 runtime: 原型被污染了，(%s).污染 = %v", i, "{}", out["污染读回"])
		}
		rt.Close()
	}
}

// 插件不该有任何渠道密钥。这一层根本不往插件里放密钥，
// 所以这里断言的是「能力面里没有任何形如密钥的名字」——
// 上层如果哪天往 Host.Capabilities 里塞了一个 credential，这条会先响。
func Test能力面里没有任何形如密钥的名字(t *testing.T) {
	m, err := Compile("p", `
export default {
  onRequest() {
    const names = Object.getOwnPropertyNames(globalThis);
    const ns = globalThis.__larkax ? Object.getOwnPropertyNames(globalThis.__larkax) : [];
    const members = [];
    for (const n of ns) {
      const v = globalThis.__larkax[n];
      if (v && typeof v === "object") {
        for (const k of Object.getOwnPropertyNames(v)) { members.push(n + "." + k); }
      }
    }
    return { names: names, ns: ns, members: members };
  },
};
`)
	if err != nil {
		t.Fatal(err)
	}
	host := Host{Capabilities: map[string]any{
		"log": map[string]any{"log": func(ctx context.Context, args []moejs.Value) (moejs.Value, error) {
			return moejs.Undefined(), nil
		}},
	}}
	rt, err := m.NewRuntime(context.Background(), host, []string{"log"}, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	var out map[string]any
	if f := rt.Call(context.Background(), "onRequest", map[string]any{"a": 1}, &out); f != nil {
		t.Fatalf("调用不该失败: %v", f)
	}

	// 插件确实看到了一个能力，否则这条测试没验到东西。
	if ns := out["ns"].([]any); len(ns) != 1 || ns[0] != "log" {
		t.Fatalf("__larkax 的键 = %v，想要 [log]", ns)
	}

	敏感词 := []string{"key", "secret", "token", "cred", "password", "passwd", "auth"}
	seen := make([]string, 0, 8)
	for _, list := range []any{out["ns"], out["members"], out["names"]} {
		for _, n := range list.([]any) {
			seen = append(seen, n.(string))
		}
	}
	for _, n := range seen {
		low := strings.ToLower(n)
		for _, w := range 敏感词 {
			if strings.Contains(low, w) {
				t.Errorf("插件看到了形如密钥的名字 %q（命中 %q）", n, w)
			}
		}
	}
}

// ---------- 元数据 ----------

// Compile 求值一次模块顶层，把 export const meta 读成 map[string]any。
// 嵌套的数组与对象也要读出来，否则上层拿到的是一半信息。
func TestMeta读出嵌套结构(t *testing.T) {
	m, err := Compile("p", `
export const meta = {
  name: "示例",
  version: "1.0.0",
  tags: ["a", "b"],
  nested: { deep: { n: 3 }, list: [1, 2, {k: "v"}] },
  ok: true,
  nothing: null,
};
export default { onRequest() {} };
`)
	if err != nil {
		t.Fatal(err)
	}
	meta := m.Meta()
	if meta == nil {
		t.Fatal("有 export const meta 时不该返回 nil")
	}
	if meta["name"] != "示例" || meta["version"] != "1.0.0" {
		t.Errorf("顶层标量没读对: %v", meta)
	}
	tags, ok := meta["tags"].([]any)
	if !ok || len(tags) != 2 || tags[0] != "a" || tags[1] != "b" {
		t.Errorf("数组没读对: %#v", meta["tags"])
	}
	nested, ok := meta["nested"].(map[string]any)
	if !ok {
		t.Fatalf("嵌套对象没读成 map: %#v", meta["nested"])
	}
	deep, ok := nested["deep"].(map[string]any)
	if !ok || deep["n"] != float64(3) {
		t.Errorf("两层嵌套没读对: %#v", nested["deep"])
	}
	list, ok := nested["list"].([]any)
	if !ok || len(list) != 3 {
		t.Fatalf("嵌套数组没读对: %#v", nested["list"])
	}
	if inner, ok := list[2].(map[string]any); !ok || inner["k"] != "v" {
		t.Errorf("数组里的对象没读对: %#v", list[2])
	}
	if meta["ok"] != true {
		t.Errorf("布尔没读对: %#v", meta["ok"])
	}
	if v, present := meta["nothing"]; !present || v != nil {
		t.Errorf("显式 null 应读成存在的 nil，实际 present=%v v=%#v", present, v)
	}
}

// 没有 export const meta 时返回 nil，不是错误：
// 「meta 必需」是上层要决定的事，这一层只如实回答有没有。
func Test没有meta时返回nil(t *testing.T) {
	m, err := Compile("p", "export default { onRequest() {} };\n")
	if err != nil {
		t.Fatal(err)
	}
	if meta := m.Meta(); meta != nil {
		t.Errorf("没有 meta 时应当返回 nil，实际 %#v", meta)
	}
}

// meta 不是对象时是编译错误。这里要把三种形态分开：
// 字符串/数字/数组都不行，而 null 与 undefined 等价于「没有 meta」。
func TestMeta不是对象时报错(t *testing.T) {
	for _, src := range []string{`"就是个字符串"`, `42`, `[1, 2]`, `true`} {
		_, err := Compile("p", "export const meta = "+src+";\nexport default { onRequest() {} };\n")
		if err == nil {
			t.Errorf("meta = %s 应当编译失败", src)
			continue
		}
		if !strings.Contains(err.Error(), "meta 不是一个对象") {
			t.Errorf("meta = %s 的错误信息应说明 meta 不是对象，实际: %v", src, err)
		}
	}
	for _, src := range []string{`null`, `undefined`} {
		m, err := Compile("p", "export const meta = "+src+";\nexport default { onRequest() {} };\n")
		if err != nil {
			t.Errorf("meta = %s 应当通过，实际: %v", src, err)
			continue
		}
		if meta := m.Meta(); meta != nil {
			t.Errorf("meta = %s 时 Meta() 应当是 nil，实际 %#v", src, meta)
		}
	}
}

// meta 里写一个死循环时，报出来的必须是**超时**，而不是「meta 不是一个对象」。
//
// 读 meta 这一段同样在跑插件代码（`toJSON`、getter 都会被执行），而它跑在
// 调用方 goroutine 上、不在 loadWithin 的 select 里：它的界来自 inspect 里
// 那个 AfterFunc。界是隐式的，失败文案因此更容易说反——原始错误是
// 「interrupted: 求值插件 p 的顶层超过 2s」，外层包装会说成「meta 不是一个对象」。
// 上传期这两种错的处置不同：一个要作者改 meta，一个要作者把重活挪进 hook。
//
// 这条是全包最慢的一条：要等满 inspectTimeout（界是常量，没法从外面调短）。
// 它值这个时间——「界还在不在」正是它要钉的东西：去掉那个 AfterFunc，
// Compile 就不会在这个窗口内返回了。
func TestMeta里的死循环报的是超时而不是形状错(t *testing.T) {
	type 结果 struct {
		m   *Module
		err error
	}
	done := make(chan 结果, 1)
	开始 := time.Now()
	go func() {
		m, err := Compile("p", `
export const meta = { key: "k", toJSON() { while (true) {} } };
export default { onRequest() { return {}; } };
`)
		done <- 结果{m: m, err: err}
	}()

	select {
	case r := <-done:
		if r.err == nil {
			t.Fatal("meta 里的死循环应当让 Compile 失败")
		}
		if !errors.Is(r.err, ErrCallTimeout) {
			t.Errorf("错误 = %v，想要包住 ErrCallTimeout", r.err)
		}
		// 这里断言**文案**而不是分类：被中断的错误本身就包着那个超时 cause，
		// 所以「分类还在不在」穿得过外层那层包装，只有文案能区分它说的是
		// 「meta 的形状不对」还是「超时」。
		if 文本 := r.err.Error(); strings.Contains(文本, "meta 不是一个对象") {
			t.Errorf("文案说的是 meta 的形状，而真正的原因是超时: %v", r.err)
		}
		if 花了 := time.Since(开始); 花了 < inspectTimeout {
			t.Errorf("耗时 %v 早于 %v，这条走的不是超时路径", 花了, inspectTimeout)
		}
	case <-time.After(inspectTimeout + 2*time.Second):
		t.Fatal("Compile 没有在窗口内返回：读 meta 那一段没有界了")
	}
}

// Meta 只返回编译期读出来的快照，不执行任何 hook。
// 用一个「被调用就抛异常」的 hook 来钉：Compile 之后 Meta 照样能读到值，
// 而且新 runtime 里的计数器仍是 0，说明 hook 一次都没跑过。
func Test读Meta不执行任何hook(t *testing.T) {
	m, err := Compile("p", `
let 调用次数 = 0;
export const meta = { name: "x" };
export default {
  onRequest() { 调用次数++; throw new Error("hook 被执行了"); },
  onUsage() { 调用次数++; return { 调用次数: 调用次数 }; },
};
`)
	if err != nil {
		t.Fatalf("读 meta 不该执行 hook，所以这里不该失败: %v", err)
	}
	if meta := m.Meta(); meta == nil || meta["name"] != "x" {
		t.Fatalf("Meta() = %#v，想要 name=x", meta)
	}
	if !m.Has("onRequest") {
		t.Fatalf("Hooks() = %v，应含 onRequest", m.Hooks())
	}

	rt, err := m.NewRuntime(context.Background(), Host{}, nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	var out map[string]any
	if f := rt.Call(context.Background(), "onUsage", map[string]any{"a": 1}, &out); f != nil {
		t.Fatalf("onUsage 不该失败: %v", f)
	}
	if out["调用次数"] != float64(1) {
		t.Errorf("hook 的计数器 = %v，想要 1——大于 1 说明读 meta 时跑过 hook", out["调用次数"])
	}
}

// Module 是不可变的：一份 Module 被多个 goroutine 各自 NewRuntime 并 Call，
// 结果各自正确。Runtime 才是必须独占的那个。
// 配 -race 跑，这条才真正有价值。
func TestModule可被多个goroutine并发使用(t *testing.T) {
	m, err := Compile("p", `
import { definePlugin } from "larkax";
export default definePlugin({
  onRequest(ctx) { return { model: ctx.model, temperature: 0.25 }; },
});
`)
	if err != nil {
		t.Fatal(err)
	}

	const n = 24
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			want := "模型-" + string(rune('a'+i%26))
			rt, err := m.NewRuntime(context.Background(), Host{}, nil, Limits{})
			if err != nil {
				errs <- err
				return
			}
			defer rt.Close()
			var out struct {
				Model       string   `json:"model"`
				Temperature *float64 `json:"temperature"`
			}
			f := rt.Call(context.Background(), "onRequest", map[string]any{"model": want}, &out)
			if f != nil {
				errs <- f
				return
			}
			if out.Model != want {
				errs <- errors.New("模型名串了: " + out.Model + " != " + want)
				return
			}
			if out.Temperature == nil || *out.Temperature != 0.25 {
				errs <- errors.New("temperature 不对")
				return
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("并发用例失败: %v", err)
	}
}

// Meta 与 Compile 也是并发安全的：Module 里缓存的是不可变快照。
func TestMeta与NewRuntime可并发调用(t *testing.T) {
	m, err := Compile("p", `
export const meta = { name: "并发" };
export default { onRequest() { return { ok: true }; } };
`)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if meta := m.Meta(); meta == nil || meta["name"] != "并发" {
				t.Errorf("Meta() = %#v", meta)
			}
		}()
		go func() {
			defer wg.Done()
			rt, err := m.NewRuntime(context.Background(), Host{}, nil, Limits{})
			if err != nil {
				t.Errorf("NewRuntime: %v", err)
				return
			}
			defer rt.Close()
			if f := rt.Call(context.Background(), "onRequest", map[string]any{"a": 1}, nil); f != nil {
				t.Errorf("Call: %v", f)
			}
		}()
	}
	wg.Wait()
}

// showPtr 把一个可空值渲染成可读文本，供断言失败时打印。
//
// 直接 %v 一个指向基本类型的指针打出来的是地址（`0xc000...`），而报错信息
// 本身是要给人读的——那串地址看不出实际值是什么，也就看不出这次失败是
// 「值算错了」还是「字段根本没给」。
func showPtr[T any](p *T) string {
	if p == nil {
		return "<nil>"
	}
	return fmt.Sprint(*p)
}
