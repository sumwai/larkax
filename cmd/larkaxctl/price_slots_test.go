package main

import (
	"flag"
	"strings"
	"testing"

	"github.com/sumwai/larkax/cmd/larkaxctl/internal/cli"
)

// TestDecimalShift 钉住「每百万 token → 每 token」那一步的十进制移位。
//
// 不经过 float64 是有意的：1.5 / 1e6 在二进制浮点里是近似值，再格式化回文本时
// 要么丢精度、要么写成 1.5e-06，而这一串最后要进计价表达式。用例里的期望全是
// 能数清零的个数的写法。
func TestDecimalShift(t *testing.T) {
	cases := []struct {
		name   string
		text   string
		places int
		want   string
	}{
		{"整数按百万折算", "1", 6, "0.000001"},
		{"一位小数", "1.5", 6, "0.0000015"},
		{"一位小数（低价）", "0.2", 6, "0.0000002"},
		{"整数九", "9", 6, "0.000009"},
		{"零", "0", 6, "0"},
		{"零的各种写法", "0.000", 6, "0"},
		{"前导零", "007.5", 6, "0.0000075"},
		{"整数位多于移位", "1234567", 6, "1.234567"},
		{"恰好移到位", "1000000", 6, "1"},
		{"移位为 0 原样规整", "1.50", 0, "1.5"},
		{"省略整数位", ".5", 6, "0.0000005"},
		{"带正号", "+2.5", 6, "0.0000025"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decimalShift(tc.text, tc.places)
			if err != nil {
				t.Fatalf("decimalShift(%q, %d) 出错：%v", tc.text, tc.places, err)
			}
			if got != tc.want {
				t.Errorf("decimalShift(%q, %d) = %q，期望 %q", tc.text, tc.places, got, tc.want)
			}
		})
	}
}

// TestDecimalShiftRejects 钉住三种不该被宽松接受的写法。
func TestDecimalShiftRejects(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{"指数写法", "1.5e-6", "指数"},
		{"负数", "-1", "负数"},
		{"空串", "  ", "空值"},
		{"不是数字", "abc", "不是十进制"},
		{"多个小数点", "1.2.3", "小数点"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decimalShift(tc.text, 6); err == nil {
				t.Fatalf("decimalShift(%q) 期望报错，却通过了", tc.text)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息 %q 里没有 %q", err.Error(), tc.want)
			}
		})
	}
}

// slotArgs 把「按项给单价」的那组旗标交给一个新 flag 集，返回拼好的表达式。
func slotExpr(t *testing.T, args ...string) (string, error) {
	t.Helper()
	fs := cli.NewFlagSet("price create", flag.ContinueOnError)
	slots := newPriceSlotFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("解析参数失败：%v", err)
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	return slots.expression(given)
}

// TestPriceSlotExpressionDefaultsToPerMillion 钉住默认单位：旗标里的数是每百万
// token 的价，拼进表达式时换成每 token。
//
// 期望值写死在断言里，不从被测函数算：`* 1e-6` 那种写法在换算被改成 1e-3 时
// 会跟着一起变，用例照样绿，而库里存的价差了一千倍。
func TestPriceSlotExpressionDefaultsToPerMillion(t *testing.T) {
	got, err := slotExpr(t,
		"--in", "1.5", "--out", "7.5",
		"--cr", "0.2", "--cw", "1.9", "--reasoning", "9")
	if err != nil {
		t.Fatalf("拼表达式出错：%v", err)
	}
	want := `{"prompt": 0.0000015, "completion": 0.0000075, ` +
		`"cache_read": 0.0000002, "cache_write": 0.0000019, "reasoning": 0.000009}`
	if got != want {
		t.Errorf("表达式 = %s\n期望 %s", got, want)
	}
}

// TestPriceSlotExpressionWritesEveryKey 钉住「缺省写 0」：只给主价时，其余四项
// 也要进表达式，而且是 0。
//
// 逐字比整条表达式而不是只断言 `prompt` 那一项：漏写一个键与写对了在
// 「prompt 是 0.0000015」这条断言下长得一样，而漏写的那一项正是让结算失败的那一项。
func TestPriceSlotExpressionWritesEveryKey(t *testing.T) {
	got, err := slotExpr(t, "--in", "0.15")
	if err != nil {
		t.Fatalf("拼表达式出错：%v", err)
	}
	want := `{"prompt": 0.00000015, "completion": 0, "cache_read": 0, ` +
		`"cache_write": 0, "reasoning": 0}`
	if got != want {
		t.Errorf("表达式 = %s\n期望 %s", got, want)
	}
}

// TestPriceSlotExpressionWritesEveryKeyPerToken 钉住「缺省写 0」与单位换算两条
// 路互不影响：--unit token 之下同样五项齐全，给了的那项不再移位。
func TestPriceSlotExpressionWritesEveryKeyPerToken(t *testing.T) {
	got, err := slotExpr(t, "--unit", "token", "--in", "0.0000015")
	if err != nil {
		t.Fatalf("拼表达式出错：%v", err)
	}
	want := `{"prompt": 0.0000015, "completion": 0, "cache_read": 0, ` +
		`"cache_write": 0, "reasoning": 0}`
	if got != want {
		t.Errorf("表达式 = %s\n期望 %s", got, want)
	}
}

// TestPriceSlotExpressionKeepsPerTokenUnit 钉住 --unit token：给的就是每 token
// 的价，不再移位。
func TestPriceSlotExpressionKeepsPerTokenUnit(t *testing.T) {
	got, err := slotExpr(t, "--unit", "token", "--in", "0.0000015")
	if err != nil {
		t.Fatalf("拼表达式出错：%v", err)
	}
	if want := `{"prompt": 0.0000015, "completion": 0, "cache_read": 0, ` +
		`"cache_write": 0, "reasoning": 0}`; got != want {
		t.Errorf("表达式 = %s，期望 %s", got, want)
	}
}

// TestPriceSlotExpressionRejects 钉住单位写错、空值与无法解析的价。
func TestPriceSlotExpressionRejects(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"未知单位", []string{"--unit", "usd", "--in", "1"}, "--unit"},
		{"单价是空串", []string{"--in", ""}, "空串"},
		{"单价是指数", []string{"--in", "1e-6"}, "指数"},
		{"单价是负数", []string{"--in", "-1"}, "负数"},
		// 主价一项都没给：只给缓存价时其余各项按 0 算，那条规则在正常请求上白送。
		{"只给缓存价", []string{"--cr", "0.2"}, "--in 与 --out 至少给一项"},
		{"只给推理价", []string{"--reasoning", "9"}, "--in 与 --out 至少给一项"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := slotExpr(t, tc.args...); err == nil {
				t.Fatalf("期望报错，却通过了")
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息 %q 里没有 %q", err.Error(), tc.want)
			}
		})
	}
}

// TestPriceCreateFromSlotFlags 断言 price create 的单价旗标最后落成同一条表达式。
func TestPriceCreateFromSlotFlags(t *testing.T) {
	req, _, err := parsePriceCreate([]string{
		"--scope", "platform", "--name", "n1", "--by", "ops",
		"--in", "1.5", "--out", "7.5",
	})
	if err != nil {
		t.Fatalf("parsePriceCreate 出错：%v", err)
	}
	if want := `{"prompt": 0.0000015, "completion": 0.0000075, "cache_read": 0, ` +
		`"cache_write": 0, "reasoning": 0}`; req.Expr != want {
		t.Errorf("Expr = %s，期望 %s", req.Expr, want)
	}
	if req.Name != "n1" {
		t.Errorf("Name = %q，期望 n1", req.Name)
	}
}

// TestPriceCreateRejectsMixedPriceForms 钉住三种给法互斥。
func TestPriceCreateRejectsMixedPriceForms(t *testing.T) {
	if _, _, err := parsePriceCreate([]string{
		"--scope", "platform", "--name", "n1", "--by", "ops",
		"--expr", testExpr, "--in", "1.5",
	}); err == nil || !strings.Contains(err.Error(), "只能给一种") {
		t.Fatalf("--expr 与 --in 同时给应当报错，得到 %v", err)
	}
}

// TestPriceCreateRejectsUnitAlone 钉住 --unit 单独出现不算给了价。
func TestPriceCreateRejectsUnitAlone(t *testing.T) {
	_, _, err := parsePriceCreate([]string{
		"--scope", "platform", "--name", "n1", "--by", "ops", "--unit", "token",
	})
	if err == nil || !strings.Contains(err.Error(), "--unit") {
		t.Fatalf("只给 --unit 应当报错，得到 %v", err)
	}
}
