package expr

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// TestCompileScaleLimits 验证三层规模闸门。
//
// 这三条是「商家可写的输入」这个威胁模型里的第一道闸门，
// 每一条都要能在一个恶意规则上生效，而不是只在明显离谱的输入上生效。
func TestCompileScaleLimits(t *testing.T) {
	// 源码长度上限：在解析之前就拦住。
	huge := `{"prompt": 1.0, "comment": "` + strings.Repeat("x", maxSourceBytes) + `"}`
	err := compileErr(t, huge)
	if err.Reason != ReasonScale {
		t.Errorf("超长源码的原因 = %s，期望 %s", err.Reason, ReasonScale)
	}

	// 节点数上限：一条几百字节的表达式就能展开出很多节点。
	many := `{"prompt": 1.0`
	for i := 0; i < 600; i++ {
		many += ` + 0.0`
	}
	many += `}`
	err = compileErr(t, many)
	if err.Reason != ReasonSyntax && err.Reason != ReasonScale {
		t.Errorf("节点过多应当被 MaxNodes 拦下（expr 报为编译失败），得到 %s: %v",
			err.Reason, err.Err)
	}

	// 嵌套深度：括号不产生节点，MaxNodes 拦不住它，必须由 checkNesting 挡。
	deep := strings.Repeat("(", maxNesting+2) + "1.0" + strings.Repeat(")", maxNesting+2)
	err = compileErr(t, `{"prompt": `+deep+`}`)
	if err.Reason != ReasonScale {
		t.Errorf("过深嵌套的原因 = %s，期望 %s", err.Reason, ReasonScale)
	}
}

// TestCompileAcceptsExactlyAtSourceLimit 验证源码长度上限那一格算通过。
//
// 上限是「超过才拒」：恰好压在上限上的规则与上限之下的规则没有区别，
// 区别只在多一个字节的时候。没有用例看着这一格的话，比较符挪一格也
// 不会有人发现——一条正好 4096 字节的规则会开始报「源码超限」，
// 而那条报错看起来完全正常。
func TestCompileAcceptsExactlyAtSourceLimit(t *testing.T) {
	at := `{"prompt": 1.0` + strings.Repeat(" ", maxSourceBytes-len(`{"prompt": 1.0`)-1) + `}`
	if len(at) != maxSourceBytes {
		t.Fatalf("夹具不在上限上：%d 字节，上限 %d", len(at), maxSourceBytes)
	}
	if _, err := Compile(at); err != nil {
		t.Errorf("恰好 %d 字节的规则应当编得过，得到 %v", maxSourceBytes, err)
	}

	// 多一个字节就该拒，而且拒在长度这一关而不是语法。
	err := compileErr(t, at+" ")
	if err.Reason != ReasonScale {
		t.Errorf("多一个字节的原因 = %s，期望 %s", err.Reason, ReasonScale)
	}
}

// TestCompileAcceptsExactlyAtNestingLimit 验证嵌套深度到上限那一格算通过。
//
// 层数由括号与最外层那对花括号一起数：外层 `{` 自己占一层，所以括号给到
// maxNesting-1 对时正好压在上限上。上限之内与上限之上只差一层，
// 也就是一对括号——比较符挪一格，一条本来合法的规则会开始报「嵌套过深」。
//
// 变异：这一格对应的是 `maxDepth > maxNesting` 那条判据；同一段里还有一处
// `if depth > maxDepth { maxDepth = depth }`（记运行中的最大深度），
// 它的 `>` 改成 `>=` 是**等价变异**：边界那一格（depth 正好等于 maxDepth）
// 赋的还是同一个数，没有哪一组输入能让两版不同，抓不到。
func TestCompileAcceptsExactlyAtNestingLimit(t *testing.T) {
	at := `{"prompt": ` + strings.Repeat("(", maxNesting-1) + "1.0" +
		strings.Repeat(")", maxNesting-1) + `}`
	if _, err := Compile(at); err != nil {
		t.Errorf("嵌套 %d 层（上限）的规则应当编得过，得到 %v", maxNesting, err)
	}

	// 多一层就该拒。
	over := `{"prompt": ` + strings.Repeat("(", maxNesting) + "1.0" +
		strings.Repeat(")", maxNesting) + `}`
	err := compileErr(t, over)
	if err.Reason != ReasonScale {
		t.Errorf("嵌套 %d 层的原因 = %s，期望 %s", maxNesting+1, err.Reason, ReasonScale)
	}
}

// TestCompileNestingIsCheckedBeforeParsing 说明深度检查为什么按字符扫。
//
// expr 的解析器是递归下降的，几千层括号会把递归压到把 goroutine 的栈打穿。
// 这条测试不真的去撞栈溢出（那会把整个测试进程带走），
// 而是钉住「超限之前就报错」这个行为：一个远小于崩溃阈值的深度
// 也必须被拒绝，且拒绝必须发生在解析之前。
func TestCompileNestingIsCheckedBeforeParsing(t *testing.T) {
	for _, n := range []int{maxNesting + 1, 200, 2000} {
		deep := strings.Repeat("(", n) + "1.0" + strings.Repeat(")", n)
		err := compileErr(t, `{"prompt": `+deep+`}`)
		if err.Reason != ReasonScale {
			t.Errorf("深度 %d 的原因 = %s，期望 %s", n, err.Reason, ReasonScale)
		}
	}

	// 括号不配对也要在解析之前给出可读的错误。
	err := compileErr(t, `{"prompt": (1.0}`)
	if err.Reason != ReasonScale {
		t.Errorf("括号不配对的原因 = %s，期望 %s", err.Reason, ReasonScale)
	}
	if !strings.Contains(err.Err.Error(), "配对") {
		t.Errorf("错误信息应当说明括号不配对: %v", err.Err)
	}
}

// TestCompileSyntaxErrors 验证语法与类型错误归到 ReasonSyntax。
func TestCompileSyntaxErrors(t *testing.T) {
	cases := map[string]string{
		"语法错":         `{"prompt": }`,
		"未知变量":        `{"prompt": zzz}`,
		"未知函数":        `{"prompt": foo(1)}`,
		"类型不匹配":       `{"prompt": len <= "abc" ? 1.0 : 2.0}`,
		"括号未闭合":       `{"prompt": (1.0}`,
		"结果不是单价表":     `1.5`,
		"空规则":         ``,
		"tier 参数个数错":  `{"prompt": tier("a")}`,
		"tier 档位名类型错": `{"prompt": tier(1, 1.0)}`,
	}
	for name, source := range cases {
		err := compileErr(t, source)
		if err.Reason == "" {
			t.Errorf("%s: 没有给出失败原因", name)
		}
		if err.Source != source {
			t.Errorf("%s: CompileError.Source 应当是原始源码", name)
		}
	}
}

// TestCompileShapeErrors 验证结果形状的检查。
//
// 检查的是 AST 而不是求值结果，所以这里列的都是「编译期就能看出来」的形态。
// tier 的参数个数与类型由 expr 的类型检查给出，因此归到 ReasonSyntax——
// 那同样是规则作者能自己修的。
func TestCompileShapeErrors(t *testing.T) {
	cases := map[string]Reason{
		// 键名要参与「所有分支的键取并集」与账单展示。
		"未知的键":   ReasonShape,
		"键不是字符串": ReasonShape,
		// 谓词类内置返回数组或对象，不是数值。
		"谓词内置返回非数值": ReasonShape,
		"谓词内置累加":    ReasonShape,
		"谓词内置布尔":    ReasonShape,
		"谓词内置分组":    ReasonShape,
	}
	sources := map[string]string{
		"未知的键":      `{"promt": 1.0}`,
		"键不是字符串":    `{1: 1.0}`,
		"谓词内置返回非数值": `{"prompt": map([1,2], #)}`,
		"谓词内置累加":    `{"prompt": sum([1,2])}`,
		"谓词内置布尔":    `{"prompt": all([1,2], # > 0)}`,
		"谓词内置分组":    `{"prompt": groupBy([1,2], #)}`,
	}
	for name, want := range cases {
		err := compileErr(t, sources[name])
		if err.Reason != want {
			t.Errorf("%s: 原因 = %s，期望 %s（%v）", name, err.Reason, want, err.Err)
		}
	}
}

// TestCompileShapeErrorsByPosition 逐个位置走过形状检查的报错路径。
//
// 上面那条用例问的是「这条规则报的是不是形状错」，这一条问的是「这几个位置上
// 的错到底有没有人来报」。两者的区别在变异测试里看得见：形状检查里那几处
// `if err := walk…; err != nil { return err }`，把 `!=` 改成 `==` 之后，
// 下面五个位置一条都杀不掉——每个位置上「写错了的规则」都没有用例走过。
// 作为对照，内置函数的参数那条是被走过的（`float(p)` 那条用例），所以它杀得掉；
// 缺的正是**用户级函数**（`fixed` / `minimum` / `hour` 这些）与计量项表那几处。
//
// 漏掉一条的后果不是「少报一次错」：变异之后那个守卫要么把错吞掉、要么提前
// `return nil` 跳过后面的检查，于是写错的规则编得过，问题被推到求值那一步，
// 而那里的报错不指向写法。
//
// 每条都点明它落在哪个位置——位置才是它们的区别，报出来的错并不一样。
func TestCompileShapeErrorsByPosition(t *testing.T) {
	cases := []struct {
		position string
		source   string
		globals  map[string]any // nil 表示用 Compile，非 nil 用 CompileWithGlobals
	}{
		// 用户级函数的参数。位置与内置函数的参数是同一处（`函数参数`），
		// 但走的是 CallNode 那一段——已有用例只喂过内置函数。
		{"用户级函数的参数", `{"prompt": fixed(p)}`, nil},
		// 规则根位置的裸标识符：`plan` 是注入常量，值合法，但落在根位置
		// 意味着结果不是一张单价表。
		{"规则根位置的裸标识符", `len <= 1 ? {"prompt": 1.0} : plan`, map[string]any{"plan": 1.0}},
		// 计量项表里的值：单价不该依赖用了多少。
		{"units 表里的裸用量变量", `{"prompt": 1.0, "units": {"img": p}}`, nil},
		// 按条件选出的 units：两支都得是计量项表，一支是数就是量纲错。
		{"units 条件式的真支", `{"prompt": 1.0, "units": len > 0 ? 1.5 : {"img": 0.01}}`, nil},
		{"units 条件式的假支", `{"prompt": 1.0, "units": len > 0 ? {"img": 0.01} : 1.5}`, nil},
	}
	for _, tc := range cases {
		var err error
		if tc.globals == nil {
			_, err = Compile(tc.source)
		} else {
			_, err = CompileWithGlobals(tc.source, tc.globals)
		}
		var ce *CompileError
		if !errors.As(err, &ce) {
			t.Errorf("%s：%q 应当被形状检查拦下，得到 %v", tc.position, tc.source, err)
			continue
		}
		if ce.Reason != ReasonShape {
			t.Errorf("%s：%q 的原因 = %s，期望 %s（%v）",
				tc.position, tc.source, ce.Reason, ReasonShape, ce.Err)
		}
	}
}

// TestCompileIsPeakWindowMustBeALiteral 验证高峰时段必须写成字面量，
// 而且写法当场就解析一遍。
//
// 规则入库时会走 Compile（见 internal/pricing 的 admin），所以这两条在
// **保存规则**时就会报出来。放到求值时才发现的话，一条写错窗口的规则会先在
// 库里躺一段时间，然后让每一次用到它的计价失败。
func TestCompileIsPeakWindowMustBeALiteral(t *testing.T) {
	// 字面量，以及会被折成字面量的常量拼接。
	for _, source := range []string{
		`{"prompt": isPeak("08:00-22:00") ? 1.0 : 2.0}`,
		`{"prompt": isPeak("08:00-12:00,22:00-02:00") ? 1.0 : 2.0}`,
		`{"prompt": isPeak("08:00" + "-22:00") ? 1.0 : 2.0}`,
	} {
		if _, err := Compile(source); err != nil {
			t.Errorf("%q 应当编得过: %v", source, err)
		}
	}

	// 运行期才知道的窗口：与 tier 的档位名同一条理由，规则保存时没法给人看。
	if _, err := Compile(`{"prompt": isPeak(region) ? 1.0 : 2.0}`); err == nil {
		t.Error("运行期才知道的高峰时段应当被拒绝")
	}

	// 写法错的窗口。每一条都点出一种写法：缺小时、不是钟点、分钟不是两位、
	// 只有一个时刻、参数个数不对。
	for _, source := range []string{
		`{"prompt": isPeak("8-22") ? 1.0 : 2.0}`,
		`{"prompt": isPeak("25:00-26:00") ? 1.0 : 2.0}`,
		`{"prompt": isPeak("08:60-09:00") ? 1.0 : 2.0}`,
		`{"prompt": isPeak("8:5-9:00") ? 1.0 : 2.0}`,
		`{"prompt": isPeak("08:00") ? 1.0 : 2.0}`,
		`{"prompt": isPeak("08:00-22:00,") ? 1.0 : 2.0}`,
		`{"prompt": isPeak() ? 1.0 : 2.0}`,
		`{"prompt": isPeak("08:00-22:00", "23:00-23:30") ? 1.0 : 2.0}`,
	} {
		if _, err := Compile(source); err == nil {
			t.Errorf("%q 应当编不过", source)
		}
	}
}

// TestCompileRejectsFullDayWindowAlongsideOthers 验证全天段不许与其他段并列，
// 而且**在编译期**就被拒。
//
// 这条检查原先是留给日历在求值时判的，后来上移到这里：预留那一步走的是
// `EvalWorstCase`，它把三元条件整个换成 `__worst(真, 假)`（见 worst.go 的
// worstPatcher），`isPeak` 在预留时根本不被求值。留在求值期的后果是这条规则
// 存得进库、预留成功、结算时才炸；而流式路上那一炸只留一行日志、随后释放
// 预扣，客户端正常收完不付钱。
//
// 单独一段的 `"00:00-00:00"`（跨满一整天）仍然编得过：那不是写错，日历那边
// 本来就把两端相等解释成整天。
func TestCompileRejectsFullDayWindowAlongsideOthers(t *testing.T) {
	err := compileErr(t, `{"prompt": isPeak("00:00-00:00,08:00-12:00") ? 1.0 : 2.0}`)
	if err.Reason != ReasonShape {
		t.Errorf("原因 = %s，期望 %s（%v）", err.Reason, ReasonShape, err.Err)
	}
	// 报错要说清是哪一段、为什么不行：写规则的人照着这一句改得动。
	if !strings.Contains(err.Error(), "全天") {
		t.Errorf("错误信息应当说清那是全天时段: %v", err)
	}
	if !errors.Is(err, ErrCalendarRequired) {
		t.Errorf("期望 ErrCalendarRequired，得到 %v", err)
	}

	if _, err := Compile(`{"prompt": isPeak("00:00-00:00") ? 1.0 : 2.0}`); err != nil {
		t.Errorf("单独一段全天时段应当编得过: %v", err)
	}
}

// TestCompilePeakWindowErrorNamesTheSegment 验证报错说得出是哪一段不行。
//
// 一条窗口里可以并列好几段，只有一段不合法时，「第几段」是照着这句去改的
// 唯一线索。编号从 1 数起——它给人看，不是数组下标。
func TestCompilePeakWindowErrorNamesTheSegment(t *testing.T) {
	cases := []struct {
		windows string
		want    string
	}{
		// 不行的那一段落在哪一段，报出来的号就该跟着变。两头各放一条，
		// 免得「总是报同一个号」也蒙混过去。
		{"00:00-00:00,08:00-12:00", "第 1 段"},
		{"08:00-12:00,09:00-09:00", "第 2 段"},
	}
	for _, c := range cases {
		err := compileErr(t, fmt.Sprintf(`{"prompt": isPeak(%q) ? 1.0 : 2.0}`, c.windows))
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("窗口 %q 的报错应当指明 %s，得到: %v", c.windows, c.want, err)
		}
	}
}

// TestCompileNestedTier 验证 tier 可以嵌在别的表达式里。
func TestCompileNestedTier(t *testing.T) {
	cases := []string{
		`{"prompt": tier("peak", 1.5)}`,
		`tier("peak", {"prompt": 1.5})`,
		`len <= 100 ? tier("lo", {"prompt": 1.5}) : tier("hi", {"prompt": 3.0})`,
		`{"prompt": (len <= 100 ? tier("a", 1.0) : tier("b", 2.0)) + tier("c", 0.5)}`,
	}
	for _, source := range cases {
		if _, err := Compile(source); err != nil {
			t.Errorf("%q 编不过: %v", source, err)
		}
	}

	// 常量拼接会被 expr 折成字面量，因此照样能取到档位名。
	if _, err := Compile(`{"prompt": tier("a" + "b", 1.0)}`); err != nil {
		t.Errorf("常量拼接的档位名应当被接受: %v", err)
	}

	// 而运行期才知道的档位名必须被拒绝：账单要靠这个名字回答
	// 「当时命中了哪一档」，一个变量拼出来的名字无法回答那句话。
	if _, err := Compile(`{"prompt": tier(region, 1.0)}`); err == nil {
		t.Error("运行期才知道的档位名应当被拒绝")
	}
}

// TestCompileErrorUnwrap 验证 CompileError 能被 errors.Is/As 拆开。
func TestCompileErrorUnwrap(t *testing.T) {
	err := compileErr(t, `{"prompt": zzz}`)
	if !errors.As(err, new(*CompileError)) {
		t.Fatal("errors.As 应当能取出 *CompileError")
	}
	if err.Unwrap() == nil {
		t.Error("Unwrap 应当给出底层原因（expr 的错误带行号）")
	}
	if !strings.Contains(err.Error(), string(ReasonSyntax)) {
		t.Errorf("Error() 应当带上大类: %s", err.Error())
	}
}

// --- 编译缓存 ---

// TestCacheReuse 验证同一把键只编译一次。
func TestCacheReuse(t *testing.T) {
	c := NewCache(8)
	source := `{"prompt": 0.0000015}`

	p1, err := c.Get("rule-1@v1", source)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := c.Get("rule-1@v1", source)
	if err != nil {
		t.Fatal(err)
	}
	if p1 != p2 {
		t.Error("同一个键应当拿到同一份编译产物")
	}
	if c.Len() != 1 {
		t.Errorf("缓存条目数 = %d，期望 1", c.Len())
	}

	// 换一个版本（商家改了规则）应当得到新的产物，
	// 而旧版本仍然在缓存里——历史订单要用旧规则复算。
	if _, err := c.Get("rule-1@v2", `{"prompt": 0.0000030}`); err != nil {
		t.Fatal(err)
	}
	if c.Len() != 2 {
		t.Errorf("缓存条目数 = %d，期望 2", c.Len())
	}
}

// TestCacheIsBounded 验证缓存不会无限增长。
//
// 这是这个类型存在的理由：键来自规则，也就是商家可写的数据。
// 只往里放不清理的话，商家改一万次规则就多一万条缓存项。
func TestCacheIsBounded(t *testing.T) {
	const max = 32
	c := NewCache(max)

	for i := 0; i < max*20; i++ {
		key := "rule-" + itoa(i) + "@v1"
		if _, err := c.Get(key, `{"prompt": `+itoa(i)+`.0}`); err != nil {
			t.Fatal(err)
		}
		if c.Len() > max {
			t.Fatalf("第 %d 次插入后缓存有 %d 条，超过上限 %d", i, c.Len(), max)
		}
	}
	// 上限是硬保证：连放 max*20 条不同的规则也不会超过容量。
	if c.Len() > max {
		t.Errorf("缓存有 %d 条，超过上限 %d", c.Len(), max)
	}
}

// TestCacheKeepsHotEntries 验证淘汰方向是对的：常用的规则留在缓存里。
//
// 判据是**同不同一份产物**，不是「拿不拿得到产物」：未命中会重新编译，而
// compile 每次都新造一个 Program，所以「非 nil」这个断言任何情况下都是绿的
// ——淘汰方向写反、热点被丢掉，它都看不出来。只有同键命中的**同一个指针**
// 才说明那一份还在缓存里。
//
// 变异：命中缓存那条路上会调 `moveToFront`，它的 `if e == nil || l.head == e`
// 改成 `&&` 是等价变异，而两支的理由不一样，分开判：
//
//   - `e == nil` 那一支**没有可到达的输入**：全仓只有 Get 与 GetWithGlobals
//     命中缓存的那几处调它（传进去的都是刚从 `c.items[…]` 里取出来的元素），
//     不会是 nil。
//   - `l.head == e` 那一支（命中的正好是队首）把队首摘下来、再推回队首，
//     得到的还是同一条链表：元素、顺序、head 与 tail 全都不变——只有一个
//     元素那一格也一样（`remove` 之后 head/tail 都空，再推回去又都指向它）。
//
// 所以这一条与 `e == nil` 那一半都不能靠「它不一样」来杀。
func TestCacheKeepsHotEntries(t *testing.T) {
	c := NewCache(16)
	const src = `{"prompt": 0.0000015}`

	// 先放一条「热点」规则，留下它那份产物——后面拿指针跟它比。
	hot := "hot@v1"
	hotProg, err := c.Get(hot, src)
	if err != nil {
		t.Fatal(err)
	}

	// 再灌入大量一次性的规则，同时反复命中热点。
	for i := 0; i < 500; i++ {
		if _, err := c.Get("cold-"+itoa(i)+"@v1", `{"prompt": 1.0}`); err != nil {
			t.Fatal(err)
		}
		// 每次插入前后都碰一下热点，把它保持在队首附近。
		if _, err := c.Get(hot, src); err != nil {
			t.Fatal(err)
		}
	}

	// 热点应当还是**原来那一份**：指针变了说明中间被淘汰过一次，
	// 这一份是重新编出来的，而「重新编得出来一份」在缓存完全没生效时也成立。
	if again := cacheProg(t, c, hot, src); again != hotProg {
		t.Error("热点规则被淘汰过：同键拿到的是重新编译的一份产物（指针不同）")
	}
}

// TestCacheDropsTheOldestBatch 把淘汰的另外两件事也用具体的值固定下来：
// **淘汰哪几条**与**一次淘汰几条**。
//
// 上面那条说得清的只有「热点还在」，而热点每次命中都被挪到队首，于是无论
// 一次淘汰 1 条还是把整个缓存清空，它都留得下来。一次淘汰几条因此只能看
// Len() 的具体值：本用例取 max = 16，也就是 drop = 16/8 = 2，满员后
// 插一条新的应当剩 16 - 2 + 1 = 15 条（淘汰比例是实现的约定，见 evictLocked
// 的注释，改它就要连这条一起改）。
//
// 「淘汰哪几条」要一次**使用顺序与插入顺序不一致**的命中才分得出来：塞满之后
// 先碰一下最旧的那条，它就该到队首去，于是被丢掉的必须是紧随其后的两条。
func TestCacheDropsTheOldestBatch(t *testing.T) {
	const max = 16
	const src = `{"prompt": 0.0000015}`
	c := NewCache(max)

	// 依次塞满。pushFront 之下 k0 是队尾（最旧），k15 是队首（最新）。
	progs := make([]*Program, max)
	for i := range progs {
		progs[i] = cacheProg(t, c, "cold-"+itoa(i)+"@v1", src)
	}

	// 碰一下最旧的那条：它从此在队首，这一轮不该被淘汰。
	cacheProg(t, c, "cold-0@v1", src)

	// 满员时插一条新的：淘汰一批，再放进这一条。
	cacheProg(t, c, "cold-new@v1", src)

	// 一次淘汰几条：多一条少一条都在这一行上现形。
	if got, want := c.Len(), max-max/8+1; got != want {
		t.Errorf("满员后插入一条之后有 %d 条，期望 %d（一次淘汰 max/8 = %d 条）",
			got, want, max/8)
	}

	// 刚碰过的 k0 必须在，指针不变才是「还在缓存里」。
	if p := cacheProg(t, c, "cold-0@v1", src); p != progs[0] {
		t.Error("刚碰过的 k0 被淘汰了：淘汰没有按使用顺序从队尾开始")
	}
	// 排在它后面的 k3 也在。k3 被丢掉只可能是淘汰的条数多于 max/8。
	if p := cacheProg(t, c, "cold-3@v1", src); p != progs[3] {
		t.Error("k3 被淘汰了：一次淘汰的条数多于 max/8")
	}
	// 而队尾那两条（k1、k2）正是该被丢掉的，它们现在得重新编译。
	for _, i := range []int{1, 2} {
		key := "cold-" + itoa(i) + "@v1"
		if p := cacheProg(t, c, key, src); p == progs[i] {
			t.Errorf("k%d 还在缓存里：该被淘汰的是队尾那两条", i)
		}
	}
}

// TestCacheBoundsTinyCapacity 把小容量下那两处「不足就取 1」的钳位用具体的值固定下来。
//
// 容量 0 与容量 1 都是容量 1 的缓存；淘汰时算出来的 `max/8` 也要从 0 钳到 1
// ——两处都钳住，缓存才有上限：不钳的话容量 0 的缓存一条都不淘汰，
// 商家改一万次规则就攒一万条。
//
// 两处的**边界那一格都是等价变异**，抓不到，而理由各自成立：
//
//   - `max < 1` 改成 `max <= 1`：max 正好是 1 时进分支把 max 赋成 1，
//     写的还是同一个数（容量提示 `make(map, 1)` 也一模一样）。
//   - `drop < 1` 改成 `drop <= 1`：max 是 8 时 `8/8` 正好是 1，进分支赋 1，
//     同一个数。
//
// 要说「加上哪一组输入会让两版不同」——没有：两处赋值都写在「算出来的正好
// 就是要赋的那个值」那一格上，换分支不改值。下面两条仍然把这一格的具体数
// 固定下来（容量 8 时一次淘汰 1 条正是 max/8 的那一格），改写法时它会红。
func TestCacheBoundsTinyCapacity(t *testing.T) {
	// 容量 0 被钳到 1：放一批不同的规则之后只剩一条，不随插入次数增长。
	c := NewCache(0)
	for i := 0; i < 8; i++ {
		cacheProg(t, c, "k"+itoa(i)+"@v1", `{"prompt": 1.0}`)
		if c.Len() > 1 {
			t.Fatalf("第 %d 次插入后有 %d 条，容量 1 的缓存不该超过 1 条", i, c.Len())
		}
	}
	if c.Len() != 1 {
		t.Errorf("插入 8 条不同规则之后有 %d 条，期望 1", c.Len())
	}

	// 容量 8：一次淘汰 8/8 = 1 条，满员后插一条再放进一条，仍是 8 条。
	c8 := NewCache(8)
	for i := 0; i < 8; i++ {
		cacheProg(t, c8, "k"+itoa(i)+"@v1", `{"prompt": 1.0}`)
	}
	if c8.Len() != 8 {
		t.Fatalf("塞满后应当有 8 条，得到 %d", c8.Len())
	}
	cacheProg(t, c8, "k-new@v1", `{"prompt": 1.0}`)
	if got := c8.Len(); got != 8 {
		t.Errorf("满员后插入一条之后有 %d 条，期望 8（一次淘汰 1 条再放进 1 条）", got)
	}
}

// TestCacheFailureNotCached 验证编译失败的规则不进缓存。
//
// 失败的规则是商家写错的中间态；把失败也缓存等于给每一条写错的规则
// 留一个常驻条目，而失败结果的复用价值为零——它每次都该重新编一次，
// 这样商家改对了立刻就能生效。
func TestCacheFailureNotCached(t *testing.T) {
	c := NewCache(8)
	bad := `{"prompt": zzz}`
	if _, err := c.Get("rule-1@v1", bad); err == nil {
		t.Fatal("坏规则应当编译失败")
	}
	if c.Len() != 0 {
		t.Errorf("失败的编译不该进缓存，当前 %d 条", c.Len())
	}

	// 同一个键改成正确的规则时应当立刻可用。
	if _, err := c.Get("rule-1@v1", `{"prompt": 1.0}`); err != nil {
		t.Fatalf("改正后应当能编过: %v", err)
	}
	if c.Len() != 1 {
		t.Errorf("条目数 = %d，期望 1", c.Len())
	}
}

// TestCacheGlobalsSignature 验证 globals 的声明进了缓存键。
//
// 同一个 (ruleID, version) 配上不同的 globals 声明是不同的编译产物：
// 一个字符串常量与一个整数常量会走不同的类型检查。
// 若键里不体现这一点，先编的那份实现会被后一次的调用方复用，
// 而两次调用的 globals 形状并不一样。
func TestCacheGlobalsSignature(t *testing.T) {
	c := NewCache(8)
	source := `{"prompt": plan == "pro" ? 1.0 : 2.0}`

	if _, err := c.GetWithGlobals("rule-1@v1", source, map[string]any{"plan": ""}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetWithGlobals("rule-1@v1", source, map[string]any{"plan": "other"}); err != nil {
		t.Fatal(err)
	}
	// 值不同、类型相同时应当复用同一份产物。
	if c.Len() != 1 {
		t.Errorf("同类型不同值应当复用一份产物，条目数 = %d", c.Len())
	}

	// 类型不同（int 与 string）必须分成两条。
	if _, err := c.GetWithGlobals("rule-1@v1", `{"prompt": plan <= 3 ? 1.0 : 2.0}`,
		map[string]any{"plan": int64(0)}); err != nil {
		t.Fatal(err)
	}
	if c.Len() != 2 {
		t.Errorf("不同类型应当分成两条，条目数 = %d", c.Len())
	}
}

// TestCacheConcurrent 用 -race 验证缓存本身并发安全。
func TestCacheConcurrent(t *testing.T) {
	c := NewCache(16)
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				key := "rule-" + itoa(i%32) + "@v1"
				if _, err := c.Get(key, `{"prompt": 0.0000015}`); err != nil {
					t.Error(err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	if c.Len() > 16 {
		t.Errorf("并发之后缓存有 %d 条，超过上限 16", c.Len())
	}
}

// --- 辅助 ---

// cacheProg 从缓存里取一份编译产物，取不到就当场断掉。
//
// 缓存那几条用例要比的是**指针**（同一份产物 = 命中），所以这里把 err 就地
// 处理掉，只把 *Program 交回去；每次调用都是真实的一次 Get，会照常把命中的
// 条目挪到队首，用例的注释里要按这一点读顺序。
func cacheProg(t *testing.T, c *Cache, key, source string) *Program {
	t.Helper()
	p, err := c.Get(key, source)
	if err != nil {
		t.Fatalf("取 %q 的编译产物失败: %v", key, err)
	}
	return p
}

func compileErr(t *testing.T, source string) *CompileError {
	t.Helper()
	_, err := Compile(source)
	if err == nil {
		t.Fatalf("%q 竟然编过了", source)
	}
	var ce *CompileError
	if !errors.As(err, &ce) {
		t.Fatalf("%q 的错误不是 *CompileError: %T", source, err)
	}
	return ce
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
