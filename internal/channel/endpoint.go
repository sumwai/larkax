package channel

import "fmt"

// Endpoint 是一次上游请求打到哪条路径。
//
// 它与 Protocol 是两个维度，今天一一对应：一套协议族的适配器只往一条路径发。
// 分开命名是因为判据的两侧不是同一件事——一侧是**渠道**（它的 type 决定适配器
// 打哪条路径），另一侧是**模型**（模型目录声明这个模型在哪几条端点上被服务）。
// 两边对上才放行。
//
// 取值就是适配器实际发的路径，所以与上游模型清单里那份 supported_endpoints
// 是同一套口径。
//
// # 为什么渠道 type 不等于端点
//
// 一个上游同时有 /v1/chat/completions、/v1/responses、/v1/messages 三套端点，
// **由模型决定走哪一套**：同一个账号下 A 模型只在 chat 那边被服务，B 模型只在
// responses 那边。往模型不支持的那条端点发，上游回 400 unsupported_model——
// 从我们这边看与「模型不存在」长得一样，要等真打一次才发现。
//
// 所以渠道的 type 只承担协议含义（用哪个适配器），「这条渠道服务哪些端点」由
// 适配器实际打的那条路径回答（见 Protocol.Endpoint），而「这个模型在哪几条
// 端点上被服务」是模型目录里的一条声明（见 internal/catalog 的 Capabilities）。
type Endpoint string

// 各端点的取值。
//
// 写的是**路径**而不是一个抽象名字，因为上游的模型清单也是按路径报的
// （supported_endpoints 那一条），两套口径合一套才不会在中间多一层翻译。
//
// 常量名带 Endpoint 前缀而不是叫 ChatCompletions：这四个取值会与协议名一起
// 出现在调用点上，没有前缀时 `channel.Responses` 读不出它说的是协议还是路径。
const (
	EndpointChatCompletions Endpoint = "/v1/chat/completions"
	EndpointResponses       Endpoint = "/v1/responses"
	EndpointMessages        Endpoint = "/v1/messages"
	EndpointSystemOne       Endpoint = "/v1/systemone"
)

// Valid 报告它是不是上面这几个取值之一。
//
// 它是「外部输入」那道门的判据：库里的 supported_endpoints 是人工/采集写进去的
// 一段文本，读到别的串时调用方要能分辨「这一项是错的」与「这一项没配过」。
// 后者是空 endpoints（nil 切片），不是空串——空串在这套取值里不合法。
func (e Endpoint) Valid() bool {
	switch e {
	case EndpointChatCompletions, EndpointResponses, EndpointMessages, EndpointSystemOne:
		return true
	}
	return false
}

// ParseEndpoint 把一个字符串解析成一个端点取值。
//
// # 解析不出来就报错，不返回空串
//
// 空串在这里的意思是「不知道」（见 Protocol.Endpoint），与「这一串是错的」
// 必须分开：前者的处置是放行（不知道就别拦），后者是这一行数据有问题。
// 把非法串折成空串会让它在选路里表现成「未知」，于是「这一行写错了」变成
// 「这一行没配过」——一条拼错的端点声明从此静默不生效。
func ParseEndpoint(raw string) (Endpoint, error) {
	e := Endpoint(raw)
	if !e.Valid() {
		return "", fmt.Errorf("未知的上游端点 %q（取值只能是 %s、%s、%s、%s）",
			raw, EndpointChatCompletions, EndpointResponses, EndpointMessages, EndpointSystemOne)
	}
	return e, nil
}

// Endpoint 返回这套协议族的适配器往哪条路径发请求。
//
// 未知协议返回空串。空串在端点评据里是「不知道」，与 Valid 为 false 一起
// 由调用方决定怎么处置（本仓的处置是放行，见 route.profileRejection）。
func (p Protocol) Endpoint() Endpoint {
	switch p {
	case ProtocolOpenAI:
		return EndpointChatCompletions
	case ProtocolOpenAIResponses:
		return EndpointResponses
	case ProtocolAnthropic:
		return EndpointMessages
	case ProtocolSystemOne:
		return EndpointSystemOne
	}
	return ""
}
