package plugin

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// errNonPositiveMaxTokens 是 max_tokens 非正时的错误。
//
// 单独一个函数而不是内联：pipeline 的校验用同一句话（它是计费预留上界的
// 输入），两处说法的差别会让排查时以为是两个不同的问题。
func errNonPositiveMaxTokens(v int64) error {
	return fmt.Errorf("%w: max_tokens 必须为正，收到 %d", ErrPatchScope, v)
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

// ErrPatchScope 表示插件改了一个它不该改的东西。
//
// 与「改错了值」分开：越界是**契约违规**，要在上传期或首次运行时就被看见，
// 而不是当成一次普通的插件失败悄悄丢掉。
var ErrPatchScope = errors.New("插件试图修改超出权限的字段")

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
// 这里只做这一层的规则（前缀 + 值里不能有换行）。适配器还会再过一道
// 更严的过滤（SafeRequestHeaders），那是最后一道防线。
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
		case strings.ContainsAny(value, "\r\n"):
			// 值里带换行等于能注入出额外的头。
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
