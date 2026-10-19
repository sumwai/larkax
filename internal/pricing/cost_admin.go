package pricing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/billing/expr"
	"github.com/sumwai/larkax/internal/dberr"
)

// ErrCostNotFound 表示要改的那条成本规则不在库里。
//
// 与 ErrRuleNotFound 分开：两个哨兵值不同，而「哪张表里少了那一行」决定了
// 下一步去看哪张清单（upstream-cost list 还是 price list）。
var ErrCostNotFound = errors.New("成本规则不存在")

// ErrCostNoChanges 表示这次修改没有任何字段要改。
//
// 与 ErrNoChanges 分开的理由同 ErrCostNotFound：都是「没有要改的字段」，
// 但它是另一条命令报出来的。
var ErrCostNoChanges = errors.New("没有要改的字段")

// CostAdmin 是成本规则的管理面：创建、修改、列举，以及停用。
//
// 它不在请求热路径上，也不改变 CostSource.Match 的行为——两者定义「什么算
// 适用」与「谁胜出」用的是同一段 SQL 片段（见 costApplicable 与 costOrderBy），
// 所以「写进去的规则怎么被读出来」不会各自漂移。
//
// # 它为什么 import 计价引擎
//
// 与 Admin 同一个理由：写入前必须把表达式编译一次。成本表达式与售价用的是
// 同一个方言（见 internal/billing/expr），所以编译不过的表达式同样是当场
// 可以拒掉的输入错误，而不是留给消费方的一次故障。
type CostAdmin struct {
	db *sql.DB
}

// NewCostAdmin 包装一个已打开的数据库句柄。
func NewCostAdmin(db *sql.DB) *CostAdmin { return &CostAdmin{db: db} }

// CostCreateRequest 是新建一条成本规则所需的全部字段。
//
// 字段与 upstream_costs 的列一一对应。表上**没有**记账人那一栏（与
// pricing_rules 的 created_by 不同），所以这里也没有「谁定的」这一项：
// 记不了的东西不收，收下来无处可放比不收更坏。
type CostCreateRequest struct {
	// ChannelID 是这条成本规则挂在哪个渠道上。必填：表上那一栏非空，
	// 读取路径也按它匹配（见 costApplicable）。
	ChannelID int64

	// ModelPatterns 是适用的**规范模型名**。空数组表示适用于全部模型。
	//
	// 与售价规则同一套键，也与 channels 的 models 相反：那里的空数组是一条
	// 永远选不中的废配置，这里的空数组是兜底规则的正常写法。
	ModelPatterns []string

	// Expr 是规则源码。写入前会被编译一次，编译不过就整次拒绝。
	Expr string

	Priority int

	// EffectiveFrom 为零值时取当前时刻。
	EffectiveFrom time.Time
	// EffectiveTo 为 nil 表示不过期。
	//
	// 时间窗口是左闭右开（effective_from <= at < effective_to），所以它必须
	// **晚于** EffectiveFrom，否则这条规则一条都匹配不上。
	EffectiveTo *time.Time
}

// CostUpdateRequest 描述一次修改。
//
// 每个字段都是指针：nil 表示「不改这一项」，非 nil 表示「改成这个值」。
// 用指针而不是零值是因为零值本身有意义——priority 改成 0、model_patterns
// 改成空数组（适用于全部模型）都是合法修改，与「没提到这个字段」必须区分开。
type CostUpdateRequest struct {
	ID int64

	ChannelID     *int64
	ModelPatterns *[]string
	Expr          *string
	Priority      *int

	// EffectiveFrom 非 nil 时改下界。零值时刻会被拒：那是「公元 1 年」，
	// 一个没人想要的值，多半是调用方少赋了一次。
	EffectiveFrom *time.Time

	// EffectiveTo 非 nil 时把上界改成这个时刻。
	//
	// **它表达不了「改成不过期」**——nil 是「不改这一项」。要清掉上界就用
	// ClearEffectiveTo。两者同时给会被拒。
	EffectiveTo *time.Time
	// ClearEffectiveTo 为 true 时把上界清成 NULL（这条规则不再过期）。
	//
	// 单独一个字段是因为指针表达不了「置空」与「不改」两件事，而「不再
	// 过期」是一次真实的运维动作——用「设成很远的未来」代替，会让库里
	// 留一个假的日期，看库的人分不出它是不是真的。
	ClearEffectiveTo bool
}

// CostListQuery 是「列出成本规则」的提问条件。
//
// 三个字段都可以不给：全空表示「全部规则」，按渠道分组、组内按裁决次序。
type CostListQuery struct {
	// ChannelID 为 0 表示不限定渠道。
	ChannelID int64

	// CanonicalModel 非空时只列**适用于这个模型**的规则（那要求同时给出
	// ChannelID：成本规则按渠道挂，只给模型问不出按哪条算）。此时清单就是
	// 「为什么是它」的答案，见 List。
	CanonicalModel string

	// At 是「问的是哪一刻」，只在给了 CanonicalModel 时有意义。
	// 零值表示当前时刻。
	At time.Time
}

// CostInfo 是管理面看到的一条成本规则。
type CostInfo struct {
	ID            int64
	ChannelID     int64
	ModelPatterns []string
	Expr          string
	Priority      int
	EffectiveFrom time.Time
	// EffectiveTo 为 nil 表示不过期。
	EffectiveTo *time.Time
	CreatedAt   time.Time

	// DeclaredKeys 是表达式里写过的键（见 expr.Program.Keys），已排序。
	//
	// 它回答的是「这条成本规则算了哪几项」。空数组意味着它一项都没写——
	// 那时每一笔的成本都是 0，而毛利会因此虚高；这与售价侧那个同样的空数组
	// 不完全是同一件事（那里是会拒绝服务的），所以列举是唯一能提前看见它的
	// 地方。ExprErr 非 nil 时它也是空的：编译不过的表达式没有键可谈。
	DeclaredKeys []string

	// ExprErr 说明这条规则的表达式为什么编译不过，为 nil 表示能编译。
	//
	// 写入路径会把编译不过的表达式拒掉，所以它只可能出现在**绕过写入路径**
	// 写进库的行上。那种行每被用到一次就失败一次，而列举是唯一能看见它的地方。
	ExprErr error
}

// Create 新建一条成本规则，返回它的 id。
//
// 顺序与 Admin.Create 相同：校验与归一化（纯 Go，不碰库）→ 编译表达式 → 落库。
// 编译放在落库之前是这里最要紧的性质：**编译不过的规则根本进不了库**。
//
// 与售价规则不同，这里没有版本历史表要一起落，所以不需要事务：一行 INSERT
// 自成一次写入。
func (a *CostAdmin) Create(ctx context.Context, req CostCreateRequest) (int64, error) {
	rule, err := req.normalize()
	if err != nil {
		return 0, err
	}
	if err = validateCost(rule); err != nil {
		return 0, err
	}
	if _, err = expr.Compile(rule.Expr); err != nil {
		return 0, err
	}

	var id int64
	err = a.db.QueryRowContext(ctx, `
		INSERT INTO upstream_costs
			(channel_id, model_patterns, expr, priority, effective_from, effective_to)
		VALUES ($1, to_jsonb($2::text[]), $3, $4, $5, $6)
		RETURNING id`,
		rule.ChannelID, rule.ModelPatterns, rule.Expr, rule.Priority,
		rule.EffectiveFrom, nullableTime(rule.EffectiveTo)).Scan(&id)
	if err != nil {
		// 套一层约束冲突的分类（见 internal/dberr）：--channel 打到一个不存在的
		// 渠道上时，退出码该是「约束冲突」那一档。售价那一侧没套，所以这条不是
		// 照抄来的——判据是 docs/cli-reference.md 的退出码表把外键算作 4。
		return 0, dberr.ConstraintFailure(fmt.Errorf("创建成本规则失败: %w", err))
	}
	return id, nil
}

// normalize 把创建请求变成一行规则：填默认值、归一化各字段。它不碰库。
func (req CostCreateRequest) normalize() (CostInfo, error) {
	patterns, err := normalizePatterns(req.ModelPatterns)
	if err != nil {
		return CostInfo{}, err
	}
	from := req.EffectiveFrom
	if from.IsZero() {
		from = time.Now()
	}
	return CostInfo{
		ChannelID:     req.ChannelID,
		ModelPatterns: patterns,
		Expr:          req.Expr,
		Priority:      req.Priority,
		EffectiveFrom: from,
		EffectiveTo:   req.EffectiveTo,
	}, nil
}

// Update 按 CostUpdateRequest 修改一条规则。
//
// 校验的对象是「改完之后的那一行」：这次写入改动的可能不只是它自己那一栏
// （把上界改到下界之前会让窗口空掉，而判据是合并后那一对时刻）。所以先在一个
// 事务里把当前值锁着读出来，与 Admin.Update 同一个理由。
//
// 表达式是例外，也与那条一致：**只有这次真的写它时才编译**。库里那条本来就
// 编译不过的行（绕过写入路径写进去的）仍然要能被停用——一次「关掉它」的调用
// 不该因为另一个字段坏着而失败。
func (a *CostAdmin) Update(ctx context.Context, req CostUpdateRequest) error {
	if req.ID <= 0 {
		return errors.New("成本规则 id 无效")
	}
	if req.EffectiveTo != nil && req.ClearEffectiveTo {
		return errors.New("EffectiveTo 与 ClearEffectiveTo 不能同时给：" +
			"前者是把上界改成某个时刻，后者是清成不过期")
	}
	if !req.changesAnything() {
		return ErrCostNoChanges
	}

	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开始事务失败: %w", err)
	}
	// 提交之后这里的 Rollback 返回 ErrTxDone，无害。
	defer func() { _ = tx.Rollback() }()

	current, err := loadCostForUpdate(ctx, tx, req.ID)
	if err != nil {
		return err
	}
	merged, err := req.applyTo(current)
	if err != nil {
		return err
	}
	if err = validateCost(merged); err != nil {
		return err
	}
	if req.Expr != nil {
		if _, err = expr.Compile(*req.Expr); err != nil {
			return err
		}
	}

	// 整行写回：校验的对象与写入的对象是同一个值。created_at 不在里面——
	// 它不是可改的字段。
	if _, err = tx.ExecContext(ctx, `
		UPDATE upstream_costs
		   SET channel_id = $1, model_patterns = to_jsonb($2::text[]), expr = $3,
		       priority = $4, effective_from = $5, effective_to = $6
		 WHERE id = $7`,
		merged.ChannelID, merged.ModelPatterns, merged.Expr, merged.Priority,
		merged.EffectiveFrom, nullableTime(merged.EffectiveTo), req.ID); err != nil {
		return dberr.ConstraintFailure(fmt.Errorf("修改成本规则失败: %w", err))
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("提交修改失败: %w", err)
	}
	return nil
}

// changesAnything 判断这次请求有没有提到任何一个字段。
func (req CostUpdateRequest) changesAnything() bool {
	return req.ChannelID != nil || req.ModelPatterns != nil || req.Expr != nil ||
		req.Priority != nil || req.EffectiveFrom != nil || req.EffectiveTo != nil ||
		req.ClearEffectiveTo
}

// applyTo 把这次修改落到当前那一行上，返回改完之后的样子。
func (req CostUpdateRequest) applyTo(cur CostInfo) (CostInfo, error) {
	out := cur

	if req.ChannelID != nil {
		out.ChannelID = *req.ChannelID
	}
	if req.Priority != nil {
		out.Priority = *req.Priority
	}
	if req.Expr != nil {
		out.Expr = *req.Expr
	}
	if req.ModelPatterns != nil {
		patterns, err := normalizePatterns(*req.ModelPatterns)
		if err != nil {
			return CostInfo{}, err
		}
		out.ModelPatterns = patterns
	}
	if req.EffectiveFrom != nil {
		if req.EffectiveFrom.IsZero() {
			return CostInfo{}, errors.New("effective_from 不能是零值时刻：" +
				"要「从现在起」就别给这个字段")
		}
		out.EffectiveFrom = *req.EffectiveFrom
	}
	switch {
	case req.ClearEffectiveTo:
		out.EffectiveTo = nil
	case req.EffectiveTo != nil:
		to := *req.EffectiveTo
		out.EffectiveTo = &to
	}

	return out, nil
}

// Disable 把一条成本规则的生效窗口收在当下：它从现在起不再适用。
//
// # 为什么是「收窗口」而不是一个状态位
//
// upstream_costs 表上没有 enabled 那一栏（pricing_rules 有），所以「停用」在
// 这张表上的形状只能是**让窗口不再覆盖当下**。写 effective_to = now() 就是
// 这句话，读路径不必为它多一条判据。
//
// # 两件它不做的事
//
//   - **还没生效的规则拒绝**：照做会写出 effective_to <= effective_from 的空
//     窗口，而同一个形状在 create / update 上是拒的（见 validateCost）。同一个
//     包里对「一行合法的生效区间」只能有一个判据，disable 不该是唯一能造出
//     反例的动词。要取消一条计划中的规则，先把它的下界改到现在再 disable。
//   - **已经失效的规则拒绝**：它现在就已经不适用，把上界改成此刻会把窗口
//     **拉长**（一条 3 月就结束的规则会变成适用到今天），那与「停用」正好相反。
//
// 两种情况都报明原因，不留一个看起来成功的调用。
func (a *CostAdmin) Disable(ctx context.Context, id int64) error {
	if id <= 0 {
		return errors.New("成本规则 id 无效")
	}

	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开始事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	current, err := loadCostForUpdate(ctx, tx, id)
	if err != nil {
		return err
	}
	now := time.Now()
	if !now.After(current.EffectiveFrom) {
		return fmt.Errorf("成本规则 %d 要到 %s 才生效，"+
			"把上界收到此刻会让生效区间变空：先把它改成现在生效，再停用它",
			id, current.EffectiveFrom.Format(time.RFC3339))
	}
	if current.EffectiveTo != nil && !current.EffectiveTo.After(now) {
		return fmt.Errorf("成本规则 %d 在 %s 就已经失效了，"+
			"把上界收到此刻等于让它从此刻起重新生效：要看它现在是什么样，用 upstream-cost list",
			id, current.EffectiveTo.Format(time.RFC3339))
	}

	if _, err = tx.ExecContext(ctx, `
		UPDATE upstream_costs
		   SET effective_to = $1
		 WHERE id = $2`, now, id); err != nil {
		return dberr.ConstraintFailure(fmt.Errorf("停用成本规则失败: %w", err))
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("提交停用失败: %w", err)
	}
	return nil
}

// List 列出成本规则。
//
// 排序用的是 Match 的同一段 ORDER BY（见 costOrderBy），于是这个清单有一个
// 可以直接读出来的性质：**同一条渠道内，排第一的那条就是 Match 会选中的那条**。
// 跨渠道的规则彼此不竞争，所以清单先按渠道分组。
//
// # 给了 CanonicalModel 时只列适用的那几条
//
// 那时这张清单回答的是「这条渠道这个模型现在按哪条算成本、为什么不是别条」，
// 所以判据与 Match 逐字相同（见 costApplicable）：不适用的规则（时间窗外、
// 模型名单里没有它）不出现在结果里，而不是排在后面——这与 price explain 的
// 处置一致。
//
// 一条都不适用时返回**空清单**，不返回 ErrNoCostRule：调用方（命令那一层）
// 要把「这个渠道这个模型没配成本」与「命令没跑起来」分开说，而那需要它自己
// 决定怎么说。选择器那一侧不这么做——用得上 Match 的调用方要的是「有没有」
// 这个问题的答案（见 ErrNoCostRule）。
//
// # 一行表达式坏了不影响别的行
//
// 编译失败只写进那一行的 ExprErr，**不跳过、也不整次报错**：整次报错会让库里
// 有一条坏规则时整个清单一条都看不到，而那正是最需要看清单的时候。
func (a *CostAdmin) List(ctx context.Context, q CostListQuery) ([]CostInfo, error) {
	query, args := listCostQuery(q)

	rows, err := a.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("查询成本规则失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// 从空切片开始而不是 nil：一条规则都没配是正常结果，不是「没有值」。
	out := []CostInfo{}
	for rows.Next() {
		info, scanErr := scanCostInfo(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, decorateCost(info))
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历成本规则失败: %w", err)
	}
	return out, nil
}

// ── SQL ─────────────────────────────────────────────────────────────────

// costColumns 是管理面要看的全部列，三条查询共用（列举的三档、修改前的读锁）。
// 表别名必须叫 r。
const costColumns = `
	    r.id,
	    r.channel_id,
	    r.model_patterns,
	    r.expr,
	    r.priority,
	    r.effective_from,
	    r.effective_to,
	    r.created_at`

// listAllCostsQuery 列出全部成本规则。
var listAllCostsQuery = `
	SELECT ` + costColumns + `
	  FROM upstream_costs r
	 ORDER BY ` + costOrderBy

// listChannelCostsQuery 列出一条渠道的全部成本规则。$1 是渠道 id。
var listChannelCostsQuery = `
	SELECT ` + costColumns + `
	  FROM upstream_costs r
	 WHERE r.channel_id = $1
	 ORDER BY ` + costOrderBy

// listApplicableCostsQuery 列出一条渠道上适用于某个模型的成本规则。
//
// 判据与 matchCostQuery 相同（同一段 costApplicable），区别只在于它取回全部
// 适用的规则（没有 LIMIT 1）。占位符含义与 matchCostQuery 一致：$1 时刻、
// $2 规范模型名、$3 渠道 id。
var listApplicableCostsQuery = `
	SELECT ` + costColumns + `
	  FROM upstream_costs r
	 WHERE ` + costApplicable + `
	 ORDER BY ` + costOrderBy

// listCostQuery 按提问条件选一条查询，返回 SQL 与它的参数。
//
// 三条查询而不是一条带 `$1 = 0 OR ...` 的：那个写法要把「0 不是合法渠道 id」
// 这条事实抄进 SQL，而三档本来就各有各的 ORDER BY 需求（只有按模型问的那
// 一档要过滤适用性）。分档让每一档的 SQL 各自读得懂。
func listCostQuery(q CostListQuery) (string, []any) {
	switch {
	case q.CanonicalModel != "":
		at := q.At
		if at.IsZero() {
			at = time.Now()
		}
		return listApplicableCostsQuery, []any{at, q.CanonicalModel, q.ChannelID}
	case q.ChannelID > 0:
		return listChannelCostsQuery, []any{q.ChannelID}
	default:
		return listAllCostsQuery, nil
	}
}

// lockCostQuery 读出一条成本规则并锁住它，供修改与停用路径做读-改-写。
var lockCostQuery = `
	SELECT ` + costColumns + `
	  FROM upstream_costs r
	 WHERE r.id = $1
	   FOR UPDATE`

// ── 读取与编译 ──────────────────────────────────────────────────────────

// scanCostInfo 把一行扫成 CostInfo。它不填 DeclaredKeys 与 ExprErr，
// 那两个字段由 decorateCost 填。
func scanCostInfo(row rowScanner) (CostInfo, error) {
	var (
		info     CostInfo
		patterns []byte
		to       sql.NullTime
	)
	err := row.Scan(
		&info.ID, &info.ChannelID, &patterns, &info.Expr, &info.Priority,
		&info.EffectiveFrom, &to, &info.CreatedAt,
	)
	if err != nil {
		return CostInfo{}, fmt.Errorf("读取成本规则行失败: %w", err)
	}

	// model_patterns 是 jsonb。它解不开说明这一行不是按表定义写进去的，
	// 与 scanRuleInfo 的处置一致：整次报错，不猜。
	if err = json.Unmarshal(patterns, &info.ModelPatterns); err != nil {
		return CostInfo{}, fmt.Errorf("成本规则 %d 的 model_patterns 不是合法 JSON 数组: %w", info.ID, err)
	}
	if info.ModelPatterns == nil {
		// 空数组与 nil 在调用方那一侧都是「适用于全部模型」，但让它们长得
		// 不一样会让调用方写出「判 nil 还是判空」这种没有答案的代码。
		info.ModelPatterns = []string{}
	}
	if to.Valid {
		at := to.Time
		info.EffectiveTo = &at
	}
	return info, nil
}

// decorateCost 编译一次表达式，填上 DeclaredKeys 与 ExprErr。
func decorateCost(info CostInfo) CostInfo {
	prog, err := expr.Compile(info.Expr)
	if err != nil {
		info.ExprErr = err
		return info
	}
	info.DeclaredKeys = prog.Keys()
	return info
}

// loadCostForUpdate 读出一条成本规则的当前值，并锁住这一行。
func loadCostForUpdate(ctx context.Context, tx *sql.Tx, id int64) (CostInfo, error) {
	info, err := scanCostInfo(tx.QueryRowContext(ctx, lockCostQuery, id))
	if errors.Is(err, sql.ErrNoRows) {
		return CostInfo{}, fmt.Errorf("%w：id %d", ErrCostNotFound, id)
	}
	if err != nil {
		return CostInfo{}, err
	}
	return info, nil
}

// ── 校验与归一化 ────────────────────────────────────────────────────────

// validateCost 校验一行成本规则的形状。
//
// 判据是「读取路径永远匹配不上」这一类安静失败：渠道那一栏非空（costApplicable
// 按它匹配），时间窗口的上界要晚于下界（左闭右开，不晚于下界的一条都匹配不上）。
// 模型名单的归一化与售价规则共用 normalizePatterns——通配符在那里被拒，理由
// 见它的注释。
//
// 它**不判**「这条规则现在能不能匹配上某个具体模型」：一条按计划将来才生效的
// 规则是正常配置，与 Admin.Create 的处置一致。
func validateCost(r CostInfo) error {
	if r.ChannelID <= 0 {
		return fmt.Errorf("渠道 ID %d 无效：成本规则必须挂在一条渠道上（渠道 ID 从 1 开始），"+
			"读取路径按它匹配", r.ChannelID)
	}
	if strings.TrimSpace(r.Expr) == "" {
		return errors.New("表达式不能为空")
	}
	if r.EffectiveTo != nil && !r.EffectiveTo.After(r.EffectiveFrom) {
		return fmt.Errorf("effective_to（%s）必须晚于 effective_from（%s）："+
			"时间窗口是左闭右开，上界不晚于下界的规则一条都匹配不上",
			r.EffectiveTo.Format(time.RFC3339), r.EffectiveFrom.Format(time.RFC3339))
	}
	return nil
}
