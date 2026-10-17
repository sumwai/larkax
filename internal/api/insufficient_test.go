package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/billing"
	"github.com/sumwai/larkax/internal/keyquota"
	"github.com/sumwai/larkax/internal/ledger/store"
)

// 本文件把「账户状态」这一类失败在三条入口上的处置一起钉住。
//
// 三件事必须同时成立，而它们分别会被不同的改法弄坏：
//
//   - 预留时余额不足与按 Key 的额度用尽给**同一套**码（调用方只写一个分支）；
//   - 两者共用状态码与码，但文案各自说清该做什么（改额度 / 充值）——文案
//     错了不会让任何 SDK 出错，只会让人照着做一件没用的事；
//   - 结算欠费（ErrInsufficientBalance）与缺上界（ErrNoUpperBound）不被这次
//     改动带偏——前者仍是 500（账本内部不变量出问题），后者仍是 400。
//
// 三条入口都要过一遍：chat（stream=false）、流式（stream=true）与决策。
// 流式那条的错误发生在 ChatStream 返回之前（预留就在 begin 里），所以此时
// 一个字节都还没写出去，仍能改状态码——这与入口代码里那条「头发出之后只能
// 截断」的分界是同一件事，本文件证明的是「预留失败落在分界之前」。

// entryCase 是一条入口的参数：把错误放在哪、发什么请求。
type entryCase struct {
	name    string
	path    string
	body    string
	install func(h *Handler, err error)
}

// entries 是三条入口。错误都从「调上游」这一步冒出来，也就是预留失败后
// 网关原样交回入口的那个位置。
func entries() []entryCase {
	return []entryCase{
		{
			name: "chat 非流式",
			path: PathChatCompletions,
			body: validBody,
			install: func(h *Handler, err error) {
				h.Chatter = &fakeChatter{err: err}
			},
		},
		{
			name: "chat 流式",
			path: PathChatCompletions,
			body: streamBody,
			install: func(h *Handler, err error) {
				h.Chatter = &fakeChatter{streamErr: err}
			},
		},
		{
			name: "决策",
			path: PathSystemOne,
			body: validDecisionBody,
			install: func(h *Handler, err error) {
				h.Decider = &fakeDecider{err: err}
			},
		},
	}
}

// reservationInsufficient 与生产上一样包一层：判据要能穿过网关那层包装
// （gateway.begin 的「预留落库失败: %w」），否则它只在测试里成立。
func reservationInsufficient() error {
	return fmt.Errorf("预留落库失败: %w: 余额 0.00000000，已冻结 0.00000000",
		store.ErrInsufficientAvailable)
}

// TestInsufficientAvailableIsInsufficientQuota 钉住本 issue 的主判据：
// 预留时余额不足回 429 + insufficient_quota，且**不**回 Retry-After。
//
// 同时断言错误体里没有内部原文：ErrInsufficientAvailable 的文本带着用户
// 余额与冻结额，那是我们账本内部的事实，不该回给调用方。
func TestInsufficientAvailableIsInsufficientQuota(t *testing.T) {
	err := reservationInsufficient()

	for _, e := range entries() {
		t.Run(e.name, func(t *testing.T) {
			h, _, _ := newTestHandler()
			e.install(h, err)

			rec := post(h, e.path, e.body)

			if rec.Code != http.StatusTooManyRequests {
				t.Fatalf("状态码 = %d，期望 429。响应体：%s", rec.Code, rec.Body)
			}
			if got := rec.Header().Get(HeaderRetryAfter); got != "" {
				t.Errorf("Retry-After = %q，期望不出现：余额不会因为等一会儿就回来", got)
			}

			var body quotaBody
			if uerr := json.Unmarshal(rec.Body.Bytes(), &body); uerr != nil {
				t.Fatalf("解析错误体失败: %v（原文 %s）", uerr, rec.Body)
			}
			// 决策入口没有 Anthropic 形状，统一取 OpenAI 那一套（见
			// systemoneErrorProtocol），所以码在 error.code 上。
			if body.Error.Code == nil || *body.Error.Code != CodeInsufficientQuota {
				t.Errorf("error.code = %s，期望 %q", showPtr(body.Error.Code), CodeInsufficientQuota)
			}
			if strings.Contains(body.Error.Message, "已冻结") ||
				strings.Contains(rec.Body.String(), "0.00000000") {
				t.Errorf("错误体里带出了账本内部原文（余额 / 冻结额）：%s", rec.Body)
			}
			// 文案要给对下一步：见 TestAccountStatusErrorsShareTheSameCode
			// 里针对余额那一句的断言。这里只钉「不是额度那句」——把余额
			// 写成「改 key 额度」是一种静默的错误建议，没有 SDK 会因此报错。
			if strings.Contains(body.Error.Message, "spending limit") {
				t.Errorf("余额不足用了额度用尽的文案，给出的处置是错的：%q", body.Error.Message)
			}
		})
	}
}

// TestAccountStatusErrorsShareTheSameCode 是 #200 那一条形状的回归：
// 两种「账户状态」的失败给出**同一个**状态码与码，只有文案不同。
//
// 文案不同是刻意的，所以这里不是逐字比较，而是比较它们**必须相同**的那些
// 字段，再分别断言文案指向了正确的处置。把余额那一条也写成「改 key 额度」
// 不会让任何用例失败，除非这里钉住它。
func TestAccountStatusErrorsShareTheSameCode(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantInMsg string
		notInMsg  string
	}{
		{
			name:      "按 Key 的额度用尽",
			err:       fmt.Errorf("预留落库失败: %w", keyquota.ErrExceeded),
			wantInMsg: "spending limit",
			notInMsg:  "balance",
		},
		{
			name:      "钱包可用余额不足",
			err:       reservationInsufficient(),
			wantInMsg: "balance",
			notInMsg:  "spending limit",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _ := newTestHandler()
			h.Chatter = &fakeChatter{err: tc.err}

			rec := post(h, PathChatCompletions, validBody)
			if rec.Code != http.StatusTooManyRequests {
				t.Fatalf("状态码 = %d，期望 429。响应体：%s", rec.Code, rec.Body)
			}
			if got := rec.Header().Get(HeaderRetryAfter); got != "" {
				t.Errorf("Retry-After = %q，期望不出现", got)
			}

			var body quotaBody
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("解析错误体失败: %v（原文 %s）", err, rec.Body)
			}
			if body.Error.Code == nil || *body.Error.Code != CodeInsufficientQuota {
				t.Fatalf("error.code = %s，期望 %q", showPtr(body.Error.Code), CodeInsufficientQuota)
			}
			if body.Error.Type != "rate_limit_error" {
				t.Errorf("error.type = %q，期望 rate_limit_error（与限流同一落点）", body.Error.Type)
			}
			if !strings.Contains(body.Error.Message, tc.wantInMsg) {
				t.Errorf("文案没说到该做什么（期望含 %q）：%q", tc.wantInMsg, body.Error.Message)
			}
			if strings.Contains(body.Error.Message, tc.notInMsg) {
				t.Errorf("文案给出了错误的处置（不该含 %q）：%q", tc.notInMsg, body.Error.Message)
			}
		})
	}
}

// TestInsufficientBalanceStaysInternalError 钉住结算欠费**不**被这次改动带走。
//
// ErrInsufficientBalance 说的是「结算扣款会让余额变成负数」——那是账本内部
// 不变量出了问题的信号（预留本该挡住它），不是「用户现在没钱了」。把它归到
// 429 会让一次内部故障表现为「去充值」，方向正好反了。
//
// 改动前它落进 httpStatusOfUpstream 的默认分支，回 500 与一句 internal error。
// 这里逐字钉住那个结局：状态码、文案，以及错误体里没有码。
func TestInsufficientBalanceStaysInternalError(t *testing.T) {
	err := fmt.Errorf("结算落库失败: %w: 用户 7 的 CNY 余额不足以扣 1.00000000",
		store.ErrInsufficientBalance)

	for _, e := range entries() {
		t.Run(e.name, func(t *testing.T) {
			h, _, _ := newTestHandler()
			e.install(h, err)

			rec := post(h, e.path, e.body)

			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("状态码 = %d，期望 500（结算欠费是内部不变量问题，不是账户状态）。响应体：%s",
					rec.Code, rec.Body)
			}
			if got := rec.Header().Get(HeaderRetryAfter); got != "" {
				t.Errorf("Retry-After = %q，期望不出现", got)
			}

			var body quotaBody
			if uerr := json.Unmarshal(rec.Body.Bytes(), &body); uerr != nil {
				t.Fatalf("解析错误体失败: %v（原文 %s）", uerr, rec.Body)
			}
			if body.Error.Message != "internal error" {
				t.Errorf("error.message = %q，期望 internal error（与改动前逐字相同）",
					body.Error.Message)
			}
			if body.Error.Code != nil {
				t.Errorf("内部错误不该带机器可读的码，得到 %q", *body.Error.Code)
			}
			if strings.Contains(rec.Body.String(), "1.00000000") {
				t.Errorf("错误体带出了内部金额原文：%s", rec.Body)
			}
		})
	}
}

// TestOtherRefusalsKeepTheirStatus 是这次改动的两条防带偏判据：
// 缺上界仍是 400，认不出来的错误仍是 500。
//
// 它们挨着新加的分支，而「新分支的 errors.Is 写宽了」正是会把这两条拖下水
// 的改法——例如误写成 errors.Is(err, someBroadSentinel) 或漏掉 return。
func TestOtherRefusalsKeepTheirStatus(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
	}{
		{
			name:   "缺 max_tokens 仍是 400",
			err:    fmt.Errorf("%w: max_tokens 必须为正，当前 %d", billing.ErrNoUpperBound, 0),
			status: http.StatusBadRequest,
		},
		{
			name:   "认不出来的错误仍是 500",
			err:    errors.New("账本写不进去"),
			status: http.StatusInternalServerError,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _ := newTestHandler()
			h.Chatter = &fakeChatter{err: tc.err}

			rec := post(h, PathChatCompletions, validBody)
			if rec.Code != tc.status {
				t.Errorf("状态码 = %d，期望 %d（响应体 %s）", rec.Code, tc.status, rec.Body)
			}
		})
	}
}
