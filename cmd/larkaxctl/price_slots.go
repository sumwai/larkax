package main

import (
	"errors"
	"fmt"
	"strings"
)

// priceSlot 是一个 token 计价项在命令行上的样子：表达式里的键、正式旗标名、
// 旧长名，以及它是不是主价那一项。
type priceSlot struct {
	key     string
	flag    string
	legacy  string
	primary bool
}

// priceSlots 是五个 token 计价项：表达式里用的键、正式旗标名，以及旧长名。
//
// 三列而不是从键推导：键里的下划线在命令行上是连字符（`--cache-read`），从键推出
// 旗标名要多一层替换，而写成三列一眼看得出它们指的是同一项。
//
// 正式名短是因为这五个天天要写（`--in 0.15 --out 0.6 --cr 0.003`），旧长名一个
// 都没删——已经在跑的脚本与部署流程照旧能用，两个名字落在同一个格子上
// （见 newPriceSlotFlags）。
//
// primary 标的是主价那两项：这组旗标被用到时它们至少要给一项。只给缓存价或
// 推理价时，主价会被写成 0，而那条规则在每一个正常请求上都是白送。
var priceSlots = []priceSlot{
	{"prompt", "in", "prompt", true},
	{"completion", "out", "completion", true},
	{"cache_read", "cr", "cache-read", false},
	{"cache_write", "cw", "cache-write", false},
	{"reasoning", "reasoning", "", false},
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
//
// 每项走 legacyStringFlag：正式名与旧长名接到**同一个** stringStorage 格子上，
// 两个名字都写时后写的赢（与 flag 包对同一个选项写两遍的处置一致）。分两次
// fs.String 会得到两个各自为政的格子，`--prompt 1.5` 写进的那一格没人读，
// 表现是「旧写法像没写一样」——这正是别名机制存在的理由。
//
// 旧长名经 aliasVar 注册，不进补全（见 flagset.go 的「别名专用注册入口」）。
func newPriceSlotFlags(fs *flagSet) priceSlotFlags {
	values := make(map[string]*string, len(priceSlots))
	for _, slot := range priceSlots {
		if slot.legacy == "" {
			values[slot.key] = fs.String(slot.flag, "", "")
			continue
		}
		values[slot.key] = legacyStringFlag(fs, slot.flag, slot.legacy, "")
	}
	return priceSlotFlags{
		values: values,
		unit:   fs.String("unit", "mtok", ""),
	}
}

// givenNames 是这一项认的全部写法。
//
// fs.Visit 记的是**实际写出来的那个名字**，所以判「这次给没给」时两种写法都要查：
// 只查正式名的表现是 `--prompt 1.5` 被判成没给，而判它的地方是主价那一项与
// 三种给法的互斥检查。
func (s priceSlot) givenNames() []string {
	if s.legacy == "" {
		return []string{s.flag}
	}
	return []string{s.flag, s.legacy}
}

// slotGiven 报告这一项这次有没有给值。
func (f priceSlotFlags) slotGiven(given map[string]bool, slot priceSlot) bool {
	return flagGiven(given, slot.givenNames()...)
}

// anyGiven 报告这组旗标里有没有给值。
//
// 只认值不认 --unit：只写 `--unit token` 而一个单价都没给，等于什么都没说，
// 由 expression 报错（见下），不在这里算作「给了」。
func (f priceSlotFlags) anyGiven(given map[string]bool) bool {
	for _, slot := range priceSlots {
		if f.slotGiven(given, slot) {
			return true
		}
	}
	return false
}

// unitGiven 报告 --unit 是不是显式给过。
func (f priceSlotFlags) unitGiven(given map[string]bool) bool { return given["unit"] }

// expression 把五个计价项拼成一条计价表达式。
//
// **五项全进表达式**：给了的按给的价，没给的显式写 0。这一条是本组旗标的
// 保护所在——上游报了哪一项，规则里就必须有哪一项的单价，少一项不是少收，是
// 这一笔的结算失败（非流式请求调用方拿到 500，流式请求只落在服务端日志里）。
// 写 0 是「免费」，与「没写」不是一回事。
//
// 键序就是 priceSlots 的次序，与 JSON 里手写的那份一致，好让两种给法落库
// 落成同一串字节（见 TestPriceSlotExpressionMatchesHandwritten）。
func (f priceSlotFlags) expression(given map[string]bool) (string, error) {
	places, err := priceUnitPlaces(*f.unit)
	if err != nil {
		return "", err
	}
	// 主价至少给一项：只给缓存价或推理价时，其余四项会被写成 0，而这条规则
	// 在每一个正常请求上都按 0 元结算。那几乎总是「旗标写漏了」，不是本意。
	primary := 0
	for _, slot := range priceSlots {
		if slot.primary && f.slotGiven(given, slot) {
			primary++
		}
	}
	if primary == 0 {
		return "", errors.New("--in 与 --out 至少给一项：这两项是输入与输出的单价，" +
			"只给缓存价或推理价时其余各项会被写成 0，那条规则在正常请求上按 0 元结算")
	}

	parts := make([]string, 0, len(priceSlots))
	for _, slot := range priceSlots {
		rate := "0"
		if f.slotGiven(given, slot) {
			raw := strings.TrimSpace(*f.values[slot.key])
			if raw == "" {
				return "", fmt.Errorf("--%s 是空串（多半是一个没展开的变量）", slot.flag)
			}
			shifted, shiftErr := decimalShift(raw, places)
			if shiftErr != nil {
				return "", fmt.Errorf("--%s %q 无法解析：%w", slot.flag, raw, shiftErr)
			}
			rate = shifted
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
