package channels

// 本文件是模型探测里**不需要数据库**的那一半：裸地址 + 凭证的形态、
// 清单的归一化、错误体里的凭证脱敏。从库里那条渠道取地址与凭证的形态
// 见 modelprobe_integration_test.go。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/channel"
)

// listingAdapter 是探测用的替身：记下它拿到的渠道，回一份给定的清单。
//
// 嵌 *stubAdapter：注册表要的另外几个方法它都不关心（见 admin_test.go）。
type listingAdapter struct {
	*stubAdapter
	models  []string
	err     error
	got     channel.Channel
	gotCall int
}

func (l *listingAdapter) ListModels(_ context.Context, ch channel.Channel) ([]string, error) {
	l.got = ch
	l.gotCall++
	return l.models, l.err
}

// probeRegistryWith 建一个只注册了被测替身的注册表。
func probeRegistryWith(t *testing.T, typ string, adapter *listingAdapter) *channel.Registry {
	t.Helper()
	adapter.stubAdapter = &stubAdapter{typ: typ}
	registry := channel.NewRegistry()
	if err := registry.Register(adapter); err != nil {
		t.Fatalf("注册替身失败: %v", err)
	}
	return registry
}

// TestNormalizeProbeModels 钉住归一化的四条：丢空名、去重、升序、
// 上限处截断（恰好等于上限不算截断）。
func TestNormalizeProbeModels(t *testing.T) {
	got := normalizeProbeModels([]string{"b", "a", "b", "", "c"})
	if !got.Supported || got.Truncated {
		t.Errorf("结果 = %+v，期望 supported 且未截断", got)
	}
	if want := []string{"a", "b", "c"}; strings.Join(got.Models, ",") != strings.Join(want, ",") {
		t.Errorf("清单 = %v，期望 %v（去重、升序、丢空名）", got.Models, want)
	}

	// 恰好等于上限：不截断。只测「超了会截断」的话，把上限写成 0 也能过。
	exact := make([]string, ProbeModelLimit)
	for i := range exact {
		exact[i] = "m" + itoa(int64(i))
	}
	if got := normalizeProbeModels(exact); got.Truncated || len(got.Models) != ProbeModelLimit ||
		got.Total != ProbeModelLimit {
		t.Errorf("恰好 %d 条时 = %d 条 / total %d / truncated=%v，期望全量且不截断",
			ProbeModelLimit, len(got.Models), got.Total, got.Truncated)
	}

	over := append(append([]string{}, exact...), "m-over")
	if got := normalizeProbeModels(over); !got.Truncated ||
		len(got.Models) != ProbeModelLimit || got.Total != ProbeModelLimit+1 {
		t.Errorf("超一条时 = %d 条 / total %d / truncated=%v，期望截到 %d、total 仍是 %d",
			len(got.Models), got.Total, got.Truncated, ProbeModelLimit, ProbeModelLimit+1)
	}
}

// TestProbeRawTarget 只给地址与凭证的形态：地址按创建渠道那条判据归一化，
// 凭证原样交给适配器。
func TestProbeRawTarget(t *testing.T) {
	adapter := &listingAdapter{models: []string{"z-model", "a-model"}}
	probe := NewModelProbe(nil, nil, probeRegistryWith(t, endpointChatType, adapter))

	res, err := probe.Probe(t.Context(), ProbeRequest{
		Type:       endpointChatType,
		BaseURL:    "https://api.example.com/v1/",
		Credential: "test-key-placeholder",
	})
	if err != nil {
		t.Fatalf("探测失败: %v", err)
	}
	if !res.Supported || strings.Join(res.Models, ",") != "a-model,z-model" {
		t.Errorf("结果 = %+v，期望 supported 且升序两条", res)
	}
	if adapter.got.BaseURL != "https://api.example.com/v1/" {
		t.Errorf("适配器拿到的地址 = %q，期望原样的合法地址（归一化只验证不改写）", adapter.got.BaseURL)
	}
	if adapter.got.Credential != "test-key-placeholder" {
		t.Errorf("适配器拿到的凭证 = %q，期望原样传入", adapter.got.Credential)
	}
}

// TestProbeUnsupported 上游没有列表能力不是错误：回 Supported=false、
// 空清单（不是 nil 切片，调用方直接序列化）。
func TestProbeUnsupported(t *testing.T) {
	adapter := &listingAdapter{err: channel.ErrUnsupported}
	probe := NewModelProbe(nil, nil, probeRegistryWith(t, endpointChatType, adapter))

	res, err := probe.Probe(t.Context(), ProbeRequest{
		Type:    endpointChatType,
		BaseURL: "https://api.example.com/v1",
	})
	if err != nil {
		t.Fatalf("不支持被当成了失败: %v", err)
	}
	if res.Supported {
		t.Error("supported = true，期望 false")
	}
	if res.Models == nil || len(res.Models) != 0 {
		t.Errorf("清单 = %v，期望空数组而不是 nil", res.Models)
	}
}

// TestProbeRedactsCredential 上游错误体里回显了凭证时，返回给调用方的错误
// 里不含它。
func TestProbeRedactsCredential(t *testing.T) {
	const credential = "sk-probe-placeholder"
	adapter := &listingAdapter{err: &channel.ErrUpstream{
		StatusCode:  401,
		GotResponse: true,
		Body:        `{"error":"invalid key ` + credential + `"}`,
	}}
	probe := NewModelProbe(nil, nil, probeRegistryWith(t, endpointChatType, adapter))

	_, err := probe.Probe(t.Context(), ProbeRequest{
		Type:       endpointChatType,
		BaseURL:    "https://api.example.com/v1",
		Credential: credential,
	})
	if err == nil {
		t.Fatal("上游失败没有返回错误")
	}
	if strings.Contains(err.Error(), credential) {
		t.Errorf("错误里带着凭证: %v", err)
	}
	var up *channel.ErrUpstream
	if !errors.As(err, &up) {
		t.Fatalf("错误链里没有 ErrUpstream: %v", err)
	}
	if !strings.Contains(up.Body, credentialPlaceholder) {
		t.Errorf("错误体 = %q，期望凭证被换成占位符", up.Body)
	}
	if up.StatusCode != 401 {
		t.Errorf("状态码 = %d，期望 401（脱敏不该动别的字段）", up.StatusCode)
	}
}

// TestProbeValidatesInput 两种形态各自缺什么就报什么。
func TestProbeValidatesInput(t *testing.T) {
	probe := NewModelProbe(nil, nil, probeRegistryWith(t, endpointChatType, &listingAdapter{}))

	cases := []struct {
		name string
		req  ProbeRequest
		want string
	}{
		{"没有 type", ProbeRequest{BaseURL: "https://api.example.com/v1"}, "type"},
		{"没有地址", ProbeRequest{Type: endpointChatType}, "base_url"},
		{"地址不是 URL", ProbeRequest{Type: endpointChatType, BaseURL: "not-a-url"}, "base_url"},
		{"未知类型", ProbeRequest{Type: "no-such-type", BaseURL: "https://api.example.com/v1"}, "渠道类型无效"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := probe.Probe(t.Context(), tc.req)
			if err == nil {
				t.Fatal("没有报错")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误 = %v，期望提到 %q", err, tc.want)
			}
		})
	}
}
