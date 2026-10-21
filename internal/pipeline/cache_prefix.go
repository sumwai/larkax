package pipeline

import (
	"encoding/json"

	"github.com/sumwai/larkax/internal/billing"
	"github.com/sumwai/larkax/internal/channel"
)

// CachePrefixOf 把一次请求折成缓存前缀的块序列。
//
// 粒度是「一块一段上游会整体落盘的内容」：
//
//	tools 定义         → 一块
//	每条 message       → 各一块
//	anthropic 的 system → 一块（顶层字段，恒在最前）
//
// # 顺序必须与实际发给上游的顺序一致
//
// 前缀比对的整个前提是「块序列相同就意味着上游看到的前缀相同」。顺序不对时，
// 两条实际前缀不同的请求会得到相同的块序列——那不是误差，是一条稳定压价的
// 构造（交替发两种顺序，第二种永远被判成命中）。
//
// 两条协议的顺序各不相同，依据都在树里：
//
//   - openai / openai-responses：messages 按数组原序发给上游（见
//     internal/relay/relay.go 的 ToRelaykit），所以 system 不做提权——
//     [user, system] 与 [system, user] 是两段不同的前缀。tools 是顶层参数，
//     由上游模板渲染在消息之前。
//   - anthropic：system 是顶层字段（见 internal/channel/anthropicadapter 的
//     encodeRequest），无论它出现在 messages 的哪个位置，实际都排在提示最前。
//
// # 拿不准的协议返回 nil
//
// 未列出的协议（例如决策表面）返回 nil：宁可让预留退回旧口径，也不能拿一个
// 猜的顺序去判命中——猜错的代价是漏收。
//
// # 今天只有隐式缓存的上游会走到这里
//
// 记录要求上游实测报出缓存活动（见 billing.CacheStore）。Anthropic 一侧今天
// 满足不了：channel.ContentBlock 没有 cache_control，relay 的转换也不带它，
// 于是 cache_read / cache_write 恒为 0，按判据不记、不命中——这条特性对
// Anthropic 上游等于没有上线。真正生效的是隐式缓存的上游（OpenAI 口径的
// cached_tokens）。
//
// 若哪天把 cache_control 透传进来，有两件事要一起做：①断点位置必须进块摘要
// （否则两条只差断点的请求块序列相同，又是一次假命中）；②TTL 是逐断点的
// （5 分钟 / 1 小时），单一的 1 小时门槛对 5 分钟那一档偏乐观。
//
// # 算不出来时返回 nil
//
// 序列化失败在当前类型上构造不出来（都是从 JSON 解出来的值），但真走到那里
// 时返回值必须是「没有前缀」而不是「一个较短的前缀」：拿不完整的序列去比，
// 有可能把没命中的判成命中，方向是漏收。nil 让预留退回按整段输入估。
func CachePrefixOf(req *channel.Request, protocol channel.Protocol) []billing.BlockHash {
	if req == nil {
		return nil
	}
	switch protocol {
	case channel.ProtocolAnthropic:
		return anthropicCachePrefix(req)
	case channel.ProtocolOpenAI, channel.ProtocolOpenAIResponses:
		return messageOrderCachePrefix(req)
	default:
		return nil
	}
}

// messageOrderCachePrefix 按 openai 系协议的顺序折块：tools 在前，messages
// 按原序跟在后面，system 不单独提权。
func messageOrderCachePrefix(req *channel.Request) []billing.BlockHash {
	blocks := make([]billing.BlockHash, 0, 1+len(req.Messages))

	if len(req.Tools) > 0 {
		block, ok := hashBlockOf(req.Tools)
		if !ok {
			return nil
		}
		blocks = append(blocks, block)
	}
	for _, m := range req.Messages {
		block, ok := hashBlockOf(m)
		if !ok {
			return nil
		}
		blocks = append(blocks, block)
	}
	return blocks
}

// anthropicCachePrefix 按 anthropic 的顺序折块：system（顶层，合并成一块）
// 在最前、tools 其次、其余 message 按原序。
func anthropicCachePrefix(req *channel.Request) []billing.BlockHash {
	blocks := make([]billing.BlockHash, 0, 2+len(req.Messages))

	var system []channel.Message
	for _, m := range req.Messages {
		if m.Role == "system" {
			system = append(system, m)
		}
	}
	if len(system) > 0 {
		block, ok := hashBlockOf(system)
		if !ok {
			return nil
		}
		blocks = append(blocks, block)
	}
	if len(req.Tools) > 0 {
		block, ok := hashBlockOf(req.Tools)
		if !ok {
			return nil
		}
		blocks = append(blocks, block)
	}
	for _, m := range req.Messages {
		if m.Role == "system" {
			continue
		}
		block, ok := hashBlockOf(m)
		if !ok {
			return nil
		}
		blocks = append(blocks, block)
	}
	return blocks
}

// hashBlockOf 把一块内容序列化后求摘要。
//
// 用 encoding/json 而不是自己拼字符串：块里带着 map（工具参数），
// 而 map 的遍历顺序不稳定——json.Marshal 会按键排序，两次相同的请求
// 因此得到同一个摘要。自己拼的话，同一段内容会时快时慢地算出不同的块，
// 表现为「缓存命中时有时无」，而且不报错。
func hashBlockOf(v any) (billing.BlockHash, bool) {
	payload, err := json.Marshal(v)
	if err != nil {
		return billing.BlockHash{}, false
	}
	return billing.HashBlock(payload), true
}
