package plugin

import (
	"encoding/json"
	"errors"
	"sort"
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
