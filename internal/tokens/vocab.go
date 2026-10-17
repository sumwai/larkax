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

// encodingRule 是「一组模型名前缀 → 一种编码」。
//
// 前缀按顺序匹配，先命中的胜。顺序有意义，所以它是一条条列出来的，
// 不是一张无序的映射。
type encodingRule struct {
	prefixes []string
	enc      tokenizer.Encoding
}

// encodingRules 是模型名前缀到编码的对应关系。
//
// 写成一张表而不是一串 switch 分支，是为了让「encodingForModel 会返回哪些
// 编码」成为一个**从数据里算得出来的**事实：预热集合的完备性用例直接取这张
// 表的值域（见 preparedCodecs），而不是另外抄一份模型名清单。抄一份的话，
// 表里加一条规则而清单没跟着改，两边都不会红——而那个模型第一次调用时照样
// 要为词表加载付时间（同一件事实写在两处、谁也不从谁推导，见 #289）。
var encodingRules = []encodingRule{
	{
		// 老一代：cl100k_base。gpt-4-*（不含 gpt-4o / gpt-4.1）与
		// gpt-3.5-turbo 一代。注意 "gpt-4o" 与 "gpt-4.1" 不是 "gpt-4-" 前缀
		//（以 o / . 分界），所以它们不会落到这一条。
		prefixes: []string{"gpt-4-", "gpt-3.5-turbo", "gpt-35-turbo"},
		enc:      tokenizer.Cl100kBase,
	},
	{
		// 同属 cl100k_base，但是另一类名字：嵌入模型与 002/003 代补全。
		prefixes: []string{"text-embedding-ada-002", "text-davinci-003", "text-davinci-002"},
		enc:      tokenizer.Cl100kBase,
	},
	{
		// 更老的一代：r50k_base。这些名字在上游已经罕见，但别名表里偶尔会残留。
		prefixes: []string{
			"text-davinci-001", "text-curie", "text-babbage", "text-ada",
			"davinci", "curie", "babbage", "ada", "text-search-", "text-similarity-",
			"code-search-", "code-cushman", "code-davinci-001",
		},
		enc: tokenizer.R50kBase,
	},
}

// defaultEncoding 是 encodingRules 一条都没命中时用的编码。
//
// 兜底给新的那个（而不是旧的）是有意的：新模型的上线速度远快于旧模型的
// 使用率增长，猜错时新编码在多数情况下是对的。
const defaultEncoding = tokenizer.O200kBase

// encodingForModel 返回该 OpenAI 模型用的编码。
//
// 只用族级的映射，不逐个模型登记：`o200k_base` 覆盖 gpt-4o 之后的全线
// （含 o 系列与 gpt-5），`cl100k_base` 覆盖 gpt-4 与 gpt-3.5-turbo 一代。
// 新模型落在哪一边取决于它是哪一代的底座，用发布日期判断比用名字判断稳，
// 但发布日期不在模型名里——所以这里按已知的名字前缀判断，兜底给 o200k。
//
// 对应关系在 encodingRules 那张表里，这个函数只负责匹配。
func encodingForModel(m string) tokenizer.Encoding {
	m = strings.ToLower(strings.TrimSpace(m))
	for _, rule := range encodingRules {
		if hasAnyPrefix(m, rule.prefixes...) {
			return rule.enc
		}
	}
	return defaultEncoding
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
// 让后到的那一个等前面那个加载完，比各自解一份（多花 7MB、白算 25ms）
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
// 它覆盖 encodingForModel 的**整个值域**：encodingRules 里出现过的每一种
// 编码，加上兜底那个。这样「没有任何请求为词表加载付过时间」是一句结构性
// 保证，而不是「我们猜哪些模型重要，猜对了的那部分请求不用付」——按流量
// 猜的话，猜错的表现只是那一次请求多几毫秒，**没有任何东西会发现**。
//
// 完备性由 TestPreparedCodecsCoverEveryReachableEncoding 从 encodingRules
// 推导着判，不靠这份清单和那张表各自维护。
//
// 实测：每种编码各起一个独立进程量它自己的加载耗时与增量；合计那一行是
// 同一次 Prepare 的实际值，**不是上面三行相加**（每次加载都含该进程里首次
// 编译切分正则的开销，跨进程加会重复计）：
//
//	o200k_base   约 25ms  RSS +12.4MB（堆 +6.8MB）
//	cl100k_base  约  9ms  RSS  +6.1MB（堆 +3.4MB）
//	r50k_base    约  6ms  RSS  +3.4MB（堆 +1.7MB）
//	三种合计     约 43ms  RSS +21.3MB（堆 +11.9MB，八次实测 40~50ms）
//
// 这个集合只跟着**值域**长：p50k_base 与 p50k_edit 能加载，但不在任何一条
// 规则里，预解它们是纯浪费。反过来，值域里多出一种就必须跟着加——那正是
// 上面那条用例判的事。
var preparedCodecs = []tokenizer.Encoding{
	tokenizer.O200kBase,
	tokenizer.Cl100kBase,
	tokenizer.R50kBase,
}

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
// **只列 tokenizer.Get 真的认的那些。** 依赖里另有一个 GPT2Enc 常量，而
// Get 的 switch 里没有它的分支（codec 包里也没有对应的构造函数），传进去
// 得到的是 ErrEncodingNotSupported。列上去的后果是 AvailableEncodings
// 报出一个永远加载不出来的编码——「能力上限」比真实能力高，而它恰恰是
// 用来回答「这台机器支持到什么程度」的。它不在 encodingForModel 的返回值
// 里，所以计数路径不受影响，受影响的只有这份清单本身。
//
// 与 preparedCodecs 的区别：那个是启动时**主动**加载的子集（= 下面这个
// 集合里、encodingForModel 的值域覆盖到的那部分），这个是**能力上限**。
// p50k/p50k_edit 能加载，但不在任何一条 encodingRule 里，所以本包不预先
// 加载它们；r50k 在值域里，因而会被预解。
//
// 目前只有 AvailableEncodings 在调它，而那个也还没有生产调用方（见它的注释）。
// TestAvailableEncodingsActuallyLoad 逐项真的调一次 tokenizer.Get，所以依赖
// 换了支持范围时这里会有一条用例失败，而不是悄悄失真。
func loadableEncodings() []tokenizer.Encoding {
	return []tokenizer.Encoding{
		tokenizer.O200kBase,
		tokenizer.Cl100kBase,
		tokenizer.P50kBase,
		tokenizer.P50kEdit,
		tokenizer.R50kBase,
	}
}
