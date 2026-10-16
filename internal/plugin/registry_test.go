package plugin

import (
	"errors"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/plugin/sandbox"
)

// src 生成一段插件源码，meta 是原样插入的 JS 片段。
func src(meta, hooks string) string {
	return "import { definePlugin } from \"larkax\";\n" +
		"export const meta = {" + meta + "};\n" +
		"export default definePlugin({" + hooks + "});\n"
}

const minimalMeta = `key: "p1", apiVersion: 1,`

func mustPublish(t *testing.T, regs ...Registration) *Registry {
	t.Helper()
	reg, err := Publish(regs)
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	return reg
}

// ── 元数据校验 ────────────────────────────────────────────────────────

func TestCompileValidatesMeta(t *testing.T) {
	cases := []struct {
		name string
		reg  Registration
		want error
	}{
		{
			name: "缺少 apiVersion",
			reg:  Registration{Key: "p1", Source: src(`key:"p1", name:"P"`, `onUsage(){return {promptTokens:1}}`)},
			want: ErrMetaAPIVersion,
		},
		{
			name: "契约版本高于宿主支持",
			reg:  Registration{Key: "p1", Source: src(`key:"p1", apiVersion:2`, `onUsage(){return {promptTokens:1}}`)},
			want: ErrMetaAPIVersion,
		},
		{
			name: "注册名与 meta.key 不一致",
			reg:  Registration{Key: "别的名字", Source: src(minimalMeta, `onUsage(){return {promptTokens:1}}`)},
			want: nil, // 这类错误没有哨兵，只看有没有错
		},
		{
			name: "申请了未开放的能力",
			reg: Registration{Key: "p1", Source: src(
				minimalMeta+` capabilities:["net"]`, `onUsage(){return {promptTokens:1}}`)},
			want: ErrMetaCapability,
		},
		{
			name: "元数据里写了未知字段",
			reg: Registration{Key: "p1", Source: src(
				minimalMeta+` maxTokens: 5`, `onUsage(){return {promptTokens:1}}`)},
			want: nil,
		},
		{
			name: "没有 meta",
			reg:  Registration{Key: "p1", Source: "export default { onUsage(){ return {}; } };"},
			want: ErrMetaMissingKey,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compile(tc.reg)
			if tc.want == nil {
				if err == nil {
					t.Fatal("本该报错，实际通过了")
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v，期望 errors.Is(…, %v)", err, tc.want)
			}
		})
	}
}

// 能力申请只能收窄：不写就拿到全部默认能力，写了就是那个子集。
func TestCapabilitiesNarrowing(t *testing.T) {
	reg := mustPublish(t,
		Registration{Key: "p1", Source: src(minimalMeta, `onUsage(){return {promptTokens:1}}`)},
		Registration{Key: "one", Source: src(
			`key:"one", apiVersion:1, capabilities:["log"]`, `onUsage(){return {promptTokens:1}}`)},
	)
	p, _ := reg.Get("p1")
	if strings.Join(p.Capabilities, ",") != "calendar,kv,log" {
		t.Errorf("不写 capabilities 应拿到全部默认能力，实际 %v", p.Capabilities)
	}
	p, _ = reg.Get("one")
	if strings.Join(p.Capabilities, ",") != "log" {
		t.Errorf("写了 capabilities 应只拿到那一个，实际 %v", p.Capabilities)
	}
}

// ── 发布 ──────────────────────────────────────────────────────────────

// 一批里有任何一个编译不过，整批都不发布。
//
// 「一半生效」在界面上看起来与「全都生效」一样，所以宁可一个都不上。
func TestPublishIsAllOrNothing(t *testing.T) {
	reg, err := Publish([]Registration{
		{Key: "好的", Source: src(`key:"好的", apiVersion:1`, `onUsage(){return {promptTokens:1}}`)},
		{Key: "坏的", Source: "这是语法错误 ((((("},
	})
	if err == nil {
		t.Fatal("有编译不过的插件时本该报错")
	}
	if reg != nil {
		t.Errorf("失败时不该返回注册表，实际 %v", reg)
	}
	if !strings.Contains(err.Error(), "坏的") {
		t.Errorf("错误里该点名是哪个插件，实际: %v", err)
	}
}

func TestPublishRejectsDuplicateKeys(t *testing.T) {
	_, err := Publish([]Registration{
		{Key: "dup", Source: src(`key:"dup", apiVersion:1`, `onUsage(){return {promptTokens:1}}`)},
		{Key: "dup", Source: src(`key:"dup", apiVersion:1`, `onRequest(){return {}}`)},
	})
	if err == nil || !strings.Contains(err.Error(), "两次") {
		t.Fatalf("重复 key 本该报错，实际 %v", err)
	}
}

// 优先级降序、同优先级按 key 升序。后者是「同样的注册集合每次选出同一个插件」
// 的唯一依据——不排序的话候选顺序取决于 map 遍历，那是随机的。
func TestPublishOrdersDeterministically(t *testing.T) {
	reg := mustPublish(t,
		Registration{Key: "b", Priority: 1, Source: src(`key:"b", apiVersion:1`, `onUsage(){return {promptTokens:1}}`)},
		Registration{Key: "a", Priority: 1, Source: src(`key:"a", apiVersion:1`, `onUsage(){return {promptTokens:1}}`)},
		Registration{Key: "z", Priority: 5, Source: src(`key:"z", apiVersion:1`, `onUsage(){return {promptTokens:1}}`)},
	)
	var keys []string
	for _, p := range reg.Plugins() {
		keys = append(keys, p.Key)
	}
	if strings.Join(keys, ",") != "z,a,b" {
		t.Errorf("顺序 = %v，期望 [z a b]（优先级降序、同优先级按 key 升序）", keys)
	}
}

// ── 匹配 ──────────────────────────────────────────────────────────────

func TestCandidatesMatchOnModelAndProtocol(t *testing.T) {
	reg := mustPublish(t,
		Registration{Key: "全部", Source: src(`key:"全部", apiVersion:1`, `onUsage(){return {promptTokens:1}}`)},
		Registration{Key: "只有A", Source: src(
			`key:"只有A", apiVersion:1, models:["model-a"]`, `onUsage(){return {promptTokens:1}}`)},
		Registration{Key: "只有Anthropic", Source: src(
			`key:"只有Anthropic", apiVersion:1, protocol:"anthropic"`, `onUsage(){return {promptTokens:1}}`)},
	)
	names := func(ps []*Plugin) string {
		var out []string
		for _, p := range ps {
			out = append(out, p.Key)
		}
		return strings.Join(out, ",")
	}

	// models 为空表示「所有模型」：一个平台级插件没法穷举模型名。
	if got := names(reg.Candidates("model-a", "openai")); got != "全部,只有A" {
		t.Errorf("model-a/openai 候选 = %q", got)
	}
	if got := names(reg.Candidates("model-b", "openai")); got != "全部" {
		t.Errorf("model-b/openai 候选 = %q", got)
	}
	if got := names(reg.Candidates("model-b", "anthropic")); got != "全部,只有Anthropic" {
		t.Errorf("model-b/anthropic 候选 = %q", got)
	}
	// 匹配只看声明，不看请求内容——后者会让同一模型的两条请求跑到不同插件上。
	if got := names(Empty().Candidates("model-a", "openai")); got != "" {
		t.Errorf("空注册表不该有候选，实际 %q", got)
	}
}

// ── 会话 ──────────────────────────────────────────────────────────────

// 一次请求只跑一个插件。多插件串联的语义未定，收窄是可逆的那一侧。
func TestSessionPicksOnePlugin(t *testing.T) {
	reg := mustPublish(t,
		Registration{Key: "低", Priority: 1, Source: src(`key:"低", apiVersion:1`, `onUsage(){return {promptTokens:1}}`)},
		Registration{Key: "高", Priority: 9, Source: src(`key:"高", apiVersion:1`, `onUsage(){return {promptTokens:1}}`)},
	)
	s, err := reg.NewSession(t.Context(), SessionRequest{CanonicalModel: "m", Protocol: "openai"},
		sandbox.Host{}, sandbox.Limits{})
	if err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	defer s.Close()
	if s.Plugin.Key != "高" {
		t.Errorf("该选优先级最高的「高」，实际 %q", s.Plugin.Key)
	}
	if len(s.Shadowed) != 1 || s.Shadowed[0].Key != "低" {
		t.Errorf("被让位的插件应被记录下来（否则「配了三个为什么只跑一个」无从回答），实际 %v", s.Shadowed)
	}
}

func TestNewSessionWithoutMatch(t *testing.T) {
	reg := mustPublish(t, Registration{Key: "p1", Source: src(
		`key:"p1", apiVersion:1, models:["别的模型"]`, `onUsage(){return {promptTokens:1}}`)})
	s, err := reg.NewSession(t.Context(), SessionRequest{CanonicalModel: "m", Protocol: "openai"},
		sandbox.Host{}, sandbox.Limits{})
	if !errors.Is(err, ErrNoPlugin) {
		t.Fatalf("没有匹配的插件时该返回 ErrNoPlugin，实际 %v", err)
	}
	if s != nil {
		t.Errorf("失败时不该返回会话，实际 %v", s)
	}
}

// 编译就拒掉一个 hook 都没有的插件：它会被注册进来、每次请求跑一遍、
// 然后什么都不做，没人看得出来。
func TestCompileRejectsPluginWithoutHooks(t *testing.T) {
	_, err := Compile(Registration{Key: "p1",
		Source: src(minimalMeta, `notAHook(){ return 1; }`)})
	if err == nil || !strings.Contains(err.Error(), "没有导出任何 hook") {
		t.Fatalf("err = %v，期望「没有导出任何 hook」", err)
	}
}

// 只导出部分 hook 是正常的，不是错误。
func TestCompileAcceptsPartialHooks(t *testing.T) {
	p, err := Compile(Registration{Key: "p1",
		Source: src(minimalMeta, `onUsage(){ return {promptTokens:1}; }`)})
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if strings.Join(p.Module.Hooks(), ",") != "onUsage" {
		t.Errorf("Hooks() = %v，期望只有 onUsage", p.Module.Hooks())
	}
}
