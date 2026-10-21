package pipeline

import (
	"context"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/billing"
	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/tokens"
)

func block(s string) billing.BlockHash { return billing.HashBlock([]byte(s)) }

// sampleRequest 是一条带 system、tools、user、assistant 的请求。
func sampleRequest() *channel.Request {
	return &channel.Request{
		Model: "upstream-model",
		Messages: []channel.Message{
			{Role: "system", Content: []channel.ContentBlock{{Type: "text", Text: "you are helpful"}}},
			{Role: "user", Content: []channel.ContentBlock{{Type: "text", Text: "hi"}}},
			{Role: "assistant", Content: []channel.ContentBlock{{Type: "text", Text: "hello"}}},
		},
		Tools: []channel.Tool{{
			Name:        "search",
			Description: "search the web",
			InputSchema: map[string]any{"type": "object"},
		}},
	}
}

// 块粒度：system 一块、tools 一块、其余每条 message 各一块。
//
// 这不是实现细节，而是前缀比对能成立的前提：末尾多一条消息时，前面的块必须
// **逐块相同**，否则 [A, B] 永远成不了 [A, B, C] 的前缀。
func TestCachePrefixOfBlockGranularity(t *testing.T) {
	blocks := CachePrefixOf(sampleRequest())
	if len(blocks) != 4 {
		t.Fatalf("块数 = %d，期望 4（system 1 + tools 1 + message 2）", len(blocks))
	}

	// 相同的请求得到相同的块序列——块是按内容算的摘要。
	again := CachePrefixOf(sampleRequest())
	for i := range blocks {
		if blocks[i] != again[i] {
			t.Errorf("第 %d 块在相同请求上不同——摘要不稳定", i)
		}
	}

	// 末尾多一条消息：前 4 块逐块不变，只多出第 5 块。
	longer := sampleRequest()
	longer.Messages = append(longer.Messages, channel.Message{
		Role: "user", Content: []channel.ContentBlock{{Type: "text", Text: "再来一次"}},
	})
	extended := CachePrefixOf(longer)
	if len(extended) != 5 {
		t.Fatalf("加一条消息后块数 = %d，期望 5", len(extended))
	}
	for i := range blocks {
		if blocks[i] != extended[i] {
			t.Errorf("第 %d 块在末尾追加消息后变了——那会让前缀比对永远不成立", i)
		}
	}

	// 改中间一条消息：它自己那一块变，位置不变。
	changed := sampleRequest()
	changed.Messages[1].Content[0].Text = "换一句"
	moved := CachePrefixOf(changed)
	if len(moved) != len(blocks) {
		t.Fatalf("改一条消息后块数 = %d，期望 %d", len(moved), len(blocks))
	}
	if moved[2] == blocks[2] {
		t.Error("改了 user 消息，它那一块却没变")
	}
	if moved[0] != blocks[0] || moved[1] != blocks[1] {
		t.Error("改一条消息把前面的块也带变了——块之间没有独立")
	}
}

// 多条 system 消息合在一块；多个 tools 定义合在一块。
func TestCachePrefixOfMergesSystemAndTools(t *testing.T) {
	systemOnly := &channel.Request{Messages: []channel.Message{
		{Role: "system", Content: []channel.ContentBlock{{Type: "text", Text: "a"}}},
		{Role: "system", Content: []channel.ContentBlock{{Type: "text", Text: "b"}}},
	}}
	if got := len(CachePrefixOf(systemOnly)); got != 1 {
		t.Errorf("两条 system 消息折成 %d 块，期望 1", got)
	}

	toolsOnly := &channel.Request{Tools: []channel.Tool{{Name: "a"}, {Name: "b"}}}
	if got := len(CachePrefixOf(toolsOnly)); got != 1 {
		t.Errorf("两个 tools 定义折成 %d 块，期望 1", got)
	}

	if got := CachePrefixOf(nil); got != nil {
		t.Errorf("nil 请求应返回 nil，得到 %d 块", len(got))
	}
}

// 按块算前缀的直接效果：上次是 [system, tools, user]、这次末尾多一条消息时
// 仍然命中，命中量是上次的整段输入量。
func TestCachePrefixMatchesLongerRequestByBlocks(t *testing.T) {
	short := sampleRequest()
	long := sampleRequest()
	long.Messages = append(long.Messages, channel.Message{
		Role: "user", Content: []channel.ContentBlock{{Type: "text", Text: "继续"}},
	})

	var store billing.CacheStore
	now := fixedAt
	store.Record(11, 7, "m", billing.CacheRecord{
		Blocks:      CachePrefixOf(short),
		TotalTokens: 900,
		SettledAt:   now.Add(-5 * time.Second),
	}, now)

	if got := store.Resolve(11, 7, "m", CachePrefixOf(long), now); got != 900 {
		t.Errorf("更长的请求命中量 = %d，期望 900", got)
	}
}

// 缓存记录按候选各自查，不拿一条候选的命中量给所有候选共用。
func TestReserveResolvesCacheHitPerCandidate(t *testing.T) {
	src := &ruleSource{byChannel: map[int64]Rule{
		7: {ID: 1, Version: 1, Source: simpleRule},
		8: {ID: 2, Version: 1, Source: simpleRule},
	}}
	req := baseRequest()
	req.APIKeyID = 11
	req.Candidates = []RuleCandidate{{ChannelID: 7, OwnerUserID: 9}, {ChannelID: 8, OwnerUserID: 9}}
	req.CachePrefix = []billing.BlockHash{block("a"), block("b")}

	// 只给渠道 8 记一条：整段 1000 都命中缓存。
	record := func(p *Pipeline, channelID int64) {
		p.cache.Record(11, channelID, req.CanonicalModel, billing.CacheRecord{
			Blocks:      req.CachePrefix,
			TotalTokens: 1000,
			SettledAt:   fixedAt.Add(-5 * time.Second),
		}, fixedAt)
	}

	// 渠道 7 没记录：1000 × 1e-6 + 500 × 2e-6 = 0.002
	// 渠道 8 整段命中：0 × 1e-6 + 500 × 2e-6 = 0.001
	p := newPipeline(t, src)
	record(p, 8)
	res, err := p.Reserve(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Amount.String() != "0.00200000" {
		t.Errorf("上界 = %s，期望 0.002（渠道 8 的命中不该算到渠道 7 头上）", res.Amount)
	}
	if res.RuleID != 1 {
		t.Errorf("给出上界的规则 = %d，期望 1（没有命中的那条候选）", res.RuleID)
	}

	// 反过来：命中在渠道 7 上，上界应来自渠道 8。
	p = newPipeline(t, src)
	record(p, 7)
	res, err = p.Reserve(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Amount.String() != "0.00200000" || res.RuleID != 2 {
		t.Errorf("上界 = %s、规则 = %d，期望 0.002 与 2（命中在 7 上，8 没命中）", res.Amount, res.RuleID)
	}

	// 单候选、有命中：确认量确实把上界压下来了。
	p = newPipeline(t, src)
	record(p, 7)
	req.Candidates = []RuleCandidate{{ChannelID: 7, OwnerUserID: 9}}
	res, err = p.Reserve(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Amount.String() != "0.00100000" {
		t.Errorf("单候选命中时上界 = %s，期望 0.001", res.Amount)
	}
}

// 没有块序列时不查记录：预留退回按整段输入估，也就是这条特性之前的口径。
func TestReserveWithoutCachePrefixKeepsWholeInputEstimate(t *testing.T) {
	src := &ruleSource{rule: Rule{ID: 1, Version: 1, Source: simpleRule}}
	p := newPipeline(t, src)

	req := baseRequest()
	req.APIKeyID = 11
	req.CachePrefix = []billing.BlockHash{block("a")}
	// 先记一条「整段命中」，再从把块序列清空的请求上预留。
	p.cache.Record(11, req.ChannelID, req.CanonicalModel, billing.CacheRecord{
		Blocks: req.CachePrefix, TotalTokens: req.InputTokens, SettledAt: fixedAt.Add(-5 * time.Second),
	}, fixedAt)

	res, err := p.Reserve(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Amount.String() != "0.00100000" {
		t.Fatalf("带块序列时上界 = %s，期望 0.001", res.Amount)
	}

	req.CachePrefix = nil
	res, err = p.Reserve(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Amount.String() != "0.00200000" {
		t.Errorf("没有块序列时上界 = %s，期望 0.002（按整段输入估）", res.Amount)
	}
}

// 记录里的整段输入是上游报的 prompt 那一整段：缓存占九成时也只算一次。
//
// 结算用的 usage 是计费口径（缓存已从 prompt 里减出去），所以「整段」是
// prompt + cache_read + cache_write 三项之和（见 billing.Usage.InputTokens）。
// 若把它错记成非缓存的那一项，下一次命中量会小一个数量级，上界退回到接近
// 旧口径——这条特性在最该生效的那类请求上失效。
func TestSettleRecordsWholeInputWhenMostlyCached(t *testing.T) {
	const cachedRule = `{"prompt": 0.000001, "completion": 0.000002, "cache_read": 0.0000001}`
	src := &ruleSource{rule: Rule{ID: 1, Version: 1, Source: cachedRule}}

	now := fixedAt
	p := newPipeline(t, src, func(p *Pipeline) {
		p.Now = func() time.Time { return now }
	})

	req := baseRequest()
	req.APIKeyID = 11
	req.CachePrefix = []billing.BlockHash{block("a"), block("b")}

	res, err := p.Reserve(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Settle(context.Background(), req, res, Outcome{
		// 上游报的 prompt 含缓存：整段 1000 = 100 非缓存 + 900 缓存读。
		Usage:       channel.Usage{PromptTokens: 1000, CacheReadTokens: 900, CompletionTokens: 100},
		UsageSource: tokens.SourceMeasured,
	}); err != nil {
		t.Fatal(err)
	}

	now = fixedAt.Add(5 * time.Second)
	req.CachePrefix = append(req.CachePrefix, block("c"))
	res, err = p.Reserve(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	// 整段 1000 命中：1000 × 1e-7 = 0.0001；输出 500 × 2e-6 = 0.001。
	// 若记录只存了非缓存的 100：900 × 1e-6 + 100 × 1e-7 = 0.00091，合计 0.00191。
	if res.Amount.String() != "0.00110000" {
		t.Errorf("上界 = %s，期望 0.0011（整段输入都算命中，缓存部分只算一次）", res.Amount)
	}
}

// 结算把这次请求的前缀记下来，下一次预留就能用上。
//
// 只记实测用量：估算值不是上游那一段落了多少 token，拿它当命中量会把上界
// 压到实际之下。
func TestSettleRecordsMeasuredPrefixForNextReserve(t *testing.T) {
	src := &ruleSource{rule: Rule{ID: 1, Version: 1, Source: simpleRule}}

	now := fixedAt
	p := newPipeline(t, src, func(p *Pipeline) {
		p.Now = func() time.Time { return now }
	})

	req := baseRequest()
	req.APIKeyID = 11
	req.CachePrefix = []billing.BlockHash{block("a"), block("b")}

	res, err := p.Reserve(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Settle(context.Background(), req, res, Outcome{
		// 上游报的 prompt 是整段输入（含缓存部分）：这里就是 1000。
		Usage:       channel.Usage{PromptTokens: 1000, CompletionTokens: 100},
		UsageSource: tokens.SourceMeasured,
	}); err != nil {
		t.Fatal(err)
	}

	// 2 秒门槛之后，同一段前缀（更长）应当命中整段 1000。
	now = fixedAt.Add(5 * time.Second)
	req.CachePrefix = append(req.CachePrefix, block("c"))
	res, err = p.Reserve(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Amount.String() != "0.00100000" {
		t.Errorf("结算之后的上界 = %s，期望 0.001（整段输入命中）", res.Amount)
	}

	// 估算用量不记：另一条请求，同一条渠道与同一段前缀，结算来源是估算。
	now = fixedAt
	p2 := newPipeline(t, src, func(p *Pipeline) {
		p.Now = func() time.Time { return now }
	})
	res, err = p2.Reserve(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p2.Settle(context.Background(), req, res, Outcome{
		Usage:       channel.Usage{PromptTokens: 1000, CompletionTokens: 100},
		UsageSource: tokens.SourceEstimated,
	}); err != nil {
		t.Fatal(err)
	}
	now = fixedAt.Add(5 * time.Second)
	res, err = p2.Reserve(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Amount.String() != "0.00200000" {
		t.Errorf("估算用量被当成记录：上界 = %s，期望 0.002", res.Amount)
	}
}
