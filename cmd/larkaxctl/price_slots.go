package main

import (
	"errors"
	"fmt"
	"strings"
)

// priceSlots 是五个 token 计价项：表达式里用的键，以及对应的旗标名。
//
// 两者写成两个字段而不是推导：键里的下划线在命令行上是连字符（`--cache-read`），
// 从键推出旗标名要多一层替换，而写成两列一眼看得出它们指的是同一项。
var priceSlots = []struct {
	key  string
	flag string
}{
	{"prompt", "prompt"},
	{"completion", "completion"},
	{"cache_read", "cache-read"},
	{"cache_write", "cache-write"},
	{"reasoning", "reasoning"},
}

// priceSlotFlags 是「按项给单价」那组旗标。
//
// 单位默认是**每百万 token**：上游报价单就是这样写的（每百万输入 1.5 元），
// 而按 token 报价意味着要数小数点后六个零——`0.0000015` 与 `0.00000015` 只差
// 一个零，看上去都对，错了就是十倍价。存库前换成每 token 的小数，转换用十进制
// 字符串移位，不经过 float64：那一步会把 1.5e-06 变成一段近似值，再写进表达式。
type priceSlotFlags struct {
	values map[string]*string
	unit   *string
}

// newPriceSlotFlags 把五个单价旗标与 --unit 注册到一行命令的 flag 集上。
func newPriceSlotFlags(fs *flagSet) priceSlotFlags {
	values := make(map[string]*string, len(priceSlots))
	for _, slot := range priceSlots {
		values[slot.key] = fs.String(slot.flag, "", "")
	}
	return priceSlotFlags{
		values: values,
		unit:   fs.String("unit", "mtok", ""),
	}
}

// anyGiven 报告这组旗标里有没有给值。
//
// 只认值不认 --unit：只写 `--unit token` 而一个单价都没给，等于什么都没说，
// 由 expression 报错（见下），不在这里算作「给了」。
func (f priceSlotFlags) anyGiven(given map[string]bool) bool {
	for _, slot := range priceSlots {
		if given[slot.flag] {
			return true
		}
	}
	return false
}

// unitGiven 报告 --unit 是不是显式给过。
func (f priceSlotFlags) unitGiven(given map[string]bool) bool { return given["unit"] }

// expression 把给了的项拼成一条计价表达式。
//
// 不给的项不进表达式：五项里缺哪一项，用到它的那一笔结算就会失败（见 priceUsage
// 里那段说明）。提示由调用方在写入后打出来，这里只负责拼。
func (f priceSlotFlags) expression(given map[string]bool) (string, error) {
	places, err := priceUnitPlaces(*f.unit)
	if err != nil {
		return "", err
	}
	parts := make([]string, 0, len(priceSlots))
	for _, slot := range priceSlots {
		if !given[slot.flag] {
			continue
		}
		raw := strings.TrimSpace(*f.values[slot.key])
		if raw == "" {
			return "", fmt.Errorf("--%s 是空串（多半是一个没展开的变量）", slot.flag)
		}
		rate, shiftErr := decimalShift(raw, places)
		if shiftErr != nil {
			return "", fmt.Errorf("--%s %q 无法解析：%w", slot.flag, raw, shiftErr)
		}
		parts = append(parts, fmt.Sprintf("%q: %s", slot.key, rate))
	}
	return "{" + strings.Join(parts, ", ") + "}", nil
}

// priceUnitPlaces 把 --unit 的取值换成「小数点左移几位」。
func priceUnitPlaces(unit string) (int, error) {
	switch strings.TrimSpace(unit) {
	case "mtok", "":
		return 6, nil
	case "token":
		return 0, nil
	}
	return 0, fmt.Errorf("--unit 只能是 mtok（每百万 token，默认）或 token，得到 %q", unit)
}

// decimalShift 把十进制字面量的小数点左移 places 位，返回规整后的十进制文本。
//
// 输出不带指数：拼进的是计价表达式，而表达式里的 `1.5e-06` 能编译，却让人看不出
// 这到底是多少——`0.0000015` 至少能数清零的个数。
//
// 只接受普通的十进制写法（可带正号与小数点）。指数写法要报错而不是宽松地接受：
// 收到 `1e-6` 的人多半想的是「每百万 1 元」那种写法，两种意图在这一行上分不清。
func decimalShift(text string, places int) (string, error) {
	s := strings.TrimSpace(text)
	if s == "" {
		return "", errors.New("空值")
	}
	if strings.ContainsAny(s, "eE") {
		return "", errors.New("不要用指数写法（1e-6），写成 0.0000015 这样的十进制")
	}
	if strings.HasPrefix(s, "-") {
		return "", errors.New("单价不能是负数")
	}
	s = strings.TrimPrefix(s, "+")

	intPart, fracPart, _ := strings.Cut(s, ".")
	if intPart == "" && fracPart == "" {
		return "", errors.New("没有数字")
	}
	for _, r := range intPart + fracPart {
		if r == '.' {
			return "", errors.New("多了小数点")
		}
		if r < '0' || r > '9' {
			return "", fmt.Errorf("不是十进制数：%q", string(r))
		}
	}

	raw := intPart + fracPart
	trimmed := strings.TrimLeft(raw, "0")
	if trimmed == "" {
		return "0", nil
	}
	// 小数点在 digits 里的位置：整数部分长度，减去被去掉的前导零，再减去左移
	// 的位数。漏掉「被去掉的前导零」这一项时，0.2 会算成 0.000002——差一位。
	point := len(intPart) - (len(raw) - len(trimmed)) - places
	digits := trimmed

	var out string
	switch {
	case point <= 0:
		out = "0." + strings.Repeat("0", -point) + digits
	case point >= len(digits):
		out = digits + strings.Repeat("0", point-len(digits))
	default:
		out = digits[:point] + "." + digits[point:]
	}
	return trimDecimalZeros(out), nil
}

// trimDecimalZeros 去掉小数部分末尾的零与孤立的小数点，整数部分前导零只留一位。
func trimDecimalZeros(text string) string {
	intPart, fracPart, hasPoint := strings.Cut(text, ".")
	intPart = strings.TrimLeft(intPart, "0")
	if intPart == "" {
		intPart = "0"
	}
	if hasPoint {
		fracPart = strings.TrimRight(fracPart, "0")
	}
	if fracPart == "" {
		return intPart
	}
	return intPart + "." + fracPart
}
