// Package relay 是本项目与 relaykit 之间的唯一一道缝。
//
// 为什么要有这道缝，而不是让 relaykit 的类型直接进管道：
//
//   - relaykit 是 v0.x，它自己声明公开 API 可能在小版本之间变化。把它放在核心
//     词汇里，等于每次升级都要在管道的每一层做一次适配。
//   - relaykit 的 dto 包里混着大量 new-api 宿主专用结构（渠道设置、定价、
//     用户设置），那些与本项目无关，却会因为同一个包名而出现在补全列表里。
//   - 本项目自己的类型携带了明确的语义决定（Temperature 用指针区分「0」与
//     「未指定」、内容是有结构的块序列、流式工具调用给字符串片段）。这些决定
//     需要一个能被注释解释清楚的地方。
//
// 代价是每个请求多一次类型转换。那点开销换来的是：升级 relaykit 只改这一个
// 文件，以及管道里所有类型都有本项目的注释可读。
//
// 直接 import relaykit 的只允许本包与各协议适配器——后者要拿 relaykit 的
// dto 类型才能与它对接。允许清单写在 scripts/check-layering.sh 里，由 CI
// 检查，每接一个协议就多一个包。其余包若需要 relaykit 的类型，说明这里缺
// 一个转换函数。
package relay

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/relayconvert/kitutil"
	"github.com/QuantumNous/new-api/relaykit/types"

	"github.com/sumwai/larkax/internal/channel"
)

// Format 是 relaykit 对协议族的称呼，本包把它映射成本项目的 Protocol。
//
// 只映射本项目真正支持的那几套。relaykit 还认 Gemini、Realtime 等，
// 那些等接进来时再补——提前为没有适配器的协议写映射，只会让「以为支持了」
// 这件事更容易发生。
func formatFor(p channel.Protocol) (types.RelayFormat, error) {
	switch p {
	case channel.ProtocolOpenAI:
		return types.RelayFormatOpenAI, nil
	case channel.ProtocolAnthropic:
		return types.RelayFormatClaude, nil
	case channel.ProtocolOpenAIResponses:
		return types.RelayFormatOpenAIResponses, nil
	default:
		// 只认这几套。在这里猜一个格式会掩盖「调用方给的路径与本项目的
		// 协议不匹配」，而那是一个必须报出来的错误。
		return "", fmt.Errorf("协议 %q 没有对应的 relaykit 格式", p)
	}
}

// ToRelaykit 把本项目的请求转成 relaykit 的 OpenAI Chat 请求。
//
// 中间以 OpenAI Chat 为枢纽：relaykit 的转换矩阵从它出发能到 Claude，
// 反向也能回来。用枢纽而不是为「每种源 × 每种目标」各写一条路径，
// 是因为组合数增长得比代码快，而枢纽这一层的损耗已经被 relaykit 处理过
// （它的转换器读过两边的语义差异）。
func ToRelaykit(req *channel.Request) (*dto.GeneralOpenAIRequest, error) {
	if req == nil {
		return nil, fmt.Errorf("请求为 nil")
	}

	out := &dto.GeneralOpenAIRequest{
		Model:           req.Model,
		ReasoningEffort: req.ReasoningEffort,
		Temperature:     req.Temperature,
		TopP:            req.TopP,
	}
	if req.MaxTokens > 0 {
		maxTokens := uint(req.MaxTokens)
		out.MaxTokens = &maxTokens
	}
	if req.Stream {
		stream := true
		out.Stream = &stream
	}

	for _, m := range req.Messages {
		converted, err := messageToRelaykit(m)
		if err != nil {
			return nil, err
		}
		out.Messages = append(out.Messages, converted)
	}

	for _, t := range req.Tools {
		out.Tools = append(out.Tools, dto.ToolCallRequest{
			Type: "function",
			Function: dto.FunctionRequest{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}

	if len(req.Extra) > 0 {
		extra, err := extrasToRelaykit(req.Extra)
		if err != nil {
			return nil, err
		}
		// 上游特有参数逐个挂上去。挂在已知字段之后，因此不会覆盖本项目
		// 明确设过的值——Extra 的定位是补充，不是覆盖。
		applyExtras(out, extra)
	}
	return out, nil
}

// messageToRelaykit 转换一条消息。
func messageToRelaykit(m channel.Message) (dto.Message, error) {
	out := dto.Message{Role: m.Role}

	// 工具结果是独立的一条消息（role=tool），它的 tool_call_id 挂在消息上而不是内容里。
	// 这与「内容块」的模型不同，需要单独处理，否则工具往返会丢掉关联关系。
	var (
		textParts []string
		media     []dto.MediaContent
		toolCalls []dto.ToolCallResponse
		toolResID string
	)
	for _, b := range m.Content {
		switch b.Type {
		case "text":
			textParts = append(textParts, b.Text)
			media = append(media, dto.MediaContent{Type: "text", Text: b.Text})
		case "image":
			media = append(media, imageBlock(b))
		case "tool_use":
			args, err := json.Marshal(b.ToolInput)
			if err != nil {
				return out, fmt.Errorf("工具 %q 的参数无法序列化: %w", b.ToolName, err)
			}
			toolCalls = append(toolCalls, dto.ToolCallResponse{
				ID:   b.ToolUseID,
				Type: "function",
				Function: dto.FunctionResponse{
					Name:      b.ToolName,
					Arguments: string(args),
				},
			})
		case "tool_result":
			toolResID = b.ToolUseID
			// 工具结果的内容按文本处理。它可能是结构化数据，但上游协议里
			// 这一段的传输形式就是文本或内容块序列，保持原样即可。
			textParts = append(textParts, b.Text)
			media = append(media, dto.MediaContent{Type: "text", Text: b.Text})
		case "thinking":
			// 思考内容不进 messages：它不是对话的一部分，而是模型的中间产物。
			// 上游对它的处理方式各不相同（有的要单独字段回传，有的直接丢弃），
			// 由适配器按需要决定，这里不猜。
		default:
			return out, fmt.Errorf("未知的内容块类型 %q", b.Type)
		}
	}

	switch m.Role {
	case "tool":
		out.ToolCallId = toolResID
		out.Content = joinOrBlocks(textParts, media)
	case "assistant":
		if len(toolCalls) > 0 {
			raw, err := json.Marshal(toolCalls)
			if err != nil {
				return out, fmt.Errorf("工具调用无法序列化: %w", err)
			}
			out.ToolCalls = raw
		}
		out.Content = joinOrBlocks(textParts, media)
	default:
		out.Content = joinOrBlocks(textParts, media)
	}
	return out, nil
}

// joinOrBlocks 在「纯文本」与「内容块序列」之间选择传输形式。
//
// 单块纯文本用字符串：那是各家 SDK 最普遍接受的形态，也能让日志与
// 结构快照读起来干净。一旦含图片或有多块，就必须用块序列，否则信息会丢。
func joinOrBlocks(parts []string, media []dto.MediaContent) any {
	if len(media) == 1 && media[0].Type == "text" {
		return media[0].Text
	}
	if len(media) == 0 {
		if len(parts) == 0 {
			return ""
		}
		return parts[0]
	}
	return media
}

func imageBlock(b channel.ContentBlock) dto.MediaContent {
	switch {
	case b.ImageURL != "":
		return dto.MediaContent{
			Type:     "image_url",
			ImageUrl: map[string]any{"url": b.ImageURL},
		}
	case b.ImageBase64 != "":
		url := "data:" + b.ImageMediaType + ";base64," + b.ImageBase64
		return dto.MediaContent{
			Type:     "image_url",
			ImageUrl: map[string]any{"url": url},
		}
	default:
		return dto.MediaContent{Type: "image_url"}
	}
}

func extrasToRelaykit(extra map[string]any) (map[string]json.RawMessage, error) {
	out := make(map[string]json.RawMessage, len(extra))
	for k, v := range extra {
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("扩展参数 %q 无法序列化: %w", k, err)
		}
		out[k] = raw
	}
	return out, nil
}

// applyExtras 把扩展参数挂到请求上。
//
// 只认已知的白名单字段。未知的键被忽略而不是塞进某个 RawMessage：
// 透传任意键意味着上游能通过 Extra 影响本项目的请求语义，
// 而那正是「插件不能改钱」这条边界的旁边。
func applyExtras(out *dto.GeneralOpenAIRequest, extra map[string]json.RawMessage) {
	if v, ok := extra["top_k"]; ok {
		var n int
		if json.Unmarshal(v, &n) == nil {
			out.TopK = &n
		}
	}
	if v, ok := extra["frequency_penalty"]; ok {
		var f float64
		if json.Unmarshal(v, &f) == nil {
			out.FrequencyPenalty = &f
		}
	}
	if v, ok := extra["presence_penalty"]; ok {
		var f float64
		if json.Unmarshal(v, &f) == nil {
			out.PresencePenalty = &f
		}
	}
	if v, ok := extra["seed"]; ok {
		var f float64
		if json.Unmarshal(v, &f) == nil {
			out.Seed = &f
		}
	}
	if v, ok := extra["parallel_tool_calls"]; ok {
		var b bool
		if json.Unmarshal(v, &b) == nil {
			out.ParallelTooCalls = &b
		}
	}
	if v, ok := extra["service_tier"]; ok {
		out.ServiceTier = v
	}
	if v, ok := extra["user"]; ok {
		out.User = v
	}
}

// ParseRequest 把调用方发来的请求体解析成管道内的请求。
//
// protocol 是**调用协议**，由入口按请求路径决定，不从请求体猜：两套协议的
// 请求体在浅层很像（都有 model 与 messages），猜错时解析往往照样成功，
// 于是错误被推迟到上游，以另一种面貌出现——那种失败排查起来要跨两个系统。
// 用路径决定，解析失败就发生在离原因最近的地方。
//
// 不认识的顶层字段被忽略而不是报错：调用方可能比我们新（见
// docs/versioning.md 的契约冻结规则），因为多了一个字段就拒收，
// 等于把一个非破坏性的加法变成了破坏性的。
//
// 解析完立刻交给 FromRelaykitRequest，于是「relaykit 的类型」这件事仍然
// 只出现在本包——入口只需要认识 protocol 与 body。
func ParseRequest(ctx context.Context, protocol channel.Protocol, body []byte) (*channel.Request, error) {
	var src any
	switch protocol {
	case channel.ProtocolOpenAI:
		src = &dto.GeneralOpenAIRequest{}
	case channel.ProtocolAnthropic:
		src = &dto.ClaudeRequest{}
	case channel.ProtocolOpenAIResponses:
		// 这一条的请求体与另外两条差得远，所以它走自己的解析（见
		// parseResponsesRequest），转出来的仍是同一个 OpenAI Chat 形状。
		req, err := parseResponsesRequest(ctx, body)
		if err != nil {
			return nil, err
		}
		return withNativeBody(req, body), nil
	default:
		// custom 没有固定的请求体形状：它由具体适配器说了算，
		// 在这里猜一个会让「调用方说的是哪套协议」这件事失去意义。
		return nil, fmt.Errorf("协议 %q 不能作为调用协议", protocol)
	}

	if err := kitutil.Unmarshal(body, src); err != nil {
		return nil, fmt.Errorf("解析请求体失败: %w", err)
	}
	req, err := FromRelaykitRequest(ctx, protocol, src)
	if err != nil {
		return nil, err
	}
	return withNativeBody(req, body), nil
}

// withNativeBody 把调用方发来的原始字节挂到解析结果上。
//
// 填在这里是因为这是唯一同时拿得到「原始字节」与「验证通过的请求」的位置：
// 两个解析函数只换回结构，拿不到原文；FromRelaykitRequest 另外也被别处调用，
// 而且它手上没有字节。放在 ParseRequest 里，入口的首次请求与撞键之后的重试
// 两个调用点就一次覆盖了。
//
// 挂的是同一个切片而不是拷贝：body 来自 readBody 的 io.ReadAll，调用方在
// 本次请求内不再改它，拷贝只是让一个大请求体在内存里多存一份。
func withNativeBody(req *channel.Request, body []byte) *channel.Request {
	req.NativeBody = body
	return req
}

// parseResponsesRequest 解析一条 OpenAI Responses 协议的请求体。
//
// 它不自己拼 messages：Responses 的 input[] 有十几种 item 类型
// （message / function_call / function_call_output / reasoning / item_reference…），
// system 是顶层的 instructions，工具声明是**平铺**的（{"type":"function","name":…}，
// 而不是 chat 的 function 嵌套）。那些差异 relaykit 的
// ResponsesRequestToChatCompletionsRequest 已经处理过并有测试覆盖，所以这里
// 只做「解 JSON → 交给它 → 落回本项目」。
//
// # 两类字段它替我们挡住
//
// **有状态的字段一律拒绝**，不静默丢弃：previous_response_id、conversation、
// prompt、context_management 都会让输入里含**上游持有的历史**，而本项目按
// 「输入全在请求体里」估算 token 做预留（见 FromRelaykitRequest 的
// MaxTokens 与 api 的 inputTokensOf）。估算低估的后果是预留不足、结算超支，
// 所以那几个字段由 relaykit 直接报错（表现为 400），而不是被丢掉之后
// 我们照旧按本地输入算账。
//
// **store 被丢弃**（它进了中间那个 chat 请求，而 FromRelaykitRequest 不读它）。
// 这是有意的：它只表达「上游可以留存这次请求」，不影响我们这边的输入量，
// 也不影响上游这次要算什么。丢弃的代价是调用方以为留了痕而实际没有——
// 而它真正会用到的续话手段是 previous_response_id，那一条已经被拒。
//
// # 工具声明里的类型要在这一步拦
//
// 非 function 的工具（type=mcp 的远端 MCP、web_search 这类内置工具）必须在
// 这里拒掉，因为转换链上**没有任何一层会替我们报错**：relaykit 的
// ResponsesRequestToChatCompletionsRequest 内部先经 toolconv 把工具声明从
// 请求里抽出来、再按目标格式重新挂回去，而目标格式（chat）表达不了这些类型，
// 于是它们被**静默丢掉**。实测发一个 type=mcp 的请求得到的是 200，工具不见了
// 而回答照常返回——客户端会以为工具挂上了，而它一次都不会被调用。
//
// FromRelaykitRequest 的 checkToolType 照不到这一条：它读的是转换**之后**的
// dto.ToolCallRequest.Type，而那时非 function 的声明已经没了。上游适配器那
// 一层同样照不到——channel.Tool 只有名字、描述与 schema，没有承载类型的字段。
// 入口是唯一还看得见原始 type 的地方，所以判据放在这里，并复用 checkToolType
// 的措辞（与另外两条入口同一句话）。
func parseResponsesRequest(ctx context.Context, body []byte) (*channel.Request, error) {
	src := &dto.OpenAIResponsesRequest{}
	if err := kitutil.Unmarshal(body, src); err != nil {
		return nil, fmt.Errorf("解析请求体失败: %w", err)
	}
	if err := checkResponsesTools(src.Tools); err != nil {
		return nil, err
	}
	converted, err := relayconvert.ResponsesRequestToChatCompletionsRequest(src)
	if err != nil {
		return nil, fmt.Errorf("解析 Responses 请求失败: %w", err)
	}
	return FromRelaykitRequest(ctx, channel.ProtocolOpenAIResponses, converted)
}

// checkResponsesTools 在转换之前拦下转发不了的工具声明。
//
// 它读的是**请求体里原始的 tools 数组**（转换之后就没有 type 可看了，见
// parseResponsesRequest 的说明），判据复用 checkToolType。
//
// # 与 chat 入口差一档：缺 type 也要拒
//
// checkToolType 放行空串，因为 chat 那边解出空串之后照样能转发（理由见它
// 自己的注释）。Responses 这边不行——toolconv 按 type 认工具，缺了它整条
// 声明会被丢掉，而请求照常成功。同一句「放行」在两条入口上落出相反的结果，
// 所以这一档在这里单独判，措辞也说清缺的是哪个字段。
func checkResponsesTools(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var tools []map[string]any
	if err := json.Unmarshal(raw, &tools); err != nil {
		return fmt.Errorf("解析工具声明失败: %w", err)
	}
	for _, tool := range tools {
		typ, _ := tool["type"].(string)
		if typ == "" {
			return fmt.Errorf("工具声明缺少 type 字段，本项目只能转发 type=%q 的工具", "function")
		}
		if err := checkToolType(typ); err != nil {
			return err
		}
	}
	return nil
}

// FromRelaykitRequest 把一个 relaykit 请求转成本项目的请求。
//
// 用途是接收客户端请求：客户端说 OpenAI 或 Anthropic 协议，先解析成
// relaykit 的类型，再用这里转成管道内的类型。放在本包而不是散在 gateway 里，
// 是为了让「relaykit 的类型」这件事只在这一个文件出现。
//
// ctx 透传给 relaykit 的转换，理由同 ToRelaykitClaude：转换里的规则求值
// 将来需要调用方的 context，而不是自己开一个新的。
func FromRelaykitRequest(ctx context.Context, target channel.Protocol, src any) (*channel.Request, error) {
	_, err := formatFor(target)
	if err != nil {
		return nil, err
	}

	// 先统一到 OpenAI Chat 形状，再从它翻译成管道类型。
	// Anthropic 请求先经 relaykit 转成 OpenAI 形状，于是下面只需要一条路径。
	var openaiReq *dto.GeneralOpenAIRequest
	switch v := src.(type) {
	case *dto.GeneralOpenAIRequest:
		openaiReq = v
	case dto.GeneralOpenAIRequest:
		openaiReq = &v
	case *dto.ClaudeRequest:
		res, err := relayconvert.ConvertRequest(ctx, nil, types.RelayFormatOpenAI, *v)
		if err != nil {
			return nil, fmt.Errorf("解析 Anthropic 请求失败: %w", err)
		}
		converted, ok := res.Value.(*dto.GeneralOpenAIRequest)
		if !ok {
			// 措辞避开句首大写：Go 的错误信息惯例是首词小写，
			// 「Anthropic」这类专有名词放在句首会让 ST1005 判为格式错误。
			return nil, fmt.Errorf("解析 Anthropic 请求后得到意外类型 %T", res.Value)
		}
		openaiReq = converted
	default:
		return nil, fmt.Errorf("不认识的请求类型 %T", src)
	}

	out := &channel.Request{
		// target 是调用方走的那条入口协议（由请求路径决定）。执行器拿它把
		// 同一渠道下多套协议分派到对应的适配器；适配器自己不读它。
		Protocol:        target,
		Model:           openaiReq.Model,
		ReasoningEffort: openaiReq.ReasoningEffort,
		Temperature:     openaiReq.Temperature,
		TopP:            openaiReq.TopP,
		Stream:          openaiReq.Stream != nil && *openaiReq.Stream,
	}
	// 输出上界有两个拼法。MaxTokens 是 chat 的老名字，MaxCompletionTokens 是
	// 新名字——而 **Responses 的 max_output_tokens 经 relaykit 的转换落在
	// 后者上**（见 ResponsesRequestToChatCompletionsRequest）。只读前者的话，
	// Responses 入口的输出上界会静默丢掉：预留要有上界才做得出来
	// （见 pipeline.Request.validate 与 billing.ErrNoUpperBound），
	// 表现为**每一个** Responses 请求都 400，而错误说的是 max_tokens 缺失，
	// 排查方向会指向调用方写错了字段。
	//
	// 两者都给时以 MaxTokens 为准：chat 的语义是「同时给就是错的」，
	// 但那是上游该拒的事，不是这里该猜的。
	if openaiReq.MaxTokens != nil {
		out.MaxTokens = int(*openaiReq.MaxTokens)
	} else if openaiReq.MaxCompletionTokens != nil {
		out.MaxTokens = int(*openaiReq.MaxCompletionTokens)
	}

	for _, m := range openaiReq.Messages {
		msg, err := messageFromRelaykit(m)
		if err != nil {
			return nil, err
		}
		out.Messages = append(out.Messages, msg)
	}

	for _, t := range openaiReq.Tools {
		if err := checkToolType(t.Type); err != nil {
			return nil, err
		}
		schema, _ := t.Function.Parameters.(map[string]any)
		out.Tools = append(out.Tools, channel.Tool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: schema,
		})
	}
	return out, nil
}

// checkToolType 判断一条工具声明能不能被转发。
//
// 判据分三档，三档的理由不同，**不要顺手统一成一条**：
//
//   - `function`（显式声明）：放行。这是最常见的那条路。
//   - 空串：放行，按 function 处理。OpenAI 的 spec 里 type 是必填，但今天
//     省略它的客户端是能用的——解出来是空串，下面的循环读 t.Function.*
//     照常工作。拒掉它是把一个能用的形状改坏，不是为了修 bug。
//   - 其余任何非空取值：拒绝，错误里带上那个类型。
//
// 第三档为什么是「不等于 function」而不是「等于 mcp」：channel.Tool 今天
// 只有名字、描述与 schema，**没有承载工具类型的字段**，所以任何非 function
// 的声明都送不出去。而送不出去时的表现不是报错，是**静默消失**——两条入口
// 各是一种消失法：
//
//   - chat 入口：下面构造的 channel.Tool 再经 ToRelaykit 出来，一律是
//     {"type":"function"}，名字还可能被 omitempty 掉。客户端发
//     {"type":"mcp",…} 或 {"type":"web_search"} 时，上游收到的是一个我们
//     伪造出来的、谁都认不出的工具。
//   - Responses 入口：relaykit 的转换先经 toolconv 抽出工具再按 chat 格式挂
//     回去，表达不了的类型直接丢掉。调用方拿到的是 200，工具不见了。
//
// 两种都比拒绝更坏——拒绝至少指向正确方向，而这两种在响应上完全看不出来。
// 远端 MCP 尤其糟：chat 那一种表现为一个上游 400，而原因在我们发出去的请求
// 里，排查方向会指向对方。
//
// 它由两个调用点共用：FromRelaykitRequest 读转换后的声明（chat / Anthropic
// 两条入口），checkResponsesTools 读请求体里原始的 tools（Responses 入口，
// 那里转换之后已经没得看了）。
//
// 将来 channel.Tool 加了类型字段、内置工具能真正转发时，这条判据要收窄成
// 「仍然送不出去的那些」（届时 mcp 是其中之一）。relaykit 的
// ChatCompletionsRequestToResponsesRequest 对未知 type 是「原样保留形状」
// 地转发，所以收窄之后内置工具能过去；但**远端 MCP 仍然要拦**——它在服务端
// 执行，而这条链路只做协议转换。
func checkToolType(toolType string) error {
	if toolType == "" || toolType == "function" {
		return nil
	}
	return fmt.Errorf("工具声明的类型是 %q，本项目只能转发 function 类型的工具", toolType)
}

func messageFromRelaykit(m dto.Message) (channel.Message, error) {
	out := channel.Message{Role: m.Role}

	if len(m.ToolCalls) > 0 {
		var calls []dto.ToolCallResponse
		if err := json.Unmarshal(m.ToolCalls, &calls); err != nil {
			return out, fmt.Errorf("解析工具调用失败: %w", err)
		}
		for _, c := range calls {
			var input map[string]any
			if c.Function.Arguments != "" {
				// 参数解析失败不算致命：上游或客户端可能给了半截 JSON，
				// 而丢掉整条请求比丢掉一段参数更糟。保留原文供排查。
				if err := json.Unmarshal([]byte(c.Function.Arguments), &input); err != nil {
					input = map[string]any{"_raw": c.Function.Arguments}
				}
			}
			out.Content = append(out.Content, channel.ContentBlock{
				Type:      "tool_use",
				ToolUseID: c.ID,
				ToolName:  c.Function.Name,
				ToolInput: input,
			})
		}
	}

	// content 有四种承载形态，都要认：
	//
	//   - nil：只有工具调用的助手消息，内容本来就是空的；
	//   - string：纯文本消息，三条入口都能直接给；
	//   - []any：JSON 反序列化出来的块数组（OpenAI 入口与 Responses 入口
	//     走这一条）；
	//   - []dto.MediaContent：relaykit 挂上去的**带类型**块数组。Anthropic
	//     入口先经 relayconvert.ConvertRequest 转成 OpenAI 形状，那一步用
	//     dto.Message.SetMediaContent 挂内容，于是类型是具体切片而不是 []any。
	//
	// 除这四种之外的形态**必须报错，不能什么都不做**：什么都不做正是 Anthropic
	// 块数组被整条丢掉时的表现——文本与图片一起消失，转发出去的是空 content，
	// 而调用方拿到的是「模型答非所问」。在代码里「没命中任何 case」与「这条消息
	// 本来就没内容」长得一模一样，所以静默跳过这件事能一直藏着。报错时带上实际
	// 类型 %T——否则排查时只知道「不支持」，不知道不支持的是什么。
	//
	// 认不出的**块类型**（数组里的元素）不在此列：那由 contentBlockFrom 处置，
	// 它不转那个块但也不失败（理由见那里）。
	switch content := m.Content.(type) {
	case nil:
		// 内容为空是这条消息的正常形态，不是「认不出」：下面报错的那一档
		// 针对的是**有值但不是我们认识的形状**。
	case string:
		if content != "" {
			out.Content = append([]channel.ContentBlock{{Type: "text", Text: content}}, out.Content...)
		}
	case []any:
		for _, item := range content {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := block["type"].(string)
			text, _ := block["text"].(string)
			if converted, ok := contentBlockFrom(typ, text, block["image_url"]); ok {
				out.Content = append(out.Content, converted)
			}
		}
	case []dto.MediaContent:
		for _, item := range content {
			if converted, ok := contentBlockFrom(item.Type, item.Text, item.ImageUrl); ok {
				out.Content = append(out.Content, converted)
			}
		}
	default:
		return out, fmt.Errorf("消息内容是本项目不认识的形式 %T", m.Content)
	}

	// role=tool 的消息靠消息上的 tool_call_id 关联，把它补成内容块，
	// 否则管道里那条结果就成了无主的文本。
	if m.Role == "tool" && m.ToolCallId != "" {
		text := ""
		for _, b := range out.Content {
			if b.Type == "text" {
				text += b.Text
			}
		}
		out.Content = []channel.ContentBlock{{
			Type:      "tool_result",
			ToolUseID: m.ToolCallId,
			Text:      text,
		}}
	}

	// 标注排在 tool_result 的改写**之后**：那一支会把内容整个换掉（结果消息
	// 只剩一个块），挂在上面的标注会跟着消失。先挂后换是本文件里最容易写反
	// 的一处顺序。
	attachAnnotations(&out, m.Annotations)
	return out, nil
}

// attachAnnotations 把引用标注挂到正文块上，没有标注时什么都不做。
//
// 标注只属于正文：它描述的是「正文的哪一段引用了哪个来源」，挂在工具调用或
// 图片块上没有意义。而 relaykit 的 dto.Message.Annotations 是**整条消息一份**
// （它的形状就是从上游 content[].annotations 折过来的，已经丢掉了「属于哪一段」
// 这个信息），所以这里能给出的只是「这条消息有这批标注」。
//
// 挑第一个正文块，而且只挑**带文字**的那一个：空正文块（上游只给了标注、正文
// 还没到，或者纯工具调用）不接它——那会让「标注挂在哪一段上」随上游的分片
// 方式漂移。没有这样的块时就不挂（见函数末尾）。
//
// 解析不出来就返回：标注的字段集由上游决定，我们不解释它；一个不是对象的
// 元素谈不上「一条标注」，丢掉它比让整条响应失败更接近原意——这与工具参数
// 解析失败时的处置同一条理由（见上面的 tool_use 分支）。
func attachAnnotations(out *channel.Message, raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var items []any
	if err := json.Unmarshal(raw, &items); err != nil {
		return
	}
	annotations := make([]map[string]any, 0, len(items))
	for _, item := range items {
		obj, ok := item.(map[string]any)
		if !ok || len(obj) == 0 {
			continue
		}
		annotations = append(annotations, obj)
	}
	if len(annotations) == 0 {
		return
	}
	for i := range out.Content {
		if out.Content[i].Type == "text" && out.Content[i].Text != "" {
			out.Content[i].Annotations = annotations
			return
		}
	}
	// 没有带文字的正文块（纯工具调用的回答）：标注没有可挂的地方。不补一个
	// 空正文块去装它——那会让客户端看到一个并不存在的正文项。
}

// contentBlockFrom 把一个内容项折成管道里的内容块，是这件事在本包里的唯一
// 一处实现。
//
// 两个调用点描述的是同一件事——客户端给的一个内容项——只有承载它的类型不同：
// JSON 反序列化出来的是 map，relaykit 挂上来的是 dto.MediaContent。各写一份
// 的话，两份对「一个块有哪些字段」的理解迟早分岔，而分岔的表现正是本包刚修掉
// 的那类事故：一边转得出来、另一边转不出来，两边看上去都在工作。所以这里按
// 三个字段收口（类型、正文、图片地址），两条分支各自把值取出来交给它。
//
// 返回 false 表示这个块类型不转：它是「这个块我们不带」，与「认不出的承载形态」
// 是两回事，后者由 messageFromRelaykit 的类型 switch 报错。这里保留「不转」，
// 因为两类入口都会给出本项目管道表达不了的块（Anthropic 的 document、音频、
// 视频），把每一条都变成请求失败，等于把「少带一个块」升级成「整条请求不可用」。
func contentBlockFrom(typ, text string, imageURL any) (channel.ContentBlock, bool) {
	switch typ {
	case "text":
		return channel.ContentBlock{Type: "text", Text: text}, true
	case "image_url":
		return fromImageURL(imageURL), true
	default:
		return channel.ContentBlock{}, false
	}
}

// fromImageURL 读出一个图片块里的地址，返回管道里的图片块。
//
// 地址有三种写法，都实际出现过：
//
//   - 字符串：OpenAI 的简写形态 {"image_url":"https://…"}；
//   - 带 url 的对象：OpenAI 的完整形态 {"image_url":{"url":"…"}}；
//   - *dto.MessageImageUrl：relaykit 把 JSON 那一层解析掉之后的带类型形态
//     （Anthropic 入口的图片经转换后就是这个）。
//
// 认不出来时返回一个空的图片块而不是报错，与 contentBlockFrom 对认不出的块
// 类型的处置同一条理由（见它自己的注释）。
func fromImageURL(raw any) channel.ContentBlock {
	out := channel.ContentBlock{Type: "image"}
	var url string
	switch v := raw.(type) {
	case string:
		url = v
	case map[string]any:
		url, _ = v["url"].(string)
	case *dto.MessageImageUrl:
		if v != nil {
			url = v.Url
		}
	}
	if data, mediaType, ok := splitDataURL(url); ok {
		out.ImageBase64 = data
		out.ImageMediaType = mediaType
		return out
	}
	out.ImageURL = url
	return out
}

// ToRelaykitClaude 把统一请求转成 relaykit 的 Anthropic Messages 请求。
//
// 路径是「本项目 → OpenAI Chat → Claude Messages」：relaykit 的转换矩阵
// 以 OpenAI Chat 为枢纽，两边都能到，而直接写「本项目 → Claude」会让
// 枢纽这一层的语义差异（system 是顶层字段、max_tokens 必填、
// tool_use 与 tool_result 的结构不同）散落在两个地方。
//
// ctx 一路传到 relaykit 的转换里。现在那里用不到它，但这个参数是刻意保留的：
// 转换将来要按规则求值（例如按日期时段决定某些字段的取值），
// 那时它需要带着调用方的 context 去查数据，而不是自己开一个新的。
// 参数缺在签名里，将来就只能从 context.Background() 起一个——
// 那正是「取消信号与超时传不下去」的来源。
func ToRelaykitClaude(ctx context.Context, req *dto.GeneralOpenAIRequest) (*dto.ClaudeRequest, error) {
	if req == nil {
		return nil, fmt.Errorf("请求为 nil")
	}
	res, err := relayconvert.ConvertRequest(ctx, nil, types.RelayFormatClaude, *req)
	if err != nil {
		return nil, fmt.Errorf("转换为 Anthropic 请求失败: %w", err)
	}
	out, ok := res.Value.(*dto.ClaudeRequest)
	if !ok {
		return nil, fmt.Errorf("转换后得到意外类型 %T", res.Value)
	}
	return out, nil
}

// ToRelaykitResponses 把统一请求转成 relaykit 的 OpenAI Responses 请求。
//
// 路径与 ToRelaykitClaude 同源：「本项目 → OpenAI Chat → Responses」。
// relaykit 的转换矩阵以 OpenAI Chat 为枢纽，两边都能到，而直接写
// 「本项目 → Responses」会让枢纽这一层已经处理过的差异（instructions 是
// 顶层字段而不是一条消息、max_tokens 叫 max_output_tokens、工具声明的
// 形状不同）在这里重做一遍。
//
// 与 Claude 那一条的差别：Responses 的 max_output_tokens 是**可选**的，
// 缺了走上游默认值，所以这里不像 ToRelaykitClaude 那样补一个默认值。
func ToRelaykitResponses(ctx context.Context, req *dto.GeneralOpenAIRequest) (*dto.OpenAIResponsesRequest, error) {
	if req == nil {
		return nil, fmt.Errorf("请求为 nil")
	}
	res, err := relayconvert.ConvertRequest(ctx, nil, types.RelayFormatOpenAIResponses, *req)
	if err != nil {
		return nil, fmt.Errorf("转换为 OpenAI Responses 请求失败: %w", err)
	}
	out, ok := res.Value.(*dto.OpenAIResponsesRequest)
	if !ok {
		return nil, fmt.Errorf("转换后得到意外类型 %T", res.Value)
	}
	return out, nil
}
