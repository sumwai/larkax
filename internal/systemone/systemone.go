// Package systemone 是 System One 决策表面的中间表示与线格式。
//
// # 它为什么不是 channel.Request
//
// 这条表面不是聊天：请求给一段 state 与一组带类型的判定问题，响应返回**概率**，
// 不返回文本。把它塞进 channel.Request（那个类型贴着 OpenAI 的 chat completions
// 形状）会让每个现有调用点都要处理「这个字段对这条协议无意义」——而
// 「有时无意义」的字段正是最容易出错的一类。
//
// 所以本包是**独立的一套表示**，三件事各自有明确的归属：
//
//   - 谁构造：internal/api 从请求体解出来（Parse），填进计费请求的是它算出来的
//     用量上界与文本。
//   - 谁消费：internal/channel/systemoneadapter 编码成上游 JSON 发出去
//     （Encode / ParseResponse），internal/api 渲染成给调用方的响应。
//   - 用量从哪来：上游响应里的 usage，由本包折进 channel.Usage（见 Usage）。
//
// # 线格式与上游一致
//
// 本包同时是这条表面的线格式：请求与响应的 JSON 形状就是上游的形状，
// larkax 只做选路、计量与账本，不替上游重新定一套调用形状。
// 编解码放在这里而不是适配器里，是为了让「这条表面长什么样」只有一份定义——
// 适配器只做 HTTP 层的事（地址、鉴权头、状态码到可重试的映射）。
package systemone

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/sumwai/larkax/internal/channel"
)

// 三种问题类型。取值就是线格式里的取值，不做本地改名——上游收到什么，
// 这里就是什么，多一层映射只会多一处会写错的地方。
const (
	// TypeNoul 是是/否判定，答案是「是」的概率。
	TypeNoul = "noul"
	// TypeChoice 是多选一，criteria 是「选项 → 说明」。
	TypeChoice = "choice"
	// TypeScore 是打分，criteria 是档位标签的数组，下标即分数。
	TypeScore = "score"
)

// MaxInputTokens 是输入侧的上界，单位是 token。
//
// 依据是上游文档给的两个预算口径：「state 加全部问题 64K token」与
// 「state 加最长单个问题 32K token」。取**较宽的那个**，因为预留要的是上界。
// 但**文档给出预算口径，不等于上游会强制它**——见下面 ⚠️ 那一节。
//
// 它是预留里输入侧取的数（见 internal/api 的 decideBillingRequest）：
// 这条表面拿不到输入的实际 token 数——那是上游在响应里报的，而预留发生在
// 请求发出之前。**按正文估出来的数不能当上界**：上游会自己拼一段指令开销进去，
// 估出来的是下界（见 Request.Text）。所以这里直接取上游自己的预算上界。
//
// # ⚠️ 这个上界是一个**待核实的假设**，不是既成事实
//
// 假设有两条：「上游真的会拒绝超出预算的请求」与「它算的预算包含自己拼的那段
// 指令开销」。**这两条都没有证据**——它们是从上游文档的预算口径转述来的，
// 没有实测过，也不该去连真实上游试。假设一旦不成立，合规请求的 input_tokens
// 就可能高于这个数。
//
// 后果是**静默**的：结算超过预留时 final_amount 被截成预留额（Capped），
// 账目仍然平、响应照常 200、日志里也没有异常。事后只有
// observe 的 larkax_billing_capped_settlements_total{protocol="systemone"}
// 能看见它发生过；这一次具体少了多少，要去库里查 computed_amount 与
// reserved_amount 的差额。所以不要把这里读成既成事实。
//
// 还有一点：「撞上界」的判据不是单独某一边超过它自己的上界，而是**两个常量
// 合起来的金额**（MaxInputTokens×输入单价 + MaxOutputTokens×输出单价）——
// 一侧超出可以被另一侧没用完的预留吸收。所以「输出没超过 4096」并不等于
// 没撞上界。
//
// # 实测：哪些用量组合会撞
//
// 下表是**测试口径**（prompt 0.00001 / completion 0.00003 每 token，两个常量
// 合起来算出预留 0.77824），不是真实价格。它比「会不会撞」这句话有用：
// 输入超过 64K 或输出超过 4096 本身都不等于撞上界，另一侧没用完的预留会
// 吸收它。
//
//	| 上游报的用量              | computed | final   |                          |
//	|--------------------------|----------|---------|--------------------------|
//	| input 70000 / output 72  | 0.70216  | 0.70216 | 未撞（输出侧的余量吸收） |
//	| input 387 / output 5000  | 0.15387  | 0.15387 | 未撞                     |
//	| input 65536 / output 5000| 0.80536  | 0.77824 | 撞上界                   |
//	| input 387 / output 30000 | 0.90387  | 0.77824 | 撞上界                   |
//
// 核实的时候要向上游确认两件事：预算是否由上游强制；input_tokens 是否计入
// 上游自己拼的那段指令开销。
const MaxInputTokens = 64 * 1024

// MaxOutputTokens 是输出侧的上界，单位是 token。
//
// **上游文档没有给这个数**——它只给了输入预算。4096 是保守估计，不是测出来的：
// 实测一次含三个问题的请求 output_tokens 是 72，量级在几十。
//
// 它是**必须有的**：这条表面没有 max_tokens 一类的参数，而预留必须有上界
// （见 pipeline.Request.validate），所以这个常量是上界的唯一来源。
// 它一旦偏小，结算就会在上界处被截断（Capped），差额由平台吃掉——调大它
// 之前先向上游确认真正的上界是多少，别按观测值调（观测值不是上界）。
const MaxOutputTokens = 4096

// Request 是一次决策请求。
//
// Model 在入口那一侧是**规范名**（调用方要的那个），到了适配器那一侧是
// **上游名**（路由层按选中的渠道翻出来的）——与 chat 那条路同一个约定，
// 见 pipeline.Request.CanonicalModel 的注释。
type Request struct {
	Model string

	// State 是这次判定所依据的上下文。
	State string

	// Questions 按 id 索引，**不是数组**。
	//
	// 这不是风格选择：上游对数组直接报错
	// （`Invalid input: expected record, received array`，param: questions）。
	Questions map[string]Question

	// Stream 只用于**识别**调用方要了流式。
	//
	// 这条表面从不流式：上游收到 stream 会忽略它，回的仍是普通 JSON。
	// 入口据此拒绝（见 internal/api 的 serveDecision），所以这个字段
	// 永远不会被编码进上游请求（omitempty 让它不出现在线上）。
	Stream bool
}

// Question 是一道判定问题。
//
// Criteria 在两种类型下形状不同，所以这里拆成两个字段而不是一个 any：
// 拆开之后「choice 必须有选项、score 必须有档位」是类型上看得见的事，
// 而不是要读一段断言才发现的约定。线格式里它们共用一个 criteria 键，
// 由本包的编解码负责合与拆。
type Question struct {
	// Type 取值见 TypeNoul / TypeChoice / TypeScore。
	Type string

	// Instructions 是这道问题要判什么。
	Instructions string

	// Choices 是 choice 的「选项 → 说明」。
	Choices map[string]string

	// Levels 是 score 的档位标签，**下标即分数**。
	Levels []string
}

// Response 是一次决策响应。
type Response struct {
	// Model 是上游回报的模型名。它与请求里的可能不同（上游做了别名或版本
	// 替换），计费与排查按它记，报给调用方的仍是调用方要的那个名字。
	Model string

	// Answers 按问题 id 索引。
	Answers map[string]Answer

	Usage Usage
}

// Answer 是一道问题的答案。
//
// 三种类型共用一个结构体而不是做成接口：它们的字段是**并列的**而不是
// 互斥的多态（一个 answer 只会有其中一组字段有值），而接口会让调用方为了
// 读一个概率写一次类型断言——那个断言失败时表现为「答案读不到」，
// 比读到一个零值更难查。
type Answer struct {
	// Type 是这道答案的问题类型。
	Type string

	// Noul 是 noul 的答案：「是」的概率，落在 [0,1]。
	Noul float64

	// Choice 是 choice 选中的选项，Confidence 是它的置信度。
	Choice     string
	Confidence float64

	// Score 是 score 的得分。它是**数值**而不是档位下标：上游报的 1.8 落在
	// 下标 1 与 2 之间，按 Legend 读才是有意义的那一档。
	Score float64

	// Probabilities 是逐项概率。键的含义随类型变：choice 是选项名，
	// score 是档位下标的字符串形式（"0"、"1"…）。
	Probabilities map[string]float64

	// Legend 是 score 的「下标 → 标签」映射。其余类型为空。
	Legend map[string]string
}

// Usage 是这条表面的用量。
//
// 字段名与 chat 那套不同（这里是 input_tokens / output_tokens），但**映射到
// 同一组计价项**：input → prompt，output → completion。折进 channel.Usage
// 的那一步在 ChannelUsage 里，只有一份实现。
type Usage struct {
	InputTokens  int64
	OutputTokens int64

	// Reported 表示上游的响应体里确实有 usage 块。
	//
	// 它与「两个数字是不是 0」是两件事：上游可以报一个全 0 的 usage，
	// 也可以整块不报；两种情形下上面两个字段一模一样。计费来源按这一位
	// 判，不按数字（见 gateway.usageSourceOf）。
	//
	// 零值是「没报」，也是漏填时落到的那一边——那只会让可对账的用量
	// 记成不可对账，金额不变，方向是保守的。
	Reported bool
}

// ChannelUsage 把这条表面的用量折进统一口径。
//
// 两个字段一一对应，没有需要拆开的东西：这条表面既没有缓存读写，
// 也没有推理 token。所以 CompletionIncludesReasoning 保持零值 false——
// 那是「推理单独报出」的意思，而这里推理量恒为 0，两种读法结果相同。
// 把它写成 true 会变成一句**没有依据**的声明（我们没有把推理算进 output
// 的证据），而那句话在别的口径下会让计费少收。
//
// Reported 原样带过去：它是「上游报没报」这件事，不该在这一层丢掉。
func (u Usage) ChannelUsage() channel.Usage {
	return channel.Usage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		Reported:         u.Reported,
	}
}

// Decider 是能执行一次决策调用的上游适配器。
//
// 声明在本包而不是 channel 里：channel.Adapter 是 chat 那套协议的契约，
// 而这条表面不属于它。要求实现它的适配器**同时**实现 channel.Adapter——
// 渠道表里那一行（channels.type）要先能被注册表认出来，请求才走得到这里。
//
// req.Model 是**上游模型名**：路由层按选中的渠道翻好了（见 internal/gateway
// 的 routeDecide），适配器不必也不该自己去查别名。
type Decider interface {
	Decide(ctx context.Context, ch channel.Channel, req *Request) (*Response, error)
}

// Invalid 表示请求本身不合法。
//
// 与「上游拒绝了」分开：这一类是**我们**在调上游之前判定的，回 400，
// 且 Field/Message 是给调用方看的（他改请求就能过）。上游拒绝的措辞不回给
// 调用方——那里面可能回显请求内容。
type Invalid struct {
	// Field 是出问题的那个字段的线格式名。
	Field string
	// Message 是给调用方看的一句话。
	Message string
}

func (e *Invalid) Error() string {
	if e.Field == "" {
		return e.Message
	}
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

// ErrStreamUnsupported 表示调用方要了流式，而这条表面从不流式。
//
// 单独一个错误类型是为了让入口把它与「请求写错了」分开措辞：
// 这一条不是调用方写错了什么，而是这条表面没有那个能力。
var ErrStreamUnsupported = &Invalid{
	Field:   "stream",
	Message: "this endpoint never streams: the upstream ignores stream and always answers with a single JSON body",
}

// Validate 检查这次请求里缺了就没法处理的部分。返回 nil 表示可以继续。
//
// 只检查「无论如何都做不成」的：没有模型名（选不了渠道）、没有问题
// （没有要判的东西）、问题类型不认识或该带的判据没带（上游一定拒）。
// state 空不空交给上游判断——那是一个我们猜不准的边界，按我们的猜测拒收
// 会把上游支持的请求挡在门外（与 internal/api 的 checkRequest 同一个取舍）。
func (r *Request) Validate() error {
	if r.Model == "" {
		return &Invalid{Field: "model", Message: "model is required"}
	}
	if len(r.Questions) == 0 {
		return &Invalid{Field: "questions", Message: "questions is required: at least one question"}
	}
	// 按 id 排序后再判：map 的遍历顺序不定，不排序的话同一个请求两次校验
	// 可能报出不同的字段，而调用方按报出来的那个字段去改。
	for _, id := range sortedIDs(r.Questions) {
		q := r.Questions[id]
		switch q.Type {
		case TypeNoul:
		case TypeChoice:
			if len(q.Choices) == 0 {
				return &Invalid{Field: "questions." + id + ".criteria",
					Message: "a choice question needs criteria: a mapping of option to description"}
			}
		case TypeScore:
			if len(q.Levels) == 0 {
				return &Invalid{Field: "questions." + id + ".criteria",
					Message: "a score question needs criteria: an array of level labels"}
			}
		case "":
			return &Invalid{Field: "questions." + id + ".type", Message: "type is required"}
		default:
			return &Invalid{Field: "questions." + id + ".type",
				Message: fmt.Sprintf("unknown question type %q", q.Type)}
		}
	}
	return nil
}

// Text 把这次请求的正文拼成一段文本，供 token 估算使用。
//
// 它**不是**上游会收到的内容：上游还会自己拼一段指令开销进去，所以按这段
// 文本估出来的数是一个**下界**（实测同一个 state 单独问一个问题时
// input_tokens 是 280、三个问题时是 387，差额不是 state 的字符数能解释的）。
//
// 拼的顺序固定（按问题 id 排序），于是同一个请求估出来的数每次都一样——
// 不排序的话，估算值会随 map 的遍历顺序变，而两个不同的估算值在日志里
// 看起来像两次不同的请求。
//
// 估算**不参与预留**：预留的输入侧取的是上游自己的预算上界（见
// MaxInputTokens）。这里给的是「按正文能数出来的那部分」。
//
// 目前**只有测试在调它**——生产里还没有哪一处按正文估算这条表面的用量。
// 留着它是因为它是一条库构件（要用就得先有个稳定的文本），不是因为漏接。
// 顺带一提：两道「有没有调用点」的门都照不到它（deadcode 的 RTA 把 runtime
// type 的导出方法整体当成可达，而按方法名判的那道门被 `Scanner.Text` 掩蔽），
// 所以它现在没有门看着——谁把它删掉、或者接上，都不需要动白名单。
func (r *Request) Text() string {
	var b strings.Builder
	b.WriteString(r.State)
	for _, id := range sortedIDs(r.Questions) {
		q := r.Questions[id]
		b.WriteString(id)
		b.WriteString(q.Instructions)
		for _, k := range sortedKeys(q.Choices) {
			b.WriteString(k)
			b.WriteString(q.Choices[k])
		}
		for _, level := range q.Levels {
			b.WriteString(level)
		}
	}
	return b.String()
}

// ── 线格式 ──────────────────────────────────────────────────────────────

// wireRequest 是请求的线格式。
type wireRequest struct {
	Model     string                  `json:"model"`
	State     string                  `json:"state"`
	Questions map[string]wireQuestion `json:"questions"`
	Stream    bool                    `json:"stream,omitempty"`
}

// wireQuestion 是一道问题的线格式。
//
// Criteria 用 json.RawMessage 原样接住：它在两种问题类型下是**不同的形状**
// （choice 是对象，score 是数组），而线格式必须与上游一致。按类型拆开的那一步
// 在 questionFromWire 里。
type wireQuestion struct {
	Type         string          `json:"type"`
	Instructions string          `json:"instructions"`
	Criteria     json.RawMessage `json:"criteria,omitempty"`
}

// wireResponse 是响应的线格式。
//
// Usage 是指针：解析时的「有没有这个键」是计费要用的一个事实（见
// Usage.Reported），而值字段会把它与「报了一个全 0」压成同一种形状。
type wireResponse struct {
	Model   string                `json:"model"`
	Answers map[string]wireAnswer `json:"answers"`
	Usage   *wireUsage            `json:"usage"`
}

// wireAnswer 是一道答案的线格式。
//
// 一份答案只带它那一组字段（noul 只有一个概率，choice 有选项与逐项概率，
// score 有得分与档位表），而**回给调用方的响应是把这里编回去的**——多带
// 字段的话，一道 noul 答案会带着 `"choice":"","legend":null` 一起发出去，
// 与上游的形状对不上。
//
// # 为什么标量是指针，而 map 仍是 omitempty
//
// 「这个类型没有这个字段」与「这个字段是零值」是**两件事**，omitempty 把它们
// 混成了一件，于是上游真的报过的零值被静默抹掉：score 的最低档恰好是 0、
// noul 可以是 0（「是」的概率为 0）、confidence 也可以是 0。抹掉之后 legend
// 还在、score 那个键却消失了，而调用方按上游形状写的严格解析器当场失败。
//
// 指针把「有没有这个键」显式带出来：nil 是「没有」，非 nil 是「有，哪怕是 0」。
// map 不必这样：一个空的 map 与「没有这个键」在形状上是一回事，omitempty
// 丢掉它不会丢信息。
//
// **哪些字段属于哪个类型由 answerToWire 按 Answer.Type 决定**：中间表示里
// 那三个标量是普通 float64，分不出「上游没给」与「上游给了 0」，所以这一步
// 只能靠类型——而类型恰恰是这条表面上唯一的判据。
type wireAnswer struct {
	Type          string             `json:"type"`
	Noul          *float64           `json:"noul,omitempty"`
	Choice        *string            `json:"choice,omitempty"`
	Confidence    *float64           `json:"confidence,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Score         *float64           `json:"score,omitempty"`
	Legend        map[string]string  `json:"legend,omitempty"`
}

// answerToWire 把一道答案编成线格式：**哪些键该出现由类型决定**。
//
// 三种已知类型各给一个确定的键集，零值照带。认不出的类型只带非零的那些：
// 那种情况下我们没有「哪些字段属于它」的判据，**不凭空造键**比少带一个更
// 要紧——凭空造出来的键是调用方按上游形状读不到的东西，而少带的那一个，
// 上游本来也没给过零值之外的证据。
func answerToWire(a Answer) wireAnswer {
	w := wireAnswer{Type: a.Type}
	switch a.Type {
	case TypeNoul:
		w.Noul = &a.Noul
	case TypeChoice:
		w.Choice = &a.Choice
		w.Confidence = &a.Confidence
		w.Probabilities = a.Probabilities
	case TypeScore:
		w.Score = &a.Score
		w.Confidence = &a.Confidence
		w.Legend = a.Legend
		w.Probabilities = a.Probabilities
	default:
		if a.Noul != 0 {
			w.Noul = &a.Noul
		}
		if a.Choice != "" {
			w.Choice = &a.Choice
		}
		if a.Confidence != 0 {
			w.Confidence = &a.Confidence
		}
		if a.Score != 0 {
			w.Score = &a.Score
		}
		w.Legend = a.Legend
		w.Probabilities = a.Probabilities
	}
	return w
}

// deref 取出指针指向的值，nil 给零值。
//
// 解析时用它把「上游有没有给这个键」折进中间表示——那一步之后这个信息就
// 没了（Answer 里是普通 float64），编码时由 answerToWire 按类型重新决定。
func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

type wireUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// Parse 把调用方发来的请求体解成一次决策请求。
//
// 它只做解码与类型拆分，**不做校验**：校验是 Validate 的事，两者分开是为了
// 让「body 不是合法 JSON」（400，细节不回给调用方）与「字段缺了」（400，
// 点名那个字段）能各自措辞。
func Parse(body []byte) (*Request, error) {
	var wire wireRequest
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("解析决策请求失败: %w", err)
	}

	out := &Request{
		Model:     wire.Model,
		State:     wire.State,
		Stream:    wire.Stream,
		Questions: make(map[string]Question, len(wire.Questions)),
	}
	for id, wq := range wire.Questions {
		q, err := questionFromWire(id, wq)
		if err != nil {
			return nil, err
		}
		out.Questions[id] = q
	}
	return out, nil
}

// questionFromWire 把线格式的一道问题拆成中间表示。
func questionFromWire(id string, wq wireQuestion) (Question, error) {
	q := Question{Type: wq.Type, Instructions: wq.Instructions}

	// criteria 的形状由 type 决定，而 type 不认识时**不猜**：猜错的表现是
	// 把一份选项表当成档位标签发出去，上游拒绝时说的是它那一侧的字段名。
	if len(wq.Criteria) == 0 {
		return q, nil
	}
	switch wq.Type {
	case TypeChoice:
		choices := map[string]string{}
		if err := json.Unmarshal(wq.Criteria, &choices); err != nil {
			return Question{}, &Invalid{
				Field:   "questions." + id + ".criteria",
				Message: "criteria of a choice question must be an object mapping option to description",
			}
		}
		q.Choices = choices
	case TypeScore:
		var levels []string
		if err := json.Unmarshal(wq.Criteria, &levels); err != nil {
			return Question{}, &Invalid{
				Field:   "questions." + id + ".criteria",
				Message: "criteria of a score question must be an array of level labels",
			}
		}
		q.Levels = levels
	}
	return q, nil
}

// Encode 把一次决策请求编码成上游认的 JSON。
//
// 编出来的是**请求本身**，不含任何本地字段：model 与 state 原样，
// questions 仍按 id 索引成对象（传数组会被上游拒），criteria 按类型还原成
// 对象或数组。
func Encode(req *Request) ([]byte, error) {
	if req == nil {
		return nil, errors.New("决策请求为 nil")
	}
	wire := wireRequest{
		Model:     req.Model,
		State:     req.State,
		Questions: make(map[string]wireQuestion, len(req.Questions)),
	}
	for id, q := range req.Questions {
		wq := wireQuestion{Type: q.Type, Instructions: q.Instructions}
		switch q.Type {
		case TypeChoice:
			if q.Choices != nil {
				raw, err := json.Marshal(q.Choices)
				if err != nil {
					return nil, fmt.Errorf("编码问题 %q 的判据失败: %w", id, err)
				}
				wq.Criteria = raw
			}
		case TypeScore:
			if q.Levels != nil {
				raw, err := json.Marshal(q.Levels)
				if err != nil {
					return nil, fmt.Errorf("编码问题 %q 的判据失败: %w", id, err)
				}
				wq.Criteria = raw
			}
		}
		wire.Questions[id] = wq
	}

	body, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("序列化决策请求失败: %w", err)
	}
	return body, nil
}

// ParseResponse 把上游的响应体解成一次决策响应。
//
// 不在这里判「答案是不是齐全」：少了一道问题的答案属于上游的行为，
// 该由调用方按自己的需要判断，而不是由我们替它拒掉一整份响应——
// 那份响应里其余的答案都是好的，且已经产生了费用。
func ParseResponse(body []byte) (*Response, error) {
	var wire wireResponse
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("解析决策响应失败: %w", err)
	}

	out := &Response{
		Model:   wire.Model,
		Answers: make(map[string]Answer, len(wire.Answers)),
	}
	if wire.Usage != nil {
		// 指针非 nil 就是「上游报了这个块」，哪怕两个数都是 0。
		out.Usage = Usage{
			InputTokens:  wire.Usage.InputTokens,
			OutputTokens: wire.Usage.OutputTokens,
			Reported:     true,
		}
	}
	for id, wa := range wire.Answers {
		out.Answers[id] = Answer{
			Type:          wa.Type,
			Noul:          deref(wa.Noul),
			Choice:        deref(wa.Choice),
			Confidence:    deref(wa.Confidence),
			Probabilities: wa.Probabilities,
			Score:         deref(wa.Score),
			Legend:        wa.Legend,
		}
	}
	return out, nil
}

// EncodeResponse 把一次决策响应编成回给调用方的 JSON。
//
// 它与 ParseResponse 共用同一份线格式定义（wireResponse），所以「上游发来的
// 形状」与「我们发出去的形状」不会各自漂移——两处各写一个结构体的话，
// 上游加一个字段而我们没跟上，表现是调用方看不到那个字段而没有任何东西报错。
//
// 编出来的是 `{model, answers, usage}`：这条表面**原样暴露**，不添本地字段。
// 报给调用方的 model 由调用方在传进来之前换好（见 internal/api 的
// renderDecision），本函数不碰它。
func EncodeResponse(resp *Response) ([]byte, error) {
	if resp == nil {
		return nil, errors.New("决策响应为 nil")
	}
	wire := wireResponse{
		Model:   resp.Model,
		Answers: make(map[string]wireAnswer, len(resp.Answers)),
		// 线格式与今天一致：usage 总是出现。Reported 是**内部计费**用的
		// 事实，不改变我们回给调用方的形状——把「上游没报」变成「我们回
		// 一个没有 usage 的响应」会让这条表面的对外契约跟着变，而那不在
		// 这一次改动里。
		Usage: &wireUsage{
			InputTokens:  resp.Usage.InputTokens,
			OutputTokens: resp.Usage.OutputTokens,
		},
	}
	for id, a := range resp.Answers {
		wire.Answers[id] = answerToWire(a)
	}

	body, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("序列化决策响应失败: %w", err)
	}
	return body, nil
}

// sortedIDs 按 id 排序返回问题 id，让遍历顺序固定。
func sortedIDs(questions map[string]Question) []string {
	out := make([]string, 0, len(questions))
	for id := range questions {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// sortedKeys 按 key 排序返回选项名。
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
