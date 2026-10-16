package tokens

// 本文件把各家上游上报的 usage 归一成 internal/channel 的口径。
//
// # 为什么需要一个归一函数
//
// 三家的缓存口径**互不相容**，而写错的后果是重复计费或漏计：
//
//	OpenAI     prompt_tokens 含缓存，prompt_tokens_details.cached_tokens
//	           是它的**子集**。每 token 只按一档计价。
//	Anthropic  input_tokens **不含**缓存，总量 = input + cache_creation
//	           + cache_read。把缓存加进 input 才与 OpenAI 同义。
//	DeepSeek   prompt_cache_hit_tokens / prompt_cache_miss_tokens **分列**，
//	           两者相加才是输入总量。
//
// 三者的差别不是「字段名不同」而是「同一个数字代表的东西不同」。
// 因此这里不是做字段改名，而是做**语义换算**，并显式写清每一步在换什么。
//
// # 输出形状
//
// 输出是 channel.Usage（那已经是本项目的统一口径：PromptTokens 含缓存）。
// 本包不定义第二套结构：多一个中间结构就多一处可能忘记同步的口径，
// 而口径不同步不会报错，只会算错钱。

import (
	"errors"
	"fmt"

	"github.com/sumwai/larkax/internal/channel"
)

// Upstream 标识一次 usage 来自哪家上游。
//
// 用它而不是上游的模型名：同一家可能同时提供两套协议（例如某网关既能
// 说 OpenAI 也能说 Anthropic），此时决定口径的是**协议**而不是厂商。
// 沿用 channel.Protocol 会引入一个本包本不该有的依赖方向
// （tokens 只该依赖 channel 的类型，不该依赖适配器的概念），所以另立一个。
type Upstream uint8

const (
	// UpstreamOpenAI 是 OpenAI 口径：cached_tokens 是 prompt_tokens 的子集。
	UpstreamOpenAI Upstream = iota
	// UpstreamAnthropic 是 Anthropic 口径：input_tokens 不含缓存，分列。
	UpstreamAnthropic
	// UpstreamDeepSeek 是 DeepSeek 口径：命中与未命中分列。
	UpstreamDeepSeek
)

// String 返回稳定的英文标识。
func (u Upstream) String() string {
	switch u {
	case UpstreamOpenAI:
		return "openai"
	case UpstreamAnthropic:
		return "anthropic"
	case UpstreamDeepSeek:
		return "deepseek"
	default:
		return fmt.Sprintf("Upstream(%d)", uint8(u))
	}
}

// RawUsage 是各家上报的原始用量字段。
//
// 全部用指针：**「字段不在」与「字段是 0」必须分开**。上游没报 cached_tokens
// 说明它不支持这个概念，报了 0 说明这次没命中缓存——前者不该出现在账单
// 明细里，后者应该。用值类型会把两者混成一件事。
//
// 这是唯一一个所有字段都可选的入口：上游的自由发挥（多给、少给、改名）
// 都在这里被吸收，出口只有一个确定形状。
type RawUsage struct {
	Upstream Upstream

	// ── OpenAI 口径 ───────────────────────────────────────────────
	// PromptTokens 含缓存。
	PromptTokens *int64
	// CompletionTokens 是输出。Anthropic 用 OutputTokens。
	CompletionTokens *int64
	// CachedTokens 是 prompt 里命中缓存的部分（子集）。
	CachedTokens *int64
	// CacheWriteTokens 是 OpenAI 风格缓存写入（多数源不报）。
	CacheWriteTokens *int64
	// ReasoningTokens 是单独报出的推理 token。
	ReasoningTokens *int64

	// ── Anthropic 口径 ────────────────────────────────────────────
	// InputTokens **不含**缓存。
	InputTokens *int64
	// OutputTokens 与 CompletionTokens 同义，按来源择一填。
	OutputTokens *int64
	// CacheReadInputTokens 是命中缓存的输入。
	CacheReadInputTokens *int64
	// CacheCreationInputTokens 是写入缓存的输入。
	CacheCreationInputTokens *int64
	// CacheCreation5mTokens / 1hTokens 是缓存写入按时长拆分的明细。
	// 它们**已经包含在** CacheCreationInputTokens 里，是更细的分解而不是
	// 额外的量——加进总量会重复计费。它们只影响单价。
	CacheCreation5mTokens *int64
	CacheCreation1hTokens *int64

	// ── DeepSeek 口径 ─────────────────────────────────────────────
	// PromptCacheHitTokens / PromptCacheMissTokens 分列，相加是输入总量。
	PromptCacheHitTokens  *int64
	PromptCacheMissTokens *int64

	// UnitCounts 是非 token 计量：搜索次数、图片张数、视频秒数。
	UnitCounts map[string]int64
	// Raw 保留上游原样的 usage，仅排查用，不参与计费。
	Raw map[string]any
}

// ErrNoInputTokens 表示一个 usage 里既没有输入也没有缓存信息。
//
// 这不是故障：有的源对某些端点根本不报 usage。单独一个错误类型是为了
// 让调用方能决定是接受一个「只有输出的用量」还是据此去估算输入。
var ErrNoInputTokens = errors.New("usage 里没有输入 token")

// Normalize 把上游 usage 归一成 channel.Usage（PromptTokens 含缓存）。
//
// 不返回错误的情形：字段缺失一律按 0 处理，因为「这家不报这个字段」
// 是常态而非异常。返回的错误只用于真正自相矛盾的输入（负值）。
func Normalize(u RawUsage) (channel.Usage, error) {
	if err := checkNonNegative(u); err != nil {
		return channel.Usage{}, err
	}

	out := channel.Usage{
		UnitCounts: u.UnitCounts,
		Raw:        u.Raw,
	}

	switch u.Upstream {
	case UpstreamAnthropic:
		// Anthropic：input_tokens 不含缓存，三者相加才是输入总量。
		// 这一步写反（只取 input_tokens）会让每一次带缓存的请求都少计
		// 输入——而 Claude 的缓存命中率在有 agent 循环的场景下很高，
		// 少计的部分会很大。
		read := deref(u.CacheReadInputTokens)
		write := deref(u.CacheCreationInputTokens)
		out.PromptTokens = deref(u.InputTokens) + read + write
		out.CacheReadTokens = read
		out.CacheWriteTokens = write
		out.CompletionTokens = deref(u.OutputTokens)
		if out.CompletionTokens == 0 {
			// 少数源把输出也放在 completion_tokens 里。
			out.CompletionTokens = deref(u.CompletionTokens)
		}

	case UpstreamDeepSeek:
		// DeepSeek：命中与未命中**分列**，相加是输入总量。
		// 有源只报 prompt_tokens（其中已含命中），此时按子集口径处理——
		// 用「miss 字段在不在」判断，而不是用「和是不是 0」：
		// 只报了 hit 而没有 miss 的源是常见的，而 hit+miss 恰好为 0
		// 也可能是「这次真的没有输入」。
		hit := deref(u.PromptCacheHitTokens)
		miss := deref(u.PromptCacheMissTokens)
		if u.PromptCacheMissTokens != nil {
			out.PromptTokens = hit + miss
		} else {
			out.PromptTokens = deref(u.PromptTokens)
			if out.PromptTokens == 0 {
				// 连 prompt_tokens 都没有，只有 hit：至少要把 hit 算进去，
				// 否则会出现「有缓存命中但输入为零」这种自相矛盾的账。
				out.PromptTokens = hit
			}
		}
		out.CacheReadTokens = hit
		if out.CacheReadTokens == 0 {
			out.CacheReadTokens = deref(u.CachedTokens)
		}
		out.CompletionTokens = deref(u.CompletionTokens)
		if out.CompletionTokens == 0 {
			out.CompletionTokens = deref(u.OutputTokens)
		}

	default: // UpstreamOpenAI
		// OpenAI：prompt_tokens **已含**缓存。这里绝不能再加一次——
		// 加了就是重复计费，而它不会报错，只会让账单悄悄变高。
		out.PromptTokens = deref(u.PromptTokens)
		if out.PromptTokens == 0 && u.InputTokens != nil {
			// 少数兼容层把 Anthropic 的字段名直接搬过来用。
			// 此时按 Anthropic 口径处理，否则输入会被算漏。
			out.PromptTokens = deref(u.InputTokens) +
				deref(u.CacheReadInputTokens) + deref(u.CacheCreationInputTokens)
		}
		out.CacheReadTokens = deref(u.CachedTokens)
		if out.CacheReadTokens == 0 {
			out.CacheReadTokens = deref(u.CacheReadInputTokens)
		}
		out.CacheWriteTokens = deref(u.CacheWriteTokens)
		if out.CacheWriteTokens == 0 {
			out.CacheWriteTokens = deref(u.CacheCreationInputTokens)
		}
		out.CompletionTokens = deref(u.CompletionTokens)
		if out.CompletionTokens == 0 {
			out.CompletionTokens = deref(u.OutputTokens)
		}
	}

	// 推理 token 与输出的关系**由协议决定**，不靠数字猜：
	//
	//   - OpenAI：completion_tokens 按官方定义含 completion_tokens_details
	//     里的 reasoning_tokens，后者是前者的子集。
	//   - DeepSeek：同一个形状（completion_tokens_details.reasoning_tokens），
	//     且它的 usage 满足 total_tokens = prompt_tokens + completion_tokens，
	//     推理没有被另算一份。
	//   - Anthropic：output_tokens 含扩展思考的 token，协议不单独报推理量。
	//
	// 三家都是「已含」，所以显式声明 true——声明漏了就会落到零值（「独立报」），
	// 而那会让同一批 token 被收两次钱。减掉推理是计费层（billing.FromChannelUsage）的事。
	out.ReasoningTokens = deref(u.ReasoningTokens)
	out.CompletionIncludesReasoning = true

	if out.ReasoningTokens > out.CompletionTokens {
		// 声明说推理含在输出里，报出来的推理量却大于输出总量——上游自相矛盾。
		// 把输出抬到至少等于推理量，让 channel.Usage 自身自洽：它还要被
		// 序列化给调用方看，而「输出 50、其中推理 80」是读不通的。
		//
		// 抬这一下不改金额：拆分时减掉的量与抬高的量相同，两边都落在 0。
		// 代价是矛盾在这里被抹平，下游拿到的是一份自洽的用量，判不出它来自
		// 矛盾输入——所以「报出矛盾」的职责落在那些**不修补**的生产者身上：
		// relay 路径原样透传上游的两个数，由 pipeline 侧按
		// billing.ReasoningExceedsCompletion 告警。这里保留修补是为了不动
		// 响应里已经发出去的 completion_tokens。
		out.CompletionTokens = out.ReasoningTokens
	}

	// 缓存明细不可能大于输入总量。上游口径不一致时（有的把缓存算进
	// prompt_tokens，有的不算）会出现这种数据。压低而不是放大：
	// 放大输入会让金额变高，而这里没有依据说明该放大多少。
	if total := out.CacheReadTokens + out.CacheWriteTokens; total > out.PromptTokens {
		over := total - out.PromptTokens
		// 先压缓存写入（它更容易缺失），再压缓存读取。
		if cut := min64(over, out.CacheWriteTokens); cut > 0 {
			out.CacheWriteTokens -= cut
			over -= cut
		}
		if cut := min64(over, out.CacheReadTokens); cut > 0 {
			out.CacheReadTokens -= cut
		}
	}

	return out, nil
}

// NormalizeResponseUsage 归一一次非流式响应的 usage。
//
// 与 Normalize 的差别：它多一层「输入缺失时该怎么办」的判断。非流式
// 响应里输入缺失通常是上游没报，而不是这次真的没有输入——所以返回
// ErrNoInputTokens 让调用方决定是否用 CountRequest 补一个估算值。
//
// 流式场景不走这里：流式的 usage 只在最后一个分片里给，
// 中途断开时调用方必须自己估，见 StreamEstimator。
func NormalizeResponseUsage(u RawUsage) (channel.Usage, error) {
	out, err := Normalize(u)
	if err != nil {
		return channel.Usage{}, err
	}
	if out.PromptTokens == 0 && out.CompletionTokens > 0 {
		return out, ErrNoInputTokens
	}
	return out, nil
}

// CacheWriteDetail 返回缓存写入按时长的拆分，用作**价格明细**，不是总量。
//
// 单独一个函数是为了让「这个数不进总量」这件事有个明确的落点：
// Anthropic 的 5m/1h 拆分已经包含在 cache_creation_input_tokens 里，
// 把它们当额外的量相加会重复计费。
func CacheWriteDetail(u RawUsage) map[string]int64 {
	out := map[string]int64{}
	if v := deref(u.CacheCreation5mTokens); v > 0 {
		out["cache_write_5m"] = v
	}
	if v := deref(u.CacheCreation1hTokens); v > 0 {
		out["cache_write_1h"] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// checkNonNegative 拦下负字段。
//
// 负 token 只可能来自解析错误（例如把带符号整数读反）。放过去会算出一笔
// 负数消费，即「上游付钱给用户」——而它不会让任何一处失败。
func checkNonNegative(u RawUsage) error {
	for _, f := range []struct {
		name string
		v    *int64
	}{
		{"prompt_tokens", u.PromptTokens},
		{"completion_tokens", u.CompletionTokens},
		{"input_tokens", u.InputTokens},
		{"output_tokens", u.OutputTokens},
		{"cached_tokens", u.CachedTokens},
		{"cache_read_input_tokens", u.CacheReadInputTokens},
		{"cache_creation_input_tokens", u.CacheCreationInputTokens},
		{"cache_write_tokens", u.CacheWriteTokens},
		{"reasoning_tokens", u.ReasoningTokens},
		{"prompt_cache_hit_tokens", u.PromptCacheHitTokens},
		{"prompt_cache_miss_tokens", u.PromptCacheMissTokens},
	} {
		if f.v != nil && *f.v < 0 {
			return fmt.Errorf("%s 为负 (%d)，疑似解析错误", f.name, *f.v)
		}
	}
	return nil
}

func deref(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
