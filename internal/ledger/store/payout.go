package store

import (
	"bytes"
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
// 所以建单、审批、驳回、打款与清算这五步都是「一个事务、要么全写要么全不写」。
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
// 五个动作各自由调用方把「同一个业务事件」映射成一个 txn_id（命令行拿幂等键
// 与用途前缀算出来）。写分录的那四步（申请 / 清算 / 驳回 / 打款）因此天然幂等：
// 重放到达时那笔交易已经在库里、内容一致，函数返回 applied=false 且什么都不写。
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

// Label 给状态值一个中文说法。
//
// 库里那一列存的是英文取值（pending / approved / …），而打给人看的地方要中文。
// 本方法**只回中文**；「英文（中文）」那种并排的写法由调用方拼（回显在
// cmd/larkaxctl 的 printPayout，错误在本包的 payoutStateError）——两处拼出来
// 一样，是因为拼的时候都调了这一个方法，而不是各写一份对照表。
//
// 库里将来多出来的取值（例如某个取消状态）照原样打，不猜中文：认不出的状态
// 原样露出来比猜错强，而它至少还能与库里的值逐字对上。
//
// 它是导出方法而不是本文件里的一个私有函数：错误信息由本包写，回显由命令行写，
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

// ErrInsufficientPayable 表示商户的分成余额不够这次提现申请。
//
// 它与 ErrInsufficientBalance 分开：那个说的是用户钱包（user_wallets 那张快照）
// 被扣成负数，这个说的是 merchant_payable 科目的余额小于申请额——商户要提的钱
// 比他还没拿到的分成多。两句话看起来都是「钱不够」，但看的地方不同（一张快照
// vs 一个科目的分录之和），下一步也不同：前者去查这一笔为什么多扣了，后者去
// 查这个商户的分成为什么还没进账。
var ErrInsufficientPayable = errors.New("商户的分成余额不足")

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
	// Ref 是这张提现单的申请单号，进 payout 那条分录的备注（见
	// ledger.PayoutRequestParams.Ref）。重放判定也要拿它算一遍期望值——
	// 同一个交易号配不同的单号是冲突，不是重放。
	Ref string
	// UserID 是申请提现的商户。它必须是 users 里真实存在的一行：那一列有
	// 外键（命令行在更早一步就把标识解析成了用户）。
	UserID int64
	Amount money.Amount
	Asset  ledger.Asset
	// Destination 是收款信息，必须是一个合法的 **JSON 对象**（列是 jsonb，
	// 而命令与文档说的都是「一段 JSON 对象」）。留空表示「这一栏还没有内容」，
	// 落库成 `{}`——那是列的默认值，不是把空值当合法。
	//
	// 它**参与重放判定**：同一个申请单号配不同的收款信息是冲突，不是重放。
	// 这一栏事后没有修改命令，放过去的话库里会留下一份错的信息，而重跑
	// 同一条命令又不会再写。
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
	if len(in.Destination) > 0 {
		if destErr := validateDestination(in.Destination); destErr != nil {
			return Payout{}, false, destErr
		}
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
			Ref:      in.Ref,
		})
		if buildErr != nil {
			return Payout{}, false, buildErr
		}
		if conflictErr := sameStoredTxn(stored, expected); conflictErr != nil {
			return Payout{}, false, conflictErr
		}
		// 收款信息不在分录里，同一条交易比不出它来，得单独比一次：这一栏
		// 事后没有修改命令，放过去的话库里会留下一份错的信息。
		//
		// 比较交给库做（`=::jsonb`）而不是比字符串：jsonb 会归一键序与空白，
		// `{"a":1,"b":2}` 与 `{"b":2,"a":1}` 是同一份收款信息，按文本比会
		// 把它们判成冲突。
		var sameDestination bool
		if destErr := tx.QueryRowContext(ctx, `
			SELECT destination = $2::jsonb FROM payout_requests WHERE id = $1`,
			payoutID, string(destinationOrEmptyObject(in.Destination))).Scan(&sameDestination); destErr != nil {
			return Payout{}, false, fmt.Errorf("核对收款信息失败: %w", destErr)
		}
		if !sameDestination {
			return Payout{}, false, fmt.Errorf(
				"%w: 申请单号对应的这条提现单已经存在，但收款信息与这次要写的不一样；"+
					"收款信息事后不能改，要换收款方式只能驳回后重新申请", ErrTxnConflict)
		}
		row, readErr := payoutByID(ctx, tx, payoutID)
		if readErr != nil {
			return Payout{}, false, readErr
		}
		return row, false, nil
	}

	// 申请要把申请额从商户的分成里挪进待付，先确认那里够。
	//
	// 余额取科目分录之和（merchant_payable 没有快照行），并且读的是**当前事务**
	// 里的余额。锁按科目名字挂在事务上：同一商户的两个不同申请单号会在这里排队，
	// 不会双双读完余额再各自扣一次。
	//
	// 让这个科目变小的路径有两条：提现申请，以及退款把分成冲回去。光在申请这一侧
	// 拿锁只把**申请之间**串行化，退款那条路不排队的话，「读完余额到写入之间」仍
	// 有一个窗口——一边校验通过，另一边已经把它冲走了。所以 store.PostTransaction
	// 对分录里出现的 merchant_payable 与 prepaid 科目取同一把锁（见
	// lockReadThenWriteAccounts），两条路因此落在同一条串行线上。消费的分成
	// 与驳回都只让它变大；清算与提现申请是同一个形状（SettlePayout），两条都
	// 先拿这把锁。加锁顺序固定为「交易键 → 科目」，两边一致。对账那条路里的
	// RepairAdjustLedger 是已知例外：它不拿这把锁（见 lockAccount）。
	//
	// 这条校验保证的是「申请时不能提平台还不欠的钱」，不是「这个科目永远非负」：
	// 退款把它冲成负数是**允许**的——负的意思是平台多付给了商户、商户欠平台
	// （应收账款），账本仍然平衡，业务上再走追回。退款不为此加校验。
	payable := ledger.MerchantPayableAccount(in.UserID)
	if lockErr := lockAccount(ctx, tx, payable); lockErr != nil {
		return Payout{}, false, lockErr
	}
	available, balanceErr := accountBalanceInTx(ctx, tx, payable, in.Asset)
	if balanceErr != nil {
		return Payout{}, false, balanceErr
	}
	if available.Cmp(in.Amount) < 0 {
		return Payout{}, false, fmt.Errorf(
			"%w：商户 %d 的分成余额 %s，这次要提 %s。先确认这个商户的分成已经记进账本，"+
				"或者少提一点——不能提平台还不欠的钱",
			ErrInsufficientPayable, in.UserID, available, in.Amount)
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
		Ref:      in.Ref,
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

// SettlePayoutInput 是一次平台主动清算的输入。
type SettlePayoutInput struct {
	// TxnID 由调用方按结算单号（Ref）推出来。同一个键重放到达时，这次调用
	// 什么都不写，返回 applied=false 与库里那张单子。
	TxnID ledger.TxnID
	// Ref 是这次清算的单号。它进分录备注，也参与重放判定：同一个交易号配
	// 不同的单号是冲突，不是重放。
	Ref string
	// UserID 是收款的商户。它必须是 users 里真实存在的一行：那一列有外键
	// （命令行在更早一步就把标识解析成了用户）。
	UserID int64
	Amount money.Amount
	Asset  ledger.Asset
	// Destination 是收款信息，**必填**，且必须是一个合法的 JSON 对象。
	//
	// 与商户申请那一档的差别就在这里：申请时收款信息可以留空（商户还没填），
	// 平台主动清算则是平台已经决定要付，付到哪里必须当场说清。它同样参与
	// 重放判定——这一栏事后没有修改命令，同一张结算单配另一份收款信息是
	// 冲突，不是重放。
	Destination []byte
	// Reviewer 是做出这次清算的操作人（users.id）。单子直接落 approved，
	// 所以这里记的是 reviewed_by：清算同样是记名的动作。
	Reviewer int64
	// Note 是清算说明（结算周期、对账批次号），留空不写。
	Note string
}

// SettlePayout 一步建一条已批准的提现单，并把商户的分成转进这条单子的待付。
//
// 与 RequestPayout 的区别只有「谁发起、走不走审批」：那一档建 pending 的单子，
// 由商户提出、等运营批准；这一档是平台主动清算，单子直接落 approved
// （reviewed_by / reviewed_at 当场记下），不用再走 approve。账面形状相同，但
// 分录类型是 settle 而不是 payout_request（见 ledger.Settle），于是对账时
// 「平台清算的」与「商户申请的」在账面上分得开。
//
// 分录（借为正、贷为负，见 ledger.Settle）：
//
//	贷 merchant_payable:<商户>   A    欠商户的分成核销
//	借 payout:<单id>             A    这条单子的待付多了一笔
//
// 建单与写分录在同一个事务里：两条事实缺一条，账面就会出现一个说得通而实际
// 不对的状态（见本文件开头）。
//
// 返回的 applied=false 表示这个结算单号已经建过单（重放），库里那一行原样返回。
func (s *Store) SettlePayout(ctx context.Context, in SettlePayoutInput) (Payout, bool, error) {
	if in.TxnID == "" {
		return Payout{}, false, errors.New("缺少 txn_id：结算单号要确定性映射成它，幂等靠它承担")
	}
	if in.UserID <= 0 {
		return Payout{}, false, fmt.Errorf("商户 id 非法: %d", in.UserID)
	}
	if in.Reviewer <= 0 {
		return Payout{}, false, fmt.Errorf("操作人 id 非法: %d", in.Reviewer)
	}
	if !in.Amount.IsPositive() {
		return Payout{}, false, fmt.Errorf("清算金额必须为正数，当前为 %s", in.Amount)
	}
	if !in.Asset.Valid() {
		return Payout{}, false, fmt.Errorf("资产 %q 不受支持（目前只有 %s）", in.Asset, ledger.AssetCNY)
	}
	if destErr := validateDestination(in.Destination); destErr != nil {
		return Payout{}, false, destErr
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
		expected, buildErr := ledger.Settle(ledger.SettleParams{
			TxnID:    in.TxnID,
			UserID:   in.UserID,
			Amount:   in.Amount,
			Asset:    in.Asset,
			PayoutID: payoutID,
			Ref:      in.Ref,
		})
		if buildErr != nil {
			return Payout{}, false, buildErr
		}
		if conflictErr := sameStoredTxn(stored, expected); conflictErr != nil {
			return Payout{}, false, conflictErr
		}
		// 收款信息不在分录里，同一条交易比不出它来，得单独比一次（理由与
		// RequestPayout 那一处相同：放过去的话，重跑一条旧命令会把那张单子
		// 的收款账号悄悄换掉）。
		var sameDestination bool
		if destErr := tx.QueryRowContext(ctx, `
			SELECT destination = $2::jsonb FROM payout_requests WHERE id = $1`,
			payoutID, string(in.Destination)).Scan(&sameDestination); destErr != nil {
			return Payout{}, false, fmt.Errorf("核对收款信息失败: %w", destErr)
		}
		if !sameDestination {
			return Payout{}, false, fmt.Errorf(
				"%w: 结算单号对应的这条提现单已经存在，但收款信息与这次要写的不一样；"+
					"收款信息事后不能改", ErrTxnConflict)
		}
		row, readErr := payoutByID(ctx, tx, payoutID)
		if readErr != nil {
			return Payout{}, false, readErr
		}
		return row, false, nil
	}

	// 清算要把这笔钱从商户的分成里挪进待付，先确认那里够。锁的口径与申请
	// 那一条相同：按科目名字挂在事务上，与并发的退款、申请排在同一条线上
	// （理由见 RequestPayout 里那一段）。
	payable := ledger.MerchantPayableAccount(in.UserID)
	if lockErr := lockAccount(ctx, tx, payable); lockErr != nil {
		return Payout{}, false, lockErr
	}
	available, balanceErr := accountBalanceInTx(ctx, tx, payable, in.Asset)
	if balanceErr != nil {
		return Payout{}, false, balanceErr
	}
	if available.Cmp(in.Amount) < 0 {
		return Payout{}, false, fmt.Errorf(
			"%w：商户 %d 的分成余额 %s，这次要清算 %s。先确认这个商户的分成已经记进账本，"+
				"或者少清算一点——不能清平台还不欠的钱",
			ErrInsufficientPayable, in.UserID, available, in.Amount)
	}

	row, err := insertApprovedPayout(ctx, tx, in)
	if err != nil {
		return Payout{}, false, err
	}
	txn, err := ledger.Settle(ledger.SettleParams{
		TxnID:    in.TxnID,
		UserID:   in.UserID,
		Amount:   in.Amount,
		Asset:    in.Asset,
		PayoutID: row.ID,
		Ref:      in.Ref,
	})
	if err != nil {
		return Payout{}, false, err
	}
	if err := writeTxnEntries(ctx, tx, txn); err != nil {
		return Payout{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Payout{}, false, fmt.Errorf("提交清算失败: %w", err)
	}
	return row, true, nil
}

// validateDestination 要求收款信息是一个 JSON **对象**（顶层的 `{…}`）。
//
// 只判 json.Valid 不够：`"abc"`、`[1]`、`null` 都是合法 JSON，落进 jsonb 之后
// 看着也像「填过了」，而命令与文档说的都是「一段 JSON 对象」。打款通道是按字段
// 取值的，数组或 null 在上面一个字段都取不到——那种失败要到打款那一刻才现形，
// 而那时单子已经批过了。
func validateDestination(raw []byte) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return fmt.Errorf("收款信息必须是合法的 JSON 对象（形如 {\"bank\":\"…\"}），得到 %q", string(trimmed))
	}
	return nil
}

// destinationOrEmptyObject 把「还没填」归一成空对象。
//
// 空收款信息落成 `{}`：列的默认值。写成 NULL 会撞上 NOT NULL，而那一列本来就是
// 「还没有内容」的容器，不是「不知道」。
func destinationOrEmptyObject(raw []byte) []byte {
	if len(bytes.TrimSpace(raw)) == 0 {
		return []byte("{}")
	}
	return raw
}

// lockAccount 把同一个科目的写入串行化（事务级 advisory lock，随 commit /
// rollback 一起释放，不需要手工解锁）。
//
// 它守的是「读余额 → 与申请额比 → 写分录」这三步之间不留窗口：两个并发申请用
// 两个不同的申请单号，各自的那把交易锁互不相干，只有这把科目锁能让它们排队。
//
// **让这个科目变小的写路径都应当先拿同一把锁**，否则窗口照样存在。今天这样做
// 的有三条：提现申请（RequestPayout）、平台清算（SettlePayout）与
// PostTransaction（退款冲回分成、额度包消费，见 lockReadThenWriteAccounts）。
// 已知的例外是对账那条路里的 RepairAdjustLedger：它在 reconcile_batch.go 里
// 直接 insertEntries，不走 PostTransaction；差额科目若被配成 merchant_payable:<id>，
// 它不拿这把锁就能改动这个科目。对账是人工、低频的动作，与提现并发时最多让一次
// 申请校验读到偏大的余额——方向不安全（可能多放一次申请）。要不要拦这一格属于
// 「差额科目能不能配成分成」那条口径，不在这把锁的范围里。
func lockAccount(ctx context.Context, tx *sql.Tx, acct ledger.Account) error {
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, acct.String()); err != nil {
		return fmt.Errorf("锁定科目 %s 失败: %w", acct, err)
	}
	return nil
}

// accountBalanceInTx 读一个科目在某种资产上的余额（分录之和），在调用方的事务里。
//
// 与 AccountBalance 同一个口径（借为正、贷为负），差别只有两处：它读的是**当前
// 事务**已经写进去的那些行（提到事务外读，判的就不是同一个时刻），并且只按科目
// 原文精确匹配——会走别名归一化的只有 user 科目（见 read.go 的
// userAccountCanonicalSQL），merchant_payable 没有别的写法。
//
// 金额取 `::numeric(20,8)::text` 再交给 money.Parse：库里的 numeric 比 money 宽，
// 放不进 int64 时报错而不是给一个夹住的数（同 AccountBalance 的理由）。
func accountBalanceInTx(ctx context.Context, tx *sql.Tx, acct ledger.Account, asset ledger.Asset) (money.Amount, error) {
	var raw string
	err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(amount), 0)::numeric(20, 8)::text
		FROM ledger_entries WHERE account = $1 AND asset = $2`,
		acct.String(), string(asset)).Scan(&raw)
	if err != nil {
		return money.Zero, fmt.Errorf("读科目 %s 的余额失败: %w", acct, err)
	}
	balance, parseErr := money.Parse(raw)
	if parseErr != nil {
		return money.Zero, fmt.Errorf("科目 %s 的余额读不出来（%s）: %w", acct, raw, parseErr)
	}
	return balance, nil
}

// insertPayoutRequest 写一行 pending 的提现单，返回库里那一行。
func insertPayoutRequest(ctx context.Context, tx *sql.Tx, in RequestPayoutInput) (Payout, error) {
	row, err := scanPayout(tx.QueryRowContext(ctx, `
		INSERT INTO payout_requests (merchant_user_id, asset, amount, destination, status)
		VALUES ($1, $2, $3::numeric, $4::jsonb, 'pending')
		RETURNING `+payoutColumns,
		in.UserID, string(in.Asset), in.Amount.String(),
		string(destinationOrEmptyObject(in.Destination))).Scan)
	if err != nil {
		return Payout{}, constraintFailure(fmt.Errorf("建提现单失败: %w", err))
	}
	return row, nil
}

// insertApprovedPayout 写一行**直接是 approved** 的提现单，返回库里那一行。
//
// 审批人与时刻在 INSERT 里一次写下：平台主动清算没有「先 pending 再批准」这
// 一步，分两次写会留下一个可以被打款却没有审批人的窗口。
func insertApprovedPayout(ctx context.Context, tx *sql.Tx, in SettlePayoutInput) (Payout, error) {
	row, err := scanPayout(tx.QueryRowContext(ctx, `
		INSERT INTO payout_requests (merchant_user_id, asset, amount, destination,
		                             status, reviewed_by, reviewed_at, review_note)
		VALUES ($1, $2, $3::numeric, $4::jsonb, 'approved', $5, now(), NULLIF($6, ''))
		RETURNING `+payoutColumns,
		in.UserID, string(in.Asset), in.Amount.String(),
		string(in.Destination), in.Reviewer, in.Note).Scan)
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
