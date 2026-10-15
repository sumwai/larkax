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
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/relaykit/dto"

	"github.com/sumwai/larkax/internal/channel"
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
	return &Adapter{client: channel.HTTPClient(timeout)}
}

func (a *Adapter) Type() string               { return Type }
func (a *Adapter) Protocol() channel.Protocol { return channel.ProtocolOpenAI }

// Chat 发起一次非流式请求。
func (a *Adapter) Chat(ctx context.Context, ch channel.Channel, req *channel.Request) (*channel.Response, error) {
	body, err := a.encodeRequest(req, false)
	if err != nil {
		return nil, err
	}

	resp, err := a.do(ctx, ch, body)
	if err != nil {
		return nil, err
	}
	defer drainAndClose(resp.Body)

	if err := checkStatus(resp); err != nil {
		return nil, err
	}

	var parsed json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, &channel.ErrUpstream{Retryable: true, Err: fmt.Errorf("解析上游响应失败: %w", err)}
	}

	// 有些上游在 HTTP 200 里塞一个 error 对象。只看状态码会把它当成正常响应，
	// 于是「上游失败了」在账上表现为「成功但用量为零」。
	if e := extractEmbeddedError(parsed); e != nil {
		return nil, e
	}

	var textResp openAITextResponse
	if err := json.Unmarshal(parsed, &textResp); err != nil {
		return nil, &channel.ErrUpstream{Retryable: true, Err: fmt.Errorf("响应结构不符: %w", err)}
	}

	out, err := relay.ResponseFromRelaykit(textResp.toRelaykit())
	if err != nil {
		return nil, &channel.ErrUpstream{Retryable: true, Err: err}
	}
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

	resp, err := a.do(ctx, ch, body)
	if err != nil {
		return nil, err
	}

	// 上游拒绝：状态码与响应体都拿到了，直接报错，不启动 goroutine。
	// 此时还没有任何内容发给客户端，调用方可以安全地换渠道。
	if err := checkStatus(resp); err != nil {
		defer drainAndClose(resp.Body)
		return nil, err
	}

	events := make(chan channel.StreamEvent)
	go func() {
		defer close(events)
		defer drainAndClose(resp.Body)
		a.pump(ctx, resp.Body, events)
	}()
	return events, nil
}

// ListModels 查询上游可用模型。
func (a *Adapter) ListModels(ctx context.Context, ch channel.Channel) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint(ch, "/v1/models"), nil)
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	a.setHeaders(req, ch)

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, &channel.ErrUpstream{Retryable: true, Err: err}
	}
	defer drainAndClose(resp.Body)

	if err := checkStatus(resp); err != nil {
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
// OpenAI 兼容协议里**没有**查额度的标准端点。各家要么没有，要么是自己加的
// 私有路径。所以这里一律返回 ErrUnsupported，由额度监控层按渠道类型
// 决定是查私有端点还是走自记账。
//
// 不在这里猜一个路径：猜错的话，一个 404 会被当成「额度查不到」，
// 而实际原因是路径不对——那会让自记账永远接不上。
func (a *Adapter) Quota(context.Context, channel.Channel) (channel.Quota, error) {
	return channel.Quota{}, channel.ErrUnsupported
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

func (a *Adapter) do(ctx context.Context, ch channel.Channel, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		endpoint(ch, "/v1/chat/completions"), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// 流式响应不要被中间层缓冲：缓冲会让「逐字返回」变成「攒完一次性返回」，
	// 而那是用户立刻能感觉到的差别。
	req.Header.Set("Accept", "text/event-stream, application/json")
	a.setHeaders(req, ch)

	resp, err := a.client.Do(req)
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

// endpoint 拼出完整 URL。
//
// 手工拼接而不是用 url.Parse 再 resolve：上游给的 base_url 常见写法
// 有带与不带结尾斜杠两种，而路径里也可能已经含 /v1。
// 用 TrimRight 统一掉结尾斜杠，比 url.ResolveReference 的语义更容易预测。
func endpoint(ch channel.Channel, path string) string {
	base := strings.TrimRight(ch.BaseURL, "/")
	// base 已经带了 /v1 时不再重复加。
	if strings.HasSuffix(base, "/v1") && strings.HasPrefix(path, "/v1") {
		path = strings.TrimPrefix(path, "/v1")
	}
	return base + path
}

// checkStatus 把非 2xx 的响应翻译成带可重试判断的错误。
func checkStatus(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	// 响应体截断后保留：上游的错误信息里可能回显请求内容，
	// 而错误会进日志，日志的去向比密钥本身更难控制。
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))

	return &channel.ErrUpstream{
		StatusCode: resp.StatusCode,
		Body:       string(body),
		Retryable:  retryableStatus(resp.StatusCode),
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
	}
}

// retryableStatus 判断某个状态码是否值得换渠道重试。
//
// 429 与 5xx 可重试；4xx 里的其余部分（400 参数错、401 钥匙错、403 权限、
// 404 路径错、422 语义错）重试多少次结果都一样，换渠道也是同一个错，
// 只会把费用与延迟乘以候选渠道数。
func retryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests:
		return true
	}
	return code >= 500
}

// parseRetryAfter 解析 Retry-After 头。它有两种合法形式：秒数或 HTTP 日期。
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := time.ParseDuration(v + "s"); err == nil && secs > 0 {
		return secs
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// extractEmbeddedError 检查 200 响应体里是否藏着错误对象。
//
// 有上游在 HTTP 200 里返回 `{"error": {...}}`。只看状态码会把它当成正常响应，
// 于是「上游失败了」在账上表现为「成功但用量为零」——那比直接报错更难查，
// 因为没有任何一处会失败。
func extractEmbeddedError(raw json.RawMessage) error {
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
		Body:      probe.Error.Message,
		Retryable: false,
	}
}

func drainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	// 排空再关闭：留着未读的数据会让连接无法复用，
	// 在高频调用下表现为连接数无谓地增长。
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 1<<20))
	_ = body.Close()
}
