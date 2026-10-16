package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
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

// TestCancelledContextPassesCauseThrough 确认客户端断开（取消而非超时）时
// 报的是取消本身，而不是被归到宿主超时里——两者的处置不同。
func TestCancelledContextPassesCauseThrough(t *testing.T) {
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
			got, err := argInt64(tc.args, 0)
			if (err != nil) != tc.wantErr {
				t.Fatalf("argInt64 错误 = %v，期望报错 %v", err, tc.wantErr)
			}
			if err == nil && got != tc.want {
				t.Errorf("argInt64 = %d，期望 %d", got, tc.want)
			}
		})
	}

	if _, err := argInt64Or(nil, 2, 42); err != nil {
		t.Errorf("可缺省的参数不该在缺失时报错: %v", err)
	}
	if got, _ := argInt64Or(nil, 2, 42); got != 42 {
		t.Errorf("缺省值 = %d，期望 42", got)
	}
	if _, err := argInt64Or(js("x"), 0, 1); err == nil {
		t.Error("显式传了一个非整数时应当报错，而不是用缺省值")
	}
	if got := argString(js("k"), 0); got != "k" {
		t.Errorf("argString = %q", got)
	}
	if got := argString(nil, 0); got != "" {
		t.Errorf("缺参数时 argString = %q，期望空串", got)
	}
}
