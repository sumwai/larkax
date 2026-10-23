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

// settingReaderDirs 是「渠道类型 → 可能读它 setting 的目录」，相对本包目录。
//
// 除了实现这个类型的适配器目录，还要算上本包（"."）：协议翻译里共用的那部分
// 放在 internal/channel，多个适配器调同一个函数，读键的下标就在那种函数里
// （见 reasoning_effort.go 与 usage_setting.go）。只列适配器目录的话，读者搬进
// 本包之后这条判据就再也扫不到它——表现恰好是「声明没有人核对」。本包里
// `for key := range setting` 之类的遍历不算读者：只有按下标取某个键才是。
//
// 这张表只属于用例：生产代码按 settingKeysByType 回答「这个 type 认哪些键」，
// 不需要知道哪个目录是实现。正因为它是一份手写的对应关系，用例先把它自己
// 核对一遍——每个适配器目录里必须有一个 `const Type = "<类型>"`，否则下面
// 所有扫描都扫错了地方，而扫错地方的输出与「干净」长得一样（LESSONS.md
// 第 16 条）。
var settingReaderDirs = map[Protocol][]string{
	ProtocolOpenAI:          {".", "openaiadapter"},
	ProtocolAnthropic:       {".", "anthropicadapter"},
	ProtocolOpenAIResponses: {".", "responsesadapter"},
	ProtocolSystemOne:       {"systemoneadapter"},
}

// TestSettingKeysMatchAdapterReaders 检查「表里声明的键」与「代码真读的键」
// 是同一组。两个方向都要查，缺一个方向就会留下一类静默的错：
//
//   - 表里声明了而没人读：管理面替一条写进去也不生效的配置作保，使用者
//     不会再看到任何提示；
//   - 有人读了而表里没写：一个真的生效的键被提示成「没有读者」，使用者照着
//     提示把它删掉，请求路径的行为就变了。
//
// 判法：扫读者目录的源码（注释不算——注释里写着键名，但它不读），把「表里
// 的键」与「代码里按 setting 取值的键」两边各求出来，逐一比对。读者必须按
// 下标取键，或者引用能追到底的常量：`var _ = channel.SettingKey*` 这种
// 不读的引用骗不过这条用例，因为反过来查的是下标集合。
func TestSettingKeysMatchAdapterReaders(t *testing.T) {
	channelConsts := readSettingKeyConsts(t)
	if len(channelConsts) == 0 {
		t.Fatal("本包一个 SettingKey 常量都没扫到：这条判据等于没跑")
	}

	if len(settingReaderDirs) == 0 {
		t.Fatal("settingReaderDirs 是空的：这条判据等于没跑")
	}
	// 表里多出一个类型而没有对应的读者目录时，下面根本不会遍历到它。
	for protocol := range settingKeysByType {
		if _, ok := settingReaderDirs[protocol]; !ok {
			t.Fatalf("表里有类型 %q，settingReaderDirs 里却没有它的读者目录："+
				"这个类型的声明没有任何东西核对", protocol)
		}
	}

	for protocol, dirs := range settingReaderDirs {
		// 表里漏了这一行时 declared 是零值，反方向那一格会红——这正是「漏写
		// 一行表就能被发现」的那条路。
		declared := settingKeysByType[protocol]
		var adapterFiles []*ast.File
		var adapterKeys []string
		var sharedFiles []*ast.File
		var sharedFset *token.FileSet
		for _, dir := range dirs {
			files, fset := parseAdapterDir(t, dir)
			keysHere := adapterSettingIndexKeys(t, files, fset, channelConsts)
			if dir == "." {
				sharedFiles, sharedFset = files, fset
				continue
			}
			if !dirDeclaresType(t, files, protocol) {
				t.Fatalf("%s 里没有 `const Type = %q`：目录与对应用例里的表对不上，"+
					"下面扫到的东西不属于类型 %q", dir, protocol, protocol)
			}
			adapterFiles = append(adapterFiles, files...)
			adapterKeys = append(adapterKeys, keysHere...)
		}
		if sharedFiles != nil && len(adapterFiles) == 0 {
			t.Fatalf("类型 %q 的读者目录只有 \".\"：共存实现算给了谁？"+
				"这里至少要有一个适配器目录来承接它", protocol)
		}
		sharedFuncs := sharedReaderFuncs(t, sharedFiles, sharedFset, channelConsts)
		// referenced 只算**这个类型的适配器真的会走到**的共用读法：
		// 本包里可以同时住着几个协议的共用函数，某个类型只调用其中一部分，
		// 另一部分读的键对它就是无关的（例如 anthropic 不翻译
		// reasoning_effort，那一项不该要求它声明）。按「调用了没有」收窄，
		// 反方向那一格才不会逼着每个共用 "." 的类型把别人的键也登记一遍。
		referenced := append([]string{}, adapterKeys...)
		for name, sharedKeyList := range sharedFuncs {
			if callsChannelFuncWithSetting(adapterFiles, name) {
				referenced = append(referenced, sharedKeyList...)
			}
		}
		// declaredPaths 是表里两层键摊平后的名单，形状与读者扫出来的键一样：
		// 顶层是 usage，内层是 usage.completion_includes_reasoning。
		declaredPaths := map[string]bool{}
		for _, key := range declared.top {
			declaredPaths[string(key)] = true
		}
		for outer, inners := range declared.inner {
			for _, inner := range inners {
				declaredPaths[string(outer)+"."+string(inner)] = true
			}
		}

		// 一、表里声明的每个键都得有人按它取过值，而且取它的那条路要能
		// 从本类型的适配器走到：适配器自己取，或者调用本包里取它的函数。
		// 只看「本包里有人读了」不够——共用函数留在那里、适配器不再调它时，
		// 声明会继续替一条已经断掉的配置作保。
		for _, key := range declared.top {
			if _, ok := constNameOf(channelConsts, string(key)); !ok {
				t.Errorf("表里声明了 %q，但本包没有这个取值的 SettingKey 常量："+
					"代码无法引用它，两个值也就无法被证明是同一个", key)
				continue
			}
			if slices.Contains(adapterKeys, string(key)) {
				continue
			}
			if callsSharedReader(adapterFiles, sharedFuncs, string(key)) {
				continue
			}
			t.Errorf("表里声明类型 %q 认 %q，但读者目录 %v 里没有一条从适配器走到"+
				"它的读法：适配器既没按它取值，也没调用本包里取它的函数",
				protocol, key, dirs)
		}
		// 内层键走同一条判据，只是键名按点号路径对账——读者是在外层那一块
		// 的值上下标，扫出来就是 usage.completion_includes_reasoning。
		for outer, inners := range declared.inner {
			if !slices.Contains(declared.top, outer) {
				t.Errorf("类型 %q 给 %q 声明了内层键，却没把 %q 本身声明成"+
					"它有读者的顶层键：管理面会先把它当成不认识的顶层键点掉",
					protocol, outer, outer)
			}
			for _, inner := range inners {
				path := string(outer) + "." + string(inner)
				if _, ok := constNameOf(channelConsts, string(inner)); !ok {
					t.Errorf("表里给 %q 声明了内层键 %q，但本包没有这个取值的"+
						" SettingKey 常量：代码无法引用它，两个值也就无法被证明是同一个",
						outer, inner)
					continue
				}
				if slices.Contains(adapterKeys, path) {
					continue
				}
				if callsSharedReader(adapterFiles, sharedFuncs, path) {
					continue
				}
				t.Errorf("表里声明类型 %q 认内层键 %q，但读者目录 %v 里没有一条"+
					"从适配器走到它的读法：适配器既没按它取值，也没调用本包里取它的函数",
					protocol, path, dirs)
			}
		}
		// 二、反方向：读者目录里按 setting 取的每个键都得是这张表为它声明的。
		for _, key := range referenced {
			if !declaredPaths[key] {
				t.Errorf("读者目录 %v 里的代码读了 setting 键 %q，但表里没有把它"+
					"声明给类型 %q：管理面会提示使用者这个键没有读者，而它其实生效",
					dirs, key, protocol)
			}
		}
	}
}

// sharedReaderFuncs 找出本包里「函数体按下标取过 setting 键」的函数，返回
// 函数名 → 它取过的键。
//
// 它用来把「共用实现读了某个键」接到「这个类型的适配器调用了它」上：
// 两个适配器共用 ReasoningEffortDialect（见 reasoning_effort.go），只看本包
// 目录里有下标，分不出这条读法服务哪个 type。
func sharedReaderFuncs(
	t *testing.T, files []*ast.File, fset *token.FileSet, channelConsts map[string]string,
) map[string][]string {
	t.Helper()
	if files == nil {
		return nil
	}
	consts := dirConsts(t, files)
	out := map[string][]string{}
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			settingIndexKeys(t, fn.Body, fset, consts, channelConsts, func(key string) {
				out[fn.Name.Name] = append(out[fn.Name.Name], key)
			})
		}
	}
	return out
}

// callsSharedReader 判断适配器有没有调用「取过这个键」的共用函数。
//
// 判的是一次**调用**，而且第一个实参是渠道的 setting：只找选择表达式的话，
// 传一份空 map（键从此没人读）、或者只取函数值不调用，都能让这一格保持绿。
//
// 残留的边界（有意不追）：先把函数值取出来再调（`f := channel.X; f(...)`），
// 或者传另一条渠道的 `.Setting`，这两种都能绕过去。要判它们得做数据流；而
// 这一格要挡的是「这条读法整个断了」，那两种绕法都还在拿某条渠道的 setting
// 按这个键取值，最坏是认错了渠道。
func callsSharedReader(adapterFiles []*ast.File, sharedFuncs map[string][]string, key string) bool {
	for name, keys := range sharedFuncs {
		if !slices.Contains(keys, key) {
			continue
		}
		if callsChannelFuncWithSetting(adapterFiles, name) {
			return true
		}
	}
	return false
}

// callsChannelFuncWithSetting reports whether any file calls `channel.<name>`
// with the channel's setting as the first argument.
//
// 函数值（`var _ = channel.X`、`f := channel.X`）不算：那证明不了这条渠道的
// 键真的被取过。传的东西也得是 setting ——传一个空 map 时翻译整条变恒等，
// 与「没人读」是同一件事。
func callsChannelFuncWithSetting(files []*ast.File, name string) bool {
	found := false
	for _, file := range files {
		ast.Inspect(file, func(node ast.Node) bool {
			if found {
				return false
			}
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "channel" || sel.Sel.Name != name {
				return true
			}
			if len(call.Args) == 0 || !indexesSetting(call.Args[0]) {
				return true
			}
			found = true
			return false
		})
	}
	return found
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
	for _, set := range settingKeysByType {
		for _, key := range set.top {
			declared[string(key)] = true
		}
		for _, inners := range set.inner {
			for _, key := range inners {
				declared[string(key)] = true
			}
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
//   - 多个键里有一个不认识：照样打，且不能因为「有别的键被认」而沉默；
//   - 内层键按点号路径判：写对的静默，拼错的点名，开放映射不点名。
func TestSettingUnreadNotePerType(t *testing.T) {
	const known = string(SettingKeyReasoningEffortMap)
	cases := []struct {
		name    string
		typ     string
		setting map[string]any
		want    []string // 提示里必须逐个点名的键；nil 表示不该有提示
		absent  []string // 提示里不能出现的键
	}{
		{
			name:    "openai 认的键",
			typ:     "openai",
			setting: map[string]any{known: map[string]any{"none": "off"}},
		},
		{
			// 第二个读者：同一个键对两个适配器都有意义。这一格不依赖连库
			// 用例，没有 DSN 时也能跑。
			name:    "openai-responses 也认这个键",
			typ:     "openai-responses",
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
		{
			// 内层键写对时零提示。这一条与「拼错要打」同样重要：正确的配置上
			// 也冒一句，这句话很快就会变成没人读的背景音。
			name:    "usage 的内层键写对",
			typ:     "openai",
			setting: map[string]any{"usage": map[string]any{"completion_includes_reasoning": false}},
		},
		{
			name:    "usage 的内层键写对时 anthropic 也静默",
			typ:     "anthropic",
			setting: map[string]any{"usage": map[string]any{"completion_includes_reasoning": true}},
		},
		{
			name:    "usage 的内层键拼错",
			typ:     "openai",
			setting: map[string]any{"usage": map[string]any{"completion_include_reasoning": false}},
			want:    []string{"usage.completion_include_reasoning"},
		},
		{
			// 三个适配器读的是同一位，点名也要落在各自的 type 上。
			name:    "anthropic 的内层键拼错",
			typ:     "anthropic",
			setting: map[string]any{"usage": map[string]any{"completion_includes_reasonings": false}},
			want:    []string{"usage.completion_includes_reasonings"},
		},
		{
			// reasoning_effort_map 的值是开放映射：那一层的键由上游决定，
			// 没有读者名单可对，不点名。
			name:    "开放映射的内层键不点名",
			typ:     "openai",
			setting: map[string]any{"reasoning_effort_map": map[string]any{"minimal": "low"}},
		},
		{
			// 整块不是对象时按「没声明」处理：值的形状没有判据，也就不点名。
			name:    "usage 是字符串",
			typ:     "openai",
			setting: map[string]any{"usage": "completion_includes_reasoning=false"},
		},
		{
			name:    "usage 是数组",
			typ:     "openai",
			setting: map[string]any{"usage": []any{false}},
		},
		{
			name:    "usage 是数字",
			typ:     "openai",
			setting: map[string]any{"usage": 0},
		},
		{
			// 整块没有读者时只点这一块的名字：内层键的「读者」是挂在外层
			// 上的，逐个点开只会把同一处错说三遍。
			name:    "整块没有读者时不点它的内层",
			typ:     "anthropic",
			setting: map[string]any{"future_block": map[string]any{"anything": 1}},
			want:    []string{"future_block"},
			absent:  []string{"future_block.anything"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			note := SettingUnreadNote(tc.typ, tc.setting)
			for _, key := range tc.absent {
				if strings.Contains(note, "「"+key+"」") {
					t.Errorf("提示里不该点名 %q：%s", key, note)
				}
			}
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
			// 一句话里要有渠道类型与两种可能的原因：只说「不支持」的话，
			// 拼错键名的人会以为等某个适配器实现就好了。
			for _, want := range []string{"type=" + tc.typ, "不会影响请求路径", "拼错", "还没接"} {
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

// adapterSettingIndexKeys 找出代码里按 setting 取值的那些键。
//
// 认的形状：`setting[...]`、`Setting[...]` 与 `x.Setting[...]` 三种下标
// 表达式，以及先从 setting 取出一块、再从那一块按下标取的内层键（见
// settingIndexKeys）。下标必须是字符串字面量，或者能追到底的包级常量——
// 追不到就判失败，而不是放过：换成变量之后这条用例什么都看不出来，表现为
// 「加了一个没人读的声明也不会红」，而那正是它要挡的那类错。
func adapterSettingIndexKeys(
	t *testing.T, files []*ast.File, fset *token.FileSet, channelConsts map[string]string,
) []string {
	t.Helper()
	consts := dirConsts(t, files)
	var keys []string
	for _, file := range files {
		for _, decl := range file.Decls {
			// 函数体单独扫：内层键靠局部变量认，变量名只在它自己那个函数
			// 里有效，跨函数借用会把别的 map 下标也算进来。
			if fn, ok := decl.(*ast.FuncDecl); ok {
				if fn.Body != nil {
					settingIndexKeys(t, fn.Body, fset, consts, channelConsts, func(key string) {
						keys = append(keys, key)
					})
				}
				continue
			}
			settingIndexKeys(t, decl, fset, consts, channelConsts, func(key string) {
				keys = append(keys, key)
			})
		}
	}
	return keys
}

// settingIndexKeys 把一段代码里「按下标取 setting 的键」逐个交给 emit。
//
// 两层都认：
//
//   - 直接取：`setting[<键>]`、`x.Setting[<键>]`，交出去的是键名本身；
//   - 取一块再取：`t, ok := setting[<键>].(map[string]any)` 之后
//     `t[<内层键>]`，交出去的是 `外层.内层`。
//
// 第二层不能省：内层键的读取处把块先收进局部变量（见
// UsageCompletionIncludesReasoning），只认直接下标的话，内层键对「声明与
// 读者一致」这条判据是隐形的——表里给一个谁都不读的内层键作保也照样绿。
func settingIndexKeys(
	t *testing.T, body ast.Node, fset *token.FileSet,
	consts map[string]ast.Expr, channelConsts map[string]string, emit func(string),
) {
	t.Helper()
	tables := settingTables(t, body, fset, consts, channelConsts)
	ast.Inspect(body, func(node ast.Node) bool {
		index, ok := node.(*ast.IndexExpr)
		if !ok {
			return true
		}
		if indexesSetting(index.X) {
			emit(resolveSettingKey(t, index, fset, consts, channelConsts))
			return true
		}
		ident, ok := index.X.(*ast.Ident)
		if !ok {
			return true
		}
		outer, ok := tables[ident.Name]
		if !ok {
			return true
		}
		// 内层键可以是变量：开放映射按请求里的取值查表（见
		// ReasoningEffortDialect 的 table[effort]）。追不出来的越过了就算
		// 没读——它不是声明里的某个键，反方向判不出东西来；而声明了却没人
		// 按常量读的键，正方向那一格仍然会红。
		key, ok := resolveKeyExpr(t, index.Index, consts, channelConsts, 0)
		if !ok {
			return true
		}
		emit(outer + "." + key)
		return true
	})
}

// settingTables 找出「从 setting 的某一层断言出来的 map」的局部变量 → 它对应
// 的外层键。
//
// 认的形状：`t, ok := setting[<键>].(map[string]any)`（含不带 ok 的直接赋值）。
// 断言成别的类型不算：把一个值断言成字符串之后不会再按 setting 的键取。
func settingTables(
	t *testing.T, body ast.Node, fset *token.FileSet,
	consts map[string]ast.Expr, channelConsts map[string]string,
) map[string]string {
	t.Helper()
	out := map[string]string{}
	ast.Inspect(body, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, rhs := range assign.Rhs {
			assert, ok := rhs.(*ast.TypeAssertExpr)
			if !ok || !settingTableType(assert.Type) || i >= len(assign.Lhs) {
				continue
			}
			index, ok := assert.X.(*ast.IndexExpr)
			if !ok || !indexesSetting(index.X) {
				continue
			}
			name, ok := assign.Lhs[i].(*ast.Ident)
			if !ok {
				continue
			}
			out[name.Name] = resolveSettingKey(t, index, fset, consts, channelConsts)
		}
		return true
	})
	return out
}

// settingTableType 判断一个类型表达式是不是 setting 那一块的值类型。
func settingTableType(expr ast.Expr) bool {
	mapType, ok := expr.(*ast.MapType)
	if !ok {
		return false
	}
	key, ok := mapType.Key.(*ast.Ident)
	if !ok || key.Name != "string" {
		return false
	}
	switch value := mapType.Value.(type) {
	case *ast.Ident:
		return value.Name == "any"
	case *ast.InterfaceType:
		return len(value.Methods.List) == 0
	}
	return false
}

// resolveSettingKey 追出下标表达式里的键，追不出就判失败。
func resolveSettingKey(
	t *testing.T, index *ast.IndexExpr, fset *token.FileSet,
	consts map[string]ast.Expr, channelConsts map[string]string,
) string {
	t.Helper()
	key, ok := resolveKeyExpr(t, index.Index, consts, channelConsts, 0)
	if !ok {
		pos := fset.Position(index.Pos())
		t.Fatalf("%s:%d 按 setting 取了键，但用例追不出它是什么键："+
			"请写成字符串字面量或引用 channel.SettingKey* 常量，否则"+
			"「声明与读者一致」这条判据对它等于不存在", pos.Filename, pos.Line)
	}
	return key
}

// indexesSetting 判断下标表达式取的是不是渠道的 setting。
func indexesSetting(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.Ident:
		// 翻译函数把 setting 收成同名参数再取键
		// （见 reasoning_effort.go 的 ReasoningEffortDialect）。
		return e.Name == "setting" || e.Name == "Setting"
	case *ast.SelectorExpr:
		return e.Sel.Name == "Setting"
	}
	return false
}

// dirConsts 收集一个目录里的包级常量（能静态求出来的那些）。
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
