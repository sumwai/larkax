package snapshot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// retentionDocPath 是那份面向使用者的留存承诺文档。
//
// 路径从本文件的测试目录往上两级：docs/ 在仓库根下。
const retentionDocPath = "../../docs/request-retention.md"

// TestDecisionAllowlistDocumented 确认决策表面记下来的字段与文档里那张清单
// 逐条对得上。
//
// # 为什么这条判据要写成用例
//
// 那份文档回答的是「你们存了我什么」，而它的读者是使用者。它写漏一个字段，
// 承诺就成了假的：一个字段悄悄开始落库，而对外说得比实际少——这类偏差没有
// 任何运行期症状。反过来，表里删掉一个字段而文档还写着它，读文档的人会去
// 库里找一个永远不存在的键。
//
// 比对是**集合相等**，不是「文档包含表」：后者漏掉的是「文档里写着、
// 代码里没有」那一半，而那一半正是让排查的人扑空的那种错。
//
// # 它为什么是一道「没跑成」也要报错的检查
//
// 两个集合都是解析出来的：文档少了那个围栏、或者允许表被清空，两边都会变成
// 空集，而空集相等——一次解析失败看起来与「全部对得上」一模一样
// （见 LESSONS.md 第 16 条）。所以任一边为零时判失败，并把两个条数都打出来。
func TestDecisionAllowlistDocumented(t *testing.T) {
	inCode := decisionAllowlistPaths()
	doc, inDoc := documentedDecisionPaths(t)

	if len(inCode) == 0 {
		t.Fatal("允许表里一个字段都没解析出来——这一步没跑成，不是「对得上」")
	}
	if len(inDoc) == 0 {
		t.Fatalf("从 %s 里一个字段都没解析出来——这一步没跑成，不是「对得上」", doc)
	}
	t.Logf("允许表 %d 个字段，文档 %d 个字段", len(inCode), len(inDoc))

	for path := range inCode {
		if !inDoc[path] {
			t.Errorf("允许表里有 %q，文档里没有——对外说得比实际少", path)
		}
	}
	for path := range inDoc {
		if !inCode[path] {
			t.Errorf("文档里写着 %q，允许表里没有——读文档的人会去库里找一个永远不存在的键", path)
		}
	}
}

// decisionAllowlistPaths 是决策表面两个方向记下来的全部字段路径。
//
// 取的是并集：两个方向共用大部分字段，而文档写的是「这条表面记什么」，
// 不分方向。
func decisionAllowlistPaths() map[string]bool {
	out := map[string]bool{}
	for _, root := range []Root{RootRequest, RootResponse} {
		for _, rule := range decisionAllowlists[root] {
			out[rule.Path] = true
		}
	}
	return out
}

// documentedDecisionPaths 从留存文档里解析出那份字段清单。
//
// 它取「决策接口的留存范围」那一节里的第一个围栏代码块：文档里别的代码块
// 讲的是别的事，跟着第一个围栏走会把它们一起读进来。
func documentedDecisionPaths(t *testing.T) (string, map[string]bool) {
	t.Helper()

	raw, err := os.ReadFile(filepath.Clean(retentionDocPath))
	if err != nil {
		t.Fatalf("读不到留存文档: %v", err)
	}

	const section = "## 决策接口的留存范围"
	body := string(raw)
	start := strings.Index(body, section)
	if start < 0 {
		t.Fatalf("%s 里找不到 %q 那一节——文档改了结构，这条判据的解析也就失效了",
			retentionDocPath, section)
	}
	body = body[start:]

	open := strings.Index(body, "```")
	if open < 0 {
		t.Fatalf("%s 的 %q 一节里没有代码块", retentionDocPath, section)
	}
	body = body[open+len("```"):]
	// 跳过语言标记那一行（```text）。
	if nl := strings.IndexByte(body, '\n'); nl >= 0 {
		body = body[nl+1:]
	}
	closeAt := strings.Index(body, "```")
	if closeAt < 0 {
		t.Fatalf("%s 的 %q 一节里代码块没有收尾", retentionDocPath, section)
	}

	out := map[string]bool{}
	for _, line := range strings.Split(body[:closeAt], "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// 每行是「路径 + 空白 + 说明」。取第一段即可：路径本身不含空白。
		fields := strings.Fields(line)
		out[fields[0]] = true
	}
	return retentionDocPath, out
}
