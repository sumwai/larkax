// Package openaiadapter 实现说 OpenAI Chat Completions 协议的上游适配器。
//
// 它只做三件事：把统一请求编码成上游认的 JSON、把上游的响应解码回统一结构、
// 把 HTTP 层的失败翻译成带可重试判断的错误。选路、重试、换渠道都不在这里。
//
// 上游行为不一致是这个包存在的主要理由。各家自称「兼容 OpenAI」，
// 但流式分片里有用的字段、错误响应的形状、usage 出现的位置都不一样。
// 所以这里的解析一律按「缺了怎么办」来写，而不是假设字段一定在。
package openaiadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/QuantumNous/new-api/relaykit/dto"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/channel/httpx"
	"github.com/sumwai/larkax/internal/quota"
	"github.com/sumwai/larkax/internal/relay"
)

// Type 是注册表里用的类型名，与 channels.type 对应。
const Type = "openai"

// Adapter 是 OpenAI 兼容上游的适配器。并发安全：只持有不可变的 HTTP 客户端。
type Adapter struct {
	client *http.Client
}

// New 构造适配器。timeout 是「拿到响应头」的上限，不含流式传输过程。
func New(timeout time.Duration) *Adapter {
	return &Adapter{client: httpx.Client(timeout)}
}

// Type 返回注册表里用的类型名。
func (a *Adapter) Type() string { return Type }

// Protocol 返回本适配器所说的协议：OpenAI Chat Completions。
func (a *Adapter) Protocol() channel.Protocol { return channel.ProtocolOpenAI }

// Chat 发起一次非流式请求。
func (a *Adapter) Chat(ctx context.Context, ch channel.Channel, req *channel.Request) (*channel.Response, error) {
	body, err := a.encodeRequest(req, false)
	if err != nil {
		return nil, err
	}

	resp, err := a.do(ctx, ch, body, req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	defer httpx.Drain(resp.Body)

	if statusErr := httpx.CheckStatus(resp); statusErr != nil {
		return nil, statusErr
	}

	var parsed json.RawMessage
	if decodeErr := json.NewDecoder(resp.Body).Decode(&parsed); decodeErr != nil {
		return nil, &channel.ErrUpstream{
			StatusCode:  resp.StatusCode,
			GotResponse: true,
			Retryable:   true,
			Err:         fmt.Errorf("解析上游响应失败: %w", decodeErr),
		}
	}

	// 有些上游在 HTTP 200 里塞一个 error 对象。只看状态码会把它当成正常响应，
	// 于是「上游失败了」在账上表现为「成功但用量为零」。
	if e := extractEmbeddedError(parsed, resp.StatusCode); e != nil {
		return nil, e
	}

	var textResp openAITextResponse
	if unmarshalErr := json.Unmarshal(parsed, &textResp); unmarshalErr != nil {
		return nil, &channel.ErrUpstream{
			StatusCode:  resp.StatusCode,
			GotResponse: true,
			Retryable:   true,
			Err:         fmt.Errorf("响应结构不符: %w", unmarshalErr),
		}
	}

	out, err := relay.ResponseFromRelaykit(textResp.toRelaykit())
	if err != nil {
		return nil, &channel.ErrUpstream{
			StatusCode:  resp.StatusCode,
			GotResponse: true,
			Retryable:   true,
			Err:         err,
		}
	}
	// 状态码照实带出去：响应方向的快照要把「上游回了几号」单独记一列，
	// 而它只在这里出现过一次（见 channel.Response.StatusCode）。
	out.StatusCode = resp.StatusCode
	return out, nil
}

// ChatStream 发起一次流式请求。
//
// 关键约束：**返回前必须确认上游已经接受请求**。这样调用方才能区分
// 「上游连不上」（可以换渠道重试）与「流中途断了」（已经吐给客户端，
// 换渠道会让客户端看到两段拼接的答案）。
//
// 因此这里同步发完请求、拿到响应头，再把读体的工作交给 goroutine。
func (a *Adapter) ChatStream(ctx context.Context, ch channel.Channel, req *channel.Request) (<-chan channel.StreamEvent, error) {
	body, err := a.encodeRequest(req, true)
	if err != nil {
		return nil, err
	}

	resp, err := a.do(ctx, ch, body, req)
	if err != nil {
		return nil, err
	}

	// 上游拒绝：状态码与响应体都拿到了，直接报错，不启动 goroutine。
	// 此时还没有任何内容发给客户端，调用方可以安全地换渠道。
	if err := httpx.CheckStatus(resp); err != nil {
		defer func() { _ = resp.Body.Close() }()
		defer httpx.Drain(resp.Body)
		return nil, err
	}

	// 读体、排空与关闭都在 goroutine 里做，因此这里不能在 defer 里关闭：
	// 函数要先带着事件通道返回，响应体要活到调用方读完事件为止。
	// 交给 pumpResponse 收尾，顺序仍是先排空、后关闭。
	events := make(chan channel.StreamEvent)
	go a.pumpResponse(ctx, resp, events)
	return events, nil
}

// pumpResponse 把响应体翻译成统一事件，并负责排空与关闭它。
//
// 单独一个方法而不是写在 goroutine 的闭包里：响应体的收尾只在关闭点
// 完整可见时才好核对，而闭包里的 resp.Body.Close() 会被静态检查当成
// 「这个响应体没人关」。
func (a *Adapter) pumpResponse(ctx context.Context, resp *http.Response, events chan channel.StreamEvent) {
	defer close(events)
	defer func() { _ = resp.Body.Close() }()
	defer httpx.Drain(resp.Body)
	// 第一条事件带上流的状态码：这条流可能一个正文分片、一个结束标记都没有
	// 就结束，那时它是网关知道「上游回过一个响应」的唯一途径（见
	// channel.StreamEvent.StatusCode）。
	events <- channel.StreamEvent{Kind: "start", StatusCode: resp.StatusCode}
	a.pump(ctx, resp.StatusCode, resp.Body, events)
}

// ListModels 查询上游可用模型。
//
// 目前只有测试在调它：channel.Adapter 的接口方法，生产里还没有拉取上游
// 模型列表的入口（渠道支持哪些模型由渠道自己的配置给出）。
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
	defer func() { _ = resp.Body.Close() }()
	defer httpx.Drain(resp.Body)

	if err := httpx.CheckStatus(resp); err != nil {
		return nil, err
	}

	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, &channel.ErrUpstream{
			StatusCode:  resp.StatusCode,
			GotResponse: true,
			Retryable:   true,
			Err:         fmt.Errorf("解析模型列表失败: %w", err),
		}
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
// OpenAI 兼容协议里**没有**查额度的标准端点。各家要么没有，要么是自己加的
// 私有路径。所以这里一律返回 ErrUnsupported，由额度监控层按渠道类型
// 决定是查私有端点还是走自记账。
//
// 不在这里猜一个路径：猜错的话，一个 404 会被当成「额度查不到」，
// 而实际原因是路径不对——那会让自记账永远接不上。
//
// 目前只有测试在调它：channel.Adapter 的接口方法，额度查询还没接线。
func (a *Adapter) Quota(context.Context, channel.Channel) (quota.State, error) {
	return quota.State{}, channel.ErrUnsupported
}

// encodeRequest 把统一请求编码成上游 JSON。
func (a *Adapter) encodeRequest(req *channel.Request, stream bool) ([]byte, error) {
	converted, err := relay.ToRelaykit(req)
	if err != nil {
		return nil, fmt.Errorf("编码请求失败: %w", err)
	}
	if stream {
		yes := true
		converted.Stream = &yes
		// 要求上游在最后一个分片里带上 usage。不是所有上游都支持，
		// 不支持的会忽略它——那正是计费层需要估算兜底的情形。
		converted.StreamOptions = &dto.StreamOptions{IncludeUsage: true}
	}

	body, err := json.Marshal(converted)
	if err != nil {
		return nil, fmt.Errorf("序列化请求失败: %w", err)
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("序列化后请求为空")
	}
	return body, nil
}

func (a *Adapter) do(ctx context.Context, ch channel.Channel, body []byte, req *channel.Request) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		httpx.Endpoint(ch.BaseURL, "/v1/chat/completions"), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// 流式响应不要被中间层缓冲：缓冲会让「逐字返回」变成「攒完一次性返回」，
	// 而那是用户立刻能感觉到的差别。
	httpReq.Header.Set("Accept", "text/event-stream, application/json")
	a.setHeaders(httpReq, ch)
	httpx.ApplyCustomHeaders(httpReq, req)

	resp, err := a.client.Do(httpReq)
	if err != nil {
		// 网络层失败一律可重试：连接被拒、超时、DNS 失败都属于换一个渠道
		// 就可能成功的情形。
		return nil, &channel.ErrUpstream{Retryable: true, Err: err}
	}
	return resp, nil
}

func (a *Adapter) setHeaders(req *http.Request, ch channel.Channel) {
	req.Header.Set("Authorization", "Bearer "+ch.Credential)
}

// extractEmbeddedError 检查 200 响应体里是否藏着错误对象。
//
// 有上游在 HTTP 200 里返回 `{"error": {...}}`。只看状态码会把它当成正常响应，
// 于是「上游失败了」在账上表现为「成功但用量为零」——那比直接报错更难查，
// 因为没有任何一处会失败。
//
// status 是这次响应的真实状态码（调用方已经过了 CheckStatus，所以它一定是
// 2xx）。要把它带进错误里：上游确实回了这一个状态，丢掉它会让账单上那一行
// 读成「没有记录到状态码」，而它其实是一个确定的事实。
func extractEmbeddedError(raw json.RawMessage, status int) error {
	var probe struct {
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    any    `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil // 解析不了就交给下面的正常路径去报错
	}
	if probe.Error == nil {
		return nil
	}

	// 有些上游把 `"error": null` 或空对象当成正常的「无错误」写法，
	// 空 message 时不当成错误，避免把正常响应误判成失败。
	if probe.Error.Message == "" && probe.Error.Type == "" && probe.Error.Code == nil {
		return nil
	}
	return &channel.ErrUpstream{
		StatusCode:  status,
		GotResponse: true,
		Body:        probe.Error.Message,
		Retryable:   false,
	}
}
