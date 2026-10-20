package store

import (
	"context"
	"fmt"
	"time"

	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/money"
)

// OpenWallet 给一个用户开通钱包，返回这次是否真的建了一行。
//
// # 为什么只开 0 余额的户
//
// 开户与充值是两件事。带一笔初始金额意味着这次调用同时是一次充值，
// 而那要回答「钱从哪个支付通道进来、通道费记在谁头上」——属于充值入口的
// 职责，不是开户能替它决定的。
//
// 新开的户在账本上**没有任何分录**：这个用户在 user 科目上的分录之和为 0，
// 快照的 balance=0 与它自洽。反过来不成立——充过 100 又花掉 100 的用户余额
// 同样是 0，却有分录——所以这是「开户 → 没有分录」这一个方向的说法，不是
// 一条双向不变式。这里也不写一条金额为零的分录来「留个开户痕迹」：账本拒绝
// 零金额分录（见 ledger.Entry.verify），而一条零金额分录也会让「有分录就说明
// 发生过资金流动」这条对账判据失效。
//
// # 幂等
//
// 钱包行已经存在时什么都不做，返回 created=false。开户命令会被写进
// 初始化脚本与重跑的准备流程，把「已存在」报成失败会让第二次运行中断。
// 调用方拿得到 created，于是「刚建出来」与「本来就有」在输出上分得开
// ——不区分两者的成功退出，读起来像一次静默的空操作。
//
// 并发由主键 (user_id, asset) 兜底：两个并发的开户请求只有一行落库，
// 另一个拿到 created=false。这正是 ON CONFLICT DO NOTHING 的含义，而不是
// 「先查再插」——那中间有一个窗口，两个请求会各自看到「不存在」。
//
// # 用户不存在
//
// 不做「先查用户再插」：user_wallets.user_id 有外键指向 users，插入一个
// 不存在的 id 会被数据库挡下，而先查一次只会多出一个竞态窗口（查完到插入
// 之间用户可能被删）。调用方（命令行）本来就会先把「ID 或用户名」解析成
// 用户，走到这里的报错是给绕过那一步的调用方与并发删除留下的。
//
// 挡下之后返回的是 **ErrNoSuchUser**（见 errors.go），不是驱动的原文：调用方
// 按哨兵分类给退出码，而「这个用户不在库里」与解析路径上的 auth.ErrUserNotFound
// 是同一个事实，两条路径要给同一个码。判据只认 user_wallets 上那一条外键——
// 别的表的外键冲突不是「用户不存在」。
func (s *Store) OpenWallet(ctx context.Context, userID int64, asset ledger.Asset) (created bool, err error) {
	if userID <= 0 {
		return false, fmt.Errorf("用户 id 非法: %d", userID)
	}
	if !asset.Valid() {
		return false, fmt.Errorf("资产 %q 不受支持（目前只有 %s）", asset, ledger.AssetCNY)
	}

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO user_wallets (user_id, asset, balance, frozen)
		VALUES ($1, $2, 0, 0)
		ON CONFLICT (user_id, asset) DO NOTHING`,
		userID, string(asset))
	if err != nil {
		// 具名的先判：这一条外键的含义是确定的，调用方可以直接问出
		// 「是用户不存在」。兜底的通用哨兵也挂在链上，于是「是不是约束
		// 冲突」只有一个判据。
		if isMissingUserViolation(err) {
			return false, constraintFailure(missingUserError(err, userID))
		}
		return false, constraintFailure(fmt.Errorf("开通钱包失败: %w", err))
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("读取影响行数失败: %w", err)
	}
	return affected > 0, nil
}

// Wallet 是一个用户钱包的**读侧**快照。
//
// 它读的是 user_wallets 那两列本身（快照值），不是从 ledger_entries 现算的
// 权威值：两者在正常情况下相等，对账任务负责校验（见 reconcile.go）。要
// 「账本上是多少」用 AccountBalance，要「库里那一行现在写着多少」用这里。
type Wallet struct {
	// UserID 是 user_wallets.user_id。
	UserID int64
	// Asset 是资产品种，钱包主键的另一半。
	Asset ledger.Asset
	// Balance 是可用余额快照；BalanceErr 非 nil 时它是零值，**不能当余额用**。
	Balance money.Amount
	// Frozen 是为在飞的请求冻结住的那部分，它不包含在 Balance 里；
	// FrozenErr 非 nil 时同样是零值。
	Frozen money.Amount

	// BalanceErr 非 nil 表示 balance 那一列读不出来，里面是 money.Parse 给的
	// **原因**。是哪一行、哪一列、库里的原文是什么，这三样要由调用方按 UserID /
	// Asset / BalanceRaw 说出来——这个错误里一个都没有，它只说「这个数怎么会读
	// 不出来」。见 Wallets 的「一行读不出来不拖垮整份清单」。
	BalanceErr error
	// FrozenErr 同 BalanceErr，对应 frozen 那一列。
	FrozenErr error
	// BalanceRaw / FrozenRaw 是两列在库里的十进制原文，**总是填**：读得出来的
	// 时候它与上面那个金额是同一个数的另一种写法，读不出来的时候它是仅剩的
	// 证据——报告要说清「读不出来的是什么」，只能引它。
	BalanceRaw string
	FrozenRaw  string
	// UpdatedAt 是这一行最后一次被写入的时刻。控制台把它展示成「余额更新于」，
	// 它是快照的新鲜度，不是账本的事实时间——账本的时间要看分录。
	UpdatedAt time.Time
}

// Wallets 列钱包快照，按 user_id、asset 排序。
//
// userID 为 nil 时列全部用户的钱包。与 ListKeys 同一条约定：标识先由命令行
// 解析成具体 id 再查。
//
// # 一行读不出来不拖垮整份清单
//
// 金额列是 numeric(20, 8)，比 money.Amount 的 int64 宽：库里可以存在一个这一列
// 装得下、而定点数装不下的余额（见 #442 与 #446）。这样的行**不**让整条查询失败，
// 其余行照常返回，那一行在对应的 Err 字段上带着原因、在 Raw 字段上带着原文
// ——**读的人必须处理它**：Err 非 nil 时同一个 Wallet 上那个金额字段是零值，
// 照着用就会把「读不出来」显示成「0」，而一个偏小的余额看起来正是「少扣了钱」
// （见 #777：一行的坏数据不该让别人的钱包一个也看不到；夹住那个数交出去更坏）。
//
// 只有**扫不动**那一行（连接断了、驱动层面出错）才是整条失败：那不是某一格的值
// 有问题，是这一份清单没法保证完整。
//
// # 这里不 JOIN users
//
// 用户名属于 users 表，是 auth 那一侧的事：本包至今一处都不读 users
// （对账按分片拿到的 id 走）。为了一栏展示在这里加一条跨表的 JOIN，等于把
// 「谁是用户」这件事复制一份到账本包的 SQL 里，而它将来还要跟着
// users 的变化改。调用方要名字就自己按 user_id 去 auth 那边取。
func (s *Store) Wallets(ctx context.Context, userID *int64) ([]Wallet, error) {
	const query = `
		SELECT user_id, asset, balance::text, frozen::text, updated_at
		  FROM user_wallets
		 WHERE $1::bigint IS NULL OR user_id = $1
		 ORDER BY user_id, asset`

	rows, err := s.db.QueryContext(ctx, query, userID)
	if err != nil {
		return nil, fmt.Errorf("查询钱包清单失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Wallet
	for rows.Next() {
		var (
			w              Wallet
			balance, froze string
		)
		if scanErr := rows.Scan(&w.UserID, &w.Asset, &balance, &froze, &w.UpdatedAt); scanErr != nil {
			return nil, fmt.Errorf("读钱包行失败: %w", scanErr)
		}
		// 金额列是 numeric，没有能直接扫进定点数类型的驱动类型，所以按
		// 十进制文本读回来再解析（与 reconcile_batch.go 的 readWallets 同一写法）。
		// 两列的原文都留着：解析不了时它是报告里唯一的证据。
		//
		// 两列各解析各的，失败只记在对应的 Err 上——**这一行照样进 out**。
		// 跳过整行会让一个只在 balance 上出问题的钱包连它读得出来的冻结额
		// 一起消失，而这一行的两个字段本来就互相独立。
		w.BalanceRaw, w.FrozenRaw = balance, froze
		w.Balance, w.BalanceErr = money.Parse(balance)
		w.Frozen, w.FrozenErr = money.Parse(froze)
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历钱包行失败: %w", err)
	}
	return out, nil
}
