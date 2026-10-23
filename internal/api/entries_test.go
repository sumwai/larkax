package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// 这个文件钉住**入口分派表**，因为入站别名只在其中两条上接得住。
//
// # 为什么要有这条用例
//
// 解析接在 serve 与 serveDecision 里，而它们是仅有的两处会按模型名选路并
// 计费的入口。新增第五条入口时，最省事的写法是照抄 serve 的骨架、忘了接
// 解析——症状是「别名在那一条上配了不生效」，其余路径全绿，而只有写别名的
// 那个客户端会撞上。表在这里逐行断言，加一行就会红，红在「表变了」这件事
// 上，而不是等某个客户端报了「查无此模型」才发现。
//
// 断言的是**分派表本身**（哪个路径交给哪个函数），不是「有几条」这个数字：
// 数字会漂，函数名不会。新增一条只读路径（像 serveModels 那样不选路不
// 计费）也要来这里登记，那是它该付出的代价——登记的时候顺手就能看见
// 「它要不要解析别名」。
//
// 读源文本而不是跑请求：分派表是编译期常量，跑请求只能覆盖到造得出请求的
// 那几条，而漏掉的那一条恰恰是没人写请求的那条。
func TestServeHTTPDispatchesEveryEntry(t *testing.T) {
	// 行按路径排序（`<default>` 排在最前），这样 diff 只显示真正变了的那些行。
	const want = `<default>           -> serveModel
PathChatCompletions -> serve
PathMessages        -> serve
PathModels          -> serveModels
PathResponses       -> serve
PathSystemOne       -> serveDecision
`

	got := serveHTTPDispatch(t)
	if got != want {
		t.Errorf("入口分派表变了：\n得到:\n%s\n期望:\n%s\n"+
			"新增入口时先问一句：它会按模型名选路并计费吗？会的话解析别名要接上"+
			"（见 serve 与 serveDecision），并且把这里的期望一起改掉。", got, want)
	}
}

// serveHTTPDispatch 读出 ServeHTTP 的 switch，排成「路径 -> 处理函数」的文本。
//
// default 那一支不按常量分派（模型 id 里可能带 "/"，写成 case 列不出来），
// 它走 modelIDFromPath + serveModel，这里按「<default>」一行记下。
func serveHTTPDispatch(t *testing.T) string {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "api.go", nil, 0)
	if err != nil {
		t.Fatalf("解析 api.go 失败: %v", err)
	}

	var fn *ast.FuncDecl
	for _, d := range file.Decls {
		decl, ok := d.(*ast.FuncDecl)
		if !ok || decl.Name.Name != "ServeHTTP" || decl.Recv == nil {
			continue
		}
		fn = decl
	}
	if fn == nil {
		t.Fatal("api.go 里找不到 Handler.ServeHTTP：分派表挪走了，这条用例得跟着改")
	}

	var sw *ast.SwitchStmt
	ast.Inspect(fn, func(n ast.Node) bool {
		s, ok := n.(*ast.SwitchStmt)
		if !ok || sw != nil {
			return sw == nil
		}
		// 认的是 `switch r.URL.Path`，不是 switch 里的任何别的分支。
		if sel, isSel := s.Tag.(*ast.SelectorExpr); isSel && sel.Sel.Name == "Path" {
			sw = s
			return false
		}
		return true
	})
	if sw == nil {
		t.Fatal("ServeHTTP 里没有按 r.URL.Path 的 switch：分派表换了写法，这条用例得跟着改")
	}

	type row struct{ path, fn string }
	rows := make([]row, 0, len(sw.Body.List))
	for _, stmt := range sw.Body.List {
		clause, ok := stmt.(*ast.CaseClause)
		if !ok {
			continue
		}
		if clause.List == nil {
			// default 那一支。
			rows = append(rows, row{"<default>", calledFuncIn(clause.Body, "serveModel")})
			continue
		}
		for _, expr := range clause.List {
			// 两种写法都认：常量名（本包现在的写法）与字面量（临时加一条
			// 路径时常见的写法）。认不出时不要静默跳过——跳过的那一行正是
			// 新加的那条入口，而这条用例存在的理由就是让新加的那行显形。
			switch e := expr.(type) {
			case *ast.Ident:
				rows = append(rows, row{e.Name, firstHandlerCall(clause.Body)})
			case *ast.BasicLit:
				lit, err := strconv.Unquote(e.Value)
				if err != nil {
					t.Fatalf("case 里的字面量 %s 解不开: %v", e.Value, err)
				}
				rows = append(rows, row{strconv.Quote(lit), firstHandlerCall(clause.Body)})
			default:
				t.Fatalf("case 里出现了 %T：分派表改成别的形状了，这条用例得跟着改", expr)
			}
		}
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].path < rows[j].path })
	var b strings.Builder
	for _, r := range rows {
		b.WriteString(r.path)
		b.WriteString(strings.Repeat(" ", 20-len(r.path)))
		b.WriteString("-> ")
		b.WriteString(r.fn)
		b.WriteString("\n")
	}
	return b.String()
}

// firstHandlerCall 返回一段 case 体里第一次调用的 h.<方法> 的方法名。
//
// 只认 `h.xxx(...)` 这一种形状：分派表里每个 case 的第一句都是它，其余写法
// （转给别的对象、先算一个值再调）说明分派形状变了，那时该重新想这条用例，
// 而不是让它在猜。
func firstHandlerCall(body []ast.Stmt) string {
	var name string
	for _, stmt := range body {
		ast.Inspect(stmt, func(n ast.Node) bool {
			if name != "" {
				return false
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, isSel := call.Fun.(*ast.SelectorExpr)
			if !isSel {
				return true
			}
			if recv, isIdent := sel.X.(*ast.Ident); isIdent && recv.Name == "h" {
				name = sel.Sel.Name
				return false
			}
			return true
		})
		if name != "" {
			return name
		}
	}
	return "?"
}

// calledFuncIn 在 default 那一支里找有没有调用某个函数，返回它的名字。
//
// default 先按前缀判路径、再调 serveModel，中间隔着 modelIDFromPath 的返回，
// 所以这里只回答「这一支最终交给谁」。
func calledFuncIn(body []ast.Stmt, want string) string {
	found := ""
	for _, stmt := range body {
		ast.Inspect(stmt, func(n ast.Node) bool {
			if found != "" {
				return false
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, isSel := call.Fun.(*ast.SelectorExpr)
			if !isSel {
				return true
			}
			if recv, isIdent := sel.X.(*ast.Ident); isIdent && recv.Name == "h" && sel.Sel.Name == want {
				found = want
			}
			return true
		})
	}
	if found == "" {
		return "?"
	}
	return found
}

// 分派表里出现的路径常量必须真的存在，且值就是文档里那几条。
//
// 只断言常量名不够：把 PathMessages 的值改成 "/v1/chat/completions" 时，
// 上面那条用例照样绿（它看的是常量名），而两条路径撞在一起的表现是
// 「/v1/messages 回来的是 OpenAI 形状」。
func TestEntryPathsMatchTheirDocuments(t *testing.T) {
	want := map[string]string{
		"PathChatCompletions": "/v1/chat/completions",
		"PathMessages":        "/v1/messages",
		"PathResponses":       "/v1/responses",
		"PathSystemOne":       "/v1/systemone",
		"PathModels":          "/v1/models",
	}
	got := map[string]string{
		"PathChatCompletions": PathChatCompletions,
		"PathMessages":        PathMessages,
		"PathResponses":       PathResponses,
		"PathSystemOne":       PathSystemOne,
		"PathModels":          PathModels,
	}
	for name, wantPath := range want {
		if gotPath := got[name]; gotPath != wantPath {
			t.Errorf("%s = %s，期望 %s", name, strconv.Quote(gotPath), strconv.Quote(wantPath))
		}
	}
}
