package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/channel"
)

// 错误体的形状与调用协议绑定（docs/versioning.md）：调用方的 SDK 按自己
// 那套去解，形状不对时它连错误类型都读不出来。
func TestErrorBodyShape(t *testing.T) {
	cases := []struct {
		name     string
		protocol channel.Protocol
		status   int
		wantType string
		wantCode string
	}{
		{name: "OpenAI 参数错", protocol: channel.ProtocolOpenAI, status: http.StatusBadRequest, wantType: "invalid_request_error"},
		{name: "OpenAI 凭证错", protocol: channel.ProtocolOpenAI, status: http.StatusUnauthorized, wantType: "invalid_request_error", wantCode: "invalid_api_key"},
		{name: "OpenAI 体过大", protocol: channel.ProtocolOpenAI, status: http.StatusRequestEntityTooLarge, wantType: "invalid_request_error", wantCode: "request_too_large"},
		{name: "OpenAI 上游错", protocol: channel.ProtocolOpenAI, status: http.StatusBadGateway, wantType: "api_error"},
		{name: "OpenAI 未实现", protocol: channel.ProtocolOpenAI, status: http.StatusNotImplemented, wantType: "not_implemented"},
		{name: "Anthropic 参数错", protocol: channel.ProtocolAnthropic, status: http.StatusBadRequest, wantType: "invalid_request_error"},
		{name: "Anthropic 凭证错", protocol: channel.ProtocolAnthropic, status: http.StatusUnauthorized, wantType: "authentication_error"},
		{name: "Anthropic 体过大", protocol: channel.ProtocolAnthropic, status: http.StatusRequestEntityTooLarge, wantType: "request_too_large"},
		{name: "Anthropic 限流", protocol: channel.ProtocolAnthropic, status: http.StatusTooManyRequests, wantType: "rate_limit_error"},
		{name: "Anthropic 上游错", protocol: channel.ProtocolAnthropic, status: http.StatusServiceUnavailable, wantType: "api_error"},
		{name: "Anthropic 未实现", protocol: channel.ProtocolAnthropic, status: http.StatusNotImplemented, wantType: "not_implemented"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := recordError(tc.protocol, tc.status)

			var body struct {
				Type  string `json:"type"`
				Error struct {
					Type    string  `json:"type"`
					Message string  `json:"message"`
					Param   *string `json:"param"`
					Code    *string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("响应不是 JSON: %v（%s）", err, rec.Body)
			}
			if body.Error.Type != tc.wantType {
				t.Errorf("错误类型 = %q，期望 %q", body.Error.Type, tc.wantType)
			}
			if body.Error.Message == "" {
				t.Error("错误体里应当有一句给调用方的话")
			}
			if got := deref(body.Error.Code); got != tc.wantCode {
				t.Errorf("code = %q，期望 %q", got, tc.wantCode)
			}

			// Anthropic 的形状多一个顶层 type，OpenAI 的没有。
			if tc.protocol == channel.ProtocolAnthropic {
				if body.Type != "error" {
					t.Errorf("Anthropic 错误体的顶层 type = %q，期望 error", body.Type)
				}
			} else if body.Type != "" {
				t.Errorf("OpenAI 错误体不该有顶层 type，实际 %q", body.Type)
			}

			if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
				t.Errorf("Content-Type = %q", ct)
			}
		})
	}
}

// retryAfterSeconds 把间隔换成整数秒：向上取整，至少 1 秒。
//
// `secs < 1` 上的边界变异（写成 `<= 1`）是**等价**的，不是缺口：守卫体做的事
// 就是「返回 1」，而 secs == 1 落进另一支返回的也正是 secs，也就是 1。
// 按 secs 的三段看（小于 1、等于 1、大于 1），两版对每一段都返回同一个值。
// 下面三段各给一个用例——其中「等于 1」那一格两版算出来是同一个数，它固定的
// 是换算结果，不是这两版的区别。
func TestRetryAfterSeconds(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   time.Duration
		want int64
	}{
		{name: "零", in: 0, want: 1},
		{name: "不足一秒", in: 100 * time.Millisecond, want: 1},
		{name: "负值", in: -3 * time.Second, want: 1},
		{name: "恰好一秒", in: time.Second, want: 1},
		{name: "一秒半", in: 1500 * time.Millisecond, want: 2},
		{name: "整两秒", in: 2 * time.Second, want: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryAfterSeconds(tc.in); got != tc.want {
				t.Errorf("retryAfterSeconds(%v) = %d，期望 %d", tc.in, got, tc.want)
			}
		})
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// showPtr 把一个可空值渲染成可读文本，供断言失败时打印。
//
// 直接 %v 一个指向基本类型的指针打出来的是地址（`0xc000...`），而报错信息
// 本身是要给人读的——那串地址看不出实际值是什么，也就看不出这次失败是
// 「值算错了」还是「字段根本没给」。
func showPtr[T any](p *T) string {
	if p == nil {
		return "<nil>"
	}
	return fmt.Sprint(*p)
}
