package tokens

// 本文件是「按模型计数」的入口：先看有没有词表，有就精确算，没有就估算。
//
// 两条路径的差别只体现在 Source 上：精确路径给 SourceTokenized，
// 估算路径给 SourceEstimated。调用方从 Source 就能知道该怎么用这个数。

import (
	"context"
	"errors"
)

// CountText 计数一段文本。
//
// model 是上游模型名（也可以是规范名）——族与编码都从它推导。
// 传空字符串等价于 FamilyUnknown，走估算。
//
// 这是最常用的入口。需要注意它**只算这一段文本**：消息框架
// （role 标记、分隔符）、工具定义的 JSON 包装都不在里面。
// 要算一次完整请求的输入，用 CountRequest。
func (c *Counter) CountText(ctx context.Context, model, text string) (Count, error) {
	if text == "" {
		return Count{Tokens: 0, Source: SourceTokenized, Model: model}, nil
	}
	if err := ctx.Err(); err != nil {
		return Count{}, err
	}

	fam := FamilyOf(model)
	if fam == FamilyOpenAI {
		if n, ok := c.countWithVocab(model, text); ok {
			return Count{Tokens: n, Source: SourceTokenized, Model: model}, nil
		}
		// 词表不可用（例如内存受限环境里加载失败）——退到估算而不是报错。
		// 报错会让「词表坏了」升级成「服务不可用」，而估算给出的数字
		// 虽然有偏差但仍然可用，且 Source 会如实标注。
	}
	return Count{
		Tokens: int64(estimateRounded(text, tableFor(fam))),
		Source: SourceEstimated,
		Model:  model,
	}, nil
}

// countWithVocab 尝试走精确路径。ok 为 false 表示没有可用的词表。
func (c *Counter) countWithVocab(model, text string) (int64, bool) {
	if c == nil || c.vocab == nil {
		return 0, false
	}
	codec, err := c.vocab.codecFor(encodingForModel(model))
	if err != nil {
		return 0, false
	}
	n, err := codec.Count(text)
	if err != nil {
		// 分词失败（输入里有无法匹配的片段等）。退到估算，不静默返回 0——
		// 返回 0 会让这段文本在账上表现为「没有输入」。
		return 0, false
	}
	return int64(n), true
}

// CountTextEstimated 强制走估算路径。
//
// 用途是当调用方**知道**某家换了词表而我们还没跟上时（例如上游公告了
// 新的 tokenizer），可以显式避开本地词表，以免给出一个看似精确、
// 实则基于过期词表的数字。那种错误比明说是估算更危险。
func (c *Counter) CountTextEstimated(model, text string) Count {
	return Count{
		Tokens: int64(estimateRounded(text, tableFor(FamilyOf(model)))),
		Source: SourceEstimated,
		Model:  model,
	}
}

// RecordMeasured 把上游给的实测值包成计数结果。
//
// 它不是「计数」而是「记账」：上游的 usage 已经是权威数字，本包要做的
// 只是给它打上正确的来源标记。放在这里是因为来源标记属于本包的职责——
// 让调用方自己构造 Count 就等于把「别忘了标来源」交给每个调用点。
//
// tokens 为负时按 0 处理并返回错误：负 token 只可能来自解析错误，
// 放过去会算出一笔负数消费。
func RecordMeasured(model string, tokens int64) (Count, error) {
	if tokens < 0 {
		return Count{}, errors.New("实测 token 数为负，疑似解析错误")
	}
	return Count{Tokens: tokens, Source: SourceMeasured, Model: model}, nil
}

// estimateRounded 把浮点估算取整。
//
// 向上取整而不是就近取整：方向与「估算一律偏高」一致，且能保证
// 极短文本（估出 0.4 个 token）也得到 1 而不是 0——
// 给非空文本算出 0 个 token 会让它在账上消失。
func estimateRounded(text string, t weightTable) float64 {
	v := estimate(text, t)
	if text == "" {
		return 0
	}
	if v < 1 {
		return 1
	}
	return ceil(v)
}

// ceil 是最小的大于等于 v 的整数。
//
// 不用 math.Ceil：这里的 v 都是非负有限值，而引入 math 只为这一处不值得。
func ceil(v float64) float64 {
	i := float64(int64(v))
	if i < v {
		return i + 1
	}
	return i
}

// EstimatedTokens 是 estimateRounded 的导出形式，供需要自己拼装文本
// 再估算的调用方使用（例如把一个请求的多段内容拼起来）。
//
// 用 model 而不是 Family，是为了让调用方的写法与 CountText 一致，
// 不必自己去判断族。
func EstimatedTokens(model, text string) int64 {
	return int64(estimateRounded(text, tableFor(FamilyOf(model))))
}

// BoundsOf 返回某个模型走估算路径时的误差范围。
//
// 它是对 BoundsFor(FamilyOf(model)) 的便捷包装：调用方手里通常有模型名
// 而不是族，让它们各写一遍 FamilyOf 迟早会有人漏掉。
func BoundsOf(model string) ErrorBounds { return BoundsFor(FamilyOf(model)) }
