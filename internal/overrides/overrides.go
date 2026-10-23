// Package overrides 管 model_aliases.overrides 这一列：商家给一条别名配的
// 一组上游参数覆盖。
//
// # 同一份白名单有三个用户，而它们分处三个包
//
//   - **写入校验**：internal/catalog 的管理面写入路径，写库之前拒掉不允许的键
//   - **热路径应用**：internal/route 在按渠道翻出上游名之后，把覆盖写到那份
//     请求副本上
//   - **预留上界**：internal/gateway 在预留之前，取候选集上覆盖之后的最大输出
//
// 判据写在其中一个包里，另外两处就得各抄一份。而「白名单里有这个键、热路径
// 不认它」的表现，正好是这一列此前的问题：配了却看不出为什么没生效。
//
// # 与插件补丁那一套的关系
//
// 插件补丁（internal/plugin 的 RequestBodyPatch）也改 max_tokens / temperature
// / top_p 这几个字段，判据却各自一份，这是有意的：补丁来自**第三方代码**，
// 它的边界要按「越权」来设计（所以它连 messages 都给得出来、由应用层拒）；
// 覆盖来自**商家自己的配置**，边界按「配置写错了没有」来设计，白名单之外一律
// 收下不了。两者今天在可改字段上恰好重合，但重合不是同一条判据，所以没有合并。
package overrides

import (
	"fmt"
	"math"
	"slices"
	"sort"

	"github.com/sumwai/larkax/internal/channel"
)

// 允许覆盖的键。
//
// # 为什么是白名单
//
// 拒绝清单永远漏：请求体里能改的东西比列出来的多（system、stop、
// tool_choice、各家自己的私有参数……），漏掉一个的后果是它悄悄生效了。
// 白名单下漏掉一个的后果是它不生效，而且**写入时就会报错**说明原因。
//
// # 这三项为什么可以覆盖
//
// 它们是采样类参数：改它们只影响这次回答怎么算出来，不改变请求的形状
// ——不问谁在说话、不问哪一段是用户输入、不增减工具。改形状的那几个键
// 见 forbidden。
//
// 往这里加键之前要先回答一句：**热路径认得它吗**？白名单里有它而 Apply
// 不认，这个键就会变成一条配了不生效的配置，而写入时还会报「成功」。
// 这条对应关系由 overrides_test.go 的 TestEveryAllowedKeyApplies 钉着。
const (
	keyTemperature = "temperature"
	keyTopP        = "top_p"
	keyMaxTokens   = "max_tokens"
)

// allowedKeys 是白名单**本身**：准入判据就是它，`parse` 先按它拦一遍。
// 顺序只用于报错文本，那份清单由它推出来、不另写一份字面量——手写的那份与
// 这里会各自漂移，而漂移的表现只是「报错里少列了一个本来能用的键」。
var allowedKeys = []string{keyTemperature, keyTopP, keyMaxTokens}

// forbidden 是**显式**拒绝的键，以及拒绝它的理由。
//
// 白名单本身就已经把它们挡住了；单独列一遍是为了报错时说得出**为什么**。
// 「不在允许的字段里」与「它是请求形状的一部分，改了等于让商家改写调用方的
// 请求」是两句不同的话，而配这个键的人需要知道的是后一句。
var forbidden = map[string]string{
	"model":    "它决定这次请求发给哪个模型，而选路与计价都按入站解析之后的规范名走",
	"messages": "它是调用方发来的正文",
	"tools":    "它是调用方声明的工具集",
	"stream":   "它决定这条响应是不是流式的，而流式与非流式的失败边界不同",
}

// Validate 检查一份覆盖能不能收下。
//
// 写入路径在写库之前调它。热路径也会走到它（见 Apply）——库里可能有绕过写入
// 校验写进去的历史行，而那种行的处置与配置写错了一样：说出来，不静默跳过。
func Validate(v map[string]any) error {
	_, err := parse(v)
	return err
}

// Apply 把覆盖写到一份请求上。
//
// # 只许写在副本上
//
// 就地改传进来的那份请求，包括给它换新的指针（temperature / top_p 是
// *float64，改的是指针本身而不是它指向的值）。热路径传的是**这次尝试自己的
// 那份副本**（见 internal/route 的 upstreamRequest），所以调用方那份原始请求
// 不会被牵动；直接传调用方那份进来就等于就地改了它。
//
// # 谁赢
//
// 除 max_tokens 之外**调用方赢**：调用方显式给了的值就是这次请求的意图，
// 覆盖掉它会让「我设的 temperature 没生效」变成一件无法解释的事——而这类
// 现象正是这一列此前最难查的地方（配了跟没配一样，还看不出来）。
//
// max_tokens 反过来，覆盖值直接生效。它是上界，商家限制它是在保护自己的成本；
// 而它一旦能比调用方给的那个数更大，预留上界就必须跟着变大——见
// EffectiveMaxTokens。
func Apply(req *channel.Request, v map[string]any) error {
	vals, err := parse(v)
	if err != nil {
		return err
	}
	if vals.hasTemperature && req.Temperature == nil {
		t := vals.temperature
		req.Temperature = &t
	}
	if vals.hasTopP && req.TopP == nil {
		p := vals.topP
		req.TopP = &p
	}
	if vals.hasMaxTokens {
		req.MaxTokens = vals.maxTokens
	}
	return nil
}

// EffectiveMaxTokens 返回覆盖之后这次请求允许的最大输出。
//
// 预留上界按它算。理由：覆盖值可以比调用方给的那个数更大，那时实际输出可能
// 超过按调用方那个数算出来的上界，结算就会撞上界变成少收——而「撞上界」本该是
// 上界估错了的信号，不该由一条别名配置制造出来。
//
// 非法覆盖与 Apply 一样报错，不悄悄退回调用方那个数：那会让一次配置错误表现成
// 「覆盖没生效」，而这正是这一列要消掉的东西。
func EffectiveMaxTokens(callerMaxTokens int, v map[string]any) (int, error) {
	vals, err := parse(v)
	if err != nil {
		return 0, err
	}
	if !vals.hasMaxTokens {
		return callerMaxTokens, nil
	}
	return vals.maxTokens, nil
}

// values 是一份覆盖解出来的各项。
//
// 每一项都带一个「给了没有」：**给了 0 与没给**必须分得开（temperature 的 0
// 是合法取值，而 max_tokens 的 0 非法但同样不能当成没给）。
type values struct {
	hasTemperature bool
	temperature    float64
	hasTopP        bool
	topP           float64
	hasMaxTokens   bool
	maxTokens      int
}

// parse 解出一份覆盖里的各项，键名与取值都在这里判。
//
// 三个用户（Validate / Apply / EffectiveMaxTokens）共用它：**认不认这个键、
// 值收不收得下**只有一份实现。
func parse(v map[string]any) (values, error) {
	var out values

	// 先排序再遍历：map 的遍历顺序是随机的，而报错文本要能逐字比较（用例断言
	// 的是「错误里说得出是哪个键」）。多个非法键时随机挑一个报，会让同一条
	// 配置两次给出不同的消息。
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		// 准入判据在这一句，不在下面的 switch：白名单是唯一的那条，而 switch
		// 只管「这个键怎么解析」。两者曾经各自判一次，于是「switch 里加了
		// case、白名单里忘了加」的表现是那个键悄悄生效、报错文本里却不列它。
		if !slices.Contains(allowedKeys, k) {
			return values{}, unknownKeyError(k)
		}
		switch k {
		case keyTemperature:
			f, err := number(k, v[k])
			if err != nil {
				return values{}, err
			}
			out.hasTemperature, out.temperature = true, f
		case keyTopP:
			f, err := number(k, v[k])
			if err != nil {
				return values{}, err
			}
			out.hasTopP, out.topP = true, f
		case keyMaxTokens:
			n, err := positiveInt(k, v[k])
			if err != nil {
				return values{}, err
			}
			out.hasMaxTokens, out.maxTokens = true, n
		default:
			// 白名单里有它、这里没有 case：加键时漏了一处。这一支今天走不到
			// （上面那句已经按白名单拦过），留着是因为它反过来钉一件事——
			// 真漏了的时候要报出来，而不是当成「不允许这个键」。
			return values{}, fmt.Errorf("overrides 的 %q 在白名单里，但解析没实现（本包的缺陷）", k)
		}
	}
	return out, nil
}

// unknownKeyError 给出「这个键不能覆盖」的说明。
//
// 显式拒绝的那几个键带上理由（见 forbidden），其余的说清白名单是什么。两种
// 情况的下一步动作不一样：前者要换个思路（这个参数不该从这里配），后者多半
// 是键名写错了。
func unknownKeyError(key string) error {
	if why, ok := forbidden[key]; ok {
		return fmt.Errorf("overrides 不允许覆盖 %q：%s（允许覆盖的只有 %s）",
			key, why, allowedList())
	}
	return fmt.Errorf("overrides 里的 %q 不在允许覆盖的字段里（允许覆盖的只有 %s）",
		key, allowedList())
}

// allowedList 把白名单拼成报错文本里那一串。
func allowedList() string {
	out := ""
	for i, k := range allowedKeys {
		if i > 0 {
			out += "、"
		}
		out += k
	}
	return out
}

// number 取一个数字型的覆盖值。
//
// 认得所有数字类型，不写成「只认 float64」：这一列的值走一遍 jsonb 之后确实
// 一律是 float64（encoding/json 把 JSON 数字都解成它），但直接调管理面写入路径
// 的 Go 代码传的是 int——让那种调用方拿到一句「不是数字」是纯粹的意外。真正要
// 拒的是**非数字**（字符串、布尔、null），因为那种值解出来既不是采样参数也不是
// 上限，而把它当成「没给」就等于这条配置静默失效。
func number(key string, raw any) (float64, error) {
	var f float64
	switch v := raw.(type) {
	case float64:
		f = v
	case float32:
		f = float64(v)
	case int:
		f = float64(v)
	case int8:
		f = float64(v)
	case int16:
		f = float64(v)
	case int32:
		f = float64(v)
	case int64:
		f = float64(v)
	case uint:
		f = float64(v)
	case uint8:
		f = float64(v)
	case uint16:
		f = float64(v)
	case uint32:
		f = float64(v)
	case uint64:
		f = float64(v)
	default:
		return 0, fmt.Errorf("overrides 里的 %q 不是数字（是 %T）", key, raw)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("overrides 里的 %q 不是有限数（%v）", key, f)
	}
	return f, nil
}

// maxExactInt 是 float64 能**精确**表示的最大整数（2^53）。
//
// 它是一条表示能力上的界，不是一条策略上的上限：超过它的 float64 已经不是一个
// 确定的整数了（2^53+1 与 2^53 在 float64 下相等），转成 int 会给出一个谁都没
// 配过的数。真正「太大」的判断该由模型的上限来做，不在这里。
const maxExactInt = float64(1 << 53)

// positiveInt 取一个正整数型的覆盖值。
//
// max_tokens 必须为正：0 或负数算不出预留上界（下游会以「没有上界」拒绝），
// 而这条配置会让那个别名上的**每一个**请求都失败——写入时就该拦住。
func positiveInt(key string, raw any) (int, error) {
	f, err := number(key, raw)
	if err != nil {
		return 0, err
	}
	if f != math.Trunc(f) {
		return 0, fmt.Errorf("overrides 里的 %q 必须是整数，当前 %v", key, f)
	}
	if f <= 0 {
		return 0, fmt.Errorf("overrides 里的 %q 必须为正，当前 %v：0 或负数算不出预留上界", key, f)
	}
	if f > maxExactInt {
		return 0, fmt.Errorf("overrides 里的 %q 超出可精确表示的整数范围（%v > 2^53）", key, f)
	}
	return int(f), nil
}
