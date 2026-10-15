// Package anthropicadapter 实现说 Anthropic Messages 协议的上游适配器。
//
// 与 openaiadapter 的分工相同：只做编码、解码、把 HTTP 失败翻译成带可重试
// 判断的错误。选路与重试不在这个包里。
//
// 两套协议在流式上的差别比看起来大：
//
//	OpenAI：每片一个 choices 数组，正文在 choices[0].delta.content
//	Anthropic：一串命名事件（message_start / content_block_delta / message_stop），
//	           每个事件有自己的形状
//
// 更大的差别在用量：Anthropic 把**输入**放在 message_start，把**输出**放在
// message_delta，跨两个事件。只读其中一个会得到「有输入没输出」或反之，
// 而两者都是算钱的依据。
package anthropicadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/channel/httpx"
	"github.com/sumwai/larkax/internal/relay"
)

// Type 是注册表里用的类型名，与 channels.type 对应。
const Type = "anthropic"

// AnthropicVersion 是必需的协议版本头。
//
// 它是**必须**的：缺少时 Anthropic 返回 400，而错误信息不会指出这一点，
// 只报「缺少必需的头部」。硬编码一个已知可用的值，而不是做成渠道配置——
// 版本升级是全局行为，不该按渠道各写一个。
const AnthropicVersion = "2023-06-01"

// Adapter 是 Anthropic 协议上游的适配器。并发安全：只持有不可变的 HTTP 客户端。
type Adapter struct {
	client *http.Client
}

// New 构造适配器。timeout 是「拿到响应头」的上限，不含流式传输过程。
func New(timeout time.Duration) *Adapter {
	return &Adapter{client: httpx.Client(timeout)}
}

func (a *Adapter) Type() string               { return Type }
func (a *Adapter) Protocol() channel.Protocol { return channel.ProtocolAnthropic }

// Chat 发起一次非流式请求。
func (a *Adapter) Chat(ctx context.Context, ch channel.Channel, req *channel.Request) (*channel.Response, error) {
	body, err := a.encodeRequest(req)
	if err != nil {
		return nil, err
	}

	resp, err := a.do(ctx, ch, body)
	if err != nil {
		return nil, err
	}
	defer httpx.DrainAndClose(resp.Body)

	if err := httpx.CheckStatus(resp); err != nil {
		return nil, err
	}

	var parsed claudeResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, &channel.ErrUpstream{Retryable: true, Err: fmt.Errorf("解析上游响应失败: %w", err)}
	}

	// Anthropic 的错误体形状是 {"type":"error","error":{...}}，而 HTTP 状态码
	// 也可能是 200。不检查会把失败当成正常响应，于是「上游失败了」在账上
	// 表现为「成功但用量为零」——没有任何一处会失败，比直接报错更难查。
	if parsed.Type == "error" || parsed.Error != nil {
		return nil, &channel.ErrUpstream{
			Body:      parsed.errorMessage(),
			Retryable: isRetryableErrorType(parsed.errorType()),
		}
	}

	out, err := relay.ResponseFromRelaykit(parsed.toRelaykit())
	if err != nil {
		return nil, &channel.ErrUpstream{Retryable: true, Err: err}
	}
	return out, nil
}

// ChatStream 发起一次流式请求。
//
// 与 OpenAI 适配器相同的约束：**返回前必须确认上游已接受请求**。
// 这样调用方才能区分「上游连不上」（可换渠道重试）与「流中途断了」
// （已吐给客户端，换渠道会让客户端看到两段拼接的答案）。
func (a *Adapter) ChatStream(ctx context.Context, ch channel.Channel, req *channel.Request) (<-chan channel.StreamEvent, error) {
	body, err := a.encodeRequest(req)
	if err != nil {
		return nil, err
	}

	resp, err := a.do(ctx, ch, body)
	if err != nil {
		return nil, err
	}

	if err := httpx.CheckStatus(resp); err != nil {
		defer httpx.DrainAndClose(resp.Body)
		return nil, err
	}

	events := make(chan channel.StreamEvent)
	go func() {
		defer close(events)
		defer httpx.DrainAndClose(resp.Body)
		a.pump(ctx, resp.Body, events)
	}()
	return events, nil
}

// ListModels 查询上游可用模型。
func (a *Adapter) ListModels(ctx context.Context, ch channel.Channel) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, httpx.Endpoint(ch.BaseURL, "/v1/models"), nil)
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	a.setHeaders(req, ch)

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, &channel.ErrUpstream{Retryable: true, Err: err}
	}
	defer httpx.DrainAndClose(resp.Body)

	if err := httpx.CheckStatus(resp); err != nil {
		return nil, err
	}

	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, &channel.ErrUpstream{Retryable: true, Err: fmt.Errorf("解析模型列表失败: %w", err)}
	}

	out := make([]string, 0, len(payload.Data))
	for _, m := range payload.Data {
		if m.ID != "" {
			out = append(out, m.ID)
		}
	}
	return out, nil
}

// Quota 查询额度。
//
// Messages 协议里没有查额度的端点，与 OpenAI 一侧同理：
// 猜一个路径会让 404 被当成「额度查不到」，从而让自记账永远接不上。
func (a *Adapter) Quota(context.Context, channel.Channel) (channel.Quota, error) {
	return channel.Quota{}, channel.ErrUnsupported
}

// encodeRequest 把统一请求编码成 Anthropic Messages 的 JSON。
//
// 走 relaykit 的转换而不是自己拼：OpenAI 与 Anthropic 的差异不只是字段名
// （system 是顶层字段而不是一条消息、tool_use 与 tool_result 的结构不同、
// max_tokens 是必填）。那些差异 relaykit 已经处理过并有测试覆盖。
func (a *Adapter) encodeRequest(req *channel.Request) ([]byte, error) {
	openaiReq, err := relay.ToRelaykit(req)
	if err != nil {
		return nil, fmt.Errorf("编码请求失败: %w", err)
	}

	// Claude 需要 max_tokens，缺了会被拒。这里给一个保守的默认值而不是报错：
	// 报错会让「客户端没传 max_tokens」变成一次失败的请求，而各家网关的
	// 通行做法是补一个默认值。
	if openaiReq.MaxTokens == nil {
		fallback := uint(DefaultMaxTokens)
		openaiReq.MaxTokens = &fallback
	}

	converted, err := relay.ToRelaykitClaude(openaiReq)
	if err != nil {
		return nil, fmt.Errorf("转换为 Anthropic 请求失败: %w", err)
	}
	if req.Stream {
		yes := true
		converted.Stream = &yes
	}

	body, err := json.Marshal(converted)
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}
	return body, nil
}

// DefaultMaxTokens 是请求未指定输出上限时补的值。
//
// 取 4096 而不是协议允许的最大值：它的作用是「让请求合法」，
// 而不是「让模型尽可能多写」。给太大反而会让一次失误的调用产生高额费用。
const DefaultMaxTokens = 4096

func (a *Adapter) do(ctx context.Context, ch channel.Channel, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		httpx.Endpoint(ch.BaseURL, "/v1/messages"), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream, application/json")
	a.setHeaders(req, ch)

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, &channel.ErrUpstream{Retryable: true, Err: err}
	}
	return resp, nil
}

// setHeaders 设置 Anthropic 的鉴权头。
//
// 注意它**不是** Bearer：Anthropic 用 x-api-key。写成 Bearer 会得到一个
// 401，而上游的错误信息不会说「你用错头了」。这个差异是两套协议里
// 最容易在复制粘贴时搞错的一处。
func (a *Adapter) setHeaders(req *http.Request, ch channel.Channel) {
	req.Header.Set("x-api-key", ch.Credential)
	req.Header.Set("anthropic-version", AnthropicVersion)
}
