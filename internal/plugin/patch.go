package plugin

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/sumwai/larkax/internal/channel"
)

// errNonPositiveMaxTokens 是 max_tokens 非正时的错误。
//
// 单独一个函数而不是内联：pipeline 与 billing 在「预留上界」那一步会拒绝
// 同一件事（max_tokens 非正）。三处的措辞与哨兵错误**并不相同**——那边是
// 「当前 %d」+ billing.ErrNoUpperBound，这边是「收到 %d」+ ErrPatchScope——
// 排查时别把同一件事当成两个不同的问题。
//
// 这一层还有**另一件**关于 max_tokens 的事：只许调小（见 errMaxTokensRaised）。
// 两者的哨兵错误相同（都是 ErrPatchScope），所以判错时不能只看 errors.Is：
// 一个说这个值本身不成立，另一个说这个值本身没问题、相对原值不行。
func errNonPositiveMaxTokens(v int64) error {
	return fmt.Errorf("%w: max_tokens 必须为正，收到 %d", ErrPatchScope, v)
}

// errMaxTokensRaised 是插件试图抬高输出上界时的错误。
//
// 为什么只许调小：预留的上界（billing.Reserve）主项是
// max_tokens × max(输出单价, 推理单价)，而结算会把实际金额钳到预留。
// 插件既能把上界抬高、又能照着抬高后的上界报用量时，两者一起放大就等于
// 没有上界——报多少收多少。压短输出是策略插件的正当用法，抬高不是。
//
// hasOriginal 为 false 表示原请求根本没给 max_tokens。那算**新增**一个上界，
// 与调大是同一件事：上界从无到有仍然由插件决定，而「上界从哪来」正是
// 这条规则要管的东西（见 UsageBound）。能读到原值时两个数都打进文本里，
// 因为「只许调小」这句话本身不告诉你原来是多少。
func errMaxTokensRaised(original int64, hasOriginal bool, patched int64) error {
	if !hasOriginal {
		return fmt.Errorf("%w: max_tokens 只许调小，但原请求没有给 max_tokens（补丁给了 %d）",
			ErrPatchScope, patched)
	}
	return fmt.Errorf("%w: max_tokens 只许调小，原值 %d，补丁给了 %d",
		ErrPatchScope, original, patched)
}

// originalMaxTokens 从请求体里读出客户端的 max_tokens。
//
// 三种结局，调用方各有一种处置：
//
//   - has 为 true：读出一个数，拿它比；
//   - has 为 false：请求体里没有这一项，也就是原请求没给上界；
//   - 错误：有这一项但读不出来。**按「比不了」拒收**——这一层的职责是拦住
//     抬高上界的补丁，而一个读不出来的原值证明不了补丁没有抬高它。
//     放过去的话，把原值写成字符串就成了绕过这条规则的写法。
//
// patched 只用在报错文本里：读不出来时要把补丁给的值一起说出来，
// 否则那条报错看不出插件想改成多少。
//
// 认 int（RequestContextFrom 放进去的就是它）与 int64，另外认 float64 的
// 整数值：请求体一旦经 encoding/json 走过一圈，数字就是这个类型。
func originalMaxTokens(body map[string]any, patched int64) (value int64, has bool, err error) {
	raw, ok := body["max_tokens"]
	if !ok || raw == nil {
		return 0, false, nil
	}
	switch v := raw.(type) {
	case int:
		return int64(v), true, nil
	case int64:
		return v, true, nil
	case float64:
		if v == float64(int64(v)) {
			return int64(v), true, nil
		}
	}
	return 0, false, fmt.Errorf("%w: 请求体里的 max_tokens 读不出来（%T: %v），无法判断补丁给的 %d 有没有把它调大",
		ErrPatchScope, raw, raw, patched)
}

// patchableBodyFields 是插件能改的请求体字段。
//
// 这张表是**唯一**的字段白名单：不在里面的字段插件改不了，因为它根本
// 进不了 RequestBodyPatch 的结构（见它的 UnmarshalJSON）。
//
// 为什么不做的更宽松（允许改任意 JSON 字段）：请求体里有 `model` 与 `stream`，
// 前者决定价格、后者决定「已吐字之后还能不能换渠道」这条硬约束。
// 一张白名单比「记得过滤那两个字段」可靠。
//
// 这张表**不是**按协议分的：`stop` 与 `stop_sequences` 是两套协议里同一个
// 意思的不同写法，而插件只看得到一种。当前采用契约里的字段名（`stop`）。
//
// `max_tokens` 留在表里，但**只许调小**（见 ApplyRequestBodyPatch 与
// applyBodyToChannel）：把输出压短是策略插件的正当用法，而调大会把计费预留
// 的上界一起抬高。它在这一层有校验，只是方向只有一个——别把它当成漏了校验
// 而顺手放开，那正好是「上界与上报值由同一方控制」那条路径。
var patchableBodyFields = []string{
	"messages",
	"system",
	"max_tokens",
	"temperature",
	"top_p",
	"stop",
	"tools",
	"tool_choice",
}

// headerPrefix 是插件能加的请求头的名字前缀。
//
// # 这一条与仓库现有实现直接冲突，尚未裁决
//
// `internal/channel/httpx` 的 reservedHeaderPrefixes 里含 `x-larkax-`，
// 理由写在它的注释里：「自己的头也不该从这条路径进来：那是宿主写的，不是插件」。
// 也就是说，**这个前缀的头会被适配器挡掉，到不了上游**。三处说法不一致，
// 处置见 internal/plugin/header_conflict_test.go（那条测试把冲突钉成可执行的）。
//
// 这里仍然按契约实现校验（只放行这个前缀），因为「放行什么」是这一层的职责，
// 而「能不能到上游」取决于适配器最后那道过滤。等契约与适配器对齐之后，
// 这一层的行为不需要改。
const headerPrefix = "x-larkax-"

// ErrPatchScope 表示插件给的补丁不符合契约。
//
// 三种情形共用这一个哨兵：
//
//   - 字段根本不存在，名字拼错了（`channeId`）——由各补丁类型的 UnmarshalJSON
//     报出，见 decodePatch；
//   - 字段存在，但插件不许改（请求体里的 `model`）；
//   - 字段对而值不能用（`max_tokens` 非正）。
//
// 它们在实现上是同一件事：**这份补丁不能原样收下**，而作者要做的动作也一样，
// 都是回去改插件。分成三个哨兵只会让同一类错误有三个名字，
// 调用方要同时判三个才算判全。
//
// 与「插件跑挂了」分开：这是**契约违规**，要在上传期或首次运行时就被看见，
// 而不是当成一次普通的插件失败悄悄丢掉。
//
// 报错文本是三者的上界，所以它只说「不符合契约」而不说具体哪一条：
// 具体原因在被包住的那一层（`json: unknown field "channeId"`、
// `max_tokens 必须为正，收到 0`），那里才说得准。
var ErrPatchScope = errors.New("插件给的补丁不符合契约")

// ApplyRequestBodyPatch 把补丁合并进请求体。
//
// 合并是**浅合并**：给一个字段就整块替换它。深合并（例如只改 messages 里
// 某一条的 content）需要一条路径语法，那是另一个契约问题——它会让
// 「插件改了什么」变得难以从返回值看出来。
//
// body 为 nil 时新建一个 map。返回的是新 map，不改动入参：
// 上游请求体可能已经被别处引用（快照），就地改会让「当时发出去的是什么」
// 这个问题的答案随时间变化。
func ApplyRequestBodyPatch(body map[string]any, patch *RequestBodyPatch) (map[string]any, []string, error) {
	if patch == nil {
		return body, nil, nil
	}
	out := make(map[string]any, len(body)+len(patchableBodyFields))
	for k, v := range body {
		out[k] = v
	}

	var changed []string
	set := func(field string, value any) {
		out[field] = value
		changed = append(changed, field)
	}

	if patch.Messages != nil {
		set("messages", patch.Messages)
	}
	if patch.System != nil {
		set("system", *patch.System)
	}
	if patch.MaxTokens != nil {
		// max_tokens 必须为正：它是计费预留上界的关键（见 pipeline 的校验），
		// 给一个 0 或负数会让预留算不出来。
		if *patch.MaxTokens <= 0 {
			return nil, nil, errNonPositiveMaxTokens(*patch.MaxTokens)
		}
		// 而且只许调小。原值从**入参 body**里读，那是客户端的请求；
		// 读不出来或没有这一项都按「调大」处置，理由见 errMaxTokensRaised。
		//
		// 这一个比较已经把「原值缺失」也覆盖了：那时 original 是 0，而上面
		// 刚保证补丁值 ≥ 1，所以 `> original` 必然成立。**不要再写成
		// `!hasOriginal || ...`**——它读起来像一道独立的门，而删掉它不会
		// 放开任何东西（实测过），那种「看起来承重其实不承重」的条件会让
		// 下一个人对着它猜。
		original, hasOriginal, err := originalMaxTokens(body, *patch.MaxTokens)
		if err != nil {
			return nil, nil, err
		}
		if *patch.MaxTokens > original {
			return nil, nil, errMaxTokensRaised(original, hasOriginal, *patch.MaxTokens)
		}
		set("max_tokens", *patch.MaxTokens)
	}
	if patch.Temperature != nil {
		set("temperature", *patch.Temperature)
	}
	if patch.TopP != nil {
		set("top_p", *patch.TopP)
	}
	if patch.Stop != nil {
		set("stop", patch.Stop)
	}
	if patch.Tools != nil {
		set("tools", patch.Tools)
	}
	if patch.ToolChoice != nil {
		set("tool_choice", patch.ToolChoice)
	}
	sort.Strings(changed)
	return out, changed, nil
}

// ApplyHeaderPatch 校验并归并插件加的请求头。
//
// 返回放行的头与被拒的头名。**被拒的必须报出去**：插件作者写了一个被拒的头
// 却没有任何反馈，会以为是上游不支持——而真正的原因是它不在白名单里。
//
// 这里只做这一层的规则（前缀 + 值的文法）。适配器还会再过一道过滤
// （SafeRequestHeaders），那是最后一道防线。两道用的是同一个判据
// （channel.ValidHeaderValue），不是两份各自维护的规则。
func ApplyHeaderPatch(base, patch map[string]string) (map[string]string, []string) {
	if len(patch) == 0 {
		return base, nil
	}
	out := make(map[string]string, len(base)+len(patch))
	for k, v := range base {
		out[k] = v
	}

	var rejected []string
	for name, value := range patch {
		lower := strings.ToLower(strings.TrimSpace(name))
		switch {
		case lower == "":
			rejected = append(rejected, name)
		case !strings.HasPrefix(lower, headerPrefix):
			// 前缀限制是契约里明写的。它挡住的不是「危险的头」——
			// 那是适配器的活——而是「插件冒充宿主或客户端加的头」。
			rejected = append(rejected, name)
		case !validHeaderName(name):
			// 头名里的换行与空白能造出额外的头，与值里的换行是同一类问题。
			// 只查值不查名会漏掉一半：`"x-larkax-a: 1\r\nx-evil"` 作为一个
			// 名字传进来，它既是名字也带着一整个头。
			rejected = append(rejected, name)
		case !channel.ValidHeaderValue(value):
			// 值必须符合 RFC 7230 的 field-value 文法。**只挡换行是不够的**：
			// NUL 之类的控制字符会一路走到 net/http 才报错，那时它表现为
			// 「上游失败」而不是「插件写了个坏头」，可能触发换渠道重试。
			rejected = append(rejected, name)
		default:
			out[name] = value
		}
	}
	sort.Strings(rejected)
	if len(out) == 0 {
		return nil, rejected
	}
	return out, rejected
}

// validHeaderName 判断头名是否符合 HTTP 的 token 文法。
//
// 用白名单字符集而不是「不含换行」：头名的合法字符是有限的
// （RFC 7230 的 tchar），而黑名单永远可能漏掉一个能造成注入的字符。
func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}
