// Package systemoneadapter 实现说 System One 决策协议的上游适配器。
//
// 它只做 HTTP 层的事：拼地址、上鉴权头、把状态码翻成带可重试判断的错误、
// 把响应体交给解码。**请求与响应的线格式不在这里**——那是
// internal/systemone 的事（Encode / ParseResponse），因为这条表面的形状
// 与上游一致，而「它长什么样」只该有一份定义。这也是本包没有 dto.go 的原因：
// 有 dto.go 就意味着线格式在这里有第二份。
//
// 这条表面**从不流式**：上游收到 stream 会忽略它，回的仍是普通 JSON。
// 所以本适配器没有 stream.go，ChatStream 也不实现——见下面的 errNoChat。
package systemoneadapter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/channel/httpx"
	"github.com/sumwai/larkax/internal/quota"
	"github.com/sumwai/larkax/internal/systemone"
)

// Type 是注册表里用的类型名，与 channels.type 对应。
const Type = "systemone"

// Path 是上游决策接口的路径。
//
// 与公共入口那条路径同名，理由与 openaiadapter 用 /v1/chat/completions 一样：
// 这条表面「原样暴露」——上游认的路径就是调用方打的路径，中间不换名字。
// httpx.Endpoint 会把 base_url 结尾已经带的版本段去掉，所以
// 「base 到 /v1」与「base 到域名」两种写法拼出来是同一个地址。
const Path = "/v1/systemone"

// maxResponseBytes 是读上游响应体的上限。
//
// 决策响应很小（一组概率），1 MiB 已经是几个数量级的余量。设这个上限是为了
// 让一个跑偏的上游不能靠一份巨型响应把内存吃掉。超限时读到的是截断的 JSON，
// 会在解析那一步失败——报出来的是「响应结构不符」，而不是一个说不清的内存问题。
const maxResponseBytes = 1 << 20

// Adapter 是 System One 上游的适配器。并发安全：只持有不可变的 HTTP 客户端。
type Adapter struct {
	client *http.Client
}

// New 构造适配器。timeout 是「拿到响应头」的上限。
func New(timeout time.Duration) *Adapter {
	return &Adapter{client: httpx.Client(timeout)}
}

// Type 返回注册表里用的类型名。
func (a *Adapter) Type() string { return Type }

// Protocol 返回本适配器所说的协议：System One 决策。
func (a *Adapter) Protocol() channel.Protocol { return channel.ProtocolSystemOne }

// errNoChat 表示这条渠道不做 chat 补全。
//
// 它包着 channel.ErrUnsupported：这个渠道**设计上**不提供这项能力，不是它
// 此刻坏了。路由层据此**跳过这条候选、试下一个**（见 route 的
// worthNextCandidate）——候选池里可以同时有 chat 渠道与决策渠道，一个配错
// 类型的渠道不该打断该模型的全部流量。
//
// 这条错误真的会出现：候选是按「渠道支不支持这个模型」筛的，而渠道表里没有
// 「这条渠道说哪套协议」这一列——协议由 type 指向的适配器决定。所以一个
// systemone 渠道配上了 chat 请求要用的模型名时，它会进候选集，然后在这里失败。
// 这是配置问题；全部候选都这样时，路由层给出的错误文本会说清是「都不支持」
// 而不是「都失败了」。
var errNoChat = fmt.Errorf("systemone 渠道不提供 chat 补全: %w", channel.ErrUnsupported)

// Chat 不实现：这条渠道不说 chat 协议。见 errNoChat。
func (a *Adapter) Chat(context.Context, channel.Channel, *channel.Request) (*channel.Response, error) {
	return nil, errNoChat
}

// ChatStream 不实现：这条渠道不说 chat 协议，而且这条表面从不流式。
func (a *Adapter) ChatStream(context.Context, channel.Channel, *channel.Request) (<-chan channel.StreamEvent, error) {
	return nil, errNoChat
}

// ListModels 返回 ErrUnsupported。
//
// 上游确实有一个模型列表端点，但那个端点列的是**全部**模型（含 chat 与
// responses 的），而这条渠道的 channels.models 记的是「这条渠道支持哪些
// 规范模型名」。拿前者填后者会把一大批这条表面根本调不动的模型写成可用，
// 而那条路径上的失败要到请求时才显形。所以不实现，而不是实现成一个会写错的映射。
func (a *Adapter) ListModels(context.Context, channel.Channel) ([]string, error) {
	return nil, channel.ErrUnsupported
}

// Quota 返回 ErrUnsupported：这条上游没有查额度的接口。
//
// 与 openaiadapter 同一个处置，理由也一样——不猜一个私有路径：猜错的话
// 一个 404 会被当成「额度查不到」，而实际原因是路径不对。
func (a *Adapter) Quota(context.Context, channel.Channel) (quota.State, error) {
	return quota.State{}, channel.ErrUnsupported
}

// Decide 发起一次决策调用。
//
// req.Model 是**上游模型名**：路由层按选中的渠道翻好了（见 internal/gateway
// 的 routeDecide），本方法不再查别名。
func (a *Adapter) Decide(ctx context.Context, ch channel.Channel, req *systemone.Request) (*systemone.Response, error) {
	body, err := systemone.Encode(req)
	if err != nil {
		return nil, fmt.Errorf("编码决策请求失败: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		httpx.Endpoint(ch.BaseURL, Path), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// 明确要 JSON 而不是 SSE：这条表面从不流式，而有些上游按 Accept 决定
	// 回什么形状。写清楚比让它去猜好。
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+ch.Credential)

	resp, err := a.client.Do(httpReq)
	if err != nil {
		// 网络层失败一律可重试：连接被拒、超时、DNS 失败都属于换一个渠道
		// 就可能成功的情形。
		return nil, &channel.ErrUpstream{Retryable: true, Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	defer httpx.Drain(resp.Body)

	if statusErr := httpx.CheckStatus(resp); statusErr != nil {
		return nil, statusErr
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, &channel.ErrUpstream{
			StatusCode:  resp.StatusCode,
			GotResponse: true,
			Retryable:   true,
			Err:         fmt.Errorf("读取上游响应失败: %w", err),
		}
	}

	out, err := systemone.ParseResponse(raw)
	if err != nil {
		// 与 openaiadapter 同一个判断：形状不符是**这个渠道**的问题，
		// 换一个渠道可能就好了，所以标成可重试。
		return nil, &channel.ErrUpstream{
			StatusCode:  resp.StatusCode,
			GotResponse: true,
			Retryable:   true,
			Err:         err,
		}
	}
	if out == nil {
		return nil, &channel.ErrUpstream{
			StatusCode:  resp.StatusCode,
			GotResponse: true,
			Retryable:   true,
			Err:         errors.New("上游返回了空响应"),
		}
	}
	return out, nil
}
