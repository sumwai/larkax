package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/sumwai/larkax/internal/keyquota"
	"github.com/sumwai/larkax/internal/money"
)

// fakeQuota 是一个可编程的额度检查器，用来把「放行 / 拒绝」这一侧固定住。
//
// 它同时记下收到的参数：额度按 key 计，入口必须把**这把 key** 的 id 与它的
// 额度上限原样传下去。传错（传用户 ID、传了另一个字段、或把 NULL 当成一个
// 金额传下去）在别的用例里看不出来——判定结果取决于实现，而这里是桩。
type fakeQuota struct {
	err error

	calls int
	keyID int64
	limit *money.Amount
}

func (f *fakeQuota) CheckQuota(_ context.Context, keyID int64, limit *money.Amount) error {
	f.calls++
	f.keyID = keyID
	f.limit = limit
	return f.err
}

// quotaBody 是两套协议共同的错误体形状里本文件要断言的部分。
//
// Code 是指针：OpenAI 那一侧它必须存在且等于 insufficient_quota，Anthropic
// 那一侧它必须**不出现**（码写在 error.type 里）。用 *string 才分得开
// 「字段缺失」与「字段是空串」。
type quotaBody struct {
	Type  string `json:"type"`
	Error struct {
		Type    string  `json:"type"`
		Message string  `json:"message"`
		Code    *string `json:"code"`
	} `json:"error"`
}

// TestQuotaExceededIsRejectedBeforeTheBodyIsRead 钉住入口这一层的四件事：
// 429、机器可读的码、**没有** Retry-After、以及没有走到上游。
//
// 请求体故意给一段不合法的 JSON。它在别处会让请求得到 400，所以拿到 429
// 就说明额度检查确实排在那一次解析之前——这一条是「省掉一次请求体读入」
// 这个理由能不能成立的唯一证据，否则两层顺序调换时没有任何用例会失败。
//
// 「没有走到上游」是最要紧的一件：上游那一侧就是预留发生的地方，被额度拒掉
// 的请求走到那里等于先把钱冻住、再把它拒掉。
func TestQuotaExceededIsRejectedBeforeTheBodyIsRead(t *testing.T) {
	limit := money.MustParse("100")

	cases := []struct {
		name         string
		path         string
		wantTopType  string
		wantErrType  string
		wantCodeSeen bool
	}{
		{"OpenAI 协议", PathChatCompletions, "", "rate_limit_error", true},
		{"Anthropic 协议", PathMessages, "error", "insufficient_quota", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, auth, chatter := newTestHandler()
			auth.user = User{ID: 7, KeyID: 42, QuotaLimit: &limit}
			quota := &fakeQuota{err: fmt.Errorf("%w: 已花 120，上限 100", keyquota.ErrExceeded)}
			h.Quota = quota

			rec := post(h, tc.path, `{"model":`)

			if rec.Code != http.StatusTooManyRequests {
				t.Fatalf("状态码 = %d，期望 429。响应体：%s", rec.Code, rec.Body)
			}
			if got := rec.Header().Get(HeaderRetryAfter); got != "" {
				t.Errorf("Retry-After = %q，期望不出现：额度不会自己回来，"+
					"回一个等待时间是在骗客户端", got)
			}
			if chatter.calls != 0 {
				t.Errorf("被额度拒掉的请求走到了上游 %d 次：它不该先冻住余额", chatter.calls)
			}
			if quota.calls != 1 {
				t.Fatalf("额度检查被调用 %d 次，期望 1", quota.calls)
			}
			// 传下去的是**这把 key** 的 id 与它的额度上限，不是用户 id，也不是别的字段。
			if quota.keyID != 42 {
				t.Errorf("额度检查收到的 keyID = %d，期望 42", quota.keyID)
			}
			// 比字符串而不是比指针：传下去的必须是**这一列的原样值**，
			// 而 money.Amount 的文本形式是定长 8 位小数。
			if quota.limit == nil || quota.limit.String() != "100.00000000" {
				t.Errorf("额度检查收到的上限 = %s，期望 100.00000000", showPtr(quota.limit))
			}

			var body quotaBody
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("解析错误体失败: %v（原文 %s）", err, rec.Body)
			}
			if body.Type != tc.wantTopType {
				t.Errorf("顶层 type = %q，期望 %q", body.Type, tc.wantTopType)
			}
			if body.Error.Type != tc.wantErrType {
				t.Errorf("error.type = %q，期望 %q", body.Error.Type, tc.wantErrType)
			}
			if tc.wantCodeSeen {
				if body.Error.Code == nil || *body.Error.Code != CodeInsufficientQuota {
					t.Errorf("error.code = %s，期望 %q", showPtr(body.Error.Code), CodeInsufficientQuota)
				}
			} else if body.Error.Code != nil {
				t.Errorf("Anthropic 的错误体里不该有 error.code，得到 %q", *body.Error.Code)
			}
			if body.Error.Message == "" {
				t.Error("error.message 为空：调用方需要一句话说明该改什么")
			}
		})
	}
}

// TestQuotaCheckerFailureFailsOpen 钉住「查不出来时放行」。
//
// 这一支是刻意的，不是漏了错误处理：权威判定读的是同一个库，这一次读不出来
// 而那一次读得出来时，超限的请求照样会在那里被拒；两次都读不出来时，预留
// 本身就失败了，请求不会因此被放过去。
//
// 反过来在这里拒掉，一次数据库抖动就会表现为「全站额度突然用尽」——而那与
// 「有人真的把额度花完了」是两个方向完全相反的排查。
func TestQuotaCheckerFailureFailsOpen(t *testing.T) {
	limit := money.MustParse("100")

	h, auth, chatter := newTestHandler()
	auth.user = User{ID: 7, KeyID: 42, QuotaLimit: &limit}
	h.Quota = &fakeQuota{err: errors.New("连不上数据库")}

	rec := post(h, PathChatCompletions, validBody)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200：查不出来时该放行。响应体：%s", rec.Code, rec.Body)
	}
	if chatter.calls != 1 {
		t.Errorf("上游被调用 %d 次，期望 1", chatter.calls)
	}
}

// TestQuotaNotWiredSkipsTheCheck 钉住「为 nil 就不做这道早拒绝」这个默认。
//
// 它与 Idempotency 的取舍相同：没接上这条链路时，行为与这道检查出现之前
// 完全一样，而权威判定仍然在。反过来（nil 时回 501）会让一次装配疏漏表现
// 为「所有请求都失败」。
func TestQuotaNotWiredSkipsTheCheck(t *testing.T) {
	h, _, chatter := newTestHandler()
	if h.Quota != nil {
		t.Fatal("新构造的 Handler 不该自带额度检查：接线由装配层做")
	}

	if rec := post(h, PathChatCompletions, validBody); rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200。响应体：%s", rec.Code, rec.Body)
	}
	if chatter.calls != 1 {
		t.Errorf("上游被调用 %d 次，期望 1", chatter.calls)
	}
}

// TestReservationQuotaRefusalBecomesInsufficientQuota 钉住**权威判定**被拒之后
// 回给调用方的是什么。
//
// 这一条与上面那条入口检查是两条不同的路：上面那条由入口自己判、自己写响应；
// 这一条的判定发生在预留事务里（落库层），错误一路穿过网关回到入口。两条路
// 必须给出**同一个**状态码与码，否则同一个事实在两次请求里会得到两种答复，
// 而调用方只能按其中一种写代码。
//
// 流式也走一遍：那一条路上错误必须在响应头发出去**之前**变成 429，否则就
// 只剩截断一条路。
func TestReservationQuotaRefusalBecomesInsufficientQuota(t *testing.T) {
	refusal := fmt.Errorf("预留落库失败: %w", keyquota.ErrExceeded)

	cases := []struct {
		name       string
		path       string
		body       string
		streaming  bool
		wantErrTyp string
	}{
		{"OpenAI 非流式", PathChatCompletions, validBody, false, "rate_limit_error"},
		{"Anthropic 非流式", PathMessages, validBody, false, "insufficient_quota"},
		{"OpenAI 流式", PathChatCompletions, streamBody, true, "rate_limit_error"},
		{"Anthropic 流式", PathMessages, streamBody, true, "insufficient_quota"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, auth, chatter := newTestHandler()
			// 入口那一层放行，把判定整个交给预留：这一条测的正是那条路。
			limit := money.MustParse("100")
			auth.user = User{ID: 7, KeyID: 42, QuotaLimit: &limit}
			h.Quota = &fakeQuota{}
			if tc.streaming {
				chatter.streamErr = refusal
			} else {
				chatter.err = refusal
			}

			rec := post(h, tc.path, tc.body)

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
			if body.Error.Type != tc.wantErrTyp {
				t.Errorf("error.type = %q，期望 %q", body.Error.Type, tc.wantErrTyp)
			}
			if tc.path == PathChatCompletions {
				if body.Error.Code == nil || *body.Error.Code != CodeInsufficientQuota {
					t.Errorf("error.code = %s，期望 %q",
						showPtr(body.Error.Code), CodeInsufficientQuota)
				}
			}
			// 上游那次调用返回的就是这个错误，所以「被调用过」是预期的；
			// 要钉的是它**没有**被当成 5xx——那会让调用方去查我们，
			// 而该改的是那把 key。
			if tc.streaming && chatter.streamCalls != 1 {
				t.Errorf("流式上游被调用 %d 次，期望 1", chatter.streamCalls)
			}
		})
	}
}
