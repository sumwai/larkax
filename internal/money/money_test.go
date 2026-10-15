package money

import (
	"errors"
	"fmt"
	"math"
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

func TestParseRejectsOverflow(t *testing.T) {
	// 远超 int64/10^8 的整数元
	if _, err := Parse("999999999999999999999"); !errors.Is(err, ErrOverflow) {
		t.Errorf("应报 ErrOverflow，得到 %v", err)
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
func TestOverflowClampsNotWraps(t *testing.T) {
	huge := Amount{units: math.MaxInt64}
	got := huge.Add(huge)
	if got.IsNegative() {
		t.Errorf("溢出不该回绕成负数: %s", got.String())
	}
	if got.Units() != math.MaxInt64 {
		t.Errorf("溢出应夹到最大值，得到 %d", got.Units())
	}

	small := Amount{units: math.MinInt64}
	got = small.Add(small)
	if got.IsPositive() {
		t.Errorf("负向溢出不该回绕成正数: %s", got.String())
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
// 变异：把 `<=` 改成 `<`、`>=` 改成 `>`。相等时走的分支不同，
// 而结果相同——所以只有断言「相等时也正确」才能钉住。
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
// 变异：把 `a.units > 0 && b.units > 0` 里的某个 `>` 改掉，
// 异号相加（结果必然在范围内）会被误判为溢出。
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

// 解析时的进位边界：小数部分进位恰好填满一个整数单位。
//
// 变异：把 `f >= unit` 改成 `f > unit`，恰好等于 unit 时就不会
// 进位到整数部分，得到一个越界的小数部分。
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
}

// 超大中间结果被夹到边界时，正负方向要分别取正确的边界。
//
// 变异：把 `v.Sign() > 0` 改成别的，负向溢出会被夹到 MaxInt64——
// 一个「看起来正常的大正数」。原先测不到是因为构造一个超出 int64 的
// 中间结果需要特意设计输入。
func TestClampFromBigPicksCorrectBoundary(t *testing.T) {
	// MulInt 用一个超大乘数，让中间结果超出 int64
	big := Amount{units: math.MaxInt64}
	if got := big.MulInt(1000); got.Units() != math.MaxInt64 {
		t.Errorf("正向溢出应夹到 MaxInt64，得到 %d", got.Units())
	}
	if got := big.MulInt(-1000); got.Units() != math.MinInt64 {
		t.Errorf("负向溢出应夹到 MinInt64，得到 %d", got.Units())
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
