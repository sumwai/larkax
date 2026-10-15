package money

import (
	"errors"
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
