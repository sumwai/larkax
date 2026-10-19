package pricing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
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
const costApplicable = `r.channel_id = $3
	       AND ` + applicableWindowAndModel

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
// 与 pipeline.ErrNoRule 分开：那一条在请求路径上是拒绝服务（没配价就不能算
// 钱），而成本这一侧今天不参与任何拒绝。上游成本还没有消费方：选路那一维
// （route.CostPerUnit）没有写入侧，读到的始终是「没有数据」，于是它连权重一起
// 退出分母（见 route.DefaultWeights）。所以拿着这个错误的调用方该做的是「记下
// 算不出成本」，不是把请求打回去。
var ErrNoCostRule = errors.New("没有生效的成本规则")

// CostSource 按渠道与模型从 upstream_costs 表里选出一条成本规则。
//
// 与 Source 分成两个类型，理由相同（见 Admin 的注释）：选规则与写规则要用的
// 东西不一样，而热路径上那个类型拿不到写入能力是编译期的事实。
type CostSource struct {
	db *sql.DB
}

// NewCostSource 包装一个已打开的数据库句柄。
func NewCostSource(db *sql.DB) *CostSource { return &CostSource{db: db} }

// CostQuery 是「这条渠道这个模型的上游成本按哪条规则算」这一次提问。
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
// 它只有 id 与表达式源码：怎么求值、求出来的数落在哪一列，是消费这一侧的事
// （今天没有消费方，见包注释）。留 id 是为了让「按哪条算的」能被记下来，
// 与售价侧 pipeline.Rule 的 ID 同一个用处。
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

// Match 返回这条渠道这个模型现在适用的成本规则。
//
// 没有匹配的规则时返回 ErrNoCostRule，**不返回一条零成本的兜底规则**：那会让
// 「没配成本价」表现为「上游不要钱」，而毛利会因此虚高——那是最不容易被发现的
// 一类错账。这条判断与售价那一侧是同一条，只是失败落在哪一侧不同（见
// ErrNoCostRule）。
func (s *CostSource) Match(ctx context.Context, q CostQuery) (CostRule, error) {
	if q.ChannelID <= 0 {
		return CostRule{}, fmt.Errorf("渠道 ID %d 无效：渠道 ID 从 1 开始", q.ChannelID)
	}
	if q.CanonicalModel == "" {
		return CostRule{}, errors.New("规范模型名为空")
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
		return CostRule{}, fmt.Errorf("%w：渠道 %d 上模型 %q 没有生效的成本规则",
			ErrNoCostRule, q.ChannelID, q.CanonicalModel)
	}
	if err != nil {
		return CostRule{}, fmt.Errorf("查询成本规则失败: %w", err)
	}
	return CostRule{ID: id, Expr: src}, nil
}
