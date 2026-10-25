package payment

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/money"
)

// PostgreSQL 的唯一约束冲突错误码，与 00064 迁移里那条部分唯一索引的名字。
//
// 只认错误码不够——23505 只说明「某个唯一约束被违反」，分不出撞的是单号
// （payment_orders_order_no_key）还是这条流水号索引。两者给人的下一步动作
// 也不同：前者看下单参数，后者要人工核两张单。
//
// 名字硬编码自 internal/migrate/sql/00064_payment_orders_gateway_txn_unique.sql。
// **改那条迁移就要同步改这里**，否则识别失效；失效的方向是安全的——认不出来
// 就按原错误上抛，不会静默当成功，只是少一层分类。
//
// 下单那条路撞不上这条索引：INSERT 不写 gateway_txn_id（orderForCreate 也不设
// 它），落库时这一列是 NULL，而索引只覆盖非空行。
const (
	pgUniqueViolation = "23505"
	// paymentOrdersGatewayTxnUnique 是 (gateway, gateway_txn_id) 的部分唯一索引。
	paymentOrdersGatewayTxnUnique = "payment_orders_gateway_txn_uniq_idx"
)

// Store 读写 payment_orders。它是这张表在 Go 侧唯一的入口。
type Store struct {
	db *sql.DB
	// lookupConflict 按 (gateway, gateway_txn_id) 找出已经记下这个流水号的那张单。
	//
	// 默认查库（NewStore 里装上）。抽成字段是为了让「补查失败」那一格能只让这
	// 一处失败：关掉共享的连接池会让同包后面的用例跟着报连接错误，而那种红看
	// 起来像被测代码坏了。
	//
	// **必须经 NewStore 构造**：直接写 `Store{db: db}` 字面量时这个字段是 nil，
	// 撞约束那一刻会 nil func panic——而那正是最需要说清楚原因的时刻。
	lookupConflict conflictLookup
}

// conflictLookup 是补查对方单号的能力，签名与 Store.conflictingOrderNo 一致。
type conflictLookup func(ctx context.Context, orderNo, gatewayTxnID string) (string, error)

// NewStore 包装一个已打开的数据库句柄。
func NewStore(db *sql.DB) *Store {
	s := &Store{db: db}
	s.lookupConflict = s.conflictingOrderNo
	return s
}

// orderDB 是 *sql.DB 与 *sql.Tx 共有的读写能力。
//
// 支付单的读与改既要在普通连接上做（读单、下单），也要在同一把锁的事务里做
// （通知的处理），两处共用这几个辅助函数，免得各写一份 SQL。
type orderDB interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// orderColumns 是读回一张支付单时取的列，顺序与 scanOrder 一一对应。
//
// 两个金额列取 `::text`：它们是 numeric(20, 8)，比 money 的定点数宽，直接扫成
// 字符串再由 money.Parse 解析，溢出时拿到的是「这个数读不出来」而不是一个被
// 四舍五入过的金额。
const orderColumns = `order_no, user_id, gateway, asset, amount::text, fee::text,
	status, gateway_txn_id, gateway_raw, paid_at, created_at, COALESCE(plan_key, ''),
	validity_days`

// scanOrder 把一行读成 Order。
func scanOrder(scan func(dest ...any) error) (Order, error) {
	var (
		o       Order
		asset   string
		amount  string
		fee     string
		status  string
		txnID   sql.NullString
		paidAt  sql.NullTime
		raw     []byte
		userID  int64
		orderNo string
		gateway string
		created time.Time
		days    sql.NullInt64
	)
	if err := scan(&orderNo, &userID, &gateway, &asset, &amount, &fee,
		&status, &txnID, &raw, &paidAt, &created, &o.PlanKey, &days); err != nil {
		return Order{}, err
	}
	if days.Valid {
		// int64 装得下天数（迁移上是 integer），窄化只在这一处。
		d := int(days.Int64)
		o.ValidityDays = &d
	}
	o.OrderNo = orderNo
	o.UserID = userID
	o.Gateway = gateway
	o.Asset = ledger.Asset(asset)
	if !o.Asset.Valid() {
		return Order{}, fmt.Errorf("支付单 %s 的资产 %q 不受支持", orderNo, asset)
	}
	parsed, err := money.Parse(amount)
	if err != nil {
		return Order{}, fmt.Errorf("支付单 %s 的金额读不出来（原文 %q）: %w", orderNo, amount, err)
	}
	o.Amount = parsed
	parsedFee, err := money.Parse(fee)
	if err != nil {
		return Order{}, fmt.Errorf("支付单 %s 的手续费读不出来（原文 %q）: %w", orderNo, fee, err)
	}
	o.Fee = parsedFee
	o.Status = Status(status)
	o.GatewayTxnID = txnID.String
	o.GatewayRaw = raw
	if paidAt.Valid {
		at := paidAt.Time
		o.PaidAt = &at
	}
	o.CreatedAt = created
	return o, nil
}

// Get 按单号读一张支付单。找不到时返回 ErrOrderNotFound。
func (s *Store) Get(ctx context.Context, orderNo string) (Order, error) {
	return getOrder(ctx, s.db, orderNo)
}

// getOrder 是 Get 的共用实现：连接与事务都能读。
func getOrder(ctx context.Context, db orderDB, orderNo string) (Order, error) {
	row, err := scanOrder(db.QueryRowContext(ctx,
		`SELECT `+orderColumns+` FROM payment_orders WHERE order_no = $1`, orderNo).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Order{}, fmt.Errorf("%w: %s", ErrOrderNotFound, orderNo)
	}
	if err != nil {
		return Order{}, fmt.Errorf("读支付单 %s 失败: %w", orderNo, err)
	}
	return row, nil
}

// Create 落一张新的支付单。
//
// 单号已经存在时按内容判定：同一份内容算重放（返回 applied=false 与库里那一行），
// 内容不同返回 ErrOrderConflict。判据放在唯一约束上而不是「先查再插」：后者
// 中间有窗口，两个并发下单会各自看到「不存在」然后各插一行。
func (s *Store) Create(ctx context.Context, o Order) (Order, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Order{}, false, fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stored, applied, err := s.CreateTx(ctx, tx, o)
	if err != nil {
		return Order{}, false, err
	}
	if commitErr := tx.Commit(); commitErr != nil {
		return Order{}, false, fmt.Errorf("提交支付单 %s 失败: %w", o.OrderNo, commitErr)
	}
	return stored, applied, nil
}

// CreateTx 在调用方给的事务里落一张新的支付单，**不提交、不回滚**。
//
// 除事务边界外，语义与 Create 逐条相同：同一份 SQL、同一条重放/冲突判据。
// 预览（`wallet payment create --dry-run`）用它把那一行走一遍再回滚，于是预览
// 里的字段与真跑同源，而不是另写一份读侧模拟。
func (s *Store) CreateTx(ctx context.Context, tx *sql.Tx, o Order) (Order, bool, error) {
	var id int64
	insertErr := tx.QueryRowContext(ctx, `
		INSERT INTO payment_orders (order_no, user_id, gateway, asset, amount, fee, status, gateway_raw, plan_key, validity_days)
		VALUES ($1, $2, $3, $4, $5::numeric, $6::numeric, $7, $8::jsonb, $9, $10)
		ON CONFLICT (order_no) DO NOTHING
		RETURNING id`,
		o.OrderNo, o.UserID, o.Gateway, string(o.Asset),
		o.Amount.String(), o.Fee.String(), string(StatusCreated), rawOrDefault(o.GatewayRaw),
		nullString(o.PlanKey), nullInt(o.ValidityDays),
	).Scan(&id)
	if insertErr == nil {
		// 读回库里那一行：created_at 等列由数据库补，入参里的同一字段是零值。
		stored, readErr := getOrder(ctx, tx, o.OrderNo)
		if readErr != nil {
			return Order{}, false, fmt.Errorf("读回刚写的支付单 %s 失败: %w", o.OrderNo, readErr)
		}
		return stored, true, nil
	}
	if !errors.Is(insertErr, sql.ErrNoRows) {
		return Order{}, false, fmt.Errorf("写支付单 %s 失败: %w", o.OrderNo, insertErr)
	}

	// ON CONFLICT DO NOTHING 没拿到行：单号已经存在。读回那一行再比内容。
	// 用同一个事务读，是为了拿到与插入尝试一致的视图。
	stored, err := scanOrder(tx.QueryRowContext(ctx,
		`SELECT `+orderColumns+` FROM payment_orders WHERE order_no = $1`, o.OrderNo).Scan)
	if err != nil {
		return Order{}, false, fmt.Errorf("读已有的支付单 %s 失败: %w", o.OrderNo, err)
	}
	if stored.UserID != o.UserID || stored.Gateway != o.Gateway ||
		stored.Asset != o.Asset || stored.Amount.Cmp(o.Amount) != 0 ||
		stored.PlanKey != o.PlanKey || !sameValidityDays(stored.ValidityDays, o.ValidityDays) {
		return Order{}, false, fmt.Errorf(
			"%w: 支付单 %s 已经存在，但内容与这次要写的不一样"+
				"（库里 用户 %d / 通道 %s / %s %s / 额度包 %q %s，这次 用户 %d / 通道 %s / %s %s / 额度包 %q %s）",
			ErrOrderConflict, o.OrderNo,
			stored.UserID, stored.Gateway, stored.Amount, stored.Asset, stored.PlanKey,
			validityDaysText(stored.ValidityDays),
			o.UserID, o.Gateway, o.Amount, o.Asset, o.PlanKey,
			validityDaysText(o.ValidityDays))
	}
	return stored, false, nil
}

// BeginTx 在支付单库上开一个事务，给「写完就回滚」的预览路径用。
//
// 预览也要走 CreateTx 那条写路径（同一份 SQL 与判据），它需要一个事务来装；
// 由 Store 开而不是把 *sql.DB 交出去，是为了不让调用方绕开这一层直接写表。
func (s *Store) BeginTx(ctx context.Context) (*sql.Tx, error) {
	return s.db.BeginTx(ctx, nil)
}

// WithOrderLock 在同一个单号上串行执行 fn。
//
// 锁是事务级的 `pg_advisory_xact_lock`，键由单号推出：fn 返回后事务提交，
// 锁随之释放；fn 返回错误时事务回滚，锁同样释放。**锁在账本写入之前拿到**，
// 所以同一张单子的两份通知不会交错成「钱写了、状态没改」。
//
// 回调把账本写入也放进这个事务（`LockedOrder.Tx` 交给
// `PostTransactionTx`），于是读单、写分录、改状态三者同生共死：一条通知只占
// 一条连接，也不会出现其中一处落库、另一处没落的中间态。
func (s *Store) WithOrderLock(ctx context.Context, orderNo string, fn func(context.Context, *LockedOrder) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启支付单 %s 的事务失败: %w", orderNo, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, orderLockKey(orderNo)); err != nil {
		return fmt.Errorf("锁定支付单 %s 失败: %w", orderNo, err)
	}
	if err := fn(ctx, &LockedOrder{tx: tx}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交支付单 %s 的状态失败: %w", orderNo, err)
	}
	return nil
}

// orderLockKey 给单号加一个域前缀，避免与账本按 txn_id 取的锁撞在同一组键上。
func orderLockKey(orderNo string) string {
	return "larkax:payment-order:v1:" + orderNo
}

// LockedOrder 是持锁事务里的支付单视图。
//
// 它的方法只在 WithOrderLock 的回调里用：状态判断与改状态因此处在同一个
// 串行区间内。
type LockedOrder struct {
	tx *sql.Tx
}

// Tx 交出这把锁所在的事务。
//
// 只给「必须与状态改动同生共死」的写入用——目前只有账本写入
// （`PostTransactionTx`）。提交与回滚由 WithOrderLock 负责，调用方不要自己
// 结束这个事务。
func (l *LockedOrder) Tx() *sql.Tx { return l.tx }

// Get 读一张支付单。
func (l *LockedOrder) Get(ctx context.Context, orderNo string) (Order, error) {
	return getOrder(ctx, l.tx, orderNo)
}

// MarkPaid 把一张 `created` 的支付单转成 `paid`，并记下通道流水与通知原文。
//
// 只改 `created` 那一行（WHERE 里带着状态）：两张通知并发到达时，后一张不会
// 覆盖前一张记下的流水号。返回 false 表示这一行已经不在 `created` 上了。
//
// 撞上「同一个通道流水号只能记在一张单上」那条约束时返回
// *GatewayTxnConflictError，不把 SQLSTATE 抛给调用方。这里不自己补读对方单号：
// 23505 已经让整个事务中止，同一条连接上再查什么都只会得到「current
// transaction is aborted」——那一步在事务回滚之后做（见
// Store.annotateGatewayTxnConflict）。
func (l *LockedOrder) MarkPaid(ctx context.Context, orderNo, gateway, gatewayTxnID string, raw []byte, paidAt time.Time, fee money.Amount) (bool, error) {
	res, err := l.tx.ExecContext(ctx, `
		UPDATE payment_orders
		SET status = 'paid', gateway_txn_id = $2, gateway_raw = $3::jsonb, paid_at = $4, fee = $5::numeric
		WHERE order_no = $1 AND status = 'created'`,
		orderNo, nullString(gatewayTxnID), rawOrDefault(raw), paidAt, fee.String())
	if err != nil {
		if gatewayTxnUniqueViolated(err) {
			return false, &GatewayTxnConflictError{
				OrderNo:      orderNo,
				Gateway:      gateway,
				GatewayTxnID: gatewayTxnID,
			}
		}
		return false, fmt.Errorf("更新支付单 %s 为已支付失败: %w", orderNo, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("读取支付单 %s 的更新行数失败: %w", orderNo, err)
	}
	return n > 0, nil
}

// gatewayTxnUniqueViolated 报告 err 是不是撞在 (gateway, gateway_txn_id) 那条
// 部分唯一索引上。
//
// 只判 23505 会把任何唯一约束冲突都翻译成「流水号被占用」；这一条要求错误码
// 与索引名同时对上，与 internal/auth、internal/catalog 判各自唯一约束的做法
// 一致。
func gatewayTxnUniqueViolated(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) &&
		pgErr.Code == pgUniqueViolation &&
		pgErr.ConstraintName == paymentOrdersGatewayTxnUnique
}

// annotateGatewayTxnConflict 给流水号冲突补上「撞的是哪一张单」。
//
// 读放在事务回滚之后：23505 中止了整个事务，唯一能查的时候是 WithOrderLock
// 交还连接之后。这次读只为了让报错更好认，**不能因为它的失败把原始冲突吞掉**，
// 也不能把它升级成 5xx——5xx 会让通道以为可以无限重试。读不到（或这次读本身
// 报错）时保留原错误、只少一个对方单号，并记一条日志说明补查没成。
func (s *Store) annotateGatewayTxnConflict(ctx context.Context, err error) error {
	var conflict *GatewayTxnConflictError
	if !errors.As(err, &conflict) || conflict.OtherOrderNo != "" {
		return err
	}
	other, lookupErr := s.lookupConflict(ctx, conflict.OrderNo, conflict.GatewayTxnID)
	if lookupErr != nil {
		slog.Default().Warn("支付单的通道流水号冲突，但补查对方单号失败",
			"err", lookupErr, "order_no", conflict.OrderNo,
			"gateway", conflict.Gateway, "gateway_txn_id", conflict.GatewayTxnID)
		return err
	}
	conflict.OtherOrderNo = other
	return err
}

// conflictingOrderNo 是默认的补查：按当前那张单的通道，找记下同一个流水号的
// 另一行。
//
// 这不是判据，是诊断信息：钱与状态那一侧的守卫是唯一索引本身。
func (s *Store) conflictingOrderNo(ctx context.Context, orderNo, gatewayTxnID string) (string, error) {
	var other string
	err := s.db.QueryRowContext(ctx, `
		SELECT other.order_no
		  FROM payment_orders AS mine
		  JOIN payment_orders AS other
		    ON other.gateway = mine.gateway
		   AND other.gateway_txn_id = $2
		 WHERE mine.order_no = $1
		   AND other.order_no <> mine.order_no
		 LIMIT 1`, orderNo, gatewayTxnID).Scan(&other)
	return other, err
}

// MarkTerminal 把一张 `created` 的支付单转成失败或过期。
//
// 只接受 `failed` 与 `expired`：`paid` 要走 MarkPaid（它还要记流水与手续费），
// `refunded` 是已支付之后的事，不能从这里直接跳到。
func (l *LockedOrder) MarkTerminal(ctx context.Context, orderNo string, to Status, raw []byte) (bool, error) {
	if to != StatusFailed && to != StatusExpired {
		return false, fmt.Errorf("支付单 %s 不能从 created 直接转成 %q", orderNo, to)
	}
	res, err := l.tx.ExecContext(ctx, `
		UPDATE payment_orders SET status = $2, gateway_raw = $3::jsonb
		WHERE order_no = $1 AND status = 'created'`,
		orderNo, string(to), rawOrDefault(raw))
	if err != nil {
		return false, fmt.Errorf("更新支付单 %s 为 %s 失败: %w", orderNo, to, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("读取支付单 %s 的更新行数失败: %w", orderNo, err)
	}
	return n > 0, nil
}

// WalletExists 报告用户有没有这个资产的钱包行。
func (s *Store) WalletExists(ctx context.Context, userID int64, asset ledger.Asset) (bool, error) {
	var exists bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM user_wallets WHERE user_id = $1 AND asset = $2)`,
		userID, string(asset)).Scan(&exists); err != nil {
		return false, fmt.Errorf("查用户 %d 的 %s 钱包失败: %w", userID, asset, err)
	}
	return exists, nil
}

// ChannelOrder 把本地这一行折成通道侧查询的答案。
//
// 假通道没有自己的账本，它答的就是这里记下的那张单子（见 fakechannel 的说明）。
// 真实通道由适配器自己发请求，不走这一条。
func (s *Store) ChannelOrder(ctx context.Context, orderNo string) (ChannelOrder, error) {
	o, err := s.Get(ctx, orderNo)
	if err != nil {
		return ChannelOrder{}, err
	}
	return ChannelOrder{
		OrderNo:      o.OrderNo,
		Status:       o.Status,
		GatewayTxnID: o.GatewayTxnID,
		PaidAt:       o.PaidAt,
	}, nil
}

// nullString 把空串写成 NULL。
//
// gateway_txn_id 为空表示「通道还没有给流水号」，与空串不是一回事：后者是一
// 个有值的字段，只是值是空的，而按这个字段去对通道账单时两种情形的下一步不同。
func nullString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// nullInt 把「没有这个值」写成 NULL。
//
// 天数是售卖单才有的列（迁移上允许 NULL，充值单就是 NULL），而「0 天」本身
// 不合法，所以用指针表达「没有」，不拿 0 顶替。
func nullInt(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

// sameValidityDays 比较两边的售卖天数，把「都没有」与「都有且相等」都算相等。
//
// 不能直接比指针（那是地址），也不能先解引用（nil 会 panic）。充值单两边都是
// nil，售卖单两边都有值——混着比才是冲突。
func sameValidityDays(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// validityDaysText 把天数打给人看，「没有」时留空而不是打 0。
func validityDaysText(v *int) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%d 天", *v)
}

// rawOrDefault 保证 gateway_raw 拿到的是合法 JSON。
//
// 那一列是 jsonb 且 NOT NULL：空字节串写进去会被数据库拒绝，而报错是
// 「invalid input syntax for type json」，看不出是哪条命令的哪一步。
func rawOrDefault(raw []byte) []byte {
	if len(raw) == 0 {
		return []byte(`{}`)
	}
	return raw
}
