package tokens

// 本文件把模型名映射到 tokenizer 族，并封装词表的加载。
//
// 分派按**族**而不是按模型名：同一族的模型共享词表，逐个模型登记会随着
// 上游不断发新版而失控，而漏登记的后果不是报错，是「悄悄退化成估算」。
// 那种失败没有任何信号，只会在对账时表现为一笔笔说不清的差额。

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tiktoken-go/tokenizer"
)

// Family 是一组共享 tokenizer 的模型。
type Family string

const (
	// FamilyOpenAI 是 OpenAI 自家模型，词表随二进制分发（o200k_base / cl100k_base）。
	FamilyOpenAI Family = "openai"
	// FamilyClaude 是 Anthropic 的 Claude。
	//
	// **Anthropic 不公开词表**，所以本族永远走估算。唯一精确的途径是调用
	// 上游的 POST /v1/messages/count_tokens——那是调用方的事（本包不发网络
	// 请求），拿到结果后用 SourceMeasured 记录。
	FamilyClaude Family = "claude"
	// FamilyDeepSeek / Qwen / Kimi / GLM 有公开或半公开的词表
	//（HuggingFace 的 tokenizer.json、tiktoken.model），但需要按族附带词表
	// 文件，本包尚未接入，一律走估算。留这些常量是为了让调用方能显式声明
	// 「我知道这是哪一家」，从而在有实测区间时选对那一列。
	FamilyDeepSeek Family = "deepseek"
	// FamilyQwen 是通义千问一族。
	FamilyQwen Family = "qwen"
	// FamilyKimi 是 Moonshot 的 Kimi 一族。
	FamilyKimi Family = "kimi"
	// FamilyGLM 是智谱的一族。
	FamilyGLM Family = "glm"
	// FamilyUnknown 是没登记的模型。
	FamilyUnknown Family = "unknown"
)

// FamilyOf 返回模型名所属的族。
//
// 用**前缀**匹配而不是精确匹配：上游每个版本都会在名字后面加日期或
// 版本号（-2026-10-16、-preview、-latest、-turbo），精确匹配会随着上游
// 发版而失效，而失效的表现是「悄悄退化成估算」。
//
// 未登记的模型返回 FamilyUnknown 而不是错误：估算对任何输入都有定义，
// 「不认识这家」只意味着精度差一档，不意味着不能计数。
func FamilyOf(model string) Family {
	m := strings.ToLower(strings.TrimSpace(model))
	// 有的渠道把厂商写进模型名（anthropic/claude-…、openrouter 风格），
	// 先剥掉这一段再看。
	if i := strings.LastIndex(m, "/"); i >= 0 && i < len(m)-1 {
		if f := familyOfName(m[i+1:]); f != FamilyUnknown {
			return f
		}
	}
	return familyOfName(m)
}

func familyOfName(m string) Family {
	switch {
	case hasAnyPrefix(m,
		"claude-", "claude2", "claude3", "claude4", "claude5", "claude6",
		"anthropic.claude"):
		return FamilyClaude
	case hasAnyPrefix(m, "deepseek-", "deepseek."):
		return FamilyDeepSeek
	case hasAnyPrefix(m, "qwen", "qwq", "qvq"):
		return FamilyQwen
	case hasAnyPrefix(m, "kimi", "moonshot"):
		return FamilyKimi
	case hasAnyPrefix(m, "glm-", "glm4", "chatglm"):
		return FamilyGLM
	case hasAnyPrefix(m,
		"gpt-", "gpt3", "gpt4", "gpt5", "chatgpt-",
		"o1", "o3", "o4",
		"text-embedding-", "text-davinci", "code-davinci", "davinci",
		"text-curie", "text-babbage", "text-ada", "curie", "babbage", "ada",
		"ft:gpt", "ft:davinci", "ft:babbage",
		"text-search-", "text-similarity-", "code-search-",
		"gpt-3.5", "gpt-35"):
		return FamilyOpenAI
	}
	return FamilyUnknown
}

func hasAnyPrefix(s string, prefixes ...string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// encodingForModel 返回该 OpenAI 模型用的编码。
//
// 只用族级的映射，不逐个模型登记：`o200k_base` 覆盖 gpt-4o 之后的全线
// （含 o 系列与 gpt-5），`cl100k_base` 覆盖 gpt-4 与 gpt-3.5-turbo 一代。
// 新模型落在哪一边取决于它是哪一代的底座，用发布日期判断比用名字判断稳，
// 但发布日期不在模型名里——所以这里按已知的名字前缀判断，兜底给 o200k。
//
// 兜底给新的那个（而不是旧的）是有意的：新模型的上线速度远快于旧模型的
// 使用率增长，猜错时新编码在多数情况下是对的。
func encodingForModel(m string) tokenizer.Encoding {
	m = strings.ToLower(strings.TrimSpace(m))
	switch {
	// 老一代：cl100k_base。gpt-4-*（不含 gpt-4o / gpt-4.1）与 gpt-3.5-turbo 一代。
	// 注意 "gpt-4o" 与 "gpt-4.1" 不是 "gpt-4-" 前缀（以 o / . 分界），
	// 所以它们不会落到这一支。
	case hasAnyPrefix(m, "gpt-4-", "gpt-3.5-turbo", "gpt-35-turbo"):
		return tokenizer.Cl100kBase
	case hasAnyPrefix(m, "text-embedding-ada-002", "text-davinci-003", "text-davinci-002"):
		return tokenizer.Cl100kBase
	// 更老的一代：r50k_base。这些名字在上游已经罕见，但别名表里偶尔会残留。
	case hasAnyPrefix(m, "text-davinci-001", "text-curie", "text-babbage", "text-ada",
		"davinci", "curie", "babbage", "ada", "text-search-", "text-similarity-",
		"code-search-", "code-cushman", "code-davinci-001"):
		return tokenizer.R50kBase
	}
	// 其余（gpt-4o、gpt-4.1、o 系列、gpt-5 及以后）走 o200k_base。
	return tokenizer.O200kBase
}

// ── 词表加载 ───────────────────────────────────────────────────────

// vocabSet 持有已加载的词表句柄。
//
// 按编码分别惰性加载并各自缓存：一个进程通常只用得到其中一两种，
// 把它们全部预先解出来是白花 16MB。
type vocabSet struct {
	mu       sync.Mutex
	codecs   map[tokenizer.Encoding]tokenizer.Codec
	loadedAt map[tokenizer.Encoding]time.Duration
	prepared bool
}

func newVocabSet() *vocabSet {
	return &vocabSet{
		codecs:   make(map[tokenizer.Encoding]tokenizer.Codec, 2),
		loadedAt: make(map[tokenizer.Encoding]time.Duration, 2),
	}
}

// codecFor 取一个词表句柄，必要时加载。
//
// 加锁的范围包含加载过程：两个 goroutine 同时首次请求同一种编码时，
// 让后到的那一个等前面那个加载完，比各自解一份（多花 7MB、白算 40ms）
// 更划算。加载是每进程一次的路径，这里不追求无锁。
func (v *vocabSet) codecFor(enc tokenizer.Encoding) (tokenizer.Codec, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if c, ok := v.codecs[enc]; ok {
		return c, nil
	}
	start := time.Now()
	c, err := tokenizer.Get(enc)
	if err != nil {
		return nil, fmt.Errorf("加载词表 %s 失败: %w", enc, err)
	}
	v.codecs[enc] = c
	v.loadedAt[enc] = time.Since(start)
	return c, nil
}

// preparedCodecs 是 Prepare 要加载的编码集合。
//
// 只加载覆盖主流流量的两种（o200k 与 cl100k）。把它们全部加载会让
// 常驻内存多出约 16MB，而那三种老编码（r50k/p50k/gpt2）的流量占比可以忽略。
var preparedCodecs = []tokenizer.Encoding{tokenizer.O200kBase, tokenizer.Cl100kBase}

// prepare 预加载词表，返回每种编码的加载耗时。
func (v *vocabSet) prepare(ctx context.Context) (map[string]time.Duration, error) {
	for _, enc := range preparedCodecs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, err := v.codecFor(enc); err != nil {
			return nil, err
		}
	}
	v.mu.Lock()
	v.prepared = true
	v.mu.Unlock()
	return v.loadDurations(), nil
}

// loadDurations 返回已加载编码的加载耗时，供启动日志使用。
//
// 暴露它是为了回答一个具体的运维问题：「这 16MB 是启动时就解了，
// 还是第一个请求进来才解？」——两者对延迟的影响完全不同。
// 只读自己刚写过的 map，不需要额外同步。
func (v *vocabSet) loadDurations() map[string]time.Duration {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make(map[string]time.Duration, len(v.loadedAt))
	for k, d := range v.loadedAt {
		out[string(k)] = d
	}
	return out
}

// loaded 返回已加载的编码名，已排序。
func (v *vocabSet) loaded() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]string, 0, len(v.codecs))
	for enc := range v.codecs {
		out = append(out, string(enc))
	}
	sort.Strings(out)
	return out
}

// loadableEncodings 是 tokenizer.Get 支持的编码集合。
//
// 单独列一个的原因：本包的词表来自第三方库，而它的支持范围是外部事实。
// 调用方（例如健康检查）要能知道「这台机器上能精确计数哪些编码」，
// 而不必去读依赖的源码或试错。
//
// 与 preparedCodecs 的区别：那个是启动时**主动**加载的子集（覆盖主流
// 流量），这个是**能力上限**。r50k/p50k/gpt2 能加载，只是本包不预先加载
// 它们——真遇到那些老模型时，codecFor 会按需惰性加载。
//
// 目前只有 AvailableEncodings 在调它，而那个也还没有生产调用方（见它的注释）。
func loadableEncodings() []tokenizer.Encoding {
	return []tokenizer.Encoding{
		tokenizer.O200kBase,
		tokenizer.Cl100kBase,
		tokenizer.P50kBase,
		tokenizer.P50kEdit,
		tokenizer.R50kBase,
		tokenizer.GPT2Enc,
	}
}
