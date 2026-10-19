// Package pricing 从 pricing_rules 表里取出一次请求适用的计价规则，
// 并提供这些规则的管理面（创建、修改、列举，见 Admin）。
//
// 读取路径是 pipeline.RuleSource 的实现，但**不 import 计价引擎**：接口由
// pipeline 声明、由本包用结构满足（两边都不必看见对方）。它只回答
// 「这次请求按哪条规则计价」，不做求值、不碰钱——求值在
// internal/billing/expr，乘用量与写分录在 internal/pipeline。
//
// 写入路径反过来要 import 计价引擎：一条规则在落库之前必须先编译一次
// （见 Admin.Create）。差别在于失败落在哪一侧——编译不过的规则在写入这一侧
// 是一次可以当场拒绝的输入错误，在请求那一侧是一次线上故障。
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
//  3. 模型名单非空的优先（具体 > 兜底）
//  4. id 大的优先（同一作用域、同优先级、同样具体时取最新的一条）
//
// 第 1 条排在最前面，是因为它表达的是「谁定的价」：渠道自己定的价是
// 这个渠道的价，商家定的价是这个商家的价，平台价是兜底。让 priority
// 压过作用域会让一个平台规则（priority 很大）盖住渠道自己的价——
// 那是把「平台统一定价」变成了「平台随时可能悄悄改掉商家的价」。
// priority 用来在**同一层**里决胜。第 3、4 条各自表达什么、以及它们为什么
// 不必进索引，见 ruleOrderBy 上面的注释。
//
// # 成本规则是另一条线
//
// upstream_costs 记的是反方向的数——「这次请求我们花了多少」（见 cost.go 与
// cost_admin.go）。它比售价规则少一层作用域（上游成本是渠道自己的属性），
// 多一栏「按渠道分组」的次序；而窗口与模型名单那两条判据与这里**共用同一段
// 片段**，那是「两条线口径一致」唯一的守得住的做法。
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

// Source 按条件从 pricing_rules 表里选出规则。
type Source struct {
	db *sql.DB
}

// New 包装一个已打开的数据库句柄。
func New(db *sql.DB) *Source { return &Source{db: db} }

// 编译期断言：它要满足 pipeline 对规则来源的要求。
//
// 写成内联接口而不是 import 一个别处的声明：本包与 pipeline 之间
// 只有这两个方法，多一层声明只会多一处要跟着改的地方。
var _ interface {
	Match(context.Context, pipeline.RuleQuery) (pipeline.Rule, error)
	MatchAll(context.Context, pipeline.RuleQuery) ([]pipeline.Rule, error)
} = (*Source)(nil)

// applicableWindowAndModel 是两条规则线共用的那一半判据：此刻在生效窗口内，
// 且这条规则适用于这个模型。
//
// 售价规则（pricing_rules）与成本规则（upstream_costs）在这一段上逐字相同，
// 所以它只写一份：两条线拼 SQL 的地方各自抄一遍时，改了一处另一处还是老样子，
// 而它们记的是同一件事——「此刻这个模型算不算数」。另一处用法见 costApplicable。
//
// 占位符约定：$1 是匹配时刻，$2 是规范模型名。
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
// 「按某条规则少收了钱」。宁可在这里失败，也不要静默地按错的价算——
// 写入路径据此把带通配符的模式拒掉（见 Admin.Create 与 CostAdmin.Create）。
const applicableWindowAndModel = `r.effective_from <= $1
	       AND (r.effective_to IS NULL OR r.effective_to > $1)
	       AND (jsonb_array_length(r.model_patterns) = 0
	            OR (r.model_patterns ? $2))`

// modelSpecificFirst 是两条线共用的第三条裁决依据：模型名单非空的规则排在
// 兜底规则（空的 model_patterns）前面。它表达的是「具体 > 兜底」，理由见
// ruleOrderBy。
const modelSpecificFirst = `CASE WHEN jsonb_array_length(r.model_patterns) = 0 THEN 1 ELSE 0 END ASC`

// ruleApplicable 是「售价规则适用于这次查询」的判据。
//
// 它在共用的那一半（窗口与模型名单，见 applicableWindowAndModel）之外还要判
// 启用状态与作用域。作用域那三条是这一条独有的：成本规则没有作用域那一层。
//
// 四处（Match、MatchAll、Explain 与写入侧的校验）必须**逐字一致**——不一致的
// 表现是「同一个模型，预留按一条规则算、结算按另一条算」，或者「列举说这条
// 生效、请求却按另一条算」，两种都是错账。所以这里拼一次、用几处，而不是
// 靠注释提醒后来者同步修改。
//
// ownerExpr 与 channelExpr 是「这次请求的商家与渠道」那两个表达式：单点查询
// 传占位符（$3/$4），候选查询传 JOIN 进来的列（c.owner_user_id /
// c.channel_id）。各处的差别只有这一点。占位符约定：$1 是匹配时刻，
// $2 是规范模型名，调用方保证这一点。
//
// # 为什么「未选定」不需要额外分支
//
// 预留发生在选路之前，那时两个 id 都是 0（见 pipeline.RuleQuery）。而
// owner_user_id 与 channel_id 是外键，真值都大于 0，所以 `= 0` 匹配不到
// 任何一行——未选定这件事由数据本身表达，不需要在 SQL 里再写一遍
// `$3 > 0 AND ...`。后者看着更明确，但它把「0 不是合法 id」这条事实
// 抄到了第二个地方。
func ruleApplicable(ownerExpr, channelExpr string) string {
	return `r.enabled
	       AND ` + applicableWindowAndModel + `
	       AND ((r.scope = 'platform' AND r.owner_user_id IS NULL)
	            OR (r.scope = 'merchant' AND r.owner_user_id = ` + ownerExpr + `)
	            OR (r.scope = 'channel' AND r.channel_id = ` + channelExpr + `))`
}

// ruleOrderBy 是裁决次序：作用域的具体程度（channel > merchant > platform）
// → priority 大的优先 → 模型名单非空的优先 → id 大的优先。表别名必须叫 r。
//
// 第 1 条排在最前面，是因为它表达的是「谁定的价」：渠道自己定的价是
// 这个渠道的价，商家定的价是这个商家的价，平台价是兜底。让 priority
// 压过作用域会让一个平台规则（priority 很大）盖住渠道自己的价——
// 那是把「平台统一定价」变成了「平台随时可能悄悄改掉商家的价」。
// priority 用来在**同一层**里决胜。pricing_rules_lookup_idx 的列顺序
// （scope, owner_user_id, priority DESC）只用来把候选行**找出来**，不负责
// 产生这个次序：ORDER BY 的第一项是 CASE 表达式，索引序用不上，排序发生在
// 取出候选之后。第 3 条因此也不必进索引——多一列只会让写入与索引维护
// 多一份成本，而候选行数由 WHERE 与规则表的大小决定，与排序键无关。
//
// 第 3 条表达的是「具体 > 兜底」：空的 model_patterns 适用于全部模型，
// 没有这一条时，它靠第 4 条（id 大的优先）赢过每一条同作用域、同优先级的
// 模型专属规则——也就是「后加一条兜底价，所有已有的专属价一起变」，
// 且没有任何一处会提示这件事。priority 仍然压过它：想让兜底赢就把
// priority 调高，那是一次显式的改价。
//
// 这里用 jsonb_array_length 而不是 `model_patterns = '[]'::jsonb`：前者
// 与 ruleApplicable 判「空数组」用的是同一个函数，两处对「空」的口径
// 不会分叉。列是 NOT NULL，所以不必再判 NULL。
//
// 第 4 条是决胜用的，不表达偏好：没有它，两条同作用域、同优先级、
// 同样具体的规则谁被选中由行的物理顺序决定，同一份数据两次查询可能
// 给出不同的价。
//
// 四条查询共用它，于是「列举里排第一的那条」与「Match 选中的那条」是
// 同一件事——那正是列举要回答的问题，靠两份手抄的排序答不准。
const ruleOrderBy = `CASE r.scope
	          WHEN 'channel' THEN 3
	          WHEN 'merchant' THEN 2
	          ELSE 1
	        END DESC,
	        r.priority DESC,
	        ` + modelSpecificFirst + `,
	        r.id DESC`

// matchQuery 是选规则的查询。
//
// 它一次查完，不先按作用域查一遍再回退：规则表不大，而「查了平台规则
// 发现有条渠道规则」这类往返会让热路径上的数据库往返数随作用域层数增长。
//
// $1 是匹配时刻，$2 是规范模型名，$3 是渠道所属商家（0 表示未选定），
// $4 是渠道（0 表示未选定）。判据与次序见 ruleApplicable 与 ruleOrderBy。
var matchQuery = `
	SELECT r.id, r.expr
	  FROM pricing_rules r
	 WHERE ` + ruleApplicable("$3", "$4") + `
	 ORDER BY ` + ruleOrderBy + `
	 LIMIT 1`

// matchAllQuery 是选候选规则的查询：候选集里**每个候选各一条**。
//
// 与 matchQuery 是同一条判据、同一套裁决次序（同一段片段），区别只在
// 「取几条」：这里按候选分组（DISTINCT ON）各取第一条，而不是整体取一条。
//
// 一条 SQL 查完，不每个候选往返一次：候选数随渠道配置增长，而这是
// 请求路径上的一步。
//
// $1 是匹配时刻，$2 是规范模型名，$3 与 $4 是候选的渠道 id 与商家 id，
// 两个平行数组按下标对齐（unnest 的多参数形式）。
//
// # 为什么是 LEFT JOIN
//
// 一个候选可能一条规则都匹配不上。INNER JOIN 会让它整行消失，于是调用方
// 分不清「这个候选没有规则」与「这个候选不在候选集里」。LEFT JOIN 让每个
// 候选都有一行，没有规则的那行是 NULL，由调用方显式跳过。
//
// # 同一个候选出现两次
//
// DISTINCT ON 按 (channel_id, owner_user_id) 去重，重复的候选只会得到一条
// 规则。这不是省略：同一个候选适用哪条规则是确定的，问两次不会得到不同
// 的答案。
var matchAllQuery = `
	SELECT DISTINCT ON (c.channel_id, c.owner_user_id)
	       c.channel_id, c.owner_user_id, r.id, r.expr
	  FROM unnest($3::bigint[], $4::bigint[]) AS c(channel_id, owner_user_id)
	  LEFT JOIN pricing_rules r
	         ON ` + ruleApplicable("c.owner_user_id", "c.channel_id") + `
	 ORDER BY c.channel_id, c.owner_user_id, ` + ruleOrderBy

// Match 返回这次请求适用的规则。
//
// 用于结算：那时渠道已经选定，q.Candidates 恰好一个元素。多于一个元素
// 是调用方的错——把整个候选集交给结算，会让「按哪条规则结算」取决于
// 谁排在第一，而候选集本来就是要拿来挑一个的。
//
// 没有匹配的规则时返回 pipeline.ErrNoRule，**不返回一条零价的兜底规则**：
// 那会让「没配价」表现为「免费」，而免费是最不容易被发现的错账
// （见 pipeline.ErrNoRule 的注释）。调用方对它的处置应当是拒绝服务并告警。
func (s *Source) Match(ctx context.Context, q pipeline.RuleQuery) (pipeline.Rule, error) {
	if q.CanonicalModel == "" {
		return pipeline.Rule{}, errors.New("规范模型名为空")
	}
	if len(q.Candidates) != 1 {
		return pipeline.Rule{}, fmt.Errorf("匹配单条规则时候选必须恰好一个，当前 %d 个", len(q.Candidates))
	}
	cand := q.Candidates[0]

	// 查询没给时刻时用当前的：生效时间窗口必须有一个判据，
	// 而零值时刻（公元 1 年）会让所有规则都还没生效。
	at := s.now(q)

	var (
		id   int64
		expr string
	)
	err := s.db.QueryRowContext(ctx, matchQuery, at, q.CanonicalModel, cand.OwnerUserID, cand.ChannelID).
		Scan(&id, &expr)
	if errors.Is(err, sql.ErrNoRows) {
		return pipeline.Rule{}, fmt.Errorf("%w: 模型 %q 在渠道 %d 下没有生效的计价规则",
			pipeline.ErrNoRule, q.CanonicalModel, cand.ChannelID)
	}
	if err != nil {
		return pipeline.Rule{}, fmt.Errorf("查询计价规则失败: %w", err)
	}

	return pipeline.Rule{ID: id, Source: expr, Version: versionOf(expr)}, nil
}

// MatchAll 返回候选集里**每个候选各自**适用的那一条规则。
//
// 用于预留：那时还没选路，只有一个候选集，上界要按候选里最贵的那条规则
// 算（见 pipeline.Pipeline.Reserve）。裁决次序与 Match 完全相同——每个
// 候选取它自己适用的那一条，渠道级盖过商家级、商家级盖过平台级。
//
// 一个候选都匹配不上时返回空的结果，**不编一条零价的兜底规则**。
// 调用方要把「空」当成算不出上界，而不是当成零价。
func (s *Source) MatchAll(ctx context.Context, q pipeline.RuleQuery) ([]pipeline.Rule, error) {
	if q.CanonicalModel == "" {
		return nil, errors.New("规范模型名为空")
	}
	// 空候选集不去查库：一条 SQL 也不会有结果，而「空」在调用方那一侧
	// 是有意义的（算不出上界），不该被一次无谓的往返掩盖。
	if len(q.Candidates) == 0 {
		return []pipeline.Rule{}, nil
	}

	at := s.now(q)

	// 两个平行数组而不是一条 VALUES 列表：候选数不定，而 unnest 让
	// 参数个数固定为两个，与候选数无关。
	channelIDs := make([]int64, len(q.Candidates))
	ownerIDs := make([]int64, len(q.Candidates))
	for i, c := range q.Candidates {
		channelIDs[i] = c.ChannelID
		ownerIDs[i] = c.OwnerUserID
	}

	rows, err := s.db.QueryContext(ctx, matchAllQuery, at, q.CanonicalModel, channelIDs, ownerIDs)
	if err != nil {
		return nil, fmt.Errorf("查询候选计价规则失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// 从空切片开始而不是 nil：没有候选匹配到规则是正常结果，
	// 不是「没有值」。
	out := []pipeline.Rule{}
	for rows.Next() {
		var (
			channelID int64
			ownerID   int64
			id        sql.NullInt64
			expr      sql.NullString
		)
		if scanErr := rows.Scan(&channelID, &ownerID, &id, &expr); scanErr != nil {
			return nil, fmt.Errorf("读取候选计价规则失败: %w", scanErr)
		}
		// 没有匹配规则的候选在这一行上是 NULL（LEFT JOIN 的产物）。
		// 跳过它，而不是给它编一条零价规则。
		if !id.Valid {
			continue
		}
		out = append(out, pipeline.Rule{ID: id.Int64, Source: expr.String, Version: versionOf(expr.String)})
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("读取候选计价规则失败: %w", err)
	}
	return out, nil
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
// 这两处现在都有写入方了（Admin.Create / Admin.Update 会插版本行并维护
// updated_at，见 admin.go），但那**不改变这个结论**：义务仍然是义务，
// 绕过管理面的写入——一条裸 SQL 的运维修复、将来某条迁移——照样会让它们
// 落后于源码，而那种写入恰恰是最需要缓存不失效的时候。
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
