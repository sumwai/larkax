// Package idempotency 是请求级幂等键的语义与存储。
//
// # 它解决什么
//
// 网络超时、连接被中间设备切断、客户端进程被杀——这些情况下调用方**不知道
// 上游收没收到**，只能重试。没有幂等键时，重试就是两次预留、两次结算、
// 两次真实上游调用，而两条分录各自都平，对账看不出来。错误的方向是**多收**。
//
// 带上 `Idempotency-Key` 之后，同一个键的第二次请求不再走一遍，
// 而是拿到第一次的结果。
//
// # 作用域是「同一把 API Key 下」
//
// 键的唯一性是 (api_key_id, key)。不全局唯一：不同租户各自用自己的键空间，
// 全局唯一只会让一个用户能用掉另一个用户的键。
//
// # 三态
//
// 记录在**调上游之前**就建好（与预留同一个事务，见 ClaimInTx），此后：
//
//	in_progress  已受理，第一次请求还在跑
//	completed    第一次请求**走完了**，响应完整地存在库里，可以重放
//	failed       第一次请求没走完（上游失败、客户端中途断开、进程没了）
//
// 「走完」是重放的前提。重放一段截断的响应比不重放更坏：调用方会把它
// 当成一次成功的完整回答，而它其实少了后半段。
package idempotency

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/sumwai/larkax/internal/channel"
)

// HeaderName 是幂等键所在的请求头。
//
// 只认请求头，不认请求体字段：请求体必须与两套协议保持逐字节兼容，
// 加一个字段要么被严格解析拒掉，要么漏进快照与计价路径。请求头是这件事
// 的既有惯例，也不污染协议面。
const HeaderName = "Idempotency-Key"

// MaxKeyBytes 是幂等键的长度上限。
//
// 键会成为 (api_key_id, key) 唯一索引的一部分，所以它必须是有界的：
// 不设上限时，一个把整段 base64 塞进这个头的调用方就能把索引撑大，
// 而那是**别人**的库要承担的代价。
//
// 256 是各家 SDK 生成 uuid / ulid 之后还绰绰有余的量级。
const MaxKeyBytes = 256

// ValidateKey 检查一个幂等键能不能用。
//
// 只收可见 ASCII：键会进日志与错误信息，而换行、控制字符会让一行日志
// 断成两行，把一个可读的记录变成一份读不出边界的文本。
func ValidateKey(key string) error {
	if key == "" {
		return errors.New("幂等键为空")
	}
	if len(key) > MaxKeyBytes {
		return fmt.Errorf("幂等键超过 %d 字节", MaxKeyBytes)
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 0x21 || key[i] > 0x7e {
			return errors.New("幂等键只能包含可见 ASCII 字符")
		}
	}
	return nil
}

// State 是幂等记录的状态。
type State string

const (
	// StateInProgress 表示这次请求已受理，第一次调用还在跑。
	StateInProgress State = "in_progress"
	// StateCompleted 表示第一次请求走完了，响应完整地存着，可以重放。
	StateCompleted State = "completed"
	// StateFailed 表示第一次请求没走完。它**不会**被重放，也不会被重新执行。
	StateFailed State = "failed"
)

// Claim 是一次幂等声明：谁、用哪个键、请求体长什么样。
//
// 它在**调上游之前**建立，并且与预留写在同一个事务里（见 ClaimInTx）。
// 两者必须一起成或一起不成：只记了幂等而没预留，重试会被告知「已处理」
// 而钱没扣；只预留而没记幂等，重试就重复扣费。
type Claim struct {
	// APIKeyID 是这次请求用的那把 API Key。作用域是它，不是用户——
	// 一个用户可以有多个 key，而「重试」是客户端拿同一把 key 再发一次。
	APIKeyID int64
	// Key 是调用方给的键，已经过 ValidateKey。
	Key string
	// Hash 是请求体的指纹，见 HashRequest。键相同而指纹不同时报错，
	// 不当成新请求。
	Hash [32]byte
}

// Record 是库里的一条幂等记录。
type Record struct {
	State State
	Hash  [32]byte

	// ResponseStatus / ResponseContentType / ResponseBody 是第一次请求的
	// **完整响应**。只有 StateCompleted 时它们才有值。
	//
	// 存字节而不是「结构化响应」：重放要的是逐字节相同，而重新渲染一次
	// 会引入第二处渲染逻辑——两处迟早会不一致，而那种不一致表现为
	// 「重试拿到的回答与第一次不一样」。
	ResponseStatus      int
	ResponseContentType string
	ResponseBody        []byte

	// BillingRequestID 是这次请求在 billing_requests 里的那一行。
	//
	// 它由同一个事务写入（见 ClaimInTx），所以它非零就说明预留真的落了库。
	// 排查「客户端说重试被拒但我找不到这次请求」时，这是唯一的入口。
	BillingRequestID int64
}

// ErrNotFound 表示这个键没有记录。
var ErrNotFound = errors.New("没有这个幂等键的记录")

// ErrClaimed 表示这个键已经被另一个请求占住了。
//
// 它不是失败，而是一个**竞态信号**：调用方该去读那条记录，按它的状态
// 决定等还是重放，而不是把这个错误回给客户端。
var ErrClaimed = errors.New("这个幂等键已被另一个请求占用")

// hashInput 是进指纹的字段集合。
//
// # 为什么逐个列出，而不是「整个请求体的 sha256」
//
// 两套协议里有几类字段**不该**影响幂等性，而它们恰好是重试时最容易变的：
// 客户端自己填的请求 id、时间戳、纯展示用的元数据。按原文哈希会把它们算进去，
// 于是「同一个请求重发一次」被当成「同键不同体」拒掉——幂等键就白设了。
//
// # 为什么哈希的是**解析后**的请求
//
// 解析器（internal/relay）只把管道真正会用到的字段搬过来，其余一律丢掉。
// 那些被丢掉的字段改不了上游调用，也就改不了结果，所以它们自动不进指纹——
// 这一条不是靠这里少列几个字段做到的，是靠哈希的对象本来就是解析结果。
//
// 逐字段的理由：
//
//   - Protocol：协议不同，响应体的**形状**就不同（OpenAI 与 Anthropic 的
//     成功响应与错误体都不一样）。同一个键在两条入口上重放，会把一种形状
//     的响应体发给按另一种形状解析的 SDK。
//   - Model：换模型就是换一次请求，答案与价格都不同。
//   - Messages：对话内容本身。
//   - MaxTokens：输出上界，影响预留金额与答案长度。
//   - Temperature / TopP / ReasoningEffort：采样参数，影响答案。
//   - Tools：工具定义，影响答案与上游是否走工具调用。
//   - Stream：流式与非流式的结果形状不同（一串 SSE 帧 vs 一个 JSON 对象），
//     存下来的那份只能按它当初的样子重放。
//   - Extra：上游特有参数，适配器会把它透传给上游（见 internal/relay 的
//     ToRelaykit），所以它影响结果。
//
// 刻意**不**进来的：
//
//   - channel.Request.Headers：它不是调用方给的东西，是插件层加上游请求的
//     头。同一份请求经同一批插件得到的头是一样的，它不携带独立信息；
//     而且在入口算指纹的那一刻它还是空的。
//   - 上游模型名映射、选中的渠道、匹配到的计价规则：那些由选路与计价决定，
//     与「调用方发的是不是同一个请求」无关。客户端重试时它们本来就可能不同
//     （换了个渠道），而重试要的正是「别再跑一遍」。
//
// 用 json.Marshal 而不是自己拼字节：map 的键序由它排序，浮点数的写法也
// 是确定的，于是同一份请求两次算出来的指纹一定相同。自己拼的话，
// 键序会随 map 的迭代顺序漂移——那种不确定只在并发下才暴露，且表现为
// 「同一个请求两次被判成不同的请求」。
type hashInput struct {
	Protocol        channel.Protocol  `json:"protocol"`
	Model           string            `json:"model"`
	Messages        []channel.Message `json:"messages"`
	MaxTokens       int               `json:"max_tokens"`
	Temperature     *float64          `json:"temperature"`
	TopP            *float64          `json:"top_p"`
	ReasoningEffort string            `json:"reasoning_effort"`
	Tools           []channel.Tool    `json:"tools"`
	Stream          bool              `json:"stream"`
	Extra           map[string]any    `json:"extra"`
}

// HashRequest 算出一次请求的幂等指纹。
//
// 返回错误而不是在失败时回落到一个默认值：回落会让两个**不同**的请求
// 得到同一个指纹，而后果是「第二个请求拿到第一个的响应」——那是对调用方
// 撒谎，且看不出来。宁可不做幂等。
//
// 实际能触发它的只有 Extra / ToolInput / InputSchema 那几个 map 里混进了
// JSON 编不出来的值（NaN、函数、channel）。从 JSON 解析出来的请求走不到
// 这里，但那些 map 也可能被插件写进去。
func HashRequest(protocol channel.Protocol, req *channel.Request) ([32]byte, error) {
	if req == nil {
		return [32]byte{}, errors.New("请求为 nil")
	}
	raw, err := json.Marshal(hashInput{
		Protocol:        protocol,
		Model:           req.Model,
		Messages:        req.Messages,
		MaxTokens:       req.MaxTokens,
		Temperature:     req.Temperature,
		TopP:            req.TopP,
		ReasoningEffort: req.ReasoningEffort,
		Tools:           req.Tools,
		Stream:          req.Stream,
		Extra:           req.Extra,
	})
	if err != nil {
		return [32]byte{}, fmt.Errorf("序列化请求以计算幂等指纹: %w", err)
	}
	return sha256.Sum256(raw), nil
}

// Retention 把小时数换成留存时长。
//
// 非正的小时数会被当成「不留存」而不是「留到永远」：settings 里写错一个
// 符号时，宁可清得比预期早（幂等退化，但账单不受影响），也不要让表无限
// 增长下去——后者是慢性的，几个月后才有人发现。
func Retention(hours int64) time.Duration {
	if hours <= 0 {
		return 0
	}
	return time.Duration(hours) * time.Hour
}
