package payment

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/ledger/store"
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
	orders   *Store
	ledger   TxPoster
	channels map[string]Channel
	// now 可注入，便于测试与「通知没给支付时刻」时的取值。
	now func() time.Time
}

// NewService 装出一个 Service。channels 的键是通道标识（Channel.Name）。
func NewService(orders *Store, ledger TxPoster, channels map[string]Channel) *Service {
	return &Service{
		orders:   orders,
		ledger:   ledger,
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
	channel, err := s.prepareCreate(ctx, channelName, req)
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
	channel, err := s.prepareCreate(ctx, channelName, req)
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
// 钱包已存在。两份各写一遍的话，补上的一项校验迟早只落在其中一条路上，而干跑
// 漏掉的那一格正是「预览说能下、真跑报错」的来源。
func (s *Service) prepareCreate(ctx context.Context, channelName string, req CreateOrderRequest) (Channel, error) {
	channel, err := s.channel(channelName)
	if err != nil {
		return nil, err
	}
	if validErr := validateCreateOrder(req); validErr != nil {
		return nil, validErr
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

// orderForCreate 把一次下单翻译成 payment_orders 那一行。
//
// raw 是通道建单的原始返回：真跑给通道的那一份，干跑给 nil（没有问通道）。
func orderForCreate(req CreateOrderRequest, channel Channel, raw []byte) Order {
	return Order{
		OrderNo:    req.OrderNo,
		UserID:     req.UserID,
		Gateway:    channel.Name(),
		Asset:      req.Asset,
		Amount:     req.Amount,
		Status:     StatusCreated,
		GatewayRaw: raw,
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
	// paid 那一种也要过一遍账本：同键同内容会返回 applied=false（不改余额），
	// 同键不同内容（例如手续费变了）会报冲突，而那个判据只在账本那一层有。
	txn, err := ledger.Topup(ledger.TopupParams{
		TxnID:      TxnIDForOrder(order.OrderNo),
		UserID:     order.UserID,
		Amount:     order.Amount,
		Asset:      order.Asset,
		Gateway:    order.Gateway,
		GatewayFee: n.Fee,
		OrderNo:    order.OrderNo,
		At:         n.PaidAt,
	})
	if err != nil {
		return NotificationResult{}, err
	}
	applied, err := s.ledger.PostTransactionTx(ctx, locked.Tx(), txn)
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

// channel 取一条已装配的通道。
func (s *Service) channel(name string) (Channel, error) {
	channel, ok := s.channels[name]
	if !ok || channel == nil {
		return nil, fmt.Errorf("%w: %s", ErrChannelDisabled, name)
	}
	return channel, nil
}
