package ledger

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/money"
)

// 本文件是各业务场景的构造函数。
//
// 每个场景一层薄封装，推导出「钱从哪来、到哪去」。把它们集中在这里而不是
// 让调用方自己写分录：方向约定在每个场景里都一样，而让每个调用点各自写
// 一遍，就必然有一处把方向写反。方向写反不会让账不平（另一半会跟着反），
// 所以求和的校验抓不到它，而它会让余额从一个应该非负的科目里漏出去。
//
// 所有构造函数都返回 (Transaction, error)，且**只报错、不猜**：
// 参数不自洽时宁可让调用方立刻失败，也不要生成一组「看起来能用」的分录
// ——那会把一个本该立即暴露的问题推迟到对账时，而那时已经很难回溯。

// validateCommon 检查各场景共有的输入。
func validateCommon(kind EntryType, txn TxnID, asset Asset, amount money.Amount) error {
	if !validEntryType(kind) {
		return fmt.Errorf("未知的场景类型 %q", kind)
	}
	if txn == "" {
		return errors.New("缺少 txn_id")
	}
	if !asset.valid() {
		return fmt.Errorf("资产 %q 未知", asset)
	}
	if !amount.IsPositive() {
		// 金额必须由调用方给出正数，方向由本文件的构造函数决定。
		// 接受负数会让「方向写反」在上游被悄悄吞掉，而这里正是唯一
		// 能把它暴露出来的地方。
		return fmt.Errorf("金额必须为正数，当前为 %s", amount)
	}
	return nil
}

// newTxn 组装一组分录并做一次自检。
//
// 自检不是多余的：它让「构造函数产出的交易一定自洽」这条性质由运行时
// 与测试同时钉住。将来给某个场景补一条分录而漏掉另一边时，会在构造的
// 那一刻立刻炸出来，而不是等到线上对账。
func newTxn(kind EntryType, txn TxnID, at time.Time, entries ...Entry) (Transaction, error) {
	t := Transaction{
		TxnID:     txn,
		CreatedAt: at,
		Entries:   entries,
		Kind:      kind,
	}
	if err := t.Verify(); err != nil {
		return Transaction{}, fmt.Errorf("构造的 %s 交易不自洽: %w", kind, err)
	}
	return t, nil
}

// TopupParams 是一次充值到账的输入。
type TopupParams struct {
	TxnID  TxnID
	UserID int64
	Amount money.Amount
	Asset  Asset
	// Gateway 是支付通道标识（stripe / alipay / …），进 payment_gateway 科目。
	Gateway string
	// GatewayFee 是通道手续费。它是**成本**，不是从用户余额里扣的钱：
	// 用户充 100 到账 100，通道扣走的那 2 由平台承担。把它记成
	// 「用户到账 98」会让客服与用户看到两个不同的数字。
	GatewayFee money.Amount
	// OrderNo 是支付订单号，进 memo 供人工追溯。
	OrderNo string
	At      time.Time
}

// Topup 构造一次充值到账。
//
// 分录（到账金额 A，通道手续费 F）：
//
//	借 user:<id>              A    平台欠用户的钱增加
//	贷 payment_gateway:<gw>   A    通道收到的全额记在通道科目
//
//	若 F > 0：
//	借 upstream_cost          F    通道费记在成本科目
//	贷 platform_revenue       F    这笔成本由平台自己承担
func Topup(p TopupParams) (Transaction, error) {
	if err := validateCommon(EntryTopup, p.TxnID, p.Asset, p.Amount); err != nil {
		return Transaction{}, err
	}
	if p.UserID <= 0 {
		return Transaction{}, fmt.Errorf("用户 id 非法: %d", p.UserID)
	}
	if p.Gateway == "" {
		return Transaction{}, errors.New("缺少支付通道标识")
	}
	if p.GatewayFee.IsNegative() {
		return Transaction{}, fmt.Errorf("通道手续费为负: %s", p.GatewayFee)
	}
	if p.GatewayFee.Cmp(p.Amount) > 0 {
		// 手续费大于充值额意味着用户实际到账比我们收的还多，
		// 那笔差额没有来源。这是参数弄反的典型征兆，直接拒。
		return Transaction{}, fmt.Errorf("通道手续费 %s 大于充值金额 %s", p.GatewayFee, p.Amount)
	}

	// 用户实付按**全额**记，手续费单独记成平台的成本。
	//
	// 通道是按全额收款的（用户付 A，通道扣走自己的 F 之后把余下的打给我们），
	// 所以通道科目先按全额记，手续费再作为一笔独立的成本由平台承担。
	// 两笔分开而不是把手续费从通道科目里直接扣掉：合成一笔的话，
	// 通道科目的余额就不再等于「通道应收的总额」，无法与通道账单逐笔核对；
	// 而通道账单上写的一定是全额加手续费两行。
	gateway := PaymentGatewayAccount(p.Gateway)
	entries := []Entry{
		debit(p.TxnID, UserAccount(p.UserID), p.Asset, p.Amount, EntryTopup),
		credit(p.TxnID, gateway, p.Asset, p.Amount, EntryTopup).
			withMemo(memoWithOrder("充值", p.OrderNo)),
	}
	if p.GatewayFee.IsPositive() {
		entries = append(entries,
			debit(p.TxnID, UpstreamCostAccount(), p.Asset, p.GatewayFee, EntryTopup).
				withMemo("支付通道手续费"),
			// 这笔成本的承担者是平台自己，所以对家是平台科目（贷方）。
			// 记账恒等式要求每一笔借方都有对家，而平台为此少赚的钱
			// 只能落在平台科目上。
			credit(p.TxnID, PlatformRevenueAccount(), p.Asset, p.GatewayFee, EntryTopup),
		)
	}
	return newTxn(EntryTopup, p.TxnID, p.At, entries...)
}

// ConsumeParams 是一次请求消费的输入。
type ConsumeParams struct {
	TxnID  TxnID
	UserID int64
	// Amount 是向用户收取的总额（售价），由 internal/billing 算出。
	Amount money.Amount
	Asset  Asset
	// MerchantID 是承接这次请求的渠道所属商户。为 0 表示这笔消费不归属
	// 任何商户（平台自营渠道），此时没有分成分录。
	MerchantID int64
	// MerchantShare 是分给商户的金额，从售价里出。为 0 表示这次不分账。
	MerchantShare money.Amount
	// UpstreamCost 是这次请求在上游实际花掉的成本。为 0 表示不记成本
	//（例如上游是包月套餐，边际成本为零）。
	UpstreamCost money.Amount
	// BillingRequestID 关联被计费的那次请求。
	BillingRequestID int64
	At               time.Time
}

// Consume 构造一次请求消费。
//
// 分录（售价 A，商户分成 S，上游成本 C）：
//
//	贷 user:<id>                  A    平台欠用户的钱减少
//	借 merchant_payable:<mid>     S    欠商户的分成增加
//	借 upstream_cost              C    上游成本增加
//	借 platform_revenue     A-S-C    剩下的是平台收入
//
// # 为什么平台收入是推出来的
//
// 平台收入写成 A-S-C，而不是让调用方再给一个「平台收入」参数：
// 四项各自给出就有对不上的可能，而账不平是最难查的一类错误。
// 推出来则天然平，且「售价 = 分成 + 成本 + 收入」这条业务等式
// 在账面上直接成立。
//
// 分成与上游成本各自是一笔独立的分录，而不是从收入里再切一刀：
// 它们是不同的科目，各有各的对账对象（商户分成对账单、上游账单）。
// 平台自己的收入明细由 internal/billing 的 Breakdown 逐项保留，
// 账本这一层不必重复一份。
//
// # 为什么分成与成本之和超过售价时直接拒
//
// 那意味着这一单在亏钱。它应当由**定价**去修正，而不是让 platform_revenue
// 出现一笔负数——负收入混进正常交易里看不出来，而它正是最该被
// 单独看见的一类。
func Consume(p ConsumeParams) (Transaction, error) {
	if err := validateCommon(EntryConsume, p.TxnID, p.Asset, p.Amount); err != nil {
		return Transaction{}, err
	}
	if p.UserID <= 0 {
		return Transaction{}, fmt.Errorf("用户 id 非法: %d", p.UserID)
	}
	if p.MerchantShare.IsNegative() || p.UpstreamCost.IsNegative() {
		return Transaction{}, fmt.Errorf("分成 %s 与上游成本 %s 都不能为负", p.MerchantShare, p.UpstreamCost)
	}
	if p.MerchantShare.IsPositive() && p.MerchantID <= 0 {
		return Transaction{}, errors.New("给了分成金额却没有商户 id：分成的归属无法确定")
	}
	if p.MerchantShare.Add(p.UpstreamCost).Cmp(p.Amount) > 0 {
		return Transaction{}, fmt.Errorf("分成 %s 与上游成本 %s 之和超过售价 %s；这一单在亏钱，应改定价而不是写负收入",
			p.MerchantShare, p.UpstreamCost, p.Amount)
	}

	entries := []Entry{
		credit(p.TxnID, UserAccount(p.UserID), p.Asset, p.Amount, EntryConsume),
	}
	if p.MerchantShare.IsPositive() {
		entries = append(entries, debit(p.TxnID, MerchantPayableAccount(p.MerchantID), p.Asset, p.MerchantShare, EntryConsume))
	}
	if p.UpstreamCost.IsPositive() {
		entries = append(entries, debit(p.TxnID, UpstreamCostAccount(), p.Asset, p.UpstreamCost, EntryConsume))
	}
	if rest := p.Amount.Sub(p.MerchantShare).Sub(p.UpstreamCost); rest.IsPositive() {
		entries = append(entries, debit(p.TxnID, PlatformRevenueAccount(), p.Asset, rest, EntryConsume))
	}

	if p.BillingRequestID > 0 {
		for i := range entries {
			entries[i] = entries[i].withBillingRequest(p.BillingRequestID)
		}
	}
	return newTxn(EntryConsume, p.TxnID, p.At, entries...)
}

// RefundParams 是一次退款的输入。
type RefundParams struct {
	TxnID  TxnID
	UserID int64
	// Amount 是退给用户的金额。可以小于原始消费（部分退款），
	// **不能**大于原始消费——后者不是退款，是一笔需要单独说明的调账。
	// 本包不查库，看不到原始消费的金额，所以这条上下界由调用方保证。
	Amount money.Amount
	Asset  Asset
	// MerchantID 与 MerchantShare 是这笔退款要冲回的商户分成。
	// 原本没分账的消费，退款时两项都留零。
	MerchantID    int64
	MerchantShare money.Amount
	// UpstreamCost 是上游退还的成本。
	//
	// 上游按实际用量计费时，失败的请求通常不收费，留零即可；上游已经
	// 收费而事后退回时才填正数。留零是一种**取舍**：上游已经产生、
	// 事后没退的成本不会出现在账面上，于是这一单的亏损看不出来，
	// 只能从 upstream_cost 的汇总里发现。代价明确，比「按估算填一个数」
	// 要好——猜出来的成本记进账本，等于把不确定的东西变成了权威数据。
	UpstreamCost money.Amount

	// BillingRequestID 关联被退的那次计费请求。
	BillingRequestID int64
	// Memo 说明退款原因。对账时「为什么退」比「退了多少」更需要解释。
	Memo string
	At   time.Time
}

// Refund 构造一次退款，它是 Consume 的镜像，每一项的符号都反过来。
//
//	借 user:<id>                  A    平台欠用户的钱增加（余额退回来了）
//	贷 merchant_payable:<mid>     S    欠商户的分成减少
//	贷 upstream_cost              C    上游成本减少
//	贷 platform_revenue     A-S-C    平台退还的收入
//
// 退多少收入也是推出来的，理由与 Consume 相同：冲回的三项正好把
// 退给用户的钱抵平。调用方要保证传入的分成与成本不超过消费时的
// 数值——部分退款时按比例拆，那是调用方的口径，本包看不到原始消费。
//
// 退款写成新分录而不是删改原分录：账本只追加，任何修正都要留下痕迹。
// 一笔「消费 100、退款 100」与「什么都没发生」在余额上等价，
// 但前者能回答「这笔钱为什么回来了」。
func Refund(p RefundParams) (Transaction, error) {
	if err := validateCommon(EntryRefund, p.TxnID, p.Asset, p.Amount); err != nil {
		return Transaction{}, err
	}
	if p.UserID <= 0 {
		return Transaction{}, fmt.Errorf("用户 id 非法: %d", p.UserID)
	}
	if p.MerchantShare.IsNegative() || p.UpstreamCost.IsNegative() {
		return Transaction{}, fmt.Errorf("分成 %s 与上游成本 %s 都不能为负", p.MerchantShare, p.UpstreamCost)
	}
	if p.MerchantShare.IsPositive() && p.MerchantID <= 0 {
		return Transaction{}, errors.New("给了分成金额却没有商户 id：退款的归属无法确定")
	}
	if p.MerchantShare.Add(p.UpstreamCost).Cmp(p.Amount) > 0 {
		return Transaction{}, fmt.Errorf("要冲回的分成 %s 与上游成本 %s 之和超过退款额 %s",
			p.MerchantShare, p.UpstreamCost, p.Amount)
	}

	entries := []Entry{
		debit(p.TxnID, UserAccount(p.UserID), p.Asset, p.Amount, EntryRefund).
			withMemo(sanitizeMemo(p.Memo)),
	}
	if p.MerchantShare.IsPositive() {
		entries = append(entries, credit(p.TxnID, MerchantPayableAccount(p.MerchantID), p.Asset, p.MerchantShare, EntryRefund))
	}
	if p.UpstreamCost.IsPositive() {
		entries = append(entries, credit(p.TxnID, UpstreamCostAccount(), p.Asset, p.UpstreamCost, EntryRefund))
	}
	if rest := p.Amount.Sub(p.MerchantShare).Sub(p.UpstreamCost); rest.IsPositive() {
		entries = append(entries, credit(p.TxnID, PlatformRevenueAccount(), p.Asset, rest, EntryRefund))
	}

	if p.BillingRequestID > 0 {
		for i := range entries {
			entries[i] = entries[i].withBillingRequest(p.BillingRequestID)
		}
	}
	return newTxn(EntryRefund, p.TxnID, p.At, entries...)
}

// SettleParams 是一次商户结算的输入。
type SettleParams struct {
	TxnID  TxnID
	UserID int64
	// Amount 是要结算的金额，必须为正。
	Amount money.Amount
	Asset  Asset
	// PayoutID 是这条提现单的 id，**必填**。
	//
	// 不设「先清算、后关联提现单」这种可空形态：payout 科目不带 id 时
	// （写成 `payout`）无法区分是哪一笔待付，而账本里一笔没有归属的
	// 负债等于没有记。真需要那个流程，应当由调用方先生成提现单再清算。
	PayoutID int64
	// Memo 说明结算依据（结算周期、对账批次号）。
	Memo string
	At   time.Time
}

// Settle 构造一次商户结算。
//
// 分录：
//
//	贷 merchant_payable:<id>   A    欠商户的分成核销
//	借 payout:<payoutID>       A    转为这条提现单的待付
//
// # 为什么必须经过 payout 科目而不是直接进 user:<id>
//
// 商户余额与用户的可用余额**不能共用同一个科目**：前者是「平台还没有
// 付给他的钱」，后者是「他可以在下一次请求里花掉的钱」。混在一个
// user:<id> 里，商户的可用余额会虚高，而提现时又要判断「哪部分是分成」
// ——那一判断一旦写错，用户就可能在花掉分成之后又提现一次。
//
// 分开的代价是提现流程多一步记账，收益是「已经申请提现、平台还没打款」
// 这个中间状态在账面上是可见的。
func Settle(p SettleParams) (Transaction, error) {
	if err := validateCommon(EntrySettle, p.TxnID, p.Asset, p.Amount); err != nil {
		return Transaction{}, err
	}
	if p.UserID <= 0 {
		return Transaction{}, fmt.Errorf("商户 id 非法: %d", p.UserID)
	}
	if p.PayoutID <= 0 {
		return Transaction{}, fmt.Errorf("提现单 id 非法: %d", p.PayoutID)
	}

	entries := []Entry{
		credit(p.TxnID, MerchantPayableAccount(p.UserID), p.Asset, p.Amount, EntrySettle).
			withMemo(sanitizeMemo(p.Memo)).
			withPayout(p.PayoutID),
		debit(p.TxnID, PayoutAccount(p.PayoutID), p.Asset, p.Amount, EntrySettle).
			withPayout(p.PayoutID),
	}
	return newTxn(EntrySettle, p.TxnID, p.At, entries...)
}

// PayoutRequestParams 是一次提现申请的输入。
type PayoutRequestParams struct {
	TxnID TxnID
	// PayoutID 是提现单 id，**必填**：它就是 payout 科目要用的那个标识，
	// 缺了它这一笔待付就没有归属，付给谁、付多少都无从查起。
	PayoutID int64
	// UserID 是申请提现的商户。
	UserID int64
	// Amount 是这次申请的金额，必须为正。
	Amount money.Amount
	Asset  Asset
	At     time.Time
}

// PayoutRequest 构造一次提现申请。
//
// 分录：
//
//	贷 merchant_payable:<id>   A    欠商户的分成核销
//	借 payout:<payoutID>       A    变为这条提现单的待付
//
// 它的形状与 Settle 相同，两者都保留是因为「谁发起的」这件事需要
// 在账面上可见：settle 是平台主动清算，payout_request 是商户发起、
// 经审核通过。用一个函数加开关会让调用点看不出自己在做哪一件事，
// 而审计时这两个问题完全不同。
//
// 本函数只管账面。审核状态（pending / approved）在 payout_requests 表上，
// 那是另一条事实，不重复记进账本。
func PayoutRequest(p PayoutRequestParams) (Transaction, error) {
	if err := validateCommon(EntryPayoutRequest, p.TxnID, p.Asset, p.Amount); err != nil {
		return Transaction{}, err
	}
	if p.UserID <= 0 {
		return Transaction{}, fmt.Errorf("商户 id 非法: %d", p.UserID)
	}
	if p.PayoutID <= 0 {
		return Transaction{}, fmt.Errorf("提现单 id 非法: %d", p.PayoutID)
	}

	entries := []Entry{
		credit(p.TxnID, MerchantPayableAccount(p.UserID), p.Asset, p.Amount, EntryPayoutRequest).
			withPayout(p.PayoutID),
		debit(p.TxnID, PayoutAccount(p.PayoutID), p.Asset, p.Amount, EntryPayoutRequest).
			withPayout(p.PayoutID),
	}
	return newTxn(EntryPayoutRequest, p.TxnID, p.At, entries...)
}

// PayoutPaidParams 是一次打款完成的输入。
type PayoutPaidParams struct {
	TxnID    TxnID
	PayoutID int64
	// UserID 是收到打款的商户。它不出现在分录里（钱从 payout 科目出去），
	// 但必填：账本要能回答「这条提现单是谁的」，而那不是从 payout 科目
	// 的 id 推得出来的。
	UserID int64
	Amount money.Amount
	Asset  Asset
	// Gateway 是实际打款走的通道（stripe / alipay / …）。
	//
	// 它必填而不是从提现单推出来：本包不查库。打款通道在
	// payout_requests.destination 里，由调用方查到后传进来。
	// 猜一个通道名会让通道对账永远差一笔，而那一笔查不出来。
	Gateway string
	// Ref 是支付通道给的打款凭据号，进 memo 供人工对账。
	Ref string
	At  time.Time
}

// PayoutPaid 构造一次打款完成。
//
// 分录：
//
//	贷 payout:<payoutID>         A    这条提现单的待付核销
//	借 payment_gateway:<gw>      A    我们欠通道的钱增加（负数即应付）
//
// 贷方走 payment_gateway 而不是让金额凭空消失：账本里每一笔钱都必须有
// 对家，没有对家的一组分录和不为零——那正是账本不允许的。
func PayoutPaid(p PayoutPaidParams) (Transaction, error) {
	if err := validateCommon(EntryPayoutPaid, p.TxnID, p.Asset, p.Amount); err != nil {
		return Transaction{}, err
	}
	if p.PayoutID <= 0 {
		return Transaction{}, fmt.Errorf("提现单 id 非法: %d", p.PayoutID)
	}
	if p.UserID <= 0 {
		return Transaction{}, fmt.Errorf("商户 id 非法: %d", p.UserID)
	}
	if p.Gateway == "" {
		return Transaction{}, errors.New("缺少打款通道标识：通道未知时打款分录会对不上通道账单")
	}

	memo := "提现打款"
	if p.Ref != "" {
		memo = memoWithOrder(memo, p.Ref)
	}
	entries := []Entry{
		credit(p.TxnID, PayoutAccount(p.PayoutID), p.Asset, p.Amount, EntryPayoutPaid).
			withMemo(sanitizeMemo(memo)).
			withPayout(p.PayoutID),
		debit(p.TxnID, PaymentGatewayAccount(p.Gateway), p.Asset, p.Amount, EntryPayoutPaid).
			withPayout(p.PayoutID),
	}
	return newTxn(EntryPayoutPaid, p.TxnID, p.At, entries...)
}

// AdjustmentParams 是一次人工调账的输入。
type AdjustmentParams struct {
	TxnID TxnID
	// Account 是被调的科目。
	Account Account
	// Amount 是**带符号**的调整金额，即「目标余额 - 当前余额」的符号。
	// 与其它场景不同，这里允许负数——调账本身就是双向的。
	Amount money.Amount
	Asset  Asset
	// Memo 必填。调账是唯一没有业务依据的分录，没有备注的调账事后
	// 完全无法解释，只能当成来路不明的一笔。
	Memo string
	// CounterAccount 是对家科目。留零表示 platform_revenue（默认）。
	//
	// 允许指定它是为了对账：对账差额的来路不明，把它记为平台收入等于
	// 让一笔没人解释过的钱混进经营收入。对账修正因此传入
	// ReconcileDifferenceAccount，把差额单独归集，查清来路后再写一笔
	// 人工调账把它转进 platform_revenue（那时差额已经被解释了）。
	//
	// 默认值保留 platform_revenue 而不是要求必填：人工调账的实质就是
	// 「平台认下这笔差额」，绝大多数调用点要的正是它。
	CounterAccount Account
	At             time.Time
}

// Adjustment 构造一次人工调账。
//
// 分录（借方被调科目或贷方被调科目，视金额符号而定）：
//
//	金额为正：借 <被调科目>   A      贷 <对家科目>       A
//	金额为负：贷 <被调科目>  |A|     借 <对家科目>      |A|
//
// 对家默认固定为 platform_revenue，而不是让调用方随手指定：调账的实质是
// 「平台认下这笔差额」。如果对家可以是任意科目，这个函数就变成了一个
// 能凭空造分录的工具，而它本应是最少使用的。对账传
// ReconcileDifferenceAccount 是有意的例外——见 AdjustmentParams.CounterAccount。
//
// 被调科目若是 user 这类「不该为负」的科目，调减到负值会破坏方向约定。
// 本包不查库，看不到当前余额，所以这里只能检查**单条分录**的方向；
// 汇总后会不会变负，由调用方拿 BalanceOf 的结果自行判断。
func Adjustment(p AdjustmentParams) (Transaction, error) {
	if p.Account.IsZero() {
		return Transaction{}, errors.New("缺少被调的科目")
	}
	if _, err := ParseAccount(p.Account.String()); err != nil {
		return Transaction{}, err
	}
	counter := p.CounterAccount
	if counter.IsZero() {
		counter = PlatformRevenueAccount()
	} else if _, err := ParseAccount(counter.String()); err != nil {
		return Transaction{}, err
	}
	if counter == p.Account {
		// 同一个科目既是被调方又是对家：两条分录互相抵消，求和为零，
		// 于是 Verify 放行——而这笔交易什么都没改。它一定是调用方
		// 传错了参数，静默成立会让一次「以为修好了」的调账变成空操作。
		return Transaction{}, fmt.Errorf("调账的对家不能是被调科目自己（%s）", counter)
	}
	if p.Memo == "" {
		return Transaction{}, errors.New("人工调账必须写 memo 说明原因")
	}
	// 这里不复用 validateCommon：它要求金额为正，而调账是唯一允许
	// 负金额的场景——那正是「把记多的钱扣回来」的写法。
	if p.TxnID == "" {
		return Transaction{}, errors.New("缺少 txn_id")
	}
	if !p.Asset.valid() {
		return Transaction{}, fmt.Errorf("资产 %q 未知", p.Asset)
	}
	if p.Amount.IsZero() {
		return Transaction{}, errors.New("调账金额为零，没有需要修正的东西")
	}

	memo := sanitizeMemo(p.Memo)
	var entries []Entry
	if p.Amount.IsPositive() {
		entries = []Entry{
			debit(p.TxnID, p.Account, p.Asset, p.Amount, EntryAdjustment).withMemo(memo),
			credit(p.TxnID, counter, p.Asset, p.Amount, EntryAdjustment).withMemo(memo),
		}
	} else {
		mag := p.Amount.Neg()
		entries = []Entry{
			credit(p.TxnID, p.Account, p.Asset, mag, EntryAdjustment).withMemo(memo),
			debit(p.TxnID, counter, p.Asset, mag, EntryAdjustment).withMemo(memo),
		}
	}
	return newTxn(EntryAdjustment, p.TxnID, p.At, entries...)
}

// memoWithOrder 拼一条带单据号的备注。
//
// 单据号来自外部系统（支付网关、银行回单），可能带换行或制表符，
// 所以它也必须过一遍清洗。
func memoWithOrder(prefix, orderNo string) string {
	// 判空在清洗之后做，且用 TrimSpace：sanitizeMemo 把控制字符换成
	// **空格**（保留分隔），所以 `"\t"` 清洗后是 `" "` 而不是空串。
	// 不 TrimSpace 的话，只有空白字符的单据号会产生「（单据 ）」
	// 这种备注，读起来像是数据缺失，排查时会去追一个不存在的单据号。
	orderNo = strings.TrimSpace(sanitizeMemo(orderNo))
	if orderNo == "" {
		return prefix
	}
	return fmt.Sprintf("%s（单据 %s）", prefix, orderNo)
}
