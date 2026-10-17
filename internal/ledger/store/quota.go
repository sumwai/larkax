package store

import (
	"context"

	"github.com/sumwai/larkax/internal/keyquota"
	"github.com/sumwai/larkax/internal/money"
)

// 本文件是额度判定在**入口那一层**的落点，也就是 api.QuotaChecker 的实现。
//
// 权威判定不在这里：它在 SaveReservation 那个事务里（见 store.go 里对
// keyquota.CheckInTx 的调用）。两者的口径同一份，都在 internal/keyquota；
// 这里只是把「一次只读查询」接到入口要的那份接口上。
//
// 之所以要分成两层，是为了让明显超限的请求在读请求体之前就被挡掉——那一步
// 最多读进 maxRequestBodyBytes（见 internal/api），而且它后面还跟着一次预留
// 事务。分成两层之后**判据仍然只有一份**，这一点由 keyquota 那个包保证。

// CheckQuota 实现 internal/api 的 QuotaChecker：报告这把 key 还收不收得下
// 这一笔。
//
// limit 是 api_keys.quota_limit 的**原样**值，nil 表示列值为 NULL，也就是
// 「这把 key 没设上限」。这一支在这里直接返回，一次查询都不发——入口每个
// 请求都会调到这里，而绝大多数 key 没有额度上限（见迁移 00001 的列注释：
// NULL 表示继承用户默认，而 users 表上没有这样一个默认值，所以它等价于不限）。
//
// 不加锁、不开事务：这一层是早拒绝，不是判据。并发的两个请求可能都过得了
// 它，那没关系——真正挡得住的是预留事务里的那一次（见 keyquota.CheckInTx）。
func (s *Store) CheckQuota(ctx context.Context, keyID int64, limit *money.Amount) error {
	if limit == nil {
		return nil
	}
	return keyquota.Check(ctx, s.db, keyID, *limit)
}
