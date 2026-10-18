package snapshot

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// 这一组测试遍历允许表，给**每一条**条目一处探针：要么断言真实取值落进了骨架
// （放行），要么断言取值没落进去而是留下了形状与路径（脱敏）。
//
// 之所以要遍历而不是逐个手写：表是数据，数据不会因为写错而编译失败。
// 手写的用例只覆盖写用例的人记得的那些条目，而漏测一条的后果是
// 「这条字段一直被脱敏，但没人知道」——症状轻到不会有人去查。

// probeSegments 把表里的路径拆成一段一段。
//
// `messages[].content[].input` → messages → content[数组] → input。
func probeSegments(path string) []probeSegment {
	parts := strings.Split(path, ".")
	segs := make([]probeSegment, 0, len(parts))
	for _, p := range parts {
		if strings.HasSuffix(p, "[]") {
			segs = append(segs, probeSegment{key: strings.TrimSuffix(p, "[]"), indexed: true})
			continue
		}
		segs = append(segs, probeSegment{key: p})
	}
	return segs
}

type probeSegment struct {
	key     string
	indexed bool
}

// probePath 是探针在骨架里对应的路径：数组下标写 0，因为探针每条数组只放一个元素。
func probePath(path string) string {
	return strings.ReplaceAll(path, "[]", "[0]")
}

// buildProbe 按路径造一份最小的输入结构，叶子是给定的取值。
//
// 路径里的 `[]` 意味着那一层是数组：`messages[].content[].input` 造出来是
// `{"messages": [{"content": [{"input": 叶子}]}]}`。数组里只放一个元素——
// 这条测试要证明的是「这一格会被怎么处理」，元素个数由别的用例覆盖。
func buildProbe(t *testing.T, path string, leaf any, marker string) map[string]any {
	t.Helper()
	segs := probeSegments(path)
	cur := leaf
	for i := len(segs) - 1; i >= 0; i-- {
		val := cur
		if segs[i].indexed {
			val = []any{cur}
		}
		cur = map[string]any{segs[i].key: val}
	}
	obj, ok := cur.(map[string]any)
	if !ok {
		t.Fatalf("路径 %q 造不出对象探针", path)
	}
	// 走一遍 Decode：探针也要按真实解析路径进来，否则「数字被当成 json.Number
	// 接住」这类解析期的行为不会被覆盖。
	raw, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("探针序列化失败: %v", err)
	}
	probe, err := Decode(raw)
	if err != nil {
		t.Fatalf("探针解析失败: %v", err)
	}
	_ = marker
	return probe
}

// navigate 沿路径取出骨架里的那个叶子。
//
// 中途断掉（字段被整个丢掉、形状与预期不符）会让测试失败：
// 那两种情形都是缺陷——前者说明「本该保留的字段为什么没留下」这个问题
// 在骨架里没有线索，后者说明布局变了而白名单没跟上。
func navigate(t *testing.T, skeleton map[string]any, path string) any {
	t.Helper()
	var cur any = skeleton
	for _, seg := range probeSegments(path) {
		m, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("路径 %q 中途遇到非对象: %T", path, cur)
		}
		next, ok := m[seg.key]
		if !ok {
			t.Fatalf("路径 %q 在骨架里断在键 %q", path, seg.key)
		}
		cur = next
		if seg.indexed {
			arr, ok := cur.([]any)
			if !ok || len(arr) == 0 {
				t.Fatalf("路径 %q 的 %q 不是非空数组: %T", path, seg.key, cur)
			}
			cur = arr[0]
		}
	}
	return cur
}

// collectStrings 收齐骨架里出现的所有字符串。
//
// 用来做「某个取值一个都不出现」的断言。按字符串逐个比对而不是在序列化结果上
// 做子串匹配：后者会因为一个值恰好是另一个值的片段而误判。
func collectStrings(v any) []string {
	var out []string
	switch t := v.(type) {
	case nil:
	case string:
		out = append(out, t)
	case map[string]any:
		for k, val := range t {
			out = append(out, k)
			out = append(out, collectStrings(val)...)
		}
	case []any:
		for _, elem := range t {
			out = append(out, collectStrings(elem)...)
		}
	}
	return out
}

// assertAbsent 断言一段内容不以任何形式出现在骨架里。
func assertAbsent(t *testing.T, skeleton map[string]any, marker string) {
	t.Helper()
	for _, s := range collectStrings(skeleton) {
		if strings.Contains(s, marker) {
			t.Fatalf("骨架里出现了 %q（在 %q 里）", marker, s)
		}
	}
}

// TestAllowlistEveryEntry 给允许表里的每一条条目喂一处探针，按处理方式断言结果。
// 这是规格里「每个采集字段一条测试」那条检查项的落地处。
func TestAllowlistEveryEntry(t *testing.T) {
	for _, tbl := range allTables() {
		rules := rulesFor(t, tbl)
		r, err := NewFor(tbl.surface, tbl.root)
		if err != nil {
			t.Fatalf("表面 %q 方向 %q 建不出脱敏器: %v", tbl.surface, tbl.root, err)
		}
		for i, rule := range rules {
			t.Run(fmt.Sprintf("%s/%s/%s", tbl.surface, tbl.root, rule.Path), func(t *testing.T) {
				probe, asserts := proveEntry(t, rule, i)
				res, err := r.RedactRaw(probe)
				if err != nil {
					t.Fatalf("脱敏失败: %v", err)
				}
				asserts(t, res)
			})
		}
	}
}

// proveEntry 按条目类型造探针，并返回针对该条目的断言。
//
// 每一种处理方式都要断言两件事之一：取值留下（放行），或取值留下**形状**且
// 路径被记进 Redacted（脱敏）。后者是必需的——只断言「取值不在」的话，
// 一条把字段整个丢掉的规则也会通过，而那正是「本该保留的字段为什么没留下」
// 这类问题最需要的线索。
func proveEntry(t *testing.T, rule fieldRule, i int) (map[string]any, func(*testing.T, *Result)) {
	t.Helper()
	enumerated := fmt.Sprintf("probe_enum_%d", i)
	marker := fmt.Sprintf("probe-marker-%d", i)
	number := 4000 + i

	switch rule.Kind {
	case allowUint:
		return buildProbe(t, rule.Path, number, marker), func(t *testing.T, res *Result) {
			t.Helper()
			got := navigate(t, res.Skeleton, rule.Path)
			if !sameNumber(got, number) {
				t.Fatalf("数值没有被放行: %#v", got)
			}
		}

	case allowFloat:
		return buildProbe(t, rule.Path, 0.25, marker), func(t *testing.T, res *Result) {
			t.Helper()
			got := navigate(t, res.Skeleton, rule.Path)
			if f, ok := got.(float64); !ok || f != 0.25 {
				t.Fatalf("浮点取值没有被放行: %#v", got)
			}
		}

	case allowBool:
		return buildProbe(t, rule.Path, true, marker), func(t *testing.T, res *Result) {
			t.Helper()
			if got := navigate(t, res.Skeleton, rule.Path); got != true {
				t.Fatalf("布尔取值没有被放行: %#v", got)
			}
		}

	case allowEnumWord:
		return buildProbe(t, rule.Path, enumerated, marker), func(t *testing.T, res *Result) {
			t.Helper()
			if got := navigate(t, res.Skeleton, rule.Path); got != enumerated {
				t.Fatalf("枚举取值没有被放行: %#v", got)
			}
		}

	case allowNumberMap:
		leaf := map[string]any{"probe_key_" + enumerated: number}
		return buildProbe(t, rule.Path, leaf, marker), func(t *testing.T, res *Result) {
			t.Helper()
			got := navigate(t, res.Skeleton, rule.Path)
			m, ok := got.(map[string]any)
			if !ok {
				t.Fatalf("数字字典没有保留形状: %#v", got)
			}
			if !sameNumber(m["probe_key_"+enumerated], number) {
				t.Fatalf("字典里的数字没有被放行: %#v", m)
			}
		}

	case keepEnumList:
		return buildProbe(t, rule.Path, []any{enumerated}, marker), func(t *testing.T, res *Result) {
			t.Helper()
			got, ok := navigate(t, res.Skeleton, rule.Path).([]any)
			if !ok || len(got) != 1 || got[0] != enumerated {
				t.Fatalf("枚举列表没有被放行: %#v", got)
			}
		}

	case keepShape:
		leaf := map[string]any{"probe_nested_key": marker}
		return buildProbe(t, rule.Path, leaf, marker), func(t *testing.T, res *Result) {
			t.Helper()
			got, ok := navigate(t, res.Skeleton, rule.Path).(map[string]any)
			if !ok {
				t.Fatalf("形状没有被保留: %#v", got)
			}
			if _, ok := got["probe_nested_key"]; !ok {
				t.Fatalf("未知键的形状没有被保留: %#v", got)
			}
			assertAbsent(t, res.Skeleton, marker)
		}

	case hideText:
		return buildProbe(t, rule.Path, marker, marker), func(t *testing.T, res *Result) {
			t.Helper()
			assertRedactedText(t, res, rule.Path, marker)
		}

	case hideStringList:
		return buildProbe(t, rule.Path, []any{marker}, marker), func(t *testing.T, res *Result) {
			t.Helper()
			assertAbsent(t, res.Skeleton, marker)
			arr, ok := navigate(t, res.Skeleton, rule.Path).([]any)
			if !ok || len(arr) != 1 {
				t.Fatalf("字符串列表没有保留形状: %#v", arr)
			}
			shape, ok := arr[0].(map[string]any)
			if !ok || shape[fieldType] != typeText || !sameNumber(shape[fieldChars], len(marker)) {
				t.Fatalf("字符串列表的元素不是文本占位: %#v", arr[0])
			}
		}

	case hideValue, hideSubtree:
		return buildProbe(t, rule.Path, marker, marker), func(t *testing.T, res *Result) {
			t.Helper()
			assertAbsent(t, res.Skeleton, marker)
			assertMarked(t, res, probePath(rule.Path))
			got, ok := navigate(t, res.Skeleton, rule.Path).(map[string]any)
			if !ok || got["redacted"] != true {
				t.Fatalf("脱敏记号不对: %#v", got)
			}
		}

	case toolRef, toolRefID:
		// 工具名与调用 id 各自一套编号：名字是 T 开头，id 是 C 开头。
		// 分开的理由见 toolRefs 的注释（同名同层的两个字段不能因为
		// map 遍历顺序不同而拿到不同的编号）。
		want := "T1"
		if rule.Kind == toolRefID {
			want = "C1"
		}
		return buildProbe(t, rule.Path, marker, marker), func(t *testing.T, res *Result) {
			t.Helper()
			assertAbsent(t, res.Skeleton, marker)
			assertMarked(t, res, probePath(rule.Path))
			if got := navigate(t, res.Skeleton, rule.Path); got != want {
				t.Fatalf("取值没有换成请求内引用 %s: %#v", want, got)
			}
		}

	case hideKeysKeepShape:
		leaf := map[string]any{"probe_arg_key": map[string]any{"inner": marker}}
		return buildProbe(t, rule.Path, leaf, marker), func(t *testing.T, res *Result) {
			t.Helper()
			assertAbsent(t, res.Skeleton, marker)
			got, ok := navigate(t, res.Skeleton, rule.Path).(map[string]any)
			if !ok {
				t.Fatalf("参数形状没有被保留: %#v", got)
			}
			inner, ok := got["probe_arg_key"].(map[string]any)
			if !ok {
				t.Fatalf("嵌套键名没有被保留: %#v", got)
			}
			shape, ok := inner["inner"].(map[string]any)
			if !ok || shape[fieldType] != typeText {
				t.Fatalf("参数取值不是文本占位: %#v", inner)
			}
		}

	case roleField:
		return buildProbe(t, rule.Path, "user", marker), func(t *testing.T, res *Result) {
			t.Helper()
			if got := navigate(t, res.Skeleton, rule.Path); got != "user" {
				t.Fatalf("角色词没有被放行: %#v", got)
			}
		}

	case blockListField:
		leaf := []any{map[string]any{"type": "text", "text": marker}}
		return buildProbe(t, rule.Path, leaf, marker), func(t *testing.T, res *Result) {
			t.Helper()
			assertAbsent(t, res.Skeleton, marker)
			arr, ok := navigate(t, res.Skeleton, rule.Path).([]any)
			if !ok || len(arr) != 1 {
				t.Fatalf("内容块序列没有保留形状: %#v", arr)
			}
			block, ok := arr[0].(map[string]any)
			if !ok || block["type"] != "text" {
				t.Fatalf("块的类型没有保留: %#v", arr[0])
			}
			assertRedactedShapes(t, block)
		}

	case schemaField:
		leaf := map[string]any{
			"type": "object",
			"properties": map[string]any{
				"probe_prop": map[string]any{"type": "string", "enum": []any{marker}},
			},
			"required": []any{"probe_prop"},
		}
		return buildProbe(t, rule.Path, leaf, marker), func(t *testing.T, res *Result) {
			t.Helper()
			assertAbsent(t, res.Skeleton, marker)
			got, ok := navigate(t, res.Skeleton, rule.Path).(map[string]any)
			if !ok {
				t.Fatalf("schema 形状没有被保留: %#v", got)
			}
			props, ok := got["properties"].(map[string]any)
			if !ok {
				t.Fatalf("properties 的键名没有被保留: %#v", got)
			}
			if _, ok := props["probe_prop"]; !ok {
				t.Fatalf("属性名没有被保留: %#v", props)
			}
			if got["type"] != "object" {
				t.Fatalf("schema 的 type 没有被放行: %#v", got["type"])
			}
		}

	case headersField:
		leaf := map[string]any{"x-probe-" + enumerated: marker}
		return buildProbe(t, rule.Path, leaf, marker), func(t *testing.T, res *Result) {
			t.Helper()
			assertAbsent(t, res.Skeleton, marker)
			got, ok := navigate(t, res.Skeleton, rule.Path).(map[string]any)
			if !ok {
				t.Fatalf("头容器形状不对: %#v", got)
			}
			hashes, ok := got[fieldHashed].([]any)
			if !ok || len(hashes) == 0 {
				t.Fatalf("头名哈希没有留下: %#v", got)
			}
			for _, h := range hashes {
				if s, ok := h.(string); !ok || s == "" {
					t.Fatalf("头名哈希不是字符串: %#v", hashes)
				}
			}
		}

	case responseHeadersField:
		leaf := map[string]any{"x-ratelimit-remaining-requests": marker}
		return buildProbe(t, rule.Path, leaf, marker), func(t *testing.T, res *Result) {
			t.Helper()
			got, ok := navigate(t, res.Skeleton, rule.Path).(map[string]any)
			if !ok {
				t.Fatalf("响应头容器形状不对: %#v", got)
			}
			if got["x-ratelimit-remaining-requests"] != marker {
				t.Fatalf("额度监控要用的响应头没有被放行: %#v", got)
			}
		}

	case extraField:
		leaf := map[string]any{"probe_extra_key_" + enumerated: marker}
		return buildProbe(t, rule.Path, leaf, marker), func(t *testing.T, res *Result) {
			t.Helper()
			assertAbsent(t, res.Skeleton, marker)
			got, ok := navigate(t, res.Skeleton, rule.Path).(map[string]any)
			if !ok {
				t.Fatalf("上游特有参数没有留下形状: %#v", got)
			}
			// 键名不留：配置方自定的名字往往就是业务名词。
			if _, ok := got["probe_extra_key_"+enumerated]; ok {
				t.Fatalf("上游特有参数的键名不该被留下: %#v", got)
			}
			if len(got) != 1 {
				t.Fatalf("上游特有参数的形状数量不对: %#v", got)
			}
		}

	case deny:
		t.Fatalf("允许表里的 deny 条目（%q）不该出现在这张表里：永不采集的字段靠代码里的名单", rule.Path)
		return nil, nil

	default:
		t.Fatalf("允许表条目 %q 用了未覆盖的处理方式 %d", rule.Path, rule.Kind)
		return nil, nil
	}
}

// assertRedactedText 断言一个字段留下了文本占位，且取值没落进去。
func assertRedactedText(t *testing.T, res *Result, path, marker string) {
	t.Helper()
	assertAbsent(t, res.Skeleton, marker)
	assertMarked(t, res, probePath(path))
	got, ok := navigate(t, res.Skeleton, path).(map[string]any)
	if !ok {
		t.Fatalf("没有文本占位: %#v", got)
	}
	if got[fieldType] != typeText || !sameNumber(got[fieldChars], len(marker)) {
		t.Fatalf("文本占位不对: %#v", got)
	}
}

// assertRedactedShapes 断言一个容器的每个字符串取值都留下了占位。
func assertRedactedShapes(t *testing.T, container map[string]any) {
	t.Helper()
	for k, v := range container {
		if k == "type" {
			continue
		}
		shape, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("容器里的 %q 不是占位: %#v", k, v)
		}
		if _, ok := shape[fieldType]; !ok {
			t.Fatalf("容器里的 %q 没有类型字段: %#v", k, shape)
		}
	}
}

// assertMarked 断言某个路径被记进了 Redacted。
func assertMarked(t *testing.T, res *Result, path string) {
	t.Helper()
	for _, p := range res.Redacted {
		if p == path {
			return
		}
	}
	t.Fatalf("Redacted 里没有 %q，实际是 %v", path, res.Redacted)
}

// sameNumber 比较骨架里的数字与期望值。
//
// 数字经过一次 JSON 解析会变成 float64，直接比对类型会失败，
// 而「数值相等」才是这里要说的事。
func sameNumber(got any, want int) bool {
	switch t := got.(type) {
	case int:
		return t == want
	case int64:
		return t == int64(want)
	case float64:
		return t == float64(want)
	case json.Number:
		f, err := t.Float64()
		return err == nil && f == float64(want)
	default:
		return false
	}
}

// TestAllowlistEntriesAreWellFormed 检查允许表本身：每条都有路径与理由，
// 且不存在「永不采集」的条目——那类字段靠代码里的名单处理，混进表里
// 会让「表里没有的一律脱敏」这条规则多出一个语义含混的例外。
func TestAllowlistEntriesAreWellFormed(t *testing.T) {
	for _, tbl := range allTables() {
		rules := rulesFor(t, tbl)
		seen := map[string]bool{}
		nested := 0
		for _, rule := range rules {
			if strings.TrimSpace(rule.Path) == "" {
				t.Fatalf("%s/%s: 有空路径的条目", tbl.surface, tbl.root)
			}
			if strings.TrimSpace(rule.Note) == "" {
				t.Errorf("%s/%s: %q 没有写明理由", tbl.surface, tbl.root, rule.Path)
			}
			if seen[rule.Path] {
				t.Errorf("%s/%s: %q 重复定义", tbl.surface, tbl.root, rule.Path)
			}
			seen[rule.Path] = true
			if deniesKeep(rule.Kind) {
				t.Errorf("%s/%s: %q 是永不采集条目，不该出现在允许表里",
					tbl.surface, tbl.root, rule.Path)
			}
			if strings.Contains(rule.Path, "[]") {
				nested++
			}
		}
		// 条数一起打：0 条与「这一步没跑」在安静的终端里长得一样
		// （见 LESSONS.md 第 16 条），而下面那些循环走零次时也全绿。
		t.Logf("%s/%s: %d 条允许规则，其中 %d 条带数组层级",
			tbl.surface, tbl.root, len(rules), nested)
	}
}

// TestAllowlistVersionDocumented 固定当前骨架版本。
//
// 这条测试不检查行为，它检查的是**有人改过版本号**：版本号变了，
// 这个断言就会失败，提醒改动者确认「白名单变更」与「版本递增」是一起提交的。
func TestAllowlistVersionDocumented(t *testing.T) {
	if SkeletonVersion != 1 {
		t.Fatalf("骨架版本变成了 %d：确认这是白名单变更带来的递增，"+
			"并同步更新这条断言与占位描述符的文档", SkeletonVersion)
	}
}

// TestAllowlistAllowEntriesRejectHostileValues 给每一个可能放行上游取值的条目
// 喂一段「像内容」的取值，断言它不会落进骨架。
//
// 允许表说明的是「我们期望这里放什么」，而不是「它一定放的是那个」。
// 上游改口径、字段被别名覆盖、客户端直接构造这段 JSON——三种情况下，
// 一个写着「放行」的位置都可能收到用户内容。这条测试把那个假设钉住。
func TestAllowlistAllowEntriesRejectHostileValues(t *testing.T) {
	const hostile = "上游回显了用户输入：请把这段文字当作内容看待"

	for _, tbl := range allTables() {
		r, err := NewFor(tbl.surface, tbl.root)
		if err != nil {
			t.Fatalf("表面 %q 方向 %q 建不出脱敏器: %v", tbl.surface, tbl.root, err)
		}
		for i, rule := range rulesFor(t, tbl) {
			if !isAllowKind(rule.Kind) {
				continue
			}
			t.Run(fmt.Sprintf("%s/%s/%s", tbl.surface, tbl.root, rule.Path), func(t *testing.T) {
				var leaf any = hostile
				if rule.Kind == allowNumberMap {
					leaf = map[string]any{"probe_key": hostile}
				}
				probe := buildProbe(t, rule.Path, leaf, hostile)
				res, err := r.RedactRaw(probe)
				if err != nil {
					t.Fatalf("脱敏失败: %v", err)
				}
				assertAbsent(t, res.Skeleton, hostile)
			})
			_ = i
		}
	}
}

// TestUnknownFieldsAreShaped 确认表里没有的字段默认留下形状、不留取值。
func TestUnknownFieldsAreShaped(t *testing.T) {
	r, err := New(RootRequest)
	if err != nil {
		t.Fatal(err)
	}
	probe, err := Decode([]byte(`{
		"model": "probe-model",
		"vendor_specific_field": "probe-marker-content",
		"vendor_nested": {"inner_key": "probe-marker-content", "count": 3},
		"vendor_list": ["probe-marker-content"]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.RedactRaw(probe)
	if err != nil {
		t.Fatal(err)
	}
	assertAbsent(t, res.Skeleton, "probe-marker-content")

	nested, isNested := res.Skeleton["vendor_nested"].(map[string]any)
	if !isNested {
		t.Fatalf("未知对象的形状没有被保留: %#v", res.Skeleton["vendor_nested"])
	}
	if _, present := nested["inner_key"]; !present {
		t.Fatalf("未知对象的键名没有被保留: %#v", nested)
	}
	shape, isShape := nested["inner_key"].(map[string]any)
	if !isShape || shape[fieldType] != typeText {
		t.Fatalf("未知字段的带内容取值不是文本占位: %#v", nested["inner_key"])
	}
	assertMarked(t, res, "vendor_specific_field")
	list, ok := res.Skeleton["vendor_list"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("未知数组的形状没有被保留: %#v", res.Skeleton["vendor_list"])
	}
}

// TestTableIntegrity 检查表在构造期被检查的那些问题都能被发现。
//
// 表是数据，写错不会编译失败：重复路径让后一条静默覆盖前一条，
// 而症状是「加了字段却还是被脱敏」，没有任何提示。
func TestTableIntegrity(t *testing.T) {
	cases := []struct {
		name  string
		rules []fieldRule
		want  string
	}{
		{
			name:  "空表",
			rules: nil,
			want:  "为空",
		},
		{
			name:  "条目没有路径",
			rules: []fieldRule{{Path: "  ", Kind: allowUint, Note: "有理由"}},
			want:  "没有路径",
		},
		{
			name:  "条目没有理由",
			rules: []fieldRule{{Path: "model", Kind: allowUint}},
			want:  "没有写明",
		},
		{
			name: "重复路径",
			rules: []fieldRule{
				{Path: "model", Kind: allowUint, Note: "有理由"},
				{Path: "model", Kind: hideText, Note: "第二条"},
			},
			want: "重复定义",
		},
		{
			name:  "对具体头写了放行规则",
			rules: []fieldRule{{Path: "headers.x-foo", Kind: allowUint, Note: "有理由"}},
			want:  "只允许 deny",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRules(tc.rules)
			if err == nil {
				t.Fatalf("这样的表应当被拒绝")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息没有说清问题，实际是 %v", err)
			}
		})
	}
}

// TestExportedAllowlistView 检查包外可见的那份视图与内部表一致。
//
// 包外的调用方（测试、将来的审计命令）看的是 FieldRule 那一份；
// 它与内部表的档位、路径、理由必须逐条对上，否则「遍历这张表做检查」
// 这件事就在检查一份与真实行为无关的清单。
func TestExportedAllowlistView(t *testing.T) {
	for _, tbl := range allTables() {
		internal := rulesFor(t, tbl)
		view := AllowlistFor(tbl.surface, tbl.root)
		if len(view) != len(internal) {
			t.Fatalf("%s/%s: 包外视图 %d 条，内部表 %d 条",
				tbl.surface, tbl.root, len(view), len(internal))
		}
		for i := range view {
			if view[i].Path != internal[i].Path {
				t.Fatalf("%s/%s 第 %d 条路径不一致：%q / %q",
					tbl.surface, tbl.root, i, view[i].Path, internal[i].Path)
			}
			if view[i].Kind != internal[i].Kind.String() {
				t.Fatalf("%s/%s/%s 的档位名字不一致：%q",
					tbl.surface, tbl.root, view[i].Path, view[i].Kind)
			}
			if view[i].Note != internal[i].Note {
				t.Fatalf("%s/%s/%s 的理由不一致", tbl.surface, tbl.root, view[i].Path)
			}
		}
	}
	// 未知方向与未知表面都给 nil，而不是回落到某一张默认表上——
	// 回落的表现是「一个拼错的表面名静默地用了 chat 的规则」。
	if AllowlistFor(Surface("不在的表面"), RootRequest) != nil {
		t.Error("未知表面不该有允许表")
	}
	if AllowlistFor(SurfaceChat, Root("不在的方向")) != nil {
		t.Error("已知表面上的未知方向不该有允许表")
	}
}

// TestTableCopyIsIndependent 确认返回的视图是副本。
func TestTableCopyIsIndependent(t *testing.T) {
	original := AllowlistFor(SurfaceChat, RootRequest)[0]

	view := AllowlistFor(SurfaceChat, RootRequest)
	view[0].Path = "被改坏了"
	view[0].Note = "被改坏了"

	again := AllowlistFor(SurfaceChat, RootRequest)
	if again[0].Path != original.Path || again[0].Note != original.Note {
		t.Fatalf("调用方改动了返回的视图，影响到了原表：%#v / %#v", view[0], again[0])
	}
}

// TestRootValuesMatchSchema 确认方向取值与库里的 CHECK 约束一致，
// 且「取值清单」与「允许表」这两处互相对得上。
//
// 原来这条断言的是 `RootRequest.valid() && RootResponse.valid() &&
// !Root("其他").valid()`，而被测的 valid() 就是那两个常量的内联比较——两边
// 是同一组字面量，怎么改都自洽。它漏掉的是反方向：valid() 认了一个允许表里
// 没有的方向，那个方向会被 New 拒绝，而 valid() 说它合法。
func TestRootValuesMatchSchema(t *testing.T) {
	// 取值对着 request_snapshots.direction 的 CHECK 约束，所以这里钉字面量：
	// 从常量取值等于用常量测常量，常量被改错时两边一起变，看不出来。
	wantValues := map[Root]string{
		RootRequest:  "request",
		RootResponse: "response",
	}

	roots := Roots()

	// 允许表 → 取值清单：表里有的方向必须是已知取值，落库的字面量也要对得上。
	for _, r := range roots {
		if !r.valid() {
			t.Errorf("允许表里有方向 %q，valid() 却不认它", r)
		}
		want, pinned := wantValues[r]
		if !pinned {
			t.Errorf("允许表里有方向 %q，但它的落库取值没有钉住", r)
			continue
		}
		if string(r) != want {
			t.Errorf("方向 %q 落库会写成 %q，而 request_snapshots.direction 的约束要的是 %q",
				r, string(r), want)
		}
	}

	// 取值清单 → 允许表：valid() 认的每一个都必须有允许表，否则同一个方向
	// 在 valid() 那里合法、在 New 那里被拒。
	for _, r := range rootValues {
		if !slices.Contains(roots, r) {
			t.Errorf("valid() 认 %q，但 Roots() = %v 里没有它——允许表漏了这个方向", r, roots)
		}
	}

	if Root("其他").valid() {
		t.Error("未列在取值清单里的取值不该通过校验")
	}
}

// ── #675：整包变异存活条目的判据 ─────────────────────────────────────────
//
// 这一批钉的是 allowlist 自己的判据：哪个位置按哪一档处理、兜底键怎么合并、
// 路径怎么归一。每条用例的注释写明它钉住的那条变异；判为等价变异的，
// 同一处也写明「哪一格分不开」以及为什么那一格到不了。

// Roots 的顺序固定：调用方照着它遍历，两次运行必须是同一列。
//
// 变异：比较器 `<` 翻成 `>=`，Roots() 变成降序——两次运行之间仍然一致，
// 但「按方向名升序」这条可预期的次序没了，而这一行 sort 存在的理由正是它。
// 同一个比较器上 `<` 翻成 `<=` 是**等价变异**：元素是 allowlists 的键，
// 两两不相等，而 `<` 与 `<=` 只在两个元素相等时给出不同答案，那一格在这份
// 输入上不存在。
func TestRootsOrderIsFixed(t *testing.T) {
	got := Roots()
	want := []Root{RootRequest, RootResponse}
	if !slices.Equal(got, want) {
		t.Fatalf("Roots() = %v，期望 %v", got, want)
	}
}

// lookup 的逐段下滑：去掉开头若干段再比，所以同一层的字段出现在别的容器里
// 也照样命中，不会因为少写一行就退化成默认档。
//
// 变异：`for i := 1; i < len(segs); i++` 的边界翻成 `<=` 是**等价变异**。
// 多出来的是 i == len(segs) 那一趟：`segs[i:]` 是空切片，拼出来是空串，
// 于是查的是 t[""]——而编译出来的表里没有这个键：键要么是归一化后的非空路径
// （validateRules 拒掉空白路径，归一化只删下标、不会把非空串变成空串），
// 要么是 nameOnlyKey 加了 `\x00` 前缀的兜底键。
func TestLookupDropsLeadingSegments(t *testing.T) {
	tab := buildTable([]fieldRule{{Path: "messages[].role", Kind: roleField, Note: "探针"}})
	for _, path := range []string{"messages[0].role", "choices[].messages[2].role"} {
		if k, ok := tab.lookup(path, "role"); !ok || k != roleField {
			t.Errorf("lookup(%q) = %v/%v，期望 roleField/true——去掉开头几段之后该命中", path, k, ok)
		}
	}
}

// normalizePath 把具体下标折成 `[]`；没有闭合的 `[` 原样带到底。
//
// 变异：`if j < 0` 的边界翻成 `j <= 0` 是**等价变异**：能走到那一行时
// path[i] 一定是 `[`（外层判的就是它），所以 `]` 不可能出现在偏移 0，
// j == 0 这一格不存在。
func TestNormalizePathFoldsIndices(t *testing.T) {
	cases := map[string]string{
		"messages[0].content[12].name": "messages[].content[].name",
		"messages[].role":              "messages[].role",
		"plain":                        "plain",
		"broken[12":                    "broken[12",
	}
	for in, want := range cases {
		if got := normalizePath(in); got != want {
			t.Errorf("normalizePath(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// trimIndex 去掉段尾的下标：兜底键就是靠它把 `content[]` 折成 `content`。
//
// 变异：循环条件 `<` 翻成 `>=`，循环一次都不走，trimIndex 退化成恒等函数。
// 兜底键于是变成 `content[]` 这种带下标的写法，运行期路径
// `messages[0].content[0]` 落到兜底那一层时对不上任何规则，整段退回默认档——
// 不报错，只是「以为留着的字段没留」。
func TestTrimIndexStripsArraySuffix(t *testing.T) {
	cases := map[string]string{
		"content[]":  "content",
		"content[3]": "content",
		"content":    "content",
	}
	for in, want := range cases {
		if got := trimIndex(in); got != want {
			t.Errorf("trimIndex(%q) = %q，期望 %q", in, got, want)
		}
	}
	// 兜底键的最后一环：取最后一段时同样要把下标去掉
	if got := lastSegment("messages[].content[]"); got != "content" {
		t.Errorf("lastSegment(%q) = %q，期望 %q", "messages[].content[]", got, "content")
	}
}

// lastSegment 的输入只有「表里那条路径」——而表里的路径没有一条以 `.` 开头。
//
// 变异：循环边界从 `i >= 0` 翻成 `i > 0`，只在下标 0 处那个字符是 `.` 时才
// 分得开（那时它会返回 `.role` 而不是 `role`）。做这件事的输入只有「表里
// 写着一条以 `.` 开头的路径」，而下面这遍遍历确认四个方向都没有这种写法。
// 将来有人真写了，这条会红：那时要么改表，要么把这条等价判据重新审一遍。
func TestRulePathsHaveNoLeadingDot(t *testing.T) {
	for _, tbl := range allTables() {
		for _, r := range rulesFor(t, tbl) {
			if strings.HasPrefix(r.Path, ".") {
				t.Errorf("%s/%s: 路径 %q 以 . 开头——lastSegment 的等价判据要重审",
					tbl.surface, tbl.root, r.Path)
			}
		}
	}
}

// 同名兜底键上有两条规则时，留下信息更少的那条；信息量相同则先写的赢。
//
// 变异（同一行两支，分开判）：
//   - `<=` 翻成 `<`：同秩时改成后写的赢。chat 请求侧的兜底键 `content` 上
//     真有两档同秩的规则（秩 3 的 block-list-field 与 hide-keys-keep-shape），
//     换谁赢是能观测到的——兜底那一层的处理方式整个换掉。
//   - `<=` 翻成 `>`：整条判据反过来，留下信息**多**的那条。同一张表里兜底键
//     `name` 上是秩 0 的 hide-value 与秩 2 的 tool-ref，翻过来之后
//     「按兜底键走的字段」反而放行得更多。
func TestMoreConservativePicksLessRevealing(t *testing.T) {
	// 异秩：hide-value（秩 0）该压过 tool-ref（秩 2）
	tab := buildTable([]fieldRule{
		{Path: "a.name", Kind: toolRef, Note: "探针"},
		{Path: "b.name", Kind: hideValue, Note: "探针"},
	})
	if got := tab[nameOnlyKey("name")]; got != hideValue {
		t.Errorf("异秩冲突选中了 %v，期望 hideValue（秩 0 小于 toolRef 的 2）", got)
	}

	// 同秩：先写的那条赢，判据不随表的书写顺序之外的东西变
	tab = buildTable([]fieldRule{
		{Path: "a.content", Kind: blockListField, Note: "探针"},
		{Path: "b.content", Kind: roleField, Note: "探针"},
	})
	if got := tab[nameOnlyKey("content")]; got != blockListField {
		t.Errorf("同秩冲突选中了 %v，期望先写的 blockListField", got)
	}
}
