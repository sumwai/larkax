// Package upstream 把协议适配器装配起来，并按渠道类型分发请求。
//
// 它只做两件事：构造时把各协议适配器注册进 channel.Registry，调用时按
// channel.Channel.Type 查表转调。协议转换在适配器里，选路与重试在
// internal/route 里，这里两者都不做。
//
// 单独一个包而不是写进 internal/channel：channel 是纯数据契约（接口与统一
// 类型），而装配必须 import 具体的适配器实现。混在一起会让「只想用类型
// 定义」的地方被迫拖上全部适配器，而那正是 httpx 从 channel 里分出去的理由。
//
// 装配与执行器合在一个类型上，是因为两者共用同一份注册表：拆开之后调用方
// 要自己把表从一处搬到另一处，搬错的表现是「某个渠道类型在运行期才查不到」。
package upstream

import (
	"context"
	"fmt"
	"time"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/channel/anthropicadapter"
	"github.com/sumwai/larkax/internal/channel/openaiadapter"
	"github.com/sumwai/larkax/internal/channel/responsesadapter"
)

// Executor 按渠道类型把请求分发给对应的适配器。
//
// 它是 route.Executor 与 route.StreamExecutor 的实现，但**不 import
// internal/route**：接口是结构满足的。这一层不需要知道选路的存在，
// 也就不该被它的变化牵动。
//
// 并发安全：注册表在 New 之后不再变化，适配器自身也要求并发安全。
type Executor struct {
	registry *channel.Registry
}

// New 注册全部内置适配器并返回执行器。
//
// timeout 是「拿到上游响应头」的上限，会传给每个适配器；它不限制流式传输
// 本身，长回复不会被它掐断（见 httpx.Client 的注释）。
//
// 超时必须为正：0 或不填等于「永远不超时」，那会让上游卡住表现为
// 「请求永远不返回」——比报错难排查得多，也正是 httpx.Client 要避免的情形。
// 在这里挡住它，比让每个调用点各自记得传一个正数可靠。
//
// 注册失败（适配器为 nil、Type 为空、协议未知、类型重复）让 New 返回错误
// 而不是留下一个残缺的注册表：这些只可能是代码改动引入的，表现为启动即
// 失败，比「运行期某个渠道类型查不到」好定位。
func New(timeout time.Duration) (*Executor, error) {
	if timeout <= 0 {
		return nil, fmt.Errorf("上游超时必须为正，得到 %v", timeout)
	}
	return newWith(
		openaiadapter.New(timeout),
		anthropicadapter.New(timeout),
		responsesadapter.New(timeout),
	)
}

// adapterTimeout 是构造内置适配器时给 HTTP 客户端的超时。
//
// 它只在执行器真的发起请求时生效。Registry 用它把适配器装出来，
// 而那条路径不发请求，所以这个值在那里不会产生任何行为——它只需要是个正数。
const adapterTimeout = 10 * time.Minute

// Registry 返回内置适配器的注册表，供**不发请求**的调用方使用。
//
// 写入路径要按渠道类型校验（见 internal/channels.Admin），而校验需要的只是
// 「哪些类型有适配器」。适配器清单只在上面的 New 里写一次，于是「运维命令
// 认得的渠道类型」与「服务认得的类型」不会各自漂移——漂移的表现是某个类型
// 配不进去，或者配进去了却在请求时报未知类型。
//
// 返回的注册表与执行器用的是同一份装配，但它不被执行器持有：调用方只该拿它
// 查表，不该往里注册。
func Registry() (*channel.Registry, error) {
	executor, err := New(adapterTimeout)
	if err != nil {
		return nil, err
	}
	return executor.registry, nil
}

// newWith 用给定的适配器建执行器。
//
// 与 New 分开，是为了让「重复类型会被拒绝」这类情况在测试里也能钉住——
// 不必为了造一个重复注册而真的往 New 里再加一个适配器。
func newWith(adapters ...channel.Adapter) (*Executor, error) {
	registry := channel.NewRegistry()
	for _, a := range adapters {
		if err := registry.Register(a); err != nil {
			return nil, fmt.Errorf("注册上游适配器: %w", err)
		}
	}
	return &Executor{registry: registry}, nil
}

// Types 返回已注册的渠道类型，已排序。
//
// 用来在启动时把「这次到底装上了哪些适配器」写进日志。没有它的话，
// 「适配器一个都没注册上」与「注册上了」在启动日志里长得一样，
// 而前者的现场是每个请求都报未知渠道类型。
func (e *Executor) Types() []string {
	return e.registry.Types()
}

// Chat 发起一次非流式请求，按 ch.Type 分发。
//
// 不做重试：换渠道是 internal/route 的事，只有它知道候选集，以及哪些失败
// 值得换一个。这里只负责「按类型找到适配器，调它一次」。
func (e *Executor) Chat(ctx context.Context, ch channel.Channel, req *channel.Request) (*channel.Response, error) {
	a, err := e.adapter(ch)
	if err != nil {
		return nil, err
	}
	return a.Chat(ctx, ch, req)
}

// ChatStream 发起一次流式请求，按 ch.Type 分发。
//
// 返回的事件流**原样交给调用方**，这里不读、不缓冲、不关闭。
// 一旦在这里开始读，「上游是否已接受请求」这个判据就落到了这一层，
// 而它该由 internal/route 判：只有它知道客户端已经收到过多少内容，
// 也就只有它能决定还能不能换渠道。
func (e *Executor) ChatStream(ctx context.Context, ch channel.Channel, req *channel.Request) (<-chan channel.StreamEvent, error) {
	a, err := e.adapter(ch)
	if err != nil {
		return nil, err
	}
	return a.ChatStream(ctx, ch, req)
}

// adapter 按渠道类型取适配器。
//
// 未知类型**包着 channel.ErrUnknownType** 返回，调用方用
// errors.Is(err, channel.ErrUnknownType) 就能认出来。这个错误不该被当成
// 普通上游故障：它说明库里有一条渠道配置，但当前版本的二进制不认识它——
// 回滚版本之后最容易出现，处置是让这条渠道退出候选，而不是重试。
// 这里不把它翻译成别的错误，也不吞掉它。
//
// 包上渠道 ID 是为了让日志能直接指到那一行配置；类型与已知类型清单
// 由 Lookup 的错误带出来。
func (e *Executor) adapter(ch channel.Channel) (channel.Adapter, error) {
	a, err := e.registry.Lookup(ch.Type)
	if err != nil {
		return nil, fmt.Errorf("渠道 %d: %w", ch.ID, err)
	}
	return a, nil
}
