package money

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"testing"
)

func TestParseAndString(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"0", "0.00000000"},
		{"1", "1.00000000"},
		{"1.5", "1.50000000"},
		{"-1.5", "-1.50000000"},
		{"+1.5", "1.50000000"},
		{"0.00000001", "0.00000001"},
		{"12.34567890", "12.34567890"},
		{" 1.5 ", "1.50000000"},
		{".5", "0.50000000"},
		{"1.", "1.00000000"},
		{"123456789.12345678", "123456789.12345678"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := Parse(tc.in)
			if err != nil {
				t.Fatalf("Parse(%q) 出错: %v", tc.in, err)
			}
			if got.String() != tc.want {
				t.Errorf("Parse(%q).String() = %q，期望 %q", tc.in, got.String(), tc.want)
			}
		})
	}
}

// 超过 Scale 位的小数要四舍五入，不是截断。
// 截断会让一批小额扣费系统性地偏向用户，累积起来是真实的差额。
func TestParseRoundsNotTruncates(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"0.000000004", "0.00000000"}, // 第 9 位是 4，舍
		{"0.000000005", "0.00000001"}, // 第 9 位是 5，入
		{"0.000000006", "0.00000001"},
		{"1.999999995", "2.00000000"}, // 进位传播到整数部分
		{"0.999999999", "1.00000000"},
		{"-0.000000005", "-0.00000001"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := Parse(tc.in)
			if err != nil {
				t.Fatalf("出错: %v", err)
			}
			if got.String() != tc.want {
				t.Errorf("Parse(%q) = %q，期望 %q", tc.in, got.String(), tc.want)
			}
		})
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "  ", "abc", "1.2.3", "1e5", "--1", "+", "-", "1,5", "0x10"} {
		if _, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) 应报错", in)
		}
	}
}

// 幅值超出能表示的范围时报 ErrOverflow，断言的是**那个错误值**
// （errors.Is），不是「有没有出错」：把 ErrOverflow 换成别的错，下面每一条
// 都要跟着红。
//
// 两端各留一条紧挨着的界外，判据才真的落在边界上：正侧上界是 MaxInt64 个
// 最小单位（String 印 92233720368.54775807），负侧上界是 2^63
// （印 -92233720368.54775808）。负侧比正侧多那一格，就是这里要守住的东西。
func TestParseRejectsOverflow(t *testing.T) {
	// 远超 int64/10^8 的整数元
	if _, err := Parse("999999999999999999999"); !errors.Is(err, ErrOverflow) {
		t.Errorf("应报 ErrOverflow，得到 %v", err)
	}
	cases := []struct {
		name string
		in   string
	}{
		{"正侧上界的下一格", "92233720368.54775808"},
		{"负侧上界的下一格", "-92233720368.54775809"},
		{"四舍五入之后越过正侧上界", "92233720368.547758075"},
		// 整数部分是 MaxInt64 元、小数又进位：幅值远在范围之外，而拦下它的是
		// **乘积之前**那条前置判据——没有它，`whole*unit` 会先在 uint64 里回绕，
		// 后面那道按幅值的比较就用在一个已经被改坏的值上了。
		{"整数部分顶在上界、小数又进位", "9223372036854775807.999999999"},
		// 下面两格钉的是**进位之后**那道 `magnitude > limit`：整数部分自己还在
		// 范围内，前面那条按幅值的前置判据放它过去，把它拦下来的是进位把幅值推过
		// 上界之后的那一次比较。少了那一次比较，正侧这一格会回绕成负值、负侧这一格
		// 会回绕成正的，两版都静默返回。
		{"进位让整数部分越过正侧上界", "92233720368.999999999"},
		{"同上，负侧", "-92233720368.999999999"},
		// 上面那格的孪生：整数部分贴下界时，拦下它的是**乘积之前**那条前置判据——
		// 没有它，`whole*unit` 会先在 uint64 里回绕成 0，进位再补一个 unit 又正好
		// 回到 0，同样静默返回零。
		{"负侧的孪生输入：整数部分贴下界、小数还要进位", "-9223372036854775807.999999999"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.in)
			if !errors.Is(err, ErrOverflow) {
				t.Fatalf("Parse(%q) = %s，err = %v，期望 ErrOverflow", tc.in, got.String(), err)
			}
			if !got.IsZero() {
				t.Fatalf("报溢出时返回的应是零，得到 %s", got.String())
			}
		})
	}
}

// 四舍五入可以正好落到负侧上界那一格，而那一格是可表示的。
//
// 这一条与上面「越过正侧上界」的那条是一对：同一句取整，正侧越界、负侧刚好
// 抵达，差的就是负侧多出来的那一格。
func TestParseRoundingReachesMinInt64(t *testing.T) {
	got, err := Parse("-92233720368.547758075")
	if err != nil {
		t.Fatalf("Parse 出错: %v", err)
	}
	if got.Units() != math.MinInt64 {
		t.Fatalf("Units() = %d，期望 %d", got.Units(), int64(math.MinInt64))
	}
	if want := "-92233720368.54775808"; got.String() != want {
		t.Fatalf("String() = %q，期望 %q", got.String(), want)
	}
}

func TestMustParse(t *testing.T) {
	if MustParse("1.5").String() != "1.50000000" {
		t.Error("MustParse 结果不对")
	}
	defer func() {
		if recover() == nil {
			t.Error("非法输入应 panic")
		}
	}()
	MustParse("abc")
}

// ── 运算 ────────────────────────────────────────────────────────────────

// 0.1 + 0.2 == 0.3。用浮点会得到 0.30000000000000004，而账目上差一分就是错账。
func TestAdditionIsExact(t *testing.T) {
	sum := MustParse("0.1").Add(MustParse("0.2"))
	if sum.String() != "0.30000000" {
		t.Errorf("0.1 + 0.2 = %s", sum.String())
	}
}

func TestAddSubNeg(t *testing.T) {
	a, b := MustParse("10.5"), MustParse("3.25")
	if got := a.Sub(b); got.String() != "7.25000000" {
		t.Errorf("Sub = %s", got)
	}
	if got := a.Add(b.Neg()); got.Cmp(a.Sub(b)) != 0 {
		t.Error("加负数应等于减")
	}
	if got := a.Neg().Neg(); got.Cmp(a) != 0 {
		t.Error("两次取负应回到原值")
	}
	// 上面几条比的是「两者相等」，两版判据错成同一种错时照样绿；这里把和本身
	// 钉成字面量——两个负数相加、异号相加、加零是账上最常见的三种形态。
	if got := MustParse("-5").Add(MustParse("-3")); got.String() != "-8.00000000" {
		t.Errorf("-5 + -3 = %s", got.String())
	}
	if got := MustParse("10").Add(MustParse("-5")); got.String() != "5.00000000" {
		t.Errorf("10 + -5 = %s", got.String())
	}
	if got := MustParse("-5").Add(Zero); got.String() != "-5.00000000" {
		t.Errorf("-5 + 0 = %s", got.String())
	}
}

func TestSum(t *testing.T) {
	total := Sum(MustParse("0.1"), MustParse("0.2"), MustParse("0.3"))
	if total.String() != "0.60000000" {
		t.Errorf("Sum = %s", total.String())
	}
	if Sum().IsZero() != true {
		t.Error("空求和应为零")
	}
}

// 单价 × 数量是最常见的运算，它必须精确。
//
// 等价变异（gremlins INVERT_LOGICAL，`if n == 0 || a.units == 0` 的 `||` 改成
// `&&`）：早退只是省一次乘法，落到下面算出来的乘积同样是 0（零乘任何数为零），
// 返回的值与早退那条逐位相同。
func TestMulInt(t *testing.T) {
	// 每千 token 0.0001 元，用 3500 个 token
	unitPrice := MustParse("0.0001")
	got := unitPrice.MulInt(35) // 3500 / 1000 = 35
	if got.String() != "0.00350000" {
		t.Errorf("MulInt = %s", got.String())
	}
	if unitPrice.MulInt(0).IsZero() != true {
		t.Error("乘零应为零")
	}
}

// 乘法也要四舍五入，且负数的方向要对。
func TestMulIntRounding(t *testing.T) {
	cases := []struct {
		amount string
		n      int64
		want   string
	}{
		{"1.00000001", 1, "1.00000001"},
		{"0.00000001", 0, "0.00000000"},
		{"-1.00000001", 1, "-1.00000001"},
		{"1", 3, "3.00000000"},
		{"1", -2, "-2.00000000"},
	}
	for _, tc := range cases {
		got := MustParse(tc.amount).MulInt(tc.n)
		if got.String() != tc.want {
			t.Errorf("%s * %d = %s，期望 %s", tc.amount, tc.n, got.String(), tc.want)
		}
	}
}

func TestMulAmount(t *testing.T) {
	// 每单位 2.5 元，共 4 个单位
	got := MulAmount(MustParse("2.5"), MustParse("4"))
	if got.String() != "10.00000000" {
		t.Errorf("MulAmount = %s", got.String())
	}
}

// 折扣系数 0.85 在浮点里不精确，乘上金额之后可能差一分。
// 倍率会用于每一次计费，一分钱的偏差乘上百万次就是一笔真实的差额。
func TestMulFloatHandlesImpreciseFactors(t *testing.T) {
	// 0.1 * 0.2 在浮点里是 0.020000000000000004
	got := MulFloat(MustParse("0.1"), 0.2)
	if got.String() != "0.02000000" {
		t.Errorf("MulFloat = %s，期望 0.02000000", got.String())
	}

	// 常见的折扣
	got = MulFloat(MustParse("100"), 0.85)
	if got.String() != "85.00000000" {
		t.Errorf("100 * 0.85 = %s", got.String())
	}

	// 0.07 在浮点里是 0.07000000000000001
	got = MulFloat(MustParse("1"), 0.07)
	if got.String() != "0.07000000" {
		t.Errorf("1 * 0.07 = %s", got.String())
	}
}

// 系数非法时返回零而不是 panic：它来自配置，配置写错不该让进程挂掉。
//
// 等价变异（gremlins INVERT_LOGICAL，`math.IsNaN(factor) || math.IsInf(factor, 0)`
// 的 `||` 改成 `&&`）：NaN 与 ±Inf 互斥，那个条件于是永远不成立，而落到下面
// `SetFloat64` 对非有限值返回 nil，`r == nil` 那一支照样给出 Zero——两版同值。
func TestMulFloatRejectsInvalidFactors(t *testing.T) {
	for _, f := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if got := MulFloat(One, f); !got.IsZero() {
			t.Errorf("系数 %v 应返回零，得到 %s", f, got.String())
		}
	}
}

// 零除返回零而不是 panic：它来自「数量为 0」这类平凡情况。
func TestDivIntByZero(t *testing.T) {
	if got := One.DivInt(0); !got.IsZero() {
		t.Errorf("除零应返回零，得到 %s", got.String())
	}
}

func TestDivInt(t *testing.T) {
	got := MustParse("10").DivInt(4)
	if got.String() != "2.50000000" {
		t.Errorf("DivInt = %s", got.String())
	}
}

// ── 溢出与边界 ──────────────────────────────────────────────────────────

// 溢出时返回边界值而不是回绕。回绕会把一个天文数字变成看起来正常的
// 负数金额，那比一个明显离谱的值更危险。
//
// 两侧都断言**具体的值**：负半原先只判「不是正数」，而 `MinInt64 + MinInt64`
// 回绕出来的恰好是 0——0 既不是正数也不是负数，于是「不是正数」放它过去，
// 这一格就这样漏了整整一轮（见 TestAddOfTwoMinInt64ClampsToMinInt64）。
func TestOverflowClampsNotWraps(t *testing.T) {
	huge := Amount{units: math.MaxInt64}
	if got := huge.Add(huge); got.Units() != math.MaxInt64 {
		t.Errorf("正向溢出应夹到最大值，得到 %d（%s）", got.Units(), got.String())
	}

	small := Amount{units: math.MinInt64}
	if got := small.Add(small); got.Units() != math.MinInt64 {
		t.Errorf("负向溢出应夹到最小值，得到 %d（%s）", got.Units(), got.String())
	}
}

// 两个下界相加必须夹住，而不是回绕成 0。
//
// 这一格是「只看结果的符号」那一版判据漏掉的：`MinInt64 + MinInt64` 回绕出来的
// 和恰好是 0，既不大于 0 也不小于 0，于是「同号相加结果必然同号，否则溢出」
// 放它过去——一笔巨额欠款被算成零。断言的是具体的值：只断言「不是正数」的话，
// 0 与 MinInt64 都能通过，这一格就是那样漏过去的。
func TestAddOfTwoMinInt64ClampsToMinInt64(t *testing.T) {
	min := Amount{units: math.MinInt64}
	if got := min.Add(min); got.Units() != math.MinInt64 {
		t.Fatalf("MinInt64 + MinInt64 = %d（%s），期望 %s（夹到下界）",
			got.Units(), got.String(), min)
	}

	// 交换操作数：判据按 b 的符号分支，这一格走的是另一支（b 为负、和变大）。
	if got := FromUnits(-1).Add(min); got.Units() != math.MinInt64 {
		t.Fatalf("-1 加 MinInt64 = %d（%s），期望 %s（夹到下界）",
			got.Units(), got.String(), min)
	}
}

// MinInt64 也要拼出合法的十进制串。
//
// `units = -units` 在 MinInt64 上会回绕成自己（-MinInt64 == MinInt64），
// 于是整数与小数部分各带一个负号，拼出 "--92233720368.-54775808"，
// 送进 SQL 是 22P02: invalid input syntax for type numeric。
func TestStringOfMinInt64(t *testing.T) {
	got := Amount{units: math.MinInt64}.String()
	if want := "-92233720368.54775808"; got != want {
		t.Fatalf("String() = %q，期望 %q", got, want)
	}

	// 形状：整个串只有一个负号且在开头，小数恰好 Scale 位。
	if strings.Count(got, "-") != 1 || got[0] != '-' {
		t.Fatalf("String() = %q，负号应只有开头一处", got)
	}
	wholeStr, fracStr, found := strings.Cut(got[1:], ".")
	if !found || len(fracStr) != Scale {
		t.Fatalf("String() = %q，应恰有一个小数点且小数 %d 位", got, Scale)
	}
	whole, err := strconv.ParseUint(wholeStr, 10, 64)
	if err != nil {
		t.Fatalf("整数部分 %q 不是无符号十进制: %v", wholeStr, err)
	}
	frac, err := strconv.ParseUint(fracStr, 10, 64)
	if err != nil {
		t.Fatalf("小数部分 %q 不是无符号十进制: %v", fracStr, err)
	}
	// 拼回去的幅值必须恰好是 2^63，即 |MinInt64|。
	if magnitude := whole*uint64(unit) + frac; magnitude != uint64(1)<<63 {
		t.Fatalf("解析回的最小单位数是 %d，期望 %d", magnitude, uint64(1)<<63)
	}

	// 这一串必须还能被 Parse 读回来：负侧的门槛按幅值到 2^63，
	// 而不是与正侧一起卡在 MaxInt64 上。卡在 MaxInt64 的话，这里会是
	// ErrOverflow——类型自己印出来的串自己读不回，而那个值完全合法。
	back, err := Parse(got)
	if err != nil {
		t.Fatalf("Parse(%q) 出错: %v", got, err)
	}
	if back.Units() != math.MinInt64 {
		t.Fatalf("Parse(%q).Units() = %d，期望 %d", got, back.Units(), int64(math.MinInt64))
	}
	if again := FromUnits(math.MinInt64).String(); again != got {
		t.Fatalf("FromUnits(MinInt64).String() = %q，期望 %q", again, got)
	}
}

// MinInt64 由加法产生，不由字面量产生：SumChecked 是它的真实来路。
func TestSumCheckedToMinInt64String(t *testing.T) {
	got, ok := SumChecked(Amount{units: -math.MaxInt64}, Amount{units: -1})
	if !ok {
		t.Fatal("-MaxInt64 加 -1 放得下 int64，不该报溢出")
	}
	if got.Units() != math.MinInt64 {
		t.Fatalf("和 = %d，期望 %d", got.Units(), int64(math.MinInt64))
	}
	if s := got.String(); s != "-92233720368.54775808" {
		t.Fatalf("String() = %q，期望 -92233720368.54775808", s)
	}
}

// String 与 Parse 在边界上互为逆运算：两端都进表——正侧上界是 MaxInt64，
// 负侧上界是 MinInt64，后一格要到 Parse 的门槛按符号分之后才读得回来。
func TestStringParseRoundTripBoundaries(t *testing.T) {
	cases := []struct {
		name  string
		units int64
		want  string
	}{
		{"零", 0, "0.00000000"},
		{"一", int64(unit), "1.00000000"},
		{"普通正数", 12345678901, "123.45678901"},
		{"普通负数", -12345678901, "-123.45678901"},
		{"MaxInt64", math.MaxInt64, "92233720368.54775807"},
		{"MinInt64", math.MinInt64, "-92233720368.54775808"},
		{"-MaxInt64", -math.MaxInt64, "-92233720368.54775807"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := Amount{units: tc.units}
			s := a.String()
			if s != tc.want {
				t.Fatalf("String() = %q，期望 %q", s, tc.want)
			}
			back, err := Parse(s)
			if err != nil {
				t.Fatalf("Parse(%q) 出错: %v", s, err)
			}
			if back.Units() != tc.units {
				t.Fatalf("Parse(%q).Units() = %d，期望 %d", s, back.Units(), tc.units)
			}
		})
	}
}

func TestNegOfMinInt(t *testing.T) {
	got := Amount{units: math.MinInt64}.Neg()
	if got.Units() != math.MaxInt64 {
		t.Errorf("Neg(MinInt64) = %d，期望 MaxInt64", got.Units())
	}
}

func TestFromInt(t *testing.T) {
	if got := FromInt(5); got.String() != "5.00000000" {
		t.Errorf("FromInt(5) = %s", got.String())
	}
	if got := FromInt(-3); got.String() != "-3.00000000" {
		t.Errorf("FromInt(-3) = %s", got.String())
	}
	// 极大值不该回绕成负数
	if got := FromInt(math.MaxInt64); got.IsNegative() {
		t.Errorf("FromInt(MaxInt64) 回绕成了负数: %s", got.String())
	}
}

func TestFromUnitsRoundTrip(t *testing.T) {
	original := MustParse("123.45678901")
	if got := FromUnits(original.Units()); got.Cmp(original) != 0 {
		t.Errorf("Units 往返丢精度: %s vs %s", got.String(), original.String())
	}
}

// ── 比较与工具 ──────────────────────────────────────────────────────────

func TestCmpAndHelpers(t *testing.T) {
	a, b := MustParse("1"), MustParse("2")
	if a.Cmp(b) != -1 || b.Cmp(a) != 1 || a.Cmp(a) != 0 {
		t.Error("Cmp 结果不对")
	}
	if Min(a, b).Cmp(a) != 0 || Max(a, b).Cmp(b) != 0 {
		t.Error("Min/Max 结果不对")
	}
	if got := MustParse("-5").ClampZero(); !got.IsZero() {
		t.Errorf("ClampZero = %s", got.String())
	}
	if got := MustParse("5").ClampZero(); got.String() != "5.00000000" {
		t.Errorf("ClampZero 不该改动正值: %s", got.String())
	}
}

func TestZeroValueIsUsable(t *testing.T) {
	var a Amount
	if !a.IsZero() || a.String() != "0.00000000" {
		t.Errorf("零值应可直接使用: %s", a.String())
	}
	if got := a.Add(One); got.String() != "1.00000000" {
		t.Errorf("零值加一 = %s", got.String())
	}
}

func TestFloat64IsForDisplayOnly(t *testing.T) {
	if got := MustParse("1.5").Float64(); got != 1.5 {
		t.Errorf("Float64 = %v", got)
	}
}

// String 保留到 Scale 位而不是去掉尾零：账目输出的一致性比短更重要，
// 同一列里混着两种写法会让比对脚本出错。
func TestStringKeepsTrailingZeros(t *testing.T) {
	if got := MustParse("1").String(); got != "1.00000000" {
		t.Errorf("应保留 Scale 位小数，得到 %q", got)
	}
	if strings.Count(MustParse("1").String(), ".") != 1 {
		t.Error("应只有一个小数点")
	}
}

// 大额与小额的边界。单价常在「每千 token 0.0001 元」量级，
// 用 2 位小数会把单价直接抹成 0。
func TestSmallUnitPricesSurvive(t *testing.T) {
	price := MustParse("0.0001")
	if price.IsZero() {
		t.Fatal("0.0001 不该被抹成零")
	}
	// 一千个 token 的费用
	if got := price.MulInt(1); got.String() != "0.00010000" {
		t.Errorf("单价 = %s", got.String())
	}
}

// ── 变异测试补的用例 ────────────────────────────────────────────────────
//
// 下面这一组是用变异测试（gremlins unleash）找出来的：把代码里的某个
// 比较符或分支方向改一下，测试**仍然通过**——那说明那个分支没有被真的
// 钉住。这类缺口用覆盖率看不出来（行被执行过），只有变异测试能发现。
//
// 每一条都对应一个曾经「存活」的变异。

// 恰好一半时向远离零的方向进位。
//
// 变异：把 `2*|rem| >= |den|` 的 `>=` 改成 `>`，恰好一半就不再进位。
// 覆盖不到是因为原有用例里没有「除数恰好是余数的两倍」这种情形。
//
// 等价变异（gremlins CONDITIONALS_BOUNDARY，把 `numerator.Sign() < 0` 的 `<`
// 改成 `<= 0`）：这一段只在「|余数| 的两倍 ≥ |除数|」时进得来，而 |除数| ≥ 1，
// 于是 |余数| ≥ 1、numerator 也就不可能是 0——`Sign() == 0` 那一格到不了。
func TestRoundHalfAwayFromZero(t *testing.T) {
	cases := []struct {
		name string
		got  Amount
		want string
	}{
		// 1 个最小单位除以 2，恰好是一半，应当进位到 1
		{"正数的一半", Amount{units: 1}.DivInt(2), "0.00000001"},
		{"负数的一半", Amount{units: -1}.DivInt(2), "-0.00000001"},
		// 3 个最小单位除以 2 = 1.5，应当进位到 2
		{"正数的一点五", Amount{units: 3}.DivInt(2), "0.00000002"},
		{"负数的一点五", Amount{units: -3}.DivInt(2), "-0.00000002"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got.String() != tc.want {
				t.Errorf("得到 %s，期望 %s", tc.got, tc.want)
			}
		})
	}
}

// 不足一半时**不**进位，且方向对负数同样正确。
//
// 变异：把 `numerator.Sign() < 0` 改掉，负数就会朝错的方向进位。
func TestRoundBelowHalfDoesNotCarry(t *testing.T) {
	cases := []struct {
		name string
		got  Amount
		want string
	}{
		{"正数不足一半", Amount{units: 1}.DivInt(3), "0.00000000"},
		{"负数不足一半", Amount{units: -1}.DivInt(3), "0.00000000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got.String() != tc.want {
				t.Errorf("得到 %s，期望 %s", tc.got, tc.want)
			}
		})
	}
}

// 零既不是正数也不是负数。
//
// 变异：把 `IsNegative` 的 `< 0` 改成 `<= 0`（或 `IsPositive` 的 `> 0`
// 改成 `>= 0`），零就会被同时算成两者。原有用例只测了正负，没测零。
func TestZeroIsNeitherPositiveNorNegative(t *testing.T) {
	var z Amount
	if z.IsNegative() {
		t.Error("零不该是负数")
	}
	if z.IsPositive() {
		t.Error("零不该是正数")
	}
}

// ClampZero 只夹负数，不碰零与正数。
//
// 变异：把 `a.units < 0` 改成 `<= 0`。零的结果不变，所以看不出来；
// 但那个分支的形状变了，而这类改动正是「看着没问题」的那类。
//
// 等价变异（gremlins CONDITIONALS_BOUNDARY，把 `a.units < 0` 的 `<` 改成
// `<= 0`）：units 为 0 时 a 本身就是 Zero，返回 Zero 与返回 a 是同一个值。
func TestClampZeroBoundary(t *testing.T) {
	if got := (Amount{units: -1}).ClampZero(); !got.IsZero() {
		t.Errorf("负数应被夹到零，得到 %s", got)
	}
	if got := (Amount{}).ClampZero(); !got.IsZero() {
		t.Errorf("零应保持不变，得到 %s", got)
	}
	if got := (Amount{units: 1}).ClampZero(); got.Units() != 1 {
		t.Errorf("正数应保持不变，得到 %s", got)
	}
}

// Min/Max 在两边相等时应返回其中之一，且不改变值。
//
// 变异：把 `<=` 改成 `<`、`>=` 改成 `>`。相等时走的分支不同，而结果相同——
// 那两条是等价变异（gremlins CONDITIONALS_BOUNDARY）：Amount 只有一个字段，
// units 相等就是同一个值，返回 a 与返回 b 给出的东西逐位相同，**断言分不开两版**。
// 这条用例留着，是把「相等时也正确」这个行为写下来。
func TestMinMaxWithEqualValues(t *testing.T) {
	a, b := MustParse("1.5"), MustParse("1.5")
	if got := Min(a, b); got.Cmp(a) != 0 {
		t.Errorf("Min 相等时 = %s，期望 %s", got, a)
	}
	if got := Max(a, b); got.Cmp(a) != 0 {
		t.Errorf("Max 相等时 = %s，期望 %s", got, a)
	}
	// 负数与零的边界
	if got := Min(Zero, MustParse("-1")); got.String() != "-1.00000000" {
		t.Errorf("Min(0, -1) = %s", got)
	}
	if got := Max(Zero, MustParse("-1")); !got.IsZero() {
		t.Errorf("Max(0, -1) = %s", got)
	}
}

// 溢出检测的边界：恰好在上界上不该被误判为溢出。
//
// 变异：把 `yuan > MaxInt64/unit` 的 `>` 改成 `>=`，
// 于是恰好能表示的整数元会被夹到最大值（一个错误的数），
// 而原有用例只测了明显超界的值。
func TestFromIntBoundaryIsNotClamped(t *testing.T) {
	// 恰好能表示的最大整数元
	maxYuan := int64(math.MaxInt64 / unit)
	got := FromInt(maxYuan)
	if got.Units() != maxYuan*unit {
		t.Errorf("恰好在上界上的值不该被夹：得到 %d，期望 %d", got.Units(), maxYuan*unit)
	}

	minYuan := int64(math.MinInt64 / unit)
	got = FromInt(minYuan)
	if got.Units() != minYuan*unit {
		t.Errorf("恰好在下界上的值不该被夹：得到 %d，期望 %d", got.Units(), minYuan*unit)
	}
}

// 解析时整数部分的溢出门槛。
//
// 变异：把 `whole > (MaxInt64-fracUnits)/unit` 的 `>` 改掉，
// 边界上的值会被误拒或误收。
func TestParseOverflowBoundary(t *testing.T) {
	// 恰好能表示的最大值：MaxInt64 个最小单位 = 92233720368.54775807
	maxStr := Amount{units: math.MaxInt64}.String()
	got, err := Parse(maxStr)
	if err != nil {
		t.Fatalf("最大值应当解析得出来: %v", err)
	}
	if got.Units() != math.MaxInt64 {
		t.Errorf("得到 %d，期望 %d", got.Units(), int64(math.MaxInt64))
	}

	// 再大一点就应当报溢出
	if _, err := Parse("92233720368.54775808"); err == nil {
		t.Error("超出范围应报错")
	}
}

// parseInt64 的溢出门槛。
//
// 变异：把 `n > (MaxInt64-d)/10` 改掉，边界上的输入会被误收成回绕的值。
func TestParseInt64Boundary(t *testing.T) {
	maxStr := fmt.Sprintf("%d", int64(math.MaxInt64))
	got, err := parseInt64(maxStr)
	if err != nil {
		t.Fatalf("MaxInt64 应解析得出来: %v", err)
	}
	if got != math.MaxInt64 {
		t.Errorf("得到 %d，期望 %d", got, int64(math.MaxInt64))
	}

	// 多一位就溢出
	if _, err := parseInt64(maxStr + "0"); !errors.Is(err, ErrOverflow) {
		t.Errorf("应报 ErrOverflow，得到 %v", err)
	}
}

// 加法的溢出检测：同号相加才可能溢出，异号相加永远不会。
//
// 这一条钉的是异号那一侧：判据按 b 的符号分支，异号时两条都不成立，和原样返回
// ——`MaxInt64 + (-MaxInt64+1)` 就是 1 个单位，不是任何边界值。同号那一侧的边界
// （尤其是 `MinInt64 + MinInt64`）由 TestAddOfTwoMinInt64ClampsToMinInt64 与
// TestAddAgreesWithSumCheckedOnEveryBoundary 钉。
//
// 判据 `(b.units > 0 && sum < a.units) || (b.units < 0 && sum > a.units)` 上
// 有四条等价变异（gremlins CONDITIONALS_BOUNDARY，把某一处比较符的边界挪一格），
// 理由都是「那一格的相等情形到不了」：
//
//   - `b.units > 0` 改成 `>= 0`、`b.units < 0` 改成 `<= 0`：多出来的是 b == 0，
//     而那时 sum 就是 a，两条里的 `sum < a` / `sum > a` 都不成立；
//   - `sum < a.units` 改成 `<=`、`sum > a.units` 改成 `>=`：多出来的是 sum == a，
//     它等价于 b == 0（int64 相加不引入别的偏差），而两支都要求 b 不为 0。
func TestAddOverflowDetectionOnlyOnSameSign(t *testing.T) {
	// 异号相加：不可能溢出，即便两边都很大
	big1 := Amount{units: math.MaxInt64}
	big2 := Amount{units: -math.MaxInt64 + 1}
	got := big1.Add(big2)
	if got.Units() != 1 {
		t.Errorf("异号相加被误判为溢出：得到 %d，期望 1", got.Units())
	}

	// 同号且都接近上限：应当被夹住
	huge := Amount{units: math.MaxInt64 / 2}
	sum := huge.Add(huge)
	if sum.IsNegative() {
		t.Errorf("同号溢出应夹住而不是回绕，得到 %s", sum)
	}
}

// Add 与 SumChecked 对同一对输入必须给出同一个答案。
//
// # 为什么这是一条独立的判据
//
// Add 的注释把「夹住的边界值不等于真实和、调用方从返回值上看不出被夹过」这件事
// 交代给了调用方，也就是说两条路都在、由调用方按场景挑。那它们对同一对输入就不能
// 各说各话：一条说「放不下」（SumChecked 报 ok=false），另一条却给出一个看起来
// 正常的数——这一格原来正是这样（Add 给 0，SumChecked 给 MinInt64）。
//
// 所以这里比的是两个函数的**一致性**，而不是一串记下来的期望值：将来任一侧再被
// 改坏、或者回绕换了个花样，这条都会说话。表里只挑边界上那几个值 + 两个溢出方向
// 各一对，不铺成一张大输入表——铺开了也证明不了「下一个回绕花样」。
//
// `92233720368.54775807` 就是 MaxInt64 个单位，`-92233720368.54775808` 是 MinInt64。
func TestAddAgreesWithSumCheckedOnEveryBoundary(t *testing.T) {
	max, min := Amount{units: math.MaxInt64}, Amount{units: math.MinInt64}
	// 一个**最小单位**（1e-8 元），不是 One（One 是 1 元）。
	plusUnit, minusUnit := Amount{units: 1}, Amount{units: -1}
	cases := []struct {
		name    string
		a, b    Amount
		want    string
		clamped bool // SumChecked 是否应当报 ok=false
	}{
		{"正向溢出：两个上界", max, max, "92233720368.54775807", true},
		{"正向溢出：上界再加一个最小单位", max, plusUnit, "92233720368.54775807", true},
		{"负向溢出：两个下界", min, min, "-92233720368.54775808", true},
		{"负向溢出：下界再减一个最小单位", min, minusUnit, "-92233720368.54775808", true},
		{"下界加一个最小单位：放得下", min, plusUnit, "-92233720368.54775807", false},
		{"上界减一个最小单位：放得下", max, minusUnit, "92233720368.54775806", false},
		{"两端相加：放得下", max, min, "-0.00000001", false},
		{"上界加「比下界多一个单位」：和为 1 个最小单位", max, Amount{units: -math.MaxInt64 + 1}, "0.00000001", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sum, ok := SumChecked(tc.a, tc.b)
			if ok == tc.clamped {
				t.Fatalf("夹具的前提不成立：SumChecked 对 %s 与 %s 报 ok=%v（%s）",
					tc.a, tc.b, ok, sum)
			}
			if got := sum.String(); got != tc.want {
				t.Errorf("SumChecked(%s, %s) = %s，期望 %s", tc.a, tc.b, got, tc.want)
			}
			if got := tc.a.Add(tc.b).String(); got != tc.want {
				t.Errorf("Add(%s, %s) = %s，期望 %s——两条路对同一对输入给出了不同的答案",
					tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// clampInt64Sign 只看**符号**，不看那个已经被回绕过的值。
//
// 变异（gremlins CONDITIONALS_BOUNDARY，把 `sign > 0` 的 `>` 改成 `>= 0`）：
// 分得开两版的输入只有 `sign == 0` 这一个（那一格它落负边界），而 Add 的溢出分支
// 只在同号相加时走到这里，传进来的 sign 就是 a.units（同号相加时不为 0）——
// 包内到不了这一格。所以这里直接调它，把口径钉住，而不是写一句「到不了」了事。
//
// 第一个参数是「这里有一个回绕之后的值」的占位，**不参与判断**：下面两行传不同的
// 数而结果相同，就是把这件事写出来。
func TestClampInt64SignUsesOnlyTheSign(t *testing.T) {
	if got := clampInt64Sign(12345, 1); got != math.MaxInt64 {
		t.Errorf("正的 sign 应取上界，得到 %d", got)
	}
	if got := clampInt64Sign(-12345, -1); got != math.MinInt64 {
		t.Errorf("负的 sign 应取下界，得到 %d", got)
	}
	if got := clampInt64Sign(math.MinInt64, 1); got != math.MaxInt64 {
		t.Errorf("结果只由 sign 决定（回绕后的值不看），得到 %d", got)
	}
	// 只有直接调用才分得出来的那一格。
	if got := clampInt64Sign(1, 0); got != math.MinInt64 {
		t.Errorf("clampInt64Sign(_, 0) = %d，期望 %d（0 不算正数）", got, int64(math.MinInt64))
	}
}

// 求和溢出时 SumChecked 要说出来，而不是交回一个夹住的数。
//
// 夹住的那个数是**错的**：真实的和比 MaxInt64 大，而返回的边界值比它小。
func TestSumCheckedReportsOverflow(t *testing.T) {
	huge := Amount{units: math.MaxInt64}

	got, ok := SumChecked(huge, One)
	if ok {
		t.Errorf("真实和放不下时应当 ok=false，得到 %s", got)
	}
	// 报出来时仍返回夹住的值（这样 Sum 才是它的薄封装），但它不该被当结果用。
	if got.Units() != math.MaxInt64 {
		t.Errorf("溢出时应返回边界值，得到 %d", got.Units())
	}

	// 放得下时就是那个和，且 ok=true。
	if got, ok := SumChecked(MustParse("0.1"), MustParse("0.2")); !ok || got.String() != "0.30000000" {
		t.Errorf("SumChecked(0.1, 0.2) = %s, ok=%v，期望 0.30000000, true", got, ok)
	}
	if got, ok := SumChecked(); !ok || !got.IsZero() {
		t.Errorf("空输入的 SumChecked = %s, ok=%v，期望 0.00000000, true", got, ok)
	}
}

// SumChecked 判的是**真实的和**，不是累加过程中的中间值。
//
// 这是它与「逐个 Add 再看有没有溢出」的分界，也是余额那条路径不能用后者的
// 理由：[+MaxInt64, +145, -145] 的真实和是 MaxInt64（放得下），而逐步加在
// 第二项上就已经夹住了——那一组是一笔完全正常的余额，不该被判成溢出。
func TestSumCheckedJudgesTheRealSumNotTheIntermediate(t *testing.T) {
	huge := Amount{units: math.MaxInt64}
	amounts := []Amount{huge, MustParse("145"), MustParse("-145")}

	got, ok := SumChecked(amounts...)
	if !ok {
		t.Fatal("真实和是 MaxInt64，放得下，不该报溢出")
	}
	if got.Units() != math.MaxInt64 {
		t.Errorf("SumChecked = %d，期望 %d", got.Units(), int64(math.MaxInt64))
	}

	// 逐步加给出的是一个**偏小**的数（145 元那一截被夹掉了），而且不报错。
	stepwise := huge.Add(amounts[1]).Add(amounts[2])
	if stepwise.Cmp(got) == 0 {
		t.Fatal("逐步加应当与精确和不相等，否则这条用例证明不了两者的区别")
	}
	if stepwise.Cmp(got) > 0 {
		t.Errorf("逐步加的结果 %s 不该大于真实和 %s", stepwise, got)
	}
}

// Sum 与 Add 一样仍然夹住：它的调用点里有「夹住比报错好」的那些。
func TestSumStillClamps(t *testing.T) {
	got := Sum(Amount{units: math.MaxInt64}, One)
	if got.IsNegative() {
		t.Errorf("溢出不该回绕成负数: %s", got)
	}
	if got.Units() != math.MaxInt64 {
		t.Errorf("溢出应夹到最大值，得到 %d", got.Units())
	}
}

// 解析时的进位边界：小数部分进位恰好填满一个整数单位，以及它与整数上界相遇的那几格。
//
// `f >= unit` 那一行上有一条等价变异（gremlins CONDITIONALS_BOUNDARY，`>=` 改成
// `>`），它不是「拿一个输入就能分开」的那类，理由分两步写：
//
//   - 进到这一支之后，改坏的那一版会把 frac 写成 9 位的 "100000000"，parseInt64
//     读回来恰好也是 unit，于是 `whole*unit + fracUnits` 与进过位的那一版逐位相同；
//   - 下面那道溢出门槛（`whole > (MaxInt64-fracUnits)/unit`）在两版下也落在同一个
//     整数上：门槛里的 fracUnits 差一个 unit，而 whole 正好也差一个。
//
// 于是两版要么给出同一个数、要么都报 ErrOverflow，没有第三种。
//
// #668 之前这条**不是**等价：那时「整数部分顶在上界、小数还要进位」这一格，原版
// 把 `whole++` 回绕过去、静默返回 0，而变异版报错——一个输入就分得开。修掉那一格
// 之后两面才收敛（见下面表里最后三条）。所以这条用例的用处不是杀它，而是把**进位
// 这件事本身**钉住：真把进位那一支去掉，0.999999995 会得到 0.99999999。
func TestParseCarryFillsWholeUnit(t *testing.T) {
	// 0.999999995 的四舍五入会让小数部分恰好进位到 1.0
	got, err := Parse("0.999999995")
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "1.00000000" {
		t.Errorf("进位应传播到整数部分，得到 %s", got)
	}

	// 1.999999995 → 2
	got, err = Parse("1.999999995")
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "2.00000000" {
		t.Errorf("进位应传播到整数部分，得到 %s", got)
	}

	// 进位刚好发生在整数的上界之内：结果仍在范围内，不该被误拒。
	got, err = Parse("92233720367.999999999")
	if err != nil {
		t.Fatalf("上界之内的进位不该报溢出: %v", err)
	}
	if want := int64(9223372036800000000); got.Units() != want {
		t.Errorf("units = %d，期望 %d", got.Units(), want)
	}

	// 上界附近逐格钉住，每一格都写具体值。`92233720368.54775807` 就是
	// MaxInt64 个最小单位——刚刚好放得下那一个；再大一个最小单位就该报错。
	// 中间那几条走的是「四舍五入之后才越界」这条路：`...074` 与 `...075`
	// 只差被丢掉的那一位，而它决定进位与否。
	cases := []struct {
		in   string
		want string // 空表示应当报 ErrOverflow
	}{
		{"92233720368.54775806", "92233720368.54775806"},
		{"92233720368.54775807", "92233720368.54775807"},
		{"92233720368.547758074", "92233720368.54775807"},
		{"92233720368.547758075", ""},
		{"92233720368.54775808", ""},
		{"92233720368.999999999", ""},
		// 整数部分顶在上界上：没有小数也放不下（整数部分是**元**，不是最小单位）。
		{"9223372036854775807.00000000", ""},
		{"9223372036854775807.99999999", ""},
		// 顶在上界、小数还要进位——#668 的那一格。修之前它静默返回 0 且 err 为 nil。
		{"9223372036854775807.999999995", ""},
		// 整数部分少一个单位，但进位之后照样越界。
		{"9223372036854775806.999999995", ""},
	}
	for _, tc := range cases {
		got, err := Parse(tc.in)
		if tc.want == "" {
			if !errors.Is(err, ErrOverflow) {
				t.Errorf("Parse(%q) = %d, err=%v，期望 ErrOverflow", tc.in, got.Units(), err)
			}
			continue
		}
		if err != nil {
			t.Errorf("Parse(%q) 应当解析得出来，得到 %v", tc.in, err)
			continue
		}
		if got.String() != tc.want {
			t.Errorf("Parse(%q) = %s，期望 %s", tc.in, got.String(), tc.want)
		}
	}
}

// 超大中间结果被夹到边界时，正负方向要分别取正确的边界。
//
// 变异：把 `v.Sign() > 0` 改成别的，负向溢出会被夹到 MaxInt64——
// 一个「看起来正常的大正数」。原先测不到是因为构造一个超出 int64 的
// 中间结果需要特意设计输入。
//
// 还有一条（gremlins CONDITIONALS_BOUNDARY，把 `v.Sign() > 0` 的 `>` 改成
// `>= 0`）：分得开两版的输入只有一个——v 正好是 0，那一格它落负边界
// （`Sign() > 0` 为假）。三个调用点都只在「big.Int 放不进 int64」那一支调它
// （IsInt64 为假意味着 |v| > MaxInt64），v 不可能是 0，所以**包内到不了这一格**。
// 到不了不等于等价，只是没有调用方递得出那个输入——所以下面直接调一次，把函数
// 自身的口径钉住：0 不算正数。谁将来给它递一个 0 进来，拿到的就是 MinInt64。
func TestClampFromBigPicksCorrectBoundary(t *testing.T) {
	// MulInt 用一个超大乘数，让中间结果超出 int64
	huge := Amount{units: math.MaxInt64}
	if got := huge.MulInt(1000); got.Units() != math.MaxInt64 {
		t.Errorf("正向溢出应夹到 MaxInt64，得到 %d", got.Units())
	}
	if got := huge.MulInt(-1000); got.Units() != math.MinInt64 {
		t.Errorf("负向溢出应夹到 MinInt64，得到 %d", got.Units())
	}

	// 直接调用那一格：包内没有调用点会传 0（见上），所以只有这里能分出来。
	if got := clampFromBig(new(big.Int)); got != math.MinInt64 {
		t.Errorf("clampFromBig(0) = %d，期望 %d（0 不算正数）", got, int64(math.MinInt64))
	}
}

// Cmp 的三个分支都要被走到，包括两边相等时返回 0。
//
// 变异：把 `a.units < b.units` 改成 `<=`，相等时就会返回 -1 而不是 0。
// 原先测过 1 与 2 的比较、以及 a.Cmp(a)，但没测**两个不同的量**相等的情形——
// 而那恰好是变异能钻的空子。
func TestCmpAllBranches(t *testing.T) {
	// 两个独立的、值相同但来源不同的金额
	a := FromInt(5)
	b := MustParse("5.00000000")
	if a.Cmp(b) != 0 {
		t.Errorf("Cmp(5, 5.00000000) = %d，期望 0", a.Cmp(b))
	}
	if a.Cmp(MustParse("4.99999999")) != 1 {
		t.Error("5 应大于 4.99999999")
	}
	if a.Cmp(MustParse("5.00000001")) != -1 {
		t.Error("5 应小于 5.00000001")
	}
	// 负数侧的相等
	c := MustParse("-5")
	d := MustParse("-5.00000000")
	if c.Cmp(d) != 0 {
		t.Errorf("Cmp(-5, -5.00000000) = %d，期望 0", c.Cmp(d))
	}
}

// MulIntChecked 只在**真实乘积**放得进 int64 时说 ok，而返回值与 MulInt
// 逐字相同——两者是同一个运算的两个视图。
//
// 三个分支都要走：正常、每个方向溢出、乘零。乘零那条不能落到乘法上判：
// 零乘任何数都是零，把它报告成溢出会让「没用量」变成一次失败。
//
// 等价变异（gremlins INVERT_LOGICAL，`if n == 0 || a.units == 0` 的 `||` 改成
// `&&`）：与 MulInt 同形——落到下面的乘积是 0，返回 (Zero, true)，
// 与早退那条逐位相同。
func TestMulIntChecked(t *testing.T) {
	cases := []struct {
		name     string
		amount   Amount
		n        int64
		wantOK   bool
		wantUnit int64
	}{
		{"正常", MustParse("0.000014"), 1_000_000, true, 1_400_000_000},
		{"乘零", MustParse("1000"), 0, true, 0},
		{"零乘", Zero, math.MaxInt64, true, 0},
		{"正向溢出", MustParse("0.000014"), 1e16, false, math.MaxInt64},
		{"负向溢出", MustParse("-0.000014"), 1e16, false, math.MinInt64},
		{"边界之内", FromUnits(1), math.MaxInt64, true, math.MaxInt64},
	}
	for _, tc := range cases {
		got, ok := MulIntChecked(tc.amount, tc.n)
		if ok != tc.wantOK {
			t.Errorf("%s: ok = %v，期望 %v", tc.name, ok, tc.wantOK)
		}
		if got.Units() != tc.wantUnit {
			t.Errorf("%s: units = %d，期望 %d", tc.name, got.Units(), tc.wantUnit)
		}
		// 与 MulInt 同一个结果：checked 版存在的意义只在 ok，不在值。
		if plain := tc.amount.MulInt(tc.n); plain != got {
			t.Errorf("%s: MulInt = %d 与 MulIntChecked = %d 不一致", tc.name, plain.Units(), got.Units())
		}
	}
}
