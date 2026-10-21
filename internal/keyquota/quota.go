// Package keyquota 判定一把 API Key 的累计花费有没有超出它的额度上限。
//
// # 口径（只有这一份）
//
// 「累计花费」= 这把 key 在 billing_requests 上**已结算**的那些行的
// final_amount 之和，加上**还挂着 pending** 的那些行的预留额之和。全部历史
// 累计，**不按周期重置**。
//
// pending 行的预留额是 `reserved_amount + plan_reserved_amount`：前者是从钱包
// 冻住的钱，后者是额度包覆盖的请求占着的面值。额度包覆盖时钱包一分没冻
// （见 internal/ledger/store 的 plancharge.go），只读前者会把这一类请求在
// 结算之前当成「没花钱」——额度上限被静默放行，而这不是额度包该带来的效果：
// 包里的钱也是这个 key 花掉的。
//
// 四条都要单独说清楚，因为每一条都能被改错，而改错之后没有东西会报出来：
//
//   - **预留中的钱算进去，而且用预留额而不是最终额**。正在跑的那一笔已经
//     冻住了钱，不计入的话，一个客户端并发打满时每一笔都「还没结算」，于是
//     每一笔都放行，上限被整个穿过去（这与幂等键要防的重复扣费是同一类
//     问题的两面）。用预留额是因为最终额那时还不存在，而预留额本身是
//     上界（见 internal/pipeline 的 Reserve）——偏保守正是额度该有的方向。
//   - **只认 settled 与 pending**。released / failed / refunded 的那些行没有
//     从用户身上扣走钱，算进去等于把一次失败的请求也记成消费。
//   - **不按周期重置**。账本里没有「周期」这个概念，按月重置要先回答
//     「周期从哪算、时区是什么、跨月的在途请求算哪一期」，而每一个都得单独
//     定口径。要改成按周期重置，先回答那三个问题，再动这里的 SQL。
//
// # 一条已知的后果：陈旧的 pending 会永久吃掉这把 key 的额度
//
// 把 pending 算进去是必须的（见上面第一条），但那些行**只被报出来、不自动
// 释放**（见 internal/ledger/store 的 reconcile：stale_reservation 是一条
// finding，不是一次修复——自动释放与在途请求有竞态）。
//
// 于是：一次硬崩（预留提交之后、结算或释放之前进程被杀）留下的那行 pending，
// 会让那笔钱**永久**占着这把 key 的额度。它一分都没花，但从这个判定看与
// 「已经花了」没有区别，而这是**有意**的——判据宁可偏保守，因为放宽的方向是
// 「上限被穿过去」。
//
// 处置出口在账本那一侧（人工确认那笔预留确实死了之后释放它），**不在本包**。
// 这句话写在这里，是为了让读到「累计花费」的人不会以为库里没花钱就等于
// 额度还在。
//
// # 这个包为什么单独存在
//
// 判定要落在**两处**：
//
//   - 入口层的一次只读早拒绝（见 internal/api 的 QuotaChecker）：省掉一次
//     预留事务，以及最多 maxRequestBodyBytes 的请求体读入；
//   - 预留事务里的权威判定（见 internal/ledger/store 的 SaveReservation）：
//     与插 pending 行同一个事务，靠行锁串行化。
//
// 两处的口径必须逐字相同。分开放的话，两处迟早会不一致——而那时一把 key
// 会在入口放行、在事务里被拒（或者反过来），两端各自的代码看起来都对。
// 所以 SQL 与判据都只有这一份，两个调用方只是**连接的持有者**不同
// （*sql.DB 与 *sql.Tx）。
package keyquota

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/sumwai/larkax/internal/money"
)

// ErrExceeded 表示这把 key 的累计花费已经用完了它的额度。
//
// 调用方按 errors.Is 认它，据此回 429 与机器可读的码（见 internal/api 的
// writeInsufficientQuota）。它**不**是「余额不足」：那个是用户钱包的事
// （见 internal/ledger/store 的 ErrInsufficientAvailable），这个只关乎
// 一把 key 的上限，用户的余额可能还剩很多。
var ErrExceeded = errors.New("API Key 的额度已用尽")

// Querier 是判定需要的最小数据库能力。
//
// 只声明 QueryRowContext 一个方法：*sql.DB 与 *sql.Tx 都满足它，于是同一份
// 读法既能用在入口的只读检查上，也能用在预留事务里。多声明一个方法就会把
// 其中一侧挡在外面。
type Querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// MultiQuerier 是批量展示读需要的能力（见 SpentMany）。
//
// 它与 Querier 分开而不合并：判定那一侧只需要单行读，把 QueryContext 也
// 声明进 Querier 会让判定的依赖面随着展示的读法一起变宽。
type MultiQuerier interface {
	Querier
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// spentSQL 是「这把 key 累计花了多少」的唯一口径。
//
// 它按 api_key_id 过滤，于是 api_key_id 为 NULL 的历史行（补上写入点之前的
// 那些，见迁移 00017 的列注释）自然落在外面——那些行不属于任何一把 key，
// 并进哪一把去汇总都是错的。
//
// 结果转成 text 再交给 money.Parse，而不是让驱动扫进一个浮点数：金额从
// numeric 到 Go 的这一段必须走十进制字符串，中间出现一次 float64 就是一个
// 对不上账的舍入点（见 internal/money 与 scripts/check-layering.sh 的第二条）。
//
// COALESCE 是必要的：这把 key 一笔都没有过时 SUM 返回 NULL，而 NULL 扫进
// string 会失败。返回 0 而不是让调用方处理 NULL，是因为「没有消费」与
// 「消费了 0」在额度判定里是同一件事。
const spentSQL = `
	SELECT COALESCE(SUM(CASE
	             WHEN status = 'settled' THEN final_amount
	             WHEN status = 'pending' THEN reserved_amount + plan_reserved_amount
	             ELSE 0
	         END), 0)::text
	  FROM billing_requests
	 WHERE api_key_id = $1`

// limitSQL 读出这把 key 的额度上限，并**锁住这一行**。
//
// 加锁是这个包里唯一不能省的东西。判据的形状是「读一次已花金额，再插一行
// pending」，而这两步之间只要有另一个事务插进一行，两个并发请求就都会看到
// 「没超」，于是都放行——上限被穿过去，且事后从数据上完全看不出来。锁住
// key 这一行，同一把 key 的预留就串行化了：后来的那个会看到前一个已经提交
// 的 pending 行。
//
// 为什么锁 api_keys 而不是别的行：额度长在这一行上，而**额度是逐把 key 设
// 的**。同一把 key 的两个请求即使落在不同的钱包行上（资产不同），也必须互相
// 看见，所以锁钱包是不够的。预留事务本来就先锁了钱包行（见 SaveReservation
// 的 lockWallets），那把锁在今天也能挡住同一把 key 的并发——但那是钱包结构的
// 副作用，不是这里的保证；把它当保证，将来改一次钱包的粒度就会静默失效。
//
// 代价说在这里：这把锁让同一把 key 的预留串行，而那是额度本来就要求的语义
// （不串行就必然穿透）。它多出来的耦合只有一处——auth 的 touch 会 UPDATE
// 同一行，于是它的写可能在这把锁上等一小会儿。touch 是每分钟每把 key 至多
// 一次的最佳努力写、错误显式丢弃（见 internal/auth 的 touch），所以这个等待
// 最坏也就是「这一分钟的 last_used_at 没刷新」。
const limitSQL = `SELECT quota_limit::text FROM api_keys WHERE id = $1 FOR UPDATE`

// over 是唯一的判据：已花加上这一笔**超过**上限就是超了。
//
// 用 `>` 而不是 `>=`：恰好用满是允许的。「已花 100、上限 100」不再放行新的
// 一笔，但「已花 90、上限 100、这一笔 10」正好把它填满，该放行——这一笔本身
// 没有越界。两者的差别只在边界上，而边界正是额度唯一会被追问的地方。
func over(spent, additional, limit money.Amount) bool {
	return spent.Add(additional).Cmp(limit) > 0
}

// Spent 读出这把 key 的累计花费。
//
// 它不判断、不加锁：口径在 spentSQL 里，判据在 over 里，两处都只有一份。
func Spent(ctx context.Context, q Querier, keyID int64) (money.Amount, error) {
	var text string
	if err := q.QueryRowContext(ctx, spentSQL, keyID).Scan(&text); err != nil {
		return money.Zero, fmt.Errorf("读 API Key %d 的累计花费失败: %w", keyID, err)
	}
	spent, err := money.Parse(text)
	if err != nil {
		return money.Zero, fmt.Errorf("API Key %d 的累计花费 %q 解析失败: %w", keyID, text, err)
	}
	return spent, nil
}

// Check 是入口层的只读判定：这把 key 现在还收不收得下一笔。
//
// 它**不加锁、不开事务**，所以读到的可能与权威判定读到的不一样——并发的
// 两个请求可能都过得了这一关。那不是缺陷，这一层只是早拒绝（见 internal/api
// 的 QuotaChecker）：把已经明显超了的请求挡在读请求体之前。
//
// 因此这里用 additional = 0，也就是「已花**已经超过**上限才拒」。它绝不会
// 拒掉一笔 CheckInTx 会放行的请求：金额非负时 `spent > limit` 蕴含
// `spent + amount > limit`。反过来（入口更松）只会多走一次事务，那正是这一层
// 愿意付的代价。
func Check(ctx context.Context, q Querier, keyID int64, limit money.Amount) error {
	spent, err := Spent(ctx, q, keyID)
	if err != nil {
		return err
	}
	if over(spent, money.Zero, limit) {
		return fmt.Errorf("%w: 已花 %s，上限 %s", ErrExceeded, spent, limit)
	}
	return nil
}

// CheckInTx 是权威判定，**必须**在预留事务里、且在任何会改动已花金额的写入
// 之前调用。
//
// 顺序是「锁 → 读 → 判」：这次调用自己拿锁（limitSQL 的 FOR UPDATE），调用方
// 紧接着插 pending 行。中间那一段就是判据的原子区间，它的两端都在同一个
// 事务里，所以要么两笔都看见对方，要么谁也看不见谁——不存在「都看到没超」。
func CheckInTx(ctx context.Context, tx *sql.Tx, keyID int64, additional money.Amount) error {
	var limitText sql.NullString
	if err := tx.QueryRowContext(ctx, limitSQL, keyID).Scan(&limitText); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// api_keys 的外键保证「插得进 billing_requests 就一定有这一行」，
			// 所以查不到只可能是调用方给的 id 不是一把真 key。报出来，不要
			// 当成「没设上限」放行——那会把一次调用方的 bug 变成一次静默的
			// 无限额度。
			return fmt.Errorf("找不到 id=%d 的 API Key", keyID)
		}
		return fmt.Errorf("读 API Key %d 的额度上限失败: %w", keyID, err)
	}

	if !limitText.Valid {
		// 没设上限：这把 key 不限。**不查已花金额**——省掉这次求和，而它正是
		// 这个判据在热路径上的全部代价（见 internal/keyquota 的包注释）。
		// 剩下的只有一次按主键定位的单行读。
		return nil
	}

	limit, err := money.Parse(limitText.String)
	if err != nil {
		return fmt.Errorf("API Key %d 的额度上限 %q 解析失败: %w", keyID, limitText.String, err)
	}

	spent, err := Spent(ctx, tx, keyID)
	if err != nil {
		return err
	}
	if over(spent, additional, limit) {
		return fmt.Errorf("%w: 已花 %s，这一笔 %s，上限 %s",
			ErrExceeded, spent, additional, limit)
	}
	return nil
}

// spentManySQL 是 Spent 的批量形状：一次查一页 key 的累计花费。
//
// 与 spentSQL 必须逐个口径字面相同——两条 SQL 各写一遍时，改了一处另一处
// 还是老样子，而它们回答的是同一个问题。所以这里不另写一份 CASE：
// 上面那条是「一把 key」，这条是「一组 key」，其余逐字照抄。
//
// 结果是每个 key 一行；一笔都没有过的 key 不会出现在结果里，由调用方补 0。
const spentManySQL = `
	SELECT api_key_id, COALESCE(SUM(CASE
	             WHEN status = 'settled' THEN final_amount
	             WHEN status = 'pending' THEN reserved_amount + plan_reserved_amount
	             ELSE 0
	         END), 0)::text
	  FROM billing_requests
	 WHERE api_key_id = ANY($1::bigint[])
	 GROUP BY api_key_id`

// SpentMany 一次读出多把 key 的累计花费，口径与 Spent 相同。
//
// 它收 MultiQuerier 而不是 Querier：判定的两条路径只需要一次单行读，而
// 批量读需要 QueryContext。把两个方法都塞进 Querier 会让「判定需要什么」
// 与「展示需要什么」长成同一个接口，而将来多一个读法就要再改一次判定那一侧。
//
// 返回的 map 里**每一个传入的 key 都有一项**：没有消费的（以及不存在的）是
// 零。少了这一条，调用方就要在渲染每一行时判一次「map 里有没有」，而漏判
// 的那一行会把「没花过钱」打成空字符串——那看起来像数据缺失，不像 0。
//
// 没有请求的 key 不查库：空切片进 SQL 也能跑，但一次注定返回空集的往返
// 只会让调用方在空清单上多等一个网络来回。
func SpentMany(ctx context.Context, q MultiQuerier, keyIDs []int64) (map[int64]money.Amount, error) {
	out := make(map[int64]money.Amount, len(keyIDs))
	if len(keyIDs) == 0 {
		return out, nil
	}
	for _, id := range keyIDs {
		out[id] = money.Zero
	}

	rows, err := q.QueryContext(ctx, spentManySQL, keyIDs)
	if err != nil {
		return nil, fmt.Errorf("读 API Key 的累计花费失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			id   int64
			text string
		)
		if err := rows.Scan(&id, &text); err != nil {
			return nil, fmt.Errorf("读 API Key 的累计花费失败: %w", err)
		}
		spent, err := money.Parse(text)
		if err != nil {
			return nil, fmt.Errorf("API Key %d 的累计花费 %q 解析失败: %w", id, text, err)
		}
		out[id] = spent
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 API Key 的累计花费失败: %w", err)
	}
	return out, nil
}
