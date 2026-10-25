package plans

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/dberr"
	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/ledger/store"
	"github.com/sumwai/larkax/internal/money"
)

// 本文件是订阅的**生命周期**：发放与作废。
//
// 两个动作都是「一行订阅 + 一组分录」写在一个事务里：
//
//   - 只有订阅行、没有分录：库里说这个人有额度，账上却没有任何东西兜着它，
//     消费时 prepaid:<订阅 id> 从零被扣成负数。
//   - 只有分录、没有订阅行：账上多出一笔不知道属于谁、什么单位、什么时候到期的
//     预存额度，它只会以「prepaid 长期不平」的形式在某个对账日出现。
//
// 所以两者必须同生共死。分录的构造在 internal/ledger，写分录（含幂等与交易锁）
// 在 store.PostTransactionTx，这里只负责订阅行与「该发多少、该退多少」。

// GrantInput 是一次额度包发放的输入。
type GrantInput struct {
	// TxnID 是这次发放的交易号，由调用方按发放单号（Ref）确定性地推出来。
	// 同一个单号重放到达时，这次调用什么都不写，返回 applied=false。
	TxnID ledger.TxnID
	// Ref 是外部发放单号（运营工单号、活动单号）。它是这次发放的幂等键，
	// 也是库上 user_plans.ref 的唯一值。
	Ref string
	// UserID 是收到额度的用户。
	UserID int64
	// PlanKey 是发放哪个额度包（plans.key）。
	PlanKey string
	// ExpiresAt 是这份额度的有效期终点，必须晚于现在。
	//
	// 它写进 user_plans.period_end；到期之后这份额度要作废（见 Void），
	// 而「什么时候到期」是发放时定下的，不由包决定——同一个包可以按不同的
	// 有效期发出去。售卖那一路不给它，由 Purchase.PaidAt 与单子上记下的
	// PurchaseInput.ValidityDays 算出来（见下）——不读包上的现值。
	ExpiresAt time.Time
	// Purchase 非 nil 表示这次发放是一次**售卖**：面值那一笔的对方科目是支付
	// 通道，而不是平台收入（见 ledger.PlanPurchase）。为 nil 时是运营赠送。
	Purchase *PurchaseInput
}

// PurchaseInput 是售卖发放比赠送多出来的那几项。
type PurchaseInput struct {
	// Gateway 是收款的支付通道标识，进 payment_gateway 科目。
	Gateway string
	// GatewayFee 是通道手续费，由平台承担，与充值那一档同形。
	GatewayFee money.Amount
	// PaidAt 是通道确认收款的时刻；有效期从它往后数 validity_days。
	//
	// 它由调用方从**支付单上已记下的时刻**取，而不是每份通知自带的那一个：
	// 重放的通知可能换一个时间戳，而到期时刻必须每次都算出同一个值，否则
	// 一次正常的重放会被判成冲突。
	PaidAt time.Time

	// ExpectedAmount 与 ExpectedAsset 是这张单**下单时**记下的面值与币种。
	//
	// 入账时会把包重新读一遍（它可能在下单之后被改过），发放的额度只能按
	// 用户实际付的那笔来。两者不符就拒绝这次通知，而不是按新值发、也不是
	// 按旧值发：前者让用户按旧价拿到新包，后者让一个已经下架的包按旧价
	// 长期生效。单子停在 created，人工核对之后再处理。
	ExpectedAmount money.Amount
	ExpectedAsset  ledger.Asset

	// ValidityDays 是这张单**下单时**包上的售卖天数，到期时刻由它与 PaidAt 算出。
	//
	// 它不读包上的现值：包在下单之后被改成 365 天时，按现在的包算会让用户按
	// 30 天付的钱拿到 12 倍时长。天数跟着单子走，于是同一份通知重放多少次都
	// 算出同一个到期时刻，重放不会被判成「到期时刻不一样」。
	//
	// 指针：0 天不合法，「没有这个值」与「0 天」不能混为一谈。售卖这条路上
	// 它必须有值，缺失由调用方在更早一层拒绝（见 payment.purchasePlan）。
	ValidityDays *int
}

// Grant 把一个额度包发给一个用户：建一条订阅，并把面值记进 prepaid:<订阅 id>。
//
// 分录（见 ledger.PlanGrant）：
//
//	借 prepaid:<订阅 id>      面值    平台欠用户的预存额度增加
//	贷 platform_revenue       面值    这份额度是平台让出来的收入
//
// 返回的 applied=false 表示这个发放单号已经发过（重放），库里那一行原样返回。
//
// # 重放怎么判
//
// 先按 Ref 找订阅行（唯一索引保证最多一行），再比对用户、包与到期时刻：
// 同一单号配不同的用户/包/到期不是重放，而是冲突（ErrTxnConflict）——那说明
// 调用方把两个不同的事件映射到了同一个单号上。**不比对分录**：分录挂在订阅 id
// 上，而订阅行存在就说明那次发放已经落过账；拿调用方这次给的 TxnID 去比，
// 重放时若换了交易号就会误判成冲突。
func (s *Service) Grant(ctx context.Context, in GrantInput) (UserPlan, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return UserPlan{}, false, fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	up, applied, err := s.GrantTx(ctx, tx, in)
	if err != nil {
		return UserPlan{}, false, err
	}
	if commitErr := tx.Commit(); commitErr != nil {
		return UserPlan{}, false, fmt.Errorf("提交额度包发放失败: %w", commitErr)
	}
	return up, applied, nil
}

// GrantTx 在调用方给的事务里发放一个额度包，**不提交、不回滚**。
//
// 除事务边界外，语义与 Grant 逐条相同：同一批校验、同一份建订阅的 SQL、
// 同一份重放判据、同一笔分录。干跑（`plan grant --dry-run`）用它把整条写路径
// 走一遍再回滚，于是预览里的订阅 id、面值与「会不会被判成重放」都出自同一条路，
// 而不是另写一份读侧模拟——两份迟早分叉，而分叉的表现是「预览说行、执行报错」。
func (s *Service) GrantTx(ctx context.Context, tx *sql.Tx, in GrantInput) (UserPlan, bool, error) {
	if in.TxnID == "" {
		return UserPlan{}, false, errors.New("缺少 txn_id：发放单号要确定性映射成它，幂等靠它承担")
	}
	ref := strings.TrimSpace(in.Ref)
	if ref == "" {
		return UserPlan{}, false, errors.New("缺少发放单号：它是这次发放的幂等键")
	}
	if ref != in.Ref {
		return UserPlan{}, false, fmt.Errorf("发放单号 %q 首尾有空白", in.Ref)
	}
	if in.UserID <= 0 {
		return UserPlan{}, false, fmt.Errorf("用户 id 非法: %d", in.UserID)
	}
	key := strings.TrimSpace(in.PlanKey)
	if key == "" {
		return UserPlan{}, false, errors.New("缺少额度包 key")
	}
	if in.ExpiresAt.IsZero() && in.Purchase == nil {
		return UserPlan{}, false, errors.New("缺少有效期终点：额度必须有一个到期时刻，否则作废无从触发")
	}

	// 包在判定重放之前读出来：售卖的有效期要从它的 validity_days 算，而重放
	// 那一步要拿这个算出来的时刻去比库里那一行。读失败只可能是包不存在——
	// 禁用的包照样读得出来，所以「停用之后重放」不是在这里被挡住的；它由下面
	// 售卖那条商品核对拦下（见 purchasePlanMismatch）。
	plan, err := planByKeyIn(ctx, tx, key)
	if err != nil {
		return UserPlan{}, false, err
	}

	expiresAt := in.ExpiresAt
	if in.Purchase != nil {
		if in.Purchase.ValidityDays == nil {
			return UserPlan{}, false, fmt.Errorf(
				"额度包 %q 的售卖没有给出天数（PurchaseInput.ValidityDays）：到期时刻只能由"+
					"单子上记下的天数与收款时刻算出", plan.Key)
		}
		if in.Purchase.PaidAt.IsZero() {
			return UserPlan{}, false, errors.New("缺少收款时刻：售卖的有效期要从它往后数")
		}
		// **按单子上记的天数算，不按包里现在的**：用户按多少天付的钱由单子回答。
		// 包后来被改过时，purchasePlanMismatch 会在首次发放前拦下；已经发放过的
		// 重放走到这里，算出的仍是当初那个时刻，grantReplayConflict 因此不会误判。
		expiresAt = in.Purchase.PaidAt.AddDate(0, 0, *in.Purchase.ValidityDays)
	}

	existing, found, err := userPlanByRefIn(ctx, tx, ref)
	if err != nil {
		return UserPlan{}, false, err
	}
	if found {
		if conflictErr := grantReplayConflict(existing, in, key, expiresAt); conflictErr != nil {
			return UserPlan{}, false, conflictErr
		}
		return existing, false, nil
	}

	// 售卖的单子与包可能已经不是同一件事了：下单时把面值与币种写进了单子，
	// 而这里读到的是**现在**的包。中间运营改了面值、币种、售卖有效期、启停或
	// 归属时，按新值发是让用户按旧价拿到新包，按旧值发是让一个已经下架的包按
	// 旧价长期生效——两种都不对，所以停在发放之前，让单子留在 created 等人工核对。
	//
	// **只对首次发放生效**：守卫防的是「下单与首次发放之间包被改了」。走到
	// 这里说明按 ref 查不到订阅（found=false），也就是这笔交易还没成立。一旦
	// 订阅已经存在，上面那条早返回已经带走了——那笔交易已成立并落库，包后来
	// 改成什么样都改变不了它；此时再拒只会换来通道无限重试和一堆 500。
	if in.Purchase != nil {
		if mismatchErr := purchasePlanMismatch(plan, in.Purchase); mismatchErr != nil {
			return UserPlan{}, false, mismatchErr
		}
	}

	// 「有效期必须在将来」只对新发放成立，所以放在「确认不是重放」之后：重放的
	// 语义是「这件事已经发生过」，拿今天的时间去校验一件过去的事，本来就不该拦。
	// 那份额度可能已经过期，但它仍然是同一次发放（上面已经按单号与关键字段认出来
	// 了），不能因此报错——重放要能返回库里那一行，调用方才能把「已经发过」与
	// 「这次没发」分开。
	if !expiresAt.After(time.Now()) {
		return UserPlan{}, false, fmt.Errorf("有效期终点 %s 不晚于现在，发出去就已经到期了",
			expiresAt.Format(time.RFC3339))
	}

	if !plan.Enabled {
		return UserPlan{}, false, fmt.Errorf("额度包 %q 已禁用，不能再发放；先把它启用", plan.Key)
	}
	if scopeErr := validatePlanScope(plan); scopeErr != nil {
		return UserPlan{}, false, scopeErr
	}

	var userPlanID int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO user_plans (user_id, plan_id, status, period_start, period_end, ref)
		VALUES ($1, $2, 'active', now(), $3, $4)
		RETURNING id`,
		in.UserID, plan.ID, expiresAt, ref).Scan(&userPlanID)
	if err != nil {
		return UserPlan{}, false, dberr.ConstraintFailure(fmt.Errorf("建额度包订阅失败: %w", err))
	}

	// 赠送与售卖只差面值那一笔的对方科目：赠送是平台让出的收入，售卖是钱从
	// 通道进来。两档各自成笔，账面上才分得清「这份额度是送的还是卖的」。
	var txn ledger.Transaction
	if in.Purchase != nil {
		txn, err = ledger.PlanPurchase(ledger.PlanPurchaseParams{
			TxnID:          in.TxnID,
			SubscriptionID: userPlanID,
			Amount:         plan.FaceValue,
			Asset:          plan.Asset,
			Gateway:        in.Purchase.Gateway,
			GatewayFee:     in.Purchase.GatewayFee,
			OrderNo:        ref,
		})
	} else {
		txn, err = ledger.PlanGrant(ledger.PlanGrantParams{
			TxnID:          in.TxnID,
			SubscriptionID: userPlanID,
			Amount:         plan.FaceValue,
			Asset:          plan.Asset,
			Ref:            ref,
		})
	}
	if err != nil {
		return UserPlan{}, false, err
	}
	applied, err := s.ledger.PostTransactionTx(ctx, tx, txn)
	if err != nil {
		return UserPlan{}, false, err
	}
	if !applied {
		// 走到这里说明这个交易号已经有一笔**完全相同**的发放分录，而我们刚
		// 建了一条新订阅——两件事对不上，不能把新订阅当成已经记过账。
		return UserPlan{}, false, fmt.Errorf(
			"交易号 %s 已经存在一笔内容相同的发放分录，这次却新建了订阅 %d；"+
				"同一个交易号不能对应两条订阅", in.TxnID, userPlanID)
	}

	// 新订阅建好之后、读回整行之前，把该用户手上待并入的结转额度并进来：
	// 同一事务、同一条订阅，于是「发放」与「并入」要么都落、要么都不落。
	// 并入的闸门是各源 carryover 科目的账本余额（见 mergeCarryovers）。
	//
	// 重放走的是上面那条早返回，压根到不了这里——同一个发放单号不会二次并入。
	if _, _, mergeErr := mergeCarryovers(ctx, s, tx, userPlanID, in.UserID, plan.Asset, plan.Unit, ref); mergeErr != nil {
		return UserPlan{}, false, mergeErr
	}

	row, err := userPlanByIDIn(ctx, tx, userPlanID, false)
	if err != nil {
		return UserPlan{}, false, err
	}
	return row, true, nil
}

// purchasePlanMismatch 核对「这张单买的包」与「这个包现在是什么」是不是同一件事。
//
// 五条各自独立报，每条都带上两边的值：运维拿到这句话就知道该核对哪张单、哪一格。
// 面值排第一是因为它最可能发生（调价），而它的后果最直接——多给或少给钱。
//
// 第五条（售卖有效期）比另外四格更要紧：前四格变了，用户付的钱与拿到的东西
// 至少还在同一个量级（面值对不上是一笔钱的事、币种与归属是记给谁的事、停用是
// 发不发的事）；天数变了是直接多给或少给时长——从 30 改成 365 是用户按 30 天
// 付钱拿到 12 倍时长，从 365 改成 7 是用户按年付拿到一周，而它不会报错、没有
// 日志，静默生效。它比得出来是因为**单子上记了下单时的天数**
// （payment_orders.validity_days → PurchaseInput.ValidityDays）。
//
// 币种那条只在包**还认得出来**的时候才走得到：scanPlan 在读到不支持的币种时
// 就返回错误（额度包 ... 的资产 "X" 不受支持），拒绝同样成立，只是报错来自读侧。
//
// 五条都**不**包 `store.ErrTxnConflict`：它们不是「两张单冲突」，而是「这张单
// 的前提变了」。包成 409 会让通道以为已经处理完、停止重试，而这类单子要人工
// 核对后补发或退款——它必须落到 500（见 payment 包 handler 的 default 分支）。
func purchasePlanMismatch(plan Plan, in *PurchaseInput) error {
	if plan.FaceValue.Cmp(in.ExpectedAmount) != 0 {
		return fmt.Errorf(
			"额度包 %q 的面值在售出之后改过：这张单下单时的面值是 %s，包现在是 %s；先核对这张单",
			plan.Key, in.ExpectedAmount, plan.FaceValue)
	}
	if plan.Asset != in.ExpectedAsset {
		return fmt.Errorf(
			"额度包 %q 的币种在售出之后改过：这张单下单时是 %s，包现在是 %s；先核对这张单",
			plan.Key, in.ExpectedAsset, plan.Asset)
	}
	if !plan.Enabled {
		return fmt.Errorf("额度包 %q 在售出之后被停用，这张单不再发放；先核对这张单再决定是否手工补发", plan.Key)
	}
	if plan.OwnerUserID != nil {
		// 商户包的售卖要回答「钱怎么分给那个商户」，而下单时它还是平台包。
		// 归属在售出之后被改到商户名下时同样停在这里：发放本身没有问题，
		// 有问题的是这笔钱该记给谁，而那要人工决定。
		return fmt.Errorf(
			"额度包 %q 在售出之后被改成归属商户 %d，这张单不再自动发放；先核对这张单",
			plan.Key, *plan.OwnerUserID)
	}
	// 这一格比的是单子上记下的天数与包上现在的天数
	// （payment_orders.validity_days → PurchaseInput.ValidityDays）：单子上有，
	// 才比得出来。到期时刻也按单子上那个数算，不按包里现在的值（见 GrantTx）。
	// 为什么它比另外四格更要紧，见本函数的文档注释。
	if plan.ValidityDays == nil {
		return fmt.Errorf(
			"额度包 %q 在售出之后被改成不出售（售卖有效期被清空）：这张单下单时是 %d 天；先核对这张单",
			plan.Key, *in.ValidityDays)
	}
	if *plan.ValidityDays != *in.ValidityDays {
		return fmt.Errorf(
			"额度包 %q 的售卖有效期在售出之后改过：这张单下单时是 %d 天，包现在是 %d 天；先核对这张单",
			plan.Key, *in.ValidityDays, *plan.ValidityDays)
	}
	return nil
}

// grantReplayConflict 核对重放到达的发放与库里那一行是不是同一件事。
//
// 比的是这次发放的**关键字段**：用户、额度包、到期时刻。金额不在其中——它是
// 包的属性（face_value），包相同金额就相同；到期时刻按微秒比，因为
// timestamptz 的精度是微秒，纳秒部分落库时会被截掉，直接比 time.Time 会把
// 一次正常的重放判成冲突。
//
// expiresAt 由调用方算好再传进来：售卖那一路不给 ExpiresAt，它的到期时刻是
// 收款时刻加**单子上记下的**天数（PurchaseInput.ValidityDays），这里拿到的是
// 算完之后的值。按单子算而不是按包里现在的值算，重放才会算出同一个时刻——
// 否则包被改过之后一次正常的重放会被判成冲突。
func grantReplayConflict(existing UserPlan, in GrantInput, planKey string, expiresAt time.Time) error {
	if existing.UserID != in.UserID {
		return fmt.Errorf("%w: 发放单号 %q 已经发给用户 %d，这次要发给用户 %d",
			store.ErrTxnConflict, in.Ref, existing.UserID, in.UserID)
	}
	if existing.PlanKey != planKey {
		return fmt.Errorf("%w: 发放单号 %q 对应额度包 %q，这次是 %q",
			store.ErrTxnConflict, in.Ref, existing.PlanKey, planKey)
	}
	if existing.ExpiresAt == nil || existing.ExpiresAt.UnixMicro() != expiresAt.UnixMicro() {
		return fmt.Errorf("%w: 发放单号 %q 对应的到期时刻与这次要写的不一样",
			store.ErrTxnConflict, in.Ref)
	}
	return nil
}

// validatePlanScope 拦截「token / 次数包却没有模型范围」的包。
//
// 写入时已经校验过一次（CreatePlan），这里再查一遍是因为库里的行可能来自更早的
// 版本、一次手工修数或另一个写入路径——而发出去一个没有模型范围的 token 包，
// 等于给出一个可能亏本的额度，事后再收回来要把额度作废，用户已经用掉的那部分
// 追不回来。
func validatePlanScope(p Plan) error {
	if p.Unit != UnitAmount && len(p.Models) == 0 {
		return fmt.Errorf("%w：额度包 %q 是 %s 包，却没有模型范围（至少要对一个模型开放）",
			ErrPlanScope, p.Key, p.Unit)
	}
	return nil
}

// VoidInput 是一次额度包作废的输入。
type VoidInput struct {
	// TxnID 是这次作废的交易号，由调用方按作废单号（Ref）确定性地推出来。
	TxnID ledger.TxnID
	// UserPlanID 是要作废的那条订阅。
	UserPlanID int64
	// Ref 是作废单号，库上 user_plans.void_ref 的唯一值。
	Ref string
	// Status 是作废之后的终态：UserPlanExpired（到期）或 UserPlanCancelled
	//（运营提前作废）。
	//
	// 两者的账面处置**不一样**：真的到期（expired 且库里的时间已经过了
	// period_end）时，剩余面值按 settings.plans.carryover_bp 留下一部分到
	// carryover:<订阅 id>，其余归平台；canceled、以及还没到期就手工标成
	// expired 的，整笔归平台。
	Status UserPlanStatus
}

// Void 作废一条订阅：把剩余面值处置掉（到期时按比例结转，其余归平台），并改状态。
//
// 分录：
//
//	真的到期且比例大于零：
//	  贷 prepaid:<订阅 id>   结转量    这部分面值转成待并入的额度
//	  借 carryover:<订阅 id> 结转量
//	贷 prepaid:<订阅 id>     归平台量  其余不再可用
//	借 platform_revenue      归平台量
//
// 两笔分录的金额之和严格等于处置前的剩余面值（见 carrySplit）。
//
// 剩余面值从**账本**现算（prepaid 科目的余额），不由调用方给：那是权威值，
// 也是「这个订阅还剩多少」的唯一答案。余额为零时只改状态、不写分录——
// 零金额交易会被 Verify 拒掉，而且账上本来就没有东西要转。
//
// 返回的 applied=false 表示这个作废单号已经用过（重放），库里那一行原样返回。
func (s *Service) Void(ctx context.Context, in VoidInput) (UserPlan, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return UserPlan{}, false, fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	up, applied, err := s.VoidTx(ctx, tx, in)
	if err != nil {
		return UserPlan{}, false, err
	}
	if commitErr := tx.Commit(); commitErr != nil {
		return UserPlan{}, false, fmt.Errorf("提交额度包作废失败: %w", commitErr)
	}
	return up, applied, nil
}

// VoidTx 在调用方给的事务里作废一条订阅，**不提交、不回滚**。
//
// 除事务边界外，语义与 Void 逐条相同（含行锁、重放判定与「剩余面值从账本现算」）。
// 干跑用它走完整条写路径再回滚：预览里那句「剩余面值已转回平台收入」背后的金额
// 就是这次真的会写进分录的那个数。
func (s *Service) VoidTx(ctx context.Context, tx *sql.Tx, in VoidInput) (UserPlan, bool, error) {
	if in.TxnID == "" {
		return UserPlan{}, false, errors.New("缺少 txn_id：作废单号要确定性映射成它，幂等靠它承担")
	}
	if in.UserPlanID <= 0 {
		return UserPlan{}, false, fmt.Errorf("订阅 id 非法: %d", in.UserPlanID)
	}
	ref := strings.TrimSpace(in.Ref)
	if ref == "" {
		return UserPlan{}, false, errors.New("缺少作废单号：它是这次作废的幂等键")
	}
	if ref != in.Ref {
		return UserPlan{}, false, fmt.Errorf("作废单号 %q 首尾有空白", in.Ref)
	}
	switch in.Status {
	case UserPlanExpired, UserPlanCancelled:
	default:
		return UserPlan{}, false, fmt.Errorf("作废的终态只能是 %s 或 %s，得到 %q",
			UserPlanExpired, UserPlanCancelled, in.Status)
	}

	// 行锁：两个不同作废单号的并发调用不能都读到 active，否则同一份额度会被
	// 作废两次。
	row, err := userPlanByIDIn(ctx, tx, in.UserPlanID, true)
	if err != nil {
		return UserPlan{}, false, err
	}
	if row.VoidRef == ref {
		// 重放。判定的位置在状态门之前：重放到达时这一行已经是终态，
		// 先判状态的话，重跑同一条命令拿到的会是「状态不允许」，而它其实
		// 一次都没改过任何东西。
		if row.Status != in.Status {
			return UserPlan{}, false, fmt.Errorf(
				"%w: 作废单号 %q 已经把这条订阅按 %s 作废，这次要按 %s",
				store.ErrTxnConflict, ref, row.Status, in.Status)
		}
		return row, false, nil
	}
	if row.Status != UserPlanActive {
		return UserPlan{}, false, fmt.Errorf("%w: 订阅 %d 现在是 %s，只有 %s 的订阅能作废",
			ErrUserPlanState, row.ID, row.Status, UserPlanActive)
	}

	// 同一个作废单号已经用在别的订阅上：直接拒，而不是等 UPDATE 撞唯一索引。
	var otherID int64
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM user_plans WHERE void_ref = $1`, ref).Scan(&otherID)
	switch {
	case err == nil:
		return UserPlan{}, false, fmt.Errorf("%w: 作废单号 %q 已经用于订阅 %d，不能再用在订阅 %d 上",
			store.ErrTxnConflict, ref, otherID, row.ID)
	case !errors.Is(err, sql.ErrNoRows):
		return UserPlan{}, false, fmt.Errorf("核对作废单号失败: %w", err)
	}

	// 这次作废适不适用期末结转：只有真的到期（expired 且库里的时间已经过了
	// period_end）才结转；canceled 与「还没到期就手工标 expired」整笔归平台。
	carryable, err := periodEndedIn(ctx, tx, row, in.Status)
	if err != nil {
		return UserPlan{}, false, err
	}
	var bp int64
	if carryable {
		// 比例在动任何一笔之前读。读不出来就整次作废失败——事务回滚后订阅仍是
		// active、面值一分没动、也没有任何分录（见 carryoverBasisPoints）。
		bp, err = carryoverBasisPoints(ctx, tx)
		if err != nil {
			return UserPlan{}, false, err
		}
	}

	prepaid := ledger.PrepaidAccount(row.ID)
	// 加锁顺序固定为全局字典序：carryover:<id> 在 prepaid:<id> 之前（"c" < "p"）。
	// 这与 store 写分录前那一组锁（lockReadThenWriteAccounts 按科目名排序）是同一个
	// 次序，于是任何同时碰这两个科目的事务都从 carryover 开始，不会互相等成环。
	// 结转量为零时也照拿：次序由路径决定，不由这一次算出来的金额决定。
	if carryable {
		carryover := ledger.CarryoverAccount(row.ID)
		if lockErr := lockPlanAccount(ctx, tx, carryover); lockErr != nil {
			return UserPlan{}, false, lockErr
		}
	}
	// 作废要把余额读出来再写分录，读的那一刻就得在锁里，所以这里显式拿一次。
	// 这把锁与账本层的是同一个（store 写分录之前会给分录里的 merchant_payable、
	// prepaid 与 carryover 科目上同一把事务级锁，见 lockReadThenWriteAccounts）：
	// 任何能让 prepaid 变小的写入都先拿它，否则余额会在「读出来」与「写进去」之间
	// 被扣走。
	if lockErr := lockPlanAccount(ctx, tx, prepaid); lockErr != nil {
		return UserPlan{}, false, lockErr
	}
	remaining, err := accountBalanceInTx(ctx, tx, prepaid, row.Asset)
	if err != nil {
		return UserPlan{}, false, err
	}
	if remaining.IsNegative() {
		return UserPlan{}, false, fmt.Errorf(
			"订阅 %d 的面值余额为 %s，已经为负——账本与订阅已经不一致，先查清是谁多扣了",
			row.ID, remaining)
	}

	// 在途预留虽然还没写成分录，那部分面值已经承诺给那几笔请求了。核销掉
	// 它们，等它们结算时 prepaid 会被扣成负数——额度包透支，而账本上分不出
	// 是哪一步错了。所以作废只核销**减掉在途之后**的剩余（见 #918）。
	pending, err := store.PendingPlanReservation(ctx, tx, row.ID)
	if err != nil {
		return UserPlan{}, false, err
	}
	remaining = remaining.Sub(pending)
	if remaining.IsNegative() {
		return UserPlan{}, false, fmt.Errorf(
			"订阅 %d 的在途预留（%s）超过面值余额——先让那几笔请求收尾，或者查清是谁多预留了",
			row.ID, pending)
	}

	// 两维各自算、各自向下取整（见 carrySplit）：面值这一维 R 会被切成
	// 「结转」与「归平台」两段，两者之和严格等于 R——向下取整的零头留给平台，
	// 不会凭空多出额度。量那一维的结转量写在 carryout_quota 上，等并入时用。
	var carryAmount, carryQuota money.Amount
	if carryable {
		basisQuota, quotaErr := store.PlanQuotaUsage(ctx, tx, row.ID)
		if quotaErr != nil {
			return UserPlan{}, false, quotaErr
		}
		carryAmount, carryQuota = carrySplit(row.Unit, remaining, basisQuota,
			row.CarriedAmount, row.CarriedQuota, bp)
	}
	voidAmount := remaining.Sub(carryAmount)

	if carryAmount.IsPositive() {
		carryTxn, carryErr := ledger.PlanCarry(ledger.PlanCarryParams{
			TxnID:          carryTxnID(carryTxnDomain, ref),
			SubscriptionID: row.ID,
			Amount:         carryAmount,
			Asset:          row.Asset,
			Reason:         voidReason(in.Status),
			Ref:            ref,
		})
		if carryErr != nil {
			return UserPlan{}, false, carryErr
		}
		applied, postErr := s.ledger.PostTransactionTx(ctx, tx, carryTxn)
		if postErr != nil {
			return UserPlan{}, false, postErr
		}
		if !applied {
			return UserPlan{}, false, fmt.Errorf(
				"交易号 %s 已经存在一笔内容相同的结转分录，而订阅 %d 还是 %s；"+
					"同一个作废单号不能对应两次结转", carryTxn.TxnID, row.ID, row.Status)
		}
	}

	if voidAmount.IsPositive() {
		voidTxn, voidErr := ledger.PlanVoid(ledger.PlanVoidParams{
			TxnID:          in.TxnID,
			SubscriptionID: row.ID,
			Amount:         voidAmount,
			Asset:          row.Asset,
			Reason:         voidReason(in.Status),
			Ref:            ref,
		})
		if voidErr != nil {
			return UserPlan{}, false, voidErr
		}
		applied, postErr := s.ledger.PostTransactionTx(ctx, tx, voidTxn)
		if postErr != nil {
			return UserPlan{}, false, postErr
		}
		if !applied {
			return UserPlan{}, false, fmt.Errorf(
				"交易号 %s 已经存在一笔内容相同的作废分录，而订阅 %d 还是 %s；"+
					"同一个交易号不能对应两次作废", in.TxnID, row.ID, row.Status)
		}
	}

	// 先改状态再读回整行：状态、作废单号、作废时刻与结转出去的两维要一次写上，
	// 而读回的那一行带 JOIN（单位、可用量、面值长在包上），UPDATE ... RETURNING
	// 里取不到那些列。
	var updatedID int64
	err = tx.QueryRowContext(ctx, `
		UPDATE user_plans SET status = $2, void_ref = $3, voided_at = now(),
		                     carryout_quota = $4::numeric, carryout_amount = $5::numeric
		 WHERE id = $1
		RETURNING id`, row.ID, string(in.Status), ref,
		carryQuota.String(), carryAmount.String()).Scan(&updatedID)
	if err != nil {
		return UserPlan{}, false, dberr.ConstraintFailure(fmt.Errorf("标记订阅 %d 已作废失败: %w", row.ID, err))
	}
	updated, err := userPlanByIDIn(ctx, tx, updatedID, false)
	if err != nil {
		return UserPlan{}, false, err
	}
	return updated, true, nil
}

// voidReason 把终态翻成备注里的说法。
//
// 账本那一档（plan_void）覆盖到期与提前作废两种处置，备注要能分出是哪一种：
// 对账看到一笔作废时，「到期了」与「运营收回」是两个不同的问题。
func voidReason(status UserPlanStatus) string {
	switch status {
	case UserPlanExpired:
		return "到期"
	case UserPlanCancelled:
		return "人工作废"
	}
	return ""
}

// accountBalanceInTx 读一个科目在某种资产上的余额（分录之和），在调用方的事务里。
//
// 与 store 里那一份是同一条 SQL 与同一个口径（借为正、贷为负）：那边是未导出的
// （它只在自己的事务里用），而这里要的正是**当前事务**里已经写进去的那些行
// ——在最外层读会读到提交前的旧值，而那正是作废金额算错的来源。
//
// 只按科目原文精确匹配：prepaid 的后缀只放行一种写法（见 ledger.ParseAccount），
// 不需要 user 科目那样的别名归一。
//
// 金额取 `::numeric(20, 8)::text` 再交给 money.Parse：库里的 numeric 比 money 宽，
// 放不进 int64 时报错，而不是给一个夹住的数。
func accountBalanceInTx(ctx context.Context, tx *sql.Tx, acct ledger.Account, asset ledger.Asset) (money.Amount, error) {
	var raw string
	err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(amount), 0)::numeric(20, 8)::text
		FROM ledger_entries WHERE account = $1 AND asset = $2`,
		acct.String(), string(asset)).Scan(&raw)
	if err != nil {
		return money.Zero, fmt.Errorf("读科目 %s 的余额失败: %w", acct, err)
	}
	balance, err := money.Parse(raw)
	if err != nil {
		return money.Zero, fmt.Errorf("科目 %s 的余额读不出来（库里的原文是 %q）: %w", acct, raw, err)
	}
	return balance, nil
}
