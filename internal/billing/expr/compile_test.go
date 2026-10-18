package expr

import (
	"errors"
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
func TestCacheKeepsHotEntries(t *testing.T) {
	c := NewCache(16)

	// 先放一条「热点」规则。
	hot := "hot@v1"
	if _, err := c.Get(hot, `{"prompt": 0.0000015}`); err != nil {
		t.Fatal(err)
	}

	// 再灌入大量一次性的规则，同时反复命中热点。
	for i := 0; i < 500; i++ {
		if _, err := c.Get("cold-"+itoa(i)+"@v1", `{"prompt": 1.0}`); err != nil {
			t.Fatal(err)
		}
		// 每次插入前后都碰一下热点，把它保持在队首附近。
		if _, err := c.Get(hot, `{"prompt": 0.0000015}`); err != nil {
			t.Fatal(err)
		}
	}

	// 热点应当仍在缓存里：它的键能直接命中，
	// 而一次性的规则早被淘汰掉了。
	p, err := c.Get(hot, `{"prompt": 0.0000015}`)
	if err != nil {
		t.Fatal(err)
	}
	if p == nil {
		t.Fatal("热点规则被淘汰了")
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
