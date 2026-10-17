package ledger

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/money"
)

func TestAccountConstructors(t *testing.T) {
	cases := []struct {
		got  Account
		want string
		typ  AccountType
		id   string
	}{
		{UserAccount(42), "user:42", AccountUser, "42"},
		{MerchantPayableAccount(7), "merchant_payable:7", AccountMerchantPayable, "7"},
		{PlatformRevenueAccount(), "platform_revenue", AccountPlatformRevenue, ""},
		{UpstreamCostAccount(), "upstream_cost", AccountUpstreamCost, ""},
		{PaymentGatewayAccount("stripe"), "payment_gateway:stripe", AccountPaymentGateway, "stripe"},
		{PayoutAccount(9), "payout:9", AccountPayout, "9"},
		{ReconcileDifferenceAccount(), "reconcile_difference", AccountReconcileDifference, ""},
	}
	for _, tc := range cases {
		if got := tc.got.String(); got != tc.want {
			t.Errorf("科目字符串 = %q，期望 %q", got, tc.want)
		}
		if got := tc.got.Type(); got != tc.typ {
			t.Errorf("%s 的大类 = %q，期望 %q", tc.want, got, tc.typ)
		}
		if got := tc.got.ID(); got != tc.id {
			t.Errorf("%s 的后缀 = %q，期望 %q", tc.want, got, tc.id)
		}
		if _, err := ParseAccount(tc.want); err != nil {
			t.Errorf("构造函数产出的科目 %q 应当能解析: %v", tc.want, err)
		}
	}
}

// 科目字符串是外部输入——从库里读回、从别的服务传来。解析必须挡住
// 每一个非法的形态，而不是「尽量读出一个能用的值」：宽松解析会把一个
// 真实存在的问题推迟到对账时才暴露。
func TestParseAccountRejects(t *testing.T) {
	bad := []string{
		"",                       // 整列漏填
		"user",                   // 缺 id 后缀
		"merchant_payable",       // 同上
		"payment_gateway",        //
		"payout",                 //
		"user:",                  // 后缀为空
		"user:1:2",               // 多余分隔符：后缀含义无法确定
		"bogus:1",                // 大类未知
		"platform_revenue:x",     // 单例科目带了后缀
		"upstream_cost:1",        // 同上
		"reconcile_difference:1", // 同上
		"user:1 2",               // 后缀含空格
		"user:1\n2",              // 后缀含换行（会污染对账输出）
		"user_id:1",              // 下划线写法看似合法，实际不是科目
		" user:1",                // 前后空白
		"user:1 ",                //
	}
	for _, s := range bad {
		if _, err := ParseAccount(s); err == nil {
			t.Errorf("ParseAccount(%q) 应当报错", s)
		}
	}
}

// 用户 id 只放行一种写法：正整数，十进制，无前导零、无正号。
//
// 别的写法读得出同一个用户，但字符串不相等，而账本的汇总是字符串比对
// ——于是那样一笔钱会同时「算进了钱包」和「不在权威值里」，对账拿一个
// 偏小的权威值去比，把**正确的**余额改小。让写法只有一种，缝就不存在。
func TestParseAccountRejectsNonCanonicalUserID(t *testing.T) {
	bad := []string{
		"user:0",                   // 用户 id 从 1 起（users.id 是 bigserial），0 不是用户
		"user:00",                  //
		"user:+0",                  //
		"user:042",                 // 前导零：ParseInt 认得，字符串比对不认得
		"user:+42",                 // 正号：同上
		"user:-1",                  // 负 id 不可能存在（钱包行有外键约束）
		"user:abc",                 // 后缀不是整数
		"user:1234567890123456789", // 十九位：超出对账认得的位数上限
	}
	for _, s := range bad {
		if _, err := ParseAccount(s); err == nil {
			t.Errorf("ParseAccount(%q) 应当报错", s)
		}
	}

	good := []string{
		"user:1",
		"user:42",
		"user:100000",
		"user:123456789012345678", // 十八位：上限之内
	}
	for _, s := range good {
		if _, err := ParseAccount(s); err != nil {
			t.Errorf("ParseAccount(%q) 不该报错: %v", s, err)
		}
	}
}

// UserIDFromAccount 比 ParseAccount 宽松：它认得旧写路径留下的写法。
//
// 宽松是有意的，不是漏网：那些钱当初**已经**被算进那个用户的钱包了
// （旧写路径把后缀读成整数），对账必须按同一个口径把它们归回同一个用户，
// 否则汇总值偏小，会把正确的余额改小。
//
// 宽松有底：**0 号用户不存在**，所以 `user:0` 那类写法上的钱没有落在任何
// 钱包上，「后缀能转成整数」不等于「钱归到位了」。它们与 `user:abc` 同类。
func TestUserIDFromAccount(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"user:42", 42, true},
		{"user:042", 42, true},
		{"user:+42", 42, true},
		{"user:+0042", 42, true},
		{"user:0000000000000000000042", 42, true},
		{"user:123456789012345678", 123456789012345678, true},

		// 读不出 id：这些写法上的钱不属于任何人的钱包。
		{"user:abc", 0, false},
		{"user:", 0, false},
		{"user:+", 0, false},
		{"user:1:2", 0, false},
		{"user :42", 0, false},                 // 分隔符前多一个空格
		{"user: 42", 0, false},                 // 后缀前多一个空格
		{"user:-5", 0, false},                  // 负号
		{"user:1234567890123456789", 0, false}, // 十九位
		{"user", 0, false},
		{"", 0, false},
		// 全零后缀：读得成整数 0，但 0 号用户不存在，钱没落在任何钱包上。
		{"user:0", 0, false},
		{"user:00", 0, false},
		{"user:000", 0, false},
		{"user:+0", 0, false},
		// 别的大类的后缀含义不同，不能读成用户 id。
		{"merchant_payable:42", 0, false},
		{"payout:42", 0, false},
		{"payment_gateway:42", 0, false},
		{"platform_revenue", 0, false},
	}
	for _, tc := range cases {
		got, ok := UserIDFromAccount(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("UserIDFromAccount(%q) = (%d, %v)，期望 (%d, %v)",
				tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// Account.UserID 与 UserIDFromAccount 是同一条判定，写路径用的是它。
func TestAccountUserID(t *testing.T) {
	if got, err := UserAccount(42).UserID(); err != nil || got != 42 {
		t.Errorf("UserAccount(42).UserID() = (%d, %v)，期望 (42, nil)", got, err)
	}
	for _, a := range []Account{
		PlatformRevenueAccount(),
		MerchantPayableAccount(42),
		PayoutAccount(42),
		PaymentGatewayAccount("stripe"),
		{},
	} {
		if got, err := a.UserID(); err == nil {
			t.Errorf("%q 不是用户钱包科目，不该读出用户 id %d", a, got)
		}
	}
	// 是 user 科目、后缀也读得成整数，但 0 号用户不存在：钱没落在任何钱包上。
	// 这里要报错而不是返回 0——写路径拿到 0 会去更新一个不存在的钱包行，
	// 而对账会据此说这笔钱「已归位」。
	if got, err := (Account{raw: "user:0"}).UserID(); err == nil {
		t.Errorf("user:0 不该读出用户 id %d（0 号用户不存在）", got)
	}
}

func TestParseAccountAcceptsIDCharset(t *testing.T) {
	// 通道名与第三方对账号会带点、连字符、下划线、@。
	for _, s := range []string{
		"payment_gateway:stripe",
		"payment_gateway:ali-pay_v2",
		"payment_gateway:user@example.com",
		"user:42",
	} {
		if _, err := ParseAccount(s); err != nil {
			t.Errorf("ParseAccount(%q) 不该报错: %v", s, err)
		}
	}
}

// 读回路径与写出路径必须对称，且读回时同样经过完整校验。
func TestAccountTextRoundTrip(t *testing.T) {
	orig := MerchantPayableAccount(1234)

	text, err := orig.MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	var back Account
	if unmarshalErr := back.UnmarshalText(text); unmarshalErr != nil {
		t.Fatalf("读回 %q 失败: %v", text, unmarshalErr)
	}
	if back != orig {
		t.Errorf("往返之后 = %q，期望 %q", back, orig)
	}

	// JSON 走的是同一对方法，所以日志、接口与库里的形态不会分叉。
	blob, err := json.Marshal(orig)
	if err != nil {
		t.Fatal(err)
	}
	var fromJSON Account
	if err := json.Unmarshal(blob, &fromJSON); err != nil {
		t.Fatalf("JSON 读回失败: %v", err)
	}
	if fromJSON != orig {
		t.Errorf("JSON 往返之后 = %q", fromJSON)
	}

	// 非法科目在读回时必须报错，而不是变成一个能用的值。
	var junk Account
	if err := junk.UnmarshalText([]byte("nonsense:1")); err == nil {
		t.Error("非法科目应当读不回来")
	}
}

// 零值科目是「忘了填」，与「填了一个合法的空科目」不同。String() 是空串，
// 它会写进 NOT NULL 的 text 列而不报错，所以必须能被识别出来。
func TestAccountZeroValue(t *testing.T) {
	var zero Account
	if !zero.IsZero() {
		t.Error("零值应当是零值")
	}
	if zero.String() != "" {
		t.Errorf("零值的字符串 = %q", zero.String())
	}
	if UserAccount(1).IsZero() {
		t.Error("合法科目不该被当成零值")
	}
}

func TestEntryTypesMatchDatabaseConstraint(t *testing.T) {
	// 这份清单必须与迁移文件 00001_init.sql 里 ledger_entries.entry_type
	// 的 CHECK 约束逐字一致。不一致的表现是写库时才报错，而那已经是
	// 一次失败的业务动作了。
	want := []EntryType{
		"topup", "consume", "refund", "settle",
		"payout_request", "payout_paid", "adjustment",
	}
	got := EntryTypes()
	if len(got) != len(want) {
		t.Fatalf("分录类型有 %d 种，期望 %d 种: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 种分录类型 = %q，期望 %q", i, got[i], want[i])
		}
		if !validEntryType(want[i]) {
			t.Errorf("%q 应当被判为合法", want[i])
		}
	}
	if validEntryType("gift") {
		t.Error("未列在约束里的取值不该通过校验")
	}
}

func TestAssetValidity(t *testing.T) {
	if !AssetCNY.Valid() {
		t.Error("CNY 应当是受支持的资产")
	}
	// 空资产**不**退回默认值：那会把一个「不知道该记哪种资产」的分录
	// 变成一笔人民币分录，而它在账面上与正常的完全一样。
	if Asset("").Valid() {
		t.Error("空资产不该通过校验")
	}
	if Asset("USDT").Valid() {
		// 接加密货币时先在 Asset 里加常量，再改这里。
		t.Error("未支持的资产不该通过校验")
	}
}

func TestTxnIDParse(t *testing.T) {
	valid := "3f2a1b4c-5d6e-4f70-8912-a3b4c5d6e7f8"
	got, err := ParseTxnID(valid)
	if err != nil {
		t.Fatalf("合法的 txn_id 被拒: %v", err)
	}
	if string(got) != valid {
		t.Errorf("解析结果 = %q", got)
	}

	bad := []string{
		"",
		"3f2a1b4c5d6e4f708912a3b4c5d6e7f8",     // 少了连字符
		"3f2a1b4c-5d6e-4f70-8912-a3b4c5d6e7f",  // 少一位
		"3F2A1B4C-5D6E-4F70-8912-A3B4C5D6E7F8", // 大写：与另一个是同一个交易，但字符串比较不等
		"3f2a1b4c_5d6e_4f70_8912_a3b4c5d6e7f8", // 位置对但分隔符不对
		"3f2a1b4c-5d6e4f70-8912-a3b4c5d6e7f8",  // 连字符位置不对
		"3f2a1b4c-5d6e-4f70-8912-a3b4c5d6e7g8", // 非十六进制字符
	}
	for _, s := range bad {
		if _, err := ParseTxnID(s); err == nil {
			t.Errorf("ParseTxnID(%q) 应当报错", s)
		} else if !errors.Is(err, ErrInvalidTxnID) {
			t.Errorf("ParseTxnID(%q) 的错误应当是 ErrInvalidTxnID，得到 %v", s, err)
		}
	}
}

func TestNewTxnIDIsWellFormedAndUnique(t *testing.T) {
	seen := make(map[TxnID]bool, 256)
	for i := 0; i < 256; i++ {
		id := NewTxnID()
		if _, err := ParseTxnID(string(id)); err != nil {
			t.Fatalf("生成的标识不合规范: %v (%q)", err, id)
		}
		// 版本位与变体位要置上：生成的字符串要能被任何标准 uuid
		// 解析器读回。
		if id[14] != '4' {
			t.Errorf("版本位 = %q，期望 4 (%q)", id[14], id)
		}
		if !strings.ContainsRune("89ab", rune(id[19])) {
			t.Errorf("变体位 = %q，期望 8/9/a/b (%q)", id[19], id)
		}
		if seen[id] {
			t.Fatalf("生成了重复的标识 %q", id)
		}
		seen[id] = true
	}
}

func TestEntryStringIsSingleLine(t *testing.T) {
	txn := TxnID("3f2a1b4c-5d6e-4f70-8912-a3b4c5d6e7f8")
	e := credit(txn, UserAccount(42), AssetCNY, money.MustParse("1.5"), EntryConsume)
	got := e.String()
	if strings.ContainsAny(got, "\r\n") {
		t.Errorf("摘要不该含换行: %q", got)
	}
	for _, want := range []string{string(txn), "consume", "user:42", "-1.50000000", "CNY"} {
		if !strings.Contains(got, want) {
			t.Errorf("摘要 %q 里少了 %q", got, want)
		}
	}
}
