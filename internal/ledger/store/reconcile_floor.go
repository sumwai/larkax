package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/money"
)

// 本文件是对账的「科目余额下界」这一类检查。
//
// 前几类判据问的都是「同一个数的两处记录对不对得上」（钱包快照与账本、
// 冻结额与在途预留），或者「一条不等式成不成立」（在途预留不超过订阅面值）。
// 这一类问的是第三种问题：**某个科目的余额有没有掉到不该到的位置**。
//
// # 为什么做成一张表
//
// 每一类科目该不该有下界、下界是多少，是逐类别的业务判断。写成
// `if account.Type() == ledger.AccountCarryover { ... }` 这样的分支，第二条
// 规则进来时就得改判定流程，而改判定流程会动到第一类的行为。做成表之后，
// 加一类只加一行（见 accountFloors），判定流程一个字都不用改。
//
// 表里暂时只有 `carryover:<订阅 id>` 一行：负数意味着并出去的比结转进来的多，
// 那笔钱是凭空多出来的，只可能来自某条写入路径扣多了。别的科目要么本来就可能
// 是负的（platform_revenue 退款为负、payment_gateway 充值时持有的是负的），
// 要么该由「两边对不对得上」那一类去查，不在这里加。

// accountFloor 是一条「科目余额下界」规则。
type accountFloor struct {
	// Type 是科目大类（见 ledger.AccountType）。规则按大类生效，不按单个科目：
	// 同一个大类的每个科目（每个订阅、每个通道）共用一条下界。
	Type ledger.AccountType
	// Floor 是下界，**含**：余额严格小于它才报。
	Floor money.Amount
	// Basis 是这条下界的依据，写进 Detail 给处置的人看。它回答的是「为什么
	// 这个大类不能低到这个位置」，不是「为什么对账不自动修」（那在
	// floorUnrepairable 里）。
	Basis string
}

// accountFloors 是全部下界规则。加一类科目只在这里加一行。
var accountFloors = []accountFloor{
	{
		Type:  ledger.AccountCarryover,
		Floor: money.Zero,
		Basis: "负数是并出去的比结转进来的多，等于凭空造了一份额度，只可能来自某条写入路径多扣了",
	},
}

// floorUnrepairable 说明这一类为什么只报告、不自动修。
//
// 与 FindingPlanFaceShortfall、FindingStaleReservation 相同的取舍：把余额
// 补回下界要写一笔没有来路的额度（对平台记账来说就是凭空认下一笔欠款），
// 而负余额本身只说明「某处扣多了」，扣多在哪里得由人先查出来。
const floorUnrepairable = "只报告不修：补足余额等于凭空记一笔平台欠款，先查清是哪条写入路径扣多了"

// accountFloorOf 返回某个科目大类对应的下界规则。表里没有的大类返回 false。
func accountFloorOf(t ledger.AccountType) (accountFloor, bool) {
	for _, f := range accountFloors {
		if f.Type == t {
			return f, true
		}
	}
	return accountFloor{}, false
}

// floorAccountTypes 返回表里全部大类，供 SQL 按科目前缀筛行。
func floorAccountTypes() []string {
	out := make([]string, 0, len(accountFloors))
	for _, f := range accountFloors {
		out = append(out, string(f.Type))
	}
	return out
}

// reconcileAccountFloors 找出余额低于所属大类下界的科目。
//
// 判据只有一条：把一个科目的全部分录按 (科目, 资产) 加起来，余额严格小于
// 表里给它的下界就报。余额恰好等于下界不算——0 是「结转的额度已经全部并出去、
// 一分不剩」，那是正常状态，报出来只会让人开始怀疑整张清单。
//
// # 它为什么不参与分片
//
// 判据只看那一个科目的分录，与用户钱包无关，所以不进按 user_id 分片的那条循环，
// 与畸形科目、陈旧预留一样一次查完。下界挂在科目大类上也是这个原因：
// `carryover:<订阅 id>` 的后缀是订阅 id，归不到任何人的钱包。
//
// # 增量怎么既不漏掉，也不会报一次就安静下来
//
// 账本只追加（见 00001_init.sql 对 ledger_entries 的注释），所以一个科目的余额
// 掉到下界以下，一定有至少一条窗口之后写下的分录；按 `created_at >= watermark`
// 挑出候选科目，再把这些科目的**全部分录**加起来（不是只加窗口内那些，否则
// 算出来的是这一段的增量，不是余额），余额不足的就会在这一轮里看到。
//
// 另一个方向是「报出来之后没人处理」：那样的负数自己不会再变动，增量看不见它，
// 下一轮就不再报——而它仍然挂着。所以已记未消除的那些科目要一起并进候选
// （open 里那一段），理由与 reconcileMalformed 完全相同。
//
// 查询的写法：候选科目先在 CTE 里算出来，再按科目把它们的分录整体求和。两层
// 都用得上索引——内层走 created_at 或 account，外层按 account 取。
func (s *Store) reconcileAccountFloors(ctx context.Context, report *Report, alert Alert, open []openFinding) error {
	types := floorAccountTypes()

	var carried []string
	for _, f := range open {
		if f.kind == FindingAccountBelowFloor {
			carried = append(carried, strings.TrimPrefix(f.scope, floorScopePrefix))
		}
	}

	const fullQuery = `
		SELECT account, asset, SUM(amount)::text
		  FROM ledger_entries
		 WHERE split_part(account, ':', 1) = ANY($1)
		 GROUP BY account, asset
		 ORDER BY account, asset`
	const incrementalQuery = `
		WITH changed AS (
			SELECT DISTINCT account
			  FROM ledger_entries
			 WHERE (created_at >= $1 OR account = ANY($2))
			   AND split_part(account, ':', 1) = ANY($3)
		)
		SELECT e.account, e.asset, SUM(e.amount)::text
		  FROM ledger_entries e
		 WHERE e.account IN (SELECT account FROM changed)
		 GROUP BY e.account, e.asset
		 ORDER BY e.account, e.asset`

	query, args := incrementalQuery, []any{report.Watermark, carried, types}
	if report.FullScan {
		query, args = fullQuery, []any{types}
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("查科目余额下界失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var account, asset, total string
		if err := rows.Scan(&account, &asset, &total); err != nil {
			return fmt.Errorf("读科目余额失败: %w", err)
		}
		// SQL 是按 floorAccountTypes() 筛的，所以这里读不出规则就说明两边的
		// 规则不一致。报出来，而不是跳过：跳过的表现是「某个科目明明越界了
		// 却没人报」，而它与「这个科目一切正常」在输出里长得一模一样。
		kind, _, _ := strings.Cut(account, ":")
		floor, ok := accountFloorOf(ledger.AccountType(kind))
		if !ok {
			return fmt.Errorf("下界检查读到了表外的科目大类 %q（科目 %q）", kind, account)
		}
		amt, err := money.Parse(total)
		if err != nil {
			return fmt.Errorf("解析科目 %q 的余额 %q 失败: %w", account, total, err)
		}
		if amt.Cmp(floor.Floor) >= 0 {
			continue
		}

		f := Finding{
			RunID: report.RunID, Asset: asset,
			Kind: FindingAccountBelowFloor,
			// 两列的取值见 Finding 上这两个字段的注释：Snapshot 是这一类
			// 里那条不等式的下界，Ledger 是科目的账本余额。它们不是「同一笔
			// 钱的两种记录」，Diff 因此是「比下界低了多少」，不是差额。
			Snapshot: floor.Floor,
			Ledger:   amt,
			// 标识用科目原文：一个科目一条，两条不同的科目各报各的。
			Key:          account,
			Unrepairable: floorUnrepairable,
			Detail: fmt.Sprintf("科目 %s（资产 %s）的余额 %s，低于 %s 大类的下界 %s：%s",
				account, asset, amt, floor.Type, floor.Floor, floor.Basis),
		}
		report.Findings = append(report.Findings, f)
		if alert != nil {
			alert(ctx, f)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("遍历科目余额失败: %w", err)
	}
	return nil
}
