package main

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"sort"
	"strconv"
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
