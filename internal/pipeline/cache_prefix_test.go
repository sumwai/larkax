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

// 块粒度：tools 一块、每条 message 各一块；块序列必须与发给上游的顺序一致。
//
// 这不是实现细节，而是前缀比对能成立的前提：末尾多一条消息时，前面的块必须
// **逐块相同**，否则 [A, B] 永远成不了 [A, B, C] 的前缀。
func TestCachePrefixOfBlockGranularity(t *testing.T) {
	blocks := CachePrefixOf(sampleRequest(), channel.ProtocolOpenAI)
	if len(blocks) != 4 {
		t.Fatalf("块数 = %d，期望 4（tools 1 + message 3）", len(blocks))
	}

	// 相同的请求得到相同的块序列——块是按内容算的摘要。
	again := CachePrefixOf(sampleRequest(), channel.ProtocolOpenAI)
	for i := range blocks {
		if blocks[i] != again[i] {
			t.Errorf("第 %d 块在相同请求上不同——摘要不稳定", i)
		}
	}

	// 末尾多一条消息：原有的块逐块不变，只多出第 5 块。
	longer := sampleRequest()
	longer.Messages = append(longer.Messages, channel.Message{
		Role: "user", Content: []channel.ContentBlock{{Type: "text", Text: "再来一次"}},
	})
	extended := CachePrefixOf(longer, channel.ProtocolOpenAI)
	if len(extended) != 5 {
		t.Fatalf("加一条消息后块数 = %d，期望 5", len(extended))
	}
	for i := range blocks {
		if blocks[i] != extended[i] {
			t.Errorf("第 %d 块在末尾追加消息后变了——那会让前缀比对永远不成立", i)
		}
	}

	// 改中间一条消息：它自己那一块变，前后都不动。messages 从第 1 块起
	// （第 0 块是 tools），所以 Messages[1] 落在第 2 块。
	changed := sampleRequest()
	changed.Messages[1].Content[0].Text = "换一句"
	moved := CachePrefixOf(changed, channel.ProtocolOpenAI)
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

// 块序列与实际发给上游的顺序一致：openai 系按 messages 原序，不把 system 提权；
// anthropic 的 system 是顶层字段，无论出现在哪里都排在最前。
//
// 这一条是「顺序错就是假命中」的判据：提权 system 时 [user, system] 与
// [system, user] 会得到同一串块，而它们在上游看到的前缀不同。
func TestCachePrefixOfFollowsUpstreamOrder(t *testing.T) {
	userFirst := &channel.Request{Messages: []channel.Message{
		{Role: "user", Content: []channel.ContentBlock{{Type: "text", Text: "hi"}}},
		{Role: "system", Content: []channel.ContentBlock{{Type: "text", Text: "s"}}},
	}}
	systemFirst := &channel.Request{Messages: []channel.Message{
		{Role: "system", Content: []channel.ContentBlock{{Type: "text", Text: "s"}}},
		{Role: "user", Content: []channel.ContentBlock{{Type: "text", Text: "hi"}}},
	}}

	openAIUserFirst := CachePrefixOf(userFirst, channel.ProtocolOpenAI)
	openAISystemFirst := CachePrefixOf(systemFirst, channel.ProtocolOpenAI)
	if len(openAIUserFirst) != 2 || len(openAISystemFirst) != 2 {
		t.Fatalf("openai 块数 = %d / %d，期望各 2（两条 message）",
			len(openAIUserFirst), len(openAISystemFirst))
	}
	if openAIUserFirst[0] == openAISystemFirst[0] || openAIUserFirst[1] == openAISystemFirst[1] {
		t.Error("openai 下两种消息顺序得到了相同的块序列——那是可构造的假命中")
	}

	// 效果落到钱上：前一种顺序留下的记录，后一种顺序查不到。
	var store billing.CacheStore
	store.Record(11, 7, "m", billing.CacheRecord{
		Blocks: openAIUserFirst, CachedTokens: 900, SettledAt: fixedAt.Add(-5 * time.Second),
	}, fixedAt)
	if got := store.Resolve(11, 7, "m", openAISystemFirst, fixedAt); got != 0 {
		t.Errorf("顺序不同的请求命中了 %d 个 token，期望 0", got)
	}

	// anthropic：system 被提到顶层，两种写法在上游是同一段提示，块序列相同。
	anthropicUserFirst := CachePrefixOf(userFirst, channel.ProtocolAnthropic)
	anthropicSystemFirst := CachePrefixOf(systemFirst, channel.ProtocolAnthropic)
	if len(anthropicUserFirst) != 2 || len(anthropicSystemFirst) != 2 {
		t.Fatalf("anthropic 块数 = %d / %d，期望各 2（system 1 + message 1）",
			len(anthropicUserFirst), len(anthropicSystemFirst))
	}
	for i := range anthropicUserFirst {
		if anthropicUserFirst[i] != anthropicSystemFirst[i] {
			t.Errorf("anthropic 第 %d 块不同——system 应在最前、与它在数组里的位置无关", i)
		}
	}
}

// 多条 system 消息只在 anthropic 下合并成一块；openai 下每条 message 各一块。
// 拿不准的协议返回 nil，预留退回旧口径。
func TestCachePrefixOfPerProtocolShape(t *testing.T) {
	systemOnly := &channel.Request{Messages: []channel.Message{
		{Role: "system", Content: []channel.ContentBlock{{Type: "text", Text: "a"}}},
		{Role: "system", Content: []channel.ContentBlock{{Type: "text", Text: "b"}}},
	}}
	if got := CachePrefixOf(systemOnly, channel.ProtocolAnthropic); len(got) != 1 {
		t.Errorf("anthropic 下两条 system 消息折成 %d 块，期望 1", len(got))
	}
	if got := CachePrefixOf(systemOnly, channel.ProtocolOpenAI); len(got) != 2 {
		t.Errorf("openai 下两条 system 消息折成 %d 块，期望 2（按原序各一块）", len(got))
	}

	toolsOnly := &channel.Request{Tools: []channel.Tool{{Name: "a"}, {Name: "b"}}}
	for _, protocol := range []channel.Protocol{channel.ProtocolOpenAI, channel.ProtocolAnthropic} {
		if got := len(CachePrefixOf(toolsOnly, protocol)); got != 1 {
			t.Errorf("%s 下两个 tools 定义折成 %d 块，期望 1", protocol, got)
		}
	}

	if got := CachePrefixOf(sampleRequest(), channel.ProtocolSystemOne); got != nil {
		t.Errorf("未列出的协议应返回 nil，得到 %d 块", len(got))
	}
	if got := CachePrefixOf(nil, channel.ProtocolOpenAI); got != nil {
		t.Errorf("nil 请求应返回 nil，得到 %d 块", len(got))
	}
}

// 按块算前缀的直接效果：上次的块序列是这次的开头时仍然命中，
// 命中量是上次上游实测报出的缓存量。
func TestCachePrefixMatchesLongerRequestByBlocks(t *testing.T) {
	short := sampleRequest()
	long := sampleRequest()
	long.Messages = append(long.Messages, channel.Message{
		Role: "user", Content: []channel.ContentBlock{{Type: "text", Text: "继续"}},
	})

	var store billing.CacheStore
	now := fixedAt
	store.Record(11, 7, "m", billing.CacheRecord{
		Blocks:       CachePrefixOf(short, channel.ProtocolOpenAI),
		CachedTokens: 900,
		SettledAt:    now.Add(-5 * time.Second),
	}, now)

	if got := store.Resolve(11, 7, "m", CachePrefixOf(long, channel.ProtocolOpenAI), now); got != 900 {
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

	// 只给渠道 8 记一条：上游报过 1000 个 token 的缓存活动。
	record := func(p *Pipeline, channelID int64) {
		p.cache.Record(11, channelID, req.CanonicalModel, billing.CacheRecord{
			Blocks:       req.CachePrefix,
			CachedTokens: 1000,
			SettledAt:    fixedAt.Add(-5 * time.Second),
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
		Blocks: req.CachePrefix, CachedTokens: req.InputTokens, SettledAt: fixedAt.Add(-5 * time.Second),
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

const cachedRule = `{"prompt": 0.000001, "completion": 0.000002, "cache_read": 0.0000001}`

// 记录里的缓存量是上游**实测**报出的那一项，不是整段输入。
//
// 上游报 prompt=1000、cache_read=900 时，下游那一次只该按 900 个 token 估缓存，
// 剩下 100 仍按输入价。若把整段 1000 记成命中，上界会再低一截（0.0011）；
// 若只记非缓存的 100，上界又回到 0.00191。
func TestSettleRecordsMeasuredCacheAmount(t *testing.T) {
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
	if _, err = p.Settle(context.Background(), req, res, Outcome{
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
	// 命中 900：非缓存 100 × 1e-6 + 900 × 1e-7 = 0.00019；输出 500 × 2e-6 = 0.001。
	if res.Amount.String() != "0.00119000" {
		t.Errorf("上界 = %s，期望 0.00119（按实测缓存量 900 估）", res.Amount)
	}
}

// 上游没报缓存活动时既不记也不命中：没有依据就不许把输入侧按缓存价估。
//
// 这类渠道的规则通常把 cache_read 填 0，一旦记成「整段命中」，输入侧直接归零，
// 每次结算都撞 Capped——与时间门槛无关，2 秒压不住。
func TestSettleDoesNotRecordWithoutCacheActivity(t *testing.T) {
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
	if _, err = p.Settle(context.Background(), req, res, Outcome{
		Usage:       channel.Usage{PromptTokens: 1000, CompletionTokens: 100},
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
	// 没有记录：仍按整段 1000 × 1e-6 + 输出 0.001 = 0.002。
	if res.Amount.String() != "0.00200000" {
		t.Errorf("上界 = %s，期望 0.002（没有缓存活动时不许按缓存价估）", res.Amount)
	}
}

// 估算用量不记：它不是上游报的缓存量，拿它当命中量会把上界压到实际之下。
func TestSettleDoesNotRecordEstimatedUsage(t *testing.T) {
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
	if _, err = p.Settle(context.Background(), req, res, Outcome{
		Usage:       channel.Usage{PromptTokens: 1000, CacheReadTokens: 900, CompletionTokens: 100},
		UsageSource: tokens.SourceEstimated,
	}); err != nil {
		t.Fatal(err)
	}

	now = fixedAt.Add(5 * time.Second)
	req.CachePrefix = append(req.CachePrefix, block("c"))
	res, err = p.Reserve(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Amount.String() != "0.00200000" {
		t.Errorf("估算用量被当成记录：上界 = %s，期望 0.002", res.Amount)
	}
}
