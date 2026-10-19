// Package fakechannel 是一条**只用于本地联调**的支付通道。
//
// # 它替换的是哪一半
//
// 真实通道对外表现为三个动作：建单、把付款结果通知回来、按单号查状态。
// 本包按同一形状实现这三个动作，签名与字段名照公开文档里常见的那几项取，
// 于是接真实通道时换的是这个包，`internal/payment` 那一层的编排不动。
//
// # 它为什么不能当成真通道
//
// 密钥是一个写在源码里的**公开占位值**（`SecretPlaceholder`），任何读过这个
// 仓库的人都能给任意单号签出一份「已支付」的通知。所以它默认关闭（见迁移
// 00044 的 `payment.fake_enabled`），打开它的部署必须明确知道自己是在做
// 本地联调。真实通道的密钥应由 `channels` 表的信封加密给出，绝不进源码。
//
// # 查询为什么读本地那一行
//
// 假通道背后没有另一个系统。按单号「查通道状态」时，它读的是本服务记下的
// 那一行（由调用方注入 `Lookup`），这是这个模拟里唯一存在的「通道账本」。
// 真实适配器在这一步发 HTTP 请求，不经过这里。
package fakechannel

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/payment"
)

// Name 是这条通道的标识，落进 payment_orders.gateway 与 payment_gateway:fake 科目。
const Name = "fake"

// SecretPlaceholder 是签名用的占位密钥。
//
// **它不是秘密**：这个值在公开仓库里，任何人都能用它签出一份通知。它只让
// 「签名校验这条代码路径」在本地联调时真的跑起来，不代表这条通道有认证能力。
const SecretPlaceholder = "test-webhook-secret-placeholder"

// payURLBase 是假支付地址的前缀。
//
// 用 example.com 这一保留域名，避免这条通道被误当成某个真实支付页。
const payURLBase = "https://pay.example.com/fake"

// orderTTL 是假通道给一张单子的有效期。
const orderTTL = 30 * time.Minute

// Lookup 按商户单号取本服务记下的那一行。
//
// 假通道没有自己的账本，查询只能读这里（见包注释）。
type Lookup func(ctx context.Context, orderNo string) (payment.ChannelOrder, error)

// Option 是构造假通道时的可选项。
type Option func(*Channel)

// WithLookup 接上「按单号查状态」的来源。
func WithLookup(lookup Lookup) Option {
	return func(c *Channel) { c.lookup = lookup }
}

// Channel 是假通道的实现。
type Channel struct {
	secret string
	lookup Lookup
}

// New 构造假通道。密钥固定取公开的占位值。
//
// 没有「传一个密钥进来」的选项：那会让人以为这条通道可以配置成有认证能力的，
// 而它不能。
func New(opts ...Option) *Channel {
	c := &Channel{secret: SecretPlaceholder}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Name 实现 payment.Channel。
func (c *Channel) Name() string { return Name }

// CreateOrder 在假通道侧建单。
//
// 它不保存任何状态：单子的全部信息在本服务那一行里，支付结果由通知带回来。
func (c *Channel) CreateOrder(_ context.Context, req payment.CreateOrderRequest) (payment.CreatedOrder, error) {
	raw, err := json.Marshal(map[string]string{
		"channel":    Name,
		"order_no":   req.OrderNo,
		"pay_url":    payURLBase + "/" + url.PathEscape(req.OrderNo),
		"expires_at": time.Now().Add(orderTTL).UTC().Format(time.RFC3339),
	})
	if err != nil {
		return payment.CreatedOrder{}, fmt.Errorf("构造假通道下单响应失败: %w", err)
	}
	return payment.CreatedOrder{
		OrderNo: req.OrderNo,
		PayURL:  payURLBase + "/" + url.PathEscape(req.OrderNo),
		Raw:     raw,
	}, nil
}

// notificationBody 是假通道通知的 JSON 形状。
//
// 字段照公开文档里常见的那几项取：商户单号、通道流水号、金额、资产、状态、
// 手续费、支付时刻与签名。
type notificationBody struct {
	OrderNo string `json:"order_no"`
	TradeNo string `json:"trade_no"`
	Amount  string `json:"amount"`
	Asset   string `json:"asset"`
	Status  string `json:"status"`
	Fee     string `json:"fee"`
	PaidAt  string `json:"paid_at"`
	Sign    string `json:"sign"`
}

// ParseNotification 校验 HMAC-SHA256 签名并解析通知。
func (c *Channel) ParseNotification(_ context.Context, req payment.NotificationRequest) (payment.Notification, error) {
	var body notificationBody
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return payment.Notification{}, fmt.Errorf("%w：请求体不是合法 JSON: %w",
			payment.ErrInvalidNotification, err)
	}
	if body.OrderNo == "" || body.TradeNo == "" || body.Amount == "" ||
		body.Asset == "" || body.Status == "" {
		return payment.Notification{}, fmt.Errorf(
			"%w：order_no、trade_no、amount、asset 与 status 都必填", payment.ErrInvalidNotification)
	}
	if body.Sign == "" {
		// 缺签名与签名不对分开：前者是通知本身不完整（400），后者是
		// 「这份通知不是通道发的」（401）。
		return payment.Notification{}, fmt.Errorf("%w：缺少 sign", payment.ErrInvalidNotification)
	}
	expected := c.sign(body)
	provided, err := hex.DecodeString(body.Sign)
	if err != nil {
		return payment.Notification{}, payment.ErrBadSignature
	}
	if !hmac.Equal(provided, mustDecode(expected)) {
		return payment.Notification{}, payment.ErrBadSignature
	}

	amount, err := money.Parse(body.Amount)
	if err != nil {
		return payment.Notification{}, fmt.Errorf("%w：amount %q 读不出来: %w",
			payment.ErrInvalidNotification, body.Amount, err)
	}
	feeText := body.Fee
	if feeText == "" {
		feeText = "0"
	}
	fee, err := money.Parse(feeText)
	if err != nil {
		return payment.Notification{}, fmt.Errorf("%w：fee %q 读不出来: %w",
			payment.ErrInvalidNotification, body.Fee, err)
	}
	var paidAt time.Time
	if body.PaidAt != "" {
		paidAt, err = time.Parse(time.RFC3339, body.PaidAt)
		if err != nil {
			return payment.Notification{}, fmt.Errorf("%w：paid_at %q 不是 RFC3339 时刻: %w",
				payment.ErrInvalidNotification, body.PaidAt, err)
		}
	}

	return payment.Notification{
		OrderNo:      body.OrderNo,
		GatewayTxnID: body.TradeNo,
		Status:       payment.Status(body.Status),
		Amount:       amount,
		Asset:        ledger.Asset(body.Asset),
		Fee:          fee,
		PaidAt:       paidAt,
		Raw:          req.Body,
	}, nil
}

// QueryOrder 按单号问「通道」这张单子现在是什么状态。
func (c *Channel) QueryOrder(ctx context.Context, orderNo string) (payment.ChannelOrder, error) {
	if c.lookup == nil {
		return payment.ChannelOrder{}, errors.New("假通道没有接上查询来源")
	}
	return c.lookup(ctx, orderNo)
}

// sign 算出通知的签名。
//
// 逐字段做 URL 转义再用 `&` 拼起来：字段值里出现 `&` 或 `=` 时，不转义会让
// 两份不同的通知拼出同一个待签名串。
func (c *Channel) sign(body notificationBody) string {
	mac := hmac.New(sha256.New, []byte(c.secret))
	_, _ = mac.Write([]byte(canonical(body)))
	return hex.EncodeToString(mac.Sum(nil))
}

// canonical 是待签名的规范化串。字段顺序固定，不依赖 map 的遍历顺序。
func canonical(body notificationBody) string {
	parts := []string{
		"order_no=" + url.QueryEscape(body.OrderNo),
		"trade_no=" + url.QueryEscape(body.TradeNo),
		"amount=" + url.QueryEscape(body.Amount),
		"asset=" + url.QueryEscape(body.Asset),
		"status=" + url.QueryEscape(body.Status),
		"fee=" + url.QueryEscape(body.Fee),
		"paid_at=" + url.QueryEscape(body.PaidAt),
	}
	return strings.Join(parts, "&")
}

// mustDecode 解开一个必然合法的十六进制串。
func mustDecode(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic("fakechannel: 自己算出来的签名不是十六进制")
	}
	return b
}
