package billing

import (
	"crypto/sha256"
	"sync"
	"time"
)

// BlockHash 是请求里一块内容的摘要。
//
// 块是缓存前缀比对的最小单位：system 消息一块、tools 定义一块、
// 其余每条 message 各一块（见 pipeline.CachePrefixOf）。用摘要而不是原文：
// 记录要在进程里长期活着，存原文等于把每个用户的历史请求复制一份。
type BlockHash [sha256.Size]byte

// HashBlock 算一块内容的摘要。
//
// 它只负责摘要，不负责「一块内容是什么」——那一步在算出块的地方
// （见 pipeline.CachePrefixOf）：本包不认识消息结构，也不该认识。
func HashBlock(content []byte) BlockHash {
	return sha256.Sum256(content)
}

// CacheRecord 是一次已结算请求留下的前缀记录。
//
// 三个字段各有用途，缺一不可：
//
//   - Blocks 是那次请求的块序列，用来判断「这次是不是以前缀开头」；
//   - TotalTokens 是那次请求的**整段**输入 token 数（见 Usage.InputTokens），
//     命中时它整段都按缓存价结算；
//   - SettledAt 是结算时刻，2 秒与 1 小时两个门槛按它判。
type CacheRecord struct {
	// Blocks 是那次请求的块序列。空序列不会被记录（见 CacheStore.Record）。
	Blocks []BlockHash
	// TotalTokens 是那次请求的整段输入 token 数（见 Usage.InputTokens）：
	// 上游报的 prompt 那一整段，缓存部分只算一次。
	//
	// 是整段而不是其中 cache_read 那一部分：上游落盘的是「输入结束位置那一整段」
	// （见 CacheStore 的注释），下一次命中时整段都按缓存价结算。
	TotalTokens int64
	// SettledAt 是那次请求结算的时刻。
	SettledAt time.Time
}

const (
	// cacheSettleGapMin 是「上一次结算离现在太近，缓存未必已落盘」的下限。
	//
	// 2 秒这个数是自定的保守值，不是上游承诺：实测间隔 < 5s 的前缀命中率
	// 明显低于 5–60s 那一档，说明刚落盘的单元在几秒内未必生效。
	// 判早了的代价是把没命中的算成命中——差额由平台承担（见 CacheStore）。
	cacheSettleGapMin = 2 * time.Second
	// cacheSettleGapMax 是「上一次结算太久远，缓存已经过期」的上限。
	//
	// 1 小时同样是自定值：实测间隔 > 1h 的前缀命中率为 0。超过它的记录不再
	// 参与判断，也会在下次写入时被清掉。
	cacheSettleGapMax = time.Hour
)

// CacheStore 记着每把 key 在每条渠道上每个模型的前缀记录，供预留判断缓存命中。
//
// # 它在进程内存里，不进库
//
// 记录只用来把预留上界往下压。判错的方向只有两种，都不影响账目正确性：
//
//   - 没记录、记录过期、或者多实例下别的实例写的那条看不见 —— 退化成
//     hit = 0，也就是这条特性出现之前的行为（按整段输入的最贵单价估）；
//   - 记下了、前缀也对上了，但上游实际没命中 —— 那一段按缓存价估的预留
//     小于结算，撞 Capped，差额由平台承担（见 Settlement.Capped）。
//     这是这条特性唯一的亏损路径，2 秒门槛就是为了压掉它（见 cacheSettleGapMin）。
//
// 所以它不需要与数据同生共死：进程重启丢掉的只是「下一次请求多冻一点」。
//
// # 按 (api_key_id, channel_id, model) 隔离
//
// 三个字段一起才是「谁的缓存」：缓存按上游账号落盘，而同一把 key 在不同渠道上
// 落的是不同上游的盘；同一个模型名的不同模型也各有各的前缀。跨渠道共享记录
// 会把一条渠道的命中算到另一条头上，方向是漏收。
//
// # 零值可用
//
// 零值是一个没有记录的存储。Pipeline 把它作为值字段持有，于是直接构造的
// Pipeline（测试、或还没装配存储的入口）自动退化成 hit = 0，不需要额外初始化。
type CacheStore struct {
	mu      sync.Mutex
	records map[cacheRecordKey]CacheRecord
}

// cacheRecordKey 是记录的键。
//
// Model 用**规范模型名**（pipeline.Request.CanonicalModel）：同一渠道上它到
// 上游模型名是一对一（别名只做映射），而计费这一层只知道规范名。
type cacheRecordKey struct {
	APIKeyID  int64
	ChannelID int64
	Model     string
}

// Resolve 返回这次请求**已确认**命中缓存的 token 数；确认不了时返回 0。
//
// 判定按顺序是四条，任何一条不成立都返回 0：
//
//  1. 没有记录（含记录已过期被清掉）；
//  2. 距上次结算 < 2 秒 —— 刚落盘的单元未必生效（见 cacheSettleGapMin）；
//  3. 距上次结算 > 1 小时 —— 缓存已过期（见 cacheSettleGapMax）；
//  4. 这次的前缀不以上次的前缀开头 —— 上游按「输入结束位置」整段落盘，
//     前缀变了就不是同一段。
//
// 第 4 条只认**完整前缀**：上次是 [A, B]、这次是 [A, C] 时返回 0，尽管上游
// 可能已经把 [A] 作为公共前缀落了盘。少算的代价是多冻，多算的代价是漏收，
// 方向不能选错。同理，这次比上次短（前缀变短）也不算完整匹配。
//
// 命中时返回的是上次的**整段**输入量，而不是其中缓存读的那一部分：落盘的是
// 输入结束位置那一整段。
//
// APIKeyID 或 ChannelID 为 0 时直接返回 0：0 表示入口没填（测试里直接构造的
// 请求就是这样），按 0 归档会让互不相干的请求共用一条记录。
func (s *CacheStore) Resolve(apiKeyID, channelID int64, model string, prefix []BlockHash, now time.Time) int64 {
	if apiKeyID == 0 || channelID == 0 || len(prefix) == 0 {
		return 0
	}

	s.mu.Lock()
	rec, ok := s.records[cacheRecordKey{APIKeyID: apiKeyID, ChannelID: channelID, Model: model}]
	s.mu.Unlock()
	if !ok {
		return 0
	}

	// 门槛按**结算时刻**算，不按记录的写入时刻：结算才是上游落盘的时间点。
	gap := now.Sub(rec.SettledAt)
	if gap < cacheSettleGapMin || gap > cacheSettleGapMax {
		return 0
	}
	if rec.TotalTokens <= 0 || len(rec.Blocks) == 0 || len(prefix) < len(rec.Blocks) {
		return 0
	}
	for i, block := range rec.Blocks {
		if prefix[i] != block {
			return 0
		}
	}
	return rec.TotalTokens
}

// Record 记下一次已结算请求的前缀与整段输入量。
//
// # 只在块数不减少时覆盖
//
// 同一把 key 可以并发发多条请求，两条结束的先后不一定与长短一致。短的那条
// 后到就覆盖长的那条的话，后续请求会匹配到更短的记录、命中更少——多冻而已，
// 但那是白白多冻，而修它只要一个比较。
//
// # 什么不记
//
// 四种情况直接跳过，都不改变账目：身份缺失（APIKeyID 或 ChannelID 为 0）、
// 没有块序列、整段输入量不为正。后两种是「这次请求没有可用的前缀依据」，
// 记下来只会覆盖掉一条更好的记录。
//
// 用量是不是**实测**由调用方判（见 pipeline.Settle）：估算值不是上游那一段
// 落了多少 token，拿它当命中量会把上界的这一侧压到实际之下。
func (s *CacheStore) Record(apiKeyID, channelID int64, model string, rec CacheRecord, now time.Time) {
	if apiKeyID == 0 || channelID == 0 || len(rec.Blocks) == 0 || rec.TotalTokens <= 0 {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.records == nil {
		s.records = make(map[cacheRecordKey]CacheRecord)
	}
	s.dropExpired(now)

	key := cacheRecordKey{APIKeyID: apiKeyID, ChannelID: channelID, Model: model}
	if old, ok := s.records[key]; ok && len(rec.Blocks) < len(old.Blocks) {
		return
	}
	// 复制一份：调用方可能在写入之后继续用同一个切片，而记录的生命周期比
	// 那次调用长。
	rec.Blocks = append([]BlockHash(nil), rec.Blocks...)
	s.records[key] = rec
}

// dropExpired 清掉已经过了 cacheSettleGapMax 的记录。
//
// 调用方持锁。清理挂在写入上而不是单开一个 goroutine：记录只在结算时写，
// 频次与请求数同阶，扫一遍的成本是 map 大小；而多一个后台协程就多一处
// 生命周期要管，且它的停止时机与 Pipeline 共享实例的边界对不上。
//
// 过期的记录本来也判不中（Resolve 第 3 条），清掉只是不让它们一直占内存。
func (s *CacheStore) dropExpired(now time.Time) {
	for key, rec := range s.records {
		if now.Sub(rec.SettledAt) > cacheSettleGapMax {
			delete(s.records, key)
		}
	}
}
