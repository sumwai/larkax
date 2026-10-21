package channel

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// settingAdapterDirs 是「渠道类型 → 实现它的适配器目录」，相对本包目录。
//
// 这张表只属于用例：生产代码按 settingKeysByType 回答「这个 type 认哪些键」，
// 不需要知道哪个目录是实现。正因为它是一份手写的对应关系，用例先把它自己
// 核对一遍——每个目录里必须有一个 `const Type = "<类型>"`，否则下面所有扫描
// 都扫错了地方，而扫错地方的输出与「干净」长得一样（LESSONS.md 第 16 条）。
var settingAdapterDirs = map[Protocol]string{
	ProtocolOpenAI:          "openaiadapter",
	ProtocolAnthropic:       "anthropicadapter",
	ProtocolOpenAIResponses: "responsesadapter",
	ProtocolSystemOne:       "systemoneadapter",
}

// TestSettingKeysMatchAdapterReaders 检查「表里声明的键」与「适配器真读的键」
// 是同一组。两个方向都要查，缺一个方向就会留下一类静默的错：
//
//   - 表里声明了而适配器没读：管理面替一条写进去也不生效的配置作保，使用者
//     不会再看到任何提示；
//   - 适配器读了而表里没写：一个真的生效的键被提示成「没有读者」，使用者照着
//     提示把它删掉，请求路径的行为就变了。
//
// 判法：扫适配器源码（注释不算——注释里写着键名，但它不读），把「表里的键」
// 与「代码里按 setting 取值的键」两边各求出来，逐一比对。适配器取值必须走
// channel.SettingKey* 常量或与之相同的字面量：换一种写法（变量、函数返回值）
// 会让这条用例判不出来，所以它直接失败并要求把写法改成可核对的那种。
func TestSettingKeysMatchAdapterReaders(t *testing.T) {
	channelConsts := readSettingKeyConsts(t)
	if len(channelConsts) == 0 {
		t.Fatal("本包一个 SettingKey 常量都没扫到：这条判据等于没跑")
	}

	// 一、每个声明过的类型都要能对上目录，且目录里真的读那个键。
	if len(settingKeysByType) == 0 {
		t.Fatal("settingKeysByType 是空的：这条判据等于没跑")
	}
	for protocol, keys := range settingKeysByType {
		dir, ok := settingAdapterDirs[protocol]
		if !ok {
			t.Fatalf("类型 %q 在用例的目录表里没有对应项，扫描无从下手", protocol)
		}
		files, fset := parseAdapterDir(t, dir)
		if !dirDeclaresType(t, files, protocol) {
			t.Fatalf("%s 里没有 `const Type = %q`：目录与对应用例里的表对不上，"+
				"下面扫到的东西不属于类型 %q", dir, protocol, protocol)
		}

		referenced := adapterSettingIndexKeys(t, files, fset, channelConsts)
		for _, key := range keys {
			name, ok := constNameOf(channelConsts, string(key))
			if !ok {
				t.Errorf("表里声明了 %q，但本包没有这个取值的 SettingKey 常量："+
					"适配器无法引用它，两个值也就无法被证明是同一个", key)
				continue
			}
			if !referencesChannelConst(files, name) {
				t.Errorf("表里声明类型 %q 认 %q（%s），但 %s 的源码里没有引用 "+
					"channel.%s，也没有等值的字面量：声明可能在替一条不会生效的配置作保",
					protocol, key, name, dir, name)
			}
		}
		// 三、反方向：适配器按 setting 取的每个键都得是这张表为它声明的。
		for _, key := range referenced {
			if !slices.Contains(keys, SettingKey(key)) {
				t.Errorf("%s 读了 setting 键 %q，但表里没有把它声明给类型 %q："+
					"管理面会提示使用者这个键没有读者，而它其实生效",
					dir, key, protocol)
			}
		}
	}
}

// TestSettingKeysMissingFromTable 堵住「加了常量、忘了加表」这一格。
//
// 上面那条用例只遍历表里的行：一个从没进表的常量对它是隐形的，而那种常量
// 恰恰是「适配器要读、管理面不知道」的那一半。所以再从那批常量出发查一遍。
func TestSettingKeysMissingFromTable(t *testing.T) {
	consts := readSettingKeyConsts(t)
	if len(consts) == 0 {
		t.Fatal("本包一个 SettingKey 常量都没扫到：这条判据等于没跑")
	}
	declared := map[string]bool{}
	for _, keys := range settingKeysByType {
		for _, key := range keys {
			declared[string(key)] = true
		}
	}
	for name, value := range consts {
		if !declared[value] {
			t.Errorf("%s = %q 是 SettingKey 常量，却没进 settingKeysByType：\n"+
				"适配器一旦引用它，管理面仍会把这条渠道上的该键提示成没有读者", name, value)
		}
	}
}

// TestSettingTypesCoverEveryProtocol 确保表覆盖了本包认的全部类型。
//
// 少一行不会让任何东西报错：那个类型会被 unreadSettingKeys 当成「本版本不
// 认识的 type」跳过，症状恰好是这张表要治的那种——键写错了也悄无声息。
// 类型的来源是 protocolEndpoints（它本身由 TestEveryProtocolConstantIsInTable
// 盯着），所以这里不另列一份清单。
func TestSettingTypesCoverEveryProtocol(t *testing.T) {
	if len(protocolEndpoints) == 0 {
		t.Fatal("protocolEndpoints 是空的：这条判据等于没跑")
	}
	for protocol := range protocolEndpoints {
		if _, ok := settingKeysByType[protocol]; !ok {
			t.Errorf("protocolEndpoints 里有 %q，settingKeysByType 里却没有它那一行："+
				"这个类型的渠道配了键也不会被提示", protocol)
		}
	}
	for protocol := range settingKeysByType {
		if _, ok := protocolEndpoints[protocol]; !ok {
			t.Errorf("settingKeysByType 里有 %q，但它不是一个协议取值："+
				"那一行永远不会被查到", protocol)
		}
	}
}

// TestRouteDoesNotReadSetting 守住 Channel.Setting 注释里的那条契约：
// 键归适配器解释，选路层与传输层不认。
//
// 一处读键的选路代码会让「这个键由哪个 type 的适配器解释」变成两套判据，
// 而 settingKeysByType 只说得清后者。出现 .Setting 就判失败——新的读者要么
// 属于适配器，要么说明这条契约该改了（那时连同本用例一起改）。
func TestRouteDoesNotReadSetting(t *testing.T) {
	for _, dir := range []string{filepath.Join("..", "route"), filepath.Join("httpx")} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("读目录 %s 失败: %v", dir, err)
		}
		scanned := 0
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			src, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatalf("读 %s/%s 失败: %v", dir, name, err)
			}
			scanned++
			for i, line := range strings.Split(string(src), "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "//") {
					continue
				}
				if strings.Contains(line, ".Setting") {
					t.Errorf("%s/%s:%d 读了 .Setting，但键只归适配器解释：\n  %s",
						dir, name, i+1, strings.TrimSpace(line))
				}
			}
		}
		if scanned == 0 {
			t.Fatalf("%s 里一个 .go 文件都没扫到：这条判据等于没跑", dir)
		}
	}
}

// TestSettingUnreadNotePerType 逐条验证四类判据，都是使用者在命令行上看得见的：
//
//   - 这个 type 认的键：不打提示；
//   - 别的 type 的键：打，并点名；
//   - 拼错的键：打，并点名；
//   - 多个键里有一个不认识：照样打，且不能因为「有别的键被认」而沉默。
func TestSettingUnreadNotePerType(t *testing.T) {
	const known = string(SettingKeyReasoningEffortMap)
	cases := []struct {
		name    string
		typ     string
		setting map[string]any
		want    []string // 提示里必须逐个点名的键；nil 表示不该有提示
	}{
		{
			name:    "openai 认的键",
			typ:     "openai",
			setting: map[string]any{known: map[string]any{"none": "off"}},
		},
		{
			name:    "别的 type 的键",
			typ:     "anthropic",
			setting: map[string]any{known: map[string]any{"none": "off"}},
			want:    []string{known},
		},
		{
			name:    "拼错的键",
			typ:     "openai",
			setting: map[string]any{"reasoning_effortmap": map[string]any{"none": "off"}},
			want:    []string{"reasoning_effortmap"},
		},
		{
			name:    "多个键里有一个不认识",
			typ:     "openai",
			setting: map[string]any{known: map[string]any{"none": "off"}, "future_key": true},
			want:    []string{"future_key"},
		},
		{
			name:    "多个不认识的键按字典序点名",
			typ:     "anthropic",
			setting: map[string]any{"zeta": 1, "alpha": 2},
			want:    []string{"alpha", "zeta"},
		},
		{
			name:    "空 setting",
			typ:     "openai",
			setting: map[string]any{},
		},
		{
			// 未知类型不是「所有键都没有读者」，而是「判不了」：那种渠道本来
			// 就选不中，再点它一句「键没读者」会把注意力引到错的地方。
			name:    "未知类型不判",
			typ:     "no-such-type",
			setting: map[string]any{"whatever": 1},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			note := SettingUnreadNote(tc.typ, tc.setting)
			if len(tc.want) == 0 {
				if note != "" {
					t.Fatalf("不该有提示，得到 %q", note)
				}
				return
			}
			if note == "" {
				t.Fatal("应当有提示，却得到空串")
			}
			// 点名要能一眼数出来：每个键各带一层「」，不是只出现子串。
			prev := -1
			for _, key := range tc.want {
				quoted := "「" + key + "」"
				at := strings.Index(note, quoted)
				if at < 0 {
					t.Fatalf("提示里没有点名键 %s：%s", quoted, note)
				}
				if at < prev {
					t.Fatalf("键 %s 出现在前一个键之前，提示里的顺序不稳定：%s", quoted, note)
				}
				prev = at
			}
			// 被认的键不能也出现在名单里——那会让使用者以为它同样没生效。
			if !slices.Contains(tc.want, known) && strings.Contains(note, "「"+known+"」") {
				t.Errorf("提示把有读者的键 %q 也点成了没有读者：%s", known, note)
			}
			// 一句话里要有渠道类型与下一步：只说「不支持」的话，使用者不知道
			// 换成哪个 type 才有用，也不知道该等什么。
			for _, want := range []string{"type=" + tc.typ, "不会影响请求路径", "生效"} {
				if !strings.Contains(note, want) {
					t.Errorf("提示里没有 %q：%s", want, note)
				}
			}
		})
	}
}

// ── 源码扫描 ────────────────────────────────────────────────────────────

// readSettingKeyConsts 读出本包里 `常量名 SettingKey = "取值"` 那一批声明。
//
// 扫源码而不是让用例自己列一份：列一份的话，新加常量时用例也得跟着改，
// 而「用例漏掉了新常量」与「新常量被正确处理」在输出上是一样的。
func readSettingKeyConsts(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("读本包目录失败: %v", err)
	}
	fset := token.NewFileSet()
	out := map[string]string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("解析 %s 失败: %v", name, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				if ident, ok := value.Type.(*ast.Ident); !ok || ident.Name != "SettingKey" {
					continue
				}
				for i, id := range value.Names {
					if i >= len(value.Values) {
						continue
					}
					lit, ok := value.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						t.Fatalf("%s 的取值不是字符串字面量：用例判不了它的值", id.Name)
					}
					out[id.Name] = unquote(t, lit.Value)
				}
			}
		}
	}
	return out
}

// parseAdapterDir 解析一个适配器目录下的非测试 .go 文件。
func parseAdapterDir(t *testing.T, dir string) (files []*ast.File, fset *token.FileSet) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读目录 %s 失败: %v", dir, err)
	}
	fset = token.NewFileSet()
	scanned := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if parseErr != nil {
			t.Fatalf("解析 %s 失败: %v", filepath.Join(dir, name), parseErr)
		}
		files = append(files, file)
		scanned++
	}
	if scanned == 0 {
		t.Fatalf("%s 里一个 .go 文件都没扫到：这条判据等于没跑", dir)
	}
	return files, fset
}

// dirDeclaresType 确认目录里有一个 `const Type = "<protocol>"`。
func dirDeclaresType(t *testing.T, files []*ast.File, protocol Protocol) bool {
	t.Helper()
	for _, file := range files {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok || len(value.Names) != len(value.Values) || len(value.Names) != 1 || value.Names[0].Name != "Type" {
					continue
				}
				lit, ok := value.Values[0].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING || unquote(t, lit.Value) != string(protocol) {
					continue
				}
				return true
			}
		}
	}
	return false
}

// referencesChannelConst reports whether any file selects `channel.<name>`.
func referencesChannelConst(files []*ast.File, name string) bool {
	found := false
	for _, file := range files {
		ast.Inspect(file, func(node ast.Node) bool {
			if found {
				return false
			}
			sel, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if ok && pkg.Name == "channel" && sel.Sel.Name == name {
				found = true
				return false
			}
			return true
		})
	}
	return found
}

// adapterSettingIndexKeys 找出适配器按 setting 取值的那些键。
//
// 认的形状：`setting[...]`、`Setting[...]` 与 `x.Setting[...]` 三种下标
// 表达式。下标必须是字符串字面量，或者能追到底的包级常量——追不到就判失败，
// 而不是放过：换成变量之后这条用例什么都看不出来，表现为「加了一个没人读的
// 声明也不会红」，而那正是它要挡的那类错。
func adapterSettingIndexKeys(
	t *testing.T, files []*ast.File, fset *token.FileSet, channelConsts map[string]string,
) []string {
	t.Helper()
	consts := dirConsts(t, files)
	var keys []string
	for _, file := range files {
		ast.Inspect(file, func(node ast.Node) bool {
			index, ok := node.(*ast.IndexExpr)
			if !ok || !indexesSetting(index.X) {
				return true
			}
			key, ok := resolveKeyExpr(t, index.Index, consts, channelConsts, 0)
			if !ok {
				pos := fset.Position(index.Pos())
				t.Fatalf("%s:%d 按 setting 取了键，但用例追不出它是什么键："+
					"请写成字符串字面量或引用 channel.SettingKey* 常量，否则"+
					"「声明与读者一致」这条判据对它等于不存在", pos.Filename, pos.Line)
			}
			keys = append(keys, key)
			return true
		})
	}
	return keys
}

// indexesSetting 判断下标表达式取的是不是渠道的 setting。
func indexesSetting(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.Ident:
		// 适配器里的统一写法是给 setting 一个局部名字再取键
		// （见 openaiadapter 的 reasoningEffortDialect）。
		return e.Name == "setting" || e.Name == "Setting"
	case *ast.SelectorExpr:
		return e.Sel.Name == "Setting"
	}
	return false
}

// dirConsts 收集一个适配器包里的包级常量（能静态求出来的那些）。
func dirConsts(t *testing.T, files []*ast.File) map[string]ast.Expr {
	t.Helper()
	out := map[string]ast.Expr{}
	for _, file := range files {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, id := range value.Names {
					if i < len(value.Values) {
						out[id.Name] = value.Values[i]
					}
				}
			}
		}
	}
	return out
}

// resolveKeyExpr 把一个下标表达式追成键的字符串值。
func resolveKeyExpr(
	t *testing.T, expr ast.Expr, consts map[string]ast.Expr, channelConsts map[string]string, depth int,
) (string, bool) {
	t.Helper()
	if depth > 8 {
		return "", false
	}
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		return unquote(t, e.Value), true
	case *ast.Ident:
		value, ok := consts[e.Name]
		if !ok {
			return "", false
		}
		return resolveKeyExpr(t, value, consts, channelConsts, depth+1)
	case *ast.CallExpr:
		// string(channel.SettingKeyX)：常量转换，取参数继续追。
		if len(e.Args) != 1 {
			return "", false
		}
		fn, ok := e.Fun.(*ast.Ident)
		if !ok || fn.Name != "string" {
			return "", false
		}
		return resolveKeyExpr(t, e.Args[0], consts, channelConsts, depth+1)
	case *ast.SelectorExpr:
		pkg, ok := e.X.(*ast.Ident)
		if !ok || pkg.Name != "channel" {
			return "", false
		}
		value, ok := channelConsts[e.Sel.Name]
		if !ok {
			return "", false
		}
		return value, true
	}
	return "", false
}

// constNameOf 反查取值对应的常量名。
func constNameOf(consts map[string]string, value string) (string, bool) {
	for name, v := range consts {
		if v == value {
			return name, true
		}
	}
	return "", false
}

func unquote(t *testing.T, lit string) string {
	t.Helper()
	out, err := strconv.Unquote(lit)
	if err != nil {
		t.Fatalf("解不出字面量 %s: %v", lit, err)
	}
	return out
}
