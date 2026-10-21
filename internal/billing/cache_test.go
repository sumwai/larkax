package billing

import (
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/channel"
)

// 记录里的「整段输入」是计费口径三项之和，缓存只算一次。
//
// 上游报 prompt=1000、cache_read=900 时，FromChannelUsage 把 900 从 prompt 里
// 减出去、记到 cache_read 上，于是三项之和回到 1000。若这张记录改写成
// Usage.PromptTokens（也就是非缓存的那 100），命中量会小一个数量级——方向是
// 多冻，而缓存占九成的请求正好是这条特性要救的那一类。
func TestUsageInputTokensCountsCacheOnce(t *testing.T) {
	u := FromChannelUsage(channel.Usage{PromptTokens: 1000, CacheReadTokens: 900})
	if got := u.InputTokens(); got != 1000 {
		t.Errorf("整段输入 = %d，期望 1000（缓存部分只算一次）", got)
	}
	// 用例前提：计费口径的 PromptTokens 是非缓存部分，不是整段。
	if u.PromptTokens != 100 {
		t.Fatalf("非缓存输入 = %d，期望 100（用例前提变了，下面的判据要重写）", u.PromptTokens)
	}
}

// 缓存的四个判定条件与覆盖规则逐条钉在这里。每条用例对应 CacheStore 注释里
// 的一条判据，把那条判据改坏（少一个比较、条件写反）就该在对应用例上变红。

var cacheNow = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

func block(s string) BlockHash { return HashBlock([]byte(s)) }

func blocks(names ...string) []BlockHash {
	out := make([]BlockHash, len(names))
	for i, name := range names {
		out[i] = block(name)
	}
	return out
}

// 没有记录时命中量是 0——这是冷启动的既定口径，不给默认命中率。
func TestCacheStoreNoRecordResolvesZero(t *testing.T) {
	var store CacheStore
	if got := store.Resolve(1, 2, "m", blocks("a"), cacheNow); got != 0 {
		t.Errorf("无记录时命中量 = %d，期望 0", got)
	}
}

// 这次以前次前缀开头时，命中量是上次的**整段**输入量。
//
// 比上次长（末尾多一块）也算命中：上游按「输入结束位置」整段落盘，
// [A, B] 是 [A, B, C] 的完整前缀。
func TestCacheStoreResolvesCompletePrefix(t *testing.T) {
	var store CacheStore
	store.Record(1, 2, "m", CacheRecord{Blocks: blocks("a", "b"), TotalTokens: 900, SettledAt: cacheNow.Add(-5 * time.Second)}, cacheNow)

	for _, prefix := range [][]BlockHash{blocks("a", "b"), blocks("a", "b", "c")} {
		if got := store.Resolve(1, 2, "m", prefix, cacheNow); got != 900 {
			t.Errorf("前缀 %d 块时命中量 = %d，期望 900（整段输入量）", len(prefix), got)
		}
	}
}

// 只认完整前缀。公共前缀检测与固定间隔落盘预测不了，推断它们的方向是漏收。
func TestCacheStoreRequiresCompletePrefix(t *testing.T) {
	var store CacheStore
	store.Record(1, 2, "m", CacheRecord{Blocks: blocks("a", "b"), TotalTokens: 900, SettledAt: cacheNow.Add(-5 * time.Second)}, cacheNow)

	cases := map[string][]BlockHash{
		"第 2 块不同":  blocks("a", "c"),
		"比上次短":     blocks("a"),
		"第 1 块就不同": blocks("x", "b"),
	}
	for name, prefix := range cases {
		if got := store.Resolve(1, 2, "m", prefix, cacheNow); got != 0 {
			t.Errorf("%s：命中量 = %d，期望 0（不是完整前缀）", name, got)
		}
	}
}

// 距上次结算 < 2 秒：刚落盘的单元未必生效。门槛本身可以命中。
func TestCacheStoreRejectsTooSoon(t *testing.T) {
	p := []BlockHash{block("a")}
	for _, tc := range []struct {
		name string
		gap  time.Duration
		want int64
	}{
		{"刚结算完", 0, 0},
		{"1.999 秒", cacheSettleGapMin - time.Nanosecond, 0},
		{"正好 2 秒", cacheSettleGapMin, 900},
		{"5 秒", 5 * time.Second, 900},
	} {
		var store CacheStore
		store.Record(1, 2, "m", CacheRecord{Blocks: p, TotalTokens: 900, SettledAt: cacheNow.Add(-tc.gap)}, cacheNow)
		if got := store.Resolve(1, 2, "m", p, cacheNow); got != tc.want {
			t.Errorf("%s：命中量 = %d，期望 %d", tc.name, got, tc.want)
		}
	}
}

// 距上次结算 > 1 小时：缓存已过期。门槛本身仍然命中。
func TestCacheStoreRejectsTooOld(t *testing.T) {
	p := []BlockHash{block("a")}
	for _, tc := range []struct {
		name string
		gap  time.Duration
		want int64
	}{
		{"正好 1 小时", cacheSettleGapMax, 900},
		{"1 小时零 1 纳秒", cacheSettleGapMax + time.Nanosecond, 0},
		{"两小时", 2 * time.Hour, 0},
	} {
		var store CacheStore
		store.Record(1, 2, "m", CacheRecord{Blocks: p, TotalTokens: 900, SettledAt: cacheNow.Add(-tc.gap)}, cacheNow)
		if got := store.Resolve(1, 2, "m", p, cacheNow); got != tc.want {
			t.Errorf("%s：命中量 = %d，期望 %d", tc.name, got, tc.want)
		}
	}
}

// 记录按 (api_key_id, channel_id, model) 分开：三者任一不同就查不到。
//
// 跨渠道共享会把一条渠道的命中算到另一条头上，方向是漏收。
func TestCacheStoreIsolatesByKeyChannelModel(t *testing.T) {
	p := []BlockHash{block("a")}
	var store CacheStore
	store.Record(1, 2, "m", CacheRecord{Blocks: p, TotalTokens: 900, SettledAt: cacheNow.Add(-5 * time.Second)}, cacheNow)

	for _, tc := range []struct {
		name      string
		apiKeyID  int64
		channelID int64
		model     string
	}{
		{"换一把 key", 3, 2, "m"},
		{"换一条渠道", 1, 4, "m"},
		{"换一个模型", 1, 2, "other"},
	} {
		if got := store.Resolve(tc.apiKeyID, tc.channelID, tc.model, p, cacheNow); got != 0 {
			t.Errorf("%s：命中量 = %d，期望 0（记录不跨维度共享）", tc.name, got)
		}
	}
	// 三个字段都对上才命中——上面三条零值不是「查不到」的另一种解释。
	if got := store.Resolve(1, 2, "m", p, cacheNow); got != 900 {
		t.Errorf("同 key 同渠道同模型时命中量 = %d，期望 900", got)
	}
}

// 短记录不覆盖长记录；块数相同或更多时才覆盖。
func TestCacheStoreKeepsLongerRecord(t *testing.T) {
	var store CacheStore
	at := cacheNow.Add(-5 * time.Second)
	store.Record(1, 2, "m", CacheRecord{Blocks: blocks("a", "b"), TotalTokens: 900, SettledAt: at}, cacheNow)

	// 更短的记录后到：不覆盖。[a] 这次查询只在上次记录是 [a] 时才会命中。
	store.Record(1, 2, "m", CacheRecord{Blocks: blocks("a"), TotalTokens: 100, SettledAt: at}, cacheNow)
	if got := store.Resolve(1, 2, "m", blocks("a"), cacheNow); got != 0 {
		t.Fatalf("短记录覆盖了长记录：查 [a] 得到 %d，期望 0（记录仍是 [a, b]）", got)
	}
	if got := store.Resolve(1, 2, "m", blocks("a", "b", "c"), cacheNow); got != 900 {
		t.Fatalf("长记录被短记录挤掉：命中量 = %d，期望 900", got)
	}

	// 块数相同：覆盖。
	store.Record(1, 2, "m", CacheRecord{Blocks: blocks("a", "z"), TotalTokens: 700, SettledAt: at}, cacheNow)
	if got := store.Resolve(1, 2, "m", blocks("a", "z", "q"), cacheNow); got != 700 {
		t.Errorf("等长记录没有覆盖：命中量 = %d，期望 700", got)
	}

	// 块数更多：覆盖。
	store.Record(1, 2, "m", CacheRecord{Blocks: blocks("a", "z", "q"), TotalTokens: 1500, SettledAt: at}, cacheNow)
	if got := store.Resolve(1, 2, "m", blocks("a", "z", "q", "r"), cacheNow); got != 1500 {
		t.Errorf("更长的记录没有覆盖：命中量 = %d，期望 1500", got)
	}
}

// 写入时顺手清掉过期记录：它们判不中，留着只是占内存。
func TestCacheStoreDropsExpiredRecords(t *testing.T) {
	var store CacheStore
	store.Record(1, 2, "m", CacheRecord{Blocks: blocks("a"), TotalTokens: 900, SettledAt: cacheNow}, cacheNow)
	if len(store.records) != 1 {
		t.Fatalf("写了 1 条，记了 %d 条", len(store.records))
	}

	// 两小时后再写一条别的记录，第一条应当被清掉。
	store.Record(3, 4, "m", CacheRecord{Blocks: blocks("a"), TotalTokens: 900, SettledAt: cacheNow.Add(2 * time.Hour)}, cacheNow.Add(2*time.Hour))
	if len(store.records) != 1 {
		t.Errorf("过期记录没被清掉：%d 条", len(store.records))
	}
	if _, ok := store.records[cacheRecordKey{APIKeyID: 1, ChannelID: 2, Model: "m"}]; ok {
		t.Error("过期的那条还在")
	}
}

// 身份缺失、没有块、输入量不为正：都不记。
//
// APIKeyID / ChannelID 为 0 表示入口没填（测试里直接构造的请求），按 0 归档
// 会让互不相干的请求共用一条记录。
func TestCacheStoreSkipsUnusableRecords(t *testing.T) {
	at := cacheNow.Add(-5 * time.Second)
	for _, tc := range []struct {
		name      string
		apiKeyID  int64
		channelID int64
		rec       CacheRecord
	}{
		{"没有 key", 0, 2, CacheRecord{Blocks: blocks("a"), TotalTokens: 900, SettledAt: at}},
		{"没有渠道", 1, 0, CacheRecord{Blocks: blocks("a"), TotalTokens: 900, SettledAt: at}},
		{"没有块", 1, 2, CacheRecord{TotalTokens: 900, SettledAt: at}},
		{"输入量不是正数", 1, 2, CacheRecord{Blocks: blocks("a"), TotalTokens: 0, SettledAt: at}},
	} {
		var store CacheStore
		store.Record(tc.apiKeyID, tc.channelID, "m", tc.rec, cacheNow)
		if len(store.records) != 0 {
			t.Errorf("%s：记了 %d 条，期望 0", tc.name, len(store.records))
		}
	}
}

// 记录里存的是块序列的副本：调用方写过之后再用同一个切片，不会改到已记下的记录。
func TestCacheStoreCopiesBlocks(t *testing.T) {
	var store CacheStore
	p := blocks("a", "b")
	store.Record(1, 2, "m", CacheRecord{Blocks: p, TotalTokens: 900, SettledAt: cacheNow.Add(-5 * time.Second)}, cacheNow)

	p[0] = block("x")
	if got := store.Resolve(1, 2, "m", blocks("a", "b", "c"), cacheNow); got != 900 {
		t.Errorf("记录被调用方后续的写改到了：命中量 = %d，期望 900", got)
	}
}
