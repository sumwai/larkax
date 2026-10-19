package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/money"
)

// 本文件是提现单（payout_requests）的读写口，也是**唯一**一处把提现单与它那
// 组分录写在一起的地方。
//
// # 为什么单子与分录必须在同一个事务里
//
// 提现单那一行说的是「商户申请了 A」，`payout:<单id>` 那个科目说的是「这 A 已经
// 从商户的分成里挪出来、等着付」。两条事实缺一条，账面就有一个说得通而实际不对
// 的状态：
//
//   - 只有单子、没有分录：这张单子看起来可以被批准并打款，而它其实一分钱都
//     没冻住——出账时 `payout:<单id>` 从零被扣成负数，那笔钱没有来处。
//   - 只有分录、没有单子：钱冻在 `payout:<单id>` 上，而没有任何一行说明它是谁
//     申请、该不该付；它只会以「payout 科目长期不平」的形式在某个对账日出现。
//
// 所以建单、审批、驳回、打款四步都是「一个事务、要么全写要么全不写」。
//
// # 状态怎么往前走
//
//	pending --批准--> approved --打款--> paid
//	    |
//	    +--驳回--> rejected（同时把冻结解除）
//
// `paid` 那一步**只认 approved**：这是本层唯一的出账门，别的路径都到不了它
// （见 PayPayout）。`rejected` 与 `paid` 是终态：驳回过的单子不会再被打款，
// 打款过的单子也不会再被驳回。
//
// # 幂等键在这一层是 txn_id
//
// 四个动作各自由调用方把「同一个业务事件」映射成一个 txn_id（命令行拿幂等键
// 与用途前缀算出来）。写分录的那三步（申请 / 驳回 / 打款）因此天然幂等：重放到
// 达时那笔交易已经在库里、内容一致，函数返回 applied=false 且什么都不写。
// 批准不写分录，它的幂等由状态挡着：已经是 approved 时返回未改动。

// PayoutStatus 是 payout_requests.status 的取值，与迁移里的 CHECK 约束一致。
//
// 它是一个具名类型而不是裸 string：这一列决定「这条单子现在能不能被打款」，
// 而在 Go 侧写错一个字母（`aprpoved`）不会报错，只会让所有比较都落空、
// 于是每条单子看起来都还停在上一步。
type PayoutStatus string

const (
	// PayoutPending 是刚申请、还没人看过。
	PayoutPending PayoutStatus = "pending"
	// PayoutApproved 是审批通过、等着打款。
	PayoutApproved PayoutStatus = "approved"
	// PayoutRejected 是审批驳回，那一笔冻结已经解除。
	PayoutRejected PayoutStatus = "rejected"
	// PayoutPaid 是已经打款出去，`payout:<单id>` 已核销。
	PayoutPaid PayoutStatus = "paid"
)

// Label 给状态值一个中文说法，供错误信息与回执用。
//
// 库里那一列存的是英文取值（pending / approved / …），而这两处都是打给人看的
// 句子；两个都给出来（`pending（待审批）`），读的人既认得库里的值，也看得懂
// 现在停在哪一步。库里将来多出来的取值（例如某个取消状态）照原样打，不猜中文。
//
// 它是导出方法而不是本文件里的一个私有函数：错误信息由本包写，回执由命令行写，
// 两处都要说这同一个状态——收在两处各写一份的话，某一处加了新状态之后，另一处
// 会安静地把那个值原样打出来。
func (s PayoutStatus) Label() string {
	switch s {
	case PayoutPending:
		return "待审批"
	case PayoutApproved:
		return "已批准"
	case PayoutRejected:
		return "已驳回"
	case PayoutPaid:
		return "已打款"
	}
	return string(s)
}

// Payout 是一条提现单。
//
// 它是**读侧**的样子：写路径上的每一步都由下面几个函数返回它，于是命令行拿到
// 的是「库里现在这一行是什么」，而不是它自己以为写完的样子（时刻、id 都由
// 数据库生成）。
type Payout struct {
	// ID 是 payout_requests.id，也是账本里 payout:<ID> 那个科目的后缀——
	// 冻结挂在这个 id 上，所以它是审批与打款要接过去的那个值。
	ID int64
	// UserID 是申请提现的商户（payout_requests.merchant_user_id）。
	UserID int64
	Asset  ledger.Asset
	Amount money.Amount
	// Destination 是商户的收款信息（jsonb 原文）。本包不解释它的内容：
	// 打款通道要什么字段是通道那一侧的事。
	Destination []byte
	Status      PayoutStatus
	// ReviewedBy / ReviewedAt 是审批人与时刻，**只有走过审批才有值**。
	ReviewedBy *int64
	ReviewedAt *time.Time
	// ReviewNote 是审批时写的说明（驳回原因落在这一栏）。
	ReviewNote string
	// PaidAt / PayoutRef 是打款时刻与通道给的凭据号，只有打过款才有值。
	PaidAt    *time.Time
	PayoutRef string
	CreatedAt time.Time
}

// ErrNoPayoutRequest 表示按 id 找不到那条提现单。
//
// 与 ErrNoWallet 同一个形状：要改的那一行不在库里。调用方（命令行）把它归入
// 「要查的那一行不存在」那一类，退出码 4——下一步是去看库里现在有哪些单子，
// 而不是改一下命令重发。
var ErrNoPayoutRequest = errors.New("没有这条提现单")

// ErrPayoutState 表示这条提现单现在的状态不允许这次动作。
//
// 它与 ErrNoPayoutRequest 分开，因为处置不同：那一行在，只是停在了别的步骤上
// （例如对一张已经打款的单子再打一次款）。两个都归退出码 4：下一步都是先看
// 清那一行现在是什么，再决定怎么做。
//
// **它不是参数错**：命令没有写错，是数据不在那个状态上。
var ErrPayoutState = errors.New("提现单的状态不允许这次动作")

// payoutColumns 是读回一条提现单时取的列，顺序与 scanPayout 一一对应。
//
// amount 取 `::text`：金额列是 numeric(20, 8)，比 money 的定点数宽，直接扫成
// 字符串再由 money.Parse 解析，溢出时拿到的是「这个数读不出来」而不是一个被
// 四舍五入过的余额（同 Wallets 那一组的理由）。
const payoutColumns = `id, merchant_user_id, asset, amount::text, destination, status,
	reviewed_by, reviewed_at, review_note, paid_at, payout_ref, created_at`

// scanPayout 把一行提现单读成 Payout。
//
// 收的是一个扫描函数（`row.Scan` 或 `rows.Scan`），于是「读一行」与「读一批」
// 共用同一份解析；两处各写一遍的话，加一列时必然有一处漏掉。
func scanPayout(scan func(dest ...any) error) (Payout, error) {
	var (
		p          Payout
		asset      string
		amount     string
		status     string
		reviewedBy sql.NullInt64
		reviewedAt sql.NullTime
		reviewNote sql.NullString
		paidAt     sql.NullTime
		payoutRef  sql.NullString
	)
	if err := scan(&p.ID, &p.UserID, &asset, &amount, &p.Destination, &status,
		&reviewedBy, &reviewedAt, &reviewNote, &paidAt, &payoutRef, &p.CreatedAt); err != nil {
		return Payout{}, err
	}
	p.Asset = ledger.Asset(asset)
	if !p.Asset.Valid() {
		return Payout{}, fmt.Errorf("提现单 %d 的资产 %q 不受支持", p.ID, asset)
	}
	parsed, err := money.Parse(amount)
	if err != nil {
		return Payout{}, fmt.Errorf("提现单 %d 的金额读不出来（库里的原文是 %q）: %w", p.ID, amount, err)
	}
	p.Amount = parsed
	p.Status = PayoutStatus(status)
	if reviewedBy.Valid {
		id := reviewedBy.Int64
		p.ReviewedBy = &id
	}
	if reviewedAt.Valid {
		at := reviewedAt.Time
		p.ReviewedAt = &at
	}
	p.ReviewNote = reviewNote.String
	if paidAt.Valid {
		at := paidAt.Time
		p.PaidAt = &at
	}
	p.PayoutRef = payoutRef.String
	return p, nil
}

// payoutByID 在一把行锁下读回一条提现单。
//
// FOR UPDATE 是必须的：审批、驳回、打款三个动作都要先看清这一行现在是什么状态
// 再决定能不能往前走，而「先读后写」之间有一个窗口——不加锁的话，一次驳回与
// 一次打款可以各自读到 approved，各自往下走，最后这笔钱既被驳回又被打款。
func payoutByID(ctx context.Context, tx *sql.Tx, payoutID int64) (Payout, error) {
	row, err := scanPayout(tx.QueryRowContext(ctx,
		`SELECT `+payoutColumns+` FROM payout_requests WHERE id = $1 FOR UPDATE`,
		payoutID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Payout{}, fmt.Errorf("%w: id = %d", ErrNoPayoutRequest, payoutID)
	}
	if err != nil {
		return Payout{}, fmt.Errorf("读提现单 %d 失败: %w", payoutID, err)
	}
	return row, nil
}

// payoutStateError 报「这条提现单的状态不允许这次动作」。
func payoutStateError(row Payout, action string, want PayoutStatus) error {
	return fmt.Errorf("%w: 提现单 %d 现在是 %s（%s）状态，%s要求它处于 %s（%s）状态",
		ErrPayoutState, row.ID, row.Status, row.Status.Label(),
		action, want, want.Label())
}

// payoutIDOf 从一组已有的分录里取出它关联的提现单 id。
//
// 用它的是申请那一步的重放路径：单子的 id 只在建单之后才知道，而重放时那行
// 已经建过了——好在建单与写分录在同一个事务里，所以「这笔交易有分录」与
// 「这行单子存在」是同一件事，而分录上带着 payout_id（见 PayoutRequest 的
// withPayout）。
//
// 读不出 id 时报错而不是猜一个：走到这条路的交易只可能是这条命令自己写的
// （域分隔前缀只此一处），真读不出来说明库里的数据被别的写入路径改过，
// 而那件事不能靠猜一个 id 掩过去。
func payoutIDOf(entries []ledger.Entry) (int64, error) {
	for _, e := range entries {
		if e.PayoutID != nil {
			return *e.PayoutID, nil
		}
	}
	return 0, fmt.Errorf("这笔交易的分录里没有 payout_id，读不出它是哪条提现单（%d 条分录）",
		len(entries))
}

// RequestPayoutInput 是一次提现申请的输入。
type RequestPayoutInput struct {
	// TxnID 由调用方按幂等键（外部申请单号）推出来。同一个键重放到达时，
	// 这次调用什么都不写，返回 applied=false。
	TxnID ledger.TxnID
	// UserID 是申请提现的商户。它必须是 users 里真实存在的一行：那一列有
	// 外键（命令行在更早一步就把标识解析成了用户）。
	UserID int64
	Amount money.Amount
	Asset  ledger.Asset
	// Destination 是收款信息，必须是合法 JSON（列是 jsonb）。留空表示
	// 「这一栏还没有内容」，落库成 `{}`——那是列的默认值，不是把空值当合法。
	Destination []byte
}

// RequestPayout 建一条提现单，并把商户的分成挪进这条单子的待付。
//
// 分录（借为正、贷为负，见 ledger.PayoutRequest）：
//
//	贷 merchant_payable:<商户>   A    他还能申请的钱少了一笔
//	借 payout:<单id>             A    这条单子的待付多了一笔
//
// 返回的 applied=false 表示这个申请单号已经建过单（重放），库里那一行原样返回。
func (s *Store) RequestPayout(ctx context.Context, in RequestPayoutInput) (Payout, bool, error) {
	if in.TxnID == "" {
		return Payout{}, false, errors.New("缺少 txn_id：申请单号要确定性映射成它，幂等靠它承担")
	}
	if in.UserID <= 0 {
		return Payout{}, false, fmt.Errorf("商户 id 非法: %d", in.UserID)
	}
	if !in.Amount.IsPositive() {
		return Payout{}, false, fmt.Errorf("提现金额必须为正数，当前为 %s", in.Amount)
	}
	if !in.Asset.Valid() {
		return Payout{}, false, fmt.Errorf("资产 %q 不受支持（目前只有 %s）", in.Asset, ledger.AssetCNY)
	}
	if len(in.Destination) > 0 && !json.Valid(in.Destination) {
		return Payout{}, false, errors.New("收款信息不是合法的 JSON")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Payout{}, false, fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if lockErr := lockTxn(ctx, tx, in.TxnID); lockErr != nil {
		return Payout{}, false, lockErr
	}

	stored, err := txnEntriesInTx(ctx, tx, in.TxnID)
	if err != nil {
		return Payout{}, false, err
	}
	if len(stored) > 0 {
		// 重放：单子已经建过，它的 id 记在分录上（见 payoutIDOf）。
		payoutID, idErr := payoutIDOf(stored)
		if idErr != nil {
			return Payout{}, false, idErr
		}
		// 拿这次要写的分录与库里那批比一遍：同一个单号配不同的金额、商户或
		// 资产不是重放，而是冲突（ErrTxnConflict），一笔都不写。
		expected, buildErr := ledger.PayoutRequest(ledger.PayoutRequestParams{
			TxnID:    in.TxnID,
			PayoutID: payoutID,
			UserID:   in.UserID,
			Amount:   in.Amount,
			Asset:    in.Asset,
		})
		if buildErr != nil {
			return Payout{}, false, buildErr
		}
		if conflictErr := sameStoredTxn(stored, expected); conflictErr != nil {
			return Payout{}, false, conflictErr
		}
		row, readErr := payoutByID(ctx, tx, payoutID)
		if readErr != nil {
			return Payout{}, false, readErr
		}
		return row, false, nil
	}

	row, err := insertPayoutRequest(ctx, tx, in)
	if err != nil {
		return Payout{}, false, err
	}
	txn, err := ledger.PayoutRequest(ledger.PayoutRequestParams{
		TxnID:    in.TxnID,
		PayoutID: row.ID,
		UserID:   in.UserID,
		Amount:   in.Amount,
		Asset:    in.Asset,
	})
	if err != nil {
		return Payout{}, false, err
	}
	if err := writeTxnEntries(ctx, tx, txn); err != nil {
		return Payout{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Payout{}, false, fmt.Errorf("提交提现申请失败: %w", err)
	}
	return row, true, nil
}

// insertPayoutRequest 写一行 pending 的提现单，返回库里那一行。
func insertPayoutRequest(ctx context.Context, tx *sql.Tx, in RequestPayoutInput) (Payout, error) {
	// 空收款信息落成 `{}`：列的默认值。写成 NULL 会撞上 NOT NULL，
	// 而那一列本来就是「还没有内容」的容器，不是「不知道」。
	destination := in.Destination
	if len(destination) == 0 {
		destination = []byte("{}")
	}
	row, err := scanPayout(tx.QueryRowContext(ctx, `
		INSERT INTO payout_requests (merchant_user_id, asset, amount, destination, status)
		VALUES ($1, $2, $3::numeric, $4::jsonb, 'pending')
		RETURNING `+payoutColumns,
		in.UserID, string(in.Asset), in.Amount.String(), string(destination)).Scan)
	if err != nil {
		return Payout{}, constraintFailure(fmt.Errorf("建提现单失败: %w", err))
	}
	return row, nil
}

// ApprovePayoutInput 是一次提现审批通过的输入。
type ApprovePayoutInput struct {
	PayoutID int64
	// Reviewer 是审批人（users.id）。审批是**记名**的动作，所以它必填：
	// 那一列有外键，而一行没有审批人的 approved 单子事后追不出是谁批的。
	Reviewer int64
	// Note 是审批说明，留空不写。
	Note string
}

// ApprovePayout 把一条待审批的提现单置为已批准。
//
// 它**不写分录**：这一步没有钱动，钱在申请时就已经挪进 payout 科目了（见
// ledger.PayoutRequest 的注释）。写下去的只有审批人与时刻。
//
// 返回的 applied=false 表示这条单子已经批准过：同样的动作重跑一次不该覆盖
// 上一次的审批人与时刻——那会把「谁批的」悄悄改成后一个人。
func (s *Store) ApprovePayout(ctx context.Context, in ApprovePayoutInput) (Payout, bool, error) {
	if in.PayoutID <= 0 {
		return Payout{}, false, fmt.Errorf("提现单 id 非法: %d", in.PayoutID)
	}
	if in.Reviewer <= 0 {
		return Payout{}, false, fmt.Errorf("审批人 id 非法: %d", in.Reviewer)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Payout{}, false, fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	row, err := payoutByID(ctx, tx, in.PayoutID)
	if err != nil {
		return Payout{}, false, err
	}
	switch row.Status {
	case PayoutApproved:
		return row, false, nil
	case PayoutPending:
	default:
		return Payout{}, false, payoutStateError(row, "批准", PayoutPending)
	}

	updated, err := reviewPayout(ctx, tx, in.PayoutID, PayoutApproved, in.Reviewer, in.Note)
	if err != nil {
		return Payout{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Payout{}, false, fmt.Errorf("提交审批结果失败: %w", err)
	}
	return updated, true, nil
}

// ReviewPayoutInput 是一次提现驳回的输入。
type ReviewPayoutInput struct {
	PayoutID int64
	// Reviewer 是驳回人（users.id），理由同 ApprovePayoutInput.Reviewer。
	Reviewer int64
	// Note 是驳回原因，留空不写。
	Note string
	// TxnID 是这次解除冻结的交易号，由调用方按提现单 id 推出来。
	// 一张单子只可能被驳回一次，所以键取单子自己的 id，而不是外部单据号：
	// 驳回没有对应的外部单据，而「同一张单驳两次」正是要幂等挡住的重复。
	TxnID ledger.TxnID
}

// RejectPayout 驳回一条待审批的提现单，并把冻结解除。
//
// 分录是 ledger.PayoutReject 造出来的，它是申请那一笔的镜像：
//
//	借 merchant_payable:<商户>   A    欠商户的分成恢复（他还能再申请）
//	贷 payout:<单id>             A    这条单子的待付核销
//
// 驳回**只处理 pending 的单子**：已经批准的单子要么往下打款，要么走撤销那条
// 路（本包还没有），而在这一步把它当驳回处理会让「批准」这个动作失去意义。
//
// 返回的 applied=false 表示这条单子已经驳回过（重放），库里那一行原样返回。
func (s *Store) RejectPayout(ctx context.Context, in ReviewPayoutInput) (Payout, bool, error) {
	if in.PayoutID <= 0 {
		return Payout{}, false, fmt.Errorf("提现单 id 非法: %d", in.PayoutID)
	}
	if in.Reviewer <= 0 {
		return Payout{}, false, fmt.Errorf("驳回人 id 非法: %d", in.Reviewer)
	}
	if in.TxnID == "" {
		return Payout{}, false, errors.New("缺少 txn_id：驳回要写一组解冻分录，幂等靠它承担")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Payout{}, false, fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if lockErr := lockTxn(ctx, tx, in.TxnID); lockErr != nil {
		return Payout{}, false, lockErr
	}
	row, err := payoutByID(ctx, tx, in.PayoutID)
	if err != nil {
		return Payout{}, false, err
	}
	// 要写的分录由**库里那一行**决定，不由调用方传：金额与商户是这条单子的
	// 属性，让调用方再给一遍就多了一处能对不上的地方。
	txn, err := ledger.PayoutReject(ledger.PayoutRejectParams{
		TxnID:    in.TxnID,
		PayoutID: row.ID,
		UserID:   row.UserID,
		Amount:   row.Amount,
		Asset:    row.Asset,
	})
	if err != nil {
		return Payout{}, false, err
	}

	stored, err := txnEntriesInTx(ctx, tx, in.TxnID)
	if err != nil {
		return Payout{}, false, err
	}
	if len(stored) > 0 {
		// 重放的判定在状态判定**之前**：重放到达时这一行已经是 rejected，
		// 而下面那道门只放行 pending——先判状态的话，重跑同一条命令拿到的
		// 会是「状态不允许」，而它其实一次都没改过任何东西。
		if conflictErr := sameStoredTxn(stored, txn); conflictErr != nil {
			return Payout{}, false, conflictErr
		}
		return row, false, nil
	}
	if row.Status != PayoutPending {
		return Payout{}, false, payoutStateError(row, "驳回", PayoutPending)
	}

	if writeErr := writeTxnEntries(ctx, tx, txn); writeErr != nil {
		return Payout{}, false, writeErr
	}
	updated, err := reviewPayout(ctx, tx, in.PayoutID, PayoutRejected, in.Reviewer, in.Note)
	if err != nil {
		return Payout{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Payout{}, false, fmt.Errorf("提交驳回失败: %w", err)
	}
	return updated, true, nil
}

// reviewPayout 写下一次审批的结果，并把改完的那一行读回来。
//
// 审批人与时刻都在这一条语句里定：reviewed_at 取库里的 now()，不由 Go 侧给
// ——同一条命令在两台时钟不同的机器上跑，落库的时刻必须是同一个来源。
func reviewPayout(ctx context.Context, tx *sql.Tx, payoutID int64,
	status PayoutStatus, reviewer int64, note string,
) (Payout, error) {
	row, err := scanPayout(tx.QueryRowContext(ctx, `
		UPDATE payout_requests
		   SET status = $2, reviewed_by = $3, reviewed_at = now(), review_note = NULLIF($4, '')
		 WHERE id = $1
		RETURNING `+payoutColumns, payoutID, string(status), reviewer, note).Scan)
	if err != nil {
		return Payout{}, constraintFailure(fmt.Errorf("写提现单 %d 的审批结果失败: %w", payoutID, err))
	}
	return row, nil
}

// PayPayoutInput 是一次打款完成的输入。
type PayPayoutInput struct {
	PayoutID int64
	// TxnID 是这次出账的交易号，由调用方按提现单 id 推出来。
	//
	// 键取单子自己的 id 而不是通道给的凭据号：一张单子只可能打款成功一次。
	// 拿凭据号当键的话，同一张单换一个凭据号（打错了、补发一笔）会被算成
	// 一笔**新交易**，payout 科目被扣成负数——那正是重复打款。
	TxnID ledger.TxnID
	// Gateway 是实际打款走的通道。它必填：通道未知时这笔分录对不上任何一份
	// 通道账单，而差的那一笔查不出来（见 ledger.PayoutPaid 的注释）。
	Gateway string
	// Ref 是通道给的打款凭据号，进分录备注供人工对账。
	Ref string
}

// PayPayout 给一条已批准的提现单打款，钱从待付出账。
//
// 分录是 ledger.PayoutPaid 造出来的：
//
//	贷 payout:<单id>             A    这条单子的待付核销
//	借 payment_gateway:<通道>    A    通道替我们垫了这笔钱
//
// **出账的唯一一道门**：这里只放行 pending → approved 之后的状态，别的状态一律
// 拒绝。所以「没经过批准就能出钱」的那条路在这一层不存在，而不是靠调用方记得
// 先走审批。
//
// 返回的 applied=false 表示这条单子已经打过款（重放），库里那一行原样返回。
func (s *Store) PayPayout(ctx context.Context, in PayPayoutInput) (Payout, bool, error) {
	if in.PayoutID <= 0 {
		return Payout{}, false, fmt.Errorf("提现单 id 非法: %d", in.PayoutID)
	}
	if in.TxnID == "" {
		return Payout{}, false, errors.New("缺少 txn_id：打款要写一组出账分录，幂等靠它承担")
	}
	if in.Gateway == "" {
		return Payout{}, false, errors.New("缺少打款通道标识：通道未知时这笔分录对不上通道账单")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Payout{}, false, fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if lockErr := lockTxn(ctx, tx, in.TxnID); lockErr != nil {
		return Payout{}, false, lockErr
	}
	row, err := payoutByID(ctx, tx, in.PayoutID)
	if err != nil {
		return Payout{}, false, err
	}
	txn, err := ledger.PayoutPaid(ledger.PayoutPaidParams{
		TxnID:    in.TxnID,
		PayoutID: row.ID,
		UserID:   row.UserID,
		Amount:   row.Amount,
		Asset:    row.Asset,
		Gateway:  in.Gateway,
		Ref:      in.Ref,
	})
	if err != nil {
		return Payout{}, false, err
	}

	stored, err := txnEntriesInTx(ctx, tx, in.TxnID)
	if err != nil {
		return Payout{}, false, err
	}
	if len(stored) > 0 {
		// 重放判定在状态判定之前，理由与 RejectPayout 那一处相同：重放到达时
		// 这一行已经是 paid，而下面那道门只放行 approved。
		if conflictErr := sameStoredTxn(stored, txn); conflictErr != nil {
			return Payout{}, false, conflictErr
		}
		return row, false, nil
	}
	if row.Status != PayoutApproved {
		return Payout{}, false, payoutStateError(row, "打款", PayoutApproved)
	}

	if writeErr := writeTxnEntries(ctx, tx, txn); writeErr != nil {
		return Payout{}, false, writeErr
	}
	updated, err := scanPayout(tx.QueryRowContext(ctx, `
		UPDATE payout_requests
		   SET status = 'paid', paid_at = now(), payout_ref = NULLIF($2, '')
		 WHERE id = $1
		RETURNING `+payoutColumns, in.PayoutID, in.Ref).Scan)
	if err != nil {
		return Payout{}, false, constraintFailure(fmt.Errorf("标记提现单 %d 已打款失败: %w", in.PayoutID, err))
	}
	if err := tx.Commit(); err != nil {
		return Payout{}, false, fmt.Errorf("提交打款失败: %w", err)
	}
	return updated, true, nil
}
