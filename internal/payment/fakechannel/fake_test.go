package fakechannel

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/ledger"
	ledgerstore "github.com/sumwai/larkax/internal/ledger/store"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/payment"
	"github.com/sumwai/larkax/internal/testdb"
)

// TestMain 给这个包建一个独立的库（见 internal/testdb）。
func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

// signedBody 把通知签好名再序列化成请求体。
func signedBody(t *testing.T, c *Channel, body notificationBody) []byte {
	t.Helper()
	body.Sign = c.sign(body)
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("序列化通知失败: %v", err)
	}
	return raw
}

func validBody() notificationBody {
	return notificationBody{
		OrderNo: "ORD-FAKE-1",
		TradeNo: "TRADE-FAKE-1",
		Amount:  "66.00",
		Asset:   "CNY",
		Status:  "paid",
		Fee:     "0.66",
		PaidAt:  "2026-10-20T08:30:00Z",
	}
}

// TestSignedNotificationRoundTrips 验签名方案本身。
func TestSignedNotificationRoundTrips(t *testing.T) {
	ctx := context.Background()
	c := New()
	body := validBody()
	raw := signedBody(t, c, body)

	n, err := c.ParseNotification(ctx, payment.NotificationRequest{Body: raw})
	if err != nil {
		t.Fatalf("解析签过名的通知失败: %v", err)
	}
	if n.OrderNo != body.OrderNo || n.GatewayTxnID != body.TradeNo {
		t.Errorf("单号/流水 = %q/%q，期望 %q/%q",
			n.OrderNo, n.GatewayTxnID, body.OrderNo, body.TradeNo)
	}
	if n.Status != payment.StatusPaid {
		t.Errorf("状态 = %q，期望 paid", n.Status)
	}
	if n.Amount.Cmp(money.MustParse("66.00")) != 0 || n.Fee.Cmp(money.MustParse("0.66")) != 0 {
		t.Errorf("金额/手续费 = %s/%s，期望 66.00/0.66", n.Amount, n.Fee)
	}
	if n.Asset != ledger.AssetCNY {
		t.Errorf("资产 = %q，期望 CNY", n.Asset)
	}
	if got := n.PaidAt.UTC(); !got.Equal(time.Date(2026, 10, 20, 8, 30, 0, 0, time.UTC)) {
		t.Errorf("支付时刻 = %v", got)
	}
}

// TestTamperedBodyIsRejected 验「签名不对就拒绝」。
func TestTamperedBodyIsRejected(t *testing.T) {
	ctx := context.Background()
	c := New()
	raw := signedBody(t, c, validBody())

	var body notificationBody
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("读回通知失败: %v", err)
	}
	body.Amount = "0.01"
	tampered, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("序列化篡改后的通知失败: %v", err)
	}

	_, err = c.ParseNotification(ctx, payment.NotificationRequest{Body: tampered})
	if !errors.Is(err, payment.ErrBadSignature) {
		t.Fatalf("错误 = %v，期望 ErrBadSignature", err)
	}
}

// TestFixedVectorSignature 用写死的 body 与签名验一遍。
//
// 上面那条往返用例两边都由被测的 sign 生成，算错了也会互相抵消；这一条把
// 待签名串与 HMAC 的结果都写死，换实现时能从两个方向发现偏差。
func TestFixedVectorSignature(t *testing.T) {
	const raw = `{"order_no":"ORD-FIXED-1","trade_no":"TRADE-FIXED-1",` +
		`"amount":"66.00","asset":"CNY","status":"paid","fee":"0.66",` +
		`"paid_at":"2026-10-20T08:30:00Z",` +
		`"sign":"1b31744c24cff6665245b697bda672d3ab3c4a8ef8c60140194e654ce977c36a"}`

	n, err := New().ParseNotification(context.Background(), payment.NotificationRequest{Body: []byte(raw)})
	if err != nil {
		t.Fatalf("解析固定向量失败: %v", err)
	}
	if n.OrderNo != "ORD-FIXED-1" || n.GatewayTxnID != "TRADE-FIXED-1" ||
		n.Amount.Cmp(money.MustParse("66.00")) != 0 || n.Fee.Cmp(money.MustParse("0.66")) != 0 {
		t.Errorf("固定向量解析结果不对: %+v", n)
	}
}

// TestMissingSignatureIsInvalid 验「没带签名」落 400 那一档。
func TestMissingSignatureIsInvalid(t *testing.T) {
	body := validBody()
	body.Sign = ""
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("序列化通知失败: %v", err)
	}

	_, parseErr := New().ParseNotification(context.Background(), payment.NotificationRequest{Body: raw})
	if !errors.Is(parseErr, payment.ErrInvalidNotification) {
		t.Fatalf("错误 = %v，期望 ErrInvalidNotification", parseErr)
	}
}

// TestMissingFieldIsRejectedBeforeSignature 验必填字段的拒绝先于签名比较。
func TestMissingFieldIsRejectedBeforeSignature(t *testing.T) {
	c := New()
	body := validBody()
	body.TradeNo = ""
	raw := signedBody(t, c, body)

	_, err := c.ParseNotification(context.Background(), payment.NotificationRequest{Body: raw})
	if !errors.Is(err, payment.ErrInvalidNotification) {
		t.Fatalf("错误 = %v，期望 ErrInvalidNotification", err)
	}
}

// TestMalformedSignatureIsRejected 验「签名不是十六进制」。
func TestMalformedSignatureIsRejected(t *testing.T) {
	c := New()
	body := validBody()
	body.Sign = "not-hex"
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("序列化通知失败: %v", err)
	}

	_, parseErr := c.ParseNotification(context.Background(), payment.NotificationRequest{Body: raw})
	if !errors.Is(parseErr, payment.ErrBadSignature) {
		t.Fatalf("错误 = %v，期望 ErrBadSignature", parseErr)
	}
}

// TestCanonicalEscapesFieldSeparators 验规范化串里的转义。
//
// 不转义时，下面两组字段会拼出同一个待签名串：只要把 `&` 与 `=` 写进字段值，
// 一份通知的尾部就能伪装成下一份的头部。
func TestCanonicalEscapesFieldSeparators(t *testing.T) {
	first := canonical(notificationBody{
		OrderNo: "x&trade_no=y", TradeNo: "z", Amount: "1", Asset: "CNY",
		Status: "paid", Fee: "0", PaidAt: "",
	})
	second := canonical(notificationBody{
		OrderNo: "x", TradeNo: "y&trade_no=z", Amount: "1", Asset: "CNY",
		Status: "paid", Fee: "0", PaidAt: "",
	})
	if first == second {
		t.Fatalf("两组不同字段拼出了同一个待签名串: %q", first)
	}
	if !bytes.Contains([]byte(first), []byte("%26")) {
		t.Errorf("待签名串没有转义 &：%q", first)
	}
}

// TestCreateOrderReturnsExampleURL 验下单回执的形状。
func TestCreateOrderReturnsExampleURL(t *testing.T) {
	c := New()
	created, err := c.CreateOrder(context.Background(), payment.CreateOrderRequest{
		OrderNo: "ORD-FAKE-1", UserID: 1, Asset: ledger.AssetCNY, Amount: money.MustParse("66.00"),
	})
	if err != nil {
		t.Fatalf("下单失败: %v", err)
	}
	if created.OrderNo != "ORD-FAKE-1" {
		t.Errorf("单号 = %q", created.OrderNo)
	}
	const prefix = "https://pay.example.com/fake/"
	if len(created.PayURL) <= len(prefix) || created.PayURL[:len(prefix)] != prefix {
		t.Errorf("支付地址 = %q，期望以 %q 开头", created.PayURL, prefix)
	}
	if !json.Valid(created.Raw) {
		t.Errorf("下单原文不是合法 JSON: %s", created.Raw)
	}
}

// TestQueryOrderNeedsLookup 验「没接查询来源就报错」。
func TestQueryOrderNeedsLookup(t *testing.T) {
	_, err := New().QueryOrder(context.Background(), "ORD-FAKE-1")
	if err == nil {
		t.Fatal("没有接查询来源时应当报错")
	}
}

// TestQueryOrderUsesLookup 验查询读的是注入的那一行。
func TestQueryOrderUsesLookup(t *testing.T) {
	want := payment.ChannelOrder{OrderNo: "ORD-FAKE-1", Status: payment.StatusPaid}
	c := New(WithLookup(func(_ context.Context, orderNo string) (payment.ChannelOrder, error) {
		if orderNo != want.OrderNo {
			t.Errorf("查询单号 = %q，期望 %q", orderNo, want.OrderNo)
		}
		return want, nil
	}))

	got, err := c.QueryOrder(context.Background(), want.OrderNo)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if got.OrderNo != want.OrderNo || got.Status != want.Status {
		t.Errorf("查询结果 = %+v，期望 %+v", got, want)
	}
}

// 每个用例用独立的用户 id；测试库每次运行都重建，所以一个进程内计数器就够。
var userSeq atomic.Int64

// seedUser 建一个用户与一个 0 余额的钱包行。
func seedUser(t *testing.T, ctx context.Context, db *sql.DB) int64 {
	t.Helper()
	id := 800000 + userSeq.Add(1)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO users (id, username, email) VALUES ($1, $2, $3)`,
		id, "fake"+strconv.FormatInt(id, 10), "fake"+strconv.FormatInt(id, 10)+"@example.com"); err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO user_wallets (user_id, asset, balance, frozen) VALUES ($1, 'CNY', 0, 0)`,
		id); err != nil {
		t.Fatalf("建钱包失败: %v", err)
	}
	return id
}

// postNotification 把一份通知交给回调入口。
func postNotification(t *testing.T, handler http.Handler, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, payment.CallbackPath(Name), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// TestCallbackEndToEnd 从下单走到回调入账，再用同一份通知重放一次。
//
// 它穿的是真实的那条路：假通道的签名方案、Handler 的解析、Service 的顺序
// 与账本的写入全在链上。前一版的单测各自只覆盖一段，接起来时仍可能在
// 「签名算的是哪几个字段」这种地方对不上。
func TestCallbackEndToEnd(t *testing.T) {
	ctx := context.Background()
	db := testdb.Open(t)
	userID := seedUser(t, ctx, db)

	orders := payment.NewStore(db)
	channel := New(WithLookup(orders.ChannelOrder))
	service := payment.NewService(orders, ledgerstore.New(db), nil, map[string]payment.Channel{Name: channel})
	handler := payment.NewHandler(service, Name, "test-version")

	if _, err := service.Create(ctx, Name, payment.CreateOrderRequest{
		OrderNo: "ORD-FAKE-1", UserID: userID, Asset: ledger.AssetCNY,
		Amount: money.MustParse("66.00"),
	}); err != nil {
		t.Fatalf("下单失败: %v", err)
	}

	body := signedBody(t, channel, validBody())
	rec := postNotification(t, handler, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("回调状态码 = %d，期望 200（响应体 %s）", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(payment.HeaderVersion); got != "test-version" {
		t.Errorf("%s = %q，期望 test-version", payment.HeaderVersion, got)
	}
	var resp struct {
		OK      bool   `json:"ok"`
		Applied bool   `json:"applied"`
		OrderNo string `json:"order_no"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("读回调响应失败: %v（响应体 %s）", err, rec.Body.String())
	}
	if !resp.OK || !resp.Applied || resp.OrderNo != "ORD-FAKE-1" || resp.Status != string(payment.StatusPaid) {
		t.Errorf("回调响应 = %+v，期望 ok/applied/paid", resp)
	}

	paid, err := orders.Get(ctx, "ORD-FAKE-1")
	if err != nil {
		t.Fatalf("读支付单失败: %v", err)
	}
	if paid.Status != payment.StatusPaid || paid.GatewayTxnID != "TRADE-FAKE-1" {
		t.Errorf("支付单 = %+v，期望 paid 且记下通道流水", paid)
	}

	txnID := payment.TxnIDForOrder("ORD-FAKE-1")
	var entries int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM ledger_entries WHERE txn_id = $1`, string(txnID)).Scan(&entries); err != nil {
		t.Fatalf("数分录失败: %v", err)
	}
	if entries != 4 {
		t.Errorf("分录 %d 条，期望 4 条（用户、通道、上游成本、平台收入）", entries)
	}
	var balance string
	if err := db.QueryRowContext(ctx,
		`SELECT balance::text FROM user_wallets WHERE user_id = $1 AND asset = 'CNY'`,
		userID).Scan(&balance); err != nil {
		t.Fatalf("读余额失败: %v", err)
	}
	if balance != money.MustParse("66.00").String() {
		t.Errorf("余额 = %s，期望 66.00", balance)
	}

	// 同一份通知再来一次：200，但 applied=false，分录与余额都不动。
	replay := postNotification(t, handler, body)
	if replay.Code != http.StatusOK {
		t.Fatalf("重放状态码 = %d，期望 200（响应体 %s）", replay.Code, replay.Body.String())
	}
	if !bytes.Contains(replay.Body.Bytes(), []byte(`"applied":false`)) {
		t.Errorf("重放响应应当说 applied=false：%s", replay.Body.String())
	}
	var entriesAgain int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM ledger_entries WHERE txn_id = $1`, string(txnID)).Scan(&entriesAgain); err != nil {
		t.Fatalf("重放后数分录失败: %v", err)
	}
	if entriesAgain != entries {
		t.Errorf("重放后分录 %d 条，重放前 %d 条", entriesAgain, entries)
	}
}

// TestCallbackRejectsTamperedNotification 验回调入口把签名错误映射成 401。
func TestCallbackRejectsTamperedNotification(t *testing.T) {
	db := testdb.Open(t)

	orders := payment.NewStore(db)
	channel := New(WithLookup(orders.ChannelOrder))
	service := payment.NewService(orders, ledgerstore.New(db), nil, map[string]payment.Channel{Name: channel})
	handler := payment.NewHandler(service, Name, "")

	body := validBody()
	raw := signedBody(t, channel, body)
	var tampered notificationBody
	if err := json.Unmarshal(raw, &tampered); err != nil {
		t.Fatalf("读回通知失败: %v", err)
	}
	tampered.Amount = "0.01"
	raw, err := json.Marshal(tampered)
	if err != nil {
		t.Fatalf("序列化篡改后的通知失败: %v", err)
	}

	rec := postNotification(t, handler, raw)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d，期望 401（响应体 %s）", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("signature mismatch")) {
		t.Errorf("响应体 = %s，期望 signature mismatch", rec.Body.String())
	}
}

// TestCallbackRejectsInvalidNotification 验「签名对得上、通知本身不成立」落到 400。
//
// 签名由通道自己生成，所以这一条穿的是完整的那条路：解析成功之后由
// Service 的校验拦下，再由 Handler 映射成 400。只在错误映射那一层写用例
// 证明不了这条路径——那里传进去的是一个手工造的错误。
func TestCallbackRejectsInvalidNotification(t *testing.T) {
	db := testdb.Open(t)

	orders := payment.NewStore(db)
	channel := New(WithLookup(orders.ChannelOrder))
	service := payment.NewService(orders, ledgerstore.New(db), nil, map[string]payment.Channel{Name: channel})
	handler := payment.NewHandler(service, Name, "")

	body := validBody()
	body.Amount = "0"
	raw := signedBody(t, channel, body)

	rec := postNotification(t, handler, raw)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400（响应体 %s）", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("金额")) {
		t.Errorf("响应体 = %s，期望点出金额那一项", rec.Body.String())
	}
}
