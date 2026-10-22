package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/auth"
	"github.com/sumwai/larkax/internal/expiry"
	"github.com/sumwai/larkax/internal/money"
)

// TestAPIKeyCreateRejectsBadFlags 走的是**不碰数据库**的那几条路径。
//
// 参数检查必须发生在连库之前：一个写错的命令不该先连上生产库再报错，
// 而「连上库之后才发现参数不对」还有一个更坏的可能——它没报错。
func TestAPIKeyCreateRejectsBadFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"没有 --user", []string{}, "缺少 --user"},
		{"--user 为空", []string{"--user", ""}, "缺少 --user"},
		{"多出位置参数", []string{"--user", "1", "extra"}, "不接受位置参数"},
		{"--expires-at 不是时间", []string{"--user", "1", "--expires-at", "明天"}, "RFC3339"},
		{"--expires-at 在过去", []string{"--user", "1", "--expires-at", "2020-01-01T00:00:00Z"}, "已经过去"},
		{"--expires-at 与 --ttl 同时给",
			[]string{"--user", "1", "--expires-at", "2030-01-01T00:00:00Z", "--ttl", "30d"},
			"不能同时给"},
		{"--ttl 与 --forever 同时给",
			[]string{"--user", "1", "--ttl", "30d", "--forever"}, "不能同时给"},
		{"--ttl 单位不认识", []string{"--user", "1", "--ttl", "30w"}, "单位"},

		{"--rate-limit-rpm 不是整数", []string{"--user", "1", "--rate-limit-rpm", "六十"}, "需要整数"},
		{"--rate-limit-rpm 是小数", []string{"--user", "1", "--rate-limit-rpm", "6.5"}, "需要整数"},
		{"--rate-limit-rpm 是负数", []string{"--user", "1", "--rate-limit-rpm", "-60"}, "不能是负数"},
		{"--rate-limit-rpm 超出 32 位", []string{"--user", "1", "--rate-limit-rpm", "3000000000"}, "32 位整数"},
		{"--rate-limit-rpm 给了空值", []string{"--user", "1", "--rate-limit-rpm", ""}, "得到空值"},

		{"--quota-limit 不是金额", []string{"--user", "1", "--quota-limit", "很多"}, "不是金额"},
		{"--quota-limit 是负数", []string{"--user", "1", "--quota-limit", "-1"}, "不能是负数"},
		{"--quota-limit 小数位过多", []string{"--user", "1", "--quota-limit", "1.123456789"}, "最多 8 位小数"},
		{"--quota-limit 给了空值", []string{"--user", "1", "--quota-limit", ""}, "得到空值"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runAPIKeyCreate(tc.args)
			if err == nil {
				t.Fatalf("期望报错，却通过了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息 %q 里没有 %q", err.Error(), tc.want)
			}
		})
	}
}

func TestAPIKeyUnknownSubcommand(t *testing.T) {
	err := runAPIKey([]string{})
	if err == nil || !strings.Contains(err.Error(), "需要子命令") {
		t.Fatalf("无子命令时应报错，得到 %v", err)
	}

	err = runAPIKey([]string{"bogus"})
	if err == nil || !strings.Contains(err.Error(), "未知的 apikey 子命令") {
		t.Fatalf("未知子命令时应报错，得到 %v", err)
	}
}

// TestUpdateParamsThreeStates 钉住额度那两项的三态。
//
// 这三态里最不能弄错的是**清空与设成 0**：前者是不限，后者是一个都不放行。
// 把清空实现成「写成 0」，表现是每一次「恢复默认」都变成一次静默的封禁，
// 而它不报任何错——所以这里逐个断言 Set* 与指针的组合，而不只是「没报错」。
func TestUpdateParamsThreeStates(t *testing.T) {
	rpmFlag := map[string]bool{"rate-limit-rpm": true}
	quotaFlag := map[string]bool{"quota-limit": true}

	t.Run("不给旗标就是不改", func(t *testing.T) {
		_, err := updateParams(map[string]bool{}, limitFlag{}, limitFlag{}, metaFlags{})
		if err == nil || !strings.Contains(err.Error(), "没有要改的东西") {
			t.Fatalf("期望「没有要改的东西」，得到 %v", err)
		}
	})

	t.Run("清空 rpm 是 Set 为真且指针为 nil", func(t *testing.T) {
		params, err := updateParams(map[string]bool{}, limitFlag{clear: true}, limitFlag{}, metaFlags{})
		if err != nil {
			t.Fatalf("出错: %v", err)
		}
		if !params.SetRateLimit || params.RateLimitRPM != nil {
			t.Fatalf("得到 SetRateLimit=%v RateLimitRPM=%s，期望 true 与 nil",
				params.SetRateLimit, showPtr(params.RateLimitRPM))
		}
		if params.SetQuota {
			t.Errorf("没要求改的 quota 被标成了要改")
		}
	})

	t.Run("rpm 给 0 是 Set 为真且指针指向 0", func(t *testing.T) {
		params, err := updateParams(rpmFlag, limitFlag{value: "0"}, limitFlag{}, metaFlags{})
		if err != nil {
			t.Fatalf("出错: %v", err)
		}
		if !params.SetRateLimit || params.RateLimitRPM == nil || *params.RateLimitRPM != 0 {
			t.Fatalf("得到 SetRateLimit=%v RateLimitRPM=%s，期望 true 与指向 0 的指针",
				params.SetRateLimit, showPtr(params.RateLimitRPM))
		}
	})

	t.Run("rpm 给 60", func(t *testing.T) {
		params, err := updateParams(rpmFlag, limitFlag{value: "60"}, limitFlag{}, metaFlags{})
		if err != nil {
			t.Fatalf("出错: %v", err)
		}
		if params.RateLimitRPM == nil || *params.RateLimitRPM != 60 {
			t.Fatalf("RateLimitRPM = %s，期望 60", showPtr(params.RateLimitRPM))
		}
	})

	t.Run("清空 quota 是 Set 为真且指针为 nil", func(t *testing.T) {
		params, err := updateParams(map[string]bool{}, limitFlag{}, limitFlag{clear: true}, metaFlags{})
		if err != nil {
			t.Fatalf("出错: %v", err)
		}
		if !params.SetQuota || params.QuotaLimit != nil {
			t.Fatalf("得到 SetQuota=%v QuotaLimit=%v，期望 true 与 nil",
				params.SetQuota, params.QuotaLimit)
		}
		if params.SetRateLimit {
			t.Errorf("没要求改的 rpm 被标成了要改")
		}
	})

	t.Run("quota 给 0 是 Set 为真且指针指向 0", func(t *testing.T) {
		params, err := updateParams(quotaFlag, limitFlag{}, limitFlag{value: "0"}, metaFlags{})
		if err != nil {
			t.Fatalf("出错: %v", err)
		}
		if !params.SetQuota || params.QuotaLimit == nil || !params.QuotaLimit.IsZero() {
			t.Fatalf("得到 SetQuota=%v QuotaLimit=%v，期望 true 与零金额",
				params.SetQuota, params.QuotaLimit)
		}
	})

	t.Run("quota 给具体金额", func(t *testing.T) {
		params, err := updateParams(quotaFlag, limitFlag{}, limitFlag{value: "12.34"}, metaFlags{})
		if err != nil {
			t.Fatalf("出错: %v", err)
		}
		if params.QuotaLimit == nil || params.QuotaLimit.String() != "12.34000000" {
			t.Fatalf("QuotaLimit = %v，期望 12.34000000", params.QuotaLimit)
		}
	})

	t.Run("两项一起改", func(t *testing.T) {
		seen := map[string]bool{"rate-limit-rpm": true, "quota-limit": true}
		params, err := updateParams(seen, limitFlag{value: "60"}, limitFlag{value: "5"}, metaFlags{})
		if err != nil {
			t.Fatalf("出错: %v", err)
		}
		if !params.SetRateLimit || !params.SetQuota {
			t.Fatalf("两项都该被标成要改，得到 %+v", params)
		}
	})

	// --clear-rate-limit-rpm=false 是「不要清空」，与不给它是同一件事，
	// 而与给它（true）是两件事。只看旗标出现过没有就会把它读反。
	t.Run("clear 旗标显式给 false 等于没给", func(t *testing.T) {
		_, err := updateParams(map[string]bool{}, limitFlag{}, limitFlag{}, metaFlags{})
		if err == nil {
			t.Fatal("clear=false 不该被当成一次修改")
		}
	})

	// ── 状态 / 备注 / 过期那三项 ──

	t.Run("status 取合法值", func(t *testing.T) {
		params, err := updateParams(map[string]bool{"status": true}, limitFlag{}, limitFlag{},
			metaFlags{status: " disabled "})
		if err != nil {
			t.Fatalf("出错: %v", err)
		}
		if !params.SetStatus || params.Status != "disabled" {
			t.Fatalf("得到 SetStatus=%v Status=%q，期望 true 与 disabled（首尾空白去掉）",
				params.SetStatus, params.Status)
		}
	})

	t.Run("status 取不认识的值", func(t *testing.T) {
		_, err := updateParams(map[string]bool{"status": true}, limitFlag{}, limitFlag{},
			metaFlags{status: "suspended"})
		if err == nil {
			t.Fatal("期望报错")
		}
		// 三个合法取值都要列出来：只报「不认识」的话，读的人还得去翻文档。
		for _, want := range []string{"active", "disabled", "revoked"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("错误信息 %q 里没有合法取值 %q", err.Error(), want)
			}
		}
	})

	t.Run("name 给空串是清空备注，不是没给", func(t *testing.T) {
		params, err := updateParams(map[string]bool{"name": true}, limitFlag{}, limitFlag{},
			metaFlags{name: ""})
		if err != nil {
			t.Fatalf("出错: %v", err)
		}
		if !params.SetName || params.Name != "" {
			t.Fatalf("得到 SetName=%v Name=%q，期望 true 与空串", params.SetName, params.Name)
		}
	})

	t.Run("expires-at 给将来的时刻", func(t *testing.T) {
		params, err := updateParams(map[string]bool{"expires-at": true}, limitFlag{}, limitFlag{},
			metaFlags{expiresAt: "2030-01-02T03:04:05Z"})
		if err != nil {
			t.Fatalf("出错: %v", err)
		}
		want := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
		if !params.SetExpiresAt || params.ExpiresAt == nil || !params.ExpiresAt.Equal(want) {
			t.Fatalf("得到 SetExpiresAt=%v ExpiresAt=%s，期望 true 与 %s",
				params.SetExpiresAt, showPtr(params.ExpiresAt), want)
		}
	})

	t.Run("forever 是 Set 为真且指针指向远期上限", func(t *testing.T) {
		params, err := updateParams(map[string]bool{}, limitFlag{}, limitFlag{},
			metaFlags{forever: true})
		if err != nil {
			t.Fatalf("出错: %v", err)
		}
		if !params.SetExpiresAt || params.ExpiresAt == nil {
			t.Fatalf("得到 SetExpiresAt=%v ExpiresAt=%s，期望 true 与远期上限",
				params.SetExpiresAt, showPtr(params.ExpiresAt))
		}
		if !params.ExpiresAt.Equal(expiry.Forever()) {
			t.Errorf("ExpiresAt = %s，期望 %s", params.ExpiresAt.Format(time.RFC3339), expiry.ForeverRFC3339)
		}
	})

	t.Run("ttl 按注入的现在换算", func(t *testing.T) {
		now := time.Date(2026, 1, 31, 12, 0, 0, 0, time.UTC)
		withExpiryNow(t, now)
		params, err := updateParams(map[string]bool{"ttl": true}, limitFlag{}, limitFlag{},
			metaFlags{ttl: "1m"})
		if err != nil {
			t.Fatalf("出错: %v", err)
		}
		want := time.Date(2026, 2, 28, 12, 0, 0, 0, time.UTC)
		if !params.SetExpiresAt || params.ExpiresAt == nil || !params.ExpiresAt.Equal(want) {
			t.Fatalf("得到 SetExpiresAt=%v ExpiresAt=%s，期望 true 与 %s",
				params.SetExpiresAt, showPtr(params.ExpiresAt), want.Format(time.RFC3339))
		}
	})

	t.Run("三项一起改", func(t *testing.T) {
		seen := map[string]bool{"status": true, "name": true, "expires-at": true}
		params, err := updateParams(seen, limitFlag{}, limitFlag{}, metaFlags{
			status: "revoked", name: "吊销了", expiresAt: "2030-01-02T03:04:05Z",
		})
		if err != nil {
			t.Fatalf("出错: %v", err)
		}
		if !params.SetStatus || !params.SetName || !params.SetExpiresAt {
			t.Fatalf("三项都该被标成要改，得到 %+v", params)
		}
		if params.SetRateLimit || params.SetQuota {
			t.Errorf("没给的两项额度被标成了要改：%+v", params)
		}
	})
}

func TestUpdateParamsRejectsConflictingAndBadValues(t *testing.T) {
	cases := []struct {
		name       string
		seen       map[string]bool
		rpm        string
		clearRPM   bool
		quota      string
		clearQuota bool
		meta       metaFlags
		want       string
	}{
		{"rpm 设值与清空同时给",
			map[string]bool{"rate-limit-rpm": true}, "60", true, "", false, metaFlags{},
			"不能同时给"},
		{"quota 设值与清空同时给",
			map[string]bool{"quota-limit": true}, "", false, "5", true, metaFlags{},
			"不能同时给"},
		{"rpm 是负数", map[string]bool{"rate-limit-rpm": true}, "-1", false, "", false, metaFlags{},
			"不能是负数"},
		{"rpm 不是整数", map[string]bool{"rate-limit-rpm": true}, "六十", false, "", false, metaFlags{},
			"需要整数"},
		{"quota 小数位过多", map[string]bool{"quota-limit": true}, "", false, "1.123456789", false,
			metaFlags{}, "最多 8 位小数"},
		{"quota 是负数", map[string]bool{"quota-limit": true}, "", false, "-0.5", false, metaFlags{},
			"不能是负数"},

		// 元信息那三项：过期与状态各有自己的冲突与取值判据。
		{"expires-at 设值与清空同时给",
			map[string]bool{"expires-at": true}, "", false, "", false,
			metaFlags{expiresAt: "2030-01-02T03:04:05Z", forever: true},
			"不能同时给"},
		{"expires-at 不是 RFC3339",
			map[string]bool{"expires-at": true}, "", false, "", false,
			metaFlags{expiresAt: "2030-01-02"}, "RFC3339"},
		{"expires-at 与 ttl 同时给",
			map[string]bool{"expires-at": true, "ttl": true}, "", false, "", false,
			metaFlags{expiresAt: "2030-01-02T03:04:05Z", ttl: "30d"},
			"不能同时给"},
		{"ttl 与 forever 同时给",
			map[string]bool{"ttl": true}, "", false, "", false,
			metaFlags{ttl: "30d", forever: true}, "不能同时给"},
		{"ttl 的单位不认识",
			map[string]bool{"ttl": true}, "", false, "", false,
			metaFlags{ttl: "30w"}, "单位"},
		{"expires-at 在过去",
			map[string]bool{"expires-at": true}, "", false, "", false,
			metaFlags{expiresAt: "2020-01-01T00:00:00Z"}, "已经过去"},
		{"status 取值不认识",
			map[string]bool{"status": true}, "", false, "", false,
			metaFlags{status: "suspended"}, "--status 取"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := updateParams(tc.seen,
				limitFlag{value: tc.rpm, clear: tc.clearRPM},
				limitFlag{value: tc.quota, clear: tc.clearQuota},
				tc.meta)
			if err == nil {
				t.Fatal("期望报错，却通过了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息 %q 里没有 %q", err.Error(), tc.want)
			}
		})
	}
}

// update 的那几条不碰数据库的拒绝路径。
func TestAPIKeyUpdateRejectsBadFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"没有 --id", []string{"--rate-limit-rpm", "60"}, "缺少 --id"},
		{"--id 是 0", []string{"--id", "0", "--rate-limit-rpm", "60"}, "缺少 --id"},
		{"什么都没要求改", []string{"--id", "3"}, "没有要改的东西"},
		{"多出位置参数", []string{"--id", "3", "extra"}, "不接受位置参数"},
		{"rpm 设值与清空同时给",
			[]string{"--id", "3", "--rate-limit-rpm", "60", "--clear-rate-limit-rpm"},
			"不能同时给"},
		{"quota 设值与清空同时给",
			[]string{"--id", "3", "--quota-limit", "5", "--clear-quota-limit"},
			"不能同时给"},
		{"rpm 是负数", []string{"--id", "3", "--rate-limit-rpm", "-1"}, "不能是负数"},
		{"quota 小数位过多", []string{"--id", "3", "--quota-limit", "1.123456789"}, "最多 8 位小数"},

		// 状态 / 备注 / 过期那三项：这三条路径的错误都发生在连库之前，
		// 所以它们能在不打库的情况下被钉住——而它们正是最常被敲错的地方。
		{"--status 取值不认识",
			[]string{"--id", "3", "--status", "suspended"}, "--status 取"},
		{"--status 给了空值", []string{"--id", "3", "--status", ""}, "--status 取"},
		{"--expires-at 不是时间",
			[]string{"--id", "3", "--expires-at", "明天"}, "RFC3339"},
		{"--expires-at 在过去",
			[]string{"--id", "3", "--expires-at", "2020-01-01T00:00:00Z"}, "已经过去"},
		{"--expires-at 与 --clear-expires-at 同时给",
			[]string{"--id", "3", "--expires-at", "2030-01-01T00:00:00Z", "--clear-expires-at"},
			"不能同时给"},
		{"--expires-at 与 --ttl 同时给",
			[]string{"--id", "3", "--expires-at", "2030-01-01T00:00:00Z", "--ttl", "30d"},
			"不能同时给"},
		{"--ttl 与 --forever 同时给",
			[]string{"--id", "3", "--ttl", "30d", "--forever"}, "不能同时给"},
		{"--ttl 数值不是正整数", []string{"--id", "3", "--ttl", "-1d"}, "正整数"},
		{"--ttl 没写单位", []string{"--id", "3", "--ttl", "30"}, "单位"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runAPIKeyUpdate(tc.args)
			if err == nil {
				t.Fatalf("期望报错，却通过了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息 %q 里没有 %q", err.Error(), tc.want)
			}
		})
	}
}

// apikey get 的那几条不碰数据库的拒绝路径。
//
// 与 update 一样，判据是「参数错要在连库之前报出来」：一条 id 都没给的命令
// 不该先连上生产库再说话。--id 0 与负数走的是同一条：id 是 bigserial，
// 它们不可能有一行，放进去也只会得到一句「找不到」。
func TestAPIKeyGetRejectsBadFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"没有 --id", []string{}, "缺少 --id"},
		{"--id 是 0", []string{"--id", "0"}, "缺少 --id"},
		{"--id 是负数", []string{"--id", "-2"}, "缺少 --id"},
		{"位置参数多余", []string{"--id", "3", "extra"}, "不接受位置参数"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runAPIKeyGet(tc.args)
			if err == nil {
				t.Fatalf("期望报错，却通过了")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息 %q 里没有 %q", err.Error(), tc.want)
			}
		})
	}
}

func TestAPIKeyListRejectsPositional(t *testing.T) {
	err := runAPIKeyList([]string{"alice"})
	if err == nil || !strings.Contains(err.Error(), "不接受位置参数") {
		t.Fatalf("期望报错，得到 %v", err)
	}
}

// TestPrintCreatedKeySplitsStreams 钉住输出分流这件事。
//
// `larkaxctl apikey create ... > key.txt` 是保存一把 key 最自然的动作。
// 元信息一旦混进 stdout，那个文件就没法被程序直接读走——粘贴到客户端配置里
// 多一行就少一个能用的 key。所以「stdout 只有明文」是一条契约，不是排版偏好。
func TestPrintCreatedKeySplitsStreams(t *testing.T) {
	var stdout, stderr bytes.Buffer
	expires := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	rpm := 60
	quota := money.MustParse("12.34")

	printCreatedKey(&stdout, &stderr,
		auth.User{ID: 7, Username: "user-placeholder"},
		auth.Created{ID: 3, Prefix: "larkax_abcd", Plaintext: "larkax_abcd_plaintext"},
		&expires,
		keyLimits{rpm: &rpm, quota: &quota},
	)

	if got, want := stdout.String(), "larkax_abcd_plaintext\n"; got != want {
		t.Errorf("stdout = %q，期望只有明文加一个换行 %q", got, want)
	}

	// 提示语必须在：明文只返回一次，不说明的话它就是一次静默的数据丢失。
	// 额度也要回显：它是这次创建里唯一看不见的东西——明文能看见，
	// 而写进去的额度不打印就只能去查库。
	for _, want := range []string{
		"明文只打印这一次", "larkax_abcd", "2030-01-02T03:04:05Z", "user-placeholder",
		"每分钟=60", "额度=12.34000000",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr 里没有 %q，实际内容：\n%s", want, stderr.String())
		}
	}
}

// 没设额度的 key 在回显里要写成「继承」，而不是留空或 0。
//
// 留空与「这一栏不存在」长得一样，0 更糟——它是有含义的取值（一个都不放行）。
func TestPrintCreatedKeyShowsInheritedLimits(t *testing.T) {
	var stdout, stderr bytes.Buffer

	printCreatedKey(&stdout, &stderr,
		auth.User{ID: 7, Username: "user-placeholder"},
		auth.Created{ID: 3, Prefix: "larkax_abcd", Plaintext: "larkax_abcd_plaintext"},
		nil, keyLimits{},
	)

	for _, want := range []string{"过期=不过期", "每分钟=继承", "额度=继承"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr 里没有 %q，实际内容：\n%s", want, stderr.String())
		}
	}
}

// TestPrintKeyList 钉住每一栏的取值，尤其是两列额度的 NULL 与「还没用过」。
//
// 三个时刻各给一个**互不相同**的值：断言写成整段比对之后，把某一栏接成另一个
// 时刻（例如把「创建」打成 last_used_at）会当场挂掉。只断言「行里出现了一个
// 日期」是分辨不出来的——那正是这一栏最容易出的错。
//
// 表头也算一行：清单是指着列读的，列错位比取值错更难发现。
func TestPrintKeyList(t *testing.T) {
	rpm := 60
	quota := money.MustParse("12.34")
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	expires := time.Date(2027, 6, 7, 8, 9, 10, 0, time.UTC)
	used := time.Date(2026, 10, 17, 6, 10, 0, 0, time.UTC)

	var buf bytes.Buffer
	printKeyList(&buf, []auth.Key{
		{
			ID: 3, UserID: 7, Username: "user-placeholder", Name: "限额的",
			Prefix: "larkax_abcd", Status: "active",
			RateLimitRPM: &rpm, QuotaLimit: &quota,
			CreatedAt: created, ExpiresAt: &expires, LastUsedAt: &used,
		},
		{
			ID: 4, UserID: 7, Username: "user-placeholder",
			Prefix: "larkax_efgh", Status: "revoked",
			CreatedAt: created,
		},
	})

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("打印了 %d 行（含表头），期望 3 行：\n%s", len(lines), buf.String())
	}

	header := lines[0]
	for _, want := range []string{"ID", "KEY 前缀", "所属用户", "备注", "状态",
		"每分钟", "额度", "创建", "过期", "最近使用"} {
		if !strings.Contains(header, want) {
			t.Errorf("表头里没有 %q：\n%s", want, header)
		}
	}

	first := lines[1]
	for _, want := range []string{
		"3", "larkax_abcd", "user-placeholder", "限额的",
		"active", "60", "12.34000000",
		// 三个时刻截到分钟，且三个值互不相同：接错栏会当场挂。
		"01-02 03:04", "06-07 08:09", "10-17 06:10",
	} {
		if !strings.Contains(first, want) {
			t.Errorf("第一行里没有 %q：\n%s", want, first)
		}
	}
	// id 单独一列，所以用户那一栏不再附 `(id: 7)`：重复一遍会把这一栏撑宽一倍。
	if strings.Contains(first, "(id:") || strings.Contains(first, "id=7") {
		t.Errorf("用户那一栏不该再附 id：\n%s", first)
	}
	// 整条 RFC3339 不该出现在清单里：那是这一栏被撑宽的来源。
	if strings.Contains(first, "T03:04:05Z") || strings.Contains(first, "+00:00") {
		t.Errorf("时刻该截到分钟，不该是 RFC3339：\n%s", first)
	}

	second := lines[2]
	for _, want := range []string{
		"4", "larkax_efgh", "-", "revoked", "继承", "继承",
		"01-02 03:04", "不过期", "从未",
	} {
		if !strings.Contains(second, want) {
			t.Errorf("第二行里没有 %q：\n%s", want, second)
		}
	}

	// 一行一把、栏位对齐：表头与两行的同一栏必须从同一**显示列**开始。
	// 用 table_test.go 里那套折算（不复用被测的 displayWidth，见那里的说明）。
	wantStarts := columnStarts(t, lines[0], []string{
		"ID", "KEY 前缀", "所属用户", "备注", "状态", "每分钟", "额度", "创建", "过期"})
	for i, row := range []struct {
		cells []string
	}{
		{[]string{"3", "larkax_abcd", "user-placeholder", "限额的", "active",
			"60", "12.34000000", "01-02 03:04", "06-07 08:09"}},
		{[]string{"4", "larkax_efgh", "user-placeholder", "-", "revoked",
			"继承", "继承", "01-02 03:04", "不过期"}},
	} {
		line := lines[i+1]
		if got := columnStarts(t, line, row.cells); !slices.Equal(got, wantStarts) {
			t.Errorf("第 %d 行的栏起点 = %v，表头是 %v：\n%s", i+1, got, wantStarts, line)
		}
	}
}

func TestPrintKeyListEmpty(t *testing.T) {
	var buf bytes.Buffer
	printKeyList(&buf, nil)
	if got, want := buf.String(), "暂无 API Key。可使用 larkaxctl apikey create 添加。\n"; got != want {
		t.Errorf("空清单 = %q，期望 %q", got, want)
	}
}

// printUpdatedKey 要能区分「清空」与「设成 0」——两者的输出不能一样，
// 否则回显就失去了它的用处（确认自己做的正是想做的那一件事）。
func TestPrintUpdatedKeyDistinguishesClearFromZero(t *testing.T) {
	rpm := 60
	zero := 0

	var cleared bytes.Buffer
	printUpdatedKey(&cleared,
		auth.Key{ID: 3, RateLimitRPM: &rpm},
		auth.Key{ID: 3},
		auth.KeyParams{SetRateLimit: true})

	var setToZero bytes.Buffer
	printUpdatedKey(&setToZero,
		auth.Key{ID: 3},
		auth.Key{ID: 3, RateLimitRPM: &zero},
		auth.KeyParams{SetRateLimit: true, RateLimitRPM: &zero})

	if got, want := cleared.String(), "已改 API Key id=3：每分钟 从 60 改为 继承\n"; got != want {
		t.Errorf("清空 = %q，期望 %q", got, want)
	}
	if got, want := setToZero.String(), "已改 API Key id=3：每分钟 从 继承 改为 0\n"; got != want {
		t.Errorf("设成 0 = %q，期望 %q", got, want)
	}
	if cleared.String() == setToZero.String() {
		t.Error("清空与设成 0 打出来的东西一样")
	}
}

// 一次改多项时，回显要**逐项**说出从什么变成什么，且只列命令行上给出的那几项。
//
// 打的是「给出过的那几项」而不是「两次读出来不一样的那几项」：给出一个与库里
// 相同的值时后者什么都不打，读的人分不清「没生效」与「本来就是它」——而这正
// 是回显存在的理由。
func TestPrintUpdatedKeyListsEveryGivenField(t *testing.T) {
	rpm := 60
	quota := money.MustParse("12.34")
	expires := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)

	before := auth.Key{
		ID: 3, Status: "active", Name: "旧备注",
		RateLimitRPM: &rpm, QuotaLimit: &quota, ExpiresAt: &expires,
	}
	// 改完之后：状态与备注变了，过期被清空，两列额度原样不动。
	after := auth.Key{
		ID: 3, Status: "revoked", Name: "新备注",
		RateLimitRPM: &rpm, QuotaLimit: &quota,
	}

	var buf bytes.Buffer
	printUpdatedKey(&buf, before, after, auth.KeyParams{
		SetStatus: true, Status: "revoked",
		SetName:      true,
		Name:         "新备注",
		SetExpiresAt: true,
	})

	want := "已改 API Key id=3：状态 从 active 改为 revoked；备注 从 旧备注 改为 新备注；" +
		"过期 从 2030-01-02T03:04:05Z 改为 不过期\n"
	if got := buf.String(); got != want {
		t.Errorf("回显 = %q，期望 %q", got, want)
	}
	// 没给的两项不许出现：打出来会让人以为这次命令也动过额度。
	for _, unwanted := range []string{"每分钟", "额度"} {
		if strings.Contains(buf.String(), unwanted) {
			t.Errorf("回显里出现了没要求改的 %q：%s", unwanted, buf.String())
		}
	}
}

// 备注被清空要打成 `-`，而不是留一个空档。
//
// 空档与「这一栏不存在」长得一样，而这两种情形在读回显的人那里是两件事。
func TestPrintUpdatedKeyShowsClearedName(t *testing.T) {
	var buf bytes.Buffer
	printUpdatedKey(&buf,
		auth.Key{ID: 3, Name: "旧备注"},
		auth.Key{ID: 3},
		auth.KeyParams{SetName: true})

	if got, want := buf.String(), "已改 API Key id=3：备注 从 旧备注 改为 -\n"; got != want {
		t.Errorf("回显 = %q，期望 %q", got, want)
	}
}

// ── 机器可读输出（--format json） ───────────────────────────────────────
//
// 每条接上的命令一条用例，判据一律是「编一遍、再解回来，按字段断言」，而不是
// 比对整段文本：字段一多，整段比对会因为一个空格而红，而它证明不了脚本读得到
// 那个字段。**null 的那几条最要紧**——它们正是表格里那些 `-`、「继承」的对应物，
// 落地成空串或 0 之后看的人不会察觉，按它分派的脚本会走错分支。

// decodeDoc 把一份文档编出来、再解进 out（传 &结构体 或 &map），返回编出来的原文。
//
// 走一遍编码再解码而不是直接看那个 map：编不出来的东西（chan、函数、循环引用）
// 在这里失败，而混进一行提示、一个 ANSI 转义、或者把金额从字符串改成了数字，
// 也各自在这里失败——一份多了 `"name": "-"` 的 JSON 仍然**是**合法 JSON，
// 肉眼未必看得出来。
//
// 解进**结构体**是刻意的：字段的类型也一并被钉住（`*string` 解一个数字会失败，
// 于是「金额必须是字符串」这件事不用另写一条断言）。
func decodeDoc(t *testing.T, doc map[string]any, out any) []byte {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("这份文档编不出来: %v", err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("编出来的东西解不回来: %v\n%s", err, raw)
	}
	return raw
}

// assertDocKeys 断言一个 JSON 对象的键**恰好**是这些。
//
// 多一个键就红：多出来的可能是一个不该出现在这里的字段（凭证明文、哈希、
// 别人的 id），而它一旦编进 JSON，没有人会注意到。
func assertDocKeys(t *testing.T, obj map[string]any, want []string) {
	t.Helper()
	got := make([]string, 0, len(obj))
	for k := range obj {
		got = append(got, k)
	}
	sort.Strings(got)

	sorted := slices.Clone(want)
	sort.Strings(sorted)
	if !slices.Equal(got, sorted) {
		t.Errorf("键集 = %v，期望 %v", got, sorted)
	}
}

// assertOnlyJSON 断言一段 stdout **整体**是一份合法 JSON，且不含转义序列。
//
// 这是 `--format json` 对外的全部承诺。拿整段去 json.Unmarshal 而不是
// 「看着像 JSON」：混进一行提示、一个进度点、或者一段 ANSI 上色都会在这里失败，
// 而人眼看那些输出时，一行提示语与 JSON 混在一起未必看得出来。
func assertOnlyJSON(t *testing.T, stdout string) {
	t.Helper()
	var got any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("json 模式下 stdout 不是一份合法的 JSON: %v\n%s", err, stdout)
	}
	if strings.ContainsRune(stdout, escByte) {
		t.Errorf("json 模式的 stdout 里不许有转义序列：%q", stdout)
	}
}

func TestAPIKeyListDocIsMachineReadable(t *testing.T) {
	rpm := 60
	quota := money.MustParse("12.34")
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	expires := time.Date(2027, 6, 7, 8, 9, 10, 0, time.UTC)
	used := time.Date(2026, 10, 17, 6, 10, 11, 0, time.UTC)

	doc := apiKeyListDoc([]auth.Key{
		{
			ID: 3, UserID: 7, Username: "user-placeholder", Name: "限额的",
			Prefix: "larkax_abcd", Status: "active",
			RateLimitRPM: &rpm, QuotaLimit: &quota,
			CreatedAt: created, ExpiresAt: &expires, LastUsedAt: &used,
		},
		{
			// 第二把什么都没设过：两列额度、过期与最近使用全是 NULL。
			ID: 4, UserID: 7, Username: "user-placeholder",
			Prefix: "larkax_efgh", Status: "revoked", CreatedAt: created,
		},
	})

	var got struct {
		Status  string `json:"status"`
		Summary struct {
			Total int `json:"total"`
		} `json:"summary"`
		Keys []struct {
			ID       int64  `json:"id"`
			Prefix   string `json:"prefix"`
			Username string `json:"username"`
			// 指针类型是刻意的：键不在、或者编成了 null 之外的别的东西，
			// 这里就是 nil，下面的断言当场失败。
			Name         *string `json:"name"`
			Status       string  `json:"status"`
			RateLimitRPM *int    `json:"rate_limit_rpm"`
			QuotaLimit   *string `json:"quota_limit"`
			CreatedAt    string  `json:"created_at"`
			ExpiresAt    *string `json:"expires_at"`
			LastUsedAt   *string `json:"last_used_at"`
		} `json:"keys"`
	}
	decodeDoc(t, doc, &got)

	if got.Status != jsonOK {
		t.Errorf("status = %q，想要 %q：清单类命令的结论一律是 ok", got.Status, jsonOK)
	}
	if got.Summary.Total != 2 {
		t.Errorf("summary.total = %d，想要 2", got.Summary.Total)
	}
	if len(got.Keys) != 2 {
		t.Fatalf("keys 有 %d 条，想要 2", len(got.Keys))
	}

	first := got.Keys[0]
	if first.ID != 3 || first.Prefix != "larkax_abcd" || first.Username != "user-placeholder" {
		t.Errorf("第一把的标识对不上：%+v", first)
	}
	if first.Name == nil || *first.Name != "限额的" {
		t.Errorf("name = %s，想要「限额的」", showPtr(first.Name))
	}
	if first.Status != "active" {
		t.Errorf("status = %q，想要 active", first.Status)
	}
	// 有值的那一档走 jsonAmount（十进制定点字符串），不是 JSON 数字，
	// 也不是表格里那个「继承」。
	if first.RateLimitRPM == nil || *first.RateLimitRPM != 60 {
		t.Errorf("rate_limit_rpm = %s，想要 60", showPtr(first.RateLimitRPM))
	}
	if first.QuotaLimit == nil || *first.QuotaLimit != "12.34000000" {
		t.Errorf("quota_limit = %s，想要字符串 \"12.34000000\"", showPtr(first.QuotaLimit))
	}
	// 三个时刻都是完整精度的 RFC3339，且取自不同的列（接错栏会当场挂）。
	for _, tc := range []struct {
		name string
		got  string
		want string
	}{
		{"created_at", first.CreatedAt, "2026-01-02T03:04:05Z"},
		{"expires_at", showPtr(first.ExpiresAt), "2027-06-07T08:09:10Z"},
		{"last_used_at", showPtr(first.LastUsedAt), "2026-10-17T06:10:11Z"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q，想要 %q", tc.name, tc.got, tc.want)
		}
	}

	// 第二把：五个空值全是 null，而不是「继承」「不过期」「从未」或空串。
	// 逐个判指针为 nil，不把它们装进 any 再比——装进去的是接口，一个 nil 的
	// `*string` 装进去之后**不等于** nil，那样的断言永远不会红。
	second := got.Keys[1]
	if second.Name != nil {
		t.Errorf("第二把的 name = %q，想要 null（没给 --name）", *second.Name)
	}
	if second.RateLimitRPM != nil {
		t.Errorf("第二把的 rate_limit_rpm = %d，想要 null（继承用户默认）", *second.RateLimitRPM)
	}
	if second.QuotaLimit != nil {
		t.Errorf("第二把的 quota_limit = %q，想要 null（继承用户默认）", *second.QuotaLimit)
	}
	if second.ExpiresAt != nil {
		t.Errorf("第二把的 expires_at = %q，想要 null（不过期）", *second.ExpiresAt)
	}
	if second.LastUsedAt != nil {
		t.Errorf("第二把的 last_used_at = %q，想要 null（从未用过）", *second.LastUsedAt)
	}
	// created_at 是 NOT NULL DEFAULT now()，没有 null 这一档。
	if second.CreatedAt != "2026-01-02T03:04:05Z" {
		t.Errorf("created_at = %q，想要 2026-01-02T03:04:05Z", second.CreatedAt)
	}

	// 键集逐个钉住。
	var generic map[string]any
	raw := decodeDoc(t, doc, &generic)
	keys, ok := generic["keys"].([]any)
	if !ok || len(keys) != 2 {
		t.Fatalf("keys 不是两条的数组：%v", generic["keys"])
	}
	for i, item := range keys {
		entry, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("第 %d 条不是对象：%v", i, item)
		}
		assertDocKeys(t, entry, []string{
			"id", "prefix", "username", "name", "status",
			"rate_limit_rpm", "quota_limit", "created_at", "expires_at", "last_used_at",
		})
	}
	// 明文与哈希一个字节都不许出现：库里只存哈希（见 internal/auth），而这份
	// 输出是给管道下游的，任何一处漏出去都不可撤销。
	if text := string(raw); strings.Contains(text, "plaintext") || strings.Contains(text, "hash") {
		t.Errorf("清单里出现了 key 的明文或哈希字段：\n%s", text)
	}
}

// 空清单是 `[]`，不是 null，也不是表格那一档的那句「暂无 API Key。」。
func TestAPIKeyListDocEmptyListIsEmptyArray(t *testing.T) {
	var got struct {
		Summary struct {
			Total int `json:"total"`
		} `json:"summary"`
		Keys *[]map[string]any `json:"keys"`
	}
	decodeDoc(t, apiKeyListDoc(nil), &got)

	if got.Keys == nil {
		t.Fatal("空清单编成了 null，要的是 []")
	}
	if len(*got.Keys) != 0 {
		t.Errorf("空清单里有 %d 条", len(*got.Keys))
	}
	if got.Summary.Total != 0 {
		t.Errorf("空清单的 summary.total = %d，想要 0", got.Summary.Total)
	}
}

// 写类命令在 json 模式下**必须在连库之前**被拦下。
//
// 这一条钉的是顺序，不是那句错误信息本身：DSN 指向一个没人听的端口，于是
// 「先连库、再报不支持」的实现在这里会拿到一句「连接数据库失败」（退出码 3），
// 而不是「还不支持 --format json」（退出码 2）。写类命令上这个顺序是硬要求
// ——先做了再报不支持，库里那一行已经改了，而调用方只看到一句参数错，于是重跑。
func TestAPIKeyWriteCommandsRejectJSONFormatBeforeTouchingTheDB(t *testing.T) {
	withFormat(t, formatJSON)
	// 连不上的库：真的去连就会以 exitDBUnreach 收场。
	t.Setenv("LARKAX_DB_DSN", "postgres://larkax:placeholder@127.0.0.1:1/larkax?sslmode=disable")

	cases := []struct {
		name string
		run  func() error
		want string
	}{
		{"create", func() error {
			return runAPIKeyCreate([]string{"--user", "1"})
		}, "apikey create"},
		{"update", func() error {
			return runAPIKeyUpdate([]string{"--id", "3", "--rate-limit-rpm", "60"})
		}, "apikey update"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			stdout, _ := captureStreams(t, func() { err = tc.run() })
			if err == nil {
				t.Fatal("json 模式下应当把还没接上的写类命令拦下来")
			}
			if !strings.Contains(err.Error(), "不支持 --format json") {
				t.Fatalf("错误信息 %q 里没有「不支持 --format json」——是不是先连库了", err.Error())
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误信息里要点名是哪条命令（%q）：%v", tc.want, err)
			}
			if code := exitCode(err); code != exitUsage {
				t.Errorf("退出码 = %d，想要 %d（参数错）", code, exitUsage)
			}
			if stdout != "" {
				t.Errorf("被拦下时 stdout 应当为空，得到 %q", stdout)
			}
		})
	}
}

// 命令级：过真命令、连真库，stdout 整体是一份合法 JSON，且不含转义序列。
func TestAPIKeyListJSONStdoutIsMachineReadable(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	owner := createTestUser(t)
	rpm := 60
	created, err := auth.New(db).Create(context.Background(), auth.CreateParams{
		UserID: owner, Name: uniqueName("json"), RateLimitRPM: &rpm,
	})
	if err != nil {
		t.Fatalf("建一把 key 失败: %v", err)
	}

	withFormat(t, formatJSON)
	userRef := strconv.FormatInt(owner, 10)
	stdout, _ := captureStreams(t, func() {
		err = runAPIKeyList([]string{"--user", userRef})
	})
	if err != nil {
		t.Fatalf("apikey list 失败: %v", err)
	}
	assertOnlyJSON(t, stdout)

	var got struct {
		Status string `json:"status"`
		Keys   []struct {
			ID       int64   `json:"id"`
			Prefix   string  `json:"prefix"`
			Username string  `json:"username"`
			Quota    *string `json:"quota_limit"`
		} `json:"keys"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("解不回来: %v\n%s", err, stdout)
	}
	if got.Status != jsonOK {
		t.Errorf("status = %q，想要 %q", got.Status, jsonOK)
	}
	if len(got.Keys) != 1 {
		t.Fatalf("这个用户下应当只有一把 key，得到 %d 把：\n%s", len(got.Keys), stdout)
	}
	if got.Keys[0].ID != created.ID || got.Keys[0].Prefix != created.Prefix {
		t.Errorf("这一行不是刚建的那把：%+v（想要 id=%d prefix=%s）",
			got.Keys[0], created.ID, created.Prefix)
	}
	// 没设过的额度是 null（表格那一档打「继承」），明文一个字节都不许进 stdout。
	if got.Keys[0].Quota != nil {
		t.Errorf("没设过的额度应当是 null，得到 %v", *got.Keys[0].Quota)
	}
	if strings.Contains(stdout, created.Plaintext) {
		t.Errorf("stdout 里出现了 key 的明文：\n%s", stdout)
	}
}

// 还没有结果时，json 模式的 stdout 上一个字节都不打。
//
// 命令在产出结果之前就失败（连不上库、查询失败）时，这份文档的 exit_code 契约
// 覆盖不了它——不过没有 exit_code 可填，一份「全零、0 条」的 JSON 更坏：它读起来
// 正是一次成功的空查询（见 format.go 里那条「还没有结果」的约定）。错误照旧返回，
// 由 main 打到 stderr。
func TestAPIKeyListJSONWithoutResultWritesNothingToStdout(t *testing.T) {
	setupEnv(t)
	withFormat(t, formatJSON)
	// 指向一个没人听的端口：连接立刻被拒。
	t.Setenv("LARKAX_DB_DSN", "postgres://larkax:placeholder@127.0.0.1:1/larkax?sslmode=disable")

	var err error
	stdout, _ := captureStreams(t, func() { err = runAPIKeyList(nil) })
	if err == nil {
		t.Fatal("连不上库时应当报错")
	}
	if code := exitCode(err); code != exitDBUnreach {
		t.Errorf("退出码 = %d，想要 %d（连不上库）", code, exitDBUnreach)
	}
	if stdout != "" {
		t.Errorf("还没有结果时 stdout 应当为空，得到 %q", stdout)
	}
}

// TestAPIKeyTimesAreUTCWhateverTheProcessZone 钉住「时刻一律按 UTC 打」。
//
// 库里那三列是 timestamptz，驱动交回来的是**带进程时区**的 time.Time（pgx
// 未设 ScanLocation 时用 time.Local 构造）：同一行在 TZ=UTC 与 TZ=Asia/Shanghai
// 的机器上，`t.Format(time.RFC3339)` 出来的是两个不同的字符串
// （`2026-01-02T03:04:05Z` 与 `2026-01-02T11:04:05+08:00`）。机器读的那一份不能
// 这样——读它的是脚本，而脚本不会去猜写它的机器在哪个时区。
//
// fixture 的三个时刻都取**进程时区**（`instant.In(time.Local)`），与驱动交回来
// 的那个 location 一致，所以这条用例的成败**取决于进程时区**：
//
//   - TZ=UTC：location 是 UTC，去掉 .UTC() 也编出同样的串 → 绿；
//   - TZ=Asia/Shanghai：去掉 .UTC() 编出 `2026-01-02T11:04:05+08:00` → 红。
//
// 这一对反差是 gates.yml 的「非 UTC 时区下的时刻门禁」那一步的判据（见
// docs/cli-output.md 的「时刻与时区」）：用固定东八区 fixture 的话它在任何
// 时区下都会红，证明的是「这一列恒 UTC」，证明不了「这道门跑在哪个时区」。
func TestAPIKeyTimesAreUTCWhateverTheProcessZone(t *testing.T) {
	// 三个同一瞬间：先按 UTC 构造，再 .In(time.Local) 换成进程时区。
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).In(time.Local)
	expires := time.Date(2027, 6, 7, 8, 9, 10, 0, time.UTC).In(time.Local)
	used := time.Date(2026, 10, 17, 6, 10, 11, 0, time.UTC).In(time.Local)

	doc := apiKeyListDoc([]auth.Key{{
		ID: 3, Username: "user-placeholder", Prefix: "larkax_abcd", Status: "active",
		CreatedAt: created, ExpiresAt: &expires, LastUsedAt: &used,
	}})

	var got struct {
		Keys []struct {
			CreatedAt  string  `json:"created_at"`
			ExpiresAt  *string `json:"expires_at"`
			LastUsedAt *string `json:"last_used_at"`
		} `json:"keys"`
	}
	decodeDoc(t, doc, &got)
	if len(got.Keys) != 1 {
		t.Fatalf("keys 有 %d 条，想要 1", len(got.Keys))
	}

	for _, tc := range []struct {
		name string
		got  string
		want string
	}{
		{"created_at", got.Keys[0].CreatedAt, "2026-01-02T03:04:05Z"},
		{"expires_at", showPtr(got.Keys[0].ExpiresAt), "2027-06-07T08:09:10Z"},
		{"last_used_at", showPtr(got.Keys[0].LastUsedAt), "2026-10-17T06:10:11Z"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q，想要 %q（同一瞬间的 UTC 写法，与进程时区无关）",
				tc.name, tc.got, tc.want)
		}
	}
}

// apikey get 的机器可读输出：与 list 里那一条**逐字段相同**。
//
// 同一份数据的两条读路径给出两套字段名时，对读它的脚本来说那就是两件事：
// 读 list 的那段代码换到 get 上要改一遍字段名，而漏改的那个字段会静默变成
// 零值。所以这里不只比「有哪些键」，而是把两份文档里对应的那一条整体比一遍。
func TestAPIKeyGetDocIsMachineReadable(t *testing.T) {
	rpm := 60
	quota := money.MustParse("12.34")
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	expires := time.Date(2027, 6, 7, 8, 9, 10, 0, time.UTC)
	used := time.Date(2026, 10, 17, 6, 10, 11, 0, time.UTC)

	key := auth.Key{
		ID: 3, UserID: 7, Username: "user-placeholder", Name: "限额的",
		Prefix: "larkax_abcd", Status: "active",
		RateLimitRPM: &rpm, QuotaLimit: &quota,
		CreatedAt: created, ExpiresAt: &expires, LastUsedAt: &used,
	}

	var got struct {
		Status string `json:"status"`
		Key    struct {
			ID       int64  `json:"id"`
			Prefix   string `json:"prefix"`
			Username string `json:"username"`
			// 指针类型是刻意的：键不在、或者编成了 null 之外的别的东西，
			// 这里就是 nil，下面的断言当场失败。
			Name         *string `json:"name"`
			Status       string  `json:"status"`
			RateLimitRPM *int    `json:"rate_limit_rpm"`
			QuotaLimit   *string `json:"quota_limit"`
			CreatedAt    string  `json:"created_at"`
			ExpiresAt    *string `json:"expires_at"`
			LastUsedAt   *string `json:"last_used_at"`
		} `json:"key"`
	}
	raw := decodeDoc(t, apiKeyGetDoc(key), &got)

	if got.Status != jsonOK {
		t.Errorf("status = %q，想要 %q：看一把 key 的结论也是 ok", got.Status, jsonOK)
	}
	if got.Key.ID != 3 || got.Key.Prefix != "larkax_abcd" || got.Key.Username != "user-placeholder" {
		t.Errorf("key 的标识对不上：%+v", got.Key)
	}
	if got.Key.Name == nil || *got.Key.Name != "限额的" {
		t.Errorf("name = %s，想要「限额的」", showPtr(got.Key.Name))
	}
	if got.Key.Status != "active" {
		t.Errorf("status = %q，想要 active", got.Key.Status)
	}
	if got.Key.RateLimitRPM == nil || *got.Key.RateLimitRPM != 60 {
		t.Errorf("rate_limit_rpm = %s，想要 60", showPtr(got.Key.RateLimitRPM))
	}
	if got.Key.QuotaLimit == nil || *got.Key.QuotaLimit != "12.34000000" {
		t.Errorf("quota_limit = %s，想要字符串 \"12.34000000\"", showPtr(got.Key.QuotaLimit))
	}
	// 三个时刻都是完整精度的 RFC3339，且取自不同的列（接错栏会当场挂）。
	for _, tc := range []struct {
		name string
		got  string
		want string
	}{
		{"created_at", got.Key.CreatedAt, "2026-01-02T03:04:05Z"},
		{"expires_at", showPtr(got.Key.ExpiresAt), "2027-06-07T08:09:10Z"},
		{"last_used_at", showPtr(got.Key.LastUsedAt), "2026-10-17T06:10:11Z"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q，想要 %q", tc.name, tc.got, tc.want)
		}
	}
	// 明文与哈希一个字节都不许出现：这一份是给管道下游的。
	if text := string(raw); strings.Contains(text, "plaintext") || strings.Contains(text, "hash") {
		t.Errorf("这一份里出现了 key 的明文或哈希字段：\n%s", text)
	}

	// 键集与 list 的条目逐个相同，两条路径给的是同一条。
	var generic, listDoc map[string]any
	decodeDoc(t, apiKeyGetDoc(key), &generic)
	decodeDoc(t, apiKeyListDoc([]auth.Key{key}), &listDoc)

	entry, ok := generic["key"].(map[string]any)
	if !ok {
		t.Fatalf("key 不是一个对象：%v", generic["key"])
	}
	assertDocKeys(t, entry, []string{
		"id", "prefix", "username", "name", "status",
		"rate_limit_rpm", "quota_limit", "created_at", "expires_at", "last_used_at",
	})

	// 解回 any 之后，数组是 []any、元素是 map[string]any——JSON 里没有
	// 「map 的切片」这个概念，所以这里按解码后的形状取。
	listed, ok := listDoc["keys"].([]any)
	if !ok || len(listed) != 1 {
		t.Fatalf("list 那一份不是一条的数组：%v", listDoc["keys"])
	}
	listedEntry, ok := listed[0].(map[string]any)
	if !ok {
		t.Fatalf("list 里的那一条不是对象：%v", listed[0])
	}
	if !maps.Equal(entry, listedEntry) {
		t.Errorf("get 的那一条与 list 里那一条不一样：\nget  = %v\nlist = %v", entry, listedEntry)
	}
}

// apikey get 是读类命令，json 模式必须**支持**。
//
// 与写类那两条相反：它在 json 模式下要真的去连库，所以连不上时拿到的是
// 「连接数据库失败」（退出码 3），而不是「还不支持 --format json」（退出码 2）。
// 拿一句「不支持」把它当写类命令拦下来，脚本就再也拿不到单把 key 的字段了。
func TestAPIKeyGetSupportsJSONFormat(t *testing.T) {
	withFormat(t, formatJSON)
	// 连不上的库：真的去连就会以 exitDBUnreach 收场。
	t.Setenv("LARKAX_DB_DSN", "postgres://larkax:placeholder@127.0.0.1:1/larkax?sslmode=disable")

	var err error
	stdout, _ := captureStreams(t, func() { err = runAPIKeyGet([]string{"--id", "1"}) })
	if err == nil {
		t.Fatal("连不上库时应当报错")
	}
	if strings.Contains(err.Error(), "不支持 --format json") {
		t.Fatalf("apikey get 是读类命令，不该被拦下：%v", err)
	}
	if code := exitCode(err); code != exitDBUnreach {
		t.Errorf("退出码 = %d，想要 %d（连不上库）", code, exitDBUnreach)
	}
	if stdout != "" {
		t.Errorf("还没有结果时 stdout 应当为空，得到 %q", stdout)
	}
}

// 按 --id 找不着一行时落 exitConflict（4），不是 1。
//
// 下一步是「先去看库里现在有什么」（apikey list），而不是改一下命令重发——
// 这正是 1 与 4 的区别。判据落在具体的哨兵上，不只判「报错了」。
func TestAPIKeyGetMissingKeyExitsFour(t *testing.T) {
	// 库要真的连得上：这条命令的失败发生在**查询**那一步，不是连库那一步。
	testDB(t)
	setupEnv(t)

	err := runCaptured(t, func() error {
		return runAPIKeyGet([]string{"--id", "9223372036854775807"})
	})
	if err == nil {
		t.Fatal("期望报错")
	}
	if !errors.Is(err, auth.ErrKeyNotFound) {
		t.Fatalf("错误 %v 不满足 errors.Is(err, auth.ErrKeyNotFound)", err)
	}
	if got := exitCode(err); got != exitConflict {
		t.Errorf("退出码 = %d，想要 %d（要查的那一行不存在）", got, exitConflict)
	}
}

// detailField 从 printKeyDetail 的竖排输出里取一个字段的值。
func detailField(t *testing.T, out, label string) string {
	t.Helper()

	for line := range strings.SplitSeq(out, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), label)
		if !ok {
			continue
		}
		return strings.TrimSpace(rest)
	}
	t.Fatalf("输出里没有 %q 这一栏：\n%s", label, out)
	return ""
}

// apikey 的写侧串一遍：真库 + 真命令。
//
// 走「建 → 看现状 → 吊销 → 再看」这一串：#693 之前没有任何一条路径能把状态
// 写进去，而 get 是这次新加的「读一把」的路径。断的是命令打出来的东西，不是
// 包里的结构体——中间那一段（把旗标读成一次修改意图、再把结果渲染出来）只有
// 过真命令才覆盖得到，而它正是最容易「编得过、做出来是另一件事」的一段。
func TestAPIKeyStatusRoundTripThroughRealCommands(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	owner := createTestUser(t)
	created, err := auth.New(db).Create(context.Background(), auth.CreateParams{
		UserID: owner, Name: uniqueName("吊销"),
	})
	if err != nil {
		t.Fatalf("建一把 key 失败: %v", err)
	}
	id := strconv.FormatInt(created.ID, 10)

	getDetail := func() string {
		t.Helper()
		var getErr error
		stdout, _ := captureStreams(t, func() { getErr = runAPIKeyGet([]string{"--id", id}) })
		if getErr != nil {
			t.Fatalf("apikey get 失败: %v", getErr)
		}
		return stdout
	}

	if got := detailField(t, getDetail(), "状态"); got != "active" {
		t.Fatalf("刚建出来的 key 状态 = %q，期望 active", got)
	}

	var updateErr error
	_, stderr := captureStreams(t, func() {
		updateErr = runAPIKeyUpdate([]string{"--id", id, "--status", "revoked"})
	})
	if updateErr != nil {
		t.Fatalf("apikey update 失败: %v", updateErr)
	}
	if !strings.Contains(stderr, "状态 从 active 改为 revoked") {
		t.Errorf("回显没说清这次改了什么：\n%s", stderr)
	}

	// 再看一次：变的是库里那一列，不是回显自己编的。
	if got := detailField(t, getDetail(), "状态"); got != "revoked" {
		t.Errorf("吊销之后状态 = %q，期望 revoked", got)
	}
}

// 一个字段都没给时报用法错，且库里那一行原样不动。
//
// 前半句是「不能静默空转」——一条什么都没改的命令与一条改成功的命令在屏幕上
// 一模一样；后半句是「这半句真的成立」：报错发生在连库之前，所以那一行不该
// 有任何变化。
func TestAPIKeyUpdateWithNoFieldsChangesNothing(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	owner := createTestUser(t)
	name := uniqueName("空转")
	created, err := auth.New(db).Create(context.Background(), auth.CreateParams{
		UserID: owner, Name: name,
	})
	if err != nil {
		t.Fatalf("建一把 key 失败: %v", err)
	}
	id := strconv.FormatInt(created.ID, 10)

	var runErr error
	stdout, _ := captureStreams(t, func() { runErr = runAPIKeyUpdate([]string{"--id", id}) })
	if runErr == nil {
		t.Fatal("一个字段都没给时应当报错，而不是静默空转")
	}
	if code := exitCode(runErr); code != exitUsage {
		t.Errorf("退出码 = %d，想要 %d（参数错）", code, exitUsage)
	}
	if !strings.Contains(runErr.Error(), "没有要改的东西") {
		t.Errorf("错误信息 %q 里没说清缺什么", runErr.Error())
	}
	if stdout != "" {
		t.Errorf("被拦下时 stdout 应当为空，得到 %q", stdout)
	}

	after, err := auth.New(db).GetKey(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("读回 key 失败: %v", err)
	}
	if after.Status != "active" || after.Name != name {
		t.Errorf("这次调用动了库里的行：status=%q name=%q，期望 %q / %q",
			after.Status, after.Name, "active", name)
	}
}

// 备注与过期时间也串一遍：这两列原先没有写侧，而它们的判据与状态不同
// （备注空串是合法取值、过期要能写回 NULL）。
func TestAPIKeyNameAndExpiryRoundTripThroughRealCommands(t *testing.T) {
	db := testDB(t)
	setupEnv(t)

	owner := createTestUser(t)
	created, err := auth.New(db).Create(context.Background(), auth.CreateParams{
		UserID: owner, Name: uniqueName("备注"),
	})
	if err != nil {
		t.Fatalf("建一把 key 失败: %v", err)
	}
	id := strconv.FormatInt(created.ID, 10)

	getDetail := func() string {
		t.Helper()
		var getErr error
		stdout, _ := captureStreams(t, func() { getErr = runAPIKeyGet([]string{"--id", id}) })
		if getErr != nil {
			t.Fatalf("apikey get 失败: %v", getErr)
		}
		return stdout
	}

	if err := runCaptured(t, func() error {
		return runAPIKeyUpdate([]string{"--id", id, "--name", "运维备用", "--expires-at", "2030-01-02T03:04:05Z"})
	}); err != nil {
		t.Fatalf("apikey update 失败: %v", err)
	}

	detail := getDetail()
	if got := detailField(t, detail, "备注"); got != "运维备用" {
		t.Errorf("备注 = %q，期望 运维备用", got)
	}
	// 详情里的时刻打完整 RFC3339：这一栏回答的是「什么时候到期」，秒是它要
	// 回答的问题的一部分。它是**给人看的那一档**，按进程时区打（与 list 的
	// 时刻同一套约定）；机器读的那一份走 jsonTime，恒 UTC。所以期望值按
	// 进程时区算出来，别钉一个字面量——钉了的那一份只在 UTC 的机器上是对的。
	wantExpires := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC).In(time.Local).Format(time.RFC3339)
	if got := detailField(t, detail, "过期"); got != wantExpires {
		t.Errorf("过期 = %q，期望 %q（进程时区下的写法）", got, wantExpires)
	}

	// 清空（旧名 --clear-expires-at）：写远期上限，人读的那一档仍打「不过期」。
	if err := runCaptured(t, func() error {
		return runAPIKeyUpdate([]string{"--id", id, "--name", "", "--clear-expires-at"})
	}); err != nil {
		t.Fatalf("清空失败: %v", err)
	}
	detail = getDetail()
	if got := detailField(t, detail, "过期"); got != "不过期" {
		t.Errorf("清空之后过期 = %q，期望 不过期", got)
	}
	if got := detailField(t, detail, "备注"); got != "-" {
		t.Errorf("清空之后备注 = %q，期望 -", got)
	}
}

// --forever 与它的旧名 --clear-expires-at 必须写回同一个值。
//
// 判据读的是**库里的那一列**，不是命令打出来的那句话：两个入口在屏幕上
// 都打成「不过期」，而这一条要证明的是它们落库的值也逐字相等。
func TestAPIKeyForeverAndClearExpiresWriteTheSameValue(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	ctx := context.Background()

	// 造用户在闭包外面：createTestUser 走的是 `user create` 那条命令，
	// 命令自己取 context（CLI 命令的签名不收 ctx），放在闭包里会让
	// contextcheck 认为这里该把 ctx 传下去。
	userForever := createTestUser(t)
	userClear := createTestUser(t)
	createKey := func(userID int64) int64 {
		t.Helper()
		created, err := auth.New(db).Create(ctx, auth.CreateParams{
			UserID: userID, Name: uniqueName("forever"),
		})
		if err != nil {
			t.Fatalf("建一把 key 失败: %v", err)
		}
		return created.ID
	}
	expiryOf := func(id int64) *time.Time {
		t.Helper()
		var at *time.Time
		if err := db.QueryRowContext(ctx,
			`SELECT expires_at FROM api_keys WHERE id = $1`, id).Scan(&at); err != nil {
			t.Fatalf("读 key %d 的 expires_at 失败: %v", id, err)
		}
		return at
	}

	idForever := createKey(userForever)
	idClear := createKey(userClear)
	if err := runCaptured(t, func() error {
		return runAPIKeyUpdate([]string{"--id", strconv.FormatInt(idForever, 10), "--forever"})
	}); err != nil {
		t.Fatalf("--forever 失败: %v", err)
	}
	if err := runCaptured(t, func() error {
		return runAPIKeyUpdate([]string{"--id", strconv.FormatInt(idClear, 10), "--clear-expires-at"})
	}); err != nil {
		t.Fatalf("--clear-expires-at 失败: %v", err)
	}

	forever, clear := expiryOf(idForever), expiryOf(idClear)
	if forever == nil || clear == nil {
		t.Fatalf("两个入口都该写一个时刻，得到 --forever=%v --clear-expires-at=%v", forever, clear)
	}
	if !forever.Equal(*clear) {
		t.Errorf("--forever 写入 %s，--clear-expires-at 写入 %s，两者应当逐字相等",
			forever.UTC().Format(time.RFC3339), clear.UTC().Format(time.RFC3339))
	}
	if got := forever.UTC().Format(time.RFC3339); got != expiry.ForeverRFC3339 {
		t.Errorf("写入 %s，期望远期上限 %s", got, expiry.ForeverRFC3339)
	}
}

// create 上的 --ttl 与 --forever 走真命令写库：--ttl 的期望值来自注入的现在。
func TestAPIKeyCreateTTLAndForever(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	ctx := context.Background()

	expiryOf := func(userID int64) *time.Time {
		t.Helper()
		var at *time.Time
		if err := db.QueryRowContext(ctx,
			`SELECT expires_at FROM api_keys WHERE user_id = $1 ORDER BY id DESC LIMIT 1`,
			userID).Scan(&at); err != nil {
			t.Fatalf("读用户 %d 那把 key 的 expires_at 失败: %v", userID, err)
		}
		return at
	}

	// 1 月 31 日 + 1m 落在 2 月 28 日（2026 不是闰年）；判据是逐秒相等。
	now := time.Date(2026, 1, 31, 12, 0, 0, 0, time.UTC)
	withExpiryNow(t, now)

	ttlUser := createTestUser(t)
	_, stderr := mustStream(t, func() error {
		return runAPIKeyCreate([]string{"--user", strconv.FormatInt(ttlUser, 10), "--ttl", "1m"})
	})
	wantTTL := time.Date(2026, 2, 28, 12, 0, 0, 0, time.UTC)
	// create 的回显打的是「算出来的那个时刻」本身的时区：--ttl 按 UTC 换算，
	// 所以这里就是 Z。读库那一档（get / update）才跟着会话时区走。
	if want := "过期=" + wantTTL.Format(time.RFC3339); !strings.Contains(stderr, want) {
		t.Errorf("create 回显里没有 %q：%q", want, stderr)
	}
	if got := expiryOf(ttlUser); got == nil || !got.Equal(wantTTL) {
		t.Errorf("--ttl 1m 写进去的是 %v，期望 %s", got, wantTTL.Format(time.RFC3339))
	}

	foreverUser := createTestUser(t)
	_, stderr = mustStream(t, func() error {
		return runAPIKeyCreate([]string{"--user", strconv.FormatInt(foreverUser, 10), "--forever"})
	})
	if !strings.Contains(stderr, "过期=不过期") {
		t.Errorf("--forever 建出来的 key 回显该说「不过期」：%q", stderr)
	}
	if got := expiryOf(foreverUser); got == nil ||
		got.UTC().Format(time.RFC3339) != expiry.ForeverRFC3339 {
		t.Errorf("--forever 写进去的是 %v，期望 %s", got, expiry.ForeverRFC3339)
	}
}

// splitPaddedCells 把一行对齐后的表格按「两格以上的空白」切成栏。
//
// 不用 strings.Fields：创建、过期、最近使用这三栏的值本身就是「月-日 时:分」，
// 里面有一个空格；按空白切会把一栏拆成两栏，而「不过期」只有一个词——同一行
// 的栏数会随这一格渲染成什么而变。栏与栏之间固定隔两格（table.go 的 columnGap），
// 按它切才与列对齐无关。
func splitPaddedCells(line string) []string {
	var cells []string
	start := 0
	for i := 0; i < len(line); {
		if line[i] != ' ' {
			i++
			continue
		}
		j := i
		for j < len(line) && line[j] == ' ' {
			j++
		}
		if j-i >= 2 {
			cells = append(cells, line[start:i])
			start = j
		}
		i = j
	}
	return append(cells, line[start:])
}

// TestAPIKeyNullAndForeverRenderAlike 把「没设过」与「永久」并排放在同一条
// 用例里对拍：库里一条是 NULL、一条是远期时间戳，而给人看的两处输出说的是
// 同一句话。
//
// 判据是**整格逐字相等**，不是 Contains：带日期的渲染与「不过期」之间差的
// 正是这一格的全部内容，逐字相等顺带把「不得出现远期上限或任何年月日
// 形态」也钉住了——只要那一格不是「不过期」，它就红。
func TestAPIKeyNullAndForeverRenderAlike(t *testing.T) {
	db := testDB(t)
	setupEnv(t)
	ctx := context.Background()

	// 与上一条同理：造用户放在闭包外，命令自己取 context。
	userNull := createTestUser(t)
	userForever := createTestUser(t)
	createKey := func(userID int64, name string) int64 {
		t.Helper()
		k, err := auth.New(db).Create(ctx, auth.CreateParams{
			UserID: userID, Name: uniqueName(name),
		})
		if err != nil {
			t.Fatalf("建一把 key 失败: %v", err)
		}
		return k.ID
	}
	expiryOf := func(id int64) *time.Time {
		t.Helper()
		var at *time.Time
		if err := db.QueryRowContext(ctx,
			`SELECT expires_at FROM api_keys WHERE id = $1`, id).Scan(&at); err != nil {
			t.Fatalf("读 key %d 的 expires_at 失败: %v", id, err)
		}
		return at
	}

	idNull := createKey(userNull, "null-expiry")
	idForever := createKey(userForever, "forever-expiry")
	if err := runCaptured(t, func() error {
		return runAPIKeyUpdate([]string{"--id", strconv.FormatInt(idForever, 10), "--forever"})
	}); err != nil {
		t.Fatalf("--forever 失败: %v", err)
	}

	// 断言之前先读库：这一条用例的前提正是两侧落库值不同——一边 NULL、
	// 一边远期时间戳。前提不成立时，下面的相等就没有意义。
	if at := expiryOf(idNull); at != nil {
		t.Fatalf("NULL 那一侧的 expires_at = %s，期望 NULL", at.Format(time.RFC3339))
	}
	atForever := expiryOf(idForever)
	if atForever == nil || atForever.UTC().Format(time.RFC3339) != expiry.ForeverRFC3339 {
		t.Fatalf("远期那一侧的 expires_at = %v，期望 %s", atForever, expiry.ForeverRFC3339)
	}

	// list：「过期」是从 0 数的第 9 栏（ID、KEY 前缀、所属用户、备注、状态、
	// 每分钟、额度、创建、过期、最近使用）。
	stdout, _ := mustStream(t, func() error { return runAPIKey([]string{"list"}) })
	listExpiry := func(id int64) string {
		t.Helper()
		idText := strconv.FormatInt(id, 10)
		for _, line := range strings.Split(strings.TrimRight(stdout, "\n"), "\n") {
			cells := splitPaddedCells(line)
			if len(cells) > 8 && cells[0] == idText {
				return cells[8]
			}
		}
		t.Fatalf("list 输出里找不到 id=%s 的「过期」那一栏：\n%s", idText, stdout)
		return ""
	}

	detailExpiry := func(id int64) string {
		t.Helper()
		out, _ := mustStream(t, func() error {
			return runAPIKey([]string{"get", "--id", strconv.FormatInt(id, 10)})
		})
		for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
			cells := splitPaddedCells(line)
			if len(cells) == 2 && cells[0] == "过期" {
				return cells[1]
			}
		}
		t.Fatalf("详情里找不到「过期」那一栏：\n%s", out)
		return ""
	}

	checks := []struct {
		where string
		got   string
	}{
		{"list 的 NULL 行", listExpiry(idNull)},
		{"list 的远期行", listExpiry(idForever)},
		{"get 的 NULL 详情", detailExpiry(idNull)},
		{"get 的远期详情", detailExpiry(idForever)},
	}
	for _, c := range checks {
		if c.got != "不过期" {
			t.Errorf("%s「过期」= %q，期望「不过期」：远期值走的是日期渲染", c.where, c.got)
		}
	}
}
