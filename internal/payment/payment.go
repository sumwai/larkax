// Package payment 是支付通道的适配层与回调入口。
//
// # 它做什么
//
// 一次充值从「下单」到「钱进账本」分三步：`Channel.CreateOrder` 在通道侧建单、
// 通道把结果以通知的形式发回 `Handler`、`Service` 校验并落库后调
// `internal/ledger` 的 `Topup` 写一组充值分录。本包只决定这三步的顺序与
// 各自的判据，记账规则一行都不在这里。
//
// # 通道是可替换的
//
// `Channel` 三个动作——下单、校验并解析通知、按订单号查状态——是通道之间
// 唯一需要各自实现的部分。签名方案、字段名与请求形状都属于通道，本包只接收
// 它们归一之后的结果。今天只有一条假通道（见 internal/payment/fakechannel），
// 接真实通道时换的是 `Channel` 的实现，`Service` 与 `Handler` 不动。
//
// # 幂等键是订单号
//
// 同一个 `order_no` 只入一次账：它的唯一约束在 `payment_orders.order_no` 上，
// 账本那一侧的交易号由它确定性地推出来，重放与冲突的判定复用
// `ledger/store.PostTransaction`（同键同内容算重放、同键不同内容报冲突）。
// 本包不另造一套幂等判据。
//
// # 买额度包走同一条链
//
// 下单时带 `plan_key` 的支付单不是充值：付款通知到达后，它在同一个事务里发放
// 一条订阅并写一组 `plan_purchase` 分录（借 `prepaid:<订阅 id>`、贷
// `payment_gateway:<通道>`），钱不进用户钱包。两条路共用同一个状态机与同一个
// 幂等键（订单号），差别只在「入账时写哪一档分录」。
//
// # 状态只许往前走
//
// `created` 是唯一能入账的起点：只有它收到「已支付」才写分录并转成 `paid`；
// `failed` / `expired` / `refunded` 是终态，之后的通知不改账。
package payment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/money"
)

// Status 是 payment_orders.status 的取值，与迁移里的 CHECK 约束一致。
//
// 用具名类型而不是裸 string：写错一个字母时所有比较都落空，表现是「这张单子
// 看起来一直停在原地」，而哪里都没有报错。
type Status string

const (
	// StatusCreated 是刚下单、还没收到通道结果。
	StatusCreated Status = "created"
	// StatusPaid 是通道确认收款，分录已经写进账本。
	StatusPaid Status = "paid"
	// StatusFailed 是通道明确拒绝或支付失败。
	StatusFailed Status = "failed"
	// StatusExpired 是订单过了通道给的有效期。
	StatusExpired Status = "expired"
	// StatusRefunded 是这笔充值事后被退回去了。
	StatusRefunded Status = "refunded"
)

// Label 给状态值一个中文说法。
//
// 库里存英文取值，打给人看的地方要中文。认不出的取值原样打出来：猜一个中文
// 比露出原文更坏，而原文至少还能与库里的值逐字对上。
func (s Status) Label() string {
	switch s {
	case StatusCreated:
		return "待支付"
	case StatusPaid:
		return "已支付"
	case StatusFailed:
		return "支付失败"
	case StatusExpired:
		return "已过期"
	case StatusRefunded:
		return "已退款"
	}
	return string(s)
}

// Terminal 报告这个状态是否已经走到终点。
//
// `paid` 之后还能退款，但在「充值到账」这条链上它已经是终态：再收到支付通知
// 不能让它再多入一次账。
func (s Status) Terminal() bool {
	switch s {
	case StatusPaid, StatusFailed, StatusExpired, StatusRefunded:
		return true
	}
	return false
}

// validNotificationStatus 报告通道通知里的状态是不是这条链认得的那三个。
//
// 只有支付成功、失败与过期三种；`created` 是本地状态，`refunded` 是事后动作，
// 都不该从支付通知里来。认不出的取值直接拒，而不是当成「无事发生」——后者会
// 让一个通道侧的新状态静默地永远到不了账。
func validNotificationStatus(s Status) bool {
	switch s {
	case StatusPaid, StatusFailed, StatusExpired:
		return true
	}
	return false
}

// Order 是 payment_orders 里的一行。
type Order struct {
	OrderNo string
	UserID  int64
	Gateway string
	Asset   ledger.Asset
	Amount  money.Amount
	Fee     money.Amount
	Status  Status
	// PlanKey 是这张单买的额度包（plans.key）；空表示充值。
	PlanKey string
	// ValidityDays 是**下单那一刻**包上的售卖天数，售卖的单子才有；充值为 nil。
	//
	// 到期时刻由它与收款时刻算出，入账时**不再读包上的值**：用户按多少天付的钱，
	// 从下单那一刻起就只由这张单回答。包后来被改成 365 天，这一单也不会多给
	// 11 个月——那一格由 plans.purchasePlanMismatch 拦下（首次发放拒绝）。
	//
	// 用指针而不是 int 表达「没有」：0 天与「这不是售卖单」是两件事，而 0 天
	// 本身不合法（迁移上写着 > 0），混用会让「没记」看起来像「记了 0 天」。
	ValidityDays *int
	// GatewayTxnID 是通道侧的流水号，只有付过款之后才有。
	GatewayTxnID string
	// GatewayRaw 是通道原始响应的 JSON 文本。
	GatewayRaw []byte
	PaidAt     *time.Time
	CreatedAt  time.Time
}

// CreateOrderRequest 是下单的输入。
//
// 字段照真实通道公开文档里的那几项取：商户单号、金额、商品说明与回调地址。
// 本包不解释 `Subject` 与 `NotifyURL`，它们原样交给通道。
type CreateOrderRequest struct {
	// OrderNo 是商户单号，也是这一整条链的幂等键。
	OrderNo string
	UserID  int64
	Asset   ledger.Asset
	Amount  money.Amount
	Subject string
	// NotifyURL 是通道把结果发回来的地址（本服务的支付回调入口）。
	NotifyURL string
	// PlanKey 非空表示这笔不是充值，而是买一个额度包：钱不进用户钱包，付款
	// 通知到达后发放一条订阅，面值记进 prepaid:<订阅 id>。
	//
	// 给了它时 Amount 与 Asset 可以留空，服务端从包上取（见 Service.applyPlan）；
	// 给了就必须与包上的值相等——金额与币种由商品决定，不由调用方决定。
	// 「给没给」看下面两个 Given 标志，不看字段的零值（理由见各自的注释）。
	PlanKey string
	// AmountGiven 表示调用方**明确给了**金额。
	//
	// 买额度包时它决定 applyPlan 走哪一条：给了就必须为正且等于包的面值，
	// 没给就用包的面值。充值（PlanKey 为空）不看它——那条路 validateCreateOrder
	// 要求金额为正。
	AmountGiven bool
	// AssetGiven 表示调用方**明确给了**币种，形状与 AmountGiven 相同。
	//
	// 买额度包时币种由商品决定：给了只用来核对，与包不符就拒；没给就用包的。
	// 币种也单独用一个标志：CLI 在充值那条路上会把 settings 里的默认资产
	// 填进 Asset，买包那条路不填。两条路都落到同一个 CreateOrderRequest 上，
	// 服务层只有看这个标志才分得清「调用方给的」与「CLI 补的默认值」。
	AssetGiven bool
	// ValidityDays 由 applyPlan 在下单时从包上取，**调用方不该填**。
	//
	// 它落进 payment_orders.validity_days，成为「用户按多少天付的钱」的唯一
	// 依据：入账时按它算到期时刻，不再读包（见 Order.ValidityDays）。放在这里
	// 是因为订单那一行由 req 翻译而成（orderForCreate），而包只在 applyPlan
	// 里读得到。
	ValidityDays *int
}

// CreatedOrder 是通道建单之后回来的东西。
type CreatedOrder struct {
	OrderNo string
	// PayURL 是让付款人打开的支付地址。
	PayURL string
	// GatewayTxnID 可能为空：有的通道在付款之前不给流水号。
	GatewayTxnID string
	// Raw 是通道响应的原文，落进 payment_orders.gateway_raw。
	Raw []byte
}

// NotificationRequest 是一次通知的原始输入。
type NotificationRequest struct {
	Header map[string][]string
	Body   []byte
}

// Notification 是通道通知归一之后的结果。
type Notification struct {
	OrderNo      string
	GatewayTxnID string
	Status       Status
	Amount       money.Amount
	Asset        ledger.Asset
	// Fee 是通道收走的手续费，没有就留零。
	Fee money.Amount
	// PaidAt 是通道记的支付时刻；没有给时取收到通知的时刻。
	PaidAt time.Time
	// Raw 是通知原文，落进 payment_orders.gateway_raw。
	Raw []byte
}

// ChannelOrder 是通道侧「这张单子现在怎么样」的答案。
type ChannelOrder struct {
	OrderNo      string
	Status       Status
	GatewayTxnID string
	PaidAt       *time.Time
}

// Channel 是一条支付通道要实现的全部动作。
type Channel interface {
	// Name 是通道标识，落进 payment_orders.gateway，也进 payment_gateway:<name> 科目。
	Name() string
	// CreateOrder 在通道侧建单。
	CreateOrder(ctx context.Context, req CreateOrderRequest) (CreatedOrder, error)
	// ParseNotification 校验签名并把通知解析成统一的形状。签名不对时返回
	// ErrBadSignature；字段缺失或取值不认识时返回 ErrInvalidNotification。
	ParseNotification(ctx context.Context, req NotificationRequest) (Notification, error)
	// QueryOrder 按商户单号问通道这张单子现在是什么状态。
	QueryOrder(ctx context.Context, orderNo string) (ChannelOrder, error)
}

// ErrOrderNotFound 表示按单号找不到这张支付单。
var ErrOrderNotFound = errors.New("没有这张支付单")

// ErrOrderConflict 表示同一个单号已经是一条内容不同的支付单。
//
// 与 ErrOrderNotFound 分开：那一行在，只是与这次说的不是同一件事。两者的
// 下一步也不同——前者先看下单那条命令，后者先看库里这一单原来是什么。
var ErrOrderConflict = errors.New("支付单已存在且内容不同")

// ErrGatewayTxnConflict 表示这笔通道流水号已经记在另一张支付单上。
//
// 与 ErrOrderConflict 分开：那一条说的是同一个单号下内容对不上，这一条说的是
// 两张不同的单争同一个流水号。两者都回 409，但下一步不同——前者先看下单参数，
// 后者要人工核这两张单：多半是通道重号，或者通知发错了单号。
var ErrGatewayTxnConflict = errors.New("通道流水号已经记在另一张支付单上")

// GatewayTxnConflictError 是 ErrGatewayTxnConflict 的落库形态，带上争用的
// 流水号与两张单的单号：排障的人第一眼就该知道要看哪两行。
type GatewayTxnConflictError struct {
	// OrderNo 是这次要入账、被拒的那张单。
	OrderNo string
	// Gateway 是这张单所属的通道。流水号只在同一个通道内判重，所以光有号码
	// 分不出是同一条通道重号还是两家通道各给了同一个号。
	Gateway string
	// GatewayTxnID 是两张单争的那个流水号。
	GatewayTxnID string
	// OtherOrderNo 是已经记下这个流水号的那张单。撞约束之后那一次补读没读到时
	// 为空（见 Store.annotateGatewayTxnConflict）。
	OtherOrderNo string
}

// Error 把两张单、通道与流水号连成一句。
//
// 认不出另一张单时只说「另一张」，不猜一个单号：猜错比说得含糊更坏。
func (e *GatewayTxnConflictError) Error() string {
	if e.OtherOrderNo != "" {
		return fmt.Sprintf("%v：通道 %s 上支付单 %s 的流水号 %s 已经记在支付单 %s 上",
			ErrGatewayTxnConflict, e.Gateway, e.OrderNo, e.GatewayTxnID, e.OtherOrderNo)
	}
	return fmt.Sprintf("%v：通道 %s 上支付单 %s 的流水号 %s 已经记在另一张支付单上",
		ErrGatewayTxnConflict, e.Gateway, e.OrderNo, e.GatewayTxnID)
}

// Unwrap 让 errors.Is(err, ErrGatewayTxnConflict) 成立。
func (e *GatewayTxnConflictError) Unwrap() error { return ErrGatewayTxnConflict }

// ErrBadSignature 表示通知的签名对不上。
//
// 它是**拒绝**，不是「再试一次」：签名不对说明这份通知不是通道发的，重试
// 只会让同一份伪造通知被处理第二次。
var ErrBadSignature = errors.New("支付通知签名不对")

// ErrInvalidNotification 表示通知的字段缺失、格式不对或取值不认识。
var ErrInvalidNotification = errors.New("支付通知不合法")

// ErrChannelDisabled 表示这条通道没有启用。
var ErrChannelDisabled = errors.New("支付通道没有启用")

// TxnIDForOrder 把商户单号确定性地映射成账本交易号。
//
// 账本那一侧的幂等判据是 `txn_id`（见 PostTransaction），而通道的通知会重复
// 到达，所以同一个单号必须在每次重算时都得到同一个交易号。与
// `larkaxctl wallet credit` 的键用不同的域前缀：同一张单据手工入账与走通道
// 入账是两笔互不相干的动作，共用一个域会让它们互相算成重放。
func TxnIDForOrder(orderNo string) ledger.TxnID {
	return txnIDForDomain("larkax:payment-topup:v1", orderNo)
}

// PlanTxnIDForOrder 把支付单号确定性地映射成**额度包售卖**那笔交易的账本交易号。
//
// 与 TxnIDForOrder 用不同的域前缀：同一张单子只可能是其中一种，但充值与售卖
// 是两种业务事件，共用一个域会让将来的对账把它们混成一类。
func PlanTxnIDForOrder(orderNo string) ledger.TxnID {
	return txnIDForDomain("larkax:plan-purchase:v1", orderNo)
}

// txnIDForDomain 是上面两条共用的映射：域前缀、分隔符与置位规则只有一份，
// 两条命令的 id 因此除了前缀之外逐字同形。
func txnIDForDomain(domain, orderNo string) ledger.TxnID {
	sum := sha256.Sum256([]byte(domain + "\x00" + orderNo))
	var b [16]byte
	copy(b[:], sum[:16])
	// 置版本位与变体位，使它能被任何标准 uuid 解析器读回（同 ledger.NewTxnID）。
	b[6] = (b[6] & 0x0f) | 0x80
	b[8] = (b[8] & 0x3f) | 0x80

	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])
	return ledger.TxnID(out[:])
}

// CallbackPath 是某条通道的支付回调路径。
//
// 它不在这四条推理路径里（见 docs/api.md）：由通道在付款完成后调用，认证靠
// 通知里的签名而不是 API Key。这条路径只在通道启用时才挂上。
func CallbackPath(channelName string) string {
	return CallbackPrefix + channelName
}

// CallbackPrefix 是支付回调路径的公共前缀。
const CallbackPrefix = "/v1/payments/callback/"

// validateCreateOrder 在下单之前把明显不成立的输入挡掉。
func validateCreateOrder(req CreateOrderRequest) error {
	if req.OrderNo == "" {
		return fmt.Errorf("缺少商户单号")
	}
	if req.UserID <= 0 {
		return fmt.Errorf("用户 id 非法: %d", req.UserID)
	}
	if !req.Asset.Valid() {
		return fmt.Errorf("资产 %q 未知", req.Asset)
	}
	if !req.Amount.IsPositive() {
		return fmt.Errorf("金额必须为正数，当前为 %s", req.Amount)
	}
	return nil
}

// validateNotification 检查通知里本包自己要用到的那几项。
//
// 已支付通知**不要求**带 GatewayTxnID：空串经 nullString 落成 NULL，而
// (gateway, gateway_txn_id) 那条部分唯一索引不含 NULL 行。也就是说，真实通道
// 在已支付通知里漏给流水号时，这条唯一性不会生效（也不会报错）。这是当前取舍
// 的边界，不是缺陷：有的通道付完款也不给号。
func validateNotification(n Notification) error {
	if n.OrderNo == "" {
		return fmt.Errorf("%w：缺少商户单号", ErrInvalidNotification)
	}
	if !validNotificationStatus(n.Status) {
		return fmt.Errorf("%w：状态 %q 不是支付通知会带的那几个", ErrInvalidNotification, n.Status)
	}
	if !n.Asset.Valid() {
		return fmt.Errorf("%w：资产 %q 未知", ErrInvalidNotification, n.Asset)
	}
	if !n.Amount.IsPositive() {
		return fmt.Errorf("%w：金额必须为正数，当前为 %s", ErrInvalidNotification, n.Amount)
	}
	if n.Fee.IsNegative() {
		return fmt.Errorf("%w：手续费为负: %s", ErrInvalidNotification, n.Fee)
	}
	return nil
}
