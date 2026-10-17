package snapshot

import (
	"sort"
	"testing"
)

// surfaceTable 是一张「表面 × 方向」的允许表。
type surfaceTable struct {
	surface Surface
	root    Root
}

// allTables 列出全部允许表，供遍历型用例逐张走过。
//
// 直接走 surfaces 里真实存在的那几张，而不是写一层「表面 × Roots()」的嵌套
// 循环：两张表的键是不是一样，正是要断言的事（见
// TestEverySurfaceHasBothDirections），用例自己假定它一样就等于把它钉死了
// 却假装在检查。
func allTables() []surfaceTable {
	var out []surfaceTable
	for _, s := range Surfaces() {
		roots := make([]Root, 0, len(surfaces[s]))
		for r := range surfaces[s] {
			roots = append(roots, r)
		}
		sort.Slice(roots, func(i, j int) bool { return roots[i] < roots[j] })
		for _, r := range roots {
			out = append(out, surfaceTable{surface: s, root: r})
		}
	}
	return out
}

// rulesFor 取某张表的内部条目，供遍历型用例使用。
//
// 表不存在或为空时直接让用例失败：遍历型用例的判据是「每一条都被走过一遍」，
// 而一张空表会让那个循环走零次、输出与「全部通过」一模一样
// （见 LESSONS.md 第 16 条）。
func rulesFor(t *testing.T, tbl surfaceTable) []fieldRule {
	t.Helper()
	rules := surfaces[tbl.surface][tbl.root]
	if len(rules) == 0 {
		t.Fatalf("表面 %q 方向 %q 没有允许表", tbl.surface, tbl.root)
	}
	out := make([]fieldRule, len(rules))
	copy(out, rules)
	return out
}

// TestEverySurfaceHasBothDirections 钉住「哪几张表该存在」。
//
// 两个方向都写死在用例里，而不是拿 surfaces 自己的键去测它自己：后者是
// 用常量测常量，删掉一张表时两边一起变、看不出来。这里删掉任何一张表都会红，
// 而「表面取值清单里有、表里没有」这一条反方向也单独走一遍——那种表缺失
// 同样的表现是 NewFor 在构造期拒绝，而用例不该等到那时才发现。
func TestEverySurfaceHasBothDirections(t *testing.T) {
	want := []surfaceTable{
		{surface: SurfaceChat, root: RootRequest},
		{surface: SurfaceChat, root: RootResponse},
		{surface: SurfaceDecision, root: RootRequest},
		{surface: SurfaceDecision, root: RootResponse},
	}
	got := allTables()
	if len(got) != len(want) {
		t.Fatalf("允许表有 %d 张（%v），期望 %d 张（%v）", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 张表是 %v，期望 %v", i, got[i], want[i])
		}
	}

	for _, s := range surfaceValues {
		if _, ok := surfaces[s]; !ok {
			t.Errorf("表面清单里有 %q，但 surfaces 里没有它", s)
		}
	}
}

// TestSurfaceTablesAreDistinct 确认两条表面用的是**不同的**表。
//
// 这不是废话：把决策那两个方向指回 chat 的表（或者反过来）在编译期完全合法，
// 而现场表现是「一批字段静默地按另一条表面的规则处理」——
// 多了几个本该脱敏的字段，或者少了几个本该留下的判据，两者都不报错。
func TestSurfaceTablesAreDistinct(t *testing.T) {
	for _, root := range []Root{RootRequest, RootResponse} {
		chat := surfaces[SurfaceChat][root]
		decision := surfaces[SurfaceDecision][root]
		if len(chat) == 0 || len(decision) == 0 {
			t.Fatalf("方向 %q 上有表是空的", root)
		}
		// 判据用第一条规则的路径：两张表若是同一份，第一条必然相同。
		if chat[0].Path == decision[0].Path {
			t.Errorf("方向 %q 上 chat 与决策的表指向同一份（首条都是 %q）", root, chat[0].Path)
		}
	}
}
