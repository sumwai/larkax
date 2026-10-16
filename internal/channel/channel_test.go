package channel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// fakeAdapter 是注册表测试用的最小实现。
type fakeAdapter struct {
	typ      string
	protocol Protocol
}

func (f *fakeAdapter) Type() string       { return f.typ }
func (f *fakeAdapter) Protocol() Protocol { return f.protocol }

func (f *fakeAdapter) Chat(context.Context, Channel, *Request) (*Response, error) {
	return nil, ErrUnsupported
}

func (f *fakeAdapter) ChatStream(context.Context, Channel, *Request) (<-chan StreamEvent, error) {
	return nil, ErrUnsupported
}

func (f *fakeAdapter) ListModels(context.Context, Channel) ([]string, error) {
	return nil, ErrUnsupported
}

func (f *fakeAdapter) Quota(context.Context, Channel) (Quota, error) {
	return Quota{}, ErrUnsupported
}

func mustRegister(t *testing.T, r *Registry, typ string, p Protocol) {
	t.Helper()
	if err := r.Register(&fakeAdapter{typ: typ, protocol: p}); err != nil {
		t.Fatalf("Register(%q) 出错: %v", typ, err)
	}
}

func TestRegistryRegisterAndLookup(t *testing.T) {
	r := NewRegistry()
	a := &fakeAdapter{typ: "example", protocol: ProtocolOpenAI}
	if err := r.Register(a); err != nil {
		t.Fatalf("Register 出错: %v", err)
	}

	got, err := r.Lookup("example")
	if err != nil {
		t.Fatalf("Lookup 出错: %v", err)
	}
	if got != Adapter(a) {
		t.Error("取回的适配器不是注册进去的那个")
	}
}

// 覆盖注册会让「两个适配器抢一个名字」表现为其中一个静默失效，
// 而失效的那个通常是新加的、本该生效的那个。
func TestRegistryRejectsDuplicateType(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, "dup", ProtocolOpenAI)
	if err := r.Register(&fakeAdapter{typ: "dup", protocol: ProtocolAnthropic}); err == nil {
		t.Error("重复注册应报错")
	}
}

func TestRegistryRejectsInvalidAdapter(t *testing.T) {
	cases := map[string]Adapter{
		"nil 适配器": nil,
		"未声明类型":   &fakeAdapter{typ: "", protocol: ProtocolOpenAI},
		"未知协议":    &fakeAdapter{typ: "x", protocol: "gemini"},
		"空协议":     &fakeAdapter{typ: "y", protocol: ""},
	}
	for name, a := range cases {
		t.Run(name, func(t *testing.T) {
			if err := NewRegistry().Register(a); err == nil {
				t.Error("应报错")
			}
		})
	}
}

// 未知类型要报错并列出已知类型。调用方拿到 nil 接口的话，第一次调用时才崩，
// 而那时现场是「某个请求 panic」而不是「这个渠道类型没接」。
func TestLookupUnknownTypeListsKnown(t *testing.T) {
	r := NewRegistry()
	for _, typ := range []string{"bbb", "aaa", "ccc"} {
		mustRegister(t, r, typ, ProtocolOpenAI)
	}

	_, err := r.Lookup("nope")
	if err == nil {
		t.Fatal("应报错")
	}
	if !errors.Is(err, ErrUnknownType) {
		t.Errorf("期望 ErrUnknownType，得到 %v", err)
	}
	for _, want := range []string{"aaa", "bbb", "ccc"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息未列出已知类型 %q: %v", want, err)
		}
	}
}

func TestTypesIsSorted(t *testing.T) {
	r := NewRegistry()
	for _, typ := range []string{"zebra", "alpha", "middle"} {
		mustRegister(t, r, typ, ProtocolOpenAI)
	}

	got := r.Types()
	want := []string{"alpha", "middle", "zebra"}
	if len(got) != len(want) {
		t.Fatalf("得到 %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("得到 %v，期望 %v", got, want)
		}
	}
}

// 注册表会被并发查询：每个请求都要查一次。
func TestRegistryConcurrentLookupAndRegister(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, "base", ProtocolOpenAI)

	done := make(chan struct{})
	for i := 0; i < 16; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 100; j++ {
				if _, err := r.Lookup("base"); err != nil {
					t.Errorf("Lookup 出错: %v", err)
					return
				}
				_ = r.Types()
			}
		}()
	}
	go func() {
		defer func() { done <- struct{}{} }()
		for j := 0; j < 16; j++ {
			_ = r.Register(&fakeAdapter{typ: fmt.Sprintf("extra%02d", j), protocol: ProtocolOpenAI})
		}
	}()
	for i := 0; i < 17; i++ {
		<-done
	}
}

func TestChannelSupports(t *testing.T) {
	ch := Channel{Models: []string{"gpt-4o", "claude-sonnet"}}
	if !ch.Supports("gpt-4o") {
		t.Error("应支持 gpt-4o")
	}
	if ch.Supports("gpt-4o-mini") {
		t.Error("不应支持未列出的模型")
	}
	if (Channel{}).Supports("anything") {
		t.Error("空模型列表不应支持任何模型")
	}
}

// 协议的取值只有这两套：调用方的入口只有两条路径，已注册的适配器也只有两个。
// 多认一个没人实现的取值，会让「以为支持了」这件事发生。
func TestProtocolValuesAreKnown(t *testing.T) {
	for _, p := range []Protocol{ProtocolOpenAI, ProtocolAnthropic} {
		if !p.valid() {
			t.Errorf("协议 %q 应有效", p)
		}
	}
	if Protocol("gemini").valid() {
		t.Error("未知协议不应有效")
	}
	// custom 曾经是第三个取值，没有任何消费者，已删掉：
	// 适配器只能说它实现的是哪套现成的协议。
	if Protocol("custom").valid() {
		t.Error("custom 已不再是合法协议")
	}
}

// ErrUpstream 的错误文本不应包含响应体——上游可能在错误信息里回显请求内容，
// 而错误文本会进日志，日志的去向比密钥本身更难控制。
func TestErrUpstreamDoesNotIncludeBody(t *testing.T) {
	e := &ErrUpstream{StatusCode: 429, Body: "slow down, your key is sk-abc123", Retryable: true}
	if strings.Contains(e.Error(), "sk-abc123") {
		t.Errorf("错误文本里含响应体: %s", e.Error())
	}
	if !strings.Contains(e.Error(), "429") {
		t.Errorf("错误文本应含状态码: %s", e.Error())
	}
}

func TestErrUpstreamUnwrap(t *testing.T) {
	e := &ErrUpstream{Err: ErrUnsupported}
	if !errors.Is(e, ErrUnsupported) {
		t.Error("应能 unwrap 到底层错误")
	}
	if e.Error() == "" {
		t.Error("Error() 不应为空")
	}
	// 没有状态码也没有底层错误时仍要给出可读文本
	if (&ErrUpstream{}).Error() == "" {
		t.Error("Error() 不应为空")
	}
}
