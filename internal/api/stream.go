package api

// 本文件是「项目 → 调用方」方向的**流式**序列化：把管道里的事件
// （channel.StreamEvent）渲染成调用协议认识的 SSE 帧。
//
// 与非流式那一份（response.go）分开写，因为两边几乎没有共同点：那边是一个
// JSON 体，这边是一串带事件名的帧，而且「先写头、再逐帧写、每帧 flush」这个
// 顺序本身就是契约的一部分。
//
// # 响应头一旦发出去就改不了了
//
// 状态码与响应头在**第一个事件之前**写出去（见 writeStreamHeaders）：之后再
// 想回 500 或 429，只能靠一个已经发出的 200 去解释。所以流开始之后的任何
// 失败都只能作为**流里的事件**表达，不能走 writeError。
//
// # 结束标记不能伪造
//
// 只有真的收到 done 事件才写 [DONE] / message_stop。上游中途断了、连接被
// 关掉，都不写——伪造一个正常结束会让「回答被截断」看起来像「模型答完了」，
// 而调用方会据此把半截结果当成完整结果用。**帧序列里没有结束标记，就是
// 截断的信号**，这与各协议适配器「不补 done 事件」的决定是同一件事。
//
// # 两处已知的简化
//
//   - OpenAI 的用量合并在结束分片里，而官方是在它之后另发一个 choices 为空的
//     分片。分开写会让按 choices[0] 取值的朴素客户端读到空数组而崩，合并
//     则是多一个它们不认识的字段——后者是更安全的偏差。
//   - Anthropic 的 message_start 里的 input_tokens 在**上游没在流的最前面报过
//     输入用量**时是本地的输入估算。上游报过就是真值：Anthropic 上游在自己的
//     message_start 里就给了输入用量，适配器把它发成一个 usage_input 事件，第一帧
//     因此等得到它。跨协议那条路（OpenAI 上游的用量在结束分片里）没有这个数，只能
//     填估算——**等下去会把整个流卡到结束**，那比填估算更坏。
//
//     这一处只在**枢纽**分支上存在。同协议直通转发的是上游原文，那一帧由上游写，
//     我们不重新序列化它——上游报 0 就转 0（见 docs/api.md 的「同协议直通」一节）。
//     message_delta 那一处两条路一致：没有实测值就不写这个键，不拿估算顶上（见
//     anthropicStream.finish 与 nativeStreamPassthrough.event）。

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/ledger/store"
	"github.com/sumwai/larkax/internal/route"
)

// sseContentType 是流式响应的 Content-Type。
//
// charset 不是协议要求的，但缺了它有些客户端会按 latin-1 解，而事件流里
// 常常有中文。
const sseContentType = "text/event-stream; charset=utf-8"

// sseContentTypePrefix 是不带 charset 的那一段。
//
// 重放那条路要按**留档的** Content-Type 判断第一次是不是流式（见 writeReplay），
// 而留档的值可能带别的 charset 写法，所以比的是前缀而不是整个常量。
const sseContentTypePrefix = "text/event-stream"

// streamHeaders 把流式响应那几个头写进 h。
//
// # 为什么单独抽出来
//
// 它有**两个**调用方：第一次请求（writeStreamHeaders）与重放（writeReplay）。
// 两处各写一遍的话，以后往这里加一个头就只在新请求上生效，重放悄悄少一个——
// 而重放少了 Cache-Control 恰恰是这几个头要防的事：一个按它行事的中间缓存
// 能把重放出去的 SSE 缓存住，而第一次请求特意防住了这一点。
func streamHeaders(h http.Header) {
	h.Set("Content-Type", sseContentType)
	// SSE 是给「一边生成一边看」用的，缓存住它就等于把它变成非流式。
	h.Set("Cache-Control", "no-cache")
	// 反向代理（nginx 等）默认会缓冲响应体。缓冲住 SSE 的后果与缓存相同，
	// 而它发生在链路中间——从我们的日志和指标上都看不出来。
	h.Set("X-Accel-Buffering", "no")
}

// writeStreamHeaders 写出流式响应的响应头。**必须在第一个事件之前调用**。
func writeStreamHeaders(w http.ResponseWriter) {
	streamHeaders(w.Header())
	w.WriteHeader(http.StatusOK)
}

// streamRenderer 把一个统一事件渲染成某个调用协议的 SSE 帧。
//
// 三件事分开：start 写开头的帧（OpenAI 是带 role 的分片，Anthropic 是
// message_start），event 写中间的事件，finish 写结束帧。**调用顺序由调用方
// 保证**，渲染器只负责形状。
//
// # 开头的帧可以推迟到第一个事件
//
// 调用顺序是「先 start、再逐个 event」，但这不等于第一帧必须在 start 里写出去：
// Anthropic 的 message_start 里要填上游报的输入用量，而那个数**可能就在第一个
// 事件里**（见 channel.StreamEvent 的 usage_input）。所以它的 start 什么都不写，
// 把第一帧留到 event 里——谁先到就按谁写。
//
// **推迟的实现必须在第一个 event 里把它写出去**：帧的顺序是契约的一部分，
// 留到 finish 就等于整条流都没有 message_start。
//
// 方法都不导出：接口与三个实现都是本包的内部结构，没有任何包外调用点。
// 导出它们只会让人以为别处可以按形状直接调，而顺序这一半契约在调用方那边。
//
// 方法收 io.Writer 而不是 http.ResponseWriter：渲染器不该知道 flush 的存在，
// 而「每帧之后 flush 一次」是调用方的事（见 serveStream）。
type streamRenderer interface {
	start(w io.Writer) error
	event(w io.Writer, ev channel.StreamEvent) error
	finish(w io.Writer) error

	// errorFrame 写一次流中途失败，**不碰「第一帧还没写」这个状态**。
	//
	// 它单独成一个方法而不是从 event 里绕过去，是为了直通那条路（见
	// nativeStreamPassthrough）：直通流里归一化事件一律不转发，而余额切断注入的
	// error 是唯一要写出去的合成帧。若借道 event，各协议的惰性首帧会先被
	// 触发，在错误帧前面多写一帧合成信封——那正是直通要禁的东西。
	errorFrame(w io.Writer, err error) error
}

// newStreamRenderer 按调用协议选一个渲染器。
//
// 认不出的协议返回错误而不是回落到其中一套：两套协议的帧形状不兼容，
// 回落会让调用方拿到一套它读不懂的帧，而失败现场离原因很远。
//
// 返回的总是原生直通包装器（见 nativeStreamPassthrough）：直通与否要按**流里收到
// 的事件**判，而选协议这一步是在第一个事件之前做的——只能先包上，等第一个
// 事件到了再定。
func newStreamRenderer(protocol channel.Protocol, id, model string, now time.Time, inputTokens int64) (streamRenderer, error) {
	var inner streamRenderer
	switch protocol {
	case channel.ProtocolOpenAI:
		inner = &openAIStream{id: id, model: model, created: now.Unix()}
	case channel.ProtocolAnthropic:
		inner = &anthropicStream{
			id:         id,
			model:      model,
			input:      inputTokens,
			textIndex:  -1,
			toolBlocks: map[int]int{},
		}
	case channel.ProtocolOpenAIResponses:
		inner = &responsesStream{
			id:        id,
			model:     model,
			created:   now.Unix(),
			textIndex: -1,
			tools:     map[int]*responsesStreamTool{},
		}
	default:
		return nil, fmt.Errorf("协议 %q 没有流式响应形状", protocol)
	}
	// inputTokens 只交给 anthropicStream：它在**枢纽**那条路上填 message_start
	// 的输入量（那里没有上游 early usage 可用）。直通那条路不补任何数——上游
	// 没报就什么都不写，理由见 event 里那块。
	return &nativeStreamPassthrough{inner: inner, protocol: protocol, id: id, model: model}, nil
}

// ── 同协议直通 ─────────────────────────────────────────────────────────

// nativeStreamPassthrough 包住协议渲染器，在见到匹配的 native 帧后切到直通。
//
// # 判据为什么是「见过匹配的 native 帧」
//
// 直通与否不能从别的地方看出来：
//
//   - 不能看**首条事件的形状**。上游常常先发一个 `ping`（或一个纯注释块），
//     它是首条、而它本来就是 native——按「首条是 delta 才算归一化流」去判，
//     这类流会被误判成枢纽流，于是客户端收到一份重新渲染的副本、上游那份
//     字节则被丢掉；
//   - 不能看 `res.Channel`。调用方是**先建渲染器、后拿到 res**（见 serveStream：
//     newStreamRenderer 在 ChatStream 之前），建的时候还不知道渠道是哪一个；
//   - 不能看事件里的 `Raw` 之类。那属于非流式结果，事件流里没有它。
//
// 逐帧判据是够用的，因为适配器保证 native 块先于由它解析出的归一化事件发出
// （见 channel.StreamEvent 的 Frame），而 `start` 事件不转发给渲染器（网关
// 内部消费）。所以一条直通流到达渲染器的第一条事件必然是 native：见到它就
// 已经能确定整条流的形状，不必等到结束。
//
// # 直通之后一个字都不自己合成
//
// 上游原文已经把内容送到客户端了，再按归一化事件写一份就是重复：同一个
// delta 会出现两遍。首帧、每一帧正文、收尾的 message_delta / [DONE]，全都
// 由上游那份字节负责。这里因此把归一化事件一律吞掉，只放余额切断过去
// （理由见 event）。
//
// # 为什么不能借 frame{} 写原生块
//
// `frame` 只会写 `event:` 与一行 `data:`，它服务的是枢纽分支——那条路上的
// 载荷都来自 json.Marshal，可以假定 data 里没有裸换行。原生块没有这个前提：
// 它可能带注释行（`:` 开头，keep-alive 就是这种）、带多条 `data:` 行、带
// 自己的 `event:` 行。拆开再按 frame 写会把块内结构改掉，拼出来的已不是上游
// 那一块。所以原生帧直接按字节写出，再补一个块分隔符。
type nativeStreamPassthrough struct {
	inner    streamRenderer
	protocol channel.Protocol
	// id 是回显给调用方的响应标识（本次请求的 request_id）。
	//
	// 它与 model 是同一类东西：**由网关决定、不取上游那份**。非流式直通
	// （api.go 的 renderNativePassthrough）与枢纽两条路都写它，直通流式不写
	// 就会让同一个响应对象在两条路上有相反的 id——调用方按响应区分不出
	// 「这次是谁的账单、该拿哪个 id 去问客服」（见 docs/api.md 的同协议直通
	// 一节）。上游在哪些位置报 id 见 rewriteBlockKeys。
	id string
	// model 是回显给调用方的模型名（他要的那个，见 modelNames）。
	model string
	// nativeSeen 表示这条流见过与本协议匹配的 native 帧，此后整条流走直通。
	nativeSeen bool
	// nextSeq 是下一条**我们造的帧**该用的 sequence_number（仅 Responses 用）。
	//
	// 它取「上游在各帧里报过的最大序号 + 1」：直通下客户端手里的序号是上游
	// 给的，我们后来补的那一帧（余额切断、上游错误）必须接着那个数往下排，
	// 否则客户端按序号排序时会把它当成乱序（见 docs/api.md 的「流中途的
	// 错误」一节：错误帧仍带 sequence_number）。
	nextSeq int64
	// upstreamError 表示刚刚拦下了一个上游错误块。随后到达的归一化 error
	// 事件据此补一帧我们自己的错误帧（见 event）。
	upstreamError bool
	// finishBlock 是攒着的结束分片（OpenAI 的 finish_reason 片）。
	//
	// 上游把用量发在它**之后**，而我们要合并成一片，所以先攒着——见
	// writeFinishBlock 与 flushFinishBlock。
	finishBlock []byte
}

// start 什么都不写。
//
// 首帧的写入交给了 inner 的惰性首帧（三个渲染器都在第一个事件时才写）：
// 此刻还不知道这条流是不是直通，写了就晚了——上游那份字节随后到达时，
// 客户端已经先收到一帧本地合成的信封。
func (p *nativeStreamPassthrough) start(io.Writer) error { return nil }

func (p *nativeStreamPassthrough) event(w io.Writer, ev channel.StreamEvent) error {
	if ev.Kind == "native" {
		if ev.NativeFormat != p.protocol {
			// 另一套协议的帧。跨协议那条路上适配器也会发 native 帧（它的
			// 来源协议与调用协议不同），而客户端读不懂它——忽略即可，归一化
			// 事件照旧走枢纽渲染。
			return nil
		}
		p.nativeSeen = true
		// 上游把用量发在结束分片**之后**（OpenAI 的顺序是：finish_reason
		// 分片 → choices 为空的用量分片 → [DONE]），而我们要把它们合并成
		// 一片。所以结束分片得先攒着，等看到用量（或这一块是别的）再写出去。
		// 见 docs/api.md 的「与上游契约的已知偏差」。
		//
		// 攒着的那一块在**这一块判完之后**才决定怎么处理：这一块是错误块就
		// 原样写出再拦下错误，是用量分片就并进去，别的块则先原样写出、再看
		// 这一块自己要不要攒。三条分支各自负责 flush，这里不预先写。
		if isUpstreamErrorBlock(p.protocol, ev.Frame) {
			// 上游的错误块**不转发**（见 docs/api.md 的「流中途的错误」一节：
			// `message` 里不回上游原文）。记下这一刻，由随后到达的归一化
			// error 事件走 inner 的 errorFrame 说我们自己的措辞。
			//
			// 判据落在**这里**而不是 error 事件上：适配器先发 native 帧、
			// 再发由它解析出的归一化事件（见 channel.StreamEvent 的 Frame），
			// 所以等 error 事件到了再判，那块字节已经写出去了。
			if err := p.flushFinishBlock(w); err != nil {
				return err
			}
			p.upstreamError = true
			return nil
		}
		block := ev.Frame
		if rewritten, ok := rewriteBlockKeys(p.protocol, block, p.id, p.model); ok {
			// 上游在这块里报了它自己的响应标识与模型名，而后我们承诺两者都由网关
			// 决定：`id` 是本次请求的关联标识（客户端与账单都按它对齐），`model`
			// 回显调用方写的名字（见 docs/api.md 的「入站别名」与「同协议直通」
			// 两节）。一处改写只动这两个键，其余键不动。
			block = rewritten
		}
		p.observeSequence(block)
		if usage, ok := p.absorbUsageOnlyBlock(block); ok {
			// 用量分片**不单独转发**：我们承诺把它合并进结束分片（见
			// docs/api.md 的「与上游契约的已知偏差」一节）。它上游是发在
			// 结束分片**之后**的，所以结束分片这会儿已经攒在 finishBlock 里，
			// 直接合进去写出去。
			//
			// 这不是直通的副作用，是我们刻意偏离上游的一处：分开写会让按
			// `choices[0]` 取值的朴素客户端读到空数组而崩。
			return p.writeFinishBlock(w, usage)
		}
		// 到这里的块既不是错误块也不是用量分片，所以攒着的结束分片（如果有）
		// 等不到用量了——原样写出去。
		if err := p.flushFinishBlock(w); err != nil {
			return err
		}
		if p.isFinishBlock(block) {
			// 结束分片先攒着：上游的用量紧跟在它后面。若下一个块不是用量
			// 分片（上游不报用量），上面那个 flush 会把它原样写出去。
			p.finishBlock = block
			return nil
		}
		// 这里**不补** message_delta 缺的 usage.input_tokens。
		//
		// 上游没在那一帧报输入量时，我们手上只剩本地估算，而本地估算与结算层
		// 的估算**不是同一个数**：两者刻意取了不同的包装常数（一个怕多收取下界，
		// 一个怕少冻取实测，见 internal/tokens 的 payload.go）。补进客户端那一份
		// 就成了一条只有调用方看得到的数——账本上仍然是 0，而两边都声称是实测。
		//
		// 不补的时候客户端读到什么：与枢纽那条路一致，那个键不存在（见
		// anthropicStream.finish 的同一条判据）。「上游报 0 就转 0」也是同一件事
		// 的另一面：原样转发，不替上游改数。
		return writeNativeBlock(w, block)
	}

	if !p.nativeSeen {
		return p.inner.event(w, ev)
	}

	// 直通流：归一化事件**照旧送给 inner**，但它的输出不落到 w。
	//
	// 「送给 inner」不是可省的：渲染器在 event 里推进内部状态（Anthropic 的
	// usage 累计、三者的惰性首帧标记）。不送的话我们自己造的帧（余额切断的
	// 错误帧）会拿一个没累计过的用量。
	//
	// 「不落到 w」是因为上游原文已经把同一份内容送出去了，再写一遍就是重复。
	//
	// # 为什么不靠 inner 的 seq 定序号
	//
	// Responses 的 sequence_number 在直通里**不能用 inner 的计数器**：那个
	// 计数器数的是归一化事件，而上游一条原生帧可能对应零个、一个或多个
	// 归一化事件（一个 delta 块里塞两行 data 就是多个），两者条数不对应，
	// 补出来的号会与客户端手里上游那几帧对不上。序号改由 observeSequence
	// 从**上游原文**读（见 event 里的 native 分支）。
	//
	// 两个例外，都是「我们承诺过、而上游那份字节没有兑现」：
	//
	//   - 余额切断：网关**自己注入**的错误事件（见 internal/gateway 的
	//     streamFreeze），上游那条流里没有对应的 native 帧，抑制它等于无因
	//     截断——客户端只会看到一条没有结束标记的流，分不出是余额用尽、上游
	//     断了还是网络问题。
	//   - 上游流内的错误：那块 native 字节**已被拦下没转发**（见上面
	//     isUpstreamErrorBlock），所以这里必须补一帧，否则客户端看不到任何
	//     终止信号。措辞由 errorFrame 给，不回上游原文。
	if ev.Kind == "error" {
		if errors.Is(ev.Err, store.ErrInsufficientAvailable) || p.upstreamError {
			// 攒着的结束分片先写出去。丢掉它的话，客户端拿到的是一条没有
			// finish_reason 的流——那正是「不要静默丢一块」要避免的。上游
			// 真按结束分片 → 用量分片的顺序发时，截断发生在那之间，这里是
			// 唯一能把结束分片交出去的地方。
			if err := p.flushFinishBlock(w); err != nil {
				return err
			}
			// 走 errorFrame 而不是 event：inner.event 会顺带写出惰性首帧，
			// 而在直通流里那帧是重复的。
			//
			// 先把 inner 的序号**对齐到上游那几帧之后**：直通下客户端手里的
			// 序号是上游给的（见 observeSequence），这一帧插在它们后面，序号
			// 就得接着排，否则客户端按序号排序会把它当成乱序。
			//
			// 是「对齐」不是「取较大值」：inner 的计数器数的是归一化事件
			// （上面那些 io.Discard 的调用把它推到了远大于上游帧数的地方），
			// 那个数在直通流里没有任何意义，拿它当序号会直接跳号。
			if s, ok := p.inner.(*responsesStream); ok {
				s.seq = p.nextSeq
			}
			return p.inner.errorFrame(w, ev.Err)
		}
	}
	// 推进 inner 的状态，输出丢弃。
	return p.inner.event(io.Discard, ev)
}

func (p *nativeStreamPassthrough) finish(w io.Writer) error {
	if p.nativeSeen {
		// 攒着的结束分片若还没写出去（上游没发用量分片，且它后面也没有别的
		// 块），在这里原样写出。不写的话调用方收不到 finish_reason。
		if err := p.flushFinishBlock(w); err != nil {
			return err
		}
		// 收尾串与终 chunk 都由上游原文负责。
		return nil
	}
	return p.inner.finish(w)
}

// errorFrame 是**唯一一个包装器不转发的方法**，而且它永远不会被调到：
// serveStream 只按 start / event / finish 驱动，错误帧由各渲染器在自己
// 的 event 里写（直通那条路见 p.event，它调 p.inner.errorFrame）。
//
// 所以这里**不转发**，而是炸掉。转发一个永远到不了的调用看着无害，实际
// 有害：接口上有一半实现者的这个方法被真正调用（三个 inner 都走它），
// 另一半（这里）不生效——下一个人照着接口实现新渲染器时，会以为它是必须
// 的、并且以为包装器的那个是活的。要留的是「它会炸」这个断言，不是一句
// 注释：注释只对读代码的人有效，而这里要防的正是读代码时被误导。
func (p *nativeStreamPassthrough) errorFrame(io.Writer, error) error {
	panic("nativeStreamPassthrough.errorFrame 不该被调用：错误帧由 inner.event 或 p.event 写")
}

// isUpstreamErrorBlock 判断一个原生块是不是上游的错误块。
//
// 这三条形状各自来自真实上游（不是照 dto 猜的）：
//
//   - OpenAI chat：`{"error":{"message":…,"type":…}}`，没有 `choices`；
//   - Anthropic：`event: error` 行，载荷 `{"type":"error","error":{…}}`；
//   - OpenAI Responses：`event: error` 行，载荷是**平铺**的
//     `{"type":"error","code":…,"message":…}`（没有 `error` 外层）。
//
// # 只认已知结构，认不出就当内容
//
// 与 internal/idempotency 的残差表同一个纪律：判据是**肯定**的形状，不是
// 否定。解析不了、没有 data 行、`type` 是别的值——一律返回 false，那块照旧
// 原样转发。认错的方向必须是「多转了一块内容」，而不是「把内容当错误吞掉」：
// 前者调用方看得见、能自己判断，后者是静默丢数据。
func isUpstreamErrorBlock(protocol channel.Protocol, frame []byte) bool {
	eventName, payloads := sseBlockParts(frame)
	if len(payloads) == 0 {
		return false
	}
	// 多行 data 的块按 SSE 规范用 \n 拼成一条载荷；错误块只会有一行，
	// 拼接在这里只是为了不把「恰好分了两行」的块判成认不出。
	payload := strings.Join(payloads, "\n")
	var m map[string]any
	if err := json.Unmarshal([]byte(payload), &m); err != nil {
		return false
	}
	if m == nil {
		return false
	}

	switch protocol {
	case channel.ProtocolOpenAI:
		_, hasError := m["error"]
		_, hasChoices := m["choices"]
		return hasError && !hasChoices
	case channel.ProtocolAnthropic:
		// 真上游两处都写：`event: error` 行与载荷里的 `type`。只要有一处
		// 说是错误就算——这句判据的方向是「认得出来就拦」，而靠得住的是
		// 载荷那个 type（见 anthropicadapter 的注释：event: 行可能被省略）。
		if t, _ := m["type"].(string); t == "error" {
			return true
		}
		return eventName == "error"
	case channel.ProtocolOpenAIResponses:
		if t, _ := m["type"].(string); t == "error" {
			return true
		}
		return eventName == "error"
	}
	return false
}

// sseBlockParts 拆出一个 SSE 块的事件名与全部 data 载荷。
//
// 与 frame{} 的分工：那个是**写**，这个是**读**，而且它只服务于「认块」这一
// 件事——不重建块、不重新序列化，所以块内结构（注释行、多行 data、event:
// 行）在这里只被读取。
func sseBlockParts(block []byte) (eventName string, payloads []string) {
	for _, raw := range strings.Split(string(block), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if v, ok := strings.CutPrefix(line, "event:"); ok {
			eventName = strings.TrimSpace(v)
			continue
		}
		if v, ok := strings.CutPrefix(line, "data:"); ok {
			payloads = append(payloads, strings.TrimSpace(v))
		}
	}
	return eventName, payloads
}

// rewriteBlockKeys 把一块原生帧里汇报的响应标识与模型名改成我们承诺的那两个。
//
// # 这两个键为什么同一条规矩
//
// 它们都不是「上游内容的副本」，而是**由网关决定的东西**：
//
//   - `id` 是本次请求的关联标识——客户端与账单都按它对齐（非流式直通
//     api.go 的 renderNativePassthrough 与枢纽那两条路都写我们的 request_id）；
//   - `model` 回显调用方写的名字（docs/api.md 的「入站别名」一节，明写流式与
//     非流式一样）。
//
// 直通若原样转发上游那两个值，同一个响应对象在两条路上就有相反的答案：非流式
// 给我们的 request_id、流式给上游的 id，而调用方无从分辨哪个才是它的。
//
// # 只改这两个键，且只在它们出现在对应那一层时
//
// 上游在每一块里都报这两样（OpenAI chat 五个分片每个都有 `id` 与 `model`）；
// Anthropic 在 `message_start` 的 `message` 对象里报；Responses 在
// `response` 对象里报。**只认那一层**：`content_block.id`（工具调用的
// `toolu_…`）、Responses 的 `item_id` 与 `output[].id` 是上游内容自己的标识，
// 客户端靠它们在同一块流里配对，改掉就是另一处破坏。
//
// 其余键一个不动：这是「逐字段等价、不逐字节相同」那条偏差的流式形态
// （非流式见 api.go 的 renderNativePassthrough）。重新序列化会让键序与上游
// 不同、大整数过一遍 float64——这与那边是同一个代价，不再重复解释。
//
// # 认不出就不改
//
// 返回 false 表示「这块里没有需要改的键」，调用方照原字节转发。判据与
// isUpstreamErrorBlock 同一个纪律：解析不了、没有 data 行、认不出那一层、
// 两个键的值都已经是我们要的——一律不动。认错的方向必须是「多转了一块原文」，
// 不是「把内容改坏」。
//
// 键**不存在**时也不补：非流式那边缺了会补（响应体必须有个 id），而流里每一块
// 各自独立，给没有 id 的块凭空加一个不在任何上游事件里出现过的东西，等于
// 伪造上游没说过的话。
func rewriteBlockKeys(protocol channel.Protocol, frame []byte, wantID, wantModel string) ([]byte, bool) {
	if wantID == "" && wantModel == "" {
		return frame, false
	}
	eventName, payloads := sseBlockParts(frame)
	if len(payloads) == 0 {
		return frame, false
	}
	payload := strings.Join(payloads, "\n")
	var m map[string]any
	if err := json.Unmarshal([]byte(payload), &m); err != nil || m == nil {
		return frame, false
	}

	// 这两样所在的那一层按协议不同，三套协议各有各的嵌套：
	//   - OpenAI chat：顶层；
	//   - Anthropic：`message.id` / `message.model`（message_start 里）；
	//   - Responses：`response.id` / `response.model`（流事件的 response 对象里）。
	// 认错一层就等于没改——那时上游真名与上游 id 照样泄漏出去，而用例可能因为
	// 夹具恰好同名而全绿（本文件的对应用例专门造了不同的值来盯这一点）。
	target := m
	switch protocol {
	case channel.ProtocolAnthropic:
		nested, ok := m["message"].(map[string]any)
		if !ok {
			return frame, false
		}
		target = nested
	case channel.ProtocolOpenAIResponses:
		nested, ok := m["response"].(map[string]any)
		if !ok {
			return frame, false
		}
		target = nested
	}

	changed := false
	if wantID != "" {
		if got, ok := target["id"].(string); ok && got != wantID {
			target["id"] = wantID
			changed = true
		}
	}
	if wantModel != "" {
		if got, ok := target["model"].(string); ok && got != wantModel {
			target["model"] = wantModel
			changed = true
		}
	}
	if !changed {
		return frame, false
	}

	raw, err := json.Marshal(m)
	if err != nil {
		return frame, false
	}
	// 重建这一块：保留原块的 event: 行（如果有），data 换成改过的载荷。
	// 其余行（注释行、`id:` 行）在正常上游的这两块里不会出现——真出现就走
	// 下面这条路重建，而它们本来也不是数据。
	var b strings.Builder
	if eventName != "" {
		b.WriteString("event: ")
		b.WriteString(eventName)
		b.WriteString("\n")
	}
	b.WriteString("data: ")
	b.Write(raw)
	return []byte(b.String()), true
}

// observeSequence 记住上游在这块里报过的 sequence_number，供我们造的帧接排。
//
// 只认 Responses：另外两套协议没有这个字段，也不会因此写什么。判据与别处
// 同一个纪律——解析不了、没有这个键、值不是数字，一律不动。
func (p *nativeStreamPassthrough) observeSequence(block []byte) {
	if p.protocol != channel.ProtocolOpenAIResponses {
		return
	}
	_, payloads := sseBlockParts(block)
	if len(payloads) == 0 {
		return
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.Join(payloads, "\n")), &m); err != nil || m == nil {
		return
	}
	v, ok := m["sequence_number"].(float64)
	if !ok {
		return
	}
	if seq := int64(v) + 1; seq > p.nextSeq {
		p.nextSeq = seq
	}
}

// absorbUsageOnlyBlock 取走 OpenAI 那个 choices 为空的用量分片。
//
// # 为什么它不转发
//
// docs/api.md 的「与上游契约的已知偏差」一节写着：**我们有意把用量合并进
// 结束分片**，因为「分开写会让按 `choices[0]` 取值的朴素客户端读到空数组
// 而崩」。这是对上游形状的**刻意偏离**，不是枢纽路径的实现细节——直通若把
// 上游那一块原样发出去，调用方就收到一个 `choices: []` 的分片，那是文档
// 点名要避免的。
//
// 所以拿到的 usage 由调用方立刻并进攒着的结束分片（见 writeFinishBlock），
// 这个分片本身不落地。
//
// # 判据：choices 是空数组且带 usage
//
// 两个条件都要成立。只判 `choices` 为空会误吞「上游发了个空 choices 别的
// 东西」的块；只判有 usage 会误吞正常分片（有些上游在每个分片里都带
// usage）。认不出就返回 false，那块照原样转发——认错的方向必须是「多转
// 一块」，不是「静默吞掉一块」。
func (p *nativeStreamPassthrough) absorbUsageOnlyBlock(block []byte) (map[string]any, bool) {
	if p.protocol != channel.ProtocolOpenAI {
		return nil, false
	}
	m, ok := jsonBlock(block)
	if !ok {
		return nil, false
	}
	choices, ok := m["choices"].([]any)
	if !ok || len(choices) != 0 {
		return nil, false
	}
	usage, ok := m["usage"].(map[string]any)
	if !ok || len(usage) == 0 {
		return nil, false
	}
	return usage, true
}

// isFinishBlock 判断一个原生块是不是 OpenAI 的结束分片。
//
// 判据：`choices` 非空、第一项的 `finish_reason` 非空字符串，且这块**没有**
// usage。最后一条不是判「是不是结束分片」，是判「要不要攒」：上游已经在
// 结束分片里带了用量，说明它不另发用量分片，没什么可合的，直接转发。
//
// 只对 OpenAI 生效。另外两套协议没有「用量单独一片」这个形状。
func (p *nativeStreamPassthrough) isFinishBlock(block []byte) bool {
	if p.protocol != channel.ProtocolOpenAI {
		return false
	}
	m, ok := jsonBlock(block)
	if !ok {
		return false
	}
	if _, exists := m["usage"]; exists {
		return false
	}
	choices, ok := m["choices"].([]any)
	if !ok || len(choices) == 0 {
		return false
	}
	first, ok := choices[0].(map[string]any)
	if !ok {
		return false
	}
	reason, _ := first["finish_reason"].(string)
	return reason != ""
}

// writeFinishBlock 把攒着的结束分片写出去，并（若给了 usage）把用量并进去。
//
// 合并是我们对上游形状的**刻意偏离**，不是直通的实现细节：docs/api.md 的
// 「与上游契约的已知偏差」一节写着分开写会让按 `choices[0]` 取值的朴素客户端
// 读到空数组而崩。所以用量分片本身不转发，它的 usage 在这里并进结束分片，
// 客户端看到的分片序列里不出现 `choices: []`。
//
// 合并点在这里而不是在收到用量分片的时候，是因为上游的顺序是
// finish_reason 分片 → 用量分片，合并只能发生在前者已经攒下之后。
func (p *nativeStreamPassthrough) writeFinishBlock(w io.Writer, usage map[string]any) error {
	block := p.finishBlock
	p.finishBlock = nil
	if block == nil {
		// 用量分片先于结束分片到（非常规顺序），或者上游压根没发结束分片。
		// 没有可合并的块，就没有理由替上游造一个——这一片的 usage 就此丢掉。
		// 丢掉一处用量，好过让调用方收到一个 `choices: []` 的分片（那是这条
		// 契约要避免的形状，见上）。
		return nil
	}
	if usage != nil {
		if merged, ok := mergeUsageIntoBlock(block, usage); ok {
			block = merged
		}
	}
	return writeNativeBlock(w, block)
}

// flushFinishBlock 把攒着的结束分片原样写出去。
//
// 用在两种「用量分片不会来了」的时机：新的一块到达、以及整条流结束
// （见 finish）。
func (p *nativeStreamPassthrough) flushFinishBlock(w io.Writer) error {
	block := p.finishBlock
	p.finishBlock = nil
	if block == nil {
		return nil
	}
	return writeNativeBlock(w, block)
}

// mergeUsageIntoBlock 把 usage 并进一个块，其余键不动。
//
// 块里已经有 usage 时不覆盖（那说明上游自己给了，替它改数就是另一个错误）。
// 解析不了、重建不出来时返回 false，调用方按原文写出——认不出的方向必须是
// 「多转一块」，不是「丢掉一块」。
func mergeUsageIntoBlock(block []byte, usage map[string]any) ([]byte, bool) {
	m, ok := jsonBlock(block)
	if !ok {
		return block, false
	}
	if _, exists := m["usage"]; exists {
		return block, false
	}
	m["usage"] = usage
	eventName, _ := sseBlockParts(block)
	return rebuildBlock(eventName, m)
}

// writeNativeBlock 写一个原生块：原字节 + 一个空行。
//
// Frame 是块内各行用 `\n` 连起来的字节，不带末尾换行，也不带那个起分隔作用
// 的空行（见 channel.StreamEvent 的 Frame），所以两处都补上。两处都要检查：
// 写失败意味着调用方已经不在了。
func writeNativeBlock(w io.Writer, block []byte) error {
	if _, err := w.Write(block); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n\n")
	return err
}

// jsonBlock 解析一个原生块的数据载荷成一个 JSON 对象。
//
// 与 rewriteBlockKeys 等处共用的读取方式，抽出来只为一件事：把「认块」这条
// 判据的解析写法收在一处，省得四份各写一遍而在边角上分岔。
func jsonBlock(block []byte) (map[string]any, bool) {
	_, payloads := sseBlockParts(block)
	if len(payloads) == 0 {
		return nil, false
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.Join(payloads, "\n")), &m); err != nil || m == nil {
		return nil, false
	}
	return m, true
}

// rebuildBlock 把改过的载荷写回一个 SSE 块。
//
// 保留原块的 `event:` 行（如果有）；数据换成重新序列化的那一份。注释行与
// id:/retry: 行不保留——它们在这几类要改的块里本就不出现，真出现了也说明
// 上游发了个非常规形状，那时重建出来的块仍然合法（SSE 允许没有注释行）。
func rebuildBlock(eventName string, m map[string]any) ([]byte, bool) {
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, false
	}
	var b strings.Builder
	if eventName != "" {
		b.WriteString("event: ")
		b.WriteString(eventName)
		b.WriteString("\n")
	}
	b.WriteString("data: ")
	b.Write(raw)
	return []byte(b.String()), true
}

// ── 帧 ─────────────────────────────────────────────────────────────────

// frame 是一帧 SSE。
type frame struct {
	// event 为空表示不写 event: 行。OpenAI 的帧全都没有事件名，
	// Anthropic 的每一帧都有。
	event string
	data  []byte
}

// write 把一帧写进 w。
//
// data 里不会有裸换行：它来自 json.Marshal，而 JSON 会把字符串里的换行
// 转义成 \n。这一点是必须的——SSE 按行切帧，data 里出现裸换行会把一帧
// 拆成两帧，客户端看到的是截断的 JSON。
func (f frame) write(w io.Writer) error {
	if f.event != "" {
		if _, err := fmt.Fprintf(w, "event: %s\n", f.event); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "data: %s\n\n", f.data)
	return err
}

// writeSSE 编码并写出一帧。
//
// 编码失败原样返回：这里的载荷都是字符串与数字，编不出来说明结构体里混进了
// 不能编码的东西——那该在改结构体的那一次被发现。静默跳过一帧会让客户端
// 收到一段缺了中间某片的内容，比截断难查得多。
func writeSSE(w io.Writer, event string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("编码 SSE 帧失败: %w", err)
	}
	return frame{event: event, data: raw}.write(w)
}

// streamErrorMessage 是一句**给调用方看的**流中途失败原因。
//
// 措辞按失败的性质分三种，判据都是错误值本身：
//
//   - 余额耗尽（store.ErrInsufficientAvailable）：这次流是**我们**按余额切断的
//     （见 internal/gateway 的 streamFreeze）。它要说清「回答被截断了」与「该去
//     充值」，见 msgInsufficientBalanceStream；
//   - 已经吐过字之后上游失败：判据是 route.ErrOutputStarted（由网关在吐字之后
//     失败时包上）。调用方手里有半截回答，他需要知道的是「这段被截断了」，而不是
//     「服务暂时不可用」——后者会让他直接重试，而重试会把两段回答接在一起；
//   - 其余：上游调用失败。
//
// 余额那一条排在 ErrOutputStarted 之前：两者说的是两件事，而余额那个更具体
// （它同时说清了原因与处置）。次序也符合「先看是不是我们自己断的」这个读法。
//
// 三套协议共用它：同一个失败在三种形状里说同一句话，形状的差别只在
// 字段名与嵌套上（见 streamErrorFrame 与 responsesStream.errorFrame）。
func streamErrorMessage(err error) string {
	if errors.Is(err, store.ErrInsufficientAvailable) {
		return msgInsufficientBalanceStream
	}
	if errors.Is(err, route.ErrOutputStarted) {
		return "the response was interrupted before it finished"
	}
	return "upstream call failed"
}

// streamErrorFrame 把一个流中途的错误渲染成调用协议的错误帧。
//
// 不回上游的错误原文：那里面可能回显请求内容（与非流式的 writeError 同一条
// 规矩）。细节留在日志里。
//
// Responses 那套**不走这里**：它的事件里带 sequence_number，而那个计数器在
// 渲染器手里（见 responsesStream.errorFrame）。
func streamErrorFrame(protocol channel.Protocol, err error) (frame, error) {
	message := streamErrorMessage(err)

	if protocol == channel.ProtocolAnthropic {
		raw, mErr := json.Marshal(claudeErrorBody{
			Type:  "error",
			Error: claudeError{Type: "api_error", Message: message},
		})
		return frame{event: "error", data: raw}, mErr
	}
	raw, mErr := json.Marshal(openAIErrorBody{
		Error: openAIError{Message: message, Type: "server_error"},
	})
	return frame{data: raw}, mErr
}

// ── OpenAI ─────────────────────────────────────────────────────────────

// openAIStream 渲染 OpenAI 的流式分片。
//
// 形状是「每片一个 choices[0].delta」，结束分片带 finish_reason，最后是
// 一行 `data: [DONE]`。
type openAIStream struct {
	id      string
	model   string
	created int64

	// started 表示首片已经写出去了。与 anthropicStream 同一个形状：首帧由
	// 第一个事件写出（见 start）。
	started bool

	usage        *channel.Usage
	finishReason string
}

type openAIStreamChunk struct {
	ID      string               `json:"id"`
	Object  string               `json:"object"`
	Created int64                `json:"created"`
	Model   string               `json:"model"`
	Choices []openAIStreamChoice `json:"choices"`
	// Usage 只在结束分片里出现，且只在知道用量时出现。
	Usage *openAIUsage `json:"usage,omitempty"`
}

type openAIStreamChoice struct {
	Index int               `json:"index"`
	Delta openAIStreamDelta `json:"delta"`
	// FinishReason 用指针：中途分片里它是 null，而 null 与空串在客户端
	// 那里是两件事（前者是「还没结束」，后者是「结束了但不知道原因」）。
	FinishReason *string `json:"finish_reason"`
}

type openAIStreamDelta struct {
	// Role 只在第一片里出现。
	Role string `json:"role,omitempty"`
	// Content 不加 omitempty 的反面：空的正文增量没有意义，省掉它让
	// 「这一片只是框架」在报文里一眼可见。
	Content   string                 `json:"content,omitempty"`
	ToolCalls []openAIStreamToolCall `json:"tool_calls,omitempty"`
}

type openAIStreamToolCall struct {
	Index int `json:"index"`
	// ID 与 Type 只在一次工具调用的第一个分片里出现，后续分片里没有。
	ID       string                   `json:"id,omitempty"`
	Type     string                   `json:"type,omitempty"`
	Function openAIStreamToolFunction `json:"function"`
}

type openAIStreamToolFunction struct {
	Name string `json:"name,omitempty"`
	// Arguments 始终写出（哪怕是空串）：它是**分片拼接**的载体，
	// 客户端靠「收到一个 arguments 片段」推进拼接，缺字段与空串
	// 在严格实现里不等价。
	Arguments string `json:"arguments"`
}

func (s *openAIStream) start(io.Writer) error {
	// 第一帧推迟到第一个事件（见 writeStart）。理由两条：
	//
	//   - 直通流的第一条事件是 native 帧，那时整条流根本不该有本地合成的
	//     信封（见 nativeStreamPassthrough）；
	//   - 零事件的流一条帧都不写。它是失败流（不计费、没有 done、按截断
	//     处置），写一帧 role 片会让客户端以为流正常开始了。
	//
	// 非直通流的输出与改动前逐字节相同：第一帧仍然在第一个事件到达时写出，
	// 只是从「start 时写」变成「同一个 event 调用里先写」。唯一的例外是
	// 第一个事件就是 error——那种流不再写这一帧（见 event 里的守卫）。
	return nil
}

// writeStart 写第一片（只带 role）。
//
// 官方的第一片是 {"role":"assistant","content":""}，空 content 被 omitempty
// 省掉，客户端行为不变。
func (s *openAIStream) writeStart(w io.Writer) error {
	return s.chunk(w, openAIStreamDelta{Role: "assistant"}, nil)
}

func (s *openAIStream) event(w io.Writer, ev channel.StreamEvent) error {
	if !s.started {
		s.started = true
		// error 不写首帧：上游接受请求后立刻失败时它就是第一个事件，
		// 先写一帧 role 片会让客户端以为回答正常开始了，而这条流一个字
		// 都没有。错误帧本身已经说明「这次调用失败了」。
		if ev.Kind != "error" {
			if err := s.writeStart(w); err != nil {
				return err
			}
		}
	}
	switch ev.Kind {
	case "delta":
		return s.chunk(w, openAIStreamDelta{Content: ev.Text}, nil)

	case "tool_use":
		if ev.ToolCall == nil {
			return nil
		}
		tc := openAIStreamToolCall{
			Index: ev.ToolCall.Index,
			ID:    ev.ToolCall.ID,
			Function: openAIStreamToolFunction{
				Name:      ev.ToolCall.Name,
				Arguments: ev.ToolCall.Arguments,
			},
		}
		// type 只在第一个分片里给：那时才有 id。后续分片只带
		// index 与 arguments 片段。
		if ev.ToolCall.ID != "" {
			tc.Type = "function"
		}
		return s.chunk(w, openAIStreamDelta{ToolCalls: []openAIStreamToolCall{tc}}, nil)

	case "annotation":
		// 引用标注。chat completions 的流式分片里**没有**它的位置：
		// 这套协议没有对应的官方事件，而 relaykit 的 dto 里那个
		// delta.annotations 是非官方扩展，我们不往对外的帧里放它
		//（那会是一次契约变更，见 docs/versioning.md）。
		//
		// 写成显式的空分支而不是让它落到 switch 外面：少这一支时行为完全
		// 相同（默认什么都不做），而读这段的人分不出「漏了」与「决定不转」。
		// Responses 入口那一份会把它渲染成 response.output_text.annotation.added
		//（见 responsesStream.emitAnnotations）。
		return nil

	case "usage":
		if ev.Usage != nil {
			u := *ev.Usage
			s.usage = &u
		}
		return nil

	case "done":
		// 结束分片由 Finish 写：它要带上用量，而用量在 done 之前的
		// 那一片里，这里只把它记下来。
		s.finishReason = ev.FinishReason
		return nil

	case "error":
		return s.errorFrame(w, ev.Err)
	}
	return nil
}

// errorFrame 写一次流中途失败。
func (s *openAIStream) errorFrame(w io.Writer, err error) error {
	f, err := streamErrorFrame(channel.ProtocolOpenAI, err)
	if err != nil {
		return err
	}
	return f.write(w)
}

func (s *openAIStream) finish(w io.Writer) error {
	var reason *string
	// 认不出来的结束原因原样透出，没有就写 null——不编一个 "stop"：
	// 把「上游没正常结束」说成「正常结束」，会让调用方按完成去处理一个
	// 半截结果（与非流式的 openAIFinishReason 同一条规矩）。
	if r := openAIFinishReason(s.finishReason); r != "" {
		reason = &r
	}
	if err := s.chunk(w, openAIStreamDelta{}, reason); err != nil {
		return err
	}
	return frame{data: []byte("[DONE]")}.write(w)
}

// chunk 写一个分片。
func (s *openAIStream) chunk(w io.Writer, delta openAIStreamDelta, reason *string) error {
	return writeSSE(w, "", openAIStreamChunk{
		ID:      s.id,
		Object:  "chat.completion.chunk",
		Created: s.created,
		Model:   s.model,
		Choices: []openAIStreamChoice{{Index: 0, Delta: delta, FinishReason: reason}},
		Usage:   s.usageJSON(),
	})
}

// usageJSON 把上游的用量转成 OpenAI 的口径。
//
// 与结束分片一起发出，见文件头的说明。
func (s *openAIStream) usageJSON() *openAIUsage {
	if s.usage == nil {
		return nil
	}
	return &openAIUsage{
		PromptTokens:     s.usage.PromptTokens,
		CompletionTokens: s.usage.CompletionTokens,
		TotalTokens:      s.usage.PromptTokens + s.usage.CompletionTokens,
	}
}

// ── Anthropic ──────────────────────────────────────────────────────────

// anthropicStream 渲染 Anthropic 的流式事件。
//
// 与 OpenAI 的差别不只是字段名：那边是「每片一个 delta」，这边是**有名字的
// 事件**，而且正文要包在 content_block 的开/关之间：
//
//	message_start → content_block_start → content_block_delta… →
//	content_block_stop → message_delta → message_stop
//
// 块的下标是**我们自己分配**的：上游给的下标属于它自己那条流，而这里要
// 发出的是一条新的流，两者对不上。正文块按下标递增分配，工具调用按上游的
// 分片下标映射到我们分配的块上（同一个工具调用的分片必须落到同一个块里，
// 否则客户端会看到好几个半截的工具调用）。
//
// # 正文块与工具块的开合规则不一样
//
// 正文没有下标：一个 delta 就是「当前这一段正文又长了」，所以正文块开着时
// 又来一段正文，就是同一块在增长。工具调用的分片则**带着上游给的下标，而且
// 可以交错**（OpenAI 允许多个 tool_calls[].index 交替出现，见
// internal/channel/openaiadapter）。两条规则因此必须分开：
//
//   - 打开工具块时关掉开着的正文块：那一段正文已经说完了，它的 stop 该在
//     工具块之前写出去，让它在最终 content 数组里占住自己的位置。
//   - 打开正文块时**不**关工具块：工具块一关，它后面那个分片就落在自己的
//     stop 之后了——那正是这个渲染器要防的事。
//
// 也就是说工具块一旦打开就开到流末尾，多个工具块可以同时开着。两个官方 SDK
// 都接得住这一点：它们的累加器都是「content_block_start 往 content 数组里
// 追加，content_block_delta 与 content_block_stop 按帧里的 index 取块」，
// 所以同时开着不影响拼出来的结果；而**重开同一个下标**会让最终消息里多出
// 一个同 id 的空 tool_use 块，客户端会拿它再调一次工具。协议文档也是按
// 「index 就是它在最终 content 数组里的位置」描述的，所以宁可让块同时开着，
// 也不能让一个下标出现两次。
//
// 顺带一提，「等流末尾再关」不只是取舍的结果，也是这里唯一能做的：统一事件
// 流里没有「块结束了」这件事（anthropicadapter 把上游的 content_block_stop
// 丢掉了，openaiadapter 那边 OpenAI 压根不发），所以渲染器根本收不到「这个
// 工具调用说完了」的信号。
type anthropicStream struct {
	id    string
	model string
	// input 是输入 token 的本地估算。它只在**上游没在流的最前面报过输入用量**
	// 时用来填 message_start（见 writeStart）——也就是跨协议那条路。
	input int64

	// started 表示 message_start 已经写出去了。第一帧由第一个事件写出，
	// 理由见 writeStart。
	started bool

	usage        *channel.Usage
	finishReason string

	// nextIndex 是下一个块的分配下标。
	nextIndex int
	// textIndex 是当前打开的正文块下标，-1 表示没有正文块开着。
	textIndex int
	// openToolBlocks 是当前开着的工具块下标，按打开顺序排列（关的时候也按
	// 这个顺序）。它只在流末尾清空：见上面「正文块与工具块的开合规则不一样」。
	openToolBlocks []int
	// toolBlocks 把上游的工具调用下标映射到本流里分配的块下标。
	toolBlocks map[int]int
}

type claudeStreamMessageStart struct {
	Type    string              `json:"type"`
	Message claudeStreamMessage `json:"message"`
}

type claudeStreamMessage struct {
	ID           string      `json:"id"`
	Type         string      `json:"type"`
	Role         string      `json:"role"`
	Model        string      `json:"model"`
	Content      []any       `json:"content"`
	StopReason   *string     `json:"stop_reason"`
	StopSequence *string     `json:"stop_sequence"`
	Usage        claudeUsage `json:"usage"`
}

type claudeBlockStart struct {
	Type         string `json:"type"`
	Index        int    `json:"index"`
	ContentBlock any    `json:"content_block"`
}

type claudeTextDelta struct {
	Type  string              `json:"type"`
	Index int                 `json:"index"`
	Delta claudeTextDeltaBody `json:"delta"`
}

type claudeTextDeltaBody struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type claudeJSONDelta struct {
	Type  string              `json:"type"`
	Index int                 `json:"index"`
	Delta claudeJSONDeltaBody `json:"delta"`
}

type claudeJSONDeltaBody struct {
	Type        string `json:"type"`
	PartialJSON string `json:"partial_json"`
}

type claudeBlockStop struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

type claudeMessageDelta struct {
	Type  string              `json:"type"`
	Delta claudeStopDeltaBody `json:"delta"`
	// Usage 只在知道用量时出现：没有实测值时不写，而不是写一个 0——
	// 0 会被客户端当成「这次真的没消耗」。
	Usage *claudeUsage `json:"usage,omitempty"`
}

type claudeStopDeltaBody struct {
	StopReason   *string `json:"stop_reason"`
	StopSequence *string `json:"stop_sequence"`
}

type claudeMessageStop struct {
	Type string `json:"type"`
}

// start 在这里**什么都不写**，第一帧由第一个事件写出去（见 writeStart）。
//
// 理由是时序：message_start 里的 input_tokens 要用上游报的真值，而那个数
// **可能就在第一个事件里**（usage_input——Anthropic 上游在自己的 message_start
// 里就给了输入用量），也可能永远不来（跨协议那条路：OpenAI 的用量在结束分片
// 里）。所以「写第一帧」只能发生在**看到第一个事件之后**：谁先到就按谁写。
//
// 调用方那边没有变化：它照旧先 start、再逐个 event，而 start 不写东西与写一帧
// 在它看来是同一件事（每帧之后 flush 一次）。
func (s *anthropicStream) start(io.Writer) error { return nil }

// writeStart 写 message_start，input_tokens 取第一个事件里能拿到的东西。
//
// 判据是**第一个事件是谁**：
//
//   - 带用量的事件（usage_input 是输入侧刚确定，usage 是完整用量）：上游在流
//     的最前面就报了输入用量，用真值；
//   - 其余（内容帧、结束标记、错误）：流里没有更早的输入用量可给，用本地估算。
//
// 最后一条是**必须**的，不是保守：等下去的话，跨协议那条路会把整个流卡到结束
// 才发第一帧——那比报一个偏低的估算更坏。
//
// 估算与真值都走 claudeUsageOf 这**同一个**换算入口（message_delta 用的也是
// 它）：自己拼一份的话，缓存那一份会只加在一边，两帧对同一个请求报出两个不同的
// input_tokens，而对账的人会去找一个不存在的差额。
func (s *anthropicStream) writeStart(w io.Writer, first channel.StreamEvent) error {
	u := channel.Usage{PromptTokens: s.input}
	if first.Usage != nil && (first.Kind == "usage_input" || first.Kind == "usage") {
		u = *first.Usage
	}
	msg := claudeStreamMessage{
		ID:    s.id,
		Type:  "message",
		Role:  "assistant",
		Model: s.model,
		// content 必须是数组，不能是 null。
		Content: []any{},
		Usage:   claudeUsageOf(u),
	}
	return writeSSE(w, "message_start", claudeStreamMessageStart{Type: "message_start", Message: msg})
}

func (s *anthropicStream) event(w io.Writer, ev channel.StreamEvent) error {
	if !s.started {
		s.started = true
		// error 不写首帧，理由见 openAIStream.event 同一个位置：上游接受
		// 请求后立刻失败时它就是第一个事件，先写 message_start 再报错会让
		// 客户端以为这条消息正常开始了，而它一个正文块都没有。
		if ev.Kind != "error" {
			if err := s.writeStart(w, ev); err != nil {
				return err
			}
		}
	}
	switch ev.Kind {
	case "delta":
		if err := s.openTextBlock(w); err != nil {
			return err
		}
		delta := claudeTextDelta{Type: "content_block_delta", Index: s.textIndex}
		delta.Delta.Type = "text_delta"
		delta.Delta.Text = ev.Text
		return writeSSE(w, "content_block_delta", delta)

	case "tool_use":
		if ev.ToolCall == nil {
			return nil
		}
		index, err := s.openToolBlock(w, ev.ToolCall)
		if err != nil {
			return err
		}
		if ev.ToolCall.Arguments == "" {
			return nil
		}
		delta := claudeJSONDelta{Type: "content_block_delta", Index: index}
		delta.Delta.Type = "input_json_delta"
		delta.Delta.PartialJSON = ev.ToolCall.Arguments
		return writeSSE(w, "content_block_delta", delta)

	case "usage_input":
		// 输入侧的用量。第一个事件就是它时，writeStart 已经用它填了
		// message_start；更晚才到时，那一帧早就写出去了，这里也补不回来。
		// 两种情况下这一层都没有别的可做——它不是结算依据，结算等 "usage"
		// （见 channel.StreamEvent 的 Usage）。
		return nil

	case "annotation":
		// 引用标注。Anthropic 的对应物是 content_block_delta 里的
		// citations_delta，而那条形状要挂在**哪一个正文块**上、以及
		// citation 的字段怎么映射，都是另一次契约变更（见 docs/versioning.md）。
		// 今天只把 Responses 入口那一条补上：它与非流式那条路是同一个协议，
		// 而 chat 与 Anthropic 两侧没有「非流式已经有、流式丢了」这个缺口。
		//
		// 写成显式的空分支而不是让它落到 switch 外面：见 openAIStream.event
		// 里同一个位置的说明。
		return nil

	case "usage":
		if ev.Usage != nil {
			u := *ev.Usage
			s.usage = &u
		}
		return nil

	case "done":
		s.finishReason = ev.FinishReason
		return nil

	case "error":
		return s.errorFrame(w, ev.Err)
	}
	return nil
}

// errorFrame 写一次流中途失败。
//
// 它**不**写 message_start：错误可能落在第一个事件上（上游接受请求后立刻
// 失败），而那时首帧还没出去。补一帧 message_start 再报错，客户端会以为
// 消息正常开始了——而这条流一个正文块都没有。
func (s *anthropicStream) errorFrame(w io.Writer, err error) error {
	f, err := streamErrorFrame(channel.ProtocolAnthropic, err)
	if err != nil {
		return err
	}
	return f.write(w)
}

func (s *anthropicStream) finish(w io.Writer) error {
	// 关的顺序不影响客户端（它按 index 找块），但按下标从小到大更顺眼：与
	// 最终 content 数组的顺序一致。同时开着的时候正文块的下标总是最大的
	// （打开工具块会先关掉正文块再分配自己的下标），所以先关工具块。
	if err := s.closeToolBlocks(w); err != nil {
		return err
	}
	if err := s.closeTextBlock(w); err != nil {
		return err
	}

	md := claudeMessageDelta{Type: "message_delta"}
	if r := claudeStopReason(s.finishReason); r != "" {
		md.Delta.StopReason = &r
	}
	if s.usage != nil {
		u := claudeUsageOf(*s.usage)
		md.Usage = &u
	}
	if err := writeSSE(w, "message_delta", md); err != nil {
		return err
	}
	return writeSSE(w, "message_stop", claudeMessageStop{Type: "message_stop"})
}

// openTextBlock 打开正文块，已经开着就什么都不做。
//
// 延迟打开而不是在 Start 里就开：一条只有工具调用的回答不该出现空的正文块，
// 而空正文块会让客户端渲染出一段空白。
//
// 每次打开都分配一个新下标，哪怕前面开过正文块：工具调用之后的正文是
// **另一块**，复用下标会让客户端把两段正文当成同一段在增长。
//
// 这里不关工具块：理由见结构体上「正文块与工具块的开合规则不一样」。
func (s *anthropicStream) openTextBlock(w io.Writer) error {
	if s.textIndex >= 0 {
		return nil
	}
	s.textIndex = s.allocate()
	return writeSSE(w, "content_block_start", claudeBlockStart{
		Type:         "content_block_start",
		Index:        s.textIndex,
		ContentBlock: claudeTextBlock{Type: "text", Text: ""},
	})
}

// openToolBlock 打开（或复用）一次工具调用对应的块，返回块下标。
func (s *anthropicStream) openToolBlock(w io.Writer, tc *channel.ToolCallDelta) (int, error) {
	if index, ok := s.toolBlocks[tc.Index]; ok {
		// 复用：这个工具调用的块已经开着。工具块只在流末尾关，所以它一定
		// 还开着——分片直接续在它后面，不必重开（重开会多出一个同 id 的
		// 空块，见结构体上的说明）。
		return index, nil
	}
	// 新的工具调用：它开在正文块之后，先把那一段正文关掉。
	if err := s.closeTextBlock(w); err != nil {
		return 0, err
	}
	index := s.allocate()
	s.toolBlocks[tc.Index] = index
	s.openToolBlocks = append(s.openToolBlocks, index)
	err := writeSSE(w, "content_block_start", claudeBlockStart{
		Type:  "content_block_start",
		Index: index,
		ContentBlock: claudeToolUseBlock{
			Type: "tool_use",
			ID:   tc.ID,
			Name: tc.Name,
			// input 是必填对象，参数要靠后面的 input_json_delta 拼出来。
			Input: map[string]any{},
		},
	})
	if err != nil {
		return 0, err
	}
	return index, nil
}

// closeTextBlock 关掉开着的正文块，没有则什么都不做。
func (s *anthropicStream) closeTextBlock(w io.Writer) error {
	if s.textIndex < 0 {
		return nil
	}
	index := s.textIndex
	s.textIndex = -1
	return writeBlockStop(w, index)
}

// closeToolBlocks 关掉所有开着的工具块，没有则什么都不做。
//
// 只在流末尾调用：中途关掉其中任何一个，它后面那些分片就落在自己的 stop
// 之后了——那正是这个渲染器要防的事。
func (s *anthropicStream) closeToolBlocks(w io.Writer) error {
	open := s.openToolBlocks
	s.openToolBlocks = nil
	for _, index := range open {
		if err := writeBlockStop(w, index); err != nil {
			return err
		}
	}
	return nil
}

// writeBlockStop 写一帧 content_block_stop。
func writeBlockStop(w io.Writer, index int) error {
	return writeSSE(w, "content_block_stop", claudeBlockStop{Type: "content_block_stop", Index: index})
}

// allocate 分配下一个块下标。
func (s *anthropicStream) allocate() int {
	index := s.nextIndex
	s.nextIndex++
	return index
}

// ── OpenAI Responses ───────────────────────────────────────────────────

// responsesStream 渲染 OpenAI Responses 的流式事件。
//
// 形状是「一串带名字的事件」，每一项输出都有开始 / 增量 / 结束三段：
//
//	response.created
//	response.output_item.added    → response.output_text.delta…
//	                              → response.output_text.done
//	                              → response.output_item.done
//	response.output_item.added    → response.function_call_arguments.delta…
//	                              → response.function_call_arguments.done
//	                              → response.output_item.done
//	response.completed（或 response.incomplete）
//
// 与非流式那一份（responses.go）共用同一批结构体：事件里嵌的那个 response
// 对象就是非流式的响应体，两处各写一份只会让它们慢慢分岔。
//
// # output_index 在**打开时**分配，而且只分配一次
//
// 它是这一项在最终 output[] 里的位置，客户端按它把增量拼回项里。所以正文项
// 在第一个正文增量到达时才分配（一个只有工具调用的回答不该出现空的正文项），
// 工具项在它第一次出现时才分配。
//
// # 工具项一旦打开就开到流末尾
//
// 中途关掉它，后面那些参数分片就落在自己的 done 之后了。这与 anthropicStream
// 的取舍相同，理由也相同：统一事件流里没有「这个工具调用说完了」这件事
// （anthropicadapter 把上游的 content_block_stop 丢掉了，openaiadapter 那边
// OpenAI 压根不发），所以渲染器根本收不到那个信号。
//
// # 正文项只有一项
//
// 与 anthropicStream 不同，这里不给工具调用之后的正文另开一项：正文项的
// 下标已经分配过，后续增量拼进同一项即可（客户端按 index 找项），而多开一项
// 会让最终 output[] 里出现两条助手消息。顺序因此是「正文项在最前、工具项
// 按出现顺序在后」，与 responsesOutput 的非流式那一份一致。
type responsesStream struct {
	id      string
	model   string
	created int64

	// started 表示 response.created 已经写出去了，首帧由第一个事件写出
	// （见 start）。
	started bool

	// seq 是每个事件的 sequence_number。官方 SDK 的事件模型把它标成必填，
	// 所以每个事件都要带上，而且从 0 起递增。
	seq int64

	// items 按分配顺序记下每一项。finish 时按这个顺序收尾，它就是最终
	// output[] 的顺序。
	items []responsesStreamItem

	// textIndex 是正文项的下标，-1 表示还没有正文。
	textIndex int
	text      strings.Builder

	// annotations 是这条流里已经转出去的引用标注，按下标顺序。
	//
	// 它是收尾事件里 output[].content[].annotations 的来源：增量事件与最终
	// 那一份必须同序同内容，否则客户端按 annotation_index 拼出来的结果与
	// 收尾时整份替换掉的结果对不上。与非流式那条路（responses.go 的
	// responsesOutput）是同一个口径。
	annotations []map[string]any

	// tools 把上游给的工具调用下标映射到本流里的一项。
	tools map[int]*responsesStreamTool

	usage        *channel.Usage
	finishReason string
}

// responsesStreamItem 是已分配的一项：tool 为 nil 表示这是正文项。
type responsesStreamItem struct {
	outputIndex int
	tool        *responsesStreamTool
}

// responsesStreamTool 是一次工具调用在本流里的状态。
type responsesStreamTool struct {
	outputIndex int
	itemID      string
	callID      string
	name        string
	arguments   strings.Builder
}

// responsesEvent 是一帧 Responses 事件的载荷。
//
// 用一个结构接住全部事件类型，而不是按 type 各声明一个：这里只会发其中几种，
// 而字段集高度重合（每一条都带 type 与 sequence_number）。不在这条事件里的
// 字段靠 omitempty 省掉——这与另外两套协议的做法不同（那边每帧一个结构体），
// 因为那边一帧里的字段少且互不重合。
type responsesEvent struct {
	Type           string             `json:"type"`
	SequenceNumber int64              `json:"sequence_number"`
	Response       *responsesResponse `json:"response,omitempty"`
	OutputIndex    *int               `json:"output_index,omitempty"`
	ContentIndex   *int               `json:"content_index,omitempty"`
	ItemID         string             `json:"item_id,omitempty"`
	Item           any                `json:"item,omitempty"`
	Delta          string             `json:"delta,omitempty"`
	// Text 与 Arguments 用指针：它们只在 done 事件里出现，而**空串是合法值**
	// （模型答了个空串、工具没有参数），省掉字段与给空串在严格实现里不等价。
	Text      *string `json:"text,omitempty"`
	Arguments *string `json:"arguments,omitempty"`
	// 引用标注（response.output_text.annotation.added）。Annotation 是标注
	// 本身，AnnotationIndex 是它在这一项的标注列表里的下标——客户端按它
	// 把标注拼回最终 output[] 的那一项（收尾事件里那份是本流的权威列表）。
	Annotation      map[string]any `json:"annotation,omitempty"`
	AnnotationIndex *int           `json:"annotation_index,omitempty"`
	// 错误事件（平铺，没有 error 外层，见 errorFrame）。
	Code    string  `json:"code,omitempty"`
	Message string  `json:"message,omitempty"`
	Param   *string `json:"param,omitempty"`
}

// start 什么都不写，response.created 由第一个事件写出去（见 writeStart）。
//
// 理由与 openAIStream.start 相同：直通流的第一条事件是 native 帧，那时这条
// 流不该有本地合成的信封；零事件的流也一帧都不写（它是失败流，一帧
// response.created 会让客户端以为流正常开始了）。
func (s *responsesStream) start(io.Writer) error { return nil }

// writeStart 写 response.created，它是非直通流的第一条事件。
//
// 与非流式那份的差别只有 status 与空的 output：这条流还没开始产出，
// 而客户端要拿这个 id 与 model 去建自己的状态。
func (s *responsesStream) writeStart(w io.Writer) error {
	return s.emit(w, "response.created", responsesEvent{
		Response: &responsesResponse{
			ID:         s.id,
			Object:     "response",
			CreatedAt:  s.created,
			Status:     "in_progress",
			Model:      s.model,
			Output:     []any{},
			ToolChoice: "auto",
			Tools:      []any{},
		},
	})
}

func (s *responsesStream) event(w io.Writer, ev channel.StreamEvent) error {
	if !s.started {
		s.started = true
		// error 不写首帧，理由见 openAIStream.event 同一个位置：上游接受
		// 请求后立刻失败时它就是第一个事件，先写 response.created 再报错会
		// 让客户端以为这次响应正常开始了。
		if ev.Kind != "error" {
			if err := s.writeStart(w); err != nil {
				return err
			}
		}
	}
	switch ev.Kind {
	case "delta":
		if err := s.openText(w); err != nil {
			return err
		}
		s.text.WriteString(ev.Text)
		index := s.textIndex
		return s.emit(w, "response.output_text.delta", responsesEvent{
			OutputIndex:  &index,
			ContentIndex: intPtr(0),
			ItemID:       responsesMessageItemID(s.id),
			Delta:        ev.Text,
		})

	case "annotation":
		return s.emitAnnotations(w, ev.Annotations)

	case "tool_use":
		if ev.ToolCall == nil {
			return nil
		}
		tool, err := s.openTool(w, ev.ToolCall)
		if err != nil {
			return err
		}
		if ev.ToolCall.Arguments == "" {
			return nil
		}
		tool.arguments.WriteString(ev.ToolCall.Arguments)
		index := tool.outputIndex
		return s.emit(w, "response.function_call_arguments.delta", responsesEvent{
			OutputIndex: &index,
			ItemID:      tool.itemID,
			Delta:       ev.ToolCall.Arguments,
		})

	case "usage_input":
		// 输入侧的用量。Responses 的用量只在终态事件里报（见 responsesadapter
		// 的说明），所以这里没有可写的地方——它不是结算依据。
		return nil

	case "usage":
		if ev.Usage != nil {
			u := *ev.Usage
			s.usage = &u
		}
		return nil

	case "done":
		// 终态事件由 finish 写：它要带上用量与最终的 output[]，而用量在
		// done 之前的那一片里。
		s.finishReason = ev.FinishReason
		return nil

	case "error":
		return s.errorFrame(w, ev.Err)
	}
	return nil
}

// errorFrame 写一次流中途失败。
//
// Responses 的流中途失败是一个**平铺**的事件——type / code / message / param
// 就在顶层，没有另外两套那样的 error 外层。这是第三套形状，不能与它们合并，
// 所以它没有走 streamErrorFrame。
func (s *responsesStream) errorFrame(w io.Writer, err error) error {
	return s.emit(w, "error", responsesEvent{
		Code:    "server_error",
		Message: streamErrorMessage(err),
	})
}

// emitAnnotations 把一批引用标注渲染成 response.output_text.annotation.added。
//
// 每一项一条事件，下标是它在本流标注列表里的位置（从 0 起、连续）。这个列表
// 也原样进收尾事件的 output[]——客户端按 collection 语义把增量拼成最终那一份，
// 两处必须是同一个顺序、同一批内容（与非流式的 output[].content[].annotations
// 也是同一批）。
//
// 存进列表的是**摊平之后**的那一条（见 responsesAnnotation）：增量帧与收尾事件里
// 的同一批标注必须是同一个形状，客户端把两处拼起来时才不会看到同一条标注有两种
// 读法。
//
// 标注到达时正文项可能还没打开（上游只保证「标注属于这一项」，不保证先有
// 哪怕一个正文增量）。这里先把项打开：事件里的 output_index / item_id 必须
// 指向一个已经存在、且与 item.added 里那个一致的项，客户端才能把它挂上去。
func (s *responsesStream) emitAnnotations(w io.Writer, annotations []map[string]any) error {
	if len(annotations) == 0 {
		return nil
	}
	if err := s.openText(w); err != nil {
		return err
	}
	index := s.textIndex
	for _, annotation := range annotations {
		if len(annotation) == 0 {
			continue
		}
		annotation = responsesAnnotation(annotation)
		annotationIndex := len(s.annotations)
		s.annotations = append(s.annotations, annotation)
		if err := s.emit(w, "response.output_text.annotation.added", responsesEvent{
			OutputIndex:     &index,
			ContentIndex:    intPtr(0),
			ItemID:          responsesMessageItemID(s.id),
			Annotation:      annotation,
			AnnotationIndex: intPtr(annotationIndex),
		}); err != nil {
			return err
		}
	}
	return nil
}

// openText 打开正文项，已经开着就什么都不做。
func (s *responsesStream) openText(w io.Writer) error {
	if s.textIndex >= 0 {
		return nil
	}
	s.textIndex = s.allocate(nil)
	index := s.textIndex
	return s.emit(w, "response.output_item.added", responsesEvent{
		OutputIndex: &index,
		ItemID:      responsesMessageItemID(s.id),
		Item: responsesMessageItem{
			Type:    "message",
			ID:      responsesMessageItemID(s.id),
			Status:  "in_progress",
			Role:    "assistant",
			Content: []responsesContent{},
		},
	})
}

// openTool 打开（或复用）一次工具调用对应的项。
//
// 名字取**第一次**出现的那一片：三家的适配器都把 id 与名字放在这个工具调用
// 的第一个分片里（OpenAI 的首片带 function.name、Anthropic 的
// content_block_start 带 name、Responses 的 output_item.added 带 name），
// 后面那些分片只有参数片段。
func (s *responsesStream) openTool(w io.Writer, tc *channel.ToolCallDelta) (*responsesStreamTool, error) {
	if tool, ok := s.tools[tc.Index]; ok {
		return tool, nil
	}
	tool := &responsesStreamTool{
		itemID: responsesCallItemID(s.id, tc.Index),
		callID: tc.ID,
		name:   tc.Name,
	}
	tool.outputIndex = s.allocate(tool)
	s.tools[tc.Index] = tool
	index := tool.outputIndex
	return tool, s.emit(w, "response.output_item.added", responsesEvent{
		OutputIndex: &index,
		ItemID:      tool.itemID,
		Item: responsesFunctionCallItem{
			Type:   "function_call",
			ID:     tool.itemID,
			Status: "in_progress",
			CallID: tool.callID,
			Name:   tool.name,
		},
	})
}

// finish 收尾：逐项写 done 事件，再写终态事件。
//
// 逐项的顺序按 output_index 升序——最终 output[] 的顺序就是它，而每项的
// done 事件要与它在数组里的位置一致。
func (s *responsesStream) finish(w io.Writer) error {
	status, incomplete := responsesStatus(s.finishReason)

	output := make([]any, 0, len(s.items))
	for _, item := range s.items {
		index := item.outputIndex
		if item.tool == nil {
			text := s.text.String()
			if err := s.emit(w, "response.output_text.done", responsesEvent{
				OutputIndex:  &index,
				ContentIndex: intPtr(0),
				ItemID:       responsesMessageItemID(s.id),
				Text:         &text,
			}); err != nil {
				return err
			}
			done := s.messageItem(status)
			output = append(output, done)
			if err := s.emit(w, "response.output_item.done", responsesEvent{
				OutputIndex: &index,
				ItemID:      done.ID,
				Item:        done,
			}); err != nil {
				return err
			}
			continue
		}

		arguments := item.tool.arguments.String()
		if err := s.emit(w, "response.function_call_arguments.done", responsesEvent{
			OutputIndex: &index,
			ItemID:      item.tool.itemID,
			Arguments:   &arguments,
		}); err != nil {
			return err
		}
		done := responsesFunctionCallItem{
			Type:      "function_call",
			ID:        item.tool.itemID,
			Status:    status,
			CallID:    item.tool.callID,
			Name:      item.tool.name,
			Arguments: arguments,
		}
		output = append(output, done)
		if err := s.emit(w, "response.output_item.done", responsesEvent{
			OutputIndex: &index,
			ItemID:      done.ID,
			Item:        done,
		}); err != nil {
			return err
		}
	}

	resp := &responsesResponse{
		ID:                s.id,
		Object:            "response",
		CreatedAt:         s.created,
		Status:            status,
		Model:             s.model,
		Output:            output,
		IncompleteDetails: incomplete,
		ToolChoice:        "auto",
		Tools:             []any{},
	}
	if s.usage != nil {
		resp.Usage = responsesUsageOf(*s.usage)
	}
	return s.emit(w, responsesFinishEvent(status), responsesEvent{Response: resp})
}

// allocate 分配下一个输出项下标并把它记进顺序表。
func (s *responsesStream) allocate(tool *responsesStreamTool) int {
	index := len(s.items)
	s.items = append(s.items, responsesStreamItem{outputIndex: index, tool: tool})
	return index
}

// messageItem 是收尾时写出的那个完整的正文项。
func (s *responsesStream) messageItem(status string) responsesMessageItem {
	return responsesMessageItem{
		Type:   "message",
		ID:     responsesMessageItemID(s.id),
		Status: status,
		Role:   "assistant",
		Content: []responsesContent{{
			Type:        "output_text",
			Text:        s.text.String(),
			Annotations: annotationList(s.annotations),
		}},
	}
}

// emit 写一帧事件，sequence_number 由这里递增。
//
// 计数器放在一处而不是每个调用点各给一个数：漏掉一个或重复一个都会让客户端的
// 顺序判据失效，而那种失效在报文里看不出来。
func (s *responsesStream) emit(w io.Writer, eventType string, payload responsesEvent) error {
	payload.Type = eventType
	payload.SequenceNumber = s.seq
	s.seq++
	return writeSSE(w, eventType, payload)
}

// intPtr 取一个 int 的地址。事件的字段用指针区分「不在这条事件里」与「值是 0」。
func intPtr(v int) *int { return &v }
