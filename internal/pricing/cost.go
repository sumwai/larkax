package pricing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sumwai/larkax/internal/pipeline"
)

// 本文件是**成本规则**那一侧：upstream_costs 表里的一行说的是「这条渠道上，
// 这个模型的上游成本按哪个表达式算」，也就是「这次请求我们花了多少」。
// 售价规则（pricing_rules）说的是「向用户收了多少」，两条线合起来才有毛利。
//
// # 与售价规则的差别只有一处：没有作用域
//
// 售价那条要回答「谁定的价」，所以有作用域那一层（platform / merchant /
// channel），裁决次序的第一项是作用域的具体程度。成本规则没有这个问题——
// 上游成本是**渠道自己的属性**：同一个模型在两条渠道上是两个上游、两个价，
// 与谁定的价无关。所以这里的作用域那一层塌成了 channel_id 这一个条件。
//
// 相同的那一半（生效窗口、模型名单、优先级的裁决次序）与售价规则共用同一段
// 片段，见 applicableWindowAndModel 与 modelSpecificFirst——口径对齐这件事
// 只有写成同一段文字才守得住，抄第二份的那天就是两份开始分岔的那天。

// costApplicable 是「这条成本规则适用于这条渠道这个模型」的判据。
//
// 占位符沿用 matchQuery 的约定：$1 是匹配时刻，$2 是规范模型名，另加 $3
// （渠道 id）。窗口与模型名单那两条来自与售价规则共用的那一段。
var costApplicable = `r.channel_id = $3
	       AND ` + applicableWindowAndModel("$2")

// costOrderBy 是成本规则的裁决次序：渠道 ID 升序 → priority 大的优先 →
// 模型名单非空的优先于兜底 → id 大的优先。
//
// 第一栏按渠道分组，是因为成本规则只在**同一条渠道内**彼此竞争：跨渠道比
// priority 没有含义，一条渠道的成本价压不住另一条渠道的。于是「这个清单里
// 排第一的那条」与「这条渠道这个模型会被选中的那条」在同一条渠道内是同一
// 件事，一张只按优先级排的表做不到这一点。其余三项与 ruleOrderBy 逐条同义，
// 各自表达什么见那里的注释。
const costOrderBy = `r.channel_id ASC,
	        r.priority DESC,
	        ` + modelSpecificFirst + `,
	        r.id DESC`

// ErrNoCostRule 表示这条渠道这个模型没有生效的成本规则。
//
// 定义在 pipeline 那一侧（`pipeline.ErrNoCostRule`），这里只保留一个别名：
// 「算不出成本」是消费方的结论——结算拿它决定这一列写 NULL 而不是报错——
// 而本包只是把它作为匹配失败的一个返回值。命令行与用例沿用这个包的名字，
// 用 errors.Is 判定时两者是同一个值。
var ErrNoCostRule = pipeline.ErrNoCostRule

// CostSource 按渠道与模型从 upstream_costs 表里选出一条成本规则。
//
// 与 Source 分成两个类型，理由相同（见 Admin 的注释）：选规则与写规则要用的
// 东西不一样，而热路径上那个类型拿不到写入能力是编译期的事实。
type CostSource struct {
	db *sql.DB
}

// NewCostSource 包装一个已打开的数据库句柄。
func NewCostSource(db *sql.DB) *CostSource { return &CostSource{db: db} }

// 编译期断言：它要满足 pipeline 对「结算时的成本规则来源」的要求。
//
// 与 Source 那处同一个做法（内联接口，不 import 一个别处的声明）。
var _ interface {
	MatchCost(context.Context, pipeline.CostQuery) (pipeline.CostRule, error)
} = (*CostSource)(nil)

// CostQuery 是「这条渠道这个模型的上游成本按哪条规则算」这一次提问。
//
// 这是管理面与 explain 用的形状；结算路径问的是同一个问题，但用
// pipeline.CostQuery——那边多一个版本号，用于编译缓存（见 MatchCost）。
type CostQuery struct {
	// ChannelID 是这次请求落到的那条渠道。成本规则按渠道挂，所以它没有
	// 「未选定」（0）这一档：调用方拿到渠道之后才问得出这个问题。
	ChannelID int64
	// CanonicalModel 是规范模型名。与售价规则同一套键：渠道上挂的是规范名，
	// 上游名由 alias 那一层映射。
	CanonicalModel string
	// At 为零值时取当前时刻。
	At time.Time
}

// CostRule 是被选中的那条成本规则。
//
// 它只有 id 与表达式源码：这是 explain 要回答的形状（「按哪条算」）。
// 结算路径上用的是 pipeline.CostRule，那份多带版本号供编译缓存。
type CostRule struct {
	ID   int64
	Expr string
}

// matchCostQuery 是选成本规则的查询。$1 时刻、$2 规范模型名、$3 渠道 id，
// 判据与次序见 costApplicable 与 costOrderBy。
var matchCostQuery = `
	SELECT r.id, r.expr
	  FROM upstream_costs r
	 WHERE ` + costApplicable + `
	 ORDER BY ` + costOrderBy + `
	 LIMIT 1`

// Match 返回这条渠道这个模型现在适用的成本规则（explain 用的形状）。
//
// 没有匹配的规则时返回 ErrNoCostRule，**不返回一条零成本的兜底规则**：那会让
// 「没配成本价」表现为「上游不要钱」，而毛利会因此虚高——那是最不容易被发现的
// 一类错账。这条判断与售价那一侧是同一条。
func (s *CostSource) Match(ctx context.Context, q CostQuery) (CostRule, error) {
	rule, err := s.MatchCost(ctx, pipeline.CostQuery{
		ChannelID:      q.ChannelID,
		CanonicalModel: q.CanonicalModel,
		At:             q.At,
	})
	if err != nil {
		return CostRule{}, err
	}
	return CostRule{ID: rule.ID, Expr: rule.Source}, nil
}

// MatchCost 返回结算路径要的那条成本规则，带编译缓存需要的版本号。
//
// 与 Match 共用同一条查询与同一份校验：两处各拼一份的话，explain 说「按这条
// 算」与结算实际按的那条会各自漂移，而两边都算得出数来。
//
// 没有匹配的规则时返回 ErrNoCostRule——结算据此把这一列写 NULL，不拒绝请求
// （见 pipeline.ErrNoCostRule）。
func (s *CostSource) MatchCost(ctx context.Context, q pipeline.CostQuery) (pipeline.CostRule, error) {
	if q.ChannelID <= 0 {
		return pipeline.CostRule{}, fmt.Errorf("渠道 ID %d 无效：渠道 ID 从 1 开始", q.ChannelID)
	}
	if q.CanonicalModel == "" {
		return pipeline.CostRule{}, errors.New("规范模型名为空")
	}
	// 查询没给时刻时用当前的，与 Source.now 同一个判据：零值时刻（公元 1 年）
	// 会让所有规则都还没生效。
	at := q.At
	if at.IsZero() {
		at = time.Now()
	}

	var (
		id  int64
		src string
	)
	err := s.db.QueryRowContext(ctx, matchCostQuery, at, q.CanonicalModel, q.ChannelID).Scan(&id, &src)
	if errors.Is(err, sql.ErrNoRows) {
		return pipeline.CostRule{}, fmt.Errorf("%w：渠道 %d 上模型 %q 没有生效的成本规则",
			ErrNoCostRule, q.ChannelID, q.CanonicalModel)
	}
	if err != nil {
		return pipeline.CostRule{}, fmt.Errorf("查询成本规则失败: %w", err)
	}
	// upstream_costs 没有版本列，于是版本号按表达式算出来——与售价侧
	// pricing.Source.Match 的 versionOf 同一个做法：规则被原地改过之后，
	// 缓存键必须跟着变，否则「改了规则但没生效」看起来像「规则写错了」。
	return pipeline.CostRule{ID: id, Source: src, Version: versionOf(src)}, nil
}
