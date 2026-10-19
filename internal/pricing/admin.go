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
	"github.com/sumwai/larkax/internal/pipeline"
)

// 作用域的取值。
//
// 表里的 CHECK、读取路径的匹配判据与写入路径的校验用的是同一组字符串，
// 导出它们是为了让调用方不必手抄字面量——抄错一个字母的表现是
// 「这条规则一条都匹配不上」，而不是一次报错。
const (
	ScopePlatform = "platform"
	ScopeMerchant = "merchant"
	ScopeChannel  = "channel"
)

// ErrRuleNotFound 表示要改的那条规则不在库里。
//
// 与「改不动」分开：前者是 id 写错了或者那条已经被删了，后者是数据库的问题。
// 没有这个区分的话，一次拼错 id 的调用会静默成功，而人以为改完了。
var ErrRuleNotFound = errors.New("计价规则不存在")

// ErrNoChanges 表示这次修改没有任何字段要改。
//
// 单独报错而不是发一条只动 updated_at 的 UPDATE：后者会让「调用方拼错了
// 结构体」看起来像一次成功的修改。
var ErrNoChanges = errors.New("没有要改的字段")

// Admin 是计价规则的管理面：创建、修改、列举，以及说明「现在按哪条计价」。
//
// 它不在请求热路径上，也不改变 Source.Match 的行为——两者定义「什么算适用」
// 与「谁胜出」用的是同一段 SQL 片段（见 ruleApplicable 与 ruleOrderBy），
// 所以「写进去的规则怎么被读出来」不会各自漂移。
//
// # 为什么是单独一个类型，而不是挂在 Source 上
//
// Source 在请求热路径上——每个请求都会调它。管理面只在运维动作里被调用，
// 而它要做的事情（编译表达式）读路径根本不需要。分成两个类型之后，热路径
// 那个类型**拿不到**写入能力：这是编译期的事实，不是一句约定。
//
// # 它为什么 import 计价引擎
//
// 读取路径不 import 计价引擎（见包注释），管理面反过来：写入前必须把表达式
// 编译一次（见 Create）。一条编译不过的规则进了库，表现是每个用到它的请求
// 都失败，而失败点在热路径上，排查的人会先去怀疑请求。
//
// 编译产物不留在这里：管理面只问「编不编得过」与「它声明了哪几项单价」
// （见 RuleInfo.DeclaredKeys），求值仍然是 pipeline 那一侧的事。
type Admin struct {
	db *sql.DB
}

// NewAdmin 包装一个已打开的数据库句柄。
func NewAdmin(db *sql.DB) *Admin { return &Admin{db: db} }

// CreateRequest 是新建一条计价规则所需的全部字段。
type CreateRequest struct {
	// Scope 是作用域，取值见 ScopePlatform / ScopeMerchant / ScopeChannel。
	//
	// 它决定另外两栏哪一栏有效：platform 两栏都必须为空，merchant 只认
	// OwnerUserID，channel 只认 ChannelID。写了读取路径不看的那一栏会被拒，
	// 理由见 validateRule。
	Scope string

	// OwnerUserID 为 0 表示不挂商家，库里存 NULL（读取路径把两者都看成 0）。
	OwnerUserID int64
	// ChannelID 为 0 表示不挂渠道，库里存 NULL。
	ChannelID int64

	// Name 是给人看的名字，不能为空：列举与告警都靠它指认一条规则。
	Name string

	// ModelPatterns 是适用的规范模型名。**空数组表示适用于全部模型**。
	//
	// 与 channels 的 models 相反：那里的空数组是一条永远选不中的废配置，
	// 这里的空数组是「兜底规则」这个正常写法（见 ruleApplicable）。
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

	// Enabled 为 nil 表示启用。用指针而不是 bool：零值 false 会让「忘了写
	// 这个字段」变成一条静默停用的规则。
	Enabled *bool

	// CreatedBy 为 0 表示不记，库里存 NULL。它记的是「这条价是谁定的」，
	// 而写入路径是唯一能填它的地方。
	CreatedBy int64
}

// Create 新建一条计价规则，返回它的 id。
//
// 顺序是：校验与归一化（纯 Go，不碰库）→ 编译表达式 → 落库。编译放在落库
// 之前是这里最要紧的性质：**编译不过的规则根本进不了库**。
//
// 新建的规则不保证「能匹配上」：一条生效时间还没到的规则、一条模型名单
// 不包含任何在用模型的规则，都是合法的（前者是按计划生效，后者可能是先
// 配好再上模型）。但**形状上**匹配不上的组合会被拒——例如 merchant 作用域
// 不给 owner，那种规则无论等到什么时候都不会生效，见 validateRule。
//
// 规则与它的第一版历史在**同一个事务**里落：只落了规则而没落版本，表现为
// 「这条规则的版本历史从第二版开始」，而差额出现时正好要用第一版对照。
func (a *Admin) Create(ctx context.Context, req CreateRequest) (int64, error) {
	rule, err := req.normalize()
	if err != nil {
		return 0, err
	}
	if err = validateRule(rule); err != nil {
		return 0, err
	}
	if _, err = expr.Compile(rule.Expr); err != nil {
		return 0, err
	}

	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("开始事务失败: %w", err)
	}
	// 提交之后这里的 Rollback 返回 ErrTxDone，无害。
	defer func() { _ = tx.Rollback() }()

	var id int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO pricing_rules
			(scope, owner_user_id, channel_id, name, model_patterns, expr, priority,
			 enabled, effective_from, effective_to, created_by)
		VALUES ($1, $2, $3, $4, to_jsonb($5::text[]), $6, $7, $8, $9, $10, $11)
		RETURNING id`,
		rule.Scope, nullableID(rule.OwnerUserID), nullableID(rule.ChannelID),
		rule.Name, rule.ModelPatterns, rule.Expr, rule.Priority,
		rule.Enabled, rule.EffectiveFrom, nullableTime(rule.EffectiveTo),
		nullableID(rule.CreatedBy)).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("创建计价规则失败: %w", err)
	}

	// 第一版的改动人就是建它的人：取值来源是 CreateRequest.CreatedBy，
	// 不需要调用方额外提供任何东西。为 0 时落成 NULL——那是「没有人可记」，
	// 与「记了一个没做过这件事的人」是两回事，所以这里不兜一个编出来的值。
	if err = insertRuleVersion(ctx, tx, id, rule.Expr, rule.CreatedBy); err != nil {
		return 0, err
	}

	if err = tx.Commit(); err != nil {
		return 0, fmt.Errorf("提交创建失败: %w", err)
	}
	return id, nil
}

// normalize 把创建请求变成一行规则：填默认值、归一化各字段。它不碰库。
//
// 归一化与校验分开：归一化只做「把一个合法值变成库里的写法」（去空白、
// 零值换成默认），校验判「这个组合是不是读取路径认得的那种」。
func (req CreateRequest) normalize() (RuleInfo, error) {
	name, err := normalizeName(req.Name)
	if err != nil {
		return RuleInfo{}, err
	}
	patterns, err := normalizePatterns(req.ModelPatterns)
	if err != nil {
		return RuleInfo{}, err
	}

	// 生效时刻默认取当前：列默认值也是 now()，但这里显式算出来是因为
	// validateRule 要拿它与 effective_to 比，而那一次比较必须与真正写进库的
	// 是同一个值。
	from := req.EffectiveFrom
	if from.IsZero() {
		from = time.Now()
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	return RuleInfo{
		Scope:         req.Scope,
		OwnerUserID:   req.OwnerUserID,
		ChannelID:     req.ChannelID,
		Name:          name,
		ModelPatterns: patterns,
		Expr:          req.Expr,
		Priority:      req.Priority,
		EffectiveFrom: from,
		EffectiveTo:   req.EffectiveTo,
		Enabled:       enabled,
		CreatedBy:     req.CreatedBy,
	}, nil
}

// UpdateRequest 描述一次修改。
//
// 每个字段都是指针：nil 表示「不改这一项」，非 nil 表示「改成这个值」。
// 用指针而不是零值是因为零值本身有意义——priority 改成 0、model_patterns
// 改成空数组（适用于全部模型）都是合法修改，与「没提到这个字段」必须区分开。
type UpdateRequest struct {
	ID int64

	Scope         *string
	OwnerUserID   *int64
	ChannelID     *int64
	Name          *string
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

	Enabled *bool

	// ChangedBy 是这次改价是谁做的（users.id）。0 表示不记，库里存 NULL。
	//
	// 它只落在版本历史那一行上（pricing_rule_versions.changed_by）——那张表
	// 记的是「成交价的依据留痕」，改价的人属于这条留痕。规则行自己的
	// created_by 不跟着换：那记的是这条价是谁定的，与谁改的（见 Update 里
	// 的说明）不是同一件事。
	//
	// 它**不是**一次修改：只给 ChangedBy 而不给别的字段时，这次调用没有要
	// 改的东西（见 changesAnything 与 ErrNoChanges）。
	ChangedBy int64
}

// Update 按 UpdateRequest 修改一条规则。
//
// # 校验的对象是「改完之后的那一行」
//
// 这次写入改动的可能不只是它自己那一栏：把 scope 从 merchant 改成 platform，
// owner_user_id 那一栏的含义就变了（platform 作用域的读取判据要求它是
// NULL，而 merchant 要求它非空）。所以判据只能是合并后的那一行，而不是
// 「这次给了哪几个字段」。为此先在一个事务里把当前值锁着读出来。
//
// 表达式是例外：**只有这次真的写它时才编译**。库里那条本来就编译不过的
// 行（绕过写入路径写进去的）仍然要能被停用——一次「关掉它」的调用不该
// 因为另一个字段坏着而失败，而那正是出故障时最需要做的动作。结构上的
// 非法组合没有这个问题：它只会让规则「多一栏被忽略」或「一条都匹配不上」，
// 都不会让它在线上算错钱。
func (a *Admin) Update(ctx context.Context, req UpdateRequest) error {
	if req.ID <= 0 {
		return errors.New("计价规则 id 无效")
	}
	if req.EffectiveTo != nil && req.ClearEffectiveTo {
		return errors.New("EffectiveTo 与 ClearEffectiveTo 不能同时给：" +
			"前者是把上界改成某个时刻，后者是清成不过期")
	}
	if !req.changesAnything() {
		return ErrNoChanges
	}

	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开始事务失败: %w", err)
	}
	// 提交之后这里的 Rollback 返回 ErrTxDone，无害。
	defer func() { _ = tx.Rollback() }()

	current, err := loadRuleForUpdate(ctx, tx, req.ID)
	if err != nil {
		return err
	}

	merged, err := req.applyTo(current)
	if err != nil {
		return err
	}
	if err = validateRule(merged); err != nil {
		return err
	}
	if req.Expr != nil {
		if _, err = expr.Compile(*req.Expr); err != nil {
			return err
		}
	}

	// 整行写回：校验的对象与写入的对象是同一个值。只写改过的列也能对，
	// 但那要求 SET 列表与合并出来的那一行严格一致，而它们是两处分别拼的。
	//
	// created_at 与 created_by 不在里面：它们不是可改的字段。
	//
	// 不查 RowsAffected：这一行刚在同一个事务里被 FOR UPDATE 读出来，
	// 「改到了几行」不可能不是 1。
	if _, err = tx.ExecContext(ctx, `
		UPDATE pricing_rules
		   SET scope = $1, owner_user_id = $2, channel_id = $3, name = $4,
		       model_patterns = to_jsonb($5::text[]), expr = $6, priority = $7,
		       enabled = $8, effective_from = $9, effective_to = $10,
		       updated_at = now()
		 WHERE id = $11`,
		merged.Scope, nullableID(merged.OwnerUserID), nullableID(merged.ChannelID),
		merged.Name, merged.ModelPatterns, merged.Expr, merged.Priority,
		merged.Enabled, merged.EffectiveFrom, nullableTime(merged.EffectiveTo),
		req.ID); err != nil {
		return fmt.Errorf("修改计价规则失败: %w", err)
	}

	// 表达式变了才留一版。这张表记的是**成交价的依据**（expr）：改名字、
	// 改优先级、改生效时间都不改变任何一笔已经算出来的钱，为它们插一行会
	// 让版本历史里塞满「表达式其实没变」的噪声，而它的读者正是要在差额
	// 出现时一眼看出「价是什么时候被改的」。
	//
	// **那个读者今天还不存在**：这张表目前只有写入方，没有读取方（版本历史
	// 属于管理面，尚未开建）。先写后读是刻意的——历史一旦没记就补不回来，
	// 而「某笔差额是哪次改价造成的」只能在当时那一刻记下来。
	//
	// 判据用的是这一行**改之前**的值（current 是上面 FOR UPDATE 读出来的），
	// 不是「这次有没有传 Expr」：传了一个与现值相同的表达式不算一次改价。
	if merged.Expr != current.Expr {
		// changed_by 取自这次调用（UpdateRequest.ChangedBy）。规则行自己的
		// created_by 不动：那记的是这条价是谁**定**的，而这个人在这次改价
		// 里可能根本没参与；把它当成改动人写下来，审计会读到一个没做过
		// 这件事的人，而一条错的归属比一条空的归属坏得多。
		if err = insertRuleVersion(ctx, tx, req.ID, merged.Expr, req.ChangedBy); err != nil {
			return err
		}
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("提交修改失败: %w", err)
	}
	return nil
}

// insertRuleVersion 给一条规则留一版表达式历史，供事后回答
// 「这个价是什么时候被改成这样的」。
//
// # 版本号取 max + 1，不取 count
//
// 版本号是审计里会被引用的一个数（「这一笔是按 v3 算的」），所以它必须
// 单调、不重号。count 会在删掉中间某一版之后撞上已有号，而
// UNIQUE(rule_id, version) 会把那次写入整个拒掉——表现为「改价改不动了」。
//
// 并发安全靠调用方：两个写入路径都在事务里，Update 那一侧还先对
// pricing_rules 的这一行取了 FOR UPDATE，所以同一规则的两次改版是串行的。
//
// changedBy 为 0 表示没有操作人可记，落库成 NULL（见 Update 里的说明）。
//
// 它**不写 expr 以外的字段**：这张表只承诺「成交价的依据留痕」，
// 规则的名字、优先级、生效时间不在其中——那些改动不改变任何一笔钱。
func insertRuleVersion(ctx context.Context, tx *sql.Tx, ruleID int64, expr string, changedBy int64) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO pricing_rule_versions (rule_id, version, expr, changed_by)
		SELECT $1, COALESCE(MAX(version), 0) + 1, $2, $3
		  FROM pricing_rule_versions
		 WHERE rule_id = $1`,
		ruleID, expr, nullableID(changedBy)); err != nil {
		return fmt.Errorf("记录计价规则版本失败: %w", err)
	}
	return nil
}

// changesAnything 判断这次请求有没有提到任何一个字段。
func (req UpdateRequest) changesAnything() bool {
	return req.Scope != nil || req.OwnerUserID != nil || req.ChannelID != nil ||
		req.Name != nil || req.ModelPatterns != nil || req.Expr != nil ||
		req.Priority != nil || req.EffectiveFrom != nil || req.EffectiveTo != nil ||
		req.ClearEffectiveTo || req.Enabled != nil
}

// applyTo 把这次修改落到当前那一行上，返回改完之后的样子。
//
// 只有请求里提到（非 nil）的字段被改写，其余保持库里的值。
func (req UpdateRequest) applyTo(cur RuleInfo) (RuleInfo, error) {
	out := cur

	if req.Scope != nil {
		out.Scope = *req.Scope
	}
	if req.OwnerUserID != nil {
		out.OwnerUserID = *req.OwnerUserID
	}
	if req.ChannelID != nil {
		out.ChannelID = *req.ChannelID
	}
	if req.Priority != nil {
		out.Priority = *req.Priority
	}
	if req.Enabled != nil {
		out.Enabled = *req.Enabled
	}
	if req.Expr != nil {
		out.Expr = *req.Expr
	}
	if req.Name != nil {
		name, err := normalizeName(*req.Name)
		if err != nil {
			return RuleInfo{}, err
		}
		out.Name = name
	}
	if req.ModelPatterns != nil {
		patterns, err := normalizePatterns(*req.ModelPatterns)
		if err != nil {
			return RuleInfo{}, err
		}
		out.ModelPatterns = patterns
	}
	if req.EffectiveFrom != nil {
		if req.EffectiveFrom.IsZero() {
			return RuleInfo{}, errors.New("effective_from 不能是零值时刻：" +
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

// RuleInfo 是管理面看到的一条规则。
//
// 字段集合由 admin_test.go 的一条用例逐个钉住：加字段会先让那条用例失败，
// 于是「这个新字段该不该出现在管理面的返回值里」会被问一次。
type RuleInfo struct {
	ID            int64
	Scope         string
	OwnerUserID   int64
	ChannelID     int64
	Name          string
	ModelPatterns []string
	Expr          string
	Priority      int
	EffectiveFrom time.Time
	// EffectiveTo 为 nil 表示不过期。
	EffectiveTo *time.Time
	Enabled     bool
	CreatedBy   int64
	CreatedAt   time.Time
	UpdatedAt   time.Time

	// DeclaredKeys 是表达式里写过的键（见 expr.Program.Keys），已排序。
	//
	// 它回答的是运维真正会问的那个问题：「这条规则收了哪几项的钱」。
	// 空数组意味着这条规则一项单价都没写——那时每一笔都按 0 元计，而结算
	// 那一侧的判据（billing.Prices.Declared）要到结算时才拦。ExprErr 非 nil
	// 时它也是空的：编译不过的表达式没有键可谈。
	DeclaredKeys []string

	// ExprErr 说明这条规则的表达式为什么编译不过，为 nil 表示能编译。
	//
	// 写入路径会把编译不过的表达式拒掉，所以它只可能出现在**绕过写入路径**
	// 写进库的行上。那种行每被用到一次就失败一次，而失败点在热路径上——
	// 列举是唯一能看见它的地方。
	ExprErr error
}

// List 列出全部规则，按**裁决次序**排列。
//
// 排序用的是 Match 的同一段 ORDER BY（见 ruleOrderBy），于是这个清单有一个
// 可以直接读出来的性质：**适用于某次请求的第一条，就是 Match 会选中的那条**。
// 要精确回答「这个模型现在按哪条计价、为什么是它」，用 Explain。
//
// 不分页：规则表不大（见 matchQuery 的注释），而一次列举是要被运维逐条看的。
//
// # 一行表达式坏了不影响别的行
//
// 编译失败只写进那一行的 ExprErr，**不跳过、也不整次报错**：整次报错会让
// 库里有一条坏规则时整个清单一条都看不到，而那正是最需要看清单的时候。
// 这与 channels.List 对一行坏密文的处置一致——那里是解不开的信封，
// 这里是编不过的表达式。
func (a *Admin) List(ctx context.Context) ([]RuleInfo, error) {
	rows, err := a.db.QueryContext(ctx, listQuery)
	if err != nil {
		return nil, fmt.Errorf("查询计价规则失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// 从空切片开始而不是 nil：一条规则都没配是正常结果，不是「没有值」。
	out := []RuleInfo{}
	for rows.Next() {
		info, scanErr := scanRuleInfo(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, decorate(info))
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历计价规则失败: %w", err)
	}
	return out, nil
}

// ExplainQuery 是「这次请求按哪条规则计价」的提问条件。
type ExplainQuery struct {
	// CanonicalModel 是规范模型名。
	CanonicalModel string
	// OwnerUserID 与 ChannelID 是这次请求的商家与渠道，0 表示未选定
	// （预留发生在选路之前，那时两个都是 0，见 pipeline.RuleQuery）。
	OwnerUserID int64
	ChannelID   int64
	// At 为零值时取当前时刻。
	At time.Time
}

// Explain 返回这次请求**全部适用**的规则，按裁决次序排列。
//
// 第一条就是 Match 会选中的那条，排在它后面的每一条都在回答「为什么不是
// 它」：它们要么作用域更笼统，要么优先级更低，要么是兜底规则（空的
// model_patterns）而胜出的那条按模型名配过，要么 id 更小。光给出胜出的
// 那条不够——运维还要知道有没有一条更该生效的规则被压住了。
//
// # 判据与次序与 Match 逐字相同
//
// 查询用的 WHERE 与 ORDER BY 与 Match 是同一段片段（ruleApplicable 与
// ruleOrderBy），两处不会各自漂移。这不是一句约定：admin_test.go 里有一条
// 用例拿同一批场景同时问 Match 与 Explain，断言两者的答案一致。
//
// 一条都不适用时返回 pipeline.ErrNoRule，与 Match 同一个错误——「没有生效的
// 规则」不是一个空清单，而是一次该被处置的拒绝服务。
func (a *Admin) Explain(ctx context.Context, q ExplainQuery) ([]RuleInfo, error) {
	if q.CanonicalModel == "" {
		return nil, errors.New("规范模型名为空")
	}
	// 查询没给时刻时用当前的，与 Source.now 同一个判据：零值时刻（公元 1 年）
	// 会让所有规则都还没生效。
	at := q.At
	if at.IsZero() {
		at = time.Now()
	}

	rows, err := a.db.QueryContext(ctx, explainQuery, at, q.CanonicalModel, q.OwnerUserID, q.ChannelID)
	if err != nil {
		return nil, fmt.Errorf("查询适用计价规则失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []RuleInfo{}
	for rows.Next() {
		info, scanErr := scanRuleInfo(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, decorate(info))
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历适用计价规则失败: %w", err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: 模型 %q 在渠道 %d 下没有生效的计价规则",
			pipeline.ErrNoRule, q.CanonicalModel, q.ChannelID)
	}
	return out, nil
}

// ── SQL ─────────────────────────────────────────────────────────────────

// ruleColumns 是管理面要看的全部列，三条查询共用（列举、裁决说明、修改前
// 的读锁）。表别名必须叫 r。
//
// owner_user_id / channel_id / created_by 用 COALESCE 折成 0：它们在库里是
// 可空的外键，而 NULL 与 0 在调用方那一侧是同一件事（见 nullableID）。
const ruleColumns = `
	    r.id,
	    r.scope,
	    COALESCE(r.owner_user_id, 0),
	    COALESCE(r.channel_id, 0),
	    r.name,
	    r.model_patterns,
	    r.expr,
	    r.priority,
	    r.effective_from,
	    r.effective_to,
	    r.enabled,
	    COALESCE(r.created_by, 0),
	    r.created_at,
	    r.updated_at`

// listQuery 列出全部规则，按裁决次序。
var listQuery = `
	SELECT ` + ruleColumns + `
	  FROM pricing_rules r
	 ORDER BY ` + ruleOrderBy

// explainQuery 是「哪些规则适用于这次请求」，按裁决次序。
//
// 判据与次序与 matchQuery 相同，区别只在于它取回全部适用的规则（没有
// LIMIT 1）与全部的列。占位符含义与 matchQuery 一致：$1 时刻、$2 模型名、
// $3 商家、$4 渠道。
var explainQuery = `
	SELECT ` + ruleColumns + `
	  FROM pricing_rules r
	 WHERE ` + ruleApplicable("$3", "$4") + `
	 ORDER BY ` + ruleOrderBy

// lockRuleQuery 读出一条规则并锁住它，供修改路径做读-改-写。
var lockRuleQuery = `
	SELECT ` + ruleColumns + `
	  FROM pricing_rules r
	 WHERE r.id = $1
	   FOR UPDATE`

// ── 读取与编译 ──────────────────────────────────────────────────────────

// rowScanner 同时覆盖 *sql.Row 与 *sql.Rows。
type rowScanner interface{ Scan(dest ...any) error }

// scanRuleInfo 把一行扫成 RuleInfo。它不填 DeclaredKeys 与 ExprErr，
// 那两个字段由 decorate 填。
func scanRuleInfo(row rowScanner) (RuleInfo, error) {
	var (
		info     RuleInfo
		patterns []byte
		to       sql.NullTime
	)
	err := row.Scan(
		&info.ID, &info.Scope, &info.OwnerUserID, &info.ChannelID, &info.Name,
		&patterns, &info.Expr, &info.Priority, &info.EffectiveFrom, &to,
		&info.Enabled, &info.CreatedBy, &info.CreatedAt, &info.UpdatedAt,
	)
	if err != nil {
		return RuleInfo{}, fmt.Errorf("读取计价规则行失败: %w", err)
	}

	// model_patterns 是 jsonb。它解不开说明这一行不是按表定义写进去的，
	// 与 channels.List 对 setting 的处置一致：整次报错，不猜。
	if err = json.Unmarshal(patterns, &info.ModelPatterns); err != nil {
		return RuleInfo{}, fmt.Errorf("规则 %d 的 model_patterns 不是合法 JSON 数组: %w", info.ID, err)
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

// decorate 编译一次表达式，填上 DeclaredKeys 与 ExprErr。
//
// 编译放在读出来的时候、而不是写入时留一份：留一份就要管它的失效，而列举
// 是运维动作、规则表不大，多花的是每次列举几毫秒的编译时间。
func decorate(info RuleInfo) RuleInfo {
	prog, err := expr.Compile(info.Expr)
	if err != nil {
		info.ExprErr = err
		return info
	}
	info.DeclaredKeys = prog.Keys()
	return info
}

// loadRuleForUpdate 读出一条规则的当前值，并锁住这一行。
//
// 读-改-写要在一个事务里：校验的对象是「改完之后的那一行」，而那一行由
// 「库里现在是什么」与「这次改什么」共同决定。分成两条语句的话，两个并发的
// 修改可以各自通过校验，合起来把这一行留成一个非法状态。
func loadRuleForUpdate(ctx context.Context, tx *sql.Tx, id int64) (RuleInfo, error) {
	info, err := scanRuleInfo(tx.QueryRowContext(ctx, lockRuleQuery, id))
	if errors.Is(err, sql.ErrNoRows) {
		return RuleInfo{}, fmt.Errorf("%w：id %d", ErrRuleNotFound, id)
	}
	if err != nil {
		return RuleInfo{}, err
	}
	return info, nil
}

// ── 校验与归一化 ────────────────────────────────────────────────────────
//
// 这一批函数是写入路径与读取路径「同一套判据」的兑现方式：什么算适用在
// ruleApplicable 里定下来，写入侧按同一组事实校验。校验失败时报**哪个
// 字段不对**与**为什么不生效**，因为这两件事运维都需要知道。

// validateRule 校验一行规则的形状。
//
// 判据分两类，都是「读取路径不看它」或「读取路径永远匹配不上」这两种
// 安静失败：
//
//   - 作用域与两栏 id 的组合。ruleApplicable 只按 scope 决定看哪一栏，
//     另一栏写了就是一条不生效的配置——配的人以为它有用。
//   - 时间窗口。左闭右开，上界不晚于下界的规则一条都匹配不上。
//
// 它**不判**「这条规则现在能不能匹配上某个具体模型」：一条按计划将来才生效
// 的规则是正常配置，见 Create 的注释。
func validateRule(r RuleInfo) error {
	switch r.Scope {
	case ScopePlatform:
		if r.OwnerUserID != 0 || r.ChannelID != 0 {
			return errors.New("platform 作用域的规则不挂商家也不挂渠道：" +
				"读取路径只按 owner_user_id IS NULL 匹配，另两栏写了不生效")
		}
	case ScopeMerchant:
		if r.OwnerUserID == 0 {
			return errors.New("merchant 作用域的规则必须给 owner_user_id：" +
				"读取路径按它匹配，没有它这条规则一条都匹配不上")
		}
		if r.ChannelID != 0 {
			return errors.New("merchant 作用域的规则不挂渠道：" +
				"读取路径按 owner_user_id 匹配，channel_id 写了不生效（要按渠道定价就用 channel 作用域）")
		}
	case ScopeChannel:
		if r.ChannelID == 0 {
			return errors.New("channel 作用域的规则必须给 channel_id：" +
				"读取路径按它匹配，没有它这条规则一条都匹配不上")
		}
		if r.OwnerUserID != 0 {
			return errors.New("channel 作用域的规则不挂 owner_user_id：" +
				"读取路径按 channel_id 匹配，owner 那一栏写了不生效（渠道的归属由渠道那一侧记着）")
		}
	default:
		return fmt.Errorf("作用域 %q 不是有效取值（只接受 %q、%q 与 %q）",
			r.Scope, ScopePlatform, ScopeMerchant, ScopeChannel)
	}

	if r.Name == "" {
		return errors.New("规则名不能为空：列举与告警都靠它指认一条规则")
	}
	if r.Expr == "" {
		return errors.New("表达式不能为空")
	}
	if r.EffectiveTo != nil && !r.EffectiveTo.After(r.EffectiveFrom) {
		return fmt.Errorf("effective_to（%s）必须晚于 effective_from（%s）："+
			"时间窗口是左闭右开，上界不晚于下界的规则一条都匹配不上",
			r.EffectiveTo.Format(time.RFC3339), r.EffectiveFrom.Format(time.RFC3339))
	}
	return nil
}

func normalizeName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("规则名不能为空")
	}
	return name, nil
}

// normalizePatterns 校验模型名名单。
//
// 空数组是合法的（适用于全部模型），与 channels 的 models 相反——那里的空
// 数组是一条永远选不中的废配置，这里的空数组是兜底规则的正常写法。
//
// 逐个拒掉的两种写法都会让这条规则**一条都匹配不上**，而它不报错：
//
//	空字符串   名单里有一项永远匹配不上任何模型名
//	通配符     `?` 运算符按元素逐字匹配，`claude-*` 只是一个普通字符串
//
// 第二种尤其值得挡：规则作者写通配时想要的是「claude 开头的一律用这个价」，
// 而实际结果是这条规则静默失效，请求退到下一层的价上——账面上表现为
// 「配了渠道价却按平台价收」，而排查的人会从请求那一侧找起。
func normalizePatterns(patterns []string) ([]string, error) {
	out := make([]string, 0, len(patterns))
	for i, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			return nil, fmt.Errorf("model_patterns 里第 %d 个模型名为空", i+1)
		}
		if strings.ContainsAny(p, "*?") {
			return nil, fmt.Errorf("model_patterns 里第 %d 个模型名含通配符：读取路径按元素逐字匹配，"+
				"通配符会被当成普通字符，这条规则一条都匹配不上", i+1)
		}
		out = append(out, p)
	}
	return out, nil
}

// nullableID 把 0 变成 NULL。
//
// owner_user_id、channel_id 与 created_by 都是可空的外键，而 0 会去匹配一个
// 不存在的行——外键直接拒绝，于是「平台自营」「不挂渠道」这些路径根本走不通。
func nullableID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// nullableTime 把 nil 变成 NULL。effective_to 为 NULL 表示不过期。
func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return *t
}
