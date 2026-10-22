package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/expiry"
)

// 这一层放「到什么时候为止」的共用口径：--expires-at / --ttl / --forever 三种
// 写法在 plan grant、apikey create|update、consent grant 上说的是同一件事。
//
// 三处各写一遍换算时，差别不会报错：1 月 31 日加一个月在一处落到 2 月 28 日、
// 在另一处落到 3 月 2 日，而两条命令都只说「已设置」。远期上限同理——两个
// 名字就是两份口径的开始。

// expiryNow 返回换算用的「现在」。它是包级变量，用例可以换成固定时刻，
// 好让 --ttl 在整条命令路径上也能与注入的那个 now 逐秒对拍（用法同 outputFormat）。
var expiryNow = time.Now

// errExpiryNotGiven 表示三个过期旗标一个都没给。调用方据此区分「不设过期」
// 与「设了一个不合法的时间」。
var errExpiryNotGiven = errors.New("没有给过期时间")

// expiryFlags 是「到什么时候为止」的三种写法在命令行上的值。
type expiryFlags struct {
	expiresAt string
	ttl       string
	forever   bool
}

// resolveExpiry 在 --expires-at / --ttl / --forever 里收出唯一的那个绝对时刻。
//
// 三个旗标最多给一个：它们说的是同一件事；同时给两个时，后一个会在没有任何
// 提示的情况下赢。parseAbsolute 是 --expires-at 的解析口径，由调用方决定——
// apikey 与 plan 要求它必须在将来，consent 只认 RFC3339，未来与否由写入层判。
func resolveExpiry(seen map[string]bool, f expiryFlags, now time.Time,
	parseAbsolute func(string) (time.Time, error),
) (time.Time, error) {
	expiresGiven := flagGiven(seen, "expires-at")
	ttlGiven := flagGiven(seen, "ttl")
	foreverGiven := f.forever

	names := make([]string, 0, 3)
	if expiresGiven {
		names = append(names, "--expires-at")
	}
	if ttlGiven {
		names = append(names, "--ttl")
	}
	if foreverGiven {
		names = append(names, foreverFlagName(seen))
	}
	if len(names) > 1 {
		return time.Time{}, fmt.Errorf("%s 不能同时给：过期时间只能有一种写法",
			strings.Join(names, " 与 "))
	}

	switch {
	case expiresGiven:
		return parseAbsolute(f.expiresAt)
	case ttlGiven:
		return ttlExpiry(now, f.ttl)
	case foreverGiven:
		return expiry.Forever(), nil
	default:
		return time.Time{}, errExpiryNotGiven
	}
}

// foreverFlagName 指出使用者这次写的是规范名还是旧名。
//
// 报错时把实际写的那个名字引回来：写 --clear-expires-at 的人看到一句
// 「--forever 与 … 不能同时给」时，得先自己想起这两个是同一个旗标。
func foreverFlagName(seen map[string]bool) string {
	if flagGiven(seen, "forever") {
		return "--forever"
	}
	return "--clear-expires-at"
}

// optionalExpiry 收一条「可以不给」的过期时间：三个旗标都没给时 given 为 false。
//
// apikey create 用它：不设过期是合法的，而设了一个不合法的时间要报错。
// 「没给」用第二个返回值说出来，而不是拿 nil 指针 + nil 错误表示——后者是
// 两件事挤在一个值里，调用方漏判一次就成了「悄悄不设过期」。
func optionalExpiry(seen map[string]bool, f expiryFlags, now time.Time,
	parseAbsolute func(string) (time.Time, error),
) (time.Time, bool, error) {
	at, err := resolveExpiry(seen, f, now, parseAbsolute)
	if errors.Is(err, errExpiryNotGiven) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return at, true, nil
}

// ttlExpiry 把 --ttl 的相对时长换算成绝对时刻。
//
// 单位是 h（小时）、d（天）、m（个日历月）、y（个日历年）。基准是 now，
// 按 UTC 做日历运算：h 与 d 是精确时长（一天按 24 小时算），m 与 y 落到
// 目标月的同一天。
//
// 目标月没有那一天时夹到月末：1 月 31 日加一个月是 2 月 28 日（闰年 29 日），
// 2 月 29 日加一年是次年 2 月 28 日。这条规则钉在 TestTTLExpiry 里——换成
// 别的算法时，月末与闰日那几格必须红。
func ttlExpiry(now time.Time, ttl string) (time.Time, error) {
	amount, unit, err := parseTTL(ttl)
	if err != nil {
		return time.Time{}, err
	}

	base := now.UTC()
	var at time.Time
	switch unit {
	case 'h':
		at = base.Add(time.Duration(amount) * time.Hour)
	case 'd':
		at = base.AddDate(0, 0, amount)
	case 'm':
		at = addMonthsClamped(base, amount)
	case 'y':
		at = addMonthsClamped(base, amount*12)
	}
	if at.After(expiry.Forever()) {
		return time.Time{}, fmt.Errorf(
			"--ttl %s 换算出来的时刻 %s 超出了存储上限 %s",
			ttl, at.Format(time.RFC3339), expiry.ForeverRFC3339)
	}
	return at, nil
}

// maxTTLAmount 是单个单位的取值上限。它只防整数与日历运算溢出；真正的边界
// 是换算结果不能超过 expiry.ForeverRFC3339，那条在 ttlExpiry 里判。
const maxTTLAmount = 100000

// parseTTL 把 "30d" 这样的写法拆成正整数与单位。
func parseTTL(ttl string) (int, byte, error) {
	s := strings.TrimSpace(ttl)
	if s == "" {
		return 0, 0, errors.New("--ttl 不能为空：例如 12h、30d、3m、1y")
	}

	unit := s[len(s)-1]
	digits := s[:len(s)-1]
	if digits == "" {
		return 0, 0, fmt.Errorf("--ttl 要写成 <正整数><h|d|m|y>，例如 12h、30d、3m、1y，得到 %q", ttl)
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, 0, fmt.Errorf(
				"--ttl 要写成 <正整数><h|d|m|y>，例如 12h、30d、3m、1y，得到 %q", ttl)
		}
	}
	amount, err := strconv.Atoi(digits)
	if err != nil || amount <= 0 {
		return 0, 0, fmt.Errorf("--ttl 的数值要是正整数，得到 %q", ttl)
	}
	if amount > maxTTLAmount {
		return 0, 0, fmt.Errorf("--ttl 的数值 %d 过大（上限 %d）", amount, maxTTLAmount)
	}

	switch unit {
	case 'h', 'd', 'm', 'y':
		return amount, unit, nil
	default:
		return 0, 0, fmt.Errorf(
			"--ttl 的单位只能是 h（小时）、d（天）、m（月）、y（年），得到 %q", ttl)
	}
}

// addMonthsClamped 加若干个月历月，目标月没有那一天时夹到月末。
//
// 不直接用 time.AddDate：它在 1 月 31 日上会先溢出成 3 月 2 日或 3 日，而那
// 正是「一个月」最容易读错的一种落法。
func addMonthsClamped(t time.Time, months int) time.Time {
	year, month, day := t.Date()
	// 先落到本月的 1 号再加月，避免 AddDate 拿原日期去溢出。
	first := time.Date(year, month, 1, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), t.Location())
	target := first.AddDate(0, months, 0)
	if last := daysInMonth(target.Year(), target.Month()); day > last {
		day = last
	}
	return time.Date(target.Year(), target.Month(), day, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), t.Location())
}

// daysInMonth 返回某年某月的天数。下个月的第 0 天就是本月的最后一天。
func daysInMonth(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}
