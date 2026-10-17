package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// unmarshalPatch 走**插件返回值的真实解析路径**：插件的返回值由 sandbox 用
// encoding/json 解进 RequestBodyPatch，所以这里也用它。不另写一个包装函数——
// 包装一层就多一处「校验写在哪一份里」的分叉。
func unmarshalPatch(raw string) (*RequestBodyPatch, error) {
	var patch RequestBodyPatch
	if err := json.Unmarshal([]byte(raw), &patch); err != nil {
		return nil, err
	}
	return &patch, nil
}

// ── 请求体补丁的白名单 ─────────────────────────────────────────────────

// 这些字段**必须**进不来。它们各自对应一条会改价或改身份的约束，
// 所以「改不了」是靠类型里没有它们实现的，这条测试钉住这一点。
func TestBodyPatchRejectsOutOfScopeFields(t *testing.T) {
	cases := map[string]string{
		"model 改价":        `{"model":"更贵的模型"}`,
		"stream 改形态":      `{"stream":true}`,
		"user 改身份":        `{"user":"别人"}`,
		"任意的上游特有参数":       `{"some_upstream_flag":1}`,
		"驼峰写法的 maxTokens": `{"maxTokens":10}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := unmarshalPatch(raw)
			if !errors.Is(err, ErrPatchScope) {
				t.Fatalf("本该被白名单拒掉，实际 err=%v", err)
			}
		})
	}
}

// 契约认得的 8 个字段**都要能解出来**——解析这一层放行它们，拒绝发生在应用
// 那一步（见 TestBodyPatchRejectsUnappliedFields）。
//
// 为什么不在解析这一层就把那 5 个拒掉：拒在应用那一步，报错才说得清是
// 「这个字段不会被应用」，而不是把它当成一个不认识的字段名（那与
// `maxTokens` 拼错是同一句话，而作者要做的事不一样：一个改字段名，
// 一个把这个字段从补丁里去掉）。另外 `{ body: { messages: [...] } }` 必须
// 仍然算「这份补丁改了东西」（见 RequestBodyPatch.empty），否则它会被当成
// 空补丁跳过，拒绝就永远不触发。
func TestBodyPatchDecodesEveryContractField(t *testing.T) {
	raw := `{
		"messages": [{"role":"user","content":"x"}],
		"system": "你是助手",
		"max_tokens": 128,
		"temperature": 0.5,
		"top_p": 0.9,
		"stop": ["\n\n"],
		"tools": [{"name":"t"}],
		"tool_choice": "auto"
	}`
	patch, err := unmarshalPatch(raw)
	if err != nil {
		t.Fatalf("这些字段都该解得出来，出错: %v", err)
	}
	// 逐个判，不只看「没报错」：少解出一个字段的表现是它变成 nil，
	// 而 nil 在应用那一步是「没给」——于是补丁静默少了一项。
	switch {
	case patch.Messages == nil:
		t.Error("messages 没解出来")
	case patch.System == nil:
		t.Error("system 没解出来")
	case patch.MaxTokens == nil:
		t.Error("max_tokens 没解出来")
	case patch.Temperature == nil:
		t.Error("temperature 没解出来")
	case patch.TopP == nil:
		t.Error("top_p 没解出来")
	case patch.Stop == nil:
		t.Error("stop 没解出来")
	case patch.Tools == nil:
		t.Error("tools 没解出来")
	case patch.ToolChoice == nil:
		t.Error("tool_choice 没解出来")
	}
	// 这一份补丁不是空的：8 个字段里有一个给了就算改过东西。
	if patch.empty() {
		t.Error("给了 8 个字段的补丁不该算空补丁")
	}
}

// 会应用的三个字段必须真的写下去。
//
// 这一条与下面那条（5 个被拒）是一对：只证明「该拒的拒了」是不够的，
// 把整条路拦死也满足那一句。所以这里逐个确认 appliedBodyFields 每一项
// 都进了请求体、都出现在改动清单里。
func TestBodyPatchAppliesScalarFields(t *testing.T) {
	patch, err := unmarshalPatch(`{"max_tokens":128,"temperature":0.5,"top_p":0.9}`)
	if err != nil {
		t.Fatalf("解析出错: %v", err)
	}
	// body 里带着客户端的 max_tokens：补丁里的 128 是把它**调小**。
	// 原值取得更大是有意的——「只许调小」那条规则有自己的用例，
	// 这条测的是会应用的字段一个都没漏。
	body := map[string]any{"model": "canonical-name", "stream": true, "max_tokens": 256}
	out, changed, err := ApplyRequestBodyPatch(body, patch)
	if err != nil {
		t.Fatalf("应用补丁出错: %v", err)
	}
	for _, f := range appliedBodyFields {
		if _, ok := out[f]; !ok {
			t.Errorf("字段 %s 应当被写入，实际没有", f)
		}
	}
	if len(changed) != len(appliedBodyFields) {
		t.Errorf("改动字段数 = %d，期望 %d：%v", len(changed), len(appliedBodyFields), changed)
	}
	// 没有出现在补丁里的字段保持原样。
	if out["model"] != "canonical-name" || out["stream"] != true {
		t.Errorf("插件不该能改动 model 与 stream，实际 %v", out)
	}
	// 不改动入参：上游请求体可能已经被别处引用（快照），就地改会让
	// 「当时发出去的是什么」这个问题的答案随时间变化。
	if _, ok := body["temperature"]; ok {
		t.Errorf("入参 map 被就地修改了: %v", body)
	}
}

// 认得但不应用的 5 个字段**逐个**被拒，报错里点出是哪一个。
//
// 表驱动而不是一份大补丁：一份含全部 5 个字段的补丁只能证明「报了错」，
// 证明不了 5 个字段各自都进了判定——漏判一个的表现是那一份补丁被静默应用，
// 而其余 4 个照样报错，整体看着是好的（见 LESSONS.md 第 30 条）。
func TestBodyPatchRejectsUnappliedFields(t *testing.T) {
	cases := map[string]string{
		"messages":    `{"messages":[{"role":"user","content":"改一下"}]}`,
		"system":      `{"system":"你是助手"}`,
		"stop":        `{"stop":["\n\n"]}`,
		"tools":       `{"tools":[{"name":"t"}]}`,
		"tool_choice": `{"tool_choice":"auto"}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			// 解析这一层必须放行：拒在应用那一步（见上面那条测试的注释）。
			patch, err := unmarshalPatch(raw)
			if err != nil {
				t.Fatalf("解析 %s 不该出错，实际 %v", name, err)
			}
			out, changed, err := ApplyRequestBodyPatch(map[string]any{"model": "m"}, patch)
			if !errors.Is(err, ErrPatchScope) {
				t.Fatalf("%s 该被拒，实际 err=%v", name, err)
			}
			// 报错要点出字段名：只说「不符合契约」的话，作者手上那份补丁里
			// 有 8 个字段可选，他得逐个试。
			if !strings.Contains(err.Error(), name) {
				t.Errorf("报错该点出 %s，实际 %q", name, err)
			}
			// 拒掉时什么都不改：不给请求体，也不给改动清单。
			if out != nil || changed != nil {
				t.Errorf("拒掉时不该有产物，实际 out=%v changed=%v", out, changed)
			}
		})
	}
}

// 被拒的字段与被应用的字段混在一起：**整份补丁都不生效**。
//
// 理由与 patchOf 一致：部分应用过的补丁是没法解释的状态，作者看到的会是
// 「有的字段生效了、有的没有」——比整个不生效难查得多。Session.ApplyRequest
// 对 ErrPatchScope 的处置也是整份丢掉，两边同一个语义。
func TestBodyPatchRejectsMixedPatchWholesale(t *testing.T) {
	patch, err := unmarshalPatch(`{"temperature":0.1,"messages":[{"role":"user"}],"tools":[]}`)
	if err != nil {
		t.Fatalf("解析出错: %v", err)
	}
	body := map[string]any{"model": "m", "temperature": 0.9}
	out, changed, err := ApplyRequestBodyPatch(body, patch)
	if !errors.Is(err, ErrPatchScope) {
		t.Fatalf("混了被拒字段的补丁该被拒，实际 err=%v", err)
	}
	// 两个被拒的字段都要点出来，按契约顺序（tools 在 messages 之后）。
	// 只报一个的话，作者改完再跑一次才知道还有第二个。
	if !strings.Contains(err.Error(), "messages、tools") {
		t.Errorf("报错该按契约顺序点出两个字段，实际 %q", err)
	}
	if out != nil || changed != nil {
		t.Errorf("整份不生效：不该有产物，实际 out=%v changed=%v", out, changed)
	}
	if body["temperature"] != 0.9 {
		t.Errorf("入参不该被改动，实际 %v", body)
	}
}

// 字段表与契约对得上：RequestBodyPatch 每加一个字段，都必须在这里表态
// （会应用，还是拒掉）。
//
// 为什么要有这一条：#306 那个缺陷的形状是「字段进了契约、没人应用它、
// 也没人报错」，而**加字段**正是同一个形状的入口——往结构体里加一个字段，
// 编译、测试、三道门禁都不会有任何反应，于是它静默地不被应用。
// 判据是**并集**：unappliedFields 认出来的 5 个 + appliedBodyFields 的 3 个，
// 必须与 RequestBodyPatch 的 json 标签一字不差。
// 少一个名字 = 有人加了字段没表态；多一个名字 = 有人删了字段没收拾。
func TestBodyPatchFieldTableMatchesContract(t *testing.T) {
	// 每个字段都给上值：unappliedFields 判的是「给了没有」。
	system := "s"
	all := &RequestBodyPatch{
		Messages:    []any{},
		System:      &system,
		MaxTokens:   ptrInt64(1),
		Temperature: ptrFloat64(1),
		TopP:        ptrFloat64(1),
		Stop:        []string{},
		Tools:       []any{},
		ToolChoice:  "auto",
	}
	got := append(all.unappliedFields(), appliedBodyFields...)
	sort.Strings(got)
	want := contractFieldNames(&RequestBodyPatch{})
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("字段表与契约对不上：\n处置过的是 %v\n契约里的是 %v\n"+
			"（加了字段就来这里表态：会应用就进 appliedBodyFields，不会就进 unappliedFields）", got, want)
	}
}

// 「没给」与「给了零值」必须能分开。温度改 0 是合法意图（要确定性输出），
// 而不改温度是另一件事；float64 的零值让两者无法区分，所以字段用指针。
func TestBodyPatchDistinguishesUnsetFromZero(t *testing.T) {
	patch, err := unmarshalPatch(`{"temperature":0,"top_p":0}`)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if patch.Temperature == nil || *patch.Temperature != 0 {
		t.Fatalf("temperature 应被识别为「给了 0」，实际 %s", showPtr(patch.Temperature))
	}
	out, changed, err := ApplyRequestBodyPatch(map[string]any{"temperature": 0.9}, patch)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if out["temperature"] != float64(0) {
		t.Errorf("temperature 应被改成 0，实际 %v", out["temperature"])
	}
	// 只给了两个字段，改动清单就只该有两项。
	if len(changed) != 2 {
		t.Errorf("改动字段 = %v，期望 2 项", changed)
	}
	if patch.MaxTokens != nil {
		t.Errorf("没给的字段应为 nil，实际 %v", *patch.MaxTokens)
	}
}

// max_tokens 是计费预留上界的输入，0 与负数都算不出上界。
// 拦在这里而不是留给 pipeline 报错：pipeline 的报错发生在补丁已经应用之后，
// 那时「是谁把它改成 0 的」已经查不出来了。
func TestBodyPatchRejectsNonPositiveMaxTokens(t *testing.T) {
	for _, raw := range []string{`{"max_tokens":0}`, `{"max_tokens":-1}`} {
		patch, err := unmarshalPatch(raw)
		if err != nil {
			t.Fatalf("解析 %s 出错: %v", raw, err)
		}
		_, _, err = ApplyRequestBodyPatch(nil, patch)
		if !errors.Is(err, ErrPatchScope) {
			t.Errorf("%s：应当被拒，实际 err=%v", raw, err)
		}
	}
}

// max_tokens 只许调小。
//
// 上界与上报值由同一方控制时，两者一起放大就等于没有上界：插件把
// max_tokens 调大，预留跟着涨，于是它报多少用量就收多少钱。压短输出是策略
// 插件的正当用法，所以它留在白名单里；抬高不是，所以在合并这一步拒掉。
//
// 三个数（原值、补丁值、上界）里只要有一个错了，这条用例就会挂：
// 把比较反过来、把「原值缺失」当成放行、把两个数从报错文本里去掉，
// 都会命中。
func TestBodyPatchMaxTokensOnlyShrinks(t *testing.T) {
	cases := []struct {
		name    string
		body    map[string]any
		patched int64
		// wantText 非空表示该被拒，且报错里要出现这句话；原值读不出来时
		// 说的不是「只许调小」而是「读不出来」——那是另一种拒法。
		wantText string
		// wantOrig 非空表示报错里还要出现原值这个数。
		wantOrig string
	}{
		{"调小放行", map[string]any{"max_tokens": 100}, 40, "", ""},
		{"持平放行", map[string]any{"max_tokens": 100}, 100, "", ""},
		{"调大被拒", map[string]any{"max_tokens": 100}, 101, "只许调小", "100"},
		{"客户端没给 max_tokens 时补丁给一个也算调大", map[string]any{"model": "m"}, 10, "只许调小", ""},
		{"原值是 null 时同上", map[string]any{"max_tokens": nil}, 10, "只许调小", ""},
		{"原值读不出来时拒收", map[string]any{"max_tokens": "100"}, 10, "读不出来", ""},
		{"原值是小数时拒收", map[string]any{"max_tokens": 100.5}, 10, "读不出来", ""},
		{"原值是 int64 时照常比", map[string]any{"max_tokens": int64(100)}, 101, "只许调小", "100"},
		{"原值是 float64 的整数值时照常比", map[string]any{"max_tokens": float64(100)}, 101, "只许调小", "100"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			patch, err := unmarshalPatch(fmt.Sprintf(`{"max_tokens":%d}`, tc.patched))
			if err != nil {
				t.Fatalf("解析出错: %v", err)
			}
			out, changed, err := ApplyRequestBodyPatch(tc.body, patch)
			if tc.wantText != "" {
				if !errors.Is(err, ErrPatchScope) {
					t.Fatalf("本该被拒，实际 err=%v", err)
				}
				msg := err.Error()
				if !strings.Contains(msg, tc.wantText) {
					t.Errorf("报错里该出现 %q，实际 %q", tc.wantText, msg)
				}
				// 补丁给的值与（能读出来时的）原值都要打出来：
				// 只说「只许调小」看不出它想改成多少、原来是多少。
				if !strings.Contains(msg, strconv.FormatInt(tc.patched, 10)) {
					t.Errorf("报错该带上补丁给的值 %d，实际 %q", tc.patched, msg)
				}
				if tc.wantOrig != "" && !strings.Contains(msg, tc.wantOrig) {
					t.Errorf("报错该带上原值 %s，实际 %q", tc.wantOrig, msg)
				}
				// 拒掉时什么都不改：不改请求体，也不给改动清单。
				if out != nil || changed != nil {
					t.Errorf("拒掉时不该有产物，实际 out=%v changed=%v", out, changed)
				}
				return
			}
			if err != nil {
				t.Fatalf("出错: %v", err)
			}
			if got := out["max_tokens"]; got != tc.patched {
				t.Errorf("max_tokens = %v，期望 %d", got, tc.patched)
			}
			if len(changed) != 1 || changed[0] != "max_tokens" {
				t.Errorf("改动清单 = %v，期望 [max_tokens]", changed)
			}
			// 原值那几项不能被顺手改掉。
			if v, ok := tc.body["model"]; ok && out["model"] != v {
				t.Errorf("model 被改动了: %v", out)
			}
		})
	}
}

// 空补丁与 nil 补丁都不能改动任何东西。
func TestBodyPatchEmptyChangesNothing(t *testing.T) {
	for name, patch := range map[string]*RequestBodyPatch{"nil": nil, "空对象": {}} {
		body := map[string]any{"model": "m"}
		out, changed, err := ApplyRequestBodyPatch(body, patch)
		if err != nil {
			t.Fatalf("%s 出错: %v", name, err)
		}
		if len(changed) != 0 {
			t.Errorf("%s 不该有改动，实际 %v", name, changed)
		}
		if out["model"] != "m" {
			t.Errorf("%s 改动了请求体: %v", name, out)
		}
	}
}

// 被拒的字段连入参都不碰：补丁整个不生效，客户端的请求体一个字都不该变。
//
// 这里用的是 `messages`（一条被拒的结构化字段），因为「整块替换」这条语义
// 最容易被误读成「往原数组里追加」。今天会应用的三个字段都是标量，
// 所以这条用例钉的是拒绝那一支：原数组还在原处，且没有产物。
func TestBodyPatchLeavesOriginalUntouchedOnReject(t *testing.T) {
	patch, err := unmarshalPatch(`{"messages":[{"role":"user","content":"新"}]}`)
	if err != nil {
		t.Fatalf("解析出错: %v", err)
	}
	body := map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "旧"}},
	}
	out, changed, err := ApplyRequestBodyPatch(body, patch)
	if !errors.Is(err, ErrPatchScope) {
		t.Fatalf("messages 该被拒，实际 err=%v", err)
	}
	if out != nil || changed != nil {
		t.Errorf("拒掉时不该有产物，实际 out=%v changed=%v", out, changed)
	}
	msgs, ok := body["messages"].([]any)
	if !ok || len(msgs) != 1 || msgs[0].(map[string]any)["content"] != "旧" {
		t.Errorf("入参的 messages 被改动了: %v", body["messages"])
	}
}

// body 为 nil 时也要能建出请求体——插件给的是「新增一个字段」这种情况。
func TestBodyPatchOnNilBody(t *testing.T) {
	patch, _ := unmarshalPatch(`{"temperature":0.2}`)
	out, changed, err := ApplyRequestBodyPatch(nil, patch)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if out["temperature"] != 0.2 || len(changed) != 1 {
		t.Errorf("out=%v changed=%v", out, changed)
	}
}

// ── 请求头 ────────────────────────────────────────────────────────────

// 被拒的头必须**报出来**，而不是静默丢掉：插件作者写了一个被拒的头
// 却没有任何反馈，会以为是上游不支持。
func TestHeaderPatchReportsRejected(t *testing.T) {
	got, rejected := ApplyHeaderPatch(nil, map[string]string{
		"x-larkax-tenant": "t1",
		"authorization":   "Bearer 冒充",
		"x-custom":        "v",
	})
	if len(got) != 1 || got["x-larkax-tenant"] != "t1" {
		t.Errorf("只该放行 x-larkax- 前缀的那个，实际 %v", got)
	}
	if len(rejected) != 2 {
		t.Fatalf("被拒的应有 2 个，实际 %v", rejected)
	}
	// 排序过，便于日志比对。
	if !strings.EqualFold(rejected[0], "authorization") || !strings.EqualFold(rejected[1], "x-custom") {
		t.Errorf("被拒清单未按序: %v", rejected)
	}
}

// 值里的控制字符与换行是同一类问题，只挡 CR/LF 会漏掉一半。
//
// 被漏掉的那些会一路走到 net/http 才报错，那时它表现为**上游失败**——
// 可能触发换渠道重试，而真正的原因是插件写了一个坏头。
// 同时钉住反面：HTAB 是 RFC 7230 允许的，别把它一起拒了。
func TestHeaderPatchRejectsControlCharsInValue(t *testing.T) {
	got, rejected := ApplyHeaderPatch(nil, map[string]string{
		"x-larkax-nul": "a\x00b",
		"x-larkax-vt":  "a\x0bb",
		"x-larkax-del": "a\x7fb",
		"x-larkax-tab": "a\tb",
		"x-larkax-ok":  "a b",
	})
	if len(rejected) != 3 {
		t.Errorf("带控制字符的三个头都该被拒，实际被拒 %v", rejected)
	}
	if got["x-larkax-tab"] != "a\tb" {
		t.Errorf("HTAB 是 RFC 允许的，不该被拒: %v", got)
	}
	if got["x-larkax-ok"] != "a b" {
		t.Errorf("普通值不该被拒: %v", got)
	}
	if len(got) != 2 {
		t.Errorf("只该放行两个，实际 %v", got)
	}
}

// 已有的头保留，补丁只做叠加。基准为 nil 时不 panic 且返回 nil。
func TestHeaderPatchMergesOntoBase(t *testing.T) {
	base := map[string]string{"X-Larkax-Host": "宿主写的"}
	got, rejected := ApplyHeaderPatch(base, map[string]string{"x-larkax-p": "p"})
	if len(rejected) != 0 {
		t.Errorf("不该有被拒的: %v", rejected)
	}
	if got["X-Larkax-Host"] != "宿主写的" || got["x-larkax-p"] != "p" {
		t.Errorf("合并结果不对: %v", got)
	}
	empty, rejected := ApplyHeaderPatch(nil, nil)
	if empty != nil || rejected != nil {
		t.Errorf("空输入应返回 nil, nil，实际 %v %v", empty, rejected)
	}
}
