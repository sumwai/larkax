package main

import (
	"bytes"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/auth"
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

// TestUpdateLimitsThreeStates 钉住 update 的三态。
//
// 这三态里最不能弄错的是**清空与设成 0**：前者是不限，后者是一个都不放行。
// 把清空实现成「写成 0」，表现是每一次「恢复默认」都变成一次静默的封禁，
// 而它不报任何错——所以这里逐个断言 Set* 与指针的组合，而不只是「没报错」。
func TestUpdateLimitsThreeStates(t *testing.T) {
	rpmFlag := map[string]bool{"rate-limit-rpm": true}
	quotaFlag := map[string]bool{"quota-limit": true}

	t.Run("不给旗标就是不改", func(t *testing.T) {
		_, err := updateLimits(map[string]bool{}, limitFlag{}, limitFlag{})
		if err == nil || !strings.Contains(err.Error(), "没有要改的东西") {
			t.Fatalf("期望「没有要改的东西」，得到 %v", err)
		}
	})

	t.Run("清空 rpm 是 Set 为真且指针为 nil", func(t *testing.T) {
		params, err := updateLimits(map[string]bool{}, limitFlag{clear: true}, limitFlag{})
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
		params, err := updateLimits(rpmFlag, limitFlag{value: "0"}, limitFlag{})
		if err != nil {
			t.Fatalf("出错: %v", err)
		}
		if !params.SetRateLimit || params.RateLimitRPM == nil || *params.RateLimitRPM != 0 {
			t.Fatalf("得到 SetRateLimit=%v RateLimitRPM=%s，期望 true 与指向 0 的指针",
				params.SetRateLimit, showPtr(params.RateLimitRPM))
		}
	})

	t.Run("rpm 给 60", func(t *testing.T) {
		params, err := updateLimits(rpmFlag, limitFlag{value: "60"}, limitFlag{})
		if err != nil {
			t.Fatalf("出错: %v", err)
		}
		if params.RateLimitRPM == nil || *params.RateLimitRPM != 60 {
			t.Fatalf("RateLimitRPM = %s，期望 60", showPtr(params.RateLimitRPM))
		}
	})

	t.Run("清空 quota 是 Set 为真且指针为 nil", func(t *testing.T) {
		params, err := updateLimits(map[string]bool{}, limitFlag{}, limitFlag{clear: true})
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
		params, err := updateLimits(quotaFlag, limitFlag{}, limitFlag{value: "0"})
		if err != nil {
			t.Fatalf("出错: %v", err)
		}
		if !params.SetQuota || params.QuotaLimit == nil || !params.QuotaLimit.IsZero() {
			t.Fatalf("得到 SetQuota=%v QuotaLimit=%v，期望 true 与零金额",
				params.SetQuota, params.QuotaLimit)
		}
	})

	t.Run("quota 给具体金额", func(t *testing.T) {
		params, err := updateLimits(quotaFlag, limitFlag{}, limitFlag{value: "12.34"})
		if err != nil {
			t.Fatalf("出错: %v", err)
		}
		if params.QuotaLimit == nil || params.QuotaLimit.String() != "12.34000000" {
			t.Fatalf("QuotaLimit = %v，期望 12.34000000", params.QuotaLimit)
		}
	})

	t.Run("两项一起改", func(t *testing.T) {
		seen := map[string]bool{"rate-limit-rpm": true, "quota-limit": true}
		params, err := updateLimits(seen, limitFlag{value: "60"}, limitFlag{value: "5"})
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
		_, err := updateLimits(map[string]bool{}, limitFlag{}, limitFlag{})
		if err == nil {
			t.Fatal("clear=false 不该被当成一次修改")
		}
	})
}

func TestUpdateLimitsRejectsConflictingAndBadValues(t *testing.T) {
	cases := []struct {
		name       string
		seen       map[string]bool
		rpm        string
		clearRPM   bool
		quota      string
		clearQuota bool
		want       string
	}{
		{"rpm 设值与清空同时给",
			map[string]bool{"rate-limit-rpm": true}, "60", true, "", false,
			"不能同时给"},
		{"quota 设值与清空同时给",
			map[string]bool{"quota-limit": true}, "", false, "5", true,
			"不能同时给"},
		{"rpm 是负数", map[string]bool{"rate-limit-rpm": true}, "-1", false, "", false,
			"不能是负数"},
		{"rpm 不是整数", map[string]bool{"rate-limit-rpm": true}, "六十", false, "", false,
			"需要整数"},
		{"quota 小数位过多", map[string]bool{"quota-limit": true}, "", false, "1.123456789", false,
			"最多 8 位小数"},
		{"quota 是负数", map[string]bool{"quota-limit": true}, "", false, "-0.5", false,
			"不能是负数"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := updateLimits(tc.seen,
				limitFlag{value: tc.rpm, clear: tc.clearRPM},
				limitFlag{value: tc.quota, clear: tc.clearQuota})
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

// printUpdatedLimits 要能区分「清空」与「设成 0」——两者的输出不能一样，
// 否则回显就失去了它的用处（确认自己做的正是想做的那一件事）。
func TestPrintUpdatedLimitsDistinguishesClearFromZero(t *testing.T) {
	zero := 0

	var cleared bytes.Buffer
	printUpdatedLimits(&cleared, 3, auth.LimitParams{SetRateLimit: true})

	var setToZero bytes.Buffer
	printUpdatedLimits(&setToZero, 3, auth.LimitParams{SetRateLimit: true, RateLimitRPM: &zero})

	if got, want := cleared.String(), "已改 API Key id=3 每分钟=继承\n"; got != want {
		t.Errorf("清空 = %q，期望 %q", got, want)
	}
	if got, want := setToZero.String(), "已改 API Key id=3 每分钟=0\n"; got != want {
		t.Errorf("设成 0 = %q，期望 %q", got, want)
	}
	if cleared.String() == setToZero.String() {
		t.Error("清空与设成 0 打出来的东西一样")
	}
}
