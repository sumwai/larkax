// Package plans 管理额度包：运营发放的预存服务量，以及「谁在某段时间拥有它」。
//
// 一个**额度包**（已购/赠送的服务量）落两处：`plans` 记包本身（计量单位、可用量、
// 面值、模型范围），`plan_models` 记它能用在哪些模型上。一个**订阅**（`user_plans`）
// 记「某个用户在某段时间拥有某个包」，账上对应科目 `prepaid:<订阅 id>`。
//
// # 三种计量单位，一本账
//
// 定价永远只有一套（按 token 的单价），计量单位只决定这个包装了多少、还剩多少：
//
//	amount    金额（与账本同币种、同精度），一次调用按标价扣
//	token     token 数（输入 + 输出），按实际用量扣
//	request   调用次数，每次扣 1
//
// 账本上每个订阅只有一个数：钱。发放把面值记进 `prepaid:<订阅 id>`，消费从那里扣
// （分录形状与从用户钱包扣完全一样，见 ledger.Consume），作废把剩下的转回平台收入。
// 三种包因此在对账与商户分成上都不分叉。包「用完」的判据是**任一维度耗尽**：
// 可用量用完，或面值余额归零（用量列随扣费路径一起落，见 #860）。
//
// # 模型范围
//
// `plan_models` 按规范模型名精确匹配。**token / 次数包必须至少有一个模型**：
// 它们的价值与模型强相关，1 万 token 在贵模型与便宜模型上值不同的钱。金额包可以
// 为空，空表示不限模型——钱本来就不分模型。这条在写入时校验，不写成 CHECK：
// 它跨表，CHECK 看不见。
//
// # 这一层做什么、不做什么
//
// 做：额度包与订阅的增删查、发放、作废；一把 key 能用哪些订阅（`api_key_plans`，
// 空集合 = 全部）；一个用户的有效扣费顺序（用户覆盖或平台默认）。**不做**请求
// 路径上的选源与扣费，也不做售卖、周期续费与结转。本包不改计价规则。
//
// # 幂等
//
// 发放与作废各带一个外部单号（`ref` / `void_ref`），两者在 `user_plans` 上都唯一。
// 同一个单号重放到达时，函数返回库里那一行并说明这次没有改动（applied=false），
// 不会重复记账；同一个单号配不同的用户、包或到期时刻是**冲突**，不是重放。
package plans

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/dberr"
	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/ledger/store"
	"github.com/sumwai/larkax/internal/money"
)

// Service 是额度包的读写入口。
//
// 它持有两个句柄指向同一个库：自己的 `*sql.DB` 用来读写 plans / user_plans，
// `*store.Store` 用来把分录写进账本（幂等、交易锁、钱包快照都在那一层）。
// 分录的构造不在这里，在 internal/ledger（见 ledger.PlanGrant 与 ledger.PlanVoid）。
type Service struct {
	db     *sql.DB
	ledger *store.Store
	// log 是读侧回落时的日志出口（库里出现认不出的取值）。为 nil 时用
	// slog.Default()，测试可以换成一个收得到的句柄。
	log *slog.Logger
	// reader 非 nil 时读路径改走它（见 NewInTx），db 与 ledger 同时为 nil。
	reader queryer
}

// New 构造一个额度包服务。
func New(db *sql.DB) *Service { return &Service{db: db, ledger: store.New(db), log: slog.Default()} }

// NewInTx 构造一个读句柄指向 tx 的 Service。
//
// 它只服务「在一个事务里取齐多份数据」的读命令。**写方法在它上面没有句柄**
// （db 与 ledger 都为 nil），调用会当场 panic——宁可炸在错误的地方，也不要
// 让一次发放落到一条与调用方以为的事务无关的连接上。
func NewInTx(tx *sql.Tx) *Service { return &Service{reader: tx, log: slog.Default()} }

// read 返回读路径要用的句柄。
func (s *Service) read() queryer {
	if s.reader != nil {
		return s.reader
	}
	return s.db
}

// PlanUnit 是额度包的计量单位，取值与 `plans.unit` 的 CHECK 约束一致。
type PlanUnit string

const (
	// UnitAmount 是金额包：可用量就是钱，与面值同值。
	UnitAmount PlanUnit = "amount"
	// UnitToken 是按 token 计的包：可用量是输入加输出的 token 数。
	UnitToken PlanUnit = "token"
	// UnitRequest 是按次数计的包：可用量是调用次数。
	UnitRequest PlanUnit = "request"
)

func (u PlanUnit) valid() bool {
	switch u {
	case UnitAmount, UnitToken, UnitRequest:
		return true
	}
	return false
}

// UserPlanStatus 是订阅的状态，取值与 `user_plans.status` 的 CHECK 约束一致。
//
// active 之外的两种都是终态：作废与到期都不会再回到 active——要续，那是新的一次
// 发放、一行新订阅，因为账上那份面值已经结清了。
type UserPlanStatus string

const (
	// UserPlanActive 是有效：可以消费，也可以作废。
	UserPlanActive UserPlanStatus = "active"
	// UserPlanExpired 是到期：作废时由到期触发。
	UserPlanExpired UserPlanStatus = "expired"
	// UserPlanCancelled 是已作废：运营提前取消这个订阅。
	// 取值是 canceled：本仓的拼写统一走美式那一套（见 00016 的 client_canceled_at）。
	UserPlanCancelled UserPlanStatus = "canceled"
)

// Plan 是一个额度包（读写两侧共用这一种形状）。
type Plan struct {
	ID          int64
	Key         string
	Name        string
	Description string
	Asset       ledger.Asset
	Unit        PlanUnit
	// QuotaTotal 是按 Unit 计的可用量；金额包与 FaceValue 同值。
	QuotaTotal money.Amount
	// FaceValue 是进 prepaid 的面值：这个包值多少钱。
	FaceValue money.Amount
	// Models 是这个包能用的规范模型名。空表示不限模型——只有金额包允许为空。
	Models  []string
	Enabled bool
}

// UserPlan 是一个订阅，连同它「还剩多少」。
//
// 面值余量从账本现算（FaceValueRemaining）。按单位计的已用量不在这个结构里：
// user_plans 今天没有用量列。
type UserPlan struct {
	ID       int64
	UserID   int64
	PlanID   int64
	PlanKey  string
	Asset    ledger.Asset
	Unit     PlanUnit
	Status   UserPlanStatus
	StartsAt time.Time
	// ExpiresAt 是有效期终点；零值表示没有终点（列可空，运营发放时必填）。
	ExpiresAt  *time.Time
	QuotaTotal money.Amount
	FaceValue  money.Amount
	// FaceValueRemaining 是 prepaid:<ID> 科目现在的余额，即还剩多少面值。
	// 它从分录现算，不读缓存——账本是权威。
	FaceValueRemaining money.Amount
	// Ref 是发放单号，VoidRef 是作废单号（未作废时为空）。
	Ref     string
	VoidRef string
	// VoidedAt 是作废发生的时刻；未作废时为零值。剩余面值为零的作废不写分录，
	// 状态也只有一个终态值，对账要靠这个时间戳定位。
	VoidedAt *time.Time

	// 下面四列是期末按比例结转的两个方向。它们分成两组而不是共用两列，是因为
	// 任何不带状态过滤的查询都会把两边的数加到一起，而它们的含义正好相反。
	//
	// CarriedQuota / CarriedAmount 是**领到这条订阅时并进来的**结转量：来自用户
	// 之前那几条订阅的期末结转，进的是这条订阅的面值与可用量。它们只在一个
	// 活跃订阅上有意义，也是期末再结转时基数公式的减数——并进来的量不再结转
	// 第二次。
	CarriedQuota  money.Amount
	CarriedAmount money.Amount
	// CarryoutQuota / CarryoutAmount 是**这条订阅期末结转出去的**、等下次领包
	// 时并入新订阅的量。它们只在这条订阅已经终态（expired）且确实结转了的时候
	// 非零。
	//
	// 金额那一维的权威值是 carryover:<ID> 的**账本余额**，CarryoutAmount 只是
	// 那一刻的镜像：并入以余额为准，镜像只给读侧与对账看。
	// 量那一维没有账本可查（分录只有 asset，没有 unit），所以并入时**必须**从
	// CarryoutQuota 读——它在这里不是审计字段，是唯一来源。
	CarryoutQuota  money.Amount
	CarryoutAmount money.Amount
}

// ErrNoPlan 表示按 key 找不到那个额度包。
var ErrNoPlan = errors.New("没有这个额度包")

// ErrNoUserPlan 表示按 id 找不到那条订阅。
var ErrNoUserPlan = errors.New("没有这条额度包订阅")

// ErrUserPlanState 表示这条订阅现在的状态不允许这次动作。
//
// 它与 ErrNoUserPlan 分开：那一行在，只是已经作废或到期了。两个都是「先去看
// 库里现在是什么」，但它们要说的话不同——「不存在」与「已经有了结局」。
var ErrUserPlanState = errors.New("额度包订阅的状态不允许这次动作")

// ErrPlanScope 表示额度包的模型范围不合要求。
var ErrPlanScope = errors.New("额度包的模型范围不合要求")

// planColumns 是读一个额度包时选的列，顺序与 scanPlan 一一对应。
//
// 金额取 `::text`：金额列是 numeric(20, 8)，比 money 的定点数宽，直接扫成字符串
// 再由 money.Parse 解析，溢出时拿到的是「这个数读不出来」而不是一个被四舍五入
// 过的值（同 store 的 Wallets 那一组）。
const planColumns = `id, key, name, COALESCE(description, ''), asset, unit,
	quota_total::text, face_value::text, enabled`

// scanPlan 把一行额度包读成 Plan。收一个扫描函数，于是「读一行」与「读一批」
// 共用同一份解析。
func scanPlan(scan func(dest ...any) error) (Plan, error) {
	var (
		p                     Plan
		asset, unit           string
		quotaTotal, faceValue string
	)
	if err := scan(&p.ID, &p.Key, &p.Name, &p.Description, &asset, &unit,
		&quotaTotal, &faceValue, &p.Enabled); err != nil {
		return Plan{}, err
	}
	p.Asset = ledger.Asset(asset)
	if !p.Asset.Valid() {
		return Plan{}, fmt.Errorf("额度包 %d 的资产 %q 不受支持", p.ID, asset)
	}
	p.Unit = PlanUnit(unit)
	if !p.Unit.valid() {
		return Plan{}, fmt.Errorf("额度包 %d 的计量单位 %q 认不出", p.ID, unit)
	}
	total, err := parseMoney(quotaTotal, fmt.Sprintf("额度包 %d 的可用量", p.ID))
	if err != nil {
		return Plan{}, err
	}
	face, err := parseMoney(faceValue, fmt.Sprintf("额度包 %d 的面值", p.ID))
	if err != nil {
		return Plan{}, err
	}
	p.QuotaTotal = total
	p.FaceValue = face
	return p, nil
}

// parseMoney 解析一列金额，报错时把库里的原文带上。
// where 是「这是谁的哪个数」，例如「订阅 7 的剩余面值」。
func parseMoney(raw, where string) (money.Amount, error) {
	amt, err := money.Parse(raw)
	if err != nil {
		return money.Zero, fmt.Errorf("%s读不出来（库里的原文是 %q）: %w", where, raw, err)
	}
	return amt, nil
}

// CreatePlanInput 是新建一个额度包的输入。
type CreatePlanInput struct {
	Key         string
	Name        string
	Description string
	Asset       ledger.Asset
	Unit        PlanUnit
	// QuotaTotal 与 FaceValue 对于金额包只需要填一个：另一个按相等补齐。
	// 两个都填且不相等时报错——金额包的两个数在定义上就是同一个。
	QuotaTotal money.Amount
	FaceValue  money.Amount
	// Models 是可用的规范模型名。token / 次数包至少要有一个。
	Models []string
}

// CreatePlan 新建一个额度包，连同它的模型范围，在一个事务里写完。
//
// 返回的是库里的那一行（id、enabled 由数据库给），不是调用方传进来的样子。
func (s *Service) CreatePlan(ctx context.Context, in CreatePlanInput) (Plan, error) {
	p, err := normalizePlanInput(in)
	if err != nil {
		return Plan{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Plan{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	row, err := scanPlan(tx.QueryRowContext(ctx, `
		INSERT INTO plans (key, name, description, asset, unit, quota_total, face_value)
		VALUES ($1, $2, NULLIF($3, ''), $4, $5, $6::numeric, $7::numeric)
		RETURNING `+planColumns,
		p.Key, p.Name, p.Description, string(p.Asset), string(p.Unit),
		p.QuotaTotal.String(), p.FaceValue.String()).Scan)
	if err != nil {
		return Plan{}, dberr.ConstraintFailure(fmt.Errorf("建额度包失败: %w", err))
	}
	for _, model := range p.Models {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO plan_models (plan_id, model) VALUES ($1, $2)`,
			row.ID, model); err != nil {
			return Plan{}, fmt.Errorf("写额度包 %s 的模型范围失败: %w", row.Key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Plan{}, fmt.Errorf("提交额度包 %s 失败: %w", row.Key, err)
	}
	row.Models = p.Models
	return row, nil
}

// normalizePlanInput 校验并归一化一个新建额度包的输入。
//
// 归一化的两件事：金额包把只填了一个的可用量/面值补齐成同一个数；模型范围去重
// 并排序。排序是为了让「同一个包」在两次创建里落库的顺序一致——模型范围的比对
// 与展示都不该依赖调用方传进来的次序。
func normalizePlanInput(in CreatePlanInput) (Plan, error) {
	key := strings.TrimSpace(in.Key)
	if key == "" {
		return Plan{}, errors.New("额度包缺少 key")
	}
	if key != in.Key {
		return Plan{}, fmt.Errorf("额度包的 key %q 首尾有空白", in.Key)
	}
	if strings.ContainsAny(key, " \t\r\n") {
		return Plan{}, fmt.Errorf("额度包的 key %q 含空白字符", key)
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return Plan{}, errors.New("额度包缺少名称")
	}
	if !in.Asset.Valid() {
		return Plan{}, fmt.Errorf("资产 %q 不受支持（目前只有 %s）", in.Asset, ledger.AssetCNY)
	}
	if !in.Unit.valid() {
		return Plan{}, fmt.Errorf("计量单位 %q 认不出（只有 %s / %s / %s）",
			in.Unit, UnitAmount, UnitToken, UnitRequest)
	}

	total, face := in.QuotaTotal, in.FaceValue
	if in.Unit == UnitAmount {
		// 金额包的两个数是同一个：只填一个时按相等补齐，两个都给时必须相等。
		// 让它们可以不一致会让「可用量」与「面值」在同一个包上讲两个故事，
		// 而扣费只认面值——可用量那一栏就成了一句永远对不上的话。
		switch {
		case total.IsZero() && face.IsZero():
			return Plan{}, errors.New("金额包必须给出可用量（或面值）")
		case total.IsZero():
			total = face
		case face.IsZero():
			face = total
		case total != face:
			return Plan{}, fmt.Errorf("金额包的可用量 %s 与面值 %s 必须相等：它们定义上是同一个数",
				total, face)
		}
	}
	if !total.IsPositive() {
		return Plan{}, fmt.Errorf("可用量必须为正数，当前为 %s", total)
	}
	if !face.IsPositive() {
		return Plan{}, fmt.Errorf("面值必须为正数，当前为 %s", face)
	}

	models, err := normalizeModels(in.Models)
	if err != nil {
		return Plan{}, err
	}
	if in.Unit != UnitAmount && len(models) == 0 {
		return Plan{}, fmt.Errorf(
			"%w：%s 包至少要对一个模型开放。它的价值与模型强相关，"+
				"不限定范围就等于给出一个可能亏本的包", ErrPlanScope, in.Unit)
	}
	return Plan{
		Key:         key,
		Name:        name,
		Description: strings.TrimSpace(in.Description),
		Asset:       in.Asset,
		Unit:        in.Unit,
		QuotaTotal:  total,
		FaceValue:   face,
		Models:      models,
	}, nil
}

// normalizeModels 去掉空项与重复，按字典序返回。
func normalizeModels(models []string) ([]string, error) {
	seen := make(map[string]struct{}, len(models))
	out := make([]string, 0, len(models))
	for _, model := range models {
		m := strings.TrimSpace(model)
		if m == "" {
			return nil, errors.New("模型范围里有空的模型名")
		}
		if strings.ContainsAny(m, " \t\r\n") {
			return nil, fmt.Errorf("模型名 %q 含空白字符", m)
		}
		if _, dup := seen[m]; dup {
			continue
		}
		seen[m] = struct{}{}
		out = append(out, m)
	}
	sort.Strings(out)
	return out, nil
}

// Plans 读全部额度包（含禁用的），按 key 升序，每个包带上模型范围。
//
// 不过滤 enabled：与 UserPlans 同一条理由——运营要能同时看到停用的那些，
// 也才能把 key 重新启用。
func (s *Service) Plans(ctx context.Context) ([]Plan, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+planColumns+` FROM plans ORDER BY key`)
	if err != nil {
		return nil, fmt.Errorf("读额度包清单失败: %w", err)
	}

	var out []Plan
	for rows.Next() {
		p, err := scanPlan(rows.Scan)
		if err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("读额度包清单失败: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("遍历额度包清单失败: %w", err)
	}
	_ = rows.Close()

	// 模型范围在 rows 关掉之后逐个读：与上面共用一个连接池时，边遍历边发
	// 查询会把池子里的连接借到耗尽，而这一条命令没有并发压力，逐个读更稳。
	for i := range out {
		models, err := planModelsIn(ctx, s.db, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Models = models
	}
	return out, nil
}

// PlanByKey 按 key 读回一个额度包，连同它的模型范围。
//
// 禁用的包照样读得出来：运营要能看到它、也才能把它重新启用。能不能发放是
// Grant 的判断，不是这里的。
func (s *Service) PlanByKey(ctx context.Context, key string) (Plan, error) {
	p, err := planByKeyIn(ctx, s.db, key)
	if err != nil {
		return Plan{}, err
	}
	return p, nil
}

// planByKeyIn 在一个查询句柄上读额度包与模型范围。grant 的写路径也用它，
// 于是「一个包长什么样」只有一份解析。
func planByKeyIn(ctx context.Context, q queryer, key string) (Plan, error) {
	p, err := scanPlan(q.QueryRowContext(ctx,
		`SELECT `+planColumns+` FROM plans WHERE key = $1`, key).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Plan{}, fmt.Errorf("%w: key = %q", ErrNoPlan, key)
	}
	if err != nil {
		return Plan{}, fmt.Errorf("读额度包 %q 失败: %w", key, err)
	}
	models, err := planModelsIn(ctx, q, p.ID)
	if err != nil {
		return Plan{}, err
	}
	p.Models = models
	return p, nil
}

// planModelsIn 读一个额度包的模型范围（已排序）。
func planModelsIn(ctx context.Context, q queryer, planID int64) ([]string, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT model FROM plan_models WHERE plan_id = $1 ORDER BY model`, planID)
	if err != nil {
		return nil, fmt.Errorf("读额度包 %d 的模型范围失败: %w", planID, err)
	}
	defer func() { _ = rows.Close() }()

	var models []string
	for rows.Next() {
		var model string
		if err := rows.Scan(&model); err != nil {
			return nil, fmt.Errorf("读额度包 %d 的模型范围失败: %w", planID, err)
		}
		models = append(models, model)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历额度包 %d 的模型范围失败: %w", planID, err)
	}
	return models, nil
}

// queryer 是 *sql.DB 与 *sql.Tx 共有的那一半：只在事务内外复用同一段读逻辑时用。
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// SetPlanEnabledInput 是一次启用/禁用额度包的输入。
type SetPlanEnabledInput struct {
	Key     string
	Enabled bool
}

// SetPlanEnabled 启用或禁用一个额度包，返回改完的那一行。
//
// # 为什么是禁用而不是删除
//
// 已经发出去的订阅仍然要能按自己那份面值消费与作废：删掉包那一行，`prepaid:<订阅>`
// 就失去「它属于哪个包、什么单位、面值多少」的来源。禁用只影响**新的发放**，
// 已经发了的订阅不受影响——那份额度是发出去时就定下的。
//
// 它天然幂等：重复设成同一个值返回同一行。
func (s *Service) SetPlanEnabled(ctx context.Context, in SetPlanEnabledInput) (Plan, error) {
	key := strings.TrimSpace(in.Key)
	if key == "" {
		return Plan{}, errors.New("额度包缺少 key")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Plan{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	row, err := scanPlan(tx.QueryRowContext(ctx, `
		UPDATE plans SET enabled = $2, updated_at = now()
		 WHERE key = $1
		RETURNING `+planColumns, key, in.Enabled).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Plan{}, fmt.Errorf("%w: key = %q", ErrNoPlan, key)
	}
	if err != nil {
		return Plan{}, fmt.Errorf("改额度包 %q 的启用状态失败: %w", key, err)
	}
	models, err := planModelsIn(ctx, tx, row.ID)
	if err != nil {
		return Plan{}, err
	}
	row.Models = models
	if err := tx.Commit(); err != nil {
		return Plan{}, fmt.Errorf("提交额度包 %q 的启用状态失败: %w", key, err)
	}
	return row, nil
}

// userPlanColumns 是读一条订阅时选的列，顺序与 scanUserPlan 一一对应。
//
// # 剩余面值从分录现算
//
// `FaceValueRemaining` 不读快照：prepaid 没有快照行，它的余额就是科目分录之和。
// 子查询按科目原文精确匹配——prepaid 的后缀只放行一种写法（见 ledger.ParseAccount），
// 所以这里不需要 user 科目那样的别名归一。
const userPlanColumns = `up.id, up.user_id, up.plan_id, p.key, p.asset, p.unit,
	up.status, up.period_start, up.period_end,
	p.quota_total::text, p.face_value::text,
	COALESCE((SELECT SUM(le.amount) FROM ledger_entries le
	           WHERE le.account = 'prepaid:' || up.id::text AND le.asset = p.asset),
	         0)::numeric(20, 8)::text,
	up.ref, COALESCE(up.void_ref, ''), up.voided_at,
	up.carried_quota::text, up.carried_amount::text,
	up.carryout_quota::text, up.carryout_amount::text`

// scanUserPlan 把一行订阅读成 UserPlan。
func scanUserPlan(scan func(dest ...any) error) (UserPlan, error) {
	var (
		up                         UserPlan
		asset, unit, status        string
		periodEnd, voidedAt        sql.NullTime
		total, face, remain        string
		carriedQuota, carriedAmt   string
		carryoutQuota, carryoutAmt string
	)
	if err := scan(&up.ID, &up.UserID, &up.PlanID, &up.PlanKey, &asset, &unit,
		&status, &up.StartsAt, &periodEnd, &total, &face, &remain,
		&up.Ref, &up.VoidRef, &voidedAt,
		&carriedQuota, &carriedAmt, &carryoutQuota, &carryoutAmt); err != nil {
		return UserPlan{}, err
	}
	up.Asset = ledger.Asset(asset)
	if !up.Asset.Valid() {
		return UserPlan{}, fmt.Errorf("订阅 %d 的资产 %q 不受支持", up.ID, asset)
	}
	up.Unit = PlanUnit(unit)
	if !up.Unit.valid() {
		return UserPlan{}, fmt.Errorf("订阅 %d 的计量单位 %q 认不出", up.ID, unit)
	}
	up.Status = UserPlanStatus(status)
	switch up.Status {
	case UserPlanActive, UserPlanExpired, UserPlanCancelled:
	default:
		return UserPlan{}, fmt.Errorf("订阅 %d 的状态 %q 认不出", up.ID, status)
	}
	if periodEnd.Valid {
		at := periodEnd.Time
		up.ExpiresAt = &at
	}
	if voidedAt.Valid {
		at := voidedAt.Time
		up.VoidedAt = &at
	}
	var err error
	if up.QuotaTotal, err = parseMoney(total, fmt.Sprintf("订阅 %d 的可用量", up.ID)); err != nil {
		return UserPlan{}, err
	}
	if up.FaceValue, err = parseMoney(face, fmt.Sprintf("订阅 %d 的面值", up.ID)); err != nil {
		return UserPlan{}, err
	}
	if up.FaceValueRemaining, err = parseMoney(remain, fmt.Sprintf("订阅 %d 的剩余面值", up.ID)); err != nil {
		return UserPlan{}, err
	}
	if up.CarriedQuota, err = parseMoney(carriedQuota, fmt.Sprintf("订阅 %d 的结转进来量", up.ID)); err != nil {
		return UserPlan{}, err
	}
	if up.CarriedAmount, err = parseMoney(carriedAmt, fmt.Sprintf("订阅 %d 的结转进来面值", up.ID)); err != nil {
		return UserPlan{}, err
	}
	if up.CarryoutQuota, err = parseMoney(carryoutQuota, fmt.Sprintf("订阅 %d 的结转出去量", up.ID)); err != nil {
		return UserPlan{}, err
	}
	if up.CarryoutAmount, err = parseMoney(carryoutAmt, fmt.Sprintf("订阅 %d 的结转出去面值", up.ID)); err != nil {
		return UserPlan{}, err
	}
	return up, nil
}

// userPlanQuery 是读订阅的公共 FROM 子句：订阅与它所属的包必须一起读，
// 因为单位、可用量与面值长在包上。
const userPlanQuery = `SELECT ` + userPlanColumns + `
	FROM user_plans up JOIN plans p ON p.id = up.plan_id`

// UserPlans 读一个用户名下的全部订阅（有效、到期、已作废都在），按 id 升序。
//
// 不过滤状态：调用方常常要同时看「还剩多少」与「已经作废了几份」，而按状态过滤
// 会把后者藏起来。要不要只看有效的，是调用方一句话的事。
func (s *Service) UserPlans(ctx context.Context, userID int64) ([]UserPlan, error) {
	if userID <= 0 {
		return nil, fmt.Errorf("用户 id 非法: %d", userID)
	}
	rows, err := s.read().QueryContext(ctx,
		userPlanQuery+` WHERE up.user_id = $1 ORDER BY up.id`, userID)
	if err != nil {
		return nil, fmt.Errorf("读用户 %d 的额度包订阅失败: %w", userID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []UserPlan
	for rows.Next() {
		up, err := scanUserPlan(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("读用户 %d 的额度包订阅失败: %w", userID, err)
		}
		out = append(out, up)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历用户 %d 的额度包订阅失败: %w", userID, err)
	}
	return out, nil
}

// userPlanByIDIn 在一个事务里读回一条订阅，可选加行锁。
func userPlanByIDIn(ctx context.Context, tx *sql.Tx, userPlanID int64, forUpdate bool) (UserPlan, error) {
	query := userPlanQuery + ` WHERE up.id = $1`
	if forUpdate {
		query += ` FOR UPDATE OF up`
	}
	up, err := scanUserPlan(tx.QueryRowContext(ctx, query, userPlanID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return UserPlan{}, fmt.Errorf("%w: id = %d", ErrNoUserPlan, userPlanID)
	}
	if err != nil {
		return UserPlan{}, fmt.Errorf("读额度包订阅 %d 失败: %w", userPlanID, err)
	}
	return up, nil
}

// userPlanByRefIn 在一个事务里按发放单号读回一条订阅。
//
// 返回 found=false 表示这个单号还没有发过。单号唯一由库上的唯一索引保证，
// 所以最多只有一行。
func userPlanByRefIn(ctx context.Context, tx *sql.Tx, ref string) (UserPlan, bool, error) {
	up, err := scanUserPlan(tx.QueryRowContext(ctx,
		userPlanQuery+` WHERE up.ref = $1`, ref).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return UserPlan{}, false, nil
	}
	if err != nil {
		return UserPlan{}, false, fmt.Errorf("按发放单号读订阅失败: %w", err)
	}
	return up, true, nil
}
