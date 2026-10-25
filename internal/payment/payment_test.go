package payment

import (
	"errors"
	"testing"

	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/money"
)

// validCreateRequest 是一份能过校验的下单请求。
func validCreateRequest() CreateOrderRequest {
	return CreateOrderRequest{
		OrderNo: "ORD-VALID-1",
		UserID:  7,
		Asset:   ledger.AssetCNY,
		Amount:  money.MustParse("1.00"),
	}
}

// TestValidateCreateOrder 逐项验下单前的拦截。
func TestValidateCreateOrder(t *testing.T) {
	base := validCreateRequest()
	cases := []struct {
		name string
		mut  func(*CreateOrderRequest)
	}{
		{"缺单号", func(r *CreateOrderRequest) { r.OrderNo = "" }},
		{"用户为 0", func(r *CreateOrderRequest) { r.UserID = 0 }},
		{"用户为负", func(r *CreateOrderRequest) { r.UserID = -1 }},
		{"资产未知", func(r *CreateOrderRequest) { r.Asset = ledger.Asset("USDT") }},
		{"金额为 0", func(r *CreateOrderRequest) { r.Amount = money.Zero }},
		{"金额为负", func(r *CreateOrderRequest) { r.Amount = money.MustParse("-1.00") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := base
			tc.mut(&req)
			if err := validateCreateOrder(req); err == nil {
				t.Error("应当拒绝")
			}
		})
	}
	if err := validateCreateOrder(base); err != nil {
		t.Errorf("合法请求被拒: %v", err)
	}
}

// TestValidateNotification 逐项验通知的拦截。
func TestValidateNotification(t *testing.T) {
	base := Notification{
		OrderNo: "ORD-VALID-1",
		Status:  StatusPaid,
		Amount:  money.MustParse("1.00"),
		Asset:   ledger.AssetCNY,
	}
	cases := []struct {
		name string
		mut  func(*Notification)
	}{
		{"缺单号", func(n *Notification) { n.OrderNo = "" }},
		{"状态是 created", func(n *Notification) { n.Status = StatusCreated }},
		{"状态不认识", func(n *Notification) { n.Status = Status("bogus") }},
		{"资产未知", func(n *Notification) { n.Asset = ledger.Asset("USDT") }},
		{"金额为 0", func(n *Notification) { n.Amount = money.Zero }},
		{"金额为负", func(n *Notification) { n.Amount = money.MustParse("-1.00") }},
		{"手续费为负", func(n *Notification) { n.Fee = money.MustParse("-0.01") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := base
			tc.mut(&n)
			if err := validateNotification(n); err == nil {
				t.Error("应当拒绝")
			}
		})
	}
	if err := validateNotification(base); err != nil {
		t.Errorf("合法通知被拒: %v", err)
	}
}

// TestChannelLookupRejectsMissingOrNil 验取通道的两条拒绝路径。
func TestChannelLookupRejectsMissingOrNil(t *testing.T) {
	service := NewService(NewStore(nil), nil, nil, map[string]Channel{
		"nil":   nil,
		"empty": &stubChannel{},
	})
	for _, name := range []string{"absent", "nil"} {
		_, err := service.channel(name)
		if !errors.Is(err, ErrChannelDisabled) {
			t.Errorf("channel(%q) 的错误 = %v，期望 ErrChannelDisabled", name, err)
		}
	}
	if _, err := service.channel("empty"); err != nil {
		t.Errorf("已装配的通道被拒: %v", err)
	}
}
