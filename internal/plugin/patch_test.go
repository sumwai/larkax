package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
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

// 白名单里的字段**必须**放行。漏掉一个的表现是「插件改了但没生效」，
// 而作者会以为是上游不支持。
func TestBodyPatchAcceptsEveryWhitelistedField(t *testing.T) {
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
		t.Fatalf("这些字段都该放行，出错: %v", err)
	}
	// body 里带着客户端的 max_tokens：补丁里的 128 是把它**调小**。
	// 原值取得更大是有意的——「只许调小」那条规则有自己的用例，
	// 这条测的是白名单上的字段一个都没漏。
	body := map[string]any{"model": "canonical-name", "stream": true, "max_tokens": 256}
	out, changed, err := ApplyRequestBodyPatch(body, patch)
	if err != nil {
		t.Fatalf("应用补丁出错: %v", err)
	}
	for _, f := range patchableBodyFields {
		if _, ok := out[f]; !ok {
			t.Errorf("字段 %s 应当被写入，实际没有", f)
		}
	}
	if len(changed) != len(patchableBodyFields) {
		t.Errorf("改动字段数 = %d，期望 %d：%v", len(changed), len(patchableBodyFields), changed)
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

// 合并是浅合并：给 messages 就整块替换，不是往原数组里追加。
// 这条钉住语义，免得有人以为它能「只改某一条消息」。
func TestBodyPatchReplacesSliceWholesale(t *testing.T) {
	patch, _ := unmarshalPatch(`{"messages":[{"role":"user","content":"新"}]}`)
	out, _, err := ApplyRequestBodyPatch(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "旧"}},
	}, patch)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	msgs, ok := out["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("messages 应是替换后的 1 条，实际 %v", out["messages"])
	}
	if got := msgs[0].(map[string]any)["content"]; got != "新" {
		t.Errorf("message 内容是 %v，期望「新」", got)
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
