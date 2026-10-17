// Package responsesadapter 实现说 OpenAI Responses 协议的上游适配器。
//
// 与另外两个适配器分工相同：只做编码、解码、把 HTTP 失败翻译成带可重试
// 判断的错误。选路、重试、换渠道都不在这个包里。
//
// 它与 openaiadapter 的差别集中在三处，三处都会**静默算错钱**或**静默丢内容**，
// 所以每处都在代码里写明了判据：
//
//   - 请求体：messages 变成 input（历史与工具结果都在里面），system 变成
//     顶层的 instructions，max_tokens 变成 max_output_tokens
//   - 用量：缓存与推理两项嵌在 input_tokens_details / output_tokens_details
//     里，而 chat 那边是 prompt_tokens_details / completion_tokens_details
//   - 流式：一串**带名字的事件**，用量跟着终态事件一起来（chat 那边要
//     stream_options.include_usage 才给）
package responsesadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/channel/httpx"
	"github.com/sumwai/larkax/internal/observe"
	"github.com/sumwai/larkax/internal/quota"
	"github.com/sumwai/larkax/internal/relay"
)

// Type 是注册表里用的类型名，与 channels.type 对应。
//
// 取 openai-responses 而不是 responses：它属于 OpenAI 那一族，而
// 「responses」单独一个词在渠道表里读不出是哪个协议的表面。
const Type = "openai-responses"

// Adapter 是 Responses 协议上游的适配器。并发安全：只持有不可变的 HTTP 客户端。
type Adapter struct {
	client *http.Client
}

// New 构造适配器。timeout 是「拿到响应头」的上限，不含流式传输过程。
func New(timeout time.Duration) *Adapter {
	return &Adapter{client: httpx.Client(timeout)}
}

// Type 返回注册表里用的类型名。
func (a *Adapter) Type() string { return Type }

// Protocol 返回本适配器所说的协议：OpenAI Responses。
func (a *Adapter) Protocol() channel.Protocol { return channel.ProtocolOpenAIResponses }

// Chat 发起一次非流式请求。
func (a *Adapter) Chat(ctx context.Context, ch channel.Channel, req *channel.Request) (*channel.Response, error) {
	body, err := a.encodeRequest(ctx, req, false)
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

	var parsed responsesResponse
	if decodeErr := json.NewDecoder(resp.Body).Decode(&parsed); decodeErr != nil {
		return nil, &channel.ErrUpstream{Retryable: true, Err: fmt.Errorf("解析上游响应失败: %w", decodeErr)}
	}

	if upErr := parsed.upstreamError(); upErr != nil {
		return nil, upErr
	}

	out, err := relay.ResponseFromRelaykit(parsed.toRelaykit())
	if err != nil {
		return nil, &channel.ErrUpstream{Retryable: true, Err: err}
	}
	return out, nil
}

// upstreamError 检查这次响应是不是一次失败。
//
// 上游有两处会表达失败，两处都要看：
//
//   - 顶层 error 对象。有的上游在 HTTP 200 里塞它，只看状态码会把它当成
//     正常响应，于是「上游失败了」在账上表现为「成功但用量为零」——
//     那比直接报错更难查，因为没有任何一处会失败。
//   - status == "failed"。它是 Responses 自己的终态之一，与 incomplete
//     不同：incomplete 是「停在了长度上限上」，那是一次**正常**结束，
//     照常计费。
func (r responsesResponse) upstreamError() error {
	if !r.Error.empty() {
		return &channel.ErrUpstream{
			Body:      r.Error.Message,
			Retryable: false,
		}
	}
	if strings.EqualFold(rawString(r.Status), "failed") {
		return &channel.ErrUpstream{
			Err:       errors.New("上游把这次响应标记为 failed"),
			Retryable: false,
		}
	}
	return nil
}

// ChatStream 发起一次流式请求。
//
// 与另外两个适配器相同的约束：**返回前必须确认上游已接受请求**。
// 这样调用方才能区分「上游连不上」（可换渠道重试）与「流中途断了」
// （已吐给客户端，换渠道会让客户端看到两段拼接的答案）。
func (a *Adapter) ChatStream(ctx context.Context, ch channel.Channel, req *channel.Request) (<-chan channel.StreamEvent, error) {
	body, err := a.encodeRequest(ctx, req, true)
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
	a.pump(ctx, resp.Body, events)
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
// Responses 表面没有查额度的端点，与 OpenAI 一侧同理：猜一个路径会让 404
// 被当成「额度查不到」，从而让自记账永远接不上。
//
// 目前只有测试在调它：channel.Adapter 的接口方法，额度查询还没接线。
func (a *Adapter) Quota(context.Context, channel.Channel) (quota.State, error) {
	return quota.State{}, channel.ErrUnsupported
}

// encodeRequest 把统一请求编码成上游 JSON。
func (a *Adapter) encodeRequest(ctx context.Context, req *channel.Request, stream bool) ([]byte, error) {
	openaiReq, err := relay.ToRelaykit(req)
	if err != nil {
		return nil, fmt.Errorf("编码请求失败: %w", err)
	}

	converted, err := relay.ToRelaykitResponses(ctx, openaiReq)
	if err != nil {
		return nil, fmt.Errorf("编码请求失败: %w", err)
	}

	// 流式**不需要** opt-in 用量：usage 跟着终态事件一起来，没有 chat 那边
	// 的 stream_options.include_usage。所以这里只置 stream，不多带参数。
	if stream {
		yes := true
		converted.Stream = &yes
	}

	// max_output_tokens 不补默认值：它是可选的，缺了走上游默认值。
	// 这里与 anthropicadapter 的 DefaultMaxTokens 不同——Claude 的
	// max_tokens 是**必填**，缺了会被拒。而输出上界本身由调用方在预留时
	// 就要求给出（见 pipeline.Request.validate），所以「不补」不等于「没上界」。

	if mcpErr := checkNoRemoteMCPTools(converted.Tools); mcpErr != nil {
		return nil, mcpErr
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

// checkNoRemoteMCPTools 在把请求发出去之前挡住远端 MCP 工具。
//
// 上游对这类工具一律 400，而错误信息指向的是**我们发出去的请求**——
// 排查方向会落到对方身上，而原因在这一侧。所以在发出去之前报错，
// 错误里带上原因。
//
// 这条判据今天在适配器这一侧**打不着**：channel.Tool 只有名字、描述与
// schema，没有工具类型，于是 relay.ToRelaykit 出来的工具一律是 function。
// 它在这里是最后一道——客户端请求里的 tools 是原始 JSON，真正该拦的是
// 解析那一层（说 Responses 的入口），那里能看到 type 字段。留着它是因为
// 「以后有人把原始 tools 透传过来」正是这条路径最容易被改坏的地方。
func checkNoRemoteMCPTools(tools json.RawMessage) error {
	if len(tools) == 0 {
		return nil
	}
	var parsed []struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(tools, &parsed); err != nil {
		// 解析不出来时放行：工具声明的形状由上游判，这里只负责拦
		// 我们认得的那种。
		return nil
	}
	for _, t := range parsed {
		if t.Type == "mcp" {
			return errors.New("请求里有远端 MCP 工具（type=mcp），本项目不转发：它在服务端执行，而这条链路只做协议转换")
		}
	}
	return nil
}

func (a *Adapter) do(ctx context.Context, ch channel.Channel, body []byte, req *channel.Request) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		httpx.Endpoint(ch.BaseURL, "/v1/responses"), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// 流式响应不要被中间层缓冲：缓冲会让「逐字返回」变成「攒完一次性返回」，
	// 而那是用户立刻能感觉到的差别。
	httpReq.Header.Set("Accept", "text/event-stream, application/json")
	a.setHeaders(httpReq, ch)
	applyCustomHeaders(httpReq, req)

	resp, err := a.client.Do(httpReq)
	if err != nil {
		// 网络层失败一律可重试：连接被拒、超时、DNS 失败都属于换一个渠道
		// 就可能成功的情形。
		return nil, &channel.ErrUpstream{Retryable: true, Err: err}
	}
	return resp, nil
}

// setHeaders 设置鉴权头。
//
// Responses 与 chat completions 同属 OpenAI 一族，用 Bearer——与
// anthropicadapter 的 x-api-key 正好相反。抄错这一个头会得到 401，
// 而上游的错误信息不会说「你用错头了」。
func (a *Adapter) setHeaders(req *http.Request, ch channel.Channel) {
	req.Header.Set("Authorization", "Bearer "+ch.Credential)
}

// applyCustomHeaders 把自定义头附加到请求上。
//
// 鉴权头与协议头由 httpx.SafeRequestHeaders 滤掉，而**被拒的头会被记下来**
// 而不是静默丢弃：插件作者写了一个被拒的头却没有任何反馈，会以为是
// 上游不支持它。
//
// 放在适配器这一道而不只在插件层：插件层的白名单写错时，这里是最后一道。
// 「插件能覆盖 Authorization」意味着它能冒充别的渠道身份。
func applyCustomHeaders(httpReq *http.Request, req *channel.Request) {
	if req == nil {
		return
	}
	safe, rejected := httpx.SafeRequestHeaders(req.Headers)
	for name, value := range safe {
		httpReq.Header.Set(name, value)
	}
	if len(rejected) > 0 {
		// 只记名字不记值：值可能含敏感内容，而这里需要的只是「谁被拒了」。
		observe.FromContext(httpReq.Context()).Warn("自定义请求头被拒",
			"headers", strings.Join(rejected, ","))
	}
}

// rawString 把一个可能是字符串、也可能不是的 JSON 值读成字符串。
//
// 读不出来时给空串而不是报错：调用方拿它做判据（status 是不是 failed），
// 而一个读不出来的 status 不该让整条响应失败。
func rawString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}
