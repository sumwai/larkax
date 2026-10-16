// Package pricing 从 pricing_rules 表里取出一次请求适用的计价规则。
//
// 它是 pipeline.RuleSource 的实现，但**不 import 计价引擎**：接口由
// pipeline 声明、由本包用结构满足（两边都不必看见对方）。本包只回答
// 「这次请求按哪条规则计价」，不做求值、不碰钱——求值在
// internal/billing/expr，乘用量与写分录在 internal/pipeline。
//
// # 为什么单独一层
//
// 规则的匹配与优先级裁决是 SQL 的活（作用域、生效时间、优先级、模型过滤），
// 而 pipeline 只关心「拿到一条能求值的规则」。把这段 SQL 塞进 pipeline
// 会让那一层必须连库才能构造，而它现在能脱离数据库测试。
//
// # 匹配的判据与次序
//
// 一条规则要被选中，先得同时满足四件事：启用、在生效时间窗口内、适用于
// 这个模型、适用于这次请求的作用域。满足之后按下面的次序取第一条：
//
//  1. 作用域的具体程度：channel > merchant > platform
//  2. priority 大的优先
//  3. id 大的优先（同一作用域同一优先级下取最新的一条）
//
// 第 1 条排在最前面，是因为它表达的是「谁定的价」：渠道自己定的价是
// 这个渠道的价，商家定的价是这个商家的价，平台价是兜底。让 priority
// 压过作用域会让一个平台规则（priority 很大）盖住渠道自己的价——
// 那是把「平台统一定价」变成了「平台随时可能悄悄改掉商家的价」。
// priority 用来在**同一层**里决胜，这个次序与 pricing_rules_lookup_idx
// 的列顺序一致。
//
// 第 3 条是决胜用的，不表达偏好：没有它，两条同作用域同优先级的规则
// 谁被选中由行的物理顺序决定，同一份数据两次查询可能给出不同的价。
package pricing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/sumwai/larkax/internal/pipeline"
)

// Source 按条件从 pricing_rules 表里选出一条规则。
type Source struct {
	db *sql.DB
}

// New 包装一个已打开的数据库句柄。
func New(db *sql.DB) *Source { return &Source{db: db} }

// 编译期断言：它要满足 pipeline 对规则来源的要求。
//
// 写成内联接口而不是 import 一个别处的声明：本包与 pipeline 之间
// 只有这一个方法，多一层声明只会多一处要跟着改的地方。
var _ interface {
	Match(context.Context, pipeline.RuleQuery) (pipeline.Rule, error)
} = (*Source)(nil)

// matchQuery 是选规则的查询。
//
// 它一次查完，不先按作用域查一遍再回退：规则表不大，而「查了平台规则
// 发现有条渠道规则」这类往返会让热路径上的数据库往返数随作用域层数增长。
//
// $1 是匹配时刻，$2 是规范模型名，$3 是渠道所属商家（0 表示未选定），
// $4 是渠道（0 表示未选定）。
//
// # 为什么「未选定」不需要额外分支
//
// 预留发生在选路之前，那时两个 id 都是 0（见 pipeline.RuleQuery）。而
// owner_user_id 与 channel_id 是外键，真值都大于 0，所以 `= 0` 匹配不到
// 任何一行——未选定这件事由数据本身表达，不需要在 SQL 里再写一遍
// `$3 > 0 AND ...`。后者看着更明确，但它把「0 不是合法 id」这条事实
// 抄到了第二个地方。
//
// # 时间窗口是左闭右开
//
// effective_from <= at < effective_to。上界取开区间，于是「把旧规则的
// effective_to 设成新规则的 effective_from」不会让两条规则在同一瞬间
// 同时生效——那正是交接时最容易出账的时刻。
//
// # 模型过滤是精确匹配
//
// 空数组表示适用于全部模型；否则要求模型名**逐字**在数组里
// （jsonb 的 `?` 运算符，它按元素匹配，不是子串匹配）。
// 通配（`claude-*`）目前不做：`?` 会把它当成一个普通字符串，
// 于是配了通配的规则**一条都匹配不上**，表现为 ErrNoRule 而不是
// 「按某条规则少收了钱」。宁可在这里失败，也不要静默地按错的价算。
const matchQuery = `
	SELECT r.id, r.expr
	  FROM pricing_rules r
	 WHERE r.enabled
	   AND r.effective_from <= $1
	   AND (r.effective_to IS NULL OR r.effective_to > $1)
	   AND (jsonb_array_length(r.model_patterns) = 0
	        OR (r.model_patterns ? $2))
	   AND ((r.scope = 'platform' AND r.owner_user_id IS NULL)
	        OR (r.scope = 'merchant' AND r.owner_user_id = $3)
	        OR (r.scope = 'channel' AND r.channel_id = $4))
	 ORDER BY CASE r.scope
	            WHEN 'channel' THEN 3
	            WHEN 'merchant' THEN 2
	            ELSE 1
	          END DESC,
	          r.priority DESC,
	          r.id DESC
	 LIMIT 1`

// Match 返回这次请求适用的规则。
//
// 没有匹配的规则时返回 pipeline.ErrNoRule，**不返回一条零价的兜底规则**：
// 那会让「没配价」表现为「免费」，而免费是最不容易被发现的错账
// （见 pipeline.ErrNoRule 的注释）。调用方对它的处置应当是拒绝服务并告警。
func (s *Source) Match(ctx context.Context, q pipeline.RuleQuery) (pipeline.Rule, error) {
	if q.CanonicalModel == "" {
		return pipeline.Rule{}, errors.New("规范模型名为空")
	}

	// 查询没给时刻时用当前的：生效时间窗口必须有一个判据，
	// 而零值时刻（公元 1 年）会让所有规则都还没生效。
	at := s.now(q)

	var (
		id   int64
		expr string
	)
	err := s.db.QueryRowContext(ctx, matchQuery, at, q.CanonicalModel, q.OwnerUserID, q.ChannelID).
		Scan(&id, &expr)
	if errors.Is(err, sql.ErrNoRows) {
		return pipeline.Rule{}, fmt.Errorf("%w: 模型 %q 在渠道 %d 下没有生效的计价规则",
			pipeline.ErrNoRule, q.CanonicalModel, q.ChannelID)
	}
	if err != nil {
		return pipeline.Rule{}, fmt.Errorf("查询计价规则失败: %w", err)
	}

	return pipeline.Rule{ID: id, Source: expr, Version: versionOf(expr)}, nil
}

// versionOf 由表达式源码算出一个版本号。
//
// # 为什么是内容哈希，而不是表里的某个数
//
// Version 唯一的用途是编译缓存的键（见 pipeline.Rule.CacheKey）：同一条
// 规则被改过之后必须换一个键，否则取到的还是旧表达式的编译产物，而那种
// 故障表现为「改了规则但没生效」——排查方向完全错（见 Rule.Version 的注释）。
//
// 表里能拿来做版本的有两处，都不能兑现这条：pricing_rule_versions 需要
// 每次改规则都插一行，而那是写入方的义务，漏一次就永久错位；updated_at
// 依赖写入方自己记得 SET，而表上并没有触发器替它兜底。两者都是「改了
// 数据但版本没变」，而内容哈希把版本**定义**成源码的函数：源码变了，
// 键就一定变，与谁用什么方式写进去的无关。
//
// 碰撞（两份不同的源码得到同一个哈希）在这个用途下无害：它只会让两条
// 规则共用一份编译产物，而那正是「表达式相同」时该发生的事。
func versionOf(expr string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(expr))
	return int64(h.Sum64())
}

// now 取匹配用的时刻：查询给了就用它，没给（零值）才用当前时间。
//
// 不留一个可注入的时钟字段：调用链那一侧（pipeline）本来就是自己算出时刻
// 再传下来的，测试要钉窗口边界就传 q.At——两处时钟只会让人分不清谁说了算。
func (s *Source) now(q pipeline.RuleQuery) time.Time {
	if !q.At.IsZero() {
		return q.At
	}
	return time.Now()
}
