package ledger

import (
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/money"
)

// EntryType 是分录的业务类型，取值与数据库的 CHECK 约束一致。
//
// 它用在校验里（见 checkDirection），而校验作用在外部构造、从数据库
// 读回的分录上——所以取值必须与迁移文件逐字一致。对不上的表现是
// 「合法交易被拒」或「写库时才报错」，两者都是失败的业务动作。
type EntryType string

const (
	// EntryTopup 是用户充值到账。网关的应收与用户余额同时增加。
	EntryTopup EntryType = "topup"
	// EntryConsume 是一次请求的消费：用户余额减少，商户分成、平台收入、
	// 上游成本随之增加。
	EntryConsume EntryType = "consume"
	// EntryRefund 是退款，也是**修正**。上游失败退还用户的钱、对账发现
	// 多扣后补退、人工补偿都走它。修正也是写新分录，而不是改旧分录——
	// 账本只追加。
	EntryRefund EntryType = "refund"
	// EntrySettle 是平台侧清算商户分成，把钱转入一条提现单。
	EntrySettle EntryType = "settle"
	// EntryPayoutRequest 是商户侧发起提现，账面形状与 settle 相同。
	// 两者分开是为了让「谁发起的」这件事在账面上可见——审计时
	// 「平台主动清算」与「商户申请后审核通过」是两个不同的问题。
	EntryPayoutRequest EntryType = "payout_request"
	// EntryPayoutPaid 是提现打款完成：这条提现单的应付核销，钱离开平台。
	EntryPayoutPaid EntryType = "payout_paid"
	// EntryPayoutReject 是提现申请被驳回：这条提现单的待付核销，钱回到
	// 商户的分成上。它与 payout_request 是**互相取消**的两笔，所以两档
	// 分开：账面上要能回答「这条待付是打款核销的还是审批驳回的」，
	// 而这两种处置对账的对象不同（通道账单 vs 审批记录）。
	EntryPayoutReject EntryType = "payout_reject"
	// EntryPlanGrant 是运营发放一个额度包订阅：订阅的面值记进
	// prepaid:<订阅 id>，平台收入相应减少。
	//
	// 它是 plan_void 的镜像：两者是同一个订阅生命周期的两端，而消费走的是
	// 既有的 consume（只差用户侧科目，见 Consume）。
	EntryPlanGrant EntryType = "plan_grant"
	// EntryPlanVoid 是额度包订阅作废——到期未用完，或运营提前作废。
	// 剩余面值里没结转的那一部分，从 prepaid:<订阅 id> 转回平台收入。
	//
	// 到期与人工作废共用一档，但两者在账上不再完全等同：只有真的到期才会多出
	// 一笔 plan_carry（按比例留下的那部分），人工作废整笔走这里。区别记在订阅
	// 的状态上（到期 / 人工作废），金额上的差别由有没有 plan_carry 那一档体现。
	EntryPlanVoid EntryType = "plan_void"
	// EntryPlanCarry 是额度包订阅**期末结转**：到期时按比例留下的那部分面值，
	// 从 prepaid:<源订阅 id> 转到 carryover:<源订阅 id>，等用户下次领包时并入。
	//
	// 它与 plan_void 是同一次期末处置的两半：结转出去的那一份继续欠着用户，
	// 没结转的（比例之外的、以及向下取整的余数）由 plan_void 归平台。两笔各自
	// 是一档类型，账面上才分得清「这份额度为什么还留着」与「为什么没了」。
	EntryPlanCarry EntryType = "plan_carry"
	// EntryPlanCarryMerge 是结转额度**并入新订阅**：用户领到新包时，把
	// carryover:<源订阅 id> 上的余额转到 prepaid:<新订阅 id>。
	//
	// 它不改变平台欠用户的总数，只是把「待并入」变成「这个新订阅能花的」。
	// 与 plan_carry 分开记，是因为两者的触发时机与对家都不同：前者在到期，
	// 对家是 prepaid；后者在发放，对家是另一个 prepaid。
	EntryPlanCarryMerge EntryType = "plan_carry_merge"
	// EntryPlanPurchase 是用户**自己买**一个额度包：付款到账时，面值记进
	// prepaid:<订阅 id>，对方科目是支付通道，而不是平台收入。
	//
	// 它与 plan_grant 的科目不同，所以不合并成一档：赠送是平台让出一份收入
	//（贷 platform_revenue），售卖是钱真的从通道进来（贷 payment_gateway）。
	// 账面上要能回答「这份额度是送的还是卖的」，收入确认的口径正落在这个区别上。
	EntryPlanPurchase EntryType = "plan_purchase"
	// EntryAdjustment 是人工调账。它不参与任何业务推导，
	// 只在必须手工修正账目时使用，且强制要求写备注。
	EntryAdjustment EntryType = "adjustment"
)

// validEntryType 判断类型是否受支持。
func validEntryType(t EntryType) bool {
	_, ok := entryDirections[t]
	return ok
}

// EntryTypes 返回全部受支持的分录类型。
//
// 目前只有测试在调它。它本来是给配置校验与界面枚举用的——两者都还没有
// 调用方；生产里的类型校验走的是 validEntryType。
//
// 它与迁移文件里 ledger_entries.entry_type 的 CHECK 约束由
// TestEntryTypesMatchDatabaseConstraint 逐条比对（那条用例真的读迁移文件），
// 所以在这里加一个类型而没改迁移、或者反过来，都会让用例挂。
func EntryTypes() []EntryType {
	return []EntryType{
		EntryTopup, EntryConsume, EntryRefund, EntrySettle,
		EntryPayoutRequest, EntryPayoutPaid, EntryPayoutReject,
		EntryPlanGrant, EntryPlanVoid, EntryPlanCarry, EntryPlanCarryMerge,
		EntryPlanPurchase,
		EntryAdjustment,
	}
}

// signMask 是一位宽的符号集合，用来表述「这个科目在这类交易里只能借、
// 只能贷，还是两者皆可」。
type signMask uint8

const (
	// signDebit 表示只允许借（金额为正）。
	signDebit signMask = 1 << iota
	// signCredit 表示只允许贷（金额为负）。
	signCredit
	// signBoth 是两个方向都允许。
	signBoth = signDebit | signCredit
)

func (m signMask) allows(amount money.Amount) bool {
	if amount.IsPositive() {
		return m&signDebit != 0
	}
	return m&signCredit != 0
}

func (m signMask) String() string {
	switch m {
	case signDebit:
		return "借方（正）"
	case signCredit:
		return "贷方（负）"
	}
	return "双方"
}

// signBoth 目前没有任何科目用到。留着它是因为它定义了 signMask 这个
// 概念的上界（一个方向的集合），删掉它会让下面的表看起来只剩两个互斥值，
// 从而诱导后来者把「双向」当成「不填」——而不填在这里意味着「报错」。
var _ = signBoth

// entryDirections 钉住「哪类交易里，哪个科目只能记在哪个方向」。
//
// # 它挡的是什么
//
// 方向写反**不会**让同一 txn 内的和不为零——另一半会跟着一起反，
// 于是求和的校验完全看不见它。而它的后果是余额从一个应该非负的科目里
// 漏出去：把 consume 的用户分录写成借方，用户每消费一次余额就多一次。
//
// 所以方向必须单独查，而且要**按交易类型**查。这里的取值来自各场景的
// 业务定义，不是记账学的通用规则，改动它等于改业务。
//
// # 一张不在表里的组合为什么直接报错
//
// 与「放行未列举的组合」相比，报错更保守：一个新出现的组合要么是本表的
// 疏漏，要么是本表的**故意**——两种情况都该有人看一眼再放行。
// 放行的代价是这类错误悄无声息地成立，而它只会在对账时表现为
// 「某个科目的余额方向不对」，那时已经很难回溯到是哪一笔。
//
// # 这里查不了的
//
// 「余额非负」（user 科目）是**汇总之后**的性质，不是单条分录的性质：
// 一条 consume 的贷方分录本身是合法负值，是它与历史余额相加之后
// 才知道有没有透支。那需要期初余额，而本包不查库。调用方拿 BalanceOf
// 的结果自行判断。
var entryDirections = map[EntryType]map[AccountType]signMask{
	EntryTopup: {
		AccountUser:            signDebit,  // 平台欠用户的钱增加
		AccountPaymentGateway:  signCredit, // 通道持有的我们的钱增加
		AccountUpstreamCost:    signDebit,  // 通道手续费是成本
		AccountPlatformRevenue: signCredit, // 手续费由平台承担：平台科目减少
	},
	EntryConsume: {
		AccountUser:            signCredit, // 扣用户的钱
		AccountPrepaid:         signCredit, // 从额度包的面值里扣
		AccountMerchantPayable: signDebit,  // 欠商户的分成增加
		AccountPlatformRevenue: signDebit,  // 平台收入增加
		AccountUpstreamCost:    signDebit,  // 上游成本增加
	},
	EntryRefund: {
		AccountUser:            signDebit,  // 退回用户
		AccountMerchantPayable: signCredit, // 分成冲回
		AccountPlatformRevenue: signCredit, // 收入冲回
		AccountUpstreamCost:    signCredit, // 上游成本冲回
	},
	EntrySettle: {
		AccountMerchantPayable: signCredit, // 核销欠商户的分成
		AccountPayout:          signDebit,  // 转为这条提现单的待付
	},
	EntryPayoutRequest: {
		AccountMerchantPayable: signCredit,
		AccountPayout:          signDebit,
	},
	EntryPayoutPaid: {
		AccountPayout:         signCredit, // 这笔待付核销
		AccountPaymentGateway: signDebit,  // 通道持有的我们的钱减少
	},
	EntryPayoutReject: {
		AccountMerchantPayable: signDebit,  // 欠商户的分成恢复
		AccountPayout:          signCredit, // 这条待付核销
	},
	// 额度包的发放与作废：prepaid 是「平台还欠用户的预存额度」，正的余额
	// 表示这个订阅还剩这么多面值。发放只让它变大、作废只让它变小；prepaid
	// 够不够扣由选源那一侧判断（第 2 步），**本包不查余额**——同一档 consume
	// 也会贷 prepaid，汇总之后是否透支不在这张表的判据里。
	EntryPlanGrant: {
		AccountPrepaid:         signDebit,  // 平台欠用户的预存额度增加
		AccountPlatformRevenue: signCredit, // 平台让出这一份收入
	},
	EntryPlanVoid: {
		AccountPrepaid:         signCredit, // 剩下的面值不再可用
		AccountPlatformRevenue: signDebit,  // 未用完的部分归平台
	},
	// 结转：prepaid 变小、carryover 变大，两边是同一个源订阅。carryover 只在
	// 这一档里变正、只在并入那一档里变负，所以「它现在是正的」就等于「还有
	// 额度没并进任何新包」——这个判据就是并入的闸门（见 internal/plans）。
	EntryPlanCarry: {
		AccountPrepaid:   signCredit, // 源订阅的面值少了这一份
		AccountCarryover: signDebit,  // 平台欠用户、待并入的额度多了这一份
	},
	// 并入：carryover 清零，prepaid:<新订阅> 变大。可以有多个源（每个源一条
	// 贷方分录），目标只有一条借方——它们之和为零，由 Verify 对账。
	EntryPlanCarryMerge: {
		AccountCarryover: signCredit, // 待并入的额度转出去了
		AccountPrepaid:   signDebit,  // 新订阅的面值多了这一份
	},
	// 售卖：prepaid 变大、通道科目变大，方向与充值那一档逐字相同（钱从通道
	// 进来，平台欠用户一份等值的额度）。手续费也是成本，由平台承担。
	EntryPlanPurchase: {
		AccountPrepaid:         signDebit,  // 平台欠用户的预存额度增加
		AccountPaymentGateway:  signCredit, // 通道收到的钱增加
		AccountUpstreamCost:    signDebit,  // 通道手续费是成本
		AccountPlatformRevenue: signCredit, // 手续费由平台承担：平台科目减少
	},
	// 人工调账是唯一的开口：它本就是要修正别处算错的余额，
	// 所以科目与方向都不设限。代价是它不能被自动推导，
	// 于是强制要求备注（见 Adjustment）。
	EntryAdjustment: {},
}

// checkDirection 检查一条分录的符号与科目是否与它的交易类型相符。
func checkDirection(t EntryType, a Account, amount money.Amount) error {
	dirs, ok := entryDirections[t]
	if !ok {
		return fmt.Errorf("分录类型 %q 未知", t)
	}
	if t == EntryAdjustment {
		return nil
	}
	allowed, ok := dirs[a.Type()]
	if !ok {
		return fmt.Errorf("在 %s 交易里不该出现 %s 科目：方向约定未定义", t, a.Type())
	}
	if !allowed.allows(amount) {
		return fmt.Errorf("在 %s 交易里 %s 科目只能是%s，实际金额为 %s",
			t, a.Type(), allowed, amount)
	}
	return nil
}

// TxnID 是一次交易的标识，对应数据库里的 uuid。
//
// 表示成 36 字符的规范小写文本，而不是字节数组：它会被日志、对账脚本、
// 客服界面直接使用，而 Postgres 的 uuid 与这套文本互转不需要额外约定。
// 文本形态也是这里唯一的真值——只按 txn_id 查库查不到「生成了但一条
// 分录都没写进去」的交易（那种交易在库里没有任何行），所以一个
// 独立保存的 id 是必需的。
type TxnID string

// txnIDLen 是规范文本的长度：32 个十六进制字符加 4 个连字符。
const txnIDLen = 36

// ErrInvalidTxnID 表示交易标识不合规范。
var ErrInvalidTxnID = errors.New("交易标识不合规范")

// ParseTxnID 校验并规范化一个交易标识。
//
// 只接受规范小写形态，**不**顺手把大写转成小写：两个大小写不同的 id
// 指的是同一个交易，而按字符串比较它们是两个。宽松解析会让
// 「同一组分录被写成两个交易」这类错误悄悄成立——结果是一组
// 永远不平的分录，却找不到是哪一笔。
func ParseTxnID(s string) (TxnID, error) {
	if s == "" {
		return "", fmt.Errorf("%w: 为空", ErrInvalidTxnID)
	}
	if len(s) != txnIDLen {
		return "", fmt.Errorf("%w: %q 长度为 %d，期望 %d", ErrInvalidTxnID, s, len(s), txnIDLen)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '-':
			// 连字符只出现在固定的四个位置（8-4-4-4-12）。
			// 这条检查是为了让 id 在任何标准 uuid 解析器下都能读回。
			if i != 8 && i != 13 && i != 18 && i != 23 {
				return "", fmt.Errorf("%w: %q 的连字符位置不对", ErrInvalidTxnID, s)
			}
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
		default:
			return "", fmt.Errorf("%w: %q 含非法字符 %q", ErrInvalidTxnID, s, string(rune(c)))
		}
	}
	return TxnID(s), nil
}

// NewTxnID 生成一个新的交易标识。
//
// 随机而不递增：账本的 txn_id 要能安全地出现在 URL 与客服界面里，
// 递增值会泄露「平台一共做了多少笔交易」这个经营数据。
//
// 熵源不可用时 panic 而不是退回一个可预测的标识：两个交易撞上同一个 id
// 会把两组分录混成一组，而它们的和通常不为零——那是账本里最难查的
// 一类错误，还不如在这里立刻停下来。
func NewTxnID() TxnID {
	var b [16]byte
	// crypto/rand.Read 自 Go 1.24 起永不返回错误（系统熵源失败即崩溃）。
	// 分支留着是为了在旧工具链上也有确定行为，而不是静默拿到全零字节。
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("ledger: 无法生成交易标识: %v", err))
	}
	// 置版本位与变体位：这样生成的字符串能被任何标准 uuid 解析器读回，
	// 将来把 txn_id 交给外部系统时不必解释一句「我们的 uuid 不走标准」。
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80

	const hexDigits = "0123456789abcdef"
	var out [txnIDLen]byte
	j := 0
	for i := 0; i < len(b); i++ {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			out[j] = '-'
			j++
		}
		out[j] = hexDigits[b[i]>>4]
		out[j+1] = hexDigits[b[i]&0x0f]
		j += 2
	}
	return TxnID(out[:])
}

// Entry 是一条账本分录。
//
// 字段与 ledger_entries 表一一对应，包括两个可空外键：它们不是附带信息，
// 而是追溯争议时唯一的线索。少了 BillingRequestID，一笔扣费就关联不到
// 具体是哪次请求。
type Entry struct {
	// TxnID 是这次交易的标识，一组分录共享它。
	TxnID TxnID
	// Account 是会计科目，只能由构造函数或 ParseAccount 产生。
	Account Account
	// Asset 是资产种类。
	Asset Asset
	// Amount 是金额，**借为正、贷为负**。
	Amount money.Amount
	// EntryType 是业务类型。
	EntryType EntryType

	// BillingRequestID 关联计费请求，消费与退款用。
	// 空表示这次交易不产生于某次具体请求（充值、提现、人工调账）。
	BillingRequestID *int64
	// PayoutID 关联提现单，提现相关分录用。
	PayoutID *int64

	// Memo 是给人看的备注，应当写「为什么」——例如一次 adjustment
	// 的原因或上游失败的错误码，而不是复述金额。
	Memo string
}

// debit 返回一条金额取正的借方分录。
//
// 刻意不取绝对值：调用方若传进来一个负金额，说明它在上面某一层就
// 已经把方向搞反了，取绝对值会把那个错误吃掉。留在账面上才查得出来，
// 而且价格已经由构造函数保证为正（见 validateCommon）。
func debit(txn TxnID, acct Account, asset Asset, amount money.Amount, t EntryType) Entry {
	return Entry{TxnID: txn, Account: acct, Asset: asset, Amount: amount, EntryType: t}
}

// credit 返回一条金额取负的贷方分录。
func credit(txn TxnID, acct Account, asset Asset, amount money.Amount, t EntryType) Entry {
	return Entry{TxnID: txn, Account: acct, Asset: asset, Amount: amount.Neg(), EntryType: t}
}

// withBillingRequest 挂上计费请求的关联。
func (e Entry) withBillingRequest(id int64) Entry {
	e.BillingRequestID = &id
	return e
}

// withPayout 挂上提现单的关联。
func (e Entry) withPayout(id int64) Entry {
	e.PayoutID = &id
	return e
}

// withMemo 挂上备注。备注已由调用方清洗（见 sanitizeMemo）。
func (e Entry) withMemo(s string) Entry {
	e.Memo = s
	return e
}

// String 输出一条单行摘要。
//
// 实现它而不是让默认格式化处理：Entry 里装着金额与科目，两者各有自己的
// 字符串形态，默认的 %v 会把它们逐字段摊开成多行结构体。日志与对账要的是
// 一行一条，而实现 Stringer 同时挡住了「顺手 %v 打出整个结构体」这件事。
func (e Entry) String() string {
	return fmt.Sprintf("%s %s %s %s %s", e.TxnID, e.EntryType, e.Account, e.Amount, e.Asset)
}

// Transaction 是一组自洽的分录，共享一个 txn_id。
//
// 它是账本的最小写入单位：库里写了一半的分录等于账不平，所以调用方必须
// 在一个事务里把整组写进去。本包不碰数据库，但 Denormalize() 产出的
// []Entry 天然是「要么全写、要么全不写」的形状。
type Transaction struct {
	// TxnID 是交易标识。空值会在 Verify 里被拦下——一组分录没有归属
	// 就无法追溯，也无法与另一组区分。
	TxnID TxnID
	// CreatedAt 是这次交易的发生时间。零值表示交给数据库的 DEFAULT now()，
	// 本包不做任何时间上的补写。
	CreatedAt time.Time
	// Entries 是这次交易的全部分录。
	Entries []Entry
	// Kind 记录构造这次交易时用的场景，仅用于诊断与日志，不写库：
	// 每条分录自己的 entry_type 已经能说明业务类型。
	Kind EntryType
}

// Verify 校验一组分录是否自洽。
//
// 核心不变量：**同一 txn_id 内所有分录金额之和为零**。除此之外还检查
// 科目与类型的合法性、金额符号与业务方向的一致性、以及同一交易内
// 是否混进了别的交易的分录。
//
// 它必须能挡住外部构造的、从数据库读回的一组分录——那才是真正需要
// 校验的地方。内存里由构造函数产出的交易已经自检过一次（见 newTxn），
// 而库里读回的行可能来自任何写入路径。
//
// 校验失败一律返回错误，**绝不自动补一笔调平分录**：差额说明上游的
// 构造逻辑有 bug，补一笔会让代码继续跑、账面继续错，只是错的地方
// 从「不平」变成「一笔来路不明的分录」，而后者更查不出来。
func (t Transaction) Verify() error {
	if t.TxnID == "" {
		return errors.New("交易缺少 txn_id：一组分录没有归属就无法追溯")
	}
	if _, err := ParseTxnID(string(t.TxnID)); err != nil {
		return err
	}
	if len(t.Entries) == 0 {
		// 空交易在账面上是「什么都没发生」，而调用方写出一个空交易
		// 通常意味着某处提前 return 了。报出来。
		return fmt.Errorf("交易 %s 没有任何分录", t.TxnID)
	}

	// 按资产分别求和。跨资产直接相加会得到一个没有意义的数，
	// 而那个数恰好为零时（一笔 CNY 与一笔 USDT 互相抵消）会放过
	// 一组真实不平的分录。目前只有 CNY 一种资产，所以这个 map 里
	// 通常只有一个键；按资产分组的写法是为了接第二种资产时不必回头改
	// 校验的语义。
	totals := make(map[Asset]money.Amount, 1)
	// 同一交易里出现两条**完全相同**的分录（科目、类型、资产、金额都相同）
	// 几乎只有一种来路：同一条分录被写了两遍。它不一定是坏的——
	// 两条一样的贷方配上两条一样的借方仍然平——但它一定该被看见，
	// 因为「重复写入」与「有意写两笔」在账面上无法区分。
	//
	// 需要同一科目出现两次时，写两张不同金额的分录，或拆成两笔交易，
	// 后者更符合「一次交易对应一个业务事件」的口径。
	type entryKey struct {
		account Account
		kind    EntryType
		asset   Asset
		amount  money.Amount
	}
	seen := make(map[entryKey]int, len(t.Entries))

	for i, e := range t.Entries {
		if err := e.verify(t.TxnID); err != nil {
			return fmt.Errorf("第 %d 条分录: %w", i, err)
		}
		totals[e.Asset] = totals[e.Asset].Add(e.Amount)
		k := entryKey{e.Account, e.EntryType, e.Asset, e.Amount}
		seen[k]++
		if seen[k] > 1 {
			return fmt.Errorf("第 %d 条分录与前面的完全重复：%s；重复写入会让账面金额翻倍", i, e)
		}
	}

	for _, asset := range sortedAssets(totals) {
		sum := totals[asset]
		if !sum.IsZero() {
			return fmt.Errorf("交易 %s 在资产 %s 上不平：分录之和为 %s，应为 0；差额必须修构造逻辑，不能补一笔调平分录",
				t.TxnID, asset, sum)
		}
	}
	return nil
}

// verify 校验单条分录。
func (e Entry) verify(txn TxnID) error {
	if e.TxnID != txn {
		// 一组分录混进了别的交易的分录。拼接多组分录时会发生，
		// 而后果是两组各自平的交易合起来不平——而差额恰好抵消时，
		// 两边都看不出问题。
		return fmt.Errorf("分录的 txn_id %s 与交易的 %s 不一致", e.TxnID, txn)
	}
	if e.Account.IsZero() {
		return errors.New("科目为空")
	}
	if _, err := ParseAccount(e.Account.String()); err != nil {
		return err
	}
	if !e.Asset.Valid() {
		return fmt.Errorf("资产 %q 未知", e.Asset)
	}
	if !validEntryType(e.EntryType) {
		return fmt.Errorf("分录类型 %q 未知", e.EntryType)
	}
	if e.Amount.IsZero() {
		// 零金额分录有两种来路：某个金额被算成了零（真 bug），
		// 或者调用方想「占个位」。前者必须查，后者不该有。
		return fmt.Errorf("科目 %s 的金额为零", e.Account)
	}
	return checkDirection(e.EntryType, e.Account, e.Amount)
}

// VerifyEntries 校验一组外部来源的分录。
//
// 给「从数据库读回一批行」这条路径用：那里拿到的是 []Entry 而不是一个
// Transaction，而校验的单位是交易。它按 txn_id 分组、逐组校验。
//
// 它不能发现「某个交易少读了一行」——把一组的范围截错了，剩下的分录
// 恰好可能仍然平（例如消费的四条里少了平台收入与上游成本两条，
// 剩下的两条就是一个合法的充值）。所以调用方必须把一次对账范围内的
// **全部**行交进来，或者按 txn_id 逐组查全再调用。
func VerifyEntries(entries []Entry) error {
	if len(entries) == 0 {
		return errors.New("没有分录可校验")
	}
	groups := make(map[TxnID][]Entry)
	order := make([]TxnID, 0, 1)
	for _, e := range entries {
		if _, ok := groups[e.TxnID]; !ok {
			order = append(order, e.TxnID)
		}
		groups[e.TxnID] = append(groups[e.TxnID], e)
	}
	for _, txn := range order {
		t := Transaction{TxnID: txn, Entries: groups[txn]}
		if err := t.Verify(); err != nil {
			return err
		}
	}
	return nil
}

// Denormalize 把交易摊平成一组可以逐条写库的分录。
//
// 返回的是副本：调用方可以在上面挂写库需要的字段，而不会改动原交易。
// 提供它的理由是写库的驱动通常要逐条参数，而不是一个聚合对象。
func (t Transaction) Denormalize() []Entry {
	out := make([]Entry, len(t.Entries))
	copy(out, t.Entries)
	return out
}

// sanitizeMemo 去掉备注里的换行与制表符。
//
// 理由与 observe 包清洗请求标识相同：备注会进日志，而一个带换行的备注
// 会伪造出额外的日志行。备注的另一个来源是外部系统（上游错误信息、
// 支付通道的回执），也就是不受我们控制的字符串。
func sanitizeMemo(s string) string {
	if !strings.ContainsAny(s, "\r\n\t") {
		return s
	}
	return strings.NewReplacer("\r", " ", "\n", " ", "\t", " ").Replace(s)
}
