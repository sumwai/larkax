package payment

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/ledger/store"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/plans"
)

// TxPoster 是账本对「在给定事务里写一组分录」的能力要求。
//
// 由 `internal/ledger/store.Store` 满足（它的方法名与签名与这里逐字一致），
// 抽成接口只是为了让 Service 的判据可以脱离数据库测试。幂等、加锁顺序与
// 钱包快照都在那一边，本包不重复实现。
//
// **不接「自己开事务」的那一个入口**：通知处理已经握着一条连接，再开一个
// 事务会同时占两条，并发回调数超过连接池容量时整池停摆（见 Notify 的说明）。
type TxPoster interface {
	PostTransactionTx(ctx context.Context, tx *sql.Tx, txn ledger.Transaction) (applied bool, err error)
}

// Service 是「下单 → 通知 → 入账」这条链的编排。
type Service struct {
	orders *Store
	ledger TxPoster
	// plans 是买额度包那条路要用的商品与发放入口。为 nil 时带 plan_key 的
	// 下单与入账都被拒：这个服务没有接上额度包。
	plans    *plans.Service
	channels map[string]Channel
	// now 可注入，便于测试与「通知没给支付时刻」时的取值。
	now func() time.Time
}

// NewService 装出一个 Service。channels 的键是通道标识（Channel.Name）。
//
// planSvc 是额度包那一侧的入口（下单时读商品、通知到达时在同一事务里发放）。
// 不卖额度包的装配可以直接给 nil，带 plan_key 的操作会明确拒绝。
func NewService(orders *Store, ledger TxPoster, planSvc *plans.Service, channels map[string]Channel) *Service {
	return &Service{
		orders:   orders,
		ledger:   ledger,
		plans:    planSvc,
		channels: channels,
		now:      time.Now,
	}
}

// CreateResult 是一次下单的结果。
type CreateResult struct {
	// Order 是库里那一行（重放时是此前写下的那一行）。
	Order Order
	// Created 是通道建单的返回。
	Created CreatedOrder
	// Applied 为 false 表示这个单号已经下过、这次没有新写一行。
	Applied bool
}

// Create 在通道侧建单，并把单号落进 payment_orders。
//
// 先问通道、再落库：单号是我们给通道的，通道按它做幂等；落库失败时那张单子
// 在通道侧仍然存在，重跑同一个单号会回到这里并补上本地这一行。
func (s *Service) Create(ctx context.Context, channelName string, req CreateOrderRequest) (CreateResult, error) {
	channel, err := s.prepareCreate(ctx, channelName, &req)
	if err != nil {
		return CreateResult{}, err
	}

	created, err := channel.CreateOrder(ctx, req)
	if err != nil {
		return CreateResult{}, err
	}
	if created.OrderNo == "" {
		created.OrderNo = req.OrderNo
	}
	if created.OrderNo != req.OrderNo {
		// 通道改了单号时拒绝：付款人看到的单号与库里那一行对不上，之后的
		// 通知就再也认不出这张单子。
		return CreateResult{}, fmt.Errorf(
			"通道 %s 返回的单号 %q 与下单时的 %q 不一致", channel.Name(), created.OrderNo, req.OrderNo)
	}

	order, applied, err := s.orders.Create(ctx, orderForCreate(req, channel, created.Raw))
	if err != nil {
		return CreateResult{}, err
	}
	return CreateResult{Order: order, Created: created, Applied: applied}, nil
}

// CreatePreview 是 create 的干跑：走与 Create 相同的前置校验与本地落库，
// 但**不向通道下单、不提交**。
//
// 不下单是刻意的：通道侧建单是一个真实的外发动作，干跑不能留下它；代价是预览
// 里没有通道回的支付地址（Created 的零值），调用方要把它打出来说明这一点。
// 本地那一行仍然在事务里插一遍再回滚——字段（单号、用户、通道、资产、金额、
// 状态）与真跑同源，重放/冲突的判定也是同一条 SQL，所以预览不会出现
// 「预览说能下、真跑报冲突」。
func (s *Service) CreatePreview(ctx context.Context, channelName string, req CreateOrderRequest) (CreateResult, error) {
	channel, err := s.prepareCreate(ctx, channelName, &req)
	if err != nil {
		return CreateResult{}, err
	}

	tx, err := s.orders.BeginTx(ctx)
	if err != nil {
		return CreateResult{}, fmt.Errorf("开启预览事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	order, applied, err := s.orders.CreateTx(ctx, tx, orderForCreate(req, channel, nil))
	if err != nil {
		return CreateResult{}, err
	}
	return CreateResult{Order: order, Applied: applied}, nil
}

// prepareCreate 是 Create 与 CreatePreview 共用的前置检查：通道存在、请求合法、
// 钱包已存在（只对充值）。两份各写一遍的话，补上的一项校验迟早只落在其中一条
// 路上，而干跑漏掉的那一格正是「预览说能下、真跑报错」的来源。
func (s *Service) prepareCreate(ctx context.Context, channelName string, req *CreateOrderRequest) (Channel, error) {
	channel, err := s.channel(channelName)
	if err != nil {
		return nil, err
	}
	if req.PlanKey != "" {
		if planErr := s.applyPlan(ctx, req); planErr != nil {
			return nil, planErr
		}
	}
	if validErr := validateCreateOrder(*req); validErr != nil {
		return nil, validErr
	}
	if req.PlanKey != "" {
		// 买额度包不动用户钱包：面值进 prepaid:<订阅 id>，账本不写钱包快照，
		// 所以这条路上不要求用户先开户。
		return channel, nil
	}
	// 钱包必须先存在：账本写入时缺这一行会失败，而那时通知已经到过一次，
	// 通道看到 500 还会重发。在下单这一步先拦住，报的也是同一档（缺一行）。
	hasWallet, walletErr := s.orders.WalletExists(ctx, req.UserID, req.Asset)
	if walletErr != nil {
		return nil, walletErr
	}
	if !hasWallet {
		return nil, fmt.Errorf(
			"%w: 用户 %d 还没有 %s 钱包——先 larkaxctl wallet create；回调入账要求钱包已经存在",
			store.ErrNoWallet, req.UserID, req.Asset)
	}
	return channel, nil
}

// applyPlan 把带 plan_key 的下单请求补成一份可以落库的单子：金额与币种由包
// 决定，调用方给的值只用来核对。
//
// 每条拒绝都点名**是哪一条前提不成立**：包不存在、停用、商户包、没有售卖
// 有效期、金额不符、币种不符。只说「不能买这个包」会让运维逐个去猜。
func (s *Service) applyPlan(ctx context.Context, req *CreateOrderRequest) error {
	key := strings.TrimSpace(req.PlanKey)
	if key == "" || key != req.PlanKey {
		return fmt.Errorf("额度包 key %q 首尾有空白", req.PlanKey)
	}
	if s.plans == nil {
		return errors.New("本服务没有接入额度包：带 plan_key 的支付单不能下单")
	}
	plan, err := s.plans.PlanByKey(ctx, key)
	if err != nil {
		return err
	}
	switch {
	case !plan.Enabled:
		return fmt.Errorf("额度包 %q 已停用，不能售卖；先 larkaxctl plan enable --key %s", plan.Key, plan.Key)
	case plan.OwnerUserID != nil:
		// 商户包的售卖还要回答「钱怎么分给那个商户」，而分成口径未定。
		// 先只卖平台包：把一笔归属不清的交易放进账本，事后拆账比现在拒绝更贵。
		return fmt.Errorf("额度包 %q 归属商户 %d，目前只售平台包：商户包的分成口径未定",
			plan.Key, *plan.OwnerUserID)
	case plan.ValidityDays == nil:
		return fmt.Errorf("额度包 %q 没有设置售卖有效期（validity_days），不能售卖", plan.Key)
	}
	// 金额与币种可以不给，由包决定；给了就要对得上。
	//
	// 「给没给」看 AmountGiven，不看 Amount 是不是零：money.Amount 没有「未指定」
	// 这个值，把 0 当未指定会让「调用方明确给了 0」与「调用方没给」变成同一件事，
	// 而前者是参数错——它会被下面那一步静默覆盖成面值，调用方以为自己下的是别的
	// 金额。
	if req.AmountGiven && !req.Amount.IsPositive() {
		return fmt.Errorf("金额必须为正数，得到 %s；买额度包时不要给 0 或负数，金额由包的面值决定", req.Amount)
	}
	if req.AmountGiven && req.Amount.Cmp(plan.FaceValue) != 0 {
		return fmt.Errorf("额度包 %q 的面值是 %s，下单给的金额是 %s；金额由商品决定",
			plan.Key, plan.FaceValue, req.Amount)
	}
	// 币种同理：给了只用来核对，没给就用包的。今天只有 CNY 一种资产
	// （ledger.Asset.Valid 只认它），所以「给了别的币种」在下单前就会被
	// validateCreateOrder 拒掉；这一条守的是「给了另一种受支持的币种」——
	// 将来加第二种资产时它才真正开始工作。
	if req.AssetGiven && req.Asset != plan.Asset {
		return fmt.Errorf("额度包 %q 的币种是 %s，下单给的是 %s；币种由商品决定",
			plan.Key, plan.Asset, req.Asset)
	}
	req.Amount = plan.FaceValue
	req.Asset = plan.Asset
	req.PlanKey = plan.Key
	// 天数在下单这一刻定死：它进 payment_orders，入账时按它算到期时刻，
	// 不再回头读包。包后来被改成 365 天，这一单也不会多给 11 个月。
	days := *plan.ValidityDays
	req.ValidityDays = &days
	return nil
}

// orderForCreate 把一次下单翻译成 payment_orders 那一行。
//
// raw 是通道建单的原始返回：真跑给通道的那一份，干跑给 nil（没有问通道）。
func orderForCreate(req CreateOrderRequest, channel Channel, raw []byte) Order {
	return Order{
		OrderNo:      req.OrderNo,
		UserID:       req.UserID,
		Gateway:      channel.Name(),
		Asset:        req.Asset,
		Amount:       req.Amount,
		Status:       StatusCreated,
		PlanKey:      req.PlanKey,
		ValidityDays: req.ValidityDays,
		GatewayRaw:   raw,
	}
}

// NotificationResult 是一次通知的处理结果。
type NotificationResult struct {
	// Order 是处理之后库里那一行。
	Order Order
	// Applied 为 false 表示这次没有新写分录（重放，或单据已在终态）。
	Applied bool
	// Ignored 为 true 表示通知被有意识地放过了：单据已经在终态，状态机不往回走。
	Ignored bool
}

// Notify 处理一次通道通知：校验 → 按单号串行 → 读单 → 写分录 → 改状态 → 提交。
//
// **整段在同一条连接、同一个事务里完成**。分录与状态因此同生共死：任何一步
// 失败都整笔回滚，不会留下「钱进了账、单子停在 created」或反过来「单子变成
// paid、账上什么都没有」的中间态。重复通知仍走账本的幂等判据（同键同内容
// 返回 applied=false），状态机也只往前走。
//
// 早先的写法在持有支付单事务时另开一个事务写账本，一条通知占两条连接；并发
// 通知数超过连接池容量时，所有连接都握着第一条去等第二条，整池停摆。
//
// 两张不同的单争同一个通道流水号时，撞的是 payment_orders 上的部分唯一索引
// （00064）。错误返回会让整个事务回滚，所以两张单的状态与流水号都不变；那
// 一层约束是必须的——单号上的锁只把同一张单的通知串起来，挡不住两张单并发。
func (s *Service) Notify(ctx context.Context, channelName string, n Notification) (NotificationResult, error) {
	channel, err := s.channel(channelName)
	if err != nil {
		return NotificationResult{}, err
	}
	if validErr := validateNotification(n); validErr != nil {
		return NotificationResult{}, validErr
	}
	if n.PaidAt.IsZero() {
		n.PaidAt = s.now()
	}

	// 同一张单子的通知按 order_no 串行：读状态与改状态之间若没有这道锁，
	// 一份「失败」与一份「已支付」并发到达时，后者可能读到 created、写完
	// 分录，再去改状态时才发现单子已经被前一份转成 failed——钱进了账，
	// 单子却永远不是 paid。
	var result NotificationResult
	err = s.orders.WithOrderLock(ctx, n.OrderNo, func(ctx context.Context, locked *LockedOrder) error {
		var notifyErr error
		result, notifyErr = s.notifyLocked(ctx, channel, n, locked)
		return notifyErr
	})
	if err != nil {
		return NotificationResult{}, s.orders.annotateGatewayTxnConflict(ctx, err)
	}
	return result, nil
}

// notifyLocked 是持锁之后的处理主体。
func (s *Service) notifyLocked(ctx context.Context, channel Channel, n Notification, locked *LockedOrder) (NotificationResult, error) {
	order, err := locked.Get(ctx, n.OrderNo)
	if err != nil {
		return NotificationResult{}, err
	}
	if order.Gateway != channel.Name() {
		return NotificationResult{}, fmt.Errorf(
			"%w: 支付单 %s 属于通道 %s，不是发来通知的 %s",
			ErrOrderConflict, order.OrderNo, order.Gateway, channel.Name())
	}
	if order.Asset != n.Asset || order.Amount.Cmp(n.Amount) != 0 {
		return NotificationResult{}, fmt.Errorf(
			"%w: 支付单 %s 记的是 %s %s，通知说的是 %s %s",
			ErrOrderConflict, order.OrderNo, order.Amount, order.Asset, n.Amount, n.Asset)
	}

	if n.Status != StatusPaid {
		// 失败与过期：只改状态，不碰账本。已经在终态的单子什么都不改。
		if order.Status.Terminal() {
			return NotificationResult{Order: order, Ignored: true}, nil
		}
		changed, markErr := locked.MarkTerminal(ctx, order.OrderNo, n.Status, n.Raw)
		if markErr != nil {
			return NotificationResult{}, markErr
		}
		updated, updatedErr := locked.Get(ctx, order.OrderNo)
		if updatedErr != nil {
			return NotificationResult{}, updatedErr
		}
		return NotificationResult{Order: updated, Ignored: !changed}, nil
	}

	if order.Status.Terminal() && order.Status != StatusPaid {
		// failed / expired / refunded 之后收到「已支付」：状态机不往回走，
		// 账也不动。这里不能「顺手补一笔」——一张已经失败的单子再入账，等于
		// 让通道侧先后两份互相矛盾的通知都成立。
		return NotificationResult{Order: order, Ignored: true}, nil
	}

	// 走到这里只有两种可能：created 收到第一份支付通知，或 paid 收到重放。
	//
	// 两条路的「重放内容」判据不在同一层：
	//   - 充值：每次都要真的写一遍分录，所以由账本判——同键同内容返回
	//     applied=false，同键不同内容报冲突。
	//   - 买额度包：GrantTx 按订阅单号早返回，压根不写分录，账本层没有机会
	//     比较内容。所以它在 Go 侧显式核对（见 purchaseReplayConflict）。
	//
	// 重放时的支付时刻取单子上已经记下的那一个，不取这份通知自带的：通道重发
	// 可能换一个时间戳，而买额度包的到期时刻是从它往后数的，两边必须一致，
	// 否则一次正常的重放会被判成「到期时刻不一样」。
	paidAt := n.PaidAt
	if order.PaidAt != nil {
		paidAt = *order.PaidAt
	}
	applied, err := s.postPaid(ctx, order, paidAt, n.Fee, locked.Tx())
	if err != nil {
		// 账本给出的「同键不同内容」与「单号已经是一条别的单子」是同一种处置：
		// 先去看库里那一笔是什么，而不是改一下通知重发。两句话都留着，前一句
		// 是本包的分类，后一句点名了是哪一处分录对不上。
		// 分录与状态在同一个事务里，这里返回错误会让两者一起回滚。
		if errors.Is(err, store.ErrTxnConflict) {
			return NotificationResult{}, fmt.Errorf("%w: %w", ErrOrderConflict, err)
		}
		return NotificationResult{}, err
	}

	if order.Status == StatusCreated {
		changed, markErr := locked.MarkPaid(ctx, order.OrderNo, order.Gateway, n.GatewayTxnID, n.Raw, n.PaidAt, n.Fee)
		if markErr != nil {
			return NotificationResult{}, markErr
		}
		if !changed {
			// 读单与改状态在同一个事务里，中间没有别的写者能改这一行；走到这里
			// 说明有代码绕过了单号上的锁。返回错误会连同刚写的分录一起回滚，
			// 不会留下「账上有钱、单子不是 paid」的组合。
			return NotificationResult{}, fmt.Errorf(
				"%w: 支付单 %s 读的时候还是 created，改状态时却已经不在 created 上了",
				ErrOrderConflict, order.OrderNo)
		}
	}
	updated, err := locked.Get(ctx, order.OrderNo)
	if err != nil {
		return NotificationResult{}, err
	}
	return NotificationResult{Order: updated, Applied: applied}, nil
}

// postPaid 是「已支付」到达之后写账的那一半：充值走 Topup，买额度包走发放与
// PlanPurchase。两条路都在调用方的事务里，与单子转 paid 同生共死。
func (s *Service) postPaid(ctx context.Context, order Order, paidAt time.Time, fee money.Amount, tx *sql.Tx) (bool, error) {
	if order.PlanKey != "" {
		// 售卖的重放在 Go 侧核对：那一路上重放根本不写账本，所以充值那条靠的
		// 「同键不同内容 → 账本报冲突」在这里覆盖不到（见 purchasePlan）。
		if order.Status == StatusPaid {
			if conflictErr := purchaseReplayConflict(order, fee); conflictErr != nil {
				return false, conflictErr
			}
		}
		return s.purchasePlan(ctx, order, paidAt, fee, tx)
	}
	txn, err := ledger.Topup(ledger.TopupParams{
		TxnID:      TxnIDForOrder(order.OrderNo),
		UserID:     order.UserID,
		Amount:     order.Amount,
		Asset:      order.Asset,
		Gateway:    order.Gateway,
		GatewayFee: fee,
		OrderNo:    order.OrderNo,
		At:         paidAt,
	})
	if err != nil {
		return false, err
	}
	return s.ledger.PostTransactionTx(ctx, tx, txn)
}

// purchaseReplayConflict 核对「已 paid 的单子」收到的这份通知与当初那笔是不是
// 同一件事。
//
// 为什么在 Go 侧：售卖的重放在 GrantTx 里按 `user_plans.ref` 早返回 applied=false，
// **一条分录都不写**，账本那一层因此没有任何机会比较内容。充值那条能靠
// 「同键不同内容 → ErrTxnConflict」兜住，是因为它每次都真的去写一遍分录。
//
// 只在单子已经是 paid 时跑：首次通知到达时单子还是 created，`fee` 还是 0
// （它由 MarkPaid 在同一个事务里写进去），拿它来比会把第一次通知判成冲突。
//
// 手续费变了要拦，是因为它决定账上那两笔手续费的金额：同一个订单号记两次不同的
// 手续费，只有第一次那组会落账，第二次既不改账也不报错——那笔差额就此消失。
func purchaseReplayConflict(order Order, fee money.Amount) error {
	if order.Fee.Cmp(fee) != 0 {
		return fmt.Errorf(
			"%w: 支付单 %s 已经按手续费 %s 入过账，这份通知说的是 %s；同一个订单号不能对应两笔不同的手续费",
			ErrOrderConflict, order.OrderNo, order.Fee, fee)
	}
	return nil
}

// purchasePlan 在同一个事务里发放订阅并写售卖分录。
//
// **不写 plan_grant**：那是赠送口径（贷 platform_revenue），会把一次售卖记成
// 平台让出的收入。面值那一笔的对方科目是支付通道，见 ledger.PlanPurchase。
//
// 重放由 GrantTx 判：同一个订单号已经发过订阅时它返回 applied=false，一条新
// 分录都不写。
func (s *Service) purchasePlan(ctx context.Context, order Order, paidAt time.Time, fee money.Amount, tx *sql.Tx) (bool, error) {
	if s.plans == nil {
		return false, errors.New("本服务没有接入额度包：这张单买的是额度包，通知无法入账")
	}
	if order.ValidityDays == nil {
		// 这一格的**不变量**由下一层兜着：GrantTx 拿到 nil 的 ValidityDays 会拒绝，
		// 不会解引用、也不会回落到「读现在的包」（见 plans.GrantTx）。这里再挡一次
		// 是为了**说法**——GrantTx 那句点名的是包（「额度包的售卖没有给出天数」），
		// 而运维需要看到的是单号。
		//
		// 这一格正常写路径造不出来：带 plan_key 的下单一定会把天数一并记下
		// （见 applyPlan 与 orderForCreate），而 00070 之前落库的行连 plan_key
		// 都没有，碰不到这里。能走到这儿的只有手工修数、从列不全的备份恢复，
		// 或者以后的写入漏了天数——所以话得这么说：**先说这种行不该存在**，
		// 再让人去核它是怎么来的。说成「写于 00070 之前」会把人引去查迁移历史，
		// 而那里没有答案。
		//
		// 两层都留着不是冗余：删掉这一层不会让缺口复活（RED 里验证过，报的是
		// GrantTx 那句），只是报错会指错方向。
		return false, fmt.Errorf(
			"支付单 %s 买的是额度包，却没有记下售卖有效期；带 plan_key 的单子不会长成这样（下单时一并写入），先核一下是不是手工改过或从备份恢复的",
			order.OrderNo)
	}
	_, applied, err := s.plans.GrantTx(ctx, tx, plans.GrantInput{
		TxnID:   PlanTxnIDForOrder(order.OrderNo),
		Ref:     order.OrderNo,
		UserID:  order.UserID,
		PlanKey: order.PlanKey,
		// ExpiresAt 留空：到期时刻由 PurchaseInput.ValidityDays（单子下单时记下的
		// 天数）与 PaidAt 算出来，见 plans.GrantTx。这里不自己算，免得两处各算
		// 一次；也**不读包上的现值**——用户按多少天付的钱由这张单回答。
		Purchase: &plans.PurchaseInput{
			Gateway:    order.Gateway,
			GatewayFee: fee,
			PaidAt:     paidAt,
			// 单子带过来的三个期望值：面值、币种、天数。入账时包会被重读
			// 一遍，三者任一不符就拒绝这次通知；启停与归属不由单子带，由
			// plans.purchasePlanMismatch 直接读包核对。天数还决定到期时刻
			// ——按单子算，不按包里现在的值。
			ExpectedAmount: order.Amount,
			ExpectedAsset:  order.Asset,
			ValidityDays:   order.ValidityDays,
		},
	})
	if err != nil {
		// 带上单号：这一类错误落到 handler 的 default 分支回 500，而那里的日志只有
		// request_id。运维拿到的指令是「核对这张单、补发或退款」，没有单号就落不了地。
		// 中间那句报错点名的是包与哪一格对不上——两者拼起来才知道去核对哪张单的哪一处。
		//
		// %w 不能丢：notifyLocked 靠 errors.Is(store.ErrTxnConflict) 把它转成 409。
		return false, fmt.Errorf("支付单 %s: %w", order.OrderNo, err)
	}
	return applied, nil
}

// channel 取一条已装配的通道。
func (s *Service) channel(name string) (Channel, error) {
	channel, ok := s.channels[name]
	if !ok || channel == nil {
		return nil, fmt.Errorf("%w: %s", ErrChannelDisabled, name)
	}
	return channel, nil
}
