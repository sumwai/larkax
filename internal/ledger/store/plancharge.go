package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"github.com/sumwai/larkax/internal/billing"
	"github.com/sumwai/larkax/internal/idempotency"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/pipeline"
)

// 本文件是请求路径上的**额度包选源与预扣**（#860 第 2 步）。
//
// # 它为什么在落库层
//
// 选源的结果必须与预留行写在同一个事务里，否则「挑中了一个包」与「它当时还够」
// 之间隔着一个窗口：并发的另一笔请求可以把量用掉，然后这边照样记下一笔包覆盖
// 不了的扣除。判断与写入因此不能分开——而只有这一层握着那个事务。
//
// 读侧（internal/plans）仍然负责「谁拥有什么包、范围是什么」的查询；这里只做
// 请求路径需要的那一份**在事务内**的重复判断。两处口径写在同一个函数里
// （planCovers），所以它们不会各说各的。
//
// # 两个维度
//
// 每一条候选都要同时满足两件事：
//
//   - 面值够：prepaid 科目余额 − 该订阅在途预留的面值 ≥ 本次预留金额；
//   - 可用量够：quota_total − quota_used − 该订阅在途预留的量 ≥ 本次要占的量。
//
// 金额包没有第二维（它的可用量与面值是同一个数），只判面值。token 包要占
// 输入估算加输出上界；次数包每次占 1。
//
// # 不跨包拆单
//
// 一条请求只从**一个**订阅扣。拆分要在账本上写两笔借、两处商户分成，而「哪个包
// 出多少钱」还会随包的剩余量变化；整单顺延到下一个包更简单，也与 #860 定的
// 「取第一个能覆盖的」一致。

// ChargeOrderSource 回答「这个用户这次请求按什么顺序挑扣费来源」。
//
// 实现在 internal/plans（它已经处理了用户覆盖、平台默认与两级兜底）。这里只声明
// 方法，不 import 那个包：plans 依赖本包（它用本包写分录），反向 import 会成环。
// 装配层负责把它接上；没接上时按 plan_first 走——那是平台默认值，也是迁移
// 00051 写进去的那一行（两处要一致，见 plans.DefaultChargeOrder）。
type ChargeOrderSource interface {
	// RequestChargeOrder 返回三个取值之一：plan_first / balance_first /
	// balance_only。**不返回错误**：请求路径不因为读配置失败而拒绝服务，
	// 实现要做的是回落到默认值并记日志（见 #918 的兜底那一节）。
	RequestChargeOrder(ctx context.Context, userID int64) string
}

// 扣费顺序的三个取值。与本包外面那两个来源（users.charge_order 的 CHECK、
// plans.ChargeOrder 的常量）是同一组字符串。
const (
	chargeOrderPlanFirst    = "plan_first"
	chargeOrderBalanceFirst = "balance_first"
	chargeOrderBalanceOnly  = "balance_only"
)

// SetChargeOrderSource 接上扣费顺序的读点。装配层调用；测试不接时按平台默认走。
func (s *Store) SetChargeOrderSource(src ChargeOrderSource) { s.chargeOrder = src }

// chargeOrderFor 取这个用户的有效扣费顺序，认不出的取值按平台默认 plan_first 走。
//
// 认不出只可能来自实现方的 Bug（接口只承诺三个取值）。往保守的那一侧收：
// 先额度包后余额正是平台默认值，也是迁移里那一行的取值。
func (s *Store) chargeOrderFor(ctx context.Context, userID int64) string {
	if s.chargeOrder == nil {
		return chargeOrderPlanFirst
	}
	switch order := s.chargeOrder.RequestChargeOrder(ctx, userID); order {
	case chargeOrderPlanFirst, chargeOrderBalanceFirst, chargeOrderBalanceOnly:
		return order
	default:
		s.log().Warn("扣费顺序的读点给了一个认不出的取值，按 plan_first 走",
			"user_id", userID, "value", order)
		return chargeOrderPlanFirst
	}
}

// planCovers 判断一条候选够不够覆盖这次预留。
//
// 判据集中在这一个函数里：面值与可用量各一条，金额包只看面值。调用它的两处
// （初次选源与流式补冻）因此不会对「够不够」各有一套说法。
//
// faceAvail 与 quotaAvail 由调用方在**锁内**读出。amount 是这次要占的面值，
// quota 是这次要占的可用量（金额包为零）。
func planCovers(unit string, amount, faceAvail, quota, quotaAvail money.Amount) bool {
	if !faceAvail.IsPositive() || faceAvail.Cmp(amount) < 0 {
		return false
	}
	if unit == "amount" {
		return true
	}
	return quotaAvail.IsPositive() && quotaAvail.Cmp(quota) >= 0
}

// selectPlanCandidates 读出这个用户这次请求的**有序**候选（订阅 id）：未过期、
// 有效、同币种、模型范围覆盖本次规范名，再与 key 的允许集求交。
//
// 顺序是 #860 定的「先到期的先用」：`period_end` 升序，同一时刻按订阅 id 升序
// （确定性的次序才能写出可断言的用例）。赠送先于付费那一档 B 阶段没有字段，
// 不在这里判。
//
// 这条查询**不加锁**，也只读 id：判断要用的状态、余额与在途占用量都在真正
// 逐条判断的那一刻、锁内重读（见 reserveFromPlan）。提前把全部候选锁住会让
// 一次请求堵住用户的所有订阅；而在锁外读的量本来就会被并发的另一笔请求改掉
// ——候选只是「值得看一眼的名单」。
//
// # 「哪些包适用于本次请求」在这里是刻意的第二份谓词
//
// 模型范围与 key 允许集的两条子句，与 internal/plans 的归属判据是同一条
// 语义：`plan_models` 为空表示不限模型（见 plans.go 的「模型范围」一节），
// `api_key_plans` 没有行表示这把 key 能用户名下全部订阅（见 apikeyplans.go
// 的「空集合 = 全部」）。两处都要回答「这个包能不能用」，却没法共用一条 SQL：
// 那两处按包展开给展示与配置用，这里要在**事务内**按订阅过滤。所以各写一份，
// 改动其中一处时对照另一处一起看。
func selectPlanCandidates(ctx context.Context, tx *sql.Tx, req pipeline.Request, userID int64, asset string) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT up.id
		  FROM user_plans up
		  JOIN plans p ON p.id = up.plan_id
		 WHERE up.user_id = $1
		   AND up.status = 'active'
		   AND (up.period_end IS NULL OR up.period_end > now())
		   AND p.asset = $2
		   AND (NOT EXISTS (SELECT 1 FROM plan_models pm WHERE pm.plan_id = p.id)
		        OR EXISTS (SELECT 1 FROM plan_models pm
		                    WHERE pm.plan_id = p.id AND pm.model = $3))
		   AND (NOT EXISTS (SELECT 1 FROM api_key_plans a WHERE a.api_key_id = $4)
		        OR EXISTS (SELECT 1 FROM api_key_plans a
		                    WHERE a.api_key_id = $4 AND a.user_plan_id = up.id))
		 ORDER BY up.period_end ASC NULLS LAST, up.id ASC`,
		userID, asset, req.CanonicalModel, req.APIKeyID)
	if err != nil {
		return nil, fmt.Errorf("读额度包候选失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("读额度包候选失败: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历额度包候选失败: %w", err)
	}
	return out, nil
}

// quotaNeed 算出这次预留要从包里占多少可用量。
//
// 金额包不占可用量（它的「量」就是钱，由面值那一侧判）；token 包占输入估算加
// 输出上界；次数包每次占 1。预留按上界占，结算按实际改——与金额那侧的
// 「预留是上界」是同一条规则。
func quotaNeed(unit string, req pipeline.Request, r *pipeline.Reservation) (money.Amount, error) {
	switch unit {
	case "amount":
		return money.Zero, nil
	case "token":
		return quotaFromCount(req.InputTokens + r.OutputTokens)
	case "request":
		return quotaFromCount(1)
	default:
		return money.Zero, fmt.Errorf("额度包的计量单位认不出: %q", unit)
	}
}

// quotaFromCount 把「多少个单位」换成库里的 numeric(20, 8)。
//
// 走十进制字符串而不是自己乘 1e8：库里的列是 numeric(20, 8)，money 的表示是
// 同一套小数位，Parse 会替我们判溢出。自己乘会在大的 token 数上先回绕，而回绕
// 出来的数是能存进库的——那正是最坏的一种错。
func quotaFromCount(n int64) (money.Amount, error) {
	if n < 0 {
		return money.Zero, fmt.Errorf("可用量不能为负: %d", n)
	}
	amt, err := money.Parse(strconv.FormatInt(n, 10))
	if err != nil {
		return money.Zero, fmt.Errorf("把 %d 个单位换成金额失败: %w", n, err)
	}
	return amt, nil
}

// planQuotaAvailable 是「这份订阅现在还能用多少单位」的唯一表达式。
//
// 选源与流式补冻都调它，于是两处不会对「可用量」各有一套算法——补冻那处漏掉
// 一项的后果是流式请求在结转额度上用不了，而初次选源那处漏掉一项的后果相反
// （选了一个其实不够的订阅），两种都只在真流量上现形。
//
// carried_quota 是领到新包时从旧订阅并进来的结转量（见 00065 与
// plans 的 Carryover）。它加在总量这一侧，不是减在已用量那一侧：结转来的量
// 从来没被「用过」，把它记进 quota_used 会让「已用量」这个数谎报。
func planQuotaAvailable(total, carried, used, pending money.Amount) money.Amount {
	return total.Add(carried).Sub(used).Sub(pending)
}

// reserveFromPlan 依次检查候选，把第一条覆盖得住的记成这次预留的来源。
//
// 返回 ok=false 表示一条都不够（调用方按扣费顺序决定是否回落到余额）。
// 锁只有一把：订阅行。**不拿钱包锁**——额度包覆盖的请求不动钱包，余额为零、
// 只有额度包的用户必须能用。
//
// 每一条候选都在**它自己的锁里**重新读状态、余额与在途预留，然后做判断：
// 候选名单是加锁之前读的，两条请求可能都看到了同一个还有量的包。
func reserveFromPlan(ctx context.Context, tx *sql.Tx, req pipeline.Request, r *pipeline.Reservation, candidates []int64) (int64, bool, error) {
	for _, candID := range candidates {
		var (
			status, asset, unit                           string
			alive                                         bool
			quotaTotalStr, carriedQuotaStr, quotaUsedStr  string
			pendingQuotaStr, pendingAmountStr, balanceStr string
		)
		// 锁与读是**两条语句**，不能合并成一条。
		//
		// 第一条只锁订阅行：同一个订阅上的预留、结算与作废都从这里串行化。
		//
		// 第二条才求在途预留与账本科目余额。它必须是**锁到手之后**才开始的
		// 一条新语句：READ COMMITTED 下快照在语句开始时取，而等锁期间别的事务
		// 提交的 pending 行与分录，在等待前取的那份快照里看不见。合并成一条
		// （FOR UPDATE 与 LATERAL 求和同语句）时，两个并发请求各自都算得出
		// 「够」、都放行，在途之和超过面值与可用量——结算于是把 prepaid 扣成
		// 负数，而每一行单独看都合法。
		//
		// 未提交的行仍然任何快照都看不见（那是隔离级别，不是加锁能解决的），
		// 这里保证的是「前一笔已提交的占用一定算进来」，而同一订阅上的两笔
		// 预留必须走同一把行锁。
		var locked int
		err := tx.QueryRowContext(ctx, `
			SELECT 1 FROM user_plans WHERE id = $1 AND user_id = $2 FOR UPDATE`,
			candID, r.UserID).Scan(&locked)
		if errors.Is(err, sql.ErrNoRows) {
			// 在名单读出来之后被删了或者换了主人：跳过。
			continue
		}
		if err != nil {
			return 0, false, fmt.Errorf("锁定额度包订阅 %d 失败: %w", candID, err)
		}

		// 状态与过期时刻在锁内重判：名单是加锁之前读的，作废或到期清理可能已经
		// 把这一条改掉了，而那条更新不会去改候选切片。
		err = tx.QueryRowContext(ctx, `
			SELECT up.status, p.asset, p.unit,
			       (up.period_end IS NULL OR up.period_end > now()) AS alive,
			       p.quota_total::text, up.carried_quota::text, up.quota_used::text,
			       COALESCE(pend.q, 0)::text,
			       COALESCE(pend.m, 0)::text,
			       COALESCE(bal.s, 0)::numeric(20, 8)::text
			  FROM user_plans up
			  JOIN plans p ON p.id = up.plan_id
			  LEFT JOIN LATERAL (
			      SELECT SUM(reserved_quota) AS q, SUM(plan_reserved_amount) AS m
			        FROM billing_requests
			       WHERE user_plan_id = up.id AND status = 'pending'
			  ) pend ON true
			  LEFT JOIN LATERAL (
			      SELECT SUM(amount) AS s
			        FROM ledger_entries
			       WHERE account = 'prepaid:' || up.id AND asset = p.asset
			  ) bal ON true
			 WHERE up.id = $1`,
			candID).Scan(
			&status, &asset, &unit, &alive,
			&quotaTotalStr, &carriedQuotaStr, &quotaUsedStr,
			&pendingQuotaStr, &pendingAmountStr, &balanceStr)
		if err != nil {
			return 0, false, fmt.Errorf("读额度包订阅 %d 的余额与在途预留失败: %w", candID, err)
		}
		if status != "active" || !alive || asset != r.Asset {
			continue
		}

		quotaTotal, err := money.Parse(quotaTotalStr)
		if err != nil {
			return 0, false, fmt.Errorf("额度包订阅 %d 的可用量读不出来（原文 %q）: %w", candID, quotaTotalStr, err)
		}
		quotaUsed, err := money.Parse(quotaUsedStr)
		if err != nil {
			return 0, false, fmt.Errorf("额度包订阅 %d 的已用量读不出来（原文 %q）: %w", candID, quotaUsedStr, err)
		}
		carriedQuota, err := money.Parse(carriedQuotaStr)
		if err != nil {
			return 0, false, fmt.Errorf("额度包订阅 %d 的结转量读不出来（原文 %q）: %w", candID, carriedQuotaStr, err)
		}
		pendingQuota, err := money.Parse(pendingQuotaStr)
		if err != nil {
			return 0, false, fmt.Errorf("额度包订阅 %d 的在途预留读不出来（原文 %q）: %w", candID, pendingQuotaStr, err)
		}
		pendingAmount, err := money.Parse(pendingAmountStr)
		if err != nil {
			return 0, false, fmt.Errorf("额度包订阅 %d 的在途面值读不出来（原文 %q）: %w", candID, pendingAmountStr, err)
		}
		balance, err := money.Parse(balanceStr)
		if err != nil {
			return 0, false, fmt.Errorf("额度包订阅 %d 的面值余额读不出来（原文 %q）: %w", candID, balanceStr, err)
		}

		need, err := quotaNeed(unit, req, r)
		if err != nil {
			return 0, false, err
		}
		quotaAvail := planQuotaAvailable(quotaTotal, carriedQuota, quotaUsed, pendingQuota)
		faceAvail := balance.Sub(pendingAmount)
		if !planCovers(unit, r.Amount, faceAvail, need, quotaAvail) {
			continue
		}

		id, err := insertPlanReservationRow(ctx, tx, req, r, candID, need)
		if err != nil {
			return 0, false, err
		}
		// 快照：结算按这个 id 记 prepaid 科目，不重新选。
		r.UserPlanID = candID
		return id, true, nil
	}
	return 0, false, nil
}

// insertPlanReservationRow 写一条额度包覆盖的预留行。
//
// 与钱包那条路的差别只有三个数：`reserved_amount` 是 0（钱包一分没冻）、
// `plan_reserved_amount` 记这笔请求占着的面值、`reserved_quota` 记它占着的
// 可用量。其余字段与钱包那条路逐字一致——keyquota、幂等、协议校验都还在
// SaveReservation 里，两条路共用。
func insertPlanReservationRow(ctx context.Context, tx *sql.Tx, req pipeline.Request, r *pipeline.Reservation, planID int64, quota money.Amount) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `
		INSERT INTO billing_requests
			(request_id, user_id, api_key_id, channel_id, canonical_model, protocol,
			 streamed, reserved_amount, reserved_asset, status, user_agent,
			 tool_result_tail,
			 pricing_rule_id, pricing_expr_snapshot, started_at,
			 user_plan_id, reserved_quota, plan_reserved_amount)
		VALUES ($1, $2, $12, NULLIF($3, 0::bigint), $4, $5, $11, 0, $7, 'pending',
		        NULLIF($15, ''), $16, NULLIF($8, 0::bigint), NULLIF($9, ''), $10,
		        $13, $14::numeric, $6::numeric)
		RETURNING id`,
		req.RequestID, r.UserID, req.ChannelID, req.CanonicalModel, string(req.Protocol),
		r.Amount.String(), r.Asset, r.RuleID, r.PricingExprSnap, r.RuleMatchedAt,
		req.Streamed, req.APIKeyID, planID, quota.String(),
		// 调用方标识由入口原样带来（见 pipeline.Request.UserAgent）。
		req.UserAgent,
		// 最后一句是不是工具结果回填，由入口判定（见 pipeline.Request.ToolResultTail）。
		req.ToolResultTail).Scan(&id)
	if err != nil {
		return 0, constraintFailure(fmt.Errorf("写额度包预留记录失败: %w", err))
	}
	if req.Idempotency != nil {
		// 与钱包那条路在上层调用同一个函数：幂等记录与预留必须在同一个事务里，
		// 这里两条路各写一次不算重复——每条路只走其中一条。
		if err := idempotency.ClaimInTx(ctx, tx, req.Idempotency, id); err != nil {
			return 0, constraintFailure(err)
		}
	}
	return id, nil
}

// growPlanReservation 给一笔额度包覆盖的流式预留追加占用。
//
// 判断与初次选源同一组维度（planCovers）、同一把锁（订阅行）。不够时返回
// ErrInsufficientAvailable：调用方据此切断这条流，处置与「钱包里没钱了」相同。
func growPlanReservation(ctx context.Context, tx *sql.Tx, requestID string, planID int64, delta money.Amount, outputTokens int64) error {
	var (
		unit                                          string
		quotaTotalStr, carriedQuotaStr, quotaUsedStr  string
		pendingQuotaStr, pendingAmountStr, balanceStr string
	)
	// 锁与读分开，理由与 reserveFromPlan 同：同一条语句里等锁时，语句快照
	// 取在等锁之前，刚提交的在途行看不见；两条语句才能让第二笔在看到第一笔
	// 已提交的占用之后再判断。
	var locked int
	if err := tx.QueryRowContext(ctx, `
		SELECT 1 FROM user_plans WHERE id = $1 FOR UPDATE`, planID).Scan(&locked); err != nil {
		return fmt.Errorf("锁定额度包订阅 %d 失败: %w", planID, err)
	}

	err := tx.QueryRowContext(ctx, `
		SELECT p.unit, p.quota_total::text, up.carried_quota::text, up.quota_used::text,
		       COALESCE(pend.q, 0)::text,
		       COALESCE(pend.m, 0)::numeric(20, 8)::text,
		       COALESCE(bal.s, 0)::numeric(20, 8)::text
		  FROM user_plans up
		  JOIN plans p ON p.id = up.plan_id
		  LEFT JOIN LATERAL (
		      SELECT SUM(reserved_quota) AS q, SUM(plan_reserved_amount) AS m
		        FROM billing_requests
		       WHERE user_plan_id = up.id AND status = 'pending'
		  ) pend ON true
		  LEFT JOIN LATERAL (
		      SELECT SUM(amount) AS s
		        FROM ledger_entries
		       WHERE account = 'prepaid:' || up.id AND asset = p.asset
		  ) bal ON true
		 WHERE up.id = $1`, planID).Scan(
		&unit, &quotaTotalStr, &carriedQuotaStr, &quotaUsedStr,
		&pendingQuotaStr, &pendingAmountStr, &balanceStr)
	if err != nil {
		return fmt.Errorf("读额度包订阅 %d 的余额与在途预留失败: %w", planID, err)
	}
	// 在途量从 pend 的求和读出来（包含本行已经占着的那一部分），所以下面
	// 「可用量余多少」就是「还能不能再加这么多」的直接判据。

	quotaTotal, err := money.Parse(quotaTotalStr)
	if err != nil {
		return fmt.Errorf("额度包订阅 %d 的可用量读不出来: %w", planID, err)
	}
	quotaUsed, err := money.Parse(quotaUsedStr)
	if err != nil {
		return fmt.Errorf("额度包订阅 %d 的已用量读不出来: %w", planID, err)
	}
	carriedQuota, err := money.Parse(carriedQuotaStr)
	if err != nil {
		return fmt.Errorf("额度包订阅 %d 的结转量读不出来: %w", planID, err)
	}
	pendingQuota, err := money.Parse(pendingQuotaStr)
	if err != nil {
		return fmt.Errorf("额度包订阅 %d 的在途预留读不出来: %w", planID, err)
	}
	pendingAmount, err := money.Parse(pendingAmountStr)
	if err != nil {
		return fmt.Errorf("额度包订阅 %d 的在途面值读不出来: %w", planID, err)
	}
	balance, err := money.Parse(balanceStr)
	if err != nil {
		return fmt.Errorf("额度包订阅 %d 的面值余额读不出来: %w", planID, err)
	}

	var quotaDelta money.Amount
	if unit == "token" {
		quotaDelta, err = quotaFromCount(outputTokens)
		if err != nil {
			return err
		}
	}
	quotaAvail := planQuotaAvailable(quotaTotal, carriedQuota, quotaUsed, pendingQuota)
	faceAvail := balance.Sub(pendingAmount)
	if !planCovers(unit, delta, faceAvail, quotaDelta, quotaAvail) {
		return fmt.Errorf("%w: 额度包订阅 %d 的剩余量不够继续冻结这一段（面值余 %s，可用量余 %s）",
			ErrInsufficientAvailable, planID, faceAvail, quotaAvail)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE billing_requests
		   SET plan_reserved_amount = plan_reserved_amount + $2::numeric,
		       reserved_quota = reserved_quota + $3::numeric
		 WHERE request_id = $1`, requestID, delta.String(), quotaDelta.String()); err != nil {
		return constraintFailure(fmt.Errorf("追加额度包占用失败: %w", err))
	}
	return nil
}

// applyPlanQuota 在结算事务里记一次已消费的可用量。
//
// 它在插入分录**之前**调用，因为它要锁订阅行：作废读面值余额、结算写分录这两件
// 事必须串行，而订阅行是它们共同的串行点。放在分录之后的话，作废可能在两者
// 之间读到旧余额，把已经在途的额度一起核销掉，随后结算把它扣成负数。
//
// 计量的口径与选源那一侧**同一个方向**：预留按上界占，结算按实际改，超出预留的
// 部分截到预留额——与金额那条路的「结算不超过预留」是同一条规则（见
// billing.SettleWithCharges）。截掉意味着少记一点用量，而不是让包透支。
//
// 截断是**有意的取舍**（宁可少记、不透支），但它让「记下的用量」小于「实际
// 用量」，而这种偏差在账上完全看不出来。所以截断时留一条 WARN，带上订阅 id、
// 实际量与预留额——它是事后追查这类偏差的唯一线索。
func (s *Store) applyPlanQuota(ctx context.Context, tx *sql.Tx, planID int64, usage billing.Usage, reservedQuota money.Amount) error {
	var unit string
	err := tx.QueryRowContext(ctx, `
		SELECT p.unit
		  FROM user_plans up JOIN plans p ON p.id = up.plan_id
		 WHERE up.id = $1
		 FOR UPDATE OF up`, planID).Scan(&unit)
	if err != nil {
		return fmt.Errorf("结算时锁定额度包订阅 %d 失败: %w", planID, err)
	}

	var used money.Amount
	switch unit {
	case "token":
		used, err = quotaFromCount(usage.TotalTokens())
		if err != nil {
			return err
		}
	case "request":
		used, err = quotaFromCount(1)
		if err != nil {
			return err
		}
	default: // amount：量在面值那一侧，账本分录已经记了
		return nil
	}
	if used.Cmp(reservedQuota) > 0 {
		s.log().Warn("额度包的实际用量超过预留额，按预留额记账",
			"plan_id", planID,
			"actual", used.String(),
			"reserved", reservedQuota.String())
		used = reservedQuota
	}
	if used.IsZero() {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE user_plans SET quota_used = quota_used + $2::numeric WHERE id = $1`,
		planID, used.String()); err != nil {
		return constraintFailure(fmt.Errorf("记额度包已用量失败: %w", err))
	}
	return nil
}

// PlanQuotaUsage 读一份订阅按包自身总量算出的可用量余量：
// quota_total − quota_used − 在途预留。
//
// 它是期末结转那一侧要的**基数**，与请求路径共用同一个口径（见 plancharge.go
// 顶部「两个维度」那一节；两处的表达式本可以合成一个，但这里要的是可读的
// 余量而不是判断够不够，共用一个函数会让两边的 SQL 互相牵制）。不含
// carried_quota：领包时并进来的结转量不该再结转第二次，那个减数由调用方按
// 行上的 carried_quota 给。
//
// 可能为负（结算截断与预留之间有过不一致），调用方要在乘比例之前夹到 0。
// 这里不夹，是因为「负多少」在排查不一致时有用——夹掉会让它看起来只是「用完了」。
//
// 调用方要持有订阅行锁：它要读作废与结算都改的那几个数。
func PlanQuotaUsage(ctx context.Context, tx *sql.Tx, planID int64) (money.Amount, error) {
	var raw string
	err := tx.QueryRowContext(ctx, `
		SELECT (p.quota_total - up.quota_used - COALESCE(pend.q, 0))::numeric(20, 8)::text
		  FROM user_plans up
		  JOIN plans p ON p.id = up.plan_id
		  LEFT JOIN LATERAL (
		      SELECT SUM(reserved_quota) AS q
		        FROM billing_requests
		       WHERE user_plan_id = up.id AND status = 'pending'
		  ) pend ON true
		 WHERE up.id = $1`, planID).Scan(&raw)
	if err != nil {
		return money.Zero, fmt.Errorf("读额度包订阅 %d 的可用量余量失败: %w", planID, err)
	}
	amt, err := money.Parse(raw)
	if err != nil {
		return money.Zero, fmt.Errorf("额度包订阅 %d 的可用量余量读不出来（原文 %q）: %w", planID, raw, err)
	}
	return amt, nil
}

// PendingPlanReservation 读一个订阅**在途预留**占着的面值之和。
//
// 给作废那一侧用：作废要把「剩余面值」核销掉，而在途预留虽然还没写成分录，
// 那部分面值已经承诺给那几笔请求了。不减掉它们，作废会把同一份面值核销两次，
// 在途请求结算时 prepaid 科目变成负数——那正是「额度包透支」，而账本上
// 看不出来是哪一步错了。
func PendingPlanReservation(ctx context.Context, tx *sql.Tx, planID int64) (money.Amount, error) {
	var raw string
	err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(plan_reserved_amount), 0)::numeric(20, 8)::text
		  FROM billing_requests
		 WHERE user_plan_id = $1 AND status = 'pending'`, planID).Scan(&raw)
	if err != nil {
		return money.Zero, fmt.Errorf("读额度包订阅 %d 的在途预留失败: %w", planID, err)
	}
	amt, err := money.Parse(raw)
	if err != nil {
		return money.Zero, fmt.Errorf("额度包订阅 %d 的在途预留读不出来（原文 %q）: %w", planID, raw, err)
	}
	return amt, nil
}
