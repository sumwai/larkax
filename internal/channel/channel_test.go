package channel

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/quota"
)

// Adapter.Quota() 的返回类型必须是 quota.State。
//
// 写成编译期断言而不是运行期检查：签名换成别的类型（或退回一个本包自己的
// 快照类型）之后是构建失败，而不是某条用例报一句看不懂的错。
//
// 为什么值得钉：额度状态是路由与监控共用的一个东西，本包再声明一个只会
// 变成它的**有损投影**——投影里没有 last_probe_at / last_error_code，
// 而正是这两列让 quota.State.Routable 能区分「从未观测过」（可用）与
// 「观测过却没拿到数值」（排除）。判据退化不会让任何东西报错，只会让
// 一批渠道静默地不再被选中。
var _ interface {
	Quota(context.Context, Channel) (quota.State, error)
} = Adapter(nil)

// fakeAdapter 是注册表测试用的最小实现。
type fakeAdapter struct {
	typ      string
	protocol Protocol
	// endpoint 非空时**覆盖** Protocol().Endpoint()，用来造一个「声明的协议与
	// 自述的端点对不上」的替身（见 Registry.Register 的端点自检）。留空表示
	// 按协议推——真实适配器走的就是那条路。
	endpoint Endpoint
}

func (f *fakeAdapter) Type() string       { return f.typ }
func (f *fakeAdapter) Protocol() Protocol { return f.protocol }

func (f *fakeAdapter) Endpoint() Endpoint {
	if f.endpoint != "" {
		return f.endpoint
	}
	return f.protocol.Endpoint()
}

func (f *fakeAdapter) Chat(context.Context, Channel, *Request) (*Response, error) {
	return nil, ErrUnsupported
}

func (f *fakeAdapter) ChatStream(context.Context, Channel, *Request) (<-chan StreamEvent, error) {
	return nil, ErrUnsupported
}

func (f *fakeAdapter) ListModels(context.Context, Channel) ([]string, error) {
	return nil, ErrUnsupported
}

func (f *fakeAdapter) Quota(context.Context, Channel) (quota.State, error) {
	return quota.State{}, ErrUnsupported
}

// mustRegister 用替身占住一个类型名。
//
// 类型名只能取协议取值——注册表拒收别的名字（见 Registry.Register 的类型名自检）
// ——所以下面这些用例都从 protocolNames 里挑名字。它们测的是注册表的机制
// （查得到、去重、排序、报错时列出来），那件事与名字长什么样无关。
func mustRegister(t *testing.T, r *Registry, typ string, p Protocol) {
	t.Helper()
	if err := r.Register(&fakeAdapter{typ: typ, protocol: p}); err != nil {
		t.Fatalf("Register(%q) 出错: %v", typ, err)
	}
}

func TestRegistryRegisterAndLookup(t *testing.T) {
	r := NewRegistry()
	a := &fakeAdapter{typ: string(ProtocolOpenAI), protocol: ProtocolOpenAI}
	if err := r.Register(a); err != nil {
		t.Fatalf("Register 出错: %v", err)
	}

	got, err := r.Lookup(string(ProtocolOpenAI))
	if err != nil {
		t.Fatalf("Lookup 出错: %v", err)
	}
	if got != Adapter(a) {
		t.Error("取回的适配器不是注册进去的那个")
	}
}

// 覆盖注册会让「两个适配器抢一个名字」表现为其中一个静默失效，
// 而失效的那个通常是新加的、本该生效的那个。
//
// 两个替身声明的协议不同，是为了钉住「撞的是**类型名**这一列」：若判据是
// 「类型加协议」，那两条不同协议的适配器就会被放进去，而库里只有一列 type。
func TestRegistryRejectsDuplicateType(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, string(ProtocolOpenAI), ProtocolOpenAI)
	if err := r.Register(&fakeAdapter{
		typ: string(ProtocolOpenAI), protocol: ProtocolAnthropic,
	}); err == nil {
		t.Error("重复注册应报错")
	}
}

func TestRegistryRejectsInvalidAdapter(t *testing.T) {
	cases := map[string]Adapter{
		"nil 适配器":  nil,
		"未声明类型":    &fakeAdapter{typ: "", protocol: ProtocolOpenAI},
		"未知协议":     &fakeAdapter{typ: string(ProtocolOpenAI), protocol: "gemini"},
		"空协议":      &fakeAdapter{typ: string(ProtocolOpenAI), protocol: ""},
		"类型名不是协议名": &fakeAdapter{typ: "my-vendor", protocol: ProtocolOpenAI},
	}
	for name, a := range cases {
		t.Run(name, func(t *testing.T) {
			if err := NewRegistry().Register(a); err == nil {
				t.Error("应报错")
			}
		})
	}
}

// 类型名不是一个协议名时要报出来，且说清三件事：它声明的类型、它声明的协议
// （那才是改的目标）、本包认的取值有哪些。只报一句「不符合」的话，改的人
// 不知道该往哪个方向改。
func TestRegistryRejectsNonProtocolType(t *testing.T) {
	r := NewRegistry()
	err := r.Register(&fakeAdapter{typ: "my-vendor", protocol: ProtocolAnthropic})
	if err == nil {
		t.Fatal("类型名不是协议名时注册应当失败")
	}
	// 声明的协议那一处连着引号一起判：光判取值有没有出现的话，协议取值本来就
	// 在「认的只有…」那一串里，去掉这个子句用例照样绿。
	want := append([]string{"my-vendor", `声明的协议是 "anthropic"`}, protocolNames...)
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("报错里没出现 %q：%v", w, err)
		}
	}
	// 失败之后那一条**没有**被注册进去：留在注册表里等于把「启动时报错」
	// 变成「这台机器上一直有一条端点评据失效的渠道」。
	if _, lookupErr := r.Lookup("my-vendor"); lookupErr == nil {
		t.Error("自检失败的适配器不该留在注册表里")
	}
}

// 未知类型要报错并列出已知类型。调用方拿到 nil 接口的话，第一次调用时才崩，
// 而那时现场是「某个请求 panic」而不是「这个渠道类型没接」。
func TestLookupUnknownTypeListsKnown(t *testing.T) {
	r := NewRegistry()
	known := []string{string(ProtocolAnthropic), string(ProtocolOpenAI), string(ProtocolSystemOne)}
	for _, typ := range known {
		mustRegister(t, r, typ, Protocol(typ))
	}

	_, err := r.Lookup("nope")
	if err == nil {
		t.Fatal("应报错")
	}
	if !errors.Is(err, ErrUnknownType) {
		t.Errorf("期望 ErrUnknownType，得到 %v", err)
	}
	for _, want := range known {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息未列出已知类型 %q: %v", want, err)
		}
	}
}

func TestTypesIsSorted(t *testing.T) {
	r := NewRegistry()
	// 注册的顺序与期望的顺序不同：这条用例判的是 Types() 会排序。
	for _, typ := range []string{
		string(ProtocolSystemOne), string(ProtocolOpenAI), string(ProtocolAnthropic),
	} {
		mustRegister(t, r, typ, Protocol(typ))
	}

	got := r.Types()
	want := []string{string(ProtocolAnthropic), string(ProtocolOpenAI), string(ProtocolSystemOne)}
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
//
// 写那个 goroutine 只能在那几个协议名上轮着来——注册表不收别的名字了（见
// Registry.Register 的类型名自检），所以这里没有十六个各不相同的键。这条用例
// 在意的是**并发**：一个 goroutine 写 map、十六个读它，竞态与键有几个无关。
func TestRegistryConcurrentLookupAndRegister(t *testing.T) {
	r := NewRegistry()
	mustRegister(t, r, string(ProtocolOpenAI), ProtocolOpenAI)

	done := make(chan struct{})
	for i := 0; i < 16; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 100; j++ {
				if _, err := r.Lookup(string(ProtocolOpenAI)); err != nil {
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
			p := Protocol(protocolNames[j%len(protocolNames)])
			_ = r.Register(&fakeAdapter{typ: string(p), protocol: p})
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

// 每一个有适配器实现的协议取值都要在这里出现。多认一个没人实现的取值，
// 会让「以为支持了」这件事发生；反过来，**少列一个已实现的取值**会让这道门
// 对新增的那套协议毫无覆盖——它照样通过，而它看起来像在守「取值集合」。
func TestProtocolValuesAreKnown(t *testing.T) {
	for _, p := range []Protocol{
		ProtocolOpenAI, ProtocolAnthropic, ProtocolOpenAIResponses, ProtocolSystemOne,
	} {
		if !p.valid() {
			t.Errorf("协议 %q 应有效", p)
		}
	}
	if Protocol("gemini").valid() {
		t.Error("未知协议不应有效")
	}
	// custom 曾经是第三个取值，但没有任何适配器产出它，已删掉：
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
