// Package money 处理金额的定点运算。
//
// 为什么不用 float64：0.1 + 0.2 != 0.3，而账目上差一分钱就是错账。
// 金额一律用定点数，且**精度与舍入规则集中在这里**，不散落到各业务包里——
// 一处舍入与另一处不一致，会在对账时表现为一笔永远找不平的差额。
//
// 内部用 int64 存「最小单位的整数倍」：CNY 是分，但精度取 8 位小数
// （与数据库的 numeric(20, 8) 一致）。用整数而不是 big.Rat 或 decimal 库：
// 加减乘都是整数运算，不会引入新的精度问题，性能也最好。
package money

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
)

// Scale 是小数位数，与数据库的 numeric(20, 8) 一致。
//
// 取 8 而不是 2：上游按 token 计价，单价低到「每千 token 0.0001 元」这个量级，
// 用 2 位小数会把单价直接抹成 0。
const Scale = 8

// unit 是一单位的定点倍数，即 10^Scale。
const unit int64 = 100_000_000

// Amount 是一个定点金额。
//
// 零值就是 0，可以直接用，不需要构造函数。
type Amount struct {
	// units 是金额乘以 10^Scale 后的整数。
	units int64
}

// 几个常用常量。Zero 与 Amount{} 等价，写出来是为了可读性。
var (
	Zero Amount
	One  = Amount{units: unit}
)

// ErrOverflow 表示运算结果超出 int64 能表达的范围。
//
// 它不该在正常业务里出现（10^11 元量级才会溢出），出现了说明有人把
// 用户输入直接当金额用了。单独一个错误类型是为了让上层能区分
// 「金额算错了」与「金额太大」。
var ErrOverflow = errors.New("金额超出可表示范围")

// FromInt 用整数元构造。
//
// 目前只有测试在调它。
func FromInt(yuan int64) Amount {
	if yuan > math.MaxInt64/unit {
		// 直接乘会溢出，回绕出来的可能是一个「看起来正常」的负数金额，
		// 那是账目上最难排查的一类错。夹到边界。
		return Amount{units: math.MaxInt64}
	}
	if yuan < math.MinInt64/unit {
		return Amount{units: math.MinInt64}
	}
	return Amount{units: yuan * unit}
}

// FromUnits 用最小单位构造。
//
// 目前只有测试在调它。**库里的金额列存的不是最小单位**：它是 numeric(20, 8)，
// 读写两侧都走十进制字符串（写用 Amount.String，读用 Parse），
// 所以这个构造器在生产里没有入口。
func FromUnits(units int64) Amount { return Amount{units: units} }

// Units 返回最小单位数。
//
// 目前只有测试在调它，原因与 FromUnits 相同：库里的金额列是 numeric，
// 两侧都走十进制字符串，没有一处把最小单位数直接送进 SQL。
func (a Amount) Units() int64 { return a.units }

// Parse 解析一个十进制字符串，例如 "12.34"。
//
// 超过 Scale 位的小数被**四舍五入**，不是截断：截断会让一批小额扣费
// 系统性地偏向用户，累积起来是一笔真实的差额。
func Parse(s string) (Amount, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Zero, errors.New("金额为空")
	}
	neg := false
	switch s[0] {
	case '-':
		neg, s = true, s[1:]
	case '+':
		s = s[1:]
	}
	if s == "" {
		return Zero, fmt.Errorf("金额 %q 只有符号", s)
	}

	intPart, fracPart, _ := strings.Cut(s, ".")
	if intPart == "" {
		intPart = "0"
	}
	for _, r := range intPart + fracPart {
		if r < '0' || r > '9' {
			return Zero, fmt.Errorf("金额 %q 含非数字字符", s)
		}
	}

	whole, err := parseInt64(intPart)
	if err != nil {
		return Zero, err
	}

	// 补齐或截断到 Scale 位
	frac := fracPart
	if len(frac) > Scale {
		// 四舍五入：看被丢掉的第一位
		roundUp := frac[Scale] >= '5'
		frac = frac[:Scale]
		if roundUp {
			// 进位可能传播到整数部分，用整数运算处理
			f, _ := parseInt64(frac)
			f++
			if f >= unit {
				whole++
				f -= unit
			}
			frac = fmt.Sprintf("%0*d", Scale, f)
		}
	}
	for len(frac) < Scale {
		frac += "0"
	}

	fracUnits, _ := parseInt64(frac)
	// whole*unit + fracUnits 都可能溢出
	if whole > (math.MaxInt64-fracUnits)/unit {
		return Zero, ErrOverflow
	}
	units := whole*unit + fracUnits
	if neg {
		units = -units
	}
	return Amount{units: units}, nil
}

func parseInt64(s string) (int64, error) {
	var n int64
	if s == "" {
		return 0, nil
	}
	for _, r := range s {
		d := int64(r - '0')
		if n > (math.MaxInt64-d)/10 {
			return 0, ErrOverflow
		}
		n = n*10 + d
	}
	return n, nil
}

// MustParse 在解析失败时 panic。
//
// 目前只有测试在调它（用例拿它写期望值）。生产里的金额一律走 Parse——
// 那里出错要报出来而不是炸掉进程，因为来源是配置与上游报文。
func MustParse(s string) Amount {
	a, err := Parse(s)
	if err != nil {
		panic(fmt.Sprintf("money.MustParse(%q): %v", s, err))
	}
	return a
}

// String 输出定点十进制，小数末尾的零保留到 Scale 位。
//
// 保留到 Scale 位而不是去掉尾零：账目里 "1" 与 "1.00000000" 是同值，
// 但对账输出的一致性更重要——同一列里混着两种写法会让比对脚本出错。
func (a Amount) String() string {
	units := a.units
	sign := ""
	if units < 0 {
		sign = "-"
		units = -units
	}
	whole := units / unit
	frac := units % unit
	return fmt.Sprintf("%s%d.%0*d", sign, whole, Scale, frac)
}

// Float64 转成浮点，**只用于展示与计算分数**，绝不能用于记账。
//
// 命名上不带 To 前缀是有意的：调用点看到 Float64 会多想一下。
func (a Amount) Float64() float64 {
	return float64(a.units) / float64(unit)
}

// IsZero 判断是否为零。
func (a Amount) IsZero() bool { return a.units == 0 }

// IsNegative 判断是否为负。
func (a Amount) IsNegative() bool { return a.units < 0 }

// IsPositive 判断是否为正。
func (a Amount) IsPositive() bool { return a.units > 0 }

// Add 相加。
func (a Amount) Add(b Amount) Amount {
	sum := a.units + b.units
	// 溢出检测：同号相加的结果必然同号，否则溢出了
	if (a.units > 0 && b.units > 0 && sum < 0) || (a.units < 0 && b.units < 0 && sum > 0) {
		return Amount{units: clampInt64Sign(sum, a.units)}
	}
	return Amount{units: sum}
}

// Sub 相减。
func (a Amount) Sub(b Amount) Amount { return a.Add(b.Neg()) }

// Neg 取负。
func (a Amount) Neg() Amount {
	if a.units == math.MinInt64 {
		// -MinInt64 溢出。返回最大值而不是静默回绕成自己。
		return Amount{units: math.MaxInt64}
	}
	return Amount{units: -a.units}
}

// Sum 求一组金额之和。
func Sum(amounts ...Amount) Amount {
	var total Amount
	for _, a := range amounts {
		total = total.Add(a)
	}
	return total
}

// MulInt 乘以一个整数。用于「单价 × 数量」这类运算。
//
// 不需要任何除法：units 本身已经是定点表示，乘以整数不改变小数位。
func (a Amount) MulInt(n int64) Amount {
	if n == 0 || a.units == 0 {
		return Zero
	}
	product := new(big.Int).Mul(big.NewInt(a.units), big.NewInt(n))
	if !product.IsInt64() {
		return Amount{units: clampFromBig(product)}
	}
	return Amount{units: product.Int64()}
}

// MulAmount 两个金额相乘。
//
// 语义上是「每单位 a 元，共 b 个单位」。结果的小数位取 Scale，
// **四舍五入**——截断会让一批小额计算系统性地偏向一方。
//
// 目前只有测试在调它：按量计费用的是 MulInt（单价 × 整数用量），
// 计价规则里没有「金额 × 金额」这一项。
func MulAmount(a, b Amount) Amount {
	product := new(big.Int).Mul(big.NewInt(a.units), big.NewInt(b.units))
	// 两个 Scale 位小数相乘 → 结果有 2*Scale 位，除以 unit 回到 Scale 位。
	// 缩放在 unit² 而不是 unit：units 的量纲是元 × 10^Scale，
	// 两个相乘是元² × 10^(2·Scale)，除以 unit 得到的是元² × 10^Scale——
	// 那才是一个「每单位 a 元 × b 个单位」的结果。
	return fromBigDivRounded(product, big.NewInt(unit))
}

// MulFloat 乘以一个浮点系数（倍率、折扣）。
//
// 用 big.Rat 而不是 float64 直接乘：0.85 这样的折扣系数在浮点里不精确，
// 乘上金额之后可能差一分。倍率一旦用上就会反复用于每一次计费，
// 一分钱的偏差乘上百万次请求就是一笔真实的差额。
//
// 目前只有测试在调它：**现在还没有倍率这个概念**，计价规则里没有乘法。
// 它是为「折扣 / 倍率」预置的入口，先用 big.Rat 把口径定死。
//
// 除法只用在最后一步：factor 是有理数，乘积要除以它的分母才折回 units
// 量纲，并按 fromBigDivRounded 四舍五入。所以它是本包里带舍入的入口之一。
func MulFloat(a Amount, factor float64) Amount {
	if math.IsNaN(factor) || math.IsInf(factor, 0) {
		// 系数非法时返回零而不是 panic：它来自配置，而配置写错
		// 不该让整个进程挂掉。返回零会让账目明显不对，能被发现。
		return Zero
	}
	r := new(big.Rat).SetFloat64(factor)
	if r == nil {
		return Zero
	}
	product := new(big.Int).Mul(big.NewInt(a.units), r.Num())
	return fromBigDivRounded(product, r.Denom())
}

// DivInt 除以一个整数。零除返回零而不是 panic——
// 它来自「数量为 0」这类平凡情况，不该让进程挂掉。
//
// 目前只有测试在调它：金额库的运算面，这一档还没有生产调用方。
func (a Amount) DivInt(n int64) Amount {
	if n == 0 {
		return Zero
	}
	return fromBigDivRounded(big.NewInt(a.units), big.NewInt(n))
}

// fromBigDivRounded 计算 numerator/denominator 并四舍五入到整数。
//
// 四舍五入而不是截断，也不是 banker's rounding：
// 截断有系统性偏差，而 banker's rounding 对使用者来说难以解释
// （「为什么 0.5 有时进有时不进」）。四舍五入是唯一能被非专业人士
// 一眼验证的规则，而账目需要能被人工核对。
func fromBigDivRounded(numerator, denominator *big.Int) Amount {
	if denominator.Sign() == 0 {
		return Zero
	}
	quotient, remainder := new(big.Int).QuoRem(numerator, denominator, new(big.Int))

	// 看余数是否达到除数的一半（取绝对值比较）
	absRem := new(big.Int).Abs(remainder)
	absDen := new(big.Int).Abs(denominator)
	// 2*|rem| >= |den| 则进位
	twiceRem := new(big.Int).Lsh(absRem, 1)
	if twiceRem.Cmp(absDen) >= 0 {
		if numerator.Sign() < 0 {
			quotient.Sub(quotient, big.NewInt(1))
		} else {
			quotient.Add(quotient, big.NewInt(1))
		}
	}

	if !quotient.IsInt64() {
		return Amount{units: clampFromBig(quotient)}
	}
	return Amount{units: quotient.Int64()}
}

// clampFromBig 把一个超范围的 big.Int 夹到 int64 边界。
//
// 夹住而不是回绕：回绕会把一个天文数字变成一个看起来正常的负数金额，
// 那比一个明显离谱的值更危险。
func clampFromBig(v *big.Int) int64 {
	if v.Sign() > 0 {
		return math.MaxInt64
	}
	return math.MinInt64
}

// clampInt64Sign 把溢出后的和夹到 int64 边界。
//
// 方向由 sign 决定：同号相加溢出时，结果的符号就是两个加数的符号，
// 正数溢出取 MaxInt64、负数溢出取 MinInt64。v 只用来表明「这里有一个
// 溢出后的值」，具体数值不可信，所以不参与判断。
func clampInt64Sign(_ int64, sign int64) int64 {
	if sign > 0 {
		return math.MaxInt64
	}
	return math.MinInt64
}

// Min 返回较小者。
//
// 目前只有测试在调它。孪生的 Max 在生产里有用到（billing/reserve.go 拿它
// 取「输出单价与推理单价的较大者」和「保底与按量金额的较大者」），
// 这一半是配对的运算面。
func Min(a, b Amount) Amount {
	if a.units <= b.units {
		return a
	}
	return b
}

// Max 返回较大者。
func Max(a, b Amount) Amount {
	if a.units >= b.units {
		return a
	}
	return b
}

// ClampZero 把负值夹到零。用于「不该为负但可能算成负」的中间结果。
//
// 目前只有测试在调它：金额库的运算面，这一档还没有生产调用方。
func (a Amount) ClampZero() Amount {
	if a.units < 0 {
		return Zero
	}
	return a
}

// Cmp 比较大小，返回 -1 / 0 / 1。
func (a Amount) Cmp(b Amount) int {
	switch {
	case a.units < b.units:
		return -1
	case a.units > b.units:
		return 1
	}
	return 0
}
