package plans

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/money"
)

// 本文件是额度包期末按比例结转的另一半：比例怎么读、两维怎么切、以及
// 「待并入的额度怎么并进新订阅」。
//
// # 两个方向，四个数
//
// 一条订阅的一生里有两个与结转有关的时刻：
//
//   - 期末（到期作废）：还剩的面值按比例留在 carryover:<订阅 id> 上，其余归
//     平台。留在科目上的金额是权威值；量那一维没有账本可查（分录只有 asset，
//     没有 unit），所以写在 user_plans.carryout_quota 上，并入时从那里读。
//   - 领新包（发放）：该用户手上所有待并入的结转额度并进新订阅的
//     prepaid:<新订阅 id>，并写进新订阅的 carried_quota / carried_amount。
//
// 进来的一组（carried_*）与出去的一组（carryout_*）分开两列：它们语义相反，
// 而任何不带状态过滤的查询都会把两组加到一起。
//
// # 为什么比例是现读的
//
// 结转只在期末发生一次，一年也没有几次，没有必要缓存。现读的代价可以忽略，
// 换来的是一句可以写进文档的话：改完这条配置，下一次期末处置就生效。

// carryoverBpKey 是期末结转比例的配置键（万分点）。
const carryoverBpKey = "plans.carryover_bp"

// carryAbortNote 接在每一次「比例读不出来」的错误后面。
//
// 说清三件事：这次作废整个没有执行、订阅还是 active、面值一分没动。少了这句，
// 运维看到的是一句「配置读不出来」，而第一反应会是「那作废到底做了没有」——
// 这句话必须由报错的人给，因为调用方只拿到一个 error。
const carryAbortNote = "本次作废未执行：订阅仍是 active，面值余额与在途预留都没有改动，也没有写任何分录"

// carryoverBasisPoints 从 settings 表读期末结转比例。
//
// **读不出来就让整次期末作废失败**，绝不回落到 0。回落的表现是「结转悄悄停了」：
// 用户到期后什么都没留下，而库里、日志里都没有任何东西说这是配置的问题。宁可
// 让那次作废失败、由运维修好配置再重跑——作废是幂等的，重跑安全。
//
// 四种读不出来的情形在这里收成一个错误，都点名键名：行不存在（迁移没跑、
// 被人删了）、值不是合法 JSON、不是整数、超出 0…10000。
func carryoverBasisPoints(ctx context.Context, tx *sql.Tx) (int64, error) {
	var raw []byte
	err := tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = $1`, carryoverBpKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("settings.%s 不存在：迁移 00065 没有跑，或者这一行被人删了；%s",
			carryoverBpKey, carryAbortNote)
	}
	if err != nil {
		return 0, fmt.Errorf("读 settings.%s 失败；%s: %w", carryoverBpKey, carryAbortNote, err)
	}

	var bp int64
	if err := json.Unmarshal(raw, &bp); err != nil {
		return 0, fmt.Errorf("settings.%s 不是整数（库里的原文是 %q）；%s",
			carryoverBpKey, raw, carryAbortNote)
	}
	if bp < 0 || bp > money.MaxBasisPoints {
		return 0, fmt.Errorf("settings.%s = %d，超出 0…%d（万分点）；%s",
			carryoverBpKey, bp, money.MaxBasisPoints, carryAbortNote)
	}
	return bp, nil
}

// wholeUnitUnits 是 token / 次数包「一个整单位」在 money 定点数里的值。
//
// 由 money.Scale 算出来而不是写死 1e8：两者是同一个定义的两种写法，写死之后
// 精度一旦变化，这里会在没有人改动的地方悄悄错一位。
var wholeUnitUnits = func() int64 {
	u := int64(1)
	for i := 0; i < money.Scale; i++ {
		u *= 10
	}
	return u
}()

// carryShare 取 base 的 bp/10000，向下取整；base 非正时为零。
//
// 取整精度按单位分两档：
//
//   - 金额包（UnitAmount）：按 money 的最小单位（10^-8）向下截断。金额这一维
//     本来就有这个精度可用，再往「元」上截等于把分与厘送进「归平台」——那是
//     用户确实剩下的钱，账上对得上，没有理由扣掉。MulBasisPoints 内部对正数
//     是向零截断到 1e-8，所以这一档直接取它的结果。
//   - token / 次数包（UnitToken / UnitRequest）：截到整单位，即整 token、整次。
//     量本来就是整数，「剩余 7、比例 0.4 → 2」是这条规则的定义（见 #1193）。
//
// 两档都是向下取整：面值那一维被截掉的零头随 plan_void 归平台，量那一维被
// 截掉的部分留在终态订阅上、随订阅一起不可用（不进平台科目）。于是「结转 +
// 归平台」严格等于取整前的那份剩余面值，不会多出一点没有来路的额度；每一维
// 各自至多差一个**自己的**取整单位（金额包 10^-8，token / 次数包 1）。向下
// 取整而不是四舍五入，与商户分成的口径一致（见 money.MulBasisPoints）：零头
// 不能记到用户名下。
func carryShare(unit PlanUnit, base money.Amount, bp int64) money.Amount {
	if !base.IsPositive() {
		return money.Zero
	}
	share := base.MulBasisPoints(bp)
	if !share.IsPositive() {
		return money.Zero
	}
	if unit == UnitAmount {
		return share
	}
	return money.FromUnits(share.Units() / wholeUnitUnits * wholeUnitUnits)
}

// carrySplit 算一次期末结转的两个量。
//
//	carry_amount = floor(max(0, R − carried_amount) × bp / 10000)
//	carry_quota  = floor(max(0, Q − carried_quota)  × bp / 10000)   （token / 次数包）
//
// R 是扣掉在途预留之后的面值余额；Q 是 quota_total + carried_quota −
// quota_used − 在途预留，也就是这份订阅现在还能用多少单位（见
// store.PlanQuotaUsage，与请求路径的可用量同一个表达式）。两维各自再减掉
// 「领这条订阅时并进来的量」，所以结转进来的额度不会被结转第二次——那是本功能
// 最容易做错的一格，也是「上期结转 100、本期发放 100、用掉 50 → 结转 20」
// 那条用例钉住的性质。
//
// 两维**各算各的**、各自向下取整：从一维推另一维会在 token 与面值的比例偏离
// 标价时把量算错，而那种偏离正是 token 包存在的理由（1 万 token 在贵模型与
// 便宜模型上值不同的钱）。
//
// 向下取整的差额留给平台：plan_void 记 R − carry_amount，plan_carry 记
// carry_amount，两者之和严格等于 R——面值这一维在账上严格守恒。量那一维的
// 差额留在订阅上，随订阅终态一起不再可用。取整精度按单位分档（见 carryShare）：
// 金额包以 10^-8 为单位，token / 次数包以整单位为单位，两维各自至多差一个
// 自己的取整单位。
//
// # 取整到 0 时的代价
//
// 两个方向不对称：
//
//   - 面值算出 0 时，量也一并置 0。账本上没有一分钱对应这份量，而并入的闸门
//     是账本余额（见 mergeCarryovers）——留一个量为正、余额为零的源，它永远
//     并不过去，只会让「待并入」这份名单里多一条读不懂的行。
//   - 量算出 0 而面值不为 0 时，面值照转。钱是账上的钱，把有理由留下的钱
//     强行归零没有依据；量为 0 的那份代价则是实实在在的。
//
// 于是可能出现「量还剩一些、面值已经归零」，那份量就跟着订阅一起没了。这是
// 诚实写出来的代价——不是「什么都没损失」，而是「这一格损失的是量，且账上
// 看不出来」。
func carrySplit(unit PlanUnit, basisAmount, basisQuota, carriedAmount, carriedQuota money.Amount, bp int64) (amount, quota money.Amount) {
	amount = carryShare(unit, basisAmount.Sub(carriedAmount), bp)
	if !amount.IsPositive() {
		// 见「取整到 0 时的代价」：金额落零时量也不结转，否则量会挂在一笔
		// 没有对面分录的余额上，而并入永远读不到它。
		return money.Zero, money.Zero
	}
	if unit == UnitAmount {
		// 金额包的量与面值是同一个数（见 plans 包的注释），不必各算一次。
		return amount, amount
	}
	quota = carryShare(unit, basisQuota.Sub(carriedQuota), bp)
	if !quota.IsPositive() {
		// 金额结转了但量没结转（例如 Q 已经被别的维度用掉）：金额照转——它是
		// 账上的钱，把有理由留下的钱强行归零没有依据；量则为 0，代价同上。
		return amount, money.Zero
	}
	return amount, quota
}

// periodEndedIn 判断这次作废能不能结转。
//
// 只有**真的到期**才结转：终态是 expired、订阅有到期时刻、且库里的 now()
// 已经走到那一刻之后（时间以库为准，本机时钟与库不一致时不能让它决定账）。
//
// 提前把一条还没到期的订阅标成 expired（`plan void --status expired`）**不**
// 结转：到期是时间到了的事实，提前标成 expired 是运营收回——那份额度该像
// canceled 一样整笔归平台，而不是留一份「提前到期也能带走」的口子。
func periodEndedIn(ctx context.Context, tx *sql.Tx, row UserPlan, status UserPlanStatus) (bool, error) {
	if status != UserPlanExpired {
		return false, nil
	}
	if row.ExpiresAt == nil {
		// 没有到期时刻就不存在「到期」这件事，也就没有结转的依据。
		return false, nil
	}
	var due bool
	if err := tx.QueryRowContext(ctx,
		`SELECT $1::timestamptz <= now()`, *row.ExpiresAt).Scan(&due); err != nil {
		return false, fmt.Errorf("读数据库当前时间失败: %w", err)
	}
	return due, nil
}

// lockPlanAccount 给一个 prepaid / carryover 科目上一把事务级锁。
//
// 它与 store.PostTransactionTx 里 lockReadThenWriteAccounts 上的是同一把
// （pg_advisory_xact_lock 按字符串取），所以这里先拿、写分录时再拿，是同一把锁
// 的两次计数，不会互等。
func lockPlanAccount(ctx context.Context, tx *sql.Tx, acct ledger.Account) error {
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, acct.String()); err != nil {
		return fmt.Errorf("锁定科目 %s 失败: %w", acct, err)
	}
	return nil
}

// carryTxnID 把（用途前缀 + 单号）确定性映射成一枚交易号。
//
// 与命令行那几处（wallet credit / adjust）同一个做法与同一个理由：重跑同一条
// 作废 / 发放时，结转与并入要撞上同一笔交易并被判成重放，而不是再记一遍。
// 域前缀不同，于是同一个作废单号算出的作废交易与结转交易是两笔。
func carryTxnID(domain, ref string) ledger.TxnID {
	sum := sha256.Sum256([]byte(domain + "\x00" + ref))
	var b [16]byte
	copy(b[:], sum[:16])
	b[6] = (b[6] & 0x0f) | 0x80
	b[8] = (b[8] & 0x3f) | 0x80

	var out [36]byte
	hex.Encode(out[0:8], b[0:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:36], b[10:16])
	return ledger.TxnID(out[:])
}

// 结转与并入各自的交易号域。分开是必须的：两者都由同一个作废/发放单号派生，
// 共用一个前缀会让它们算成同一笔交易，于是并入要么被判成冲突、要么被判成重放。
const (
	carryTxnDomain      = "larkax:plan-carry:v1"
	carryMergeTxnDomain = "larkax:plan-carry-merge:v1"
)

// carryoverSource 是一条待并入的源订阅。
type carryoverSource struct {
	// SubscriptionID 是源订阅，也就是 carryover 科目的后缀。
	SubscriptionID int64
	// Amount 是这条源上的**账本余额**，并入时按它写分录。它是金额那一维的
	// 唯一权威——user_plans.carryout_amount 只是镜像。
	Amount money.Amount
	// Quota 是这一刻在 user_plans.carryout_quota 上记的量。账本没有 unit，
	// 并入只能从这里读。
	Quota money.Amount
}

// pendingCarryoverIDs 读出该用户名下可能还有待并入结转的订阅 id。
//
// 只是候选名单：真正的闸门是「锁内读出来的 carryover 余额 > 0」（见
// mergeCarryovers）。名单按 (asset, unit) 过滤——结转只能并进同币种、同计量
// 单位的包，跨单位并进去会让「可用量」变成一个没有意义的数。
func pendingCarryoverIDs(ctx context.Context, tx *sql.Tx, userID int64, asset ledger.Asset, unit PlanUnit, excludeID int64) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT up.id
		  FROM user_plans up
		  JOIN plans p ON p.id = up.plan_id
		 WHERE up.user_id = $1
		   AND up.id <> $2
		   AND p.asset = $3
		   AND p.unit = $4
		   AND (up.carryout_quota > 0 OR up.carryout_amount > 0)
		 ORDER BY up.id`, userID, excludeID, string(asset), string(unit))
	if err != nil {
		return nil, fmt.Errorf("读用户 %d 的待并入结转失败: %w", userID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("读用户 %d 的待并入结转失败: %w", userID, err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历用户 %d 的待并入结转失败: %w", userID, err)
	}
	return out, nil
}

// mergeCarryovers 把该用户手上所有待并入的结转额度并进一条新订阅。
//
// # 闸门只有一条：账本余额 > 0
//
// 名单来自 user_plans 上的两个 carryout 列，但**能不能并**只看
// carryover:<源> 的账本余额，而且是在拿到锁之后重读的余额。为零的源整个跳过，
// 连它的 carryout_quota 也不并——没有金额对应的一份额度并进新订阅，等于凭空
// 多给出可用量，而账上没有任何东西兜着它。
//
// # 锁
//
// 每个源一条 `carryover:<源订阅 id>`，按**科目字符串**排序后逐个取。排序是
// 为了避免两笔并发并入互相等成环；用字符串而不是订阅 id 排序，是为了与
// store.PostTransactionTx（lockReadThenWriteAccounts 同样按科目字符串排序）
// 落在同一个全局次序上：任何同时碰多个 carryover 科目的事务都从同一个方向
// 开始。`carryover:` 与 `prepaid:` 之间也是同一个字典序（c < p），所以期末
// 结转那条路先拿 carryover 再拿 prepaid（见 VoidTx）。
//
// 返回并进了多少（金额、量）。没有可并的源时返回两个零，调用方不必区分
// 「没有源」与「源余额都是零」——两种都是「这次没有并入」。
func mergeCarryovers(ctx context.Context, s *Service, tx *sql.Tx, newSubscriptionID, userID int64, asset ledger.Asset, unit PlanUnit, ref string) (money.Amount, money.Amount, error) {
	ids, err := pendingCarryoverIDs(ctx, tx, userID, asset, unit, newSubscriptionID)
	if err != nil || len(ids) == 0 {
		return money.Zero, money.Zero, err
	}

	// 排序后取锁：科目字符串的字典序就是 store 的取锁顺序。
	type source struct {
		id   int64
		acct ledger.Account
	}
	ordered := make([]source, 0, len(ids))
	for _, id := range ids {
		ordered = append(ordered, source{id: id, acct: ledger.CarryoverAccount(id)})
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].acct.String() < ordered[j].acct.String() })
	for _, src := range ordered {
		if lockErr := lockPlanAccount(ctx, tx, src.acct); lockErr != nil {
			return money.Zero, money.Zero, lockErr
		}
	}

	// 锁拿到之后逐个读余额与量。
	var (
		sources []carryoverSource
		amounts []money.Amount
		quotas  []money.Amount
	)
	for _, src := range ordered {
		balance, readErr := accountBalanceInTx(ctx, tx, src.acct, asset)
		if readErr != nil {
			return money.Zero, money.Zero, readErr
		}
		if !balance.IsPositive() {
			// 见「闸门只有一条」：零或负的源整个跳过，量也不并。
			continue
		}
		var rawQuota string
		if quotaErr := tx.QueryRowContext(ctx,
			`SELECT carryout_quota::text FROM user_plans WHERE id = $1`, src.id).Scan(&rawQuota); quotaErr != nil {
			return money.Zero, money.Zero, fmt.Errorf("读源订阅 %d 的结转量失败: %w", src.id, quotaErr)
		}
		quota, parseErr := parseMoney(rawQuota, fmt.Sprintf("源订阅 %d 的结转出去量", src.id))
		if parseErr != nil {
			return money.Zero, money.Zero, parseErr
		}
		sources = append(sources, carryoverSource{SubscriptionID: src.id, Amount: balance, Quota: quota})
		amounts = append(amounts, balance)
		quotas = append(quotas, quota)
	}
	if len(sources) == 0 {
		return money.Zero, money.Zero, nil
	}
	totalAmount, ok := money.SumChecked(amounts...)
	if !ok {
		return money.Zero, money.Zero, errors.New("待并入的结转金额之和超出金额类型能表示的范围")
	}
	totalQuota, ok := money.SumChecked(quotas...)
	if !ok {
		return money.Zero, money.Zero, errors.New("待并入的结转量之和超出金额类型能表示的范围")
	}

	moves := make([]ledger.PlanCarryMove, 0, len(sources))
	for _, src := range sources {
		moves = append(moves, ledger.PlanCarryMove{SourceSubscriptionID: src.SubscriptionID, Amount: src.Amount})
	}
	txn, err := ledger.PlanCarryMerge(ledger.PlanCarryMergeParams{
		TxnID:                carryTxnID(carryMergeTxnDomain, ref),
		TargetSubscriptionID: newSubscriptionID,
		Moves:                moves,
		Asset:                asset,
		Ref:                  ref,
	})
	if err != nil {
		return money.Zero, money.Zero, err
	}
	applied, err := s.ledger.PostTransactionTx(ctx, tx, txn)
	if err != nil {
		return money.Zero, money.Zero, err
	}
	if !applied {
		// 这次发放刚建了一条新订阅，并入的交易号却已经有内容相同的分录在库里：
		// 两件事对不上（并入只可能随某一次发放发生一次），不能把新订阅当成
		// 已经并过。
		return money.Zero, money.Zero, fmt.Errorf(
			"并入交易号 %s 已经存在一笔内容相同的分录，这次却新建了订阅 %d；"+
				"同一个发放单号不能对应两次并入", txn.TxnID, newSubscriptionID)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE user_plans
		   SET carried_quota = $2::numeric, carried_amount = $3::numeric
		 WHERE id = $1`, newSubscriptionID, totalQuota.String(), totalAmount.String()); err != nil {
		return money.Zero, money.Zero, fmt.Errorf("把结转额度写进新订阅 %d 失败: %w", newSubscriptionID, err)
	}
	return totalAmount, totalQuota, nil
}
