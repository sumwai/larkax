package main

import (
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// ── docs/development.md 的运维命令一览表 ──────────────────────────────
//
// 那张表（「运维命令行走另一个入口（`cmd/larkaxctl`），目前有这些」下面那张）
// 是「这台机器上有哪些运维命令」的第一入口，而它此前一个门禁都没过问：
// scripts/check-docs-index.sh 只判索引↔文件与 TOC 锚点，不看表里的内容。于是
// 它停在好几条命令的动词之前——channel 少了 get / health / restore，user 只写了
// create，整条 consent 连一行都没有（见 #730）。
//
// 这一组用例把表与**实现**逐个动词对一遍。动词集合从注册表（main.go 的
// commandGroups）与各命令分发函数的 case 分支解析出来，不在用例里抄第二份：
// 手抄的清单只能证明「两份手抄的字面量彼此一致」，实现新增一个动词时它不会响，
// 而「表追不上实现」正是这张表坏掉的方式。

// opsTableHeader 是那张表的表头。表体按它往下认，所以它是这个解析器唯一的锚点。
const opsTableHeader = "| 子命令 | 做什么 |"

// opsTableRow 是表里的一行：命令名与它那一格里的动词。
type opsTableRow struct {
	command string
	verbs   []string
}

// opsRegistryEntry 是注册表里的一条命令：名字与它的处理函数。
type opsRegistryEntry struct {
	name    string
	handler string
}

// opsClause 是分发分支里的一个 case：一组标签共用一个分支。多标签的那几个
// case 里，除了正式动词还挂着一个旧写法（见 legacyOpsVerbs）。
//
// depth 是这个分支离顶层处理函数有多远：0 是命令自己的动作，1 起是二级命令
// 的动作（`model alias` → runModelAlias、`price calendar` → runPriceCalendar）。
// 旧写法只在 depth 0 上判——legacyOpsVerbs 说的是「`model add` 是 `model
// create` 的旧名」，而不是「任何一级叫 add 的动词都是 create 的旧名」。不分开
// 的话，`model alias add` 会被当成旧写法从表里抹掉，而它是一条要写进文档的
// 正式动作。
type opsClause struct {
	labels []string
	depth  int
}

// legacyOpsVerbs 是分发分支里保留的**旧写法**：同一个 case 上挂着两个名字，
// 表里只写正式的那个。
//
// 这份清单不是信任项：verifyLegacyOpsVerbs 要求每条旧写法在源码里确实与那个
// 正式动词挂在**同一个 case** 上。源码里把旧写法删掉之后这条用例会红，而不是
// 悄悄把「旧名字没了」放过——旧名字在行为那一侧由 legacy_aliases_test.go 挡着，
// 这里挡的是同一件事在表这一侧的样子（表里不该出现旧写法，也不该因为旧写法
// 消失而整行对不上）。
var legacyOpsVerbs = map[string]map[string]string{
	"model":    {"add": "create", "show": "get"},
	"alias":    {"add": "create"},
	"price":    {"create": "add"},
	"wallet":   {"open": "create"},
	"snapshot": {"show": "get"},
}

// TestDevelopmentDocOpsTableMatchesRegistry 断言那张表与实现逐个动词对得上：
// 行 = 注册表里的命令，每行的动词集合 = 该命令分发分支上的 case 标签
// （加上子命令自己的动作，减掉旧写法）。
func TestDevelopmentDocOpsTableMatchesRegistry(t *testing.T) {
	rows := readOpsTable(t)
	funcs := opsFuncDecls(t)
	entries := registeredOpsCommands(t)

	documented := make(map[string][]string, len(rows))
	for _, row := range rows {
		if _, dup := documented[row.command]; dup {
			t.Errorf("表里 %q 有两行", row.command)
		}
		documented[row.command] = row.verbs
	}

	for _, entry := range entries {
		want := opsCommandVerbs(t, funcs, entry)
		have, ok := documented[entry.name]
		if !ok {
			t.Errorf("表里没有 %q 这一行，实现里有这条命令", entry.name)
			continue
		}
		if !slices.Equal(have, want) {
			t.Errorf("%s 那一行的动词 = %v，实现里的是 %v：表按实现补齐（少写的补上，多写的删掉）",
				entry.name, have, want)
		}
		delete(documented, entry.name)
	}

	// 剩下的都是注册表里没有的命令名——表里多出来的一行。
	extra := make([]string, 0, len(documented))
	for name := range documented {
		extra = append(extra, name)
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		t.Errorf("表里有注册表里没有的命令：%v", extra)
	}
}

// readOpsTable 解析 docs/development.md 里那张运维命令一览表。
//
// 表头是唯一的锚点：认不出它就报错，而不是返回一张空表——「表被搬走了」与
// 「这个解析器坏了」在输出上长得一样，两种都不能算通过（见 LESSONS.md 第 16 条）。
func readOpsTable(t *testing.T) []opsTableRow {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "development.md"))
	if err != nil {
		t.Fatalf("读 docs/development.md 失败：%v", err)
	}

	lines := strings.Split(string(raw), "\n")
	header := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == opsTableHeader {
			header = i
			break
		}
	}
	if header < 0 {
		t.Fatalf("docs/development.md 里找不到 %q 这一行：那张表被搬走了，或者这个解析器坏了", opsTableHeader)
	}

	var rows []opsTableRow
	for _, line := range lines[header+2:] { // +2 跳过表头与它下面那条分隔行
		if !strings.HasPrefix(line, "|") {
			break
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) != 2 {
			t.Fatalf("表里这一行不是「子命令 | 做什么」两个格子：%q", line)
		}
		rows = append(rows, opsTableRow{
			command: strings.Trim(strings.TrimSpace(cells[0]), "`"),
			verbs:   backtickedVerbs(cells[1]),
		})
	}
	if len(rows) == 0 {
		t.Fatal("表里一行数据都没读到：要么表是空的，要么这个解析器坏了")
	}
	return rows
}

// opsVerbPattern 是表里一个动词的形状：小写字母开头，只含小写字母、数字与
// 连字符（`down-to` 这种）。
var opsVerbPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)

// opsBacktickPattern 抠出「做什么」那一格里每个反引号包住的东西。
var opsBacktickPattern = regexp.MustCompile("`([^`]+)`")

// backtickedVerbs 取出一格里像动词的那些反引号内容。
//
// 同一格里还有别的反引号内容：`--all` 以连字符开头，`usage_events` 带下划线，
// 都不是动词。挑漏了不会静默放过——下面比的是**两个集合相等**，少掉的那一边
// 照样报错；而挑多了会当场报「表里有实现里没有的动词」，也不会误判成通过。
func backtickedVerbs(cell string) []string {
	seen := make(map[string]bool)
	var verbs []string
	for _, match := range opsBacktickPattern.FindAllStringSubmatch(cell, -1) {
		token := match[1]
		if !opsVerbPattern.MatchString(token) || seen[token] {
			continue
		}
		seen[token] = true
		verbs = append(verbs, token)
	}
	sort.Strings(verbs)
	return verbs
}

// opsFuncDecls 把包目录下每个非测试文件里的顶层函数收成一张表。
//
// 分发分支的递归要按**调用的那个函数**往下走，所以这里要一份按名字查的索引。
// 方法不进表：分发只发生在自由函数里，而带接收者的同名方法（`label` / `jsonLabel`
// 这类）按名字撞进来会把不相干的 case 当成动词。
func opsFuncDecls(t *testing.T) map[string]*ast.FuncDecl {
	t.Helper()

	funcs := make(map[string]*ast.FuncDecl)
	forEachNonTestFile(t, func(_ string, file *ast.File) {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil {
				continue
			}
			funcs[fn.Name.Name] = fn
		}
	})
	if len(funcs) == 0 {
		t.Fatal("一个顶层函数都没扫到：要么这个扫描器坏了，要么包被搬走了")
	}
	return funcs
}

// registeredOpsCommands 读 commandGroups 里的命令清单：名字与处理函数。
//
// legacyCommands 不算：它们不进根帮助页，也就不是这张表在说的那批命令
// （根帮助页与这张表说的是同一批，见 TestRootUsageListsExactlyTheRegisteredCommands）。
func registeredOpsCommands(t *testing.T) []opsRegistryEntry {
	t.Helper()

	var entries []opsRegistryEntry
	forEachNonTestFile(t, func(file string, node *ast.File) {
		for _, decl := range node.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				values, ok := spec.(*ast.ValueSpec)
				if !ok || len(values.Names) != 1 || values.Names[0].Name != "commandGroups" {
					continue
				}
				if len(values.Values) != 1 {
					t.Fatalf("%s：commandGroups 不是一个字面量，读不了", file)
				}
				groups, ok := values.Values[0].(*ast.CompositeLit)
				if !ok {
					t.Fatalf("%s：commandGroups 不是复合字面量，读不了", file)
				}
				entries = append(entries, opsEntriesFromGroups(t, file, groups)...)
			}
		}
	})
	if len(entries) == 0 {
		t.Fatal("注册表里一条命令都没读到：要么这个扫描器坏了，要么 commandGroups 改形状了")
	}
	return entries
}

// opsEntriesFromGroups 读出一组 commandGroup 字面量里每条命令的 name / run。
func opsEntriesFromGroups(t *testing.T, file string, groups *ast.CompositeLit) []opsRegistryEntry {
	t.Helper()

	var entries []opsRegistryEntry
	for _, group := range groups.Elts {
		g, ok := group.(*ast.CompositeLit)
		if !ok {
			t.Fatalf("%s：commandGroups 里的分组不是复合字面量", file)
		}
		for _, field := range g.Elts {
			kv, ok := field.(*ast.KeyValueExpr)
			if !ok {
				t.Fatalf("%s：分组里的字段不是 key-value", file)
			}
			key, isIdent := kv.Key.(*ast.Ident)
			if !isIdent || key.Name != "commands" {
				continue
			}
			commands, ok := kv.Value.(*ast.CompositeLit)
			if !ok {
				t.Fatalf("%s：分组里的 commands 不是复合字面量", file)
			}
			for _, item := range commands.Elts {
				entries = append(entries, opsEntryFromCommand(t, file, item))
			}
		}
	}
	return entries
}

// opsEntryFromCommand 读一条 command 字面量上的 name 与 run。
func opsEntryFromCommand(t *testing.T, file string, node ast.Expr) opsRegistryEntry {
	t.Helper()

	literal, ok := node.(*ast.CompositeLit)
	if !ok {
		t.Fatalf("%s：命令条目不是复合字面量", file)
	}
	var entry opsRegistryEntry
	for _, field := range literal.Elts {
		kv, ok := field.(*ast.KeyValueExpr)
		if !ok {
			t.Fatalf("%s：命令条目里的字段不是 key-value", file)
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			t.Fatalf("%s：命令条目里的字段名不是标识符", file)
		}
		switch key.Name {
		case "name":
			entry.name = opsStringLiteral(t, file, kv.Value)
		case "run":
			id, ok := kv.Value.(*ast.Ident)
			if !ok {
				t.Fatalf("%s：%s 的 run 不是函数名", file, entry.name)
			}
			entry.handler = id.Name
		}
	}
	if entry.name == "" || entry.handler == "" {
		t.Fatalf("%s：注册表里这一条缺 name 或 run：%+v", file, entry)
	}
	return entry
}

// opsStringLiteral 取一个字符串字面量的值。
func opsStringLiteral(t *testing.T, file string, node ast.Expr) string {
	t.Helper()

	lit, ok := node.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		t.Fatalf("%s：这里该是一个字符串字面量", file)
	}
	value, err := strconv.Unquote(lit.Value)
	if err != nil {
		t.Fatalf("%s：解析字面量 %s 失败：%v", file, lit.Value, err)
	}
	return value
}

// opsCommandVerbs 推出一条命令的动作集合，按名字排序。
func opsCommandVerbs(t *testing.T, funcs map[string]*ast.FuncDecl, entry opsRegistryEntry) []string {
	t.Helper()

	if _, ok := funcs[entry.handler]; !ok {
		t.Fatalf("%s 的处理函数 %s 不在源码里：注册表与实现分家了", entry.name, entry.handler)
	}
	clauses := opsClausesOf(funcs, entry.handler, map[string]bool{}, 0)
	if len(clauses) == 0 {
		// 一条子命令都没有的命令（doctor / version）：表里那一行也不该有动词。
		return nil
	}
	verifyLegacyOpsVerbs(t, entry.name, clauses)

	aliases := legacyOpsVerbs[entry.name]
	seen := make(map[string]bool)
	var verbs []string
	for _, clause := range clauses {
		for _, label := range clause.labels {
			// 旧写法只在顶层判：见 opsClause.depth 的注释。它不算进 seen——
			// 同一个词在更深一层是正式动作时（`model alias add`）要能进来。
			if _, isOldName := aliases[label]; isOldName && clause.depth == 0 {
				continue
			}
			if seen[label] {
				continue
			}
			seen[label] = true
			verbs = append(verbs, label)
		}
	}
	sort.Strings(verbs)
	return verbs
}

// opsClausesOf 收一个处理函数的分发分支，并递归进它调用的子命令分发函数
// （`price calendar` → runPriceCalendar、`plugin kv` → runPluginKV）。
//
// 递归跟着 case 分支里**真正调用的那个函数**走，不按名字猜：名字猜错的表现是
// 「子命令的动作一个都没收进来」，而那看起来就像「这条命令本来就没有子命令」。
// 调用的不是包里的顶层函数（方法、变量里的函数值）时停下——只有注册表里那个
// 处理函数找不到才算分家，那一条由调用方判。
func opsClausesOf(funcs map[string]*ast.FuncDecl, handler string, seen map[string]bool, depth int) []opsClause {
	if seen[handler] {
		return nil
	}
	seen[handler] = true

	fn, ok := funcs[handler]
	if !ok {
		return nil
	}
	sw := opsDispatchSwitch(fn)
	if sw == nil {
		return nil
	}

	var clauses []opsClause
	for _, stmt := range sw.Body.List {
		clause, ok := stmt.(*ast.CaseClause)
		if !ok {
			continue
		}
		var labels []string
		for _, expr := range clause.List {
			label, ok := opsStringValue(expr)
			if !ok || label == "help" || strings.HasPrefix(label, "-") {
				// `help` 与 `-h` / `--help` 是每条命令都有的求助手势，不是动作。
				continue
			}
			labels = append(labels, label)
		}
		if len(labels) == 0 {
			continue
		}
		clauses = append(clauses, opsClause{labels: labels, depth: depth})

		for _, called := range opsRunCalls(clause) {
			clauses = append(clauses, opsClausesOf(funcs, called, seen, depth+1)...)
		}
	}
	return clauses
}

// opsDispatchSwitch 找出一个处理函数里的**分发分支**。
//
// 判据是「标签是变量、每个 case 都是字符串字面量」：分发的标签就是使用者敲的
// 那些词。只认「有 switch」不行——doctor 的处理函数里全是 `switch s` 那种按
// 档位分的 switch，把它们的 case 当动词，表里就会多出一堆状态名。
func opsDispatchSwitch(fn *ast.FuncDecl) *ast.SwitchStmt {
	var found *ast.SwitchStmt
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		if found != nil {
			return false
		}
		sw, ok := node.(*ast.SwitchStmt)
		if !ok || !opsIsDispatch(sw) {
			return true
		}
		found = sw
		return false
	})
	return found
}

// opsIsDispatch 判断一个 switch 是不是分发：标签是变量，且每个 case 的标签
// 都是字符串字面量（至少要有一个）。
func opsIsDispatch(sw *ast.SwitchStmt) bool {
	if sw.Tag == nil {
		return false
	}
	if _, ok := sw.Tag.(*ast.Ident); !ok {
		return false
	}
	labels := 0
	for _, stmt := range sw.Body.List {
		clause, ok := stmt.(*ast.CaseClause)
		if !ok {
			continue
		}
		for _, expr := range clause.List {
			if _, ok := opsStringValue(expr); !ok {
				return false
			}
			labels++
		}
	}
	return labels > 0
}

// opsRunCalls 取一个 case 分支里调用的 run* 函数名。
func opsRunCalls(clause *ast.CaseClause) []string {
	var calls []string
	ast.Inspect(clause, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok || !strings.HasPrefix(id.Name, "run") {
			return true
		}
		calls = append(calls, id.Name)
		return true
	})
	return calls
}

// opsStringValue 取一个字符串字面量的值，不是字符串字面量就返回 false。
func opsStringValue(node ast.Expr) (string, bool) {
	lit, ok := node.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return value, true
}

// verifyLegacyOpsVerbs 检查 legacyOpsVerbs 里每条旧写法在源码里确实与那个正式
// 动词挂在同一个 case 上。
//
// 少了这一步，这份清单就成了唯一的真相：源码里删掉旧写法之后它照样「通过」，
// 而它本来要说明的正是「这两个名字是同一个动作」。
func verifyLegacyOpsVerbs(t *testing.T, command string, clauses []opsClause) {
	t.Helper()

	for oldName, canonical := range legacyOpsVerbs[command] {
		sameClause := false
		for _, clause := range clauses {
			if clause.depth != 0 {
				continue
			}
			if slices.Contains(clause.labels, oldName) && slices.Contains(clause.labels, canonical) {
				sameClause = true
				break
			}
		}
		if !sameClause {
			t.Errorf("%s：旧写法 %q 与正式动词 %q 不在同一个 case 分支里——要么旧写法没了（那就把这一条从 legacyOpsVerbs 里删掉），要么它们已经不是一个动作了",
				command, oldName, canonical)
		}
	}
}
