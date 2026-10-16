// Package ledger 构造与校验复式记账的账本分录。
//
// 它做两件事，且只做这两件：
//
//  1. **构造**：按业务场景生成一组自洽的分录——同一 txn_id 内金额之和为零。
//  2. **校验与推导**：检查一组分录是否自洽，并从一组分录算出某科目的余额。
//
// 不做的事：**不查库、不发 HTTP、不改余额**。分录交给调用方去持久化，
// 余额快照（user_wallets.balance）由调用方在同一个事务里更新。这样
// 「分录取向对不对」可以脱离数据库单独测试，而它一旦错了，
// 表现是账目悄悄不平——不会报错，只会让对账任务在几天后报出一笔对不上的差额。
//
// # 数据库约束以哪一种为准
//
// 表结构里的取值（科目名单、entry_type、借贷符号）在本包里逐个钉死，
// 而不是留给调用方拼字符串。原因是账本的写入方不止一处：充值回调、
// 计费结算、退款、提现。每一处各拼一次字符串，就有一处会拼错，
// 而拼错的表现是「多出一个看起来像科目的字符串」，没有任何约束会拦住它——
// ledger_entries.account 是 text。
//
// 代价是数据库改了取值，这里要跟着改。收益正是这一点：改一处就够了，
// 而不是指望所有写入方同时记得。
package ledger

import (
	"fmt"
	"strconv"
	"strings"
)

// Asset 是资产种类。
//
// 目前只有 CNY。做成独立的类型而不是裸 string，是因为分录、钱包快照、
// 支付订单三张表都有 asset 列，而「按资产分开记账」这个不变量必须由
// 类型来兜——把 USDT 的金额记到 CNY 的余额上，账面上看不出来。
type Asset string

// AssetCNY 是本币。其它资产（加密货币）接进来时在这里补。
const AssetCNY Asset = "CNY"

// valid 判断资产是否受支持。
//
// 空资产视为不合法，**不**悄悄退回 CNY：调用方漏填 asset 时，
// 退默认值会让一个「不知道该记哪种资产」的分录变成一笔人民币分录，
// 而它在账面上与正常的完全一样。宁可在这里报错。
func (a Asset) valid() bool { return a == AssetCNY }

// AccountType 是会计科目的大类。
//
// 它决定两件事：科目的字符串前缀，以及余额的正负代表什么。
// 与数据库注释里的科目名单一一对应，不多不少。
type AccountType string

const (
	// AccountUser 是用户钱包。正数是平台欠用户的钱（他的可用余额），
	// 负数是用户欠平台的——那说明某处少扣了钱，应当被当作异常。
	// 汇总后的 balance 不允许为负，见 balance.go。
	AccountUser AccountType = "user"
	// AccountMerchantPayable 是应付商户的分成。正数是平台欠商户的钱，
	// 即「这些消费里有这么多是要分给商户的」。
	AccountMerchantPayable AccountType = "merchant_payable"
	// AccountPlatformRevenue 是平台的钱。它没有冒号后缀——三个单例科目之一
	//（另见 upstream_cost 与 reconcile_difference，见 needsID）。
	// 正数是平台收到的，负数是平台付出的——退款、以及由平台承担的
	// 通道手续费都记在它上面。
	AccountPlatformRevenue AccountType = "platform_revenue"
	// AccountUpstreamCost 是付给上游的成本，也包括支付通道的手续费。
	// 与平台收入分开记，是为了让「这一单赚了多少」可以逐单算出来，
	// 而不是靠两边的汇总相减。
	AccountUpstreamCost AccountType = "upstream_cost"
	// AccountPaymentGateway 是通道替我们持有的钱。充值时是负的
	// （我们有钱在通道那里），打款出去时变正（通道替我们垫了钱），
	// 结清后为零。
	AccountPaymentGateway AccountType = "payment_gateway"
	// AccountPayout 是已申请未打款的钱。与 merchant_payable 分开：
	// 商户分成转成提现单时，钱从「他的分成」变成「这条提现单的待付」，
	// 混在一个科目里就分不清商户还能不能再申请提现。
	AccountPayout AccountType = "payout"
	// AccountReconcileDifference 是对账差额的归集科目（单例）。
	//
	// 对账修正与人工调账的区别正在于「谁认下这笔差额」：人工调账的对家是
	// platform_revenue，因为那是人判断过的、平台认下的收支；而对账是被动
	// 发现快照不对，差额的**来路不明**——它可能是一处代码绕过了 store，
	// 也可能是预留算错。把它记为平台收入，等于让一笔没人解释过的钱
	// 混进经营收入里，而收入是要被看、被汇总、被用来定价的。
	//
	// 所以单独归集：贷方为负表示账本低于快照、平台补上了差额；借方为正
	// 表示账本高于快照、多出来的那笔被冲回。这个科目的余额是**待查项**，
	// 查清来路之后再写一笔人工调账把它转进 platform_revenue（那时差额
	// 已经被解释了）。长期非零本身就是一条应当被看见的信号。
	AccountReconcileDifference AccountType = "reconcile_difference"
)

// validAccountType 判断大类是否受支持。
func (t AccountType) valid() bool {
	switch t {
	case AccountUser, AccountMerchantPayable, AccountPlatformRevenue,
		AccountUpstreamCost, AccountPaymentGateway, AccountPayout,
		AccountReconcileDifference:
		return true
	}
	return false
}

// needsID 判断大类是否必须带一个 id 后缀。
//
// 只有 platform_revenue、upstream_cost 与 reconcile_difference 是单例科目
// ——它们描述的是「平台」「上游」「对账差额」这三个整体，没有区分后缀的必要
// （上游成本若要按渠道拆，那是对账口径的变更，不该在这里悄悄支持）。
func (t AccountType) needsID() bool {
	switch t {
	case AccountPlatformRevenue, AccountUpstreamCost, AccountReconcileDifference:
		return false
	}
	return true
}

// Account 是一个会计科目，形如 `user:42`。
//
// 字段不导出，只能由下面几个构造函数或 ParseAccount 得到，于是
// 「字符串拼错」这件事在类型层面就被挡住了。零值不是合法科目，
// 必须用 ParseAccount 或构造函数——IsZero() 专门用来识别这种漏填。
//
// # 方向与符号的惯例
//
// 借为正、贷为负，与数据库的注释一致。落在各科目上：
//
//	user                    正 = 平台欠用户的钱，负 = 用户欠平台
//	merchant_payable        正 = 平台欠商户的分成
//	platform_revenue        正 = 平台收到的钱，负 = 平台付出的钱
//	                        （收入、退款、以及由平台承担的通道手续费都走它）
//	upstream_cost           正 = 上游成本
//	payment_gateway         负 = 通道替我们持有的钱；净额为零表示通道已结清
//	payout                  正 = 已申请未打款的待付
//	reconcile_difference    正 = 账本高于快照的那部分被冲回，负 = 平台补上的差额
//
// 每一项正数的含义都不一样——「正数就是有钱」在这里不成立，
// 而 payment_gateway 的资产方向甚至是负的。这条约定来自各场景的推导
// （见 transactions.go 里每个场景的分录表），不是通用记账规则。
//
// # 为什么要有类型
//
// 科目的字符串要在账本里比对与汇总（对账就是把同一个科目加起来）。
// 各自拼字符串就意味着有一处会拼错，而 `user :42` 这种错不会让
// 任何约束报错——账本里只是多了一个谁也看不懂的科目。类型的代价是
// 数据库改了取值这里要跟着改，而收益正是这一点：改一处就够。
type Account struct {
	raw string
}

// ID 是科目后缀里的标识，单例科目为空串。
//
// 返回字符串而不是 int64：后缀在数据库里就是 text，且不同大类含义不同
// （user 是用户 id，payment_gateway 是通道名，payout 是提现单 id）。
// 在这里把它解释成整数，就等于替调用方决定了解释方式。
//
// 它只切字符串，不判断后缀能不能当用户 id——那件事只有一处答案，
// 在 UserIDFromAccount（方法形态是 UserID），且只对 user 科目成立。
func (a Account) ID() string {
	_, id := splitAccount(a.raw)
	return id
}

// Type 是科目的大类。对非法科目返回空串。
func (a Account) Type() AccountType {
	t, _ := splitAccount(a.raw)
	return t
}

// String 返回科目的规范写法。
func (a Account) String() string { return a.raw }

// IsZero 判断这是不是一个空科目。
//
// 存在的意义是区分「零值科目」与「合法科目」：零值的 String() 是空串，
// 它会写进数据库的一个 NOT NULL text 列而**不报错**——
// 一条 account 为空串的分录在账面上谁也看不懂。校验时用它拦下来。
func (a Account) IsZero() bool { return a.raw == "" }

// MarshalText 让 Account 可以被 SQL 参数编码与 JSON 序列化。
//
// 实现的是 TextMarshaler 而不是提供 String 的别名：encoding/json 与
// database/sql 的驱动都认这个接口，两边共用一条路径，不会出现
// 「日志里是对的、库里写错了」这种分叉。
func (a Account) MarshalText() ([]byte, error) { return []byte(a.raw), nil }

// UnmarshalText 从数据库或 JSON 读回科目，**经过完整校验**。
//
// 读回时也校验是有意的：读回来的字符串可能来自另一条写入路径、
// 一次人工修数、或者一个已经下线的旧版本。宽松解析会把一个非法科目
// 变成「能用的值」，而它的问题是真实存在的，只是被推迟到了对账时才发现。
func (a *Account) UnmarshalText(b []byte) error {
	parsed, err := ParseAccount(string(b))
	if err != nil {
		return err
	}
	*a = parsed
	return nil
}

// 科目字符串的分隔符。用冒号而不是下划线：下划线是标识符里的常见字符，
// 用下划线会让 `user_12` 这种拼法变得看似合法。
const accountSep = ":"

// UserAccount 是用户钱包科目。
func UserAccount(userID int64) Account {
	return Account{raw: buildAccount(AccountUser, strconv.FormatInt(userID, 10))}
}

// maxUserIDDigits 是 user 科目里用户 id 的十进制位数上限。
//
// 上限的来源不是「用户 id 不会有这么大」，而是**对账的 SQL 必须认得出来**：
// 它要把后缀转成 bigint，而超出 int64 的取值会让那个转换报错、整轮对账失败，
// 于是那样一个科目既进不了汇总、又不会走到畸形那一条路。能被写进来的写法
// 必须是**对账看得见**的写法，否则写入路径认、对账不认，余额就会被改错。
//
// 十八位最大是 10^18−1，落在 int64 里；而用户 id 来自 bigserial，
// 这个上限在现实中够不着。
const maxUserIDDigits = 18

// UserIDFromAccount 从 user 科目的原文里读出用户 id。
//
// 这是「一个科目属于哪个用户」在 Go 侧**唯一的一处**判定：ParseAccount 用它
// 决定写法是否规范，写路径（把分录的净影响加到钱包上）与对账的畸形科目报告
// 也用它。SQL 那边没法调用 Go，规则在 store 里再写一遍，两处必须一致——
// 两边不一致正是这条缺陷的成因。
//
// # 它比 ParseAccount 宽松，这是有意的
//
// 它认得旧写路径留下的写法（`user:042`、`user:+42`），因为那些钱当初**已经**
// 被算进那个用户的钱包了（旧写路径把后缀读成整数）。对账必须按同一个口径
// 把它们归回同一个用户，否则汇总出来的权威值偏小，对账会据此把一个**正确的**
// 余额改小。写法规范不规范是 ParseAccount 的判断，这里只回答「读成哪个 id」。
//
// 返回 false 表示读不出来：不是 user 科目、后缀为空、含非数字字符、带负号、
// 为 0、或者位数超出 maxUserIDDigits。这些写法上的钱不属于任何人的钱包，
// 由对账单独报出来。
//
// # 为什么 0 不算「读得出」
//
// 判据是「这笔钱落在了哪个钱包上」，不是「这个后缀能不能转成整数」。
// 0 号用户不存在（users.id 是 bigserial，从 1 起，user_wallets.user_id 与
// billing_requests.user_id 都有外键指过去），所以 `user:0` 的钱没有落在
// 任何钱包上——把 0 读出来会让对账报出与事实相反的话（说金额已按 user:0
// 归位），而事实是这笔钱谁也不属于。账本包其余部分也是同一个约定：
// Topup/Consume/Refund/… 全都拒绝 UserID <= 0。
func UserIDFromAccount(s string) (int64, bool) {
	t, suffix := splitAccount(s)
	if t != AccountUser {
		return 0, false
	}
	// 正号是旧写路径认过的（strconv.ParseInt 接受它），负号不是：
	// 负 id 的钱包行不可能存在（users.id 是 bigserial），那样的分录写不进库。
	digits := strings.TrimPrefix(suffix, "+")
	if digits == "" {
		return 0, false
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, false
		}
	}
	if len(strings.TrimLeft(digits, "0")) > maxUserIDDigits {
		return 0, false
	}
	userID, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, false
	}
	if userID <= 0 {
		// 走到这里只可能是全零后缀（`user:0`、`user:000`、`user:+0`）：
		// 负号在前面就被字符判断挡掉了。见上面「为什么 0 不算读得出」。
		return 0, false
	}
	return userID, true
}

// UserID 返回 user 科目的用户 id。
//
// 与 UserIDFromAccount 同一条判定，只是接的是已经构造好的科目。
// 写路径用它，于是「一笔分录算给哪个用户」只有一处答案。
//
// 与 ID 的分工：ID 只把字符串切开、不做任何解释（后缀在不同大类里含义不同），
// 这里解释成用户 id，并且只对 user 科目成立——读不出或不成立的写法一律报错，
// 不返回一个「大概是对的」的值。
func (a Account) UserID() (int64, error) {
	userID, ok := UserIDFromAccount(a.raw)
	if !ok {
		return 0, fmt.Errorf(
			"科目 %q 读不出用户 id（不是 user 科目，或后缀不是正整数：空、含非数字字符、带负号、为 0、超过 %d 位）",
			a.raw, maxUserIDDigits)
	}
	return userID, nil
}

// MerchantPayableAccount 是应付某商户分成的科目。
func MerchantPayableAccount(userID int64) Account {
	return Account{raw: buildAccount(AccountMerchantPayable, strconv.FormatInt(userID, 10))}
}

// PlatformRevenueAccount 是平台科目（单例）。它装的是平台的收支净额：
// 收入记借、退款与被平台承担的成本记贷。
//
// 命名为 ...Account 而不是 PlatformRevenue，是为了让调用点一眼看出
// 返回的是科目而不是金额——账本代码里同时有科目与金额两种值，
// 两者的名字都会叫「平台收入」，混起来读会看错。
func PlatformRevenueAccount() Account {
	return Account{raw: string(AccountPlatformRevenue)}
}

// UpstreamCostAccount 是上游成本科目（单例）。
func UpstreamCostAccount() Account {
	return Account{raw: string(AccountUpstreamCost)}
}

// ReconcileDifferenceAccount 是对账差额的归集科目（单例）。
//
// 命名与 PlatformRevenueAccount 同构，理由也相同：调用点要一眼看出
// 拿到的是科目而不是金额。
func ReconcileDifferenceAccount() Account {
	return Account{raw: string(AccountReconcileDifference)}
}

// PaymentGatewayAccount 是某个支付通道的科目，装的是通道替我们
// 持有的钱（充值时在贷方、打款出去时在借方）。
//
// 传入真实的通道标识（stripe / alipay / …）。空名字不报错但会被校验拦下：
// 构造函数保持「怎么给就怎么构造」，判断合不合法统一交给校验，
// 这样从数据库读回的记录与内存里构造的记录走同一条判断。
func PaymentGatewayAccount(name string) Account {
	return Account{raw: buildAccount(AccountPaymentGateway, name)}
}

// PayoutAccount 是一条提现单的待付科目：已申请、尚未打款的钱。
func PayoutAccount(payoutID int64) Account {
	return Account{raw: buildAccount(AccountPayout, strconv.FormatInt(payoutID, 10))}
}

// buildAccount 拼出科目字符串。空 id 退回单例写法：这是构造期的笔误，
// 而构造期 panic 会把一个「拼错字符串」升级成「服务起不来」，
// 非法形态留给 Verify 统一报出来。
func buildAccount(t AccountType, id string) string {
	if id == "" {
		return string(t)
	}
	return string(t) + accountSep + id
}

// ParseAccount 解析科目字符串并校验。
//
// 这是唯一一条「外部输入 → 合法科目」的通道：数据库读回的行、
// 手工构造的测试数据、其它服务的请求都从这里进。
//
// # 用户 id 只放行一种写法
//
// `user:42` 是规范的，`user:042` 与 `user:+42` 不是，会被拒。理由是账本里
// 的科目要按字符串汇总（对账就是把同一个科目加起来），而多种写法指向同一个
// 用户时，字符串比对看不见另外几种——汇总值偏小，对账会把正确的余额改小。
// 让写法只有一种，这条缝就不存在了。
func ParseAccount(s string) (Account, error) {
	t, id := splitAccount(s)

	if s == "" {
		// 单独一条分支：空科目的报错信息不该是「大类 "" 未知」，
		// 那会让人以为是取值写错了，而实际是整列漏填。
		return Account{}, fmt.Errorf("科目为空")
	}
	if !t.valid() {
		return Account{}, fmt.Errorf("科目 %q 的大类 %q 未知", s, t)
	}
	if t.needsID() {
		if id == "" {
			return Account{}, fmt.Errorf("科目 %q 缺少 id 后缀", s)
		}
		if strings.ContainsRune(id, ':') {
			// 冒号只能出现一次，否则 `user:a:b` 这类值的后缀含义无法确定，
			// 两个不同的字符串会被读成同一件事。
			return Account{}, fmt.Errorf("科目 %q 的后缀含多余的分隔符", s)
		}
	} else if id != "" {
		return Account{}, fmt.Errorf("科目 %q 不该带 id 后缀", s)
	}
	if err := checkAccountID(id); err != nil {
		return Account{}, err
	}
	if t == AccountUser {
		// 用户 id 的写法只放行一种：正整数，十进制，不带前导零与正号。
		//
		// 别的写法（`user:042`、`user:+42`）读得出同一个 id，但字符串不相等，
		// 而「按科目汇总」是字符串比对——汇总值因此偏小，对账会拿它去和快照
		// 比，把一个**正确的**余额改小。收窄在这里等于堵住新的写入；库里已经
		// 存在的那种写法由对账按同一个 id 归位（见 UserIDFromAccount）。
		//
		// 只收窄 user：它是唯一会被按字符串比对归属的大类。merchant_payable
		// 与 payout 的后缀不参与任何这样的比对，一起收窄是另一件事。
		//
		// 这里不另写 `userID > 0`：UserIDFromAccount 已经把 0 判成读不出
		// （见它的注释），两处各写一遍正是这条缺陷的成因。写法是否规范由
		// 「读出来的 id 再写回去是不是原文」决定，于是规范写法只有一种。
		userID, ok := UserIDFromAccount(s)
		if !ok || strconv.FormatInt(userID, 10) != id {
			return Account{}, fmt.Errorf(
				"科目 %q 的用户 id 不是规范写法：应为 user:<正整数>，十进制、无前导零、无正号", s)
		}
	}
	return Account{raw: s}, nil
}

// checkAccountID 校验后缀里能出现的字符。
//
// 目的是让科目字符串在日志与对账输出里可读、可比、可 grep。放行
// `.-_@` 是为了让通道名与第三方对账号这类标识能直接用。
func checkAccountID(id string) error {
	if id == "" {
		return nil
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '-', c == '_', c == '@':
		default:
			return fmt.Errorf("科目后缀 %q 含非法字符 %q", id, string(rune(c)))
		}
	}
	return nil
}

// splitAccount 把科目拆成大类与后缀。不校验，只做切分——
// 校验与切分分开，是为了让报错信息里能带上原始字符串的完整形态。
func splitAccount(s string) (AccountType, string) {
	t, id, _ := strings.Cut(s, accountSep)
	return AccountType(t), id
}
