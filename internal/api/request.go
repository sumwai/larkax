package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/observe"
	"github.com/sumwai/larkax/internal/pipeline"
	"github.com/sumwai/larkax/internal/tokens"
)

// errBodyTooLarge 表示请求体超过了 maxRequestBodyBytes。
var errBodyTooLarge = errors.New("请求体超过上限")

// readBody 读取请求体，超过 maxRequestBodyBytes 时返回 errBodyTooLarge。
//
// 用 http.MaxBytesReader 而不是先看 Content-Length：那个头是调用方给的，
// 可以缺失（分块传输）也可以撒谎，而 MaxBytesReader 是在**读的时候**限制，
// 于是一个声称很小、实际很大的请求体也吃不到多余的内存。超限时它还会
// 让连接不再复用，剩下的字节不会被接着读下去。
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, errBodyTooLarge
		}
		return nil, fmt.Errorf("读取请求体失败: %w", err)
	}
	return body, nil
}

// rejection 是一次请求检查没通过的结果。
//
// 它是个普通的决策值而不是 error：这里既没有要 unwrap 的成因链，也没有
// 要按类型分支的调用方——状态码与给调用方的措辞一起决定，然后直接写出响应。
// 拆成 error 再映射一次，只会让「为什么拒绝」与「回什么状态码」在两处
// 各写一遍，然后慢慢不一致。
type rejection struct {
	status  int
	message string
}

// checkRequest 检查请求里缺了就没法处理的部分。返回 nil 表示可以继续。
//
// 只检查「无论如何都做不成」的两种：没有模型名、没有消息。其余的参数由
// 上游判断——各家支持的范围不同，在这里按我们的猜测拒收，会把上游支持的
// 请求挡在门外。
//
// protocol 是**调用方走的入口**，不参与判据（判据是转换之后的
// channel.Request.Messages，三条入口共用），只决定措辞该点哪个字段——见
// emptyRequestMessage。
func checkRequest(req *channel.Request, protocol channel.Protocol) *rejection {
	if req.Model == "" {
		return &rejection{status: http.StatusBadRequest, message: "model is required"}
	}
	if len(req.Messages) == 0 {
		return &rejection{status: http.StatusBadRequest, message: emptyRequestMessage(protocol)}
	}
	return nil
}

// emptyRequestMessage 是「请求里一条消息都没有」时给调用方的措辞。
//
// 判据在三条入口上是同一个（转换之后没有消息），但**字段名是各协议自己的**：
// /v1/chat/completions 与 /v1/messages 的调用方发的是 messages，而
// /v1/responses 的调用方发的是顶层 input——后者收到「messages is required」
// 会去改一个它从来没写过的字段。
func emptyRequestMessage(protocol channel.Protocol) string {
	if protocol == channel.ProtocolOpenAIResponses {
		return "input is required"
	}
	return "messages is required"
}

// billingRequest 组装计费侧要的那份请求。
//
// 计费要知道的与调上游要知道的是**两个请求**（见 pipeline.Request 的注释
// 与 Chatter 的签名），这里只放计费要的，上游要的留在 upstream 里。
//
// protocol 由调用方从路由处带进来：它是「调用方走的是哪条入口」这件事，
// 与 upstream 里那些上游参数无关，所以不放进 upstream。
//
// 资产品种取自 Handler.DefaultAsset——那是装配层从 settings 的
// billing.default_asset 读出来的值。本包不认识那个键，也不给默认值：
// 计费层拒绝空品种（见 pipeline.Request.validate），所以「装配时忘了填」
// 会表现为每个请求都失败，而不是一笔记错品种的账。
func (h *Handler) billingRequest(user User, upstream *channel.Request, requestID string, protocol channel.Protocol) pipeline.Request {
	return pipeline.Request{
		RequestID: requestID,
		UserID:    user.ID,
		// APIKeyID 与 UserID 一起取自鉴权结果，但两者不是一回事：一个用户
		// 可以有多把 key，而账单要能追到**哪一把**在用。鉴权那一步已经把它
		// 解析出来了（限流按 key 计，用的就是同一个值），这里只是不让它在
		// 入口与落库层之间断掉——断了的表现是那一列恒为 NULL，
		// 而单看任何一层都像是写了的。
		APIKeyID: user.KeyID,
		Asset:    h.DefaultAsset,
		// CanonicalModel 是**调用方要的那个**名字。上游名由选路层按选中的
		// 渠道翻出来（见 internal/route 的 upstreamRequest），落进
		// billing_requests.upstream_model；两者在这一层分开，是因为入口
		// 还不知道会走哪个渠道，而别名是逐渠道配的。
		CanonicalModel: upstream.Model,
		MaxTokens:      int64(upstream.MaxTokens),
		InputTokens:    inputTokensOf(upstream),
		// CachePrefix 是前缀比对用的块序列（见 pipeline.CachePrefixOf）：
		// 预留靠它判断这一段的开头是否已在上游缓存里，从而把那一段从
		// 「按最贵单价估」里拆出来。算不出来时是 nil，预留退回旧口径。
		CachePrefix: pipeline.CachePrefixOf(upstream),
		Streamed:    upstream.Stream,
		// 协议不能由这一层推：它由路径决定，而路径是调用方的事
		// （见 pipeline.Request.Protocol）。
		Protocol: protocol,
	}
}

// inputTokensOf 估算请求的输入 token 数。
//
// 预留发生在调用上游之前，那时拿不到实测值，只能用估算（见 internal/tokens）。
// 口径有四块，都是上游会计入输入的部分：
//
//  1. **消息正文**——channel.Request.PromptText，逐字分词或估算；
//  2. **工具定义**——PromptText 只拼 Content[].Text，工具定义整个在它外面，
//     而在带工具的大请求里它常常比正文还大（见 tokens.ToolDefinitionInputTokens）；
//  3. **图片**——同样在外面，按每张一个保守常数计（见 tokens.ImageInputTokens）；
//  4. **历史消息里 tool_use 块的参数**——同样在 Content[].Text 之外
//     （那一块没有正文），而它在多轮工具调用的会话里**随轮数增长**：
//     每一轮调用的参数都会随后续请求重复出现在历史里
//     （见 tokens.ToolUseInputTokens）。
//
// # 补这几项补的是口径，不是精度
//
// 结算按上游报的 usage，那份 usage **含**上面四项；预留只算正文，于是
// 预留 < 结算，而结算是按预留封顶的（见 pipeline.Reserve 与
// billing.Settlement.Capped）——差额由平台承担，账目仍然是平的，只在对账时
// 表现为「用量对不上金额」，并让 larkax_billing_capped_amount_total 变成
// 常态噪声，而它本该只在「上游不守信」时出现。
//
// 所以这里**不加整体余量**：余量是把上界做松，补漏才是把上界做实。口径按
// 实际出现的那几项走，一项都没有时本函数与补漏前逐字相同（有用例钉住）。
// 加余量的实现会在那条用例上变红。
//
// # 为什么不按入口协议分支
//
// req 是**协议转换之后**的统一形状：工具是 []channel.Tool、图片与工具调用是
// ContentBlock.Type，三条 chat 形状的入口在这一层是同一套字段，
// 各入口写法上的差异在 relay.ParseRequest 就消掉了。所以这里不需要认识任何
// 入口协议，也就没有「四套口径」这回事。
//
// # 还有两处同名口径没跟上
//
// 它们是**另外两条路**，各自属于别的改动，不在本函数范围内，但要知道它们
// 今天仍按旧口径算：internal/route 的 NeedsOf（用的是上下文上限的判据，
// 偏小等于判据偏宽松）与 internal/gateway 流式中断时的
// ObserveRequest(PromptText)（拿不到实测 usage 时的兜底，偏小等于少收）。
//
// 流式那条路用同一个函数：它要填进 Anthropic 的 message_start，而同一个
// 请求在预留与响应里报出两个不同的输入量，只会让对账的人去找一个不存在的
// 差额。
func inputTokensOf(req *channel.Request) int64 {
	n := tokens.EstimatedTokens(req.Model, req.PromptText())
	for _, tool := range req.Tools {
		n += tokens.ToolDefinitionInputTokens(req.Model, toolDefinitionJSON(tool))
	}
	for _, m := range req.Messages {
		for _, c := range m.Content {
			// 按块的 Type 判，不看它带没带载荷：一条 image 块该计多少
			// 与载荷大小无关（见 tokens.ImageInputTokens），而带没带载荷
			// 这件事在适配器那一侧还有别的分支要管。
			if c.Type == "image" {
				n += tokens.ImageInputTokens
			}
			// tool_use 则相反，它按载荷算：参数多大就估多少。同一块在历史里
			// 出现几次就计几次——多轮工具调用的会话里它确实会重复出现。
			//
			// tool_result 不在这里：它的正文存在 ContentBlock.Text 上，
			// PromptText 第一项已经算了（见 relay.messageFromRelaykit 的那一支）。
			if c.Type == "tool_use" {
				n += tokens.ToolUseInputTokens(req.Model, toolUseBlockJSON(c))
			}
		}
	}
	return n
}

// toolUseBlockJSON 把历史消息里一块 tool_use 序列化成喂给估算的 JSON。
//
// 放 id / name / input 三个字段：它们是这一块携带的全部**内容**，也都被上游
// 计进输入。input 是上一轮工具调用的参数（一段完整的 JSON），在历史里会随轮数
// 重复出现；id 与 name 同样逐轮重复，只是小得多。type 不进这里——它每块都
// 一样，属于外面那层包装。
//
// 外面那层包装（各家在自己那份 JSON 上还会套的字段与转义）由
// tokens.ToolUseReserveFrameTokens 按块数补。
//
// 序列化失败时退到「id 与 name」：input 是 map[string]any，从请求体解出来的
// 输入序列化不会失败，但**失败时不能返回空**——那等于把这一块从口径里悄悄
// 抹掉，而它恰恰是这一层要补的东西。退回去的那一份仍然非零。
func toolUseBlockJSON(block channel.ContentBlock) []byte {
	body, err := json.Marshal(struct {
		ID    string         `json:"id,omitempty"`
		Name  string         `json:"name,omitempty"`
		Input map[string]any `json:"input,omitempty"`
	}{ID: block.ToolUseID, Name: block.ToolName, Input: block.ToolInput})
	if err == nil {
		return body
	}
	return []byte(block.ToolUseID + " " + block.ToolName)
}

// toolDefinitionJSON 把工具定义序列化成喂给估算的 JSON。
//
// 只放 name / description / schema 三个字段——channel.Tool 上就这三个，
// 而它们正是各家的工具声明里都有的内容。外面那层包装各家写法不同，
// 由 tokens.ToolDefinitionReserveFrameTokens 按条数补（那是**预留**侧的那个数，
// 与流式中断那条路计费用的 ToolDefinitionFrameTokens 刻意不同）。
//
// 序列化失败时退到「名字与描述」：schema 是 map[string]any，从请求体解出来的
// 输入序列化不会失败，但**失败时不能返回空**——那等于把一个工具定义从口径里
// 悄悄抹掉，而它恰恰是这一层要补的东西。退回去的那一份仍然非零。
func toolDefinitionJSON(tool channel.Tool) []byte {
	body, err := json.Marshal(struct {
		Name        string         `json:"name"`
		Description string         `json:"description,omitempty"`
		Parameters  map[string]any `json:"parameters,omitempty"`
	}{Name: tool.Name, Description: tool.Description, Parameters: tool.InputSchema})
	if err == nil {
		return body
	}
	return []byte(tool.Name + " " + tool.Description)
}

// requestIDOf 取出本次请求的关联标识，没有就生成一个。
//
// 不能留空：它同时是响应里的 id 与计费的幂等键，而空字符串在库里是
// 一条会与下一条撞上的唯一约束（billing_requests.request_id）。
// 调用链上没有 observe.Middleware 时（例如某个调用方直接用了本 handler），
// 这里补一个，而不是把「有没有装中间件」变成计费能不能落库的前提。
func requestIDOf(r *http.Request) string {
	if id := observe.RequestIDFromContext(r.Context()); id != "" {
		return string(id)
	}
	return string(observe.NewRequestID())
}
