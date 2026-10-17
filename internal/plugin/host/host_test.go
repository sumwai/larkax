package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Calcium-Ion/moejs"

	"github.com/sumwai/larkax/internal/plugin/sandbox"
)

// 测试用的固定时刻。取值本身只是占位形态，是「某个整秒」而已。
var testInstant = time.Unix(1700000000, 0)

// ctxKey 是测试塞进 ctx 的私有键，用来确认宿主函数把 ctx 继续传给了依赖。
type ctxKey struct{}

// js 把 Go 值翻成 JS 参数。字符串、整数、布尔、nil（null）够覆盖所有用例，
// 对象与数组用 jsJSON。
func js(values ...any) []moejs.Value {
	out := make([]moejs.Value, 0, len(values))
	for _, v := range values {
		switch t := v.(type) {
		case string:
			out = append(out, moejs.String(t))
		case int64:
			out = append(out, moejs.Int(t))
		case bool:
			out = append(out, moejs.Bool(t))
		case nil:
			out = append(out, moejs.Null())
		case moejs.Value:
			out = append(out, t)
		default:
			panic("测试只用到这几种参数类型")
		}
	}
	return out
}

// jsJSON 把一段 JSON 解析成 JS 值。用它构造对象与数组，
// 比手写引擎的构造调用更接近插件实际传进来的东西。
func jsJSON(t *testing.T, text string) moejs.Value {
	t.Helper()
	rt := moejs.NewRuntime(moejs.Options{})
	v, err := rt.ParseJSONString(text)
	if err != nil {
		t.Fatalf("解析测试用 JSON %q 失败: %v", text, err)
	}
	return v
}

// jsEval 求值一段 JS 表达式，用它构造 jsJSON 表达不了的形态
// （访问器、symbol 键、函数、BigInt、带自定义属性的数组）。
func jsEval(t *testing.T, src string) moejs.Value {
	t.Helper()
	rt := moejs.NewRuntime(moejs.Options{})
	script, err := moejs.CompileScript("fields.js", "("+src+")")
	if err != nil {
		t.Fatalf("编译测试脚本失败: %v", err)
	}
	v, err := rt.RunScript(script)
	if err != nil {
		t.Fatalf("执行测试脚本失败: %v", err)
	}
	return v
}

// testLogger 返回写进 buf 的日志器。
func testLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// logEntries 把 buf 里的 JSON 日志逐行解出来。
func logEntries(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("日志行不是 JSON: %q: %v", line, err)
		}
		out = append(out, entry)
	}
	return out
}

// capability 从 Standard 的结果里取出一个能力。
func capability(t *testing.T, caps []Capability, name string) Capability {
	t.Helper()
	for _, c := range caps {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("能力 %s 不在 Standard 的返回值里", name)
	return Capability{}
}

func TestStandardAvailability(t *testing.T) {
	clock := func() time.Time { return testInstant }

	cases := []struct {
		name       string
		env        Env
		wantNames  map[string]bool
		reasonText string
	}{
		{
			name: "依赖齐备时三个能力都可用",
			env: Env{
				Calendar: &fakeCalendar{},
				KV:       newFakeKV(),
				Now:      clock,
			},
			wantNames:  map[string]bool{capLog: true, capCalendar: true, capKV: true},
			reasonText: "依赖都在",
		},
		{
			name:       "没有日历与 KV 时只剩日志",
			env:        Env{},
			wantNames:  map[string]bool{capLog: true, capCalendar: false, capKV: false},
			reasonText: "nil 依赖表示这次请求不提供该能力",
		},
		{
			name:       "只有日历",
			env:        Env{Calendar: &fakeCalendar{}},
			wantNames:  map[string]bool{capLog: true, capCalendar: true, capKV: false},
			reasonText: "KV 仍为 nil",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			caps := Standard(tc.env)
			names := make(map[string]bool, len(caps))
			for _, c := range caps {
				names[c.Name] = c.Available()
			}
			for name, want := range tc.wantNames {
				if names[name] != want {
					t.Errorf("能力 %s 的 Available() = %v，期望 %v（%s）", name, names[name], want, tc.reasonText)
				}
			}
			if len(caps) != len(tc.wantNames) {
				t.Errorf("Standard 返回 %d 个能力，期望 %d 个", len(caps), len(tc.wantNames))
			}
		})
	}
}

// TestCapabilityHostValueIsMapOfAny 钉住能力的注入形态。
//
// sandbox 注入时按 map[string]any 这个**具体类型**识别命名空间，
// 直接把 map[string]sandbox.HostFunc 放进 Host.Capabilities 会被
// 当成「这不是一个函数」。这里从类型上固定住这件事。
func TestCapabilityHostValueIsMapOfAny(t *testing.T) {
	for _, c := range Standard(Env{Calendar: &fakeCalendar{}, KV: newFakeKV()}) {
		h := sandbox.Host{
			Capabilities: map[string]any{c.Name: c.HostValue()},
		}
		got, ok := h.Capabilities[c.Name].(map[string]any)
		if !ok {
			t.Fatalf("能力 %s 的 HostValue() 不是 map[string]any", c.Name)
		}
		for fn, impl := range got {
			if _, ok := impl.(sandbox.HostFunc); !ok {
				t.Errorf("能力 %s 的 %s 不是 sandbox.HostFunc", c.Name, fn)
			}
		}
		if len(got) != len(c.Funcs) {
			t.Errorf("能力 %s 转换丢了函数：%d → %d", c.Name, len(c.Funcs), len(got))
		}
	}
}

// TestSDKNamesMatchCapabilityNames 钉住能力名。
//
// 名字与 sandbox/sdk.go 里 `capability("log")` 那几处必须一致，
// 否则插件调用会得到「能力未启用」，而那是运行时才会暴露的错。
func TestSDKNamesMatchCapabilityNames(t *testing.T) {
	want := map[string][]string{
		capLog:      {"log"},
		capCalendar: {"isOffDay", "isMakeupWorkday", "holidayName", "kind", "hourIn", "weekdayIn"},
		capKV:       {"get", "set", "del"},
	}
	caps := Standard(Env{Calendar: &fakeCalendar{}, KV: newFakeKV()})
	for name, funcs := range want {
		c := capability(t, caps, name)
		for _, fn := range funcs {
			if _, ok := c.Funcs[fn]; !ok {
				t.Errorf("能力 %s 缺少 SDK 里用到的 %s", name, fn)
			}
		}
		if len(c.Funcs) != len(funcs) {
			t.Errorf("能力 %s 有 %d 个函数，SDK 里有 %d 个", name, len(c.Funcs), len(funcs))
		}
	}
}

// TestHostFuncStopsOnExpiredContext 钉住「每个宿主函数进门先看 ctx」。
//
// 宿主函数阻塞时 Interrupt 无效（实测：宿主 sleep 300ms 配 50ms deadline
// 实际跑满 301ms），所以已经超时就必须立刻返回，且不做任何工作。
func TestHostFuncStopsOnExpiredContext(t *testing.T) {
	store := newFakeKV()
	store.data[testKVKey("p", 7, "k")] = "v"
	cal := &fakeCalendar{}
	env := Env{Plugin: "p", UserID: 7, Logger: testLogger(&bytes.Buffer{}), Calendar: cal, KV: store}

	cases := []struct {
		name    string
		capName string
		fn      string
		args    []moejs.Value
		wantErr error
	}{
		{"kv.get", capKV, "get", js("k"), sandbox.ErrHostTimeout},
		{"kv.set", capKV, "set", js("k", "v"), sandbox.ErrHostTimeout},
		{"kv.del", capKV, "del", js("k"), sandbox.ErrHostTimeout},
		{"calendar.kind", capCalendar, "kind", js(int64(1700000000), "CN"), sandbox.ErrHostTimeout},
		{"calendar.hourIn", capCalendar, "hourIn", js("CN"), sandbox.ErrHostTimeout},
		{"calendar.weekdayIn", capCalendar, "weekdayIn", js("CN"), sandbox.ErrHostTimeout},
		{"calendar.isOffDay", capCalendar, "isOffDay", js(int64(1700000000), "CN"), sandbox.ErrHostTimeout},
		{"calendar.holidayName", capCalendar, "holidayName", js(int64(1700000000), "CN"), sandbox.ErrHostTimeout},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithDeadline(context.Background(), testInstant.Add(-time.Second))
			defer cancel()
			before := store.calls
			_, err := capability(t, Standard(env), tc.capName).Funcs[tc.fn](ctx, tc.args)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("错误 = %v，期望 %v", err, tc.wantErr)
			}
			if store.calls != before {
				t.Errorf("超时的 ctx 仍然访问了 KV：调用次数 %d → %d", before, store.calls)
			}
			if cal.calls != 0 {
				t.Errorf("超时的 ctx 仍然访问了日历：%d 次", cal.calls)
			}
		})
	}
}

// TestCanceledContextPassesCauseThrough 确认客户端断开（取消而非超时）时
// 报的是取消本身，而不是被归到宿主超时里——两者的处置不同。
func TestCanceledContextPassesCauseThrough(t *testing.T) {
	store := newFakeKV()
	env := Env{Plugin: "p", Logger: testLogger(&bytes.Buffer{}), KV: store}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := capability(t, Standard(env), capKV).Funcs["get"](ctx, js("k"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("错误 = %v，期望 context.Canceled", err)
	}
	if errors.Is(err, sandbox.ErrHostTimeout) {
		t.Errorf("取消不该被报成宿主超时: %v", err)
	}
	if store.calls != 0 {
		t.Errorf("已取消的 ctx 仍然访问了 KV：%d 次", store.calls)
	}
}

// TestContextReachesDependencies 钉住「传进来的 ctx 继续传给依赖」。
//
// 宿主函数用自己的 ctx 去查依赖时，取消就传不下去，
// 表现为上游已经断开而查询还在跑。
func TestContextReachesDependencies(t *testing.T) {
	store := newFakeKV()
	cal := &fakeCalendar{}
	env := Env{Plugin: "p", Logger: testLogger(&bytes.Buffer{}), Calendar: cal, KV: store}

	ctx := context.WithValue(context.Background(), ctxKey{}, "标记")

	if _, err := capability(t, Standard(env), capKV).Funcs["get"](ctx, js("k")); err != nil {
		t.Fatalf("kv.get 出错: %v", err)
	}
	if got := store.lastCtx.Value(ctxKey{}); got != "标记" {
		t.Errorf("KV 拿到的 ctx 丢了值: %v", got)
	}
	if _, err := capability(t, Standard(env), capCalendar).Funcs["kind"](ctx, js(int64(1700000000), "CN")); err != nil {
		t.Fatalf("calendar.kind 出错: %v", err)
	}
	if got := cal.lastCtx.Value(ctxKey{}); got != "标记" {
		t.Errorf("日历拿到的 ctx 丢了值: %v", got)
	}
}

func TestParseLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"DEBUG":   slog.LevelDebug,
		"info":    slog.LevelInfo,
		"":        slog.LevelInfo,
		"warn":    slog.LevelWarn,
		"warning": slog.LevelWarn,
		" WARN ":  slog.LevelWarn,
		"error":   slog.LevelError,
		"trace":   slog.LevelInfo, // 认不出来按 info
		"随便写":     slog.LevelInfo,
	}
	for in, want := range cases {
		if got := parseLevel(in); got != want {
			t.Errorf("parseLevel(%q) = %v，期望 %v", in, got, want)
		}
	}
}

func TestLogAddsPluginAndRequestID(t *testing.T) {
	var buf bytes.Buffer
	env := Env{Plugin: "token-adjust", RequestID: "req-1", Logger: testLogger(&buf)}

	fn := capability(t, Standard(env), capLog).Funcs["log"]
	if _, err := fn(context.Background(), js("info", "插件说的话")); err != nil {
		t.Fatalf("log 出错: %v", err)
	}

	entries := logEntries(t, &buf)
	if len(entries) != 1 {
		t.Fatalf("写出了 %d 条日志，期望 1 条", len(entries))
	}
	entry := entries[0]
	if entry["plugin"] != "token-adjust" {
		t.Errorf("plugin 字段 = %v", entry["plugin"])
	}
	if entry["request_id"] != "req-1" {
		t.Errorf("request_id 字段 = %v", entry["request_id"])
	}
	if entry["msg"] != "插件说的话" {
		t.Errorf("msg = %v", entry["msg"])
	}
	if entry["level"] != "INFO" {
		t.Errorf("level = %v，期望 INFO", entry["level"])
	}
}

func TestLogLevels(t *testing.T) {
	cases := []struct {
		level string
		want  string
	}{
		{"debug", "DEBUG"},
		{"info", "INFO"},
		{"warn", "WARN"},
		{"error", "ERROR"},
		{"认不出来的级别", "INFO"}, // 认不出来按 info，不报错
	}
	for _, tc := range cases {
		t.Run(tc.level, func(t *testing.T) {
			var buf bytes.Buffer
			env := Env{Logger: testLogger(&buf)}
			fn := capability(t, Standard(env), capLog).Funcs["log"]

			if _, err := fn(context.Background(), js(tc.level, "m")); err != nil {
				t.Fatalf("log 出错: %v", err)
			}
			entries := logEntries(t, &buf)
			if len(entries) != 1 || entries[0]["level"] != tc.want {
				t.Fatalf("level = %v，期望 %v", entries, tc.want)
			}
		})
	}
}

// TestLogFieldsObject 覆盖 fields 的两种形态：对象、数组。
func TestLogFieldsObject(t *testing.T) {
	cases := []struct {
		name  string
		field string
		check func(t *testing.T, entry map[string]any)
	}{
		{
			name:  "对象",
			field: `{"model":"gpt-x","tokens":12,"cached":true,"note":null}`,
			check: func(t *testing.T, entry map[string]any) {
				if entry["model"] != "gpt-x" {
					t.Errorf("model = %v", entry["model"])
				}
				if entry["tokens"] != float64(12) {
					t.Errorf("tokens = %v", entry["tokens"])
				}
				if entry["cached"] != true {
					t.Errorf("cached = %v", entry["cached"])
				}
				if v, ok := entry["note"]; !ok || v != nil {
					t.Errorf("note = %v（应存在且为 null）", v)
				}
			},
		},
		{
			name:  "数组",
			field: `["a","b"]`,
			check: func(t *testing.T, entry map[string]any) {
				// 顶层 fields 不是对象时整个忽略，不产生任何字段。
				for k := range entry {
					switch k {
					case "time", "level", "msg", "plugin", "request_id":
					default:
						t.Errorf("数组形态的 fields 不该产生字段 %q", k)
					}
				}
			},
		},
		{
			name:  "嵌套对象",
			field: `{"usage":{"prompt":1,"completion":2}}`,
			check: func(t *testing.T, entry map[string]any) {
				usage, ok := entry["usage"].(map[string]any)
				if !ok {
					t.Fatalf("usage = %#v", entry["usage"])
				}
				if usage["prompt"] != float64(1) || usage["completion"] != float64(2) {
					t.Errorf("usage = %#v", usage)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			env := Env{Logger: testLogger(&buf)}
			fn := capability(t, Standard(env), capLog).Funcs["log"]

			if _, err := fn(context.Background(), js("info", "m", jsJSON(t, tc.field))); err != nil {
				t.Fatalf("log 出错: %v", err)
			}
			entries := logEntries(t, &buf)
			if len(entries) != 1 {
				t.Fatalf("写出了 %d 条日志", len(entries))
			}
			tc.check(t, entries[0])
		})
	}
}

// TestLogFieldsUndecodableIsIgnored 确认 fields 转不出来时只是丢掉它，
// 不让这次 log 失败——日志本身不该让请求失败。
func TestLogFieldsUndecodableIsIgnored(t *testing.T) {
	cases := []struct {
		name  string
		value func(t *testing.T) moejs.Value
	}{
		{"函数", func(t *testing.T) moejs.Value {
			rt := moejs.NewRuntime(moejs.Options{})
			return rt.Function("f", 0, func(*moejs.Realm, moejs.Value, []moejs.Value) (moejs.Value, error) {
				return moejs.Undefined(), nil
			})
		}},
		{"字符串", func(t *testing.T) moejs.Value { return moejs.String("这不是对象") }},
		{"undefined", func(t *testing.T) moejs.Value { return moejs.Undefined() }},
		{"嵌套过深", func(t *testing.T) moejs.Value {
			return jsJSON(t, `{"a":{"b":{"c":{"d":{"e":{"f":1}}}}}}`)
		}},
		{"成员是函数", func(t *testing.T) moejs.Value {
			// 函数成员转不出来，跳过它而不是让整条 log 失败。
			return jsEval(t, `({ keep: 1, fn: () => 1 })`)
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			env := Env{Logger: testLogger(&buf)}
			fn := capability(t, Standard(env), capLog).Funcs["log"]

			if _, err := fn(context.Background(), js("info", "m", tc.value(t))); err != nil {
				t.Fatalf("fields 转不出来不该让 log 失败: %v", err)
			}
			entries := logEntries(t, &buf)
			if len(entries) != 1 || entries[0]["msg"] != "m" {
				t.Fatalf("日志没写出来: %#v", entries)
			}
		})
	}
}

// TestLogFieldsGetNotInvoked 确认读 fields 时只取自有数据属性，不跑 getter。
// 让插件通过一个日志字段在宿主里执行代码是不该发生的事。
func TestLogFieldsGetNotInvoked(t *testing.T) {
	obj := jsEval(t, `({
		get boom() { throw new Error("getter 不该被调用"); },
		plain: 1,
	})`)

	var buf bytes.Buffer
	env := Env{Logger: testLogger(&buf)}
	fn := capability(t, Standard(env), capLog).Funcs["log"]

	// getter 会抛异常。它一旦被调用，这里拿到的就是那个异常，
	// 而不是一次正常的日志调用。
	if _, err := fn(context.Background(), js("info", "m", obj)); err != nil {
		t.Fatalf("读 fields 时调用了 getter: %v", err)
	}
	entries := logEntries(t, &buf)
	if len(entries) != 1 {
		t.Fatalf("写出了 %d 条日志", len(entries))
	}
	if entries[0]["plain"] != float64(1) {
		t.Errorf("自有数据属性没读出来: %#v", entries[0])
	}
	if _, ok := entries[0]["boom"]; ok {
		t.Error("访问器属性不该出现在日志字段里")
	}
}

// TestLogFieldsConversionEdges 覆盖几个少见的成员形态：
// symbol 键、成员是函数、成员是 Symbol 值、嵌套数组、以及字段数超限。
func TestLogFieldsConversionEdges(t *testing.T) {
	cases := []struct {
		name  string
		field string
		check func(t *testing.T, entry map[string]any)
	}{
		{
			name:  "symbol 键跳过、函数成员跳过",
			field: `(() => { const o = { keep: 1, fn: () => 1 }; o[Symbol("s")] = 2; return o; })()`,
			check: func(t *testing.T, entry map[string]any) {
				if entry["keep"] != float64(1) {
					t.Errorf("keep = %v", entry["keep"])
				}
				if _, ok := entry["fn"]; ok {
					t.Error("函数成员不该进日志字段")
				}
				// symbol 键在日志里没有可读名字，不该凭空多出一个字段。
				for k := range entry {
					switch k {
					case "time", "level", "msg", "plugin", "request_id", "keep":
					default:
						t.Errorf("多出了字段 %q", k)
					}
				}
			},
		},
		{
			name:  "嵌套数组",
			field: `{"hops":[["a"],["b"]]}`,
			check: func(t *testing.T, entry map[string]any) {
				hops, ok := entry["hops"].([]any)
				if !ok {
					t.Fatalf("hops = %#v", entry["hops"])
				}
				if len(hops) != 2 {
					t.Fatalf("hops 有 %d 项", len(hops))
				}
				inner, ok := hops[0].([]any)
				if !ok || len(inner) != 1 || inner[0] != "a" {
					t.Errorf("hops[0] = %#v", hops[0])
				}
			},
		},
		{
			name:  "数组上的自定义属性不进序列",
			field: `(() => { const a = [1, 2]; a.extra = "x"; return {a}; })()`,
			check: func(t *testing.T, entry map[string]any) {
				a, ok := entry["a"].([]any)
				if !ok {
					t.Fatalf("a = %#v", entry["a"])
				}
				if len(a) != 2 {
					t.Errorf("数组序列 = %#v，自定义属性不该混进来", a)
				}
			},
		},
		{
			name: "数组里的函数与访问器元素被跳过",
			field: `(() => {
				const a = [1];
				a.push(() => 1);
				Object.defineProperty(a, 2, {get: () => 2, enumerable: true});
				a.length = 3;
				return {a};
			})()`,
			check: func(t *testing.T, entry map[string]any) {
				a, ok := entry["a"].([]any)
				if !ok {
					t.Fatalf("a = %#v", entry["a"])
				}
				if len(a) != 1 || a[0] != float64(1) {
					t.Errorf("数组序列 = %#v，函数与访问器元素都该被跳过", a)
				}
			},
		},
		{
			name:  "BigInt 与 Symbol 成员不记",
			field: `({keep: 1, big: 12n, sym: Symbol("名字")})`,
			check: func(t *testing.T, entry map[string]any) {
				// 这两个没有 JSON 对应物：BigInt 当数字记会悄悄丢精度，
				// symbol 的调试名字不是它的身份。不记比记错好。
				if entry["keep"] != float64(1) {
					t.Errorf("keep = %#v", entry["keep"])
				}
				if _, ok := entry["big"]; ok {
					t.Errorf("big 不该被记下来: %#v", entry["big"])
				}
				if _, ok := entry["sym"]; ok {
					t.Errorf("sym 不该被记下来: %#v", entry["sym"])
				}
			},
		},
		{
			name:  "对象上写的数字键",
			field: `(() => { const o = {}; o["0"] = "零"; return {o}; })()`,
			check: func(t *testing.T, entry map[string]any) {
				o, ok := entry["o"].(map[string]any)
				if !ok {
					t.Fatalf("o = %#v", entry["o"])
				}
				if o["0"] != "零" {
					t.Errorf("数字键丢了: %#v", o)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			env := Env{Logger: testLogger(&buf)}
			fn := capability(t, Standard(env), capLog).Funcs["log"]

			if _, err := fn(context.Background(), js("info", "m", jsEval(t, tc.field))); err != nil {
				t.Fatalf("log 出错: %v", err)
			}
			entries := logEntries(t, &buf)
			if len(entries) != 1 {
				t.Fatalf("写出了 %d 条日志", len(entries))
			}
			tc.check(t, entries[0])
		})
	}
}

// TestLogFieldsKeyLimit 钉住字段数上限：超出部分不再收集，
// 而不是让插件决定宿主要为这份日志花多少内存。
func TestLogFieldsKeyLimit(t *testing.T) {
	var buf bytes.Buffer
	env := Env{Logger: testLogger(&buf)}
	fn := capability(t, Standard(env), capLog).Funcs["log"]

	obj := jsEval(t, `(() => {
		const o = {};
		for (let i = 0; i < 200; i++) o["k" + i] = i;
		return o;
	})()`)

	if _, err := fn(context.Background(), js("info", "m", obj)); err != nil {
		t.Fatalf("log 出错: %v", err)
	}
	entries := logEntries(t, &buf)
	if len(entries) != 1 {
		t.Fatalf("写出了 %d 条日志", len(entries))
	}
	fields := 0
	for k := range entries[0] {
		switch k {
		case "time", "level", "msg", "plugin", "request_id":
		default:
			fields++
		}
	}
	if fields != fieldKeyMax {
		t.Errorf("收集了 %d 个字段，期望上限 %d", fields, fieldKeyMax)
	}
}

// TestLogTruncatesLongFieldValue 钉住**单个值**的字节上限。
//
// 键的个数由 fieldKeyMax 管着，值的长度一直没人管：一个 100 KB 的字符串
// 会原样进日志（实测 1129 字节的日志行里 1022 字节是它）。日志的留存期与
// 传播范围远大于插件本身，所以「插件决定宿主为此花多少内存」这件事
// 在值这一维上同样不能成立。
//
// 用例里的数字是**写死的**：1024 就是 fieldValueMaxBytes 的值。
// 用常量断言等于用常量测常量——把常量改成 1，用例照样通过。
func TestLogTruncatesLongFieldValue(t *testing.T) {
	var buf bytes.Buffer
	env := Env{Logger: testLogger(&buf)}
	fn := capability(t, Standard(env), capLog).Funcs["log"]

	// 多字节字符顺带钉住截断落在 rune 边界上（否则日志里会出现乱码）。
	obj := jsEval(t, `({k: "占".repeat(100000)})`)
	if _, err := fn(context.Background(), js("info", "m", obj)); err != nil {
		t.Fatalf("log 出错: %v", err)
	}
	entries := logEntries(t, &buf)
	got, ok := entries[0]["k"].(string)
	if !ok {
		t.Fatalf("k 应当是字符串，实际 %T", entries[0]["k"])
	}
	if len(got) > 1024 {
		t.Errorf("字段值有 %d 字节，超过 1024", len(got))
	}
	// 反过来也要钉：截到只剩几十字节同样是坏的——那是把日志写没了。
	if len(got) < 900 {
		t.Errorf("字段值只剩 %d 字节，截得太狠", len(got))
	}
	if !strings.HasSuffix(got, truncatedMark) {
		t.Error("截断没有标注：读的人会以为插件只打印了这么多")
	}
	if strings.ContainsRune(got, '�') {
		t.Error("截断落在了 rune 边界之外")
	}
}

// TestLogFieldValueBoundary 钉住上限的边界：正好到上限的原样通过，
// 超一个字节才截断，且截断标记也占预算（截完不超过 1024 字节）。
//
// 只测「很大的值被截了」是不够的——把上限写成 0 也满足那一条。
func TestLogFieldValueBoundary(t *testing.T) {
	cases := []struct {
		name   string
		field  string
		want   int
		marked bool
	}{
		{"正好到上限", `({k: "x".repeat(1024)})`, 1024, false},
		{"超一个字节", `({k: "x".repeat(1025)})`, 1024, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			env := Env{Logger: testLogger(&buf)}
			fn := capability(t, Standard(env), capLog).Funcs["log"]

			if _, err := fn(context.Background(), js("info", "m", jsEval(t, tc.field))); err != nil {
				t.Fatalf("log 出错: %v", err)
			}
			got, _ := logEntries(t, &buf)[0]["k"].(string)
			if len(got) != tc.want {
				t.Errorf("字段值 %d 字节，想要 %d", len(got), tc.want)
			}
			if strings.HasSuffix(got, truncatedMark) != tc.marked {
				t.Errorf("截断标记 = %v，想要 %v: %q", !tc.marked, tc.marked, got[max(0, len(got)-20):])
			}
		})
	}
}

// TestLogBoundsArrayFieldValues 钉住**数组长度**这一维。
//
// 只限单个值是不够的：每个元素都在单值上限之内，而数组长度没有上限。
// 十万个短字符串曾经原样进日志（实测字段渲染后 32705 字节）。
//
// 8192 与 32768 都是写死的，两条都是**上界**：一字节的字符串现在也至少记
// scalarCost，所以实际只收进 1023 个（渲染约 4 KB），离上界还有很远。
// 紧的那两个数由 TestLogBoundsEmptyStringValues 与
// TestLogBoundsWorstCaseRenderedBytes 钉着。
func TestLogBoundsArrayFieldValues(t *testing.T) {
	var buf bytes.Buffer
	env := Env{Logger: testLogger(&buf)}
	fn := capability(t, Standard(env), capLog).Funcs["log"]

	obj := jsEval(t, `({k: Array.from({length: 100000}, () => "x")})`)
	if _, err := fn(context.Background(), js("info", "m", obj)); err != nil {
		t.Fatalf("log 出错: %v", err)
	}
	arr, ok := logEntries(t, &buf)[0]["k"].([]any)
	if !ok {
		t.Fatalf("k 应当是数组，实际 %T", logEntries(t, &buf)[0]["k"])
	}
	if len(arr) > 8192 {
		t.Errorf("数组收进了 %d 个元素，超过 8192", len(arr))
	}
	encoded, err := json.Marshal(arr)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > 32768 {
		t.Errorf("字段渲染后有 %d 字节，超过 32768", len(encoded))
	}
	if len(arr) == 0 {
		t.Error("一个元素都没收进来")
	}
}

// TestLogBoundsScalarFieldValues 是上一条的另一种元素：数字不占字符串的字节数，
// 但它在日志里同样各占一个位置。不记账的话「一百万个数字」这条路径没有界。
//
// 1024 是写死的：预算是 8192 字节，每个非字符串值记 8 字节。
func TestLogBoundsScalarFieldValues(t *testing.T) {
	var buf bytes.Buffer
	env := Env{Logger: testLogger(&buf)}
	fn := capability(t, Standard(env), capLog).Funcs["log"]

	obj := jsEval(t, `({k: Array.from({length: 100000}, (_, i) => i)})`)
	if _, err := fn(context.Background(), js("info", "m", obj)); err != nil {
		t.Fatalf("log 出错: %v", err)
	}
	arr, ok := logEntries(t, &buf)[0]["k"].([]any)
	if !ok {
		t.Fatalf("k 应当是数组，实际 %T", logEntries(t, &buf)[0]["k"])
	}
	if len(arr) > 1024 {
		t.Errorf("数组收进了 %d 个数字，超过 1024", len(arr))
	}
	if len(arr) == 0 {
		t.Error("一个数字都没收进来")
	}
}

// countFields 数一条日志里除固定键之外的字段个数。
func countFields(entry map[string]any) int {
	n := 0
	for k := range entry {
		switch k {
		case "time", "level", "msg", "plugin", "request_id":
		default:
			n++
		}
	}
	return n
}

// TestLogBoundsKeyBytes 钉住**键的字节**也走同一份预算。
//
// 键曾经一个字节都不记，也没有长度上限：一个 1 MB 的键原样进日志
// （实测日志行 1000105 字节）。值与键都是「插件决定宿主为此花多少内存」的
// 入口，而键还额外决定日志行的长度——只记值等于把这一维留着一个洞。
//
// 300 与 26 都是写死的：每个条目记 300（键）+ 8（值，一个数字）= 308 字节，
// 8192 / 308 = 26。**这条用例就是靠这个差分辨「键记账了」与「没记账」**：
// 只记值的话 32 个条目合计 256 字节，32 个会全进（fieldKeyMax 正好是 32，
// 数量上限一个都拦不住）。
func TestLogBoundsKeyBytes(t *testing.T) {
	var buf bytes.Buffer
	env := Env{Logger: testLogger(&buf)}
	fn := capability(t, Standard(env), capLog).Funcs["log"]

	// 键长 300 字节且互不相同（尾三位的序号），于是每个条目都真的占一份预算。
	obj := jsEval(t, `(() => {
		const o = {};
		for (let i = 0; i < 32; i++) o["k".repeat(297) + ("000" + i).slice(-3)] = 1;
		return o;
	})()`)
	if _, err := fn(context.Background(), js("info", "m", obj)); err != nil {
		t.Fatalf("log 出错: %v", err)
	}
	if got := countFields(logEntries(t, &buf)[0]); got != 26 {
		t.Errorf("收进了 %d 个字段，想要 26（每个条目 300 字节的键 + 8 字节的值，8192/308）", got)
	}

	// 单个键就超过整份预算时：整个条目丢掉，不截短键、也不留一个只剩键名的空壳。
	buf.Reset()
	obj = jsEval(t, `({["k".repeat(1000000)]: 1})`)
	if _, err := fn(context.Background(), js("info", "m", obj)); err != nil {
		t.Fatalf("log 出错: %v", err)
	}
	entries := logEntries(t, &buf)
	if got := countFields(entries[0]); got != 0 {
		t.Errorf("1 MB 的键放不下，却收进了 %d 个字段", got)
	}
	t.Logf("1 MB 的键：日志行 %d 字节", buf.Len())
	// 1024 是写死的：修前这一行是 1000105 字节（键原样进日志）。
	if n := buf.Len(); n > 1024 {
		t.Errorf("日志行 %d 字节，超过 1024：键的字节没有过预算", n)
	}
}

// TestLogBoundsEmptyStringValues 钉住**零字节的值**也要记账。
//
// 空字符串的字节数是 0，而 spend(0) 恒真——数组那一支只限了初始容量
// （min(len(keys), 8192/scalarCost)），append 之后长度不受约束，于是一百万个
// 空字符串能一个字节都不花地全进日志（实测日志行 3000106 字节、一次 log 调用
// 在宿主侧多分配约 116 MB）。既有用例用的是 `"x"`，每个元素至少占 1 字节，
// 正好绕开了这一支。
//
// 1024 与 1000 都是写死的：预算 8192 除以每个值至少记的 8 字节是 1024 个，
// 而容器（外层对象 8 + 键 "k" 1 + 数组 8）先扣掉 17，所以实际是 1021 个；
// 下界 1000 挡的是「记账记成了另一个量级」（例如每个值记 1024 字节，
// 那就只收得进 8 个）——只钉上界的话，把每个值记成 8192 也照样通过。
func TestLogBoundsEmptyStringValues(t *testing.T) {
	var buf bytes.Buffer
	env := Env{Logger: testLogger(&buf)}
	fn := capability(t, Standard(env), capLog).Funcs["log"]

	obj := jsEval(t, `({k: new Array(1000000).fill("")})`)
	if _, err := fn(context.Background(), js("info", "m", obj)); err != nil {
		t.Fatalf("log 出错: %v", err)
	}
	arr, ok := logEntries(t, &buf)[0]["k"].([]any)
	if !ok {
		t.Fatalf("k 应当是数组，实际 %T", logEntries(t, &buf)[0]["k"])
	}
	t.Logf("收进 %d 个空字符串，日志行 %d 字节", len(arr), buf.Len())
	if len(arr) > 1024 {
		t.Errorf("数组收进了 %d 个空字符串，超过 1024：零字节的值没有记账", len(arr))
	}
	if len(arr) < 1000 {
		t.Errorf("数组只收进了 %d 个空字符串，记账比名义开销重太多", len(arr))
	}
	// 8192 是写死的：修前这一行是 3000106 字节。
	if n := buf.Len(); n > 8192 {
		t.Errorf("日志行 %d 字节，超过 8192", n)
	}
}

// TestLogBoundsWorstCaseRenderedBytes 钉住**渲染口径**的那个数。
//
// 记账不超过 8192，但记账不等于渲染：数字记 8 字节，而最长的 float64
// （1.7976931348623157e+308）写成 JSON 有 24 字节，加一个逗号是 25——
// 于是整条 fields 渲染出来最多是 25 × 1021 上下（实测 24610 字节）。
//
// 32 KiB 是写死的：它是「这条 fields 最多多少字节」在**日志行**那一侧的答案，
// 与记账口径的 8192 是两个数，两个都要说。
//
// 元素故意用最长的数字而不是 `"x"`：后者一个字节记 8 字节、渲染只有 4 字节，
// **放大方向是反的**，钉不住上界。
func TestLogBoundsWorstCaseRenderedBytes(t *testing.T) {
	var buf bytes.Buffer
	env := Env{Logger: testLogger(&buf)}
	fn := capability(t, Standard(env), capLog).Funcs["log"]

	obj := jsEval(t, `({k: Array.from({length: 1000000}, () => 1.7976931348623157e308)})`)
	if _, err := fn(context.Background(), js("info", "m", obj)); err != nil {
		t.Fatalf("log 出错: %v", err)
	}
	arr, ok := logEntries(t, &buf)[0]["k"].([]any)
	if !ok {
		t.Fatalf("k 应当是数组，实际 %T", logEntries(t, &buf)[0]["k"])
	}
	t.Logf("收进 %d 个最长数字，日志行 %d 字节", len(arr), buf.Len())
	if len(arr) > 1024 {
		t.Errorf("数组收进了 %d 个数字，超过 1024", len(arr))
	}
	if len(arr) < 1000 {
		t.Errorf("数组只收进了 %d 个数字，记账比名义开销重太多", len(arr))
	}
	if n := buf.Len(); n > 32768 {
		t.Errorf("日志行 %d 字节，超过 32768（记账 8192，渲染放大三倍上下）", n)
	}
}

// TestLogFieldBudgetIsSharedAcrossKeys 钉住预算是**整条 fields 共享**的。
//
// 二十个一千字节的值合起来两万字节，每一个都在单值上限之内——
// 只限单值的实现会把它们全部放行。放不下时丢的是排在后面的键，
// 而不是把每个值都截短：一条被截过的 fields 在日志里读起来和完整的一样。
func TestLogFieldBudgetIsSharedAcrossKeys(t *testing.T) {
	var buf bytes.Buffer
	env := Env{Logger: testLogger(&buf)}
	fn := capability(t, Standard(env), capLog).Funcs["log"]

	obj := jsEval(t, `(() => {
		const o = {};
		for (let i = 0; i < 20; i++) o["k" + i] = "y".repeat(1000);
		return o;
	})()`)
	if _, err := fn(context.Background(), js("info", "m", obj)); err != nil {
		t.Fatalf("log 出错: %v", err)
	}
	entries := logEntries(t, &buf)

	total, survived := 0, 0
	for k, v := range entries[0] {
		switch k {
		case "time", "level", "msg", "plugin", "request_id":
			continue
		}
		s, ok := v.(string)
		if !ok {
			t.Fatalf("%s 应当是字符串，实际 %T", k, v)
		}
		total += len(s)
		survived++
	}
	// 8192 是写死的：它就是 fieldTotalMaxBytes 的值。
	if total > 8192 {
		t.Errorf("所有字段值合计 %d 字节，超过 8192", total)
	}
	if survived >= 20 {
		t.Errorf("二十个值全收进来了（合计 %d 字节），预算没有共享", total)
	}
	if survived == 0 {
		t.Fatal("一个字段都没收进来")
	}
	// 排在前面那个必须完好无损：预算是「放不下才丢」，不是「一律截短」。
	if got, _ := entries[0]["k0"].(string); got != strings.Repeat("y", 1000) {
		t.Errorf("k0 被改动了：%d 字节", len(got))
	}
}

// TestLogBoundsContainerPreallocation 钉住「容器不按插件的规模预分配」。
//
// 键的个数有 fieldKeyMax 管着，每个值的大小有 fieldValueMaxBytes 管着，
// 但**容器的预分配**曾经跟着 len(keys) 走：一个一百万元素的 Float64Array
// （JS 侧每元素 8 字节）会让宿主先造一个能装一百万个键的 map，然后才在
// 循环里截到 32 个。写出来的日志行一直是几百字节，差的正是这一份——
// 按默认的 64 MiB 内存上限，一个 8M 元素的数组就能让**一次 log 调用**
// 在宿主侧分配约 800 MB。
//
// 断言的是宿主侧的新分配量，不是输出的大小：TotalAlloc 只增不减，
// 不受 GC 时机影响。上限 64 MiB 是写死的——修好之后这一份只剩「把键
// 物化出来」那一次（实测约 16 MB，每键 16 字节），坏掉时是它的六倍以上。
//
// 数组那一支（`make([]any, 0, len(keys))`）用的是同一个判据，但它的容量
// 与「键物化」是同一量级（都是每元素 16 字节），所以没有单独设阈值；
// 它收进多少元素由 TestLogBoundsArrayFieldValues 钉着。
func TestLogBoundsContainerPreallocation(t *testing.T) {
	var buf bytes.Buffer
	env := Env{Logger: testLogger(&buf)}
	fn := capability(t, Standard(env), capLog).Funcs["log"]

	// Float64Array 走的不是数组那一支：它的 ClassName 不是 "Array"，
	// 于是按普通对象转，容器是 map。
	obj := jsEval(t, `({k: new Float64Array(1000000)})`)

	var 前, 后 runtime.MemStats
	runtime.ReadMemStats(&前)
	if _, err := fn(context.Background(), js("info", "m", obj)); err != nil {
		t.Fatalf("log 出错: %v", err)
	}
	runtime.ReadMemStats(&后)
	分配 := 后.TotalAlloc - 前.TotalAlloc
	t.Logf("一次 log 调用在宿主侧新分配 %d 字节", 分配)

	if 分配 > 64<<20 {
		t.Errorf("一次 log 调用新分配了 %d 字节，超过 64 MiB：容器按插件的规模预分配了", 分配)
	}
	// 上面那个上限不能靠「什么都没转」满足：键还是要收到上限为止。
	got, ok := logEntries(t, &buf)[0]["k"].(map[string]any)
	if !ok {
		t.Fatalf("k 应当是对象，实际 %T", logEntries(t, &buf)[0]["k"])
	}
	if len(got) == 0 || len(got) > fieldKeyMax {
		t.Errorf("收进了 %d 个键，想要 (0, %d]", len(got), fieldKeyMax)
	}
}

func TestLogTruncatesLongMessage(t *testing.T) {
	var buf bytes.Buffer
	env := Env{Logger: testLogger(&buf)}
	fn := capability(t, Standard(env), capLog).Funcs["log"]

	// 用多字节字符，顺带钉住截断落在 rune 边界上（否则日志里会出现乱码）。
	long := strings.Repeat("汉", 3000)
	if _, err := fn(context.Background(), js("info", long)); err != nil {
		t.Fatalf("log 出错: %v", err)
	}

	entries := logEntries(t, &buf)
	got, _ := entries[0]["msg"].(string)
	if len(got) > logMessageMaxBytes+len(truncatedMark) {
		t.Errorf("截断后仍有 %d 字节", len(got))
	}
	if !strings.HasSuffix(got, truncatedMark) {
		t.Errorf("截断没有标注：%q", got[len(got)-16:])
	}
	if strings.ContainsRune(got, '�') {
		t.Errorf("截断落在了 rune 边界之外: %q", got[len(got)-16:])
	}
}

func TestLogShortMessageNotMarked(t *testing.T) {
	var buf bytes.Buffer
	env := Env{Logger: testLogger(&buf)}
	fn := capability(t, Standard(env), capLog).Funcs["log"]

	exact := strings.Repeat("x", logMessageMaxBytes)
	if _, err := fn(context.Background(), js("info", exact)); err != nil {
		t.Fatalf("log 出错: %v", err)
	}
	entries := logEntries(t, &buf)
	got, _ := entries[0]["msg"].(string)
	if strings.Contains(got, truncatedMark) {
		t.Error("正好到上限的 message 不该标注截断")
	}
	if len(got) != logMessageMaxBytes {
		t.Errorf("message 长度 = %d，期望 %d", len(got), logMessageMaxBytes)
	}
}

// TestLogUsesDefaultLoggerWhenNil 确认 Logger 为 nil 时不 panic。
func TestLogUsesDefaultLoggerWhenNil(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(old)

	env := Env{Plugin: "p"}
	if _, err := capability(t, Standard(env), capLog).Funcs["log"](context.Background(), js("info", "m")); err != nil {
		t.Fatalf("log 出错: %v", err)
	}
	if !strings.Contains(buf.String(), "m") {
		t.Errorf("默认日志器没收到日志: %q", buf.String())
	}
}

// TestLogFieldsSurvivesHostileObject 确认读 fields 遇到代理对象时只是忽略读不出来的部分，
// 而不是把整条 log 拖失败。实测引擎对「被撤销的代理」与「ownKeys 抛异常的代理」
// 都只给回一个空对象，所以这里断言的是「不报错、日志照常写出」。
func TestLogFieldsSurvivesHostileObject(t *testing.T) {
	cases := map[string]string{
		"被撤销的代理": `(() => {
			const {proxy, revoke} = Proxy.revocable({a: 1}, {});
			revoke();
			return {keep: 1, proxy};
		})()`,
		"ownKeys 抛异常的代理": `({keep: 1, proxy: new Proxy({a: 1}, {ownKeys() { throw new Error("boom"); }})})`,
		"get 抛异常的代理":     `({keep: 1, proxy: new Proxy({a: 1}, {get() { throw new Error("boom"); }})})`,
	}

	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			obj := jsEval(t, src)
			var buf bytes.Buffer
			env := Env{Logger: testLogger(&buf)}
			fn := capability(t, Standard(env), capLog).Funcs["log"]

			if _, err := fn(context.Background(), js("info", "m", obj)); err != nil {
				t.Fatalf("恶意对象不该让 log 失败: %v", err)
			}
			entries := logEntries(t, &buf)
			if len(entries) != 1 || entries[0]["msg"] != "m" {
				t.Fatalf("日志没写出来: %#v", entries)
			}
			if entries[0]["keep"] != float64(1) {
				t.Errorf("同一对象里的正常字段丢了: %#v", entries[0])
			}
		})
	}
}

// TestArgHelpers 固定取参的边界：缺参数、非法整数都要报错，不静默按 0 处理。
func TestArgHelpers(t *testing.T) {
	cases := []struct {
		name    string
		args    []moejs.Value
		wantErr bool
		want    int64
	}{
		{"正常", js(int64(7)), false, 7},
		{"缺参数", nil, true, 0},
		{"undefined", []moejs.Value{moejs.Undefined()}, true, 0},
		{"null", []moejs.Value{moejs.Null()}, true, 0},
		{"空串", js(""), true, 0},
		{"小数", js("1.5"), true, 0},
		{"非数字", js("不是时间戳"), true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := argInt64(tc.args, 0, "时间戳")
			if (err != nil) != tc.wantErr {
				t.Fatalf("argInt64 错误 = %v，期望报错 %v", err, tc.wantErr)
			}
			if err == nil && got != tc.want {
				t.Errorf("argInt64 = %d，期望 %d", got, tc.want)
			}
		})
	}

	if _, err := argInt64Or(nil, 2, "ttlSeconds", 42); err != nil {
		t.Errorf("可缺省的参数不该在缺失时报错: %v", err)
	}
	if got, _ := argInt64Or(nil, 2, "ttlSeconds", 42); got != 42 {
		t.Errorf("缺省值 = %d，期望 42", got)
	}
	if _, err := argInt64Or(js("x"), 0, "ttlSeconds", 1); err == nil {
		t.Error("显式传了一个非整数时应当报错，而不是用缺省值")
	}
	if got := argString(js("k"), 0); got != "k" {
		t.Errorf("argString = %q", got)
	}
	if got := argString(nil, 0); got != "" {
		t.Errorf("缺参数时 argString = %q，期望空串", got)
	}
}
