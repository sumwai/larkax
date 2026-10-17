package snapshot

import "sort"

// Surface 是一套线格式：允许表按它建，脱敏器按它选表。
//
// # 它与 Root 是两条正交的轴
//
//   - **Surface 决定用哪张允许表**。同一个方向在不同表面上字段完全不同：
//     chat 那条是 messages / tools / usage，决策那条（internal/systemone）
//     是候选集合与选中项，两者没有一个字段是共用的。
//   - **Root 决定落库的 direction**。`request_snapshots.direction` 上的 CHECK
//     只认 request / response 两个取值，而决策那条表面同样有请求与响应两个
//     方向，所以两张表**共用这两个方向取值**，靠骨架里的一个顶层标量区分
//     是哪条表面产出的（见 internal/snapshotwire 的适配层）。
//
// 把两者合成一个维度会撞上那个 CHECK：多加一个 direction 取值就要一条迁移，
// 而这条链路的定位是排障辅助，不值得为它动 schema。反过来说，也**不要**
// 因为「表面不同」就新开一个方向取值——同一件事不要有两个答案。
type Surface string

// 两个已知表面。
const (
	// SurfaceChat 是客户端对话那条线（channel.Request / channel.Response）。
	SurfaceChat Surface = "chat"
	// SurfaceDecision 是决策那条线（internal/systemone 的 Request / Response）。
	SurfaceDecision Surface = "decision"
)

// surfaceValues 是全部已知表面，取值清单与表分开声明。
//
// 与 rootValues 同一个理由：内联比较没法被遍历，而「清单里有、表里没有」
// 这件事只有把两份放在一起走一遍才看得见（见 TestRootValuesMatchSchema）。
var surfaceValues = []Surface{SurfaceChat, SurfaceDecision}

// surfaces 把表面映射到「方向 → 允许表」。
//
// 每个表面的两个方向都必须有表。少一张时 NewFor 在构造期就拒绝，
// 而不是让那一侧静默落到另一张表上——落错表在快照上是**静默**的，
// 现场表现为「一批本该脱敏的字段恰好没被脱敏」，而它看起来像配置问题。
var surfaces = map[Surface]map[Root][]fieldRule{
	SurfaceChat:     allowlists,
	SurfaceDecision: decisionAllowlists,
}

// Surfaces 返回全部已知表面，顺序固定。
//
// 供包外的遍历使用（命令行打印允许表、审计时逐张走一遍）。顺序固定是为了让
// 「同一份代码两次运行给出同一份输出」不依赖 map 的遍历顺序。
func Surfaces() []Surface {
	out := append([]Surface(nil), surfaceValues...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// NewFor 按表面与方向建立一个脱敏器。
//
// 未知表面、未知方向与「表本身有问题」（重复路径、空路径、空 Note、对具体头
// 写了放行规则）都在这里返回错误。检查放在构造期而不是脱敏期的理由见 New。
func NewFor(s Surface, root Root, opts ...Option) (*Redactor, error) {
	rules, ok := surfaces[s][root]
	if !ok {
		if _, known := surfaces[s]; !known {
			return nil, redactionError("", "未知的快照表面 %q", s)
		}
		return nil, redactionError("", "表面 %q 没有 %q 方向的允许表", s, root)
	}
	if err := validateRules(rules); err != nil {
		return nil, err
	}
	r := &Redactor{surface: s, root: root, table: buildTable(rules), maxBytes: DefaultMaxBytes}
	for _, opt := range opts {
		if err := opt(r); err != nil {
			return nil, redactionError("", "选项无效: %v", err)
		}
	}
	return r, nil
}

// AllowlistFor 返回某个表面某个方向的允许表。
//
// 返回副本，理由与 Allowlist 相同。未知表面或方向返回 nil。
func AllowlistFor(s Surface, root Root) []FieldRule {
	return exportRules(surfaces[s][root])
}
