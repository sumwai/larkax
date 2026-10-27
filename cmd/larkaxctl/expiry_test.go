package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/cmd/larkaxctl/internal/cli"
	"github.com/sumwai/larkax/internal/expiry"
)

// withExpiryNow 把换算用的「现在」换成固定时刻，用完还原。
//
// --ttl 的判据是「注入的现在 + 时长」逐秒相等；用例里再取一次 time.Now()
// 得到的是另一个值，那样比等于把同一个值抄两遍，钉不住任何东西。
func withExpiryNow(t *testing.T, at time.Time) {
	t.Helper()
	saved := cli.ExpiryNow
	cli.ExpiryNow = func() time.Time { return at }
	t.Cleanup(func() { cli.ExpiryNow = saved })
}

// TestTTLExpiry 钉住相对时长到绝对时刻的换算，重点在月末与闰年。
//
// 期望值写成字面量：拿 addMonthsClamped 自己算出来再比，等于用同一个实现
// 证明它自己。
func TestTTLExpiry(t *testing.T) {
	cases := []struct {
		name string
		now  time.Time
		ttl  string
		want time.Time
	}{
		{"12h", time.Date(2026, 10, 23, 8, 30, 15, 0, time.UTC), "12h",
			time.Date(2026, 10, 23, 20, 30, 15, 0, time.UTC)},
		{"30d", time.Date(2026, 1, 31, 12, 0, 0, 0, time.UTC), "30d",
			time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)},
		{"1d 跨月", time.Date(2026, 1, 31, 12, 0, 0, 0, time.UTC), "1d",
			time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)},
		{"1m 从 31 日出发夹到月末", time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC), "1m",
			time.Date(2026, 2, 28, 0, 0, 0, 0, time.UTC)},
		{"1m 从闰年 31 日出发夹到 29 日", time.Date(2024, 1, 31, 0, 0, 0, 0, time.UTC), "1m",
			time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC)},
		{"1m 不改有这一天的月份", time.Date(2026, 3, 15, 6, 0, 0, 0, time.UTC), "1m",
			time.Date(2026, 4, 15, 6, 0, 0, 0, time.UTC)},
		{"1m 跨年", time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC), "1m",
			time.Date(2027, 1, 31, 0, 0, 0, 0, time.UTC)},
		{"1y 从闰日出发夹到 28 日", time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC), "1y",
			time.Date(2025, 2, 28, 0, 0, 0, 0, time.UTC)},
		{"1y 从平年 2 月 28 日出发仍是 28 日", time.Date(2023, 2, 28, 0, 0, 0, 0, time.UTC), "1y",
			time.Date(2024, 2, 28, 0, 0, 0, 0, time.UTC)},
		{"2y 从闰日出发夹到 28 日", time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC), "2y",
			time.Date(2026, 2, 28, 0, 0, 0, 0, time.UTC)},
		{"13m 跨年且落回 29 日", time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC), "13m",
			time.Date(2025, 3, 29, 0, 0, 0, 0, time.UTC)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := cli.TTLExpiry(tc.now, tc.ttl)
			if err != nil {
				t.Fatalf("换算失败: %v", err)
			}
			if !got.Equal(tc.want) {
				t.Errorf("ttlExpiry(%s, %q) = %s，期望 %s",
					tc.now.Format(time.RFC3339), tc.ttl,
					got.Format(time.RFC3339), tc.want.Format(time.RFC3339))
			}
		})
	}
}

// TestTTLExpiryUsesUTC 钉住时区口径：进程时区不是 UTC 时，日历运算仍按 UTC。
func TestTTLExpiryUsesUTC(t *testing.T) {
	shanghai := time.FixedZone("UTC+8", 8*60*60)
	now := time.Date(2026, 1, 31, 8, 0, 0, 0, shanghai) // 2026-01-31T00:00:00Z
	got, err := cli.TTLExpiry(now, "1m")
	if err != nil {
		t.Fatalf("换算失败: %v", err)
	}
	want := time.Date(2026, 2, 28, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("得到 %s，期望 %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	if got.Location() != time.UTC {
		t.Errorf("换算结果的时区是 %v，期望 UTC", got.Location())
	}
}

// TestTTLExpiryRejectsBadInput 钉住写法判据。
func TestTTLExpiryRejectsBadInput(t *testing.T) {
	cases := []struct {
		ttl  string
		want string
	}{
		{"", "不能为空"},
		{"30", "单位"},
		{"d", "正整数"},
		{"0d", "正整数"},
		{"-1d", "正整数"},
		{"1.5d", "正整数"},
		{" 30w ", "单位"},
		{"30D", "单位"},
		{"100001d", "过大"},
	}
	for _, tc := range cases {
		t.Run(tc.ttl, func(t *testing.T) {
			_, err := cli.TTLExpiry(time.Now(), tc.ttl)
			if err == nil {
				t.Fatal("期望报错，却通过了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误 = %q，里面没有 %q", err.Error(), tc.want)
			}
		})
	}
}

// TestTTLExpiryRejectsBeyondStorage 钉住远期上限：换算结果超过 9999 年时
// 在参数这一层报错，而不是把一句数据库的 out of range 丢给使用者。
func TestTTLExpiryRejectsBeyondStorage(t *testing.T) {
	_, err := cli.TTLExpiry(time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), "1y")
	if err == nil {
		t.Fatal("期望报错，却通过了")
	}
	if !strings.Contains(err.Error(), expiry.ForeverRFC3339) {
		t.Errorf("错误 = %q，里面没有上限 %s", err.Error(), expiry.ForeverRFC3339)
	}
}

// TestResolveExpiry 钉住三种写法的取舍与互斥。
func TestResolveExpiry(t *testing.T) {
	now := time.Date(2026, 1, 31, 12, 0, 0, 0, time.UTC)
	absolute := func(v string) (time.Time, error) {
		return time.Parse(time.RFC3339, v)
	}

	t.Run("没给任何一个", func(t *testing.T) {
		_, err := cli.ResolveExpiry(map[string]bool{}, cli.ExpiryFlags{}, now, absolute)
		if !errors.Is(err, cli.ErrExpiryNotGiven) {
			t.Fatalf("错误 = %v，期望 errExpiryNotGiven", err)
		}
	})

	t.Run("expires-at", func(t *testing.T) {
		want := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
		got, err := cli.ResolveExpiry(map[string]bool{"expires-at": true},
			cli.ExpiryFlags{ExpiresAt: "2030-01-02T03:04:05Z"}, now, absolute)
		if err != nil {
			t.Fatalf("出错: %v", err)
		}
		if !got.Equal(want) {
			t.Errorf("得到 %s，期望 %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
		}
	})

	t.Run("ttl 用注入的现在", func(t *testing.T) {
		got, err := cli.ResolveExpiry(map[string]bool{"ttl": true},
			cli.ExpiryFlags{TTL: "30d"}, now, absolute)
		if err != nil {
			t.Fatalf("出错: %v", err)
		}
		want := now.AddDate(0, 0, 30)
		if !got.Equal(want) {
			t.Errorf("得到 %s，期望 %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
		}
	})

	t.Run("forever", func(t *testing.T) {
		got, err := cli.ResolveExpiry(map[string]bool{"forever": true},
			cli.ExpiryFlags{Forever: true}, now, absolute)
		if err != nil {
			t.Fatalf("出错: %v", err)
		}
		if !got.Equal(expiry.Forever()) {
			t.Errorf("得到 %s，期望 %s", got.Format(time.RFC3339), expiry.ForeverRFC3339)
		}
	})

	t.Run("旧名 clear-expires-at 与 forever 同义", func(t *testing.T) {
		got, err := cli.ResolveExpiry(map[string]bool{"clear-expires-at": true},
			cli.ExpiryFlags{Forever: true}, now, absolute)
		if err != nil {
			t.Fatalf("出错: %v", err)
		}
		if !got.Equal(expiry.Forever()) {
			t.Errorf("得到 %s，期望 %s", got.Format(time.RFC3339), expiry.ForeverRFC3339)
		}
	})

	conflicts := []struct {
		name string
		seen map[string]bool
		f    cli.ExpiryFlags
	}{
		{"expires-at 与 ttl", map[string]bool{"expires-at": true, "ttl": true},
			cli.ExpiryFlags{ExpiresAt: "2030-01-02T03:04:05Z", TTL: "30d"}},
		{"expires-at 与 forever", map[string]bool{"expires-at": true, "forever": true},
			cli.ExpiryFlags{ExpiresAt: "2030-01-02T03:04:05Z", Forever: true}},
		{"ttl 与 forever", map[string]bool{"ttl": true, "forever": true},
			cli.ExpiryFlags{TTL: "30d", Forever: true}},
		{"旧名 clear-expires-at 与 expires-at", map[string]bool{"expires-at": true, "clear-expires-at": true},
			cli.ExpiryFlags{ExpiresAt: "2030-01-02T03:04:05Z", Forever: true}},
	}
	for _, tc := range conflicts {
		t.Run(tc.name, func(t *testing.T) {
			_, err := cli.ResolveExpiry(tc.seen, tc.f, now, absolute)
			if err == nil {
				t.Fatal("期望报错，却通过了")
			}
			if !strings.Contains(err.Error(), "不能同时给") {
				t.Errorf("错误 = %q，里面没有「不能同时给」", err.Error())
			}
		})
	}
}
