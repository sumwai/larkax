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
// 单独一个函数而不是内联：pipeline 与 billing 在「预留上界」那一步也会拒绝
// 同一件事（max_tokens 非正）。那两处的措辞与哨兵错误与本包**并不相同**——
// 那边是「当前 %d」+ billing.ErrNoUpperBound，这边是「收到 %d」+ ErrPatchScope
// ——排查时别把同一件事当成两个不同的问题。
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

// appliedBodyFields 是请求体补丁里**会被应用**的字段。
//
// 契约认得 8 个（见 RequestBodyPatch 的字段表），应用这一步只写这 3 个；
// 另外 5 个一律拒绝，见 unappliedFields。
//
// 为什么不做的更宽松（允许改任意 JSON 字段）：请求体里有 `model` 与 `stream`，
// 前者决定价格、后者决定「已吐字之后还能不能换渠道」这条硬约束。
// 一张白名单比「记得过滤那两个字段」可靠。
//
// 这份清单也用在报错文本里（「会应用的只有 …」），所以它必须与
// unappliedFields 的判定拼得回完整的契约字段表——
// TestBodyPatchFieldTableMatchesContract 拿两者的并集与 RequestBodyPatch
// 的 json 标签对一遍。
//
// `max_tokens` 在这里，但**只许调小**（见 applyBodyToChannel）：把输出压短
// 是策略插件的正当用法，而调大会把计费预留
// 的上界一起抬高。它在这一层有校验，只是方向只有一个——别把它当成漏了校验
// 而顺手放开，那正好是「上界与上报值由同一方控制」那条路径。
var appliedBodyFields = []string{
	"max_tokens",
	"temperature",
	"top_p",
}

// unappliedFields 返回补丁里给了、而应用这一步**不会写下去**的字段名，按契约顺序。
//
// # 为什么是拒绝，而不是留在补丁里
//
// 这 5 个字段（messages / system / stop / tools / tool_choice）以前是「结构化的
// 那几项留在补丁里，由调用方按自己的需要决定」，而应用那一步既不写它们、
// 也不返回错误：插件作者看到的是「我的插件没生效」，与字段名拼错
// （decodePatch 报的那个错）在观测上完全一样，而那正是契约里最难查的一类静默。
// **有意的是不应用，不是静默。**
//
// 不应用的理由：它们在上游请求体里是结构化数据，而 channel.Request 用的是
// 自己的类型（[]Message、[]Tool）。从插件的 JSON 视图反解回那些类型需要一份
// 「JSON → 结构化消息」的解析器，而那份解析器的每一条规则都是一个新的契约面
// （插件能不能改 tool_use 的 id？能不能插一条 system 消息？）。那是权限边界，
// 要单独拍板，不属于「补丁能改什么」这一条。
//
// # 判据只写一份
//
// 请求路径上只有 adapter.go 的 applyBodyToChannel 一条应用路径，它调这里；
// 校验不另写一份——分两处写的话，改了其中一处、留着一处，同一个补丁就有
// 两个判定。
func (p *RequestBodyPatch) unappliedFields() []string {
	if p == nil {
		return nil
	}
	var out []string
	for _, item := range []struct {
		name  string
		given bool
	}{
		{"messages", p.Messages != nil},
		{"system", p.System != nil},
		{"stop", p.Stop != nil},
		{"tools", p.Tools != nil},
		{"tool_choice", p.ToolChoice != nil},
	} {
		if item.given {
			out = append(out, item.name)
		}
	}
	return out
}

// errBodyFieldsNotApplied 是补丁里出现「认得但不应用」的字段时的错误。
//
// 报错**点出是哪几个字段**：只说「补丁不符合契约」的话，作者手上那份补丁里
// 有 8 个字段可选，他得逐个试。字段名按契约顺序列出（不是排过序的），
// 与 RequestBodyPatch 的字段表、与文档里的那一节是同一个顺序。
func errBodyFieldsNotApplied(fields []string) error {
	return fmt.Errorf("%w: 请求体补丁里的 %s 不会被应用（会应用的只有 %s），请把它们从补丁里去掉",
		ErrPatchScope, strings.Join(fields, "、"), strings.Join(appliedBodyFields, "、"))
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
// 四种情形共用这一个哨兵：
//
//   - 字段根本不存在，名字拼错了（`channeId`）——由各补丁类型的 UnmarshalJSON
//     报出，见 decodePatch；
//   - 字段存在，但插件不许改（请求体里的 `model`）；
//   - 字段对而值不能用（`max_tokens` 非正）；
//   - 字段与值都合法，但应用这一步不写它（`messages` 等 5 个，见
//     unappliedFields）。
//
// 它们在实现上是同一件事：**这份补丁不能原样收下**，而作者要做的动作也一样，
// 都是回去改插件。分成四个哨兵只会让同一类错误有四个名字，
// 调用方要同时判四个才算判全。
//
// 与「插件跑挂了」分开：这是**契约违规**，要在上传期或首次运行时就被看见，
// 而不是当成一次普通的插件失败悄悄丢掉。
//
// 报错文本是三者的上界，所以它只说「不符合契约」而不说具体哪一条：
// 具体原因在被包住的那一层（`json: unknown field "channeId"`、
// `max_tokens 必须为正，收到 0`），那里才说得准。
var ErrPatchScope = errors.New("插件给的补丁不符合契约")

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
