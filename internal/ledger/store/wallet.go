package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sumwai/larkax/internal/ledger"
)

// OpenWallet 给一个用户开通钱包，返回这次是否真的建了一行。
//
// # 为什么只开 0 余额的户
//
// 开户与充值是两件事。带一笔初始金额意味着这次调用同时是一次充值，
// 而那要回答「钱从哪个支付通道进来、通道费记在谁头上」——属于充值入口的
// 职责，不是开户能替它决定的。
//
// 0 余额的户在账本上的形状恰好是**没有任何分录**：这个用户在 user 科目上
// 的和为 0，快照的 balance=0 与它自洽。这里不写一条金额为零的分录来「留个
// 开户痕迹」——账本拒绝零金额分录（见 ledger.Entry.verify），而一条零金额
// 分录也会让「有分录就说明发生过资金流动」这条对账判据失效。
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
		if isMissingUserViolation(err) {
			return false, fmt.Errorf("用户 %d 不存在（users 里没有这一行）: %w", userID, err)
		}
		return false, fmt.Errorf("开通钱包失败: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("读取影响行数失败: %w", err)
	}
	return affected > 0, nil
}

// pgForeignKeyViolation 是外键约束冲突的错误码。
//
// 与 errors.go 里的 pgCheckViolation 同一个来源（驱动给的 SQLSTATE）。这条
// 插入上只有一个外键（user_id → users），所以认错误码就够，不必再看约束名。
const pgForeignKeyViolation = "23503"

// isMissingUserViolation 判断这次插入是不是被「用户不存在」挡下的。
func isMissingUserViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgForeignKeyViolation
}
