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

// Endpoint 返回本适配器把请求发到哪条路径。见 channel.Adapter.Endpoint。
func (a *Adapter) Endpoint() channel.Endpoint { return channel.EndpointResponses }

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
		return nil, &channel.ErrUpstream{
			StatusCode:  resp.StatusCode,
			GotResponse: true,
			Retryable:   true,
			Err:         fmt.Errorf("解析上游响应失败: %w", decodeErr),
		}
	}

	if upErr := parsed.upstreamError(resp.StatusCode); upErr != nil {
		return nil, upErr
	}

	out, err := relay.ResponseFromRelaykit(parsed.toRelaykit())
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
//
// status 是这次响应的真实状态码（调用方已经过了 CheckStatus，所以它一定是
// 2xx）。它要跟着错误一起带出去：上游确实回了这一个状态，丢掉会让账单上
// 那一行读成「没有记录到状态码」。
func (r responsesResponse) upstreamError(status int) error {
	if !r.Error.empty() {
		return &channel.ErrUpstream{
			StatusCode:  status,
			GotResponse: true,
			Body:        r.Error.Message,
			// 该不该换渠道重试由共享的那一处判（type 与 code 两个字段都看，
			// 见 channel.IsEmbeddedErrorRetryable）。上面那个 status 说的是
			// 「这一次失败了」，答不了「是不是上游这一侧暂时答不上来」：
			// 拿它当判据，限流会被当成这次请求的错——既不换下一个候选，
			// 也不记这个渠道的失败。
			Retryable: channel.IsEmbeddedErrorRetryable(r.Error.Type, r.Error.Code),
		}
	}
	if strings.EqualFold(rawString(r.Status), "failed") {
		return &channel.ErrUpstream{
			StatusCode:  status,
			GotResponse: true,
			Err:         errors.New("上游把这次响应标记为 failed"),
			// 这个分支上没有 type / code 可判：上游只说这次失败了，没说是什么
			// 事。默认按不可重试处置，方向与 channel.IsRetryable 一致。
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
	// 第一条事件带上流的状态码：这条流可能一个正文分片、一个结束标记都没有
	// 就结束，那时它是网关知道「上游回过一个响应」的唯一途径（见
	// channel.StreamEvent.StatusCode）。
	events <- channel.StreamEvent{Kind: "start", StatusCode: resp.StatusCode}
	a.pump(ctx, resp.StatusCode, resp.Body, events)
}

// ListModels 查询上游可用模型。
//
// 生产里的唯一调用点是 `larkaxctl channel sync-models`（经 channels.ModelSync）：
// 那是一次显式同步，把结果落库供人查看，不在请求路径上。
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
// **这条判据今天打不着，别把它当成 MCP 已经拦住了。** 真正的拦阻在解析层
// （relay.checkToolType）：channel.Tool 只有名字、描述与 schema，没有承载
// 工具类型的字段，所以任何非 function 的声明在入口就被拒了，走不到这里。
//
// 它什么时候会活过来：channel.Tool 加了类型字段、内置工具（web_search 一类）
// 能真正转发的那一天。届时解析层那条判据要收窄成「仍然送不出去的那些」，
// 而 mcp 仍是其中之一——relaykit 的转换器对未知 type 是「原样保留形状」地
// 转发，所以收窄之后 MCP 声明会真的到达这里，这条判据接住它。
//
// 留着它是因为它是**发出去之前的最后一道**：那时唯一还看得见工具类型的地方
// 就是这里。它只拦 mcp 而不拦别的，也是刻意的——内置工具本该转发。
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
		httpx.Endpoint(ch.BaseURL, string(channel.EndpointResponses)), bytes.NewReader(body))
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

// setHeaders 设置鉴权头。
//
// Responses 与 chat completions 同属 OpenAI 一族，用 Bearer——与
// anthropicadapter 的 x-api-key 正好相反。抄错这一个头会得到 401，
// 而上游的错误信息不会说「你用错头了」。
func (a *Adapter) setHeaders(req *http.Request, ch channel.Channel) {
	req.Header.Set("Authorization", "Bearer "+ch.Credential)
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
