package ledger

import (
	"encoding/json"
	"errors"
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/migrate"
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
		{PrepaidAccount(11), "prepaid:11", AccountPrepaid, "11"},
		{CarryoverAccount(11), "carryover:11", AccountCarryover, "11"},
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
		"prepaid",                // 同上：缺订阅 id
		"carryover",              // 同上
		"carryover:007",          // 前导零：同一订阅会被拆成两个科目
		"user:",                  // 后缀为空
		"user:1:2",               // 多余分隔符：后缀含义无法确定
		"prepaid:1:2",            // 同上
		"prepaid:",               // 后缀为空
		"bogus:1",                // 大类未知
		"platform_revenue:x",     // 单例科目带了后缀
		"upstream_cost:1",        // 同上
		"reconcile_difference:1", // 同上
		"user:1 2",               // 后缀含空格
		"user:1\n2",              // 后缀含换行（会污染对账输出）
		"user_id:1",              // 下划线写法看似合法，实际不是科目
		" user:1",                // 前后空白
		"user:1 ",                //
		// 字符集对每个大类是同一套，而上面几条都写在 user 上：user 的非法
		// 写法后面还有一道「写法是否规范」会报错，别的大类没有那一道——
		// 下面几条走的正是别的大类，挡住它们的只有这一层判断。
		"merchant_payable:1 2",    // 空格
		"merchant_payable:a!b",    // 标点
		"merchant_payable:a\x00b", // NUL：写进日志与对账输出会把它截断
		"merchant_payable:aé",     // 多字节
		"payment_gateway:a/b",     // 斜杠
		"prepaid:1 2",             // 空格
		"prepaid:a!b",             // 标点
		"prepaid:a\x00b",          // NUL
		"prepaid:aé",              // 多字节
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

// 订阅 id 与用户 id 一样只放行一种写法。
//
// prepaid 的余额按科目原文精确汇总（见 store 的 accountBalanceInTx 与
// internal/plans 里那份同口径的读法），而那个余额就是「这个订阅还剩多少面值」。
// 同一订阅写成 `prepaid:011` 与 `prepaid:11` 会把余额拆成两笔，两边看起来都
// 像「还剩一点」，合起来才是真的剩多少。
func TestParseAccountRejectsNonCanonicalPrepaidID(t *testing.T) {
	bad := []string{
		"prepaid:0",                   // 订阅 id 从 1 起（user_plans.id 是 bigserial）
		"prepaid:00",                  //
		"prepaid:+0",                  //
		"prepaid:011",                 // 前导零
		"prepaid:+11",                 // 正号
		"prepaid:-1",                  // 负 id 不可能存在
		"prepaid:abc",                 // 后缀不是整数
		"prepaid:1a",                  // 首字符是数字，后面混了非数字
		"prepaid:1234567890123456789", // 十九位：超出上限
	}
	for _, s := range bad {
		if _, err := ParseAccount(s); err == nil {
			t.Errorf("ParseAccount(%q) 应当报错", s)
		}
	}

	good := []struct {
		s    string
		want AccountType
	}{
		{"prepaid:1", AccountPrepaid},
		{"prepaid:9", AccountPrepaid}, // 首字符取上界那一格
		{"prepaid:11", AccountPrepaid},
		{"prepaid:123456789012345678", AccountPrepaid}, // 十八位：上限之内
		{"carryover:1", AccountCarryover},
	}
	for _, tc := range good {
		acct, err := ParseAccount(tc.s)
		if err != nil {
			t.Errorf("ParseAccount(%q) 不该报错: %v", tc.s, err)
			continue
		}
		if acct.Type() != tc.want {
			t.Errorf("ParseAccount(%q) 的大类 = %q，期望 %q", tc.s, acct.Type(), tc.want)
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
		// 重复的正号：`TrimPrefix` 只去掉一个，剩下那个还会被 ParseInt 当成符号，
		// 于是 `user:++1` 会被读成用户 1。这种写法写不进账本（写路径只放行规范
		// 写法），只有被人直接改库才会有——而对账一旦把它读成用户 1，就会把这份
		// 谁都不属于的钱算进用户 1 的权威余额，再据此去改他的钱包。
		{"user:++1", 0, false},
		{"user:++9", 0, false},
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
		{"prepaid:42", 0, false},
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

// TestEntryTypesMatchDatabaseConstraint 把 EntryTypes() 与迁移文件里的
// CHECK 约束逐条比对。
//
// 比的是**迁移文件本身**（goose 的 embed.FS），不是一份抄过来的字面量：
// 抄一份的话，迁移里加了一个类型而 Go 这边没加，用例照样通过——而它承诺的
// 恰恰是「与迁移一致」。对不上的两种表现都是失败的业务动作：
// 迁移里有而 Go 里没有 → 写库时才报错；Go 里有而迁移里没有 → 合法交易被判非法。
func TestEntryTypesMatchDatabaseConstraint(t *testing.T) {
	got := EntryTypes()
	if len(got) == 0 {
		t.Fatal("EntryTypes() 是空的，这条用例什么都没测")
	}
	known := make(map[EntryType]bool, len(got))
	for _, e := range got {
		if known[e] {
			t.Errorf("EntryTypes() 里 %q 出现了两次", e)
		}
		known[e] = true
	}

	inDB := make([]EntryType, 0, len(got))
	for _, v := range sqlCheckInValues(t, "entry_type") {
		inDB = append(inDB, EntryType(v))
	}
	inDBSet := make(map[EntryType]bool, len(inDB))
	for _, e := range inDB {
		inDBSet[e] = true
	}
	for _, e := range inDB {
		if !known[e] {
			t.Errorf("迁移的 CHECK 约束里有 %q，EntryTypes() 里没有", e)
		}
	}
	for _, e := range got {
		if !inDBSet[e] {
			t.Errorf("EntryTypes() 里有 %q，迁移的 CHECK 约束里没有", e)
		}
	}

	// validEntryType 是生产里真正的判据（checkDirection 走它），它认的每一个
	// 都必须在清单里，清单里的每一个它也都得认——只对一边，另一边的漂移就
	// 会表现成「合法分录写不进去」或者「不该写进去的写进去了」。
	for _, e := range got {
		if !validEntryType(e) {
			t.Errorf("%q 在清单里，validEntryType 却不认它", e)
		}
	}
	for e := range entryDirections {
		if !known[e] {
			t.Errorf("validEntryType 认 %q，EntryTypes() 里却没有它", e)
		}
	}
	if validEntryType("gift") {
		t.Error("未列在约束里的取值不该通过校验")
	}
}

// sqlCheckInValues 从迁移 SQL 里读出 `<column> ... CHECK (<column> IN (...))`
// 的字面量清单。
//
// 迁移按文件名顺序执行，所以同一个约束在后面的文件里重新声明时以后者为准：
// 取最后一个匹配，而不是第一个。一条都没读到直接失败——「没扫到」与
// 「约束里本来就是空的」在比对里长得一样，而前者意味着这条用例什么都没测。
func sqlCheckInValues(t *testing.T, column string) []string {
	t.Helper()

	fsys := migrate.FS()
	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		t.Fatalf("枚举迁移文件: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("迁移目录里一个 .sql 都没有，检查范围是空的")
	}
	sort.Strings(names)

	check := regexp.MustCompile(`(?s)CHECK\s*\(\s*` + regexp.QuoteMeta(column) + `\s+IN\s*\(([^)]*)\)`)
	literal := regexp.MustCompile(`'([^']*)'`)

	var found []string
	var foundIn string
	for _, name := range names {
		b, err := fs.ReadFile(fsys, name)
		if err != nil {
			t.Fatalf("读迁移 %s: %v", name, err)
		}
		m := check.FindSubmatch(b)
		if m == nil {
			continue
		}
		foundIn = name
		found = found[:0]
		for _, lit := range literal.FindAllSubmatch(m[1], -1) {
			found = append(found, string(lit[1]))
		}
	}
	if foundIn == "" {
		t.Fatalf("扫了 %d 个迁移文件，没找到 %s 的 CHECK (... IN (...)) 约束", len(names), column)
	}
	if len(found) == 0 {
		t.Fatalf("%s 里 %s 的 CHECK 约束一个取值都没读到", foundIn, column)
	}
	return found
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
