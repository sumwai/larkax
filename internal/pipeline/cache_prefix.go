package pipeline

import (
	"encoding/json"

	"github.com/sumwai/larkax/internal/billing"
	"github.com/sumwai/larkax/internal/channel"
)

// CachePrefixOf 把一次请求折成缓存前缀的块序列。
//
// 块按上游看到的顺序排，粒度是：
//
//	system 消息        → 一块（多条 system 消息合在这一块里）
//	tools 定义         → 一块
//	其余每条 message   → 各一块
//
// # 为什么是块，而不是整段一份摘要
//
// 前缀比对要回答的是「上次那一段是不是这次的开头」，而两次请求的差别通常在
// 末尾：多了一条 assistant 回复、多了一轮 tool_result。整段一份摘要只能回答
// 「是否完全相同」，答不了「前 N 块是否相同」；按块算才能让 [A, B] 成为
// [A, B, C] 的前缀。
//
// 也不按单个 token 比：那对 tokenizer 差异过于敏感，而这里比的是**内容**，
// 内容没变就该算命中。
//
// # 算不出来时返回 nil
//
// 序列化失败在当前类型上构造不出来（都是从 JSON 解出来的值），但真走到那里
// 时返回值必须是「没有前缀」而不是「一个较短的前缀」：拿不完整的序列去比，
// 有可能把没命中的判成命中，方向是漏收。nil 让预留退回按整段输入估。
func CachePrefixOf(req *channel.Request) []billing.BlockHash {
	if req == nil {
		return nil
	}

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
