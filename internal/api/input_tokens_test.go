package api

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/billing"
	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/relay"
	"github.com/sumwai/larkax/internal/tokens"
)

// 本文件管输入侧的口径：预留按哪些东西算 token。
//
// 预留是**上界**，而结算按上游报的 usage——那份 usage 含工具定义与图片。
// 只算消息正文时预留天然小于结算，差额由平台承担（见 billing.Settlement.Capped）。
// 下面的用例分三层钉住这件事：
//
//   - 口径本身：工具定义与图片各加了几个 token（字面量钉住）；
//   - 不变量：加了这两项之后，一条带大工具定义 / 带图片的请求
//     **预留 ≥ 结算**（不撞 Capped），而**去掉那一项就会撞**；
//   - 反向：两者都没有时口径与补漏前逐字相同——加整体余量的实现会在这条上红。

// parseChat 用真实入口解析一段 chat 请求体，拿到链路里那个统一的请求形状。
//
// 不手搭 channel.Request：手搭的夹具会漏掉入口真实写进去的字段，而这一层
// 要算的正是「入口转换之后还剩什么」。
func parseChat(t *testing.T, body string) *channel.Request {
	t.Helper()
	req, err := relay.ParseRequest(context.Background(), channel.ProtocolOpenAI, []byte(body))
	if err != nil {
		t.Fatalf("解析请求体失败: %v", err)
	}
	return req
}

// ── 口径本身 ───────────────────────────────────────────────────────

// 工具定义与图片之外没有别的东西时，输入口径与只算正文**逐字相同**。
//
// 这条是「没有加整体余量」的判据：把某个常数（比如整体乘 1.1、或每条请求
// 加一个固定垫底）塞进 inputTokensOf 的实现，会在这里变红。
func TestInputTokensWithoutToolsAndImagesIsTextOnly(t *testing.T) {
	bodies := []string{
		validBody,
		`{"model":"m-1","max_tokens":16,"messages":[{"role":"system","content":"你是助手"},{"role":"user","content":"你好，帮我看一下这段配置"}]}`,
	}
	for _, body := range bodies {
		req := parseChat(t, body)
		want := tokens.EstimatedTokens(req.Model, req.PromptText())
		if got := inputTokensOf(req); got != want {
			t.Errorf("inputTokensOf = %d，期望与正文口径一致 %d（请求体 %s）", got, want, body)
		}
	}
}

// 工具定义进口径：每一条按「框架开销 + 内容」加。
//
// 期望值是字面量：夹具序列化出来是 612 字节，内容那一项取启发式估算 224 与
// 按字节地板 ceil(0.35 × 612) = 215 里的较大者，得 224；再加框架 8 = 232。
// 具体怎么算见 tokens.ToolDefinitionInputTokens 的注释。
func TestInputTokensCountsToolDefinitions(t *testing.T) {
	def := `{"type":"function","function":{"name":"search_docs","description":"` +
		strings.Repeat("Look up the matching documents. ", 15) +
		`","parameters":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}}}`
	req := parseChat(t, `{"model":"m-1","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"tools":[`+def+`]}`)

	if len(req.Tools) != 1 {
		t.Fatalf("解析出 %d 条工具定义，期望 1", len(req.Tools))
	}
	// 序列化之后的字节数是上面那个期望值的前提，也顺手钉住它。
	if got, want := len(toolDefinitionJSON(req.Tools[0])), 612; got != want {
		t.Fatalf("工具定义序列化 = %d 字节，期望 %d（改夹具就要重算下面的数）", got, want)
	}

	// "hi" 的正文口径是 1 个 token。
	textOnly := tokens.EstimatedTokens(req.Model, req.PromptText())
	// 工具的 250 = 内容 224 + 每条包装 26（tokens.ToolDefinitionReserveFrameTokens）。
	if got, want := inputTokensOf(req), textOnly+250; got != want {
		t.Errorf("inputTokensOf = %d，期望 %d（正文 %d + 工具 250）", got, want, textOnly)
	}
}

// 图片进口径：每张加一个被钉住的数，两张就加两份——它按**张数**走，
// 不是「有图片就加一次」。
//
// 1400 这个字面量与 tokens.ImageInputTokens 是独立的两次声明：那边钉的是
// 「常数是多少」，这里钉的是「口径按张数累加」。两边都写死，改一处会被另一
// 处撞到。
func TestInputTokensCountsEveryImage(t *testing.T) {
	image := `{"type":"image_url","image_url":{"url":"data:image/png;base64,` +
		strings.Repeat("A", 4096) + `"}}`
	one := parseChat(t, `{"model":"m-1","max_tokens":16,"messages":[{"role":"user","content":[{"type":"text","text":"看这两张图"},`+image+`]}]}`)
	two := parseChat(t, `{"model":"m-1","max_tokens":16,"messages":[{"role":"user","content":[{"type":"text","text":"看这两张图"},`+image+`,`+image+`]}]}`)

	oneText := tokens.EstimatedTokens(one.Model, one.PromptText())
	if got, want := inputTokensOf(one), oneText+1400; got != want {
		t.Errorf("一张图 = %d，期望 %d（正文 %d + 图片 1400）", got, want, oneText)
	}
	twoText := tokens.EstimatedTokens(two.Model, two.PromptText())
	if got, want := inputTokensOf(two), twoText+2800; got != want {
		t.Errorf("两张图 = %d，期望 %d（正文 %d + 图片 2800）", got, want, twoText)
	}
}

// schema 序列化不出来时不能把这条工具定义从口径里抹掉。
//
// 现实里不会发生（入口解的就是 JSON，解出来的 map 一定可序列化），但「失败
// 时返回 0」是这一层最坏的失败方式：一个工具定义悄悄消失，账面上看不出任何
// 异常。NaN 是少数能让 encoding/json 返回错误的值，用它把那条分支按住。
func TestToolDefinitionJSONFallsBackWithoutLosingTheTool(t *testing.T) {
	def := toolDefinitionJSON(channel.Tool{
		Name:        "search_docs",
		Description: "查询文档",
		InputSchema: map[string]any{"minimum": math.NaN()},
	})
	if len(def) == 0 {
		t.Fatal("序列化失败时返回了空定义——这条工具定义会从口径里消失")
	}
	if got := tokens.ToolDefinitionInputTokens("m-1", def); got <= int64(tokens.ToolDefinitionReserveFrameTokens) {
		t.Errorf("失败回退后的估计量 = %d，只有框架开销那么多", got)
	}
}

// ── 上界不变量 ─────────────────────────────────────────────────────

// 单价全部相同，于是金额与 token 数一一对应：预留 = 输入 + max_tokens，
// 结算 = 上游报的输入 + 输出。差额一眼看得出来，不必读金额。
func flatPrices() billing.Prices {
	flat := money.MustParse("0.000001")
	return billing.Prices{
		Prompt: flat, Completion: flat, CacheRead: flat, CacheWrite: flat, Reasoning: flat,
		Declared: map[string]bool{
			"prompt": true, "completion": true, "cache_read": true,
			"cache_write": true, "reasoning": true,
		},
	}
}

// settleAgainst 用「按这个输入量预留」与「上游报的这份用量」走一遍
// 预留 → 结算，返回这次结算。
func settleAgainst(t *testing.T, inputTokens int64, maxTokens int64, actual billing.Usage) billing.Settlement {
	t.Helper()
	reserved, err := billing.Reserve(billing.ReserveRequest{
		Prices:      flatPrices(),
		InputTokens: inputTokens,
		MaxTokens:   maxTokens,
	})
	if err != nil {
		t.Fatalf("算预留失败: %v", err)
	}
	s, err := billing.SettleWithCharges(actual, flatPrices(), billing.Charges{}, reserved)
	if err != nil {
		t.Fatalf("结算失败: %v", err)
	}
	return s
}

// 一条**工具定义很大、消息文本很小**的请求：预留要盖住结算。
//
// 上游报的那个输入量是这样来的：工具定义序列化后 795 字节，按实测的 JSON
// 密度上沿 0.29 token/字符（见 internal/tokens/COUNTER.md 那张表）≈ 231，
// 加上上游自己在工具声明外面加的包装（实测约 26，见
// tokens.ToolDefinitionReserveFrameTokens 的注释）与消息框架，取 280。判据是上界，
// 所以拿这一档里偏大的那一头试。
func TestReserveCoversSettlementWithLargeToolDefinitions(t *testing.T) {
	def := `{"type":"function","function":{"name":"search_docs","description":"` +
		strings.Repeat("Look up the matching documents and return the best hits. ", 12) +
		`","parameters":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}}}`
	body := `{"model":"m-1","max_tokens":256,"messages":[{"role":"user","content":"hi"}],"tools":[` + def + `]}`
	req := parseChat(t, body)

	// 输出侧远小于 max_tokens：这条请求的输入以工具定义为主。
	actual := billing.Usage{PromptTokens: 280, CompletionTokens: 64}

	if s := settleAgainst(t, inputTokensOf(req), int64(req.MaxTokens), actual); s.Capped {
		t.Errorf("预留 %s 被结算 %s 顶破：工具定义的 token 没进上界", s.Reserved, s.Breakdown.Total)
	}
	// 红证：只按正文预留，同一次结算就会撞上界——把 inputTokensOf 里的工具
	// 那一项短路成 0，本用例在这两处一起变红。
	textOnly := tokens.EstimatedTokens(req.Model, req.PromptText())
	if s := settleAgainst(t, textOnly, int64(req.MaxTokens), actual); !s.Capped {
		t.Errorf("只按正文预留 %s 也没撞上界——这条用例钉不住工具那一项", s.Reserved)
	}
}

// 一条**带图片**的请求：预留要盖住结算。
//
// 上游那张图按 Anthropic 在 1024×1024 上的口径给：1024×1024/750 ≈ 1398，
// 加消息框架与正文取 1405。取这一头（OpenAI 同一档只要 765）同样是判据是
// 上界的意思。
func TestReserveCoversSettlementWithImage(t *testing.T) {
	image := `{"type":"image_url","image_url":{"url":"data:image/png;base64,` +
		strings.Repeat("A", 4096) + `"}}`
	body := `{"model":"m-1","max_tokens":32,"messages":[{"role":"user","content":[{"type":"text","text":"hi"},` + image + `]}]}`
	req := parseChat(t, body)

	actual := billing.Usage{PromptTokens: 1405, CompletionTokens: 3}

	if s := settleAgainst(t, inputTokensOf(req), int64(req.MaxTokens), actual); s.Capped {
		t.Errorf("预留 %s 被结算 %s 顶破：图片的 token 没进上界", s.Reserved, s.Breakdown.Total)
	}
	// 红证：只按正文预留，同一次结算就会撞上界——把图片那一项短路成 0，
	// 本用例在这两处一起变红。
	textOnly := tokens.EstimatedTokens(req.Model, req.PromptText())
	if s := settleAgainst(t, textOnly, int64(req.MaxTokens), actual); !s.Capped {
		t.Errorf("只按正文预留 %s 也没撞上界——这条用例钉不住图片那一项", s.Reserved)
	}
}
