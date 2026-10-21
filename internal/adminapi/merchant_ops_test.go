package adminapi

import (
	"encoding/json"
	"sort"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/channels"
)

// 本文件是运营页那两条端点的纯单测：时区切日、异常理由、上游分组与
// 「响应里没有凭证」的 key 集合。真库能说的那部分在 merchant_ops_integration_test.go。

func TestTodayWindowIsTheSitesCalendarDay(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("加载时区失败: %v", err)
	}
	now := time.Date(2026, 10, 22, 0, 30, 0, 0, loc)
	from, to := todayWindow(now, loc)

	wantStart := time.Date(2026, 10, 22, 0, 0, 0, 0, loc)
	if !from.Equal(wantStart) {
		t.Fatalf("起点 = %v，期望 %v", from, wantStart)
	}
	if !to.Equal(wantStart.AddDate(0, 0, 1)) {
		t.Errorf("终点 = %v，期望次日零点", to)
	}

	// 跨零点这条是时区口径的唯一证据：当地昨天 23:5x 的用量不算今天。
	justBefore := time.Date(2026, 10, 21, 23, 55, 0, 0, loc)
	justAfter := time.Date(2026, 10, 22, 0, 5, 0, 0, loc)
	if !justBefore.Before(from) {
		t.Errorf("当地昨天 23:55（%v）落进了今天，起点是 %v", justBefore, from)
	}
	if justAfter.Before(from) || !justAfter.Before(to) {
		t.Errorf("当地今天 00:05（%v）不在 [%v, %v) 里", justAfter, from, to)
	}
}

func TestUnhealthyReasons(t *testing.T) {
	probeError := "error"
	probeOK := "ok"
	cases := []struct {
		name string
		h    channels.ChannelHealth
		want []string
	}{
		{
			name: "健康渠道一条理由都没有",
			h: channels.ChannelHealth{
				Requests: 100, Successes: 100, Health: "healthy",
				LastProbeStatus: &probeOK,
			},
			want: nil,
		},
		{
			name: "失败率过阈值",
			h:    channels.ChannelHealth{Requests: 100, Failures: 25, Health: "healthy"},
			want: []string{reasonFailureRate},
		},
		{
			name: "样本不足不判失败率",
			h:    channels.ChannelHealth{Requests: 5, Failures: 5, Health: "healthy"},
			want: nil,
		},
		{
			name: "额度状态降级",
			h:    channels.ChannelHealth{Requests: 100, Health: "degraded"},
			want: []string{reasonQuotaState},
		},
		{
			name: "额度耗尽",
			h:    channels.ChannelHealth{Requests: 100, Health: "drained"},
			want: []string{reasonQuotaState},
		},
		{
			name: "最近探测失败",
			h:    channels.ChannelHealth{Requests: 100, Health: "healthy", LastProbeStatus: &probeError},
			want: []string{reasonProbeFailed},
		},
		{
			name: "连续失败",
			h:    channels.ChannelHealth{Requests: 100, Health: "healthy", ConsecutiveFailures: 2},
			want: []string{reasonConsecutiveFailures},
		},
		{
			name: "24 小时无请求",
			h:    channels.ChannelHealth{Health: "healthy"},
			want: []string{reasonNoRequests},
		},
		{
			name: "多条理由一起列",
			h: channels.ChannelHealth{
				Requests: 100, Failures: 50, Health: "drained",
				ConsecutiveFailures: 1, LastProbeStatus: &probeError,
			},
			want: []string{reasonFailureRate, reasonQuotaState, reasonProbeFailed,
				reasonConsecutiveFailures},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := unhealthyReasons(tc.h)
			if len(got) != len(tc.want) {
				t.Fatalf("理由 = %v，期望 %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("理由 = %v，期望 %v", got, tc.want)
				}
			}
		})
	}
}

func TestGroupUpstreamsByTypeAndBaseURL(t *testing.T) {
	infos := []channels.ChannelInfo{
		{ID: 3, Name: "b-2", Type: "openai", BaseURL: "https://b.example.com/v1", Status: "enabled"},
		{ID: 1, Name: "a-1", Type: "openai", BaseURL: "https://a.example.com/v1", Status: "enabled"},
		{ID: 2, Name: "a-2", Type: "openai", BaseURL: "https://a.example.com/v1", Status: "disabled"},
	}
	health := map[int64]channels.ChannelHealth{
		1: {ID: 1, Health: "healthy", Requests: 10, Failures: 1},
		2: {ID: 2, Health: "degraded", Requests: 5, Failures: 4},
		3: {ID: 3, Health: "drained", Requests: 3, Failures: 0},
	}
	groups := groupUpstreams(infos, health)
	if len(groups) != 2 {
		t.Fatalf("分组数 = %d，期望 2：%+v", len(groups), groups)
	}
	// 排序固定为 (type, base_url)：a 在 b 前，与渠道 id 的输入次序无关。
	if groups[0].BaseURL != "https://a.example.com/v1" || groups[1].BaseURL != "https://b.example.com/v1" {
		t.Fatalf("分组顺序 = %q, %q", groups[0].BaseURL, groups[1].BaseURL)
	}
	a := groups[0]
	if len(a.Channels) != 2 {
		t.Fatalf("同一 base_url 的渠道数 = %d，期望 2", len(a.Channels))
	}
	// 组内按 id 升序。
	if a.Channels[0].ID != 1 || a.Channels[1].ID != 2 {
		t.Errorf("组内渠道顺序 = %+v，期望按 id 升序", a.Channels)
	}
	// 健康取组内最差一档；请求数与失败数合计。
	if a.Health != "degraded" {
		t.Errorf("组健康 = %q，期望 degraded（最差一档）", a.Health)
	}
	if a.Requests != 15 || a.Failures != 5 {
		t.Errorf("组合计 = %d 请求 / %d 失败，期望 15 / 5", a.Requests, a.Failures)
	}
	if a.FailureRate < 0.33 || a.FailureRate > 0.34 {
		t.Errorf("组失败率 = %v，期望约 0.333", a.FailureRate)
	}
}

func TestGroupUpstreamsUnknownIsWorseThanHealthy(t *testing.T) {
	infos := []channels.ChannelInfo{
		{ID: 1, Type: "openai", BaseURL: "https://x.example.com", Status: "enabled"},
		{ID: 2, Type: "openai", BaseURL: "https://x.example.com", Status: "enabled"},
	}
	groups := groupUpstreams(infos, map[int64]channels.ChannelHealth{
		1: {ID: 1, Health: "healthy"},
		2: {ID: 2, Health: "unknown"},
	})
	if len(groups) != 1 || groups[0].Health != "unknown" {
		t.Fatalf("组健康 = %+v，期望 unknown", groups)
	}
}

func TestParseUpstreamLimit(t *testing.T) {
	cases := []struct {
		raw     string
		want    int
		wantErr bool
	}{
		{"", upstreamDefaultLimit, false},
		{"1", 1, false},
		{"200", 200, false},
		{"0", 0, true},
		{"201", 0, true},
		{"abc", 0, true},
	}
	for _, tc := range cases {
		got, err := parseUpstreamLimit(tc.raw)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseUpstreamLimit(%q) 错误 = %v，期望出错 = %v", tc.raw, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && got != tc.want {
			t.Errorf("parseUpstreamLimit(%q) = %d，期望 %d", tc.raw, got, tc.want)
		}
	}
}

// TestUpstreamJSONKeySet 逐键断言：响应里没有凭证或任何未列出的字段。
//
// 「看起来没有」不算——Hint、CredentialKeys 都在同一个 ChannelInfo 上，
// 直接序列化它就会带出去。这里比对的是**完整的键集合**，多一个键就失败。
func TestUpstreamJSONKeySet(t *testing.T) {
	groups := groupUpstreams([]channels.ChannelInfo{{
		ID: 1, Name: "ch", Type: "openai", BaseURL: "https://api.example.com/v1",
		Status: "enabled",
		Hint:   "abcd", CredentialKeys: []string{"k1"},
	}}, map[int64]channels.ChannelHealth{1: {ID: 1, Health: "healthy"}})

	raw, err := json.Marshal(groups)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var decoded []map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	wantTop := []string{"base_url", "channels", "failures", "failure_rate",
		"health", "requests", "type"}
	assertExactKeys(t, "上游", decoded[0], wantTop)

	channelsRaw, ok := decoded[0]["channels"].([]any)
	if !ok || len(channelsRaw) != 1 {
		t.Fatalf("channels = %v", decoded[0]["channels"])
	}
	channel, ok := channelsRaw[0].(map[string]any)
	if !ok {
		t.Fatalf("channel = %v", channelsRaw[0])
	}
	assertExactKeys(t, "上游里的渠道", channel, []string{"health", "id", "name", "status"})
}

// assertExactKeys 比对键集合，多一个少一个都失败。
func assertExactKeys(t *testing.T, what string, obj map[string]any, want []string) {
	t.Helper()
	got := make([]string, 0, len(obj))
	for k := range obj {
		got = append(got, k)
	}
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("%s 的键 = %v，期望 %v", what, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s 的键 = %v，期望 %v", what, got, want)
		}
	}
}
