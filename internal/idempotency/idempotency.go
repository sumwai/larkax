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

// ErrNotFound 表示这个键**根本没有**记录。
//
// 它有两个来路：Lookup 查不到，以及 Complete / Fail 要写的那条记录不在库里
// （那说明这一次请求的预留整个回滚了，见 store.go 的 requireTransition）。
// 两处说的是同一件事，所以共用一个哨兵，而不是各造一个措辞相近、含义相同的
// 错误值——两个值迟早会被当成两种情形分别处理。
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
// # 为什么哈希的是**解析后**的请求，而直通还要补一份残差
//
// 按字段列而不是按整份原文哈希，理由在上一节：原文里那些重试时最容易变的
// 东西（客户端自己填的 id、时间戳、展示用元数据）不该让一次重发被判成
// 「同键不同体」。这一半在两条路上都成立，没有变。
//
// 变的是另一半。原来这套做法有一个前提：解析器（internal/relay）只把管道
// 真正会用到的字段搬过来，其余一律丢掉，而丢掉的那些字段改不了上游调用。
// **同协议直通推翻了这个前提**——适配器以调用方的原文为报文来源，原样发给
// 上游，于是原文里那些统一结构不认识的字段（top_k、cache_control、
// stop_sequences、历史 thinking 块……）现在真的会改变上游行为。少了它们，
// 两份不同的报文会共用指纹，第二次拿到的是第一次的缓存应答。
//
// 所以指纹分两块：上面这份字段集合（解析结果），加上 NativeResidue——
// 原文里**读到但没进指纹**的那部分（见 nativeResidueOf）。原文为 nil
// （内部调用、跨协议请求）时残差是零值，指纹与加它之前逐字节相同。
//
// 残差里留什么、扣什么，按三分类：
//
//   - **读且无损** → 扣。值已经由上面某个字段携带，扣掉不会让两份报文合并。
//     典型是 role 与字符串形式的 content：枢纽把它们逐字搬进 Message 与
//     ContentBlock.Text。
//   - **读但有损（多对一折算）** → 不扣。枢纽读到它，却把它折成了比原文粗的
//     一个值；扣掉就等于宣称「凡折成同一个值的原文都是同一个请求」，而直通
//     把原文原样发上游，上游分得出它们的差别。thinking 的 budget_tokens 是
//     这一类的代表：枢纽把预算折成几档 effort（≤1024 是 low、≤8192 是
//     medium、其余 high，0 与负数另有取值），512 与 1024 都折成 low，
//     扣掉预算就让两份不同预算的报文共用指纹。注意这里**不是**「枢纽读不到」
//     ——hashInput 里就有 ReasoningEffort，枢纽确实读了它，只是读得粗。
//   - **读不到** → 不扣。直通把原文原样发上游，枢纽读不到的键对上游可能是
//     活的（cache_control 是最典型的：上游转换器读它，而 channel.ContentBlock
//     没有对应的字段，hashInput 里也就没有）。
//
// 逐字段的理由：
//
//   - Protocol：协议不同，响应体的**形状**就不同（OpenAI 与 Anthropic 的
//     成功响应与错误体都不一样）。同一个键在两条入口上重放，会把一种形状
//     的响应体发给按另一种形状解析的 SDK。
//
//   - Model：换模型就是换一次请求，答案与价格都不同。
//
//   - Messages：对话内容本身。
//
//   - MaxTokens：输出上界，影响预留金额与答案长度。
//
//   - Temperature / TopP / ReasoningEffort：采样参数，影响答案。
//
//   - Tools：工具定义，影响答案与上游是否走工具调用。
//
//   - Stream：流式与非流式的结果形状不同（一串 SSE 帧 vs 一个 JSON 对象），
//     存下来的那份只能按它当初的样子重放。
//
//   - Extra：上游特有参数。**它当前恒为 nil**——入口唯一的解析出口
//     （internal/relay 的 FromRelaykitRequest）不写这个字段，所以走公共入口
//     的请求在这里永远是 null。留着它是因为适配器那一层会把它透传给上游
//     （见 internal/relay 的 ToRelaykit）：哪天那条路接上了，改这个 map 就是
//     改一次请求，该算成另一次。写在这里是为了让「它现在不生效」与
//     「它为什么还在这张表里」两件事都留在同一处，而不是靠读代码去发现。
//
//     上一段说的「透传」今天实际生效的是下面的 NativeResidue：直通模式下
//     原文里那些 dto 不认识的参数由它进指纹。将来谁想接 Extra，先读
//     NativeResidue——不要造出两套机制管同一件事。
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

	// NativeResidue 是原文里**没被上面那几项覆盖**的部分（见 nativeResidueOf）。
	//
	// 直通模式下它才非空：原文为 nil 时它是零值，omitempty 让序列化结果与
	// 加这个字段之前逐字节相同——指纹是公开契约（docs/api.md 写着同一份
	// 请求两次得到同一个指纹），一个恒为 null 的新字段也会改变它的值。
	NativeResidue map[string]any `json:"native_residue,omitempty"`
}

// newHashInput 把一次请求折成进指纹的字段集合。
//
// HashRequest 走它，测试的前提断言也走它（见 native_residue_test.go）：
// 两处各拼一份的话，加一个字段时会出现「指纹算了、前提断言没算」，
// 而那种偏差只在某条用例变红时才暴露。
//
// 残差在这里就算好，不是留给调用方填：算残差会失败（原文不是 JSON 对象），
// 而失败必须并进 HashRequest 那一个 error——签名不动。
func newHashInput(protocol channel.Protocol, req *channel.Request) (hashInput, error) {
	// 扣除集合按**入口协议**取。以 req.Protocol 为准：它是「这份原文是从哪条
	// 入口收进来的」，扣键表认的正是这个。空串表示不知道入口是哪条（见
	// channel.Request.Protocol），那时退回 protocol 参数——两边都由入口填成
	// 同一个值，退回只是让「没有 req.Protocol 的调用点」不至于整份原文都进残差。
	entry := req.Protocol
	if entry == "" {
		entry = protocol
	}

	residue, err := nativeResidueOf(entry, req.NativeBody)
	if err != nil {
		return hashInput{}, err
	}
	return hashInput{
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
		NativeResidue:   residue,
	}, nil
}

// HashRequest 算出一次请求的幂等指纹。
//
// 返回错误而不是在失败时回落到一个默认值：回落会让两个**不同**的请求
// 得到同一个指纹，而后果是「第二个请求拿到第一个的响应」——那是对调用方
// 撒谎，且看不出来。宁可不做幂等。
//
// 实际能触发它的只有 Extra / ToolInput / InputSchema 那几个 map 里混进了
// JSON 编不出来的值（NaN、函数、channel），以及原文解析不出一个 JSON 对象。
// 后者只可能在 NativeBody 非 nil 时出现，而那意味着有人把一份解析不了的
// 字节塞进了这个字段；报错比落回空残差安全——空残差正是「两份不同原文
// 共用指纹」的那条路。
func HashRequest(protocol channel.Protocol, req *channel.Request) ([32]byte, error) {
	if req == nil {
		return [32]byte{}, errors.New("请求为 nil")
	}
	in, err := newHashInput(protocol, req)
	if err != nil {
		return [32]byte{}, err
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return [32]byte{}, fmt.Errorf("序列化请求以计算幂等指纹: %w", err)
	}
	return sha256.Sum256(raw), nil
}

// Retention 把小时数换成留存时长。
//
// 非正的小时数返回 0，而 0 到了 Cleanup 那里会被**拒绝**（见它的注释）：
// 净效果是清理命令响亮地报错、一条都不删，**不是**「不留存」。
//
// 这正是想要的：settings 里写错一个符号时，宁可让命令停下来让人看见，
// 也不要让它按一条写错的判据把整张表清空。这条注释与 Cleanup 的那条必须
// 说同一件事——两处各说各的，读的人会以为非正数意味着「什么都不删但命令
// 正常退出」，而实际上是「命令失败」。
func Retention(hours int64) time.Duration {
	if hours <= 0 {
		return 0
	}
	return time.Duration(hours) * time.Hour
}
