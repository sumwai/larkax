// Package tokens 负责 token 计数与估算——只计数，不算钱。
//
// 分层：
//
//	tokens   「这段文本有多少 token」
//	billing  「这些 token 值多少钱」
//
// 分开是因为两者的可信度不同：计数有精确与估算之分，而计费必须对两者
// 用同一套价格表。混在一起的话，「这个数是估的」这条信息会在进入计费之前丢掉，
// 于是账上分不出「估的」和「实测的」。
//
// # 两类调用者
//
//   - 请求发出前：需要知道输入大概多少 token（预留上界、限流）。
//     此时还没有上游的 usage，只能本地算。
//   - 流式中断后：客户端断开，上游的 usage 永远等不到，只能本地估。
//     这是本包存在的主要理由，见 stream.go。
//
// # 为什么估算值必须带来源
//
// 上游的 usage 只在流式响应的最后一个分片里返回。客户端中途断开时，
// 那一片永远不会到达，而费用已经产生。所以只能本地估——但估算值与实测值
// 在账上必须有区别：实测值可以拿去跟上游对账，估算值不能。
// 这一点由 Source 在**类型层面**固定，而不是靠文档约定：
// 计数结果是一个结构体，来源字段与数字绑在一起。
//
// # 保守方向
//
// 估算一律**偏高**。理由：估少了等于少收钱，且这个差额永远不会被发现——
// 没有人会因为账单变少了来投诉；估多了用户会来问，而一次人工核对就能退回。
// 代价是账面金额略高于真实成本，且这个偏差是已知、可观测、可解释的
// （每条用量都带 Source，误差范围见 ErrorBounds 与 COUNTER.md）。
package tokens

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Source 说明一个 token 数是怎么来的。
//
// 它是本包的核心类型：调用方拿到计数结果时必须同时拿到它，
// 否则「估的」与「实测的」在账上就没有区别了。
type Source uint8

const (
	// SourceMeasured 是上游上报的实测值。
	//
	// 只有来自上游 usage 字段（或 count_tokens 端点）的数字才用这个值。
	// 它是唯一可以与上游对账的来源。
	SourceMeasured Source = iota
	// SourceTokenized 是用词表本地精确计数。
	//
	// 它比启发式准得多，但仍是**本地**值：与上游实测相比，误差主要来自
	// 消息框架（role 标记、分隔符）与上游私有的特殊 token——
	// 那些本地词表里没有，算不出来。
	SourceTokenized
	// SourceEstimated 是启发式估算。
	//
	// 误差范围见 ErrorBounds。用它算出的金额只能作为参考，
	// 界面上必须如实标注。
	SourceEstimated
	// SourceUnreported 表示上游**根本没有报**这次的用量。
	//
	// 它与 SourceMeasured 的区别是「上游响应里有没有 usage 块」，不是数字：
	// 上游可以报一个全 0 的 usage（SourceMeasured），也可以整块不报
	// （SourceUnreported）。后者的数字是零值占位，不是上游说的 0 ——
	// 两者逐列相同，只有来源能把它们分开。
	//
	// 它也不是估算：没有任何一层为这次请求算过数。落库时它与估算同归
	// 「不能拿去跟上游对账」那一档，由 estimator 说明具体是哪一种
	// （见 internal/ledger/store 的 billingPathOf）。
	SourceUnreported
)

// String 返回稳定的英文标识，用于日志、数据库与 API 输出。
func (s Source) String() string {
	switch s {
	case SourceMeasured:
		return "measured"
	case SourceTokenized:
		return "tokenized"
	case SourceEstimated:
		return "estimated"
	case SourceUnreported:
		return "unreported"
	default:
		return fmt.Sprintf("Source(%d)", uint8(s))
	}
}

// IsExact 表示该来源是否可以直接与上游对账。
//
// 只有实测值可以。本地计数值不能——它与上游的差异是系统性的
// （框架 token、特殊 token），对账时会表现为「永远差一点」，
// 而那个差值没有任何业务含义。
func (s Source) IsExact() bool { return s == SourceMeasured }

// Count 是一次计数的结果。
//
// 用结构体而不是裸 int：来源必须跟着数字一起走。调用方若只想拿数字
// 也得显式写出 .Tokens，这个动作本身就是一次提醒。
type Count struct {
	// Tokens 是 token 数。非负。
	Tokens int64
	// Source 说明它是怎么来的。
	Source Source
	// Model 是计数时使用的模型名，让「这个数是用哪套规则算的」可追溯。
	Model string
}

// String 便于日志与调试输出。
func (c Count) String() string {
	return fmt.Sprintf("%d token (%s, %s)", c.Tokens, c.Source, c.Model)
}

// Add 把另一个计数并入本计数，取两者中**较不可信**的来源。
//
// 来源的合并方向是有意的：一个精确值与一个估算值相加，结果只能是估算值。
// 若反过来取精确值，误差就会被隐藏起来。
func (c Count) Add(o Count) Count {
	src := c.Source
	if o.Source > src {
		src = o.Source
	}
	model := c.Model
	if model == "" {
		model = o.Model
	}
	return Count{Tokens: c.Tokens + o.Tokens, Source: src, Model: model}
}

// ── 误差范围 ───────────────────────────────────────────────────────

// ErrorBounds 描述估算值与真实值的比例落点。
//
// 两个边界都来自**实测标定**（方法、语料与原始分布见 COUNTER.md），
// 不是估计值。用比例而不是百分比是因为估算式本身就是线性的：
// 一段文本长一倍，误差的绝对量也长一倍，而比例区间保持稳定。
type ErrorBounds struct {
	// MinRatio 是实测到的最小「估算值 / 真实值」。
	// 1 - MinRatio 就是最坏低估了多少。
	MinRatio float64
	// MaxRatio 是实测到的最大「估算值 / 真实值」。
	MaxRatio float64
	// MedianRatio 是中位数，代表「典型情况下偏多少」。
	// 它比最坏值更能说明这套估算在日常流量上的表现。
	MedianRatio float64
	// Basis 说明这组数字的来源：实测、推理，还是两者混合。
	// 一行字符串就够，但它是必要的——使用方要能判断这组数字值多少信任。
	Basis string
}

var (
	// OpenAIBounds 是 OpenAI 族走估算路径的误差。
	//
	// 标定语料：1500 条片段随机拼接（1~40 段，覆盖英文/中文/编码/数字/
	// 标点/emoji/空白七类），用 o200k_base 的真实分词结果当基准。
	// 见 COUNTER.md「标定方法」。
	OpenAIBounds = ErrorBounds{
		MinRatio:    0.70,
		MaxRatio:    2.7,
		MedianRatio: 1.38,
		Basis:       "实测：o200k_base 词表，1500 条合成语料 + 手写样本",
	}
	// ClaudeBounds 是 Claude 族走估算路径的误差。
	//
	// **这组数字不是实测。** Anthropic 不公开词表，本包无法得到任何
	// 可用于标定的真值，所以只能推理：Claude 4.7+ 换了新 tokenizer，
	// 同文本的 token 数约为旧词表的 1.3 倍，而旧词表也不公开。
	// 这里的做法是拿 OpenAI 族的实测比例区间乘 1.3 再留余量。
	// 使用方应当把这一列当作「量级正确、具体数字不可依赖」。
	ClaudeBounds = ErrorBounds{
		MinRatio:    0.70,
		MaxRatio:    3.5,
		MedianRatio: 1.8,
		Basis:       "推理：按 Anthropic 公布的 ≈1.3× 关系放大 OpenAI 族实测区间，未实测",
	}
	// UnknownModelBounds 是未登记族的兜底，取各已知族中最宽的区间。
	UnknownModelBounds = ErrorBounds{
		MinRatio:    0.70,
		MaxRatio:    3.5,
		MedianRatio: 1.8,
		Basis:       "推理：取已知族中最宽的区间",
	}
)

// ShortInputNote 说明短输入上的额外注意点。
//
// 它是一段文字而不是数字，因为短输入的误差**不适合用比例描述**：
// 12 个字符的输入估成 16 个 token，比例上是 1.33 倍，绝对上只差 4 个 token。
// 把「最多 2.7 倍」这句话套到一行 `ok` 上会吓到人，而实际上那种输入
// 的金额可以忽略。真正需要小心的是长输入，那里的比例区间是可靠的。
const ShortInputNote = "短输入（不足约 16 字符）的比例误差比 ErrorBounds 更宽" +
	"（实测低至 0.35 倍），但绝对误差只有个位数 token；比例区间对长输入才可靠。"

// BoundsFor 返回某个族走估算路径时的误差范围。
//
// 目前只有测试在调它：调用方要拿它做「这次估算有多不可信」的判断，
// 而那条路（实测 usage 的归一）还没有接上。
func BoundsFor(f Family) ErrorBounds {
	switch f {
	case FamilyOpenAI:
		return OpenAIBounds
	case FamilyClaude:
		return ClaudeBounds
	default:
		// 有开源词表但本包尚未接入的族（DeepSeek / Qwen / Kimi / GLM）
		// 也落到这里：它们的字素分布与 OpenAI 族差别不小（Qwen 的中文尤甚），
		// 拿 OpenAI 的区间去套是错的，而实测又没有做，所以用最宽的兜底。
		return UnknownModelBounds
	}
}

// ── 计数器 ─────────────────────────────────────────────────────────

// Counter 按模型计数 token。
//
// 零值不可用，用 NewCounter 构造。并发安全：构造之后内部状态只读，
// 词表的加载由互斥锁保护。
type Counter struct {
	vocab *vocabSet
}

// NewCounter 构造计数器。
//
// **不加载词表。** 词表约 16MB，编译进二进制；首次解出一种实测约 25ms、
// 堆上多约 7MB（o200k；cl100k 约 9ms 与 3.4MB，两者都随编码数增加）。
// 服务进程里并非每条路径都需要精确计数（比如控制面只做展示、
// `/v1/models` 根本不计数），对它们纯属浪费。
//
// 「不加载」的意思是**不用**，不是「不占用」：词表是 Go 的字符串表初始化，
// 以符号表的形式躺在二进制文件里（o200k 约 7.9MB、cl100k 约 3.9MB 的源码
// 常量表），进程启动时这部分是页缓存而不是堆；真正占堆的 map 是
// tokenizer.Get 时惰性构建的。所以「启动就解 16MB」这件事并不会发生，
// 但**二进制体积**会固定增加约 16MB。
//
// 需要精确计数时调用 Prepare；不调用也不会错，只是第一次走到精确路径时
// 会惰性加载，那一次请求多约 25ms。
func NewCounter() *Counter { return &Counter{vocab: newVocabSet()} }

// Prepare 预先加载词表，并返回每种编码的加载耗时供启动日志使用。
//
// 服务启动时调用一次（每个进程一次就够）。这是**唯一**能让词表加载不落在
// 用户请求上的时机：生产侧由 cmd/larkax 在**监听端口之前**调用
// （见那里的 warmVocab）——那之前的任何时刻都还没有请求，之后的第一个
// 请求就得自己付这笔钱。
//
// 失败**不是**致命的：不预热只是慢，不是不能用（codecFor 会惰性加载），
// 所以调用方应当记一条日志后继续，而不是让进程起不来。
//
// ctx 给调用方一个放弃点。加载本身是纯 CPU 计算，不可中断，但等待它的
// 启动探针可以超时退出，所以这里尊重取消。
func (c *Counter) Prepare(ctx context.Context) (map[string]time.Duration, error) {
	if c == nil || c.vocab == nil {
		return nil, errors.New("Counter 未初始化，请用 NewCounter 构造")
	}
	return c.vocab.prepare(ctx)
}

// Loaded 返回已经加载的编码名，供启动日志使用。
//
// 它与 Prepare 的返回值是同一个问题的两面：那一次启动把什么解进了内存、
// 各花了多久。cmd/larkax 的启动日志两个都打（见 warmVocab）——只打
// 「预热完成」的话，「一种都没解开」与「都解开了」在日志里长得一样。
func (c *Counter) Loaded() []string {
	if c == nil || c.vocab == nil {
		return nil
	}
	return c.vocab.loaded()
}

// AvailableEncodings 返回本机能够精确计数的编码（未必已加载）。
//
// 与 Loaded 的区别：这个是能力上限，那个是当前状态。要回答「这台机器支持到
// 什么程度」用前者，要回答「那一次启动解开了哪些、各花了多久」用后者。
//
// **目前只有测试在调它。** 同一组的另外两个已经接上了（cmd/larkax 的
// warmVocab 调 Prepare 与 Loaded），而它还没有——内部端口的就绪检查今天
// 只报 postgres 的连通性（见 cmd/larkax 的 newInternalHandler）。接线之前
// 别把它当成已经在跑的东西。
func AvailableEncodings() []string {
	encodings := loadableEncodings()
	out := make([]string, 0, len(encodings))
	for _, enc := range encodings {
		out = append(out, string(enc))
	}
	return out
}
