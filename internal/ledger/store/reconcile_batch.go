package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strconv"

	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/money"
)

// 本文件是对账的一批之内怎么做：读快照、读权威值、逐行比对、按口径修正。
//
// 与 reconcile.go 的分工：那边是「一轮怎么跑」（分片、起点、运行记录），
// 这边是「一批怎么比」。分开是因为两者的正确性关注点不同——前者关心
// 别漏读、别重跑，后者关心同一时刻的读与写在不在同一份数据上。

// userAsset 是一个钱包行的主键，也是账本汇总与预留汇总的键。
//
// 资产是键的一部分：同一个用户的不同币种余额是两件独立的事，混起来
// 比对会把 USDT 的金额算到 CNY 上，而两边都不报错。
type userAsset struct {
	userID int64
	asset  string
}

// walletSnapshot 是钱包行里参与比对的两位数字。
type walletSnapshot struct {
	balance money.Amount
	frozen  money.Amount
}

// reconcileUsers 比对一批用户并（在开关打开时）修正。
//
// # 为什么整批在一个事务里，且要先加锁
//
// 比对读的是「某一时刻的钱包 + 某一时刻的账本」。两者之间若有写入插进来，
// 就会比出一个**假**的不一致：账本刚多了一条分录而快照还没跟上，
// 那笔差额其实正在被另一个事务提交。在自动修正打开时，假不一致会让
// 对账去改一个本来正确的余额。
//
// 所以先按 store 的固定顺序锁住这批钱包行，再读、再改：锁之后读到的
// 值在本事务结束前不会变。锁的顺序必须与写入路径一致，否则对账与一次
// 结算并发时会互相等待成死锁。
func (s *Store) reconcileUsers(ctx context.Context, cfg ReconcileConfig, runID int64, ids []int64) ([]Finding, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	keys, err := walletKeysOfUsers(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	sortWalletKeys(keys)
	if lockErr := lockWallets(ctx, tx, keys...); lockErr != nil {
		return nil, lockErr
	}

	wallets, err := readWallets(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	ledgerSums, err := readLedgerSums(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	pending, err := readPendingReservations(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	planFaces, err := readPlanFaces(ctx, tx, ids)
	if err != nil {
		return nil, err
	}

	findings, err := compareAndRepair(ctx, tx, cfg, runID, wallets, ledgerSums, pending)
	if err != nil {
		return nil, err
	}
	// 额度包面值那一维不属于钱包的三份数据：它的两边都是别处的数
	// （prepaid 科目的分录之和、预留表上的 plan_reserved_amount），
	// 所以另起一段比，见 readPlanFaces 与 comparePlanFaces。
	findings = append(findings, comparePlanFaces(runID, planFaces)...)
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("提交对账修正失败: %w", err)
	}
	return findings, nil
}

// walletKeysOfUsers 取这批用户现有的钱包行主键，供加锁用。
//
// 加锁只锁**已存在的行**（见 lockWallets）：不存在的行由后续的
// 「影响 0 行」判据暴露出来，而那是更明确的错误。
func walletKeysOfUsers(ctx context.Context, tx *sql.Tx, ids []int64) ([]WalletKey, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT user_id, asset FROM user_wallets WHERE user_id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("查钱包行失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []WalletKey
	for rows.Next() {
		var k WalletKey
		if err := rows.Scan(&k.UserID, &k.Asset); err != nil {
			return nil, fmt.Errorf("读钱包行失败: %w", err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历钱包行失败: %w", err)
	}
	return out, nil
}

// readWallets 读这批用户的余额快照。
func readWallets(ctx context.Context, tx *sql.Tx, ids []int64) (map[userAsset]walletSnapshot, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT user_id, asset, balance::text, frozen::text
		  FROM user_wallets WHERE user_id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("读钱包快照失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[userAsset]walletSnapshot{}
	for rows.Next() {
		var (
			key            userAsset
			balance, froze string
		)
		if err := rows.Scan(&key.userID, &key.asset, &balance, &froze); err != nil {
			return nil, fmt.Errorf("读钱包快照失败: %w", err)
		}
		b, err := money.Parse(balance)
		if err != nil {
			return nil, fmt.Errorf("解析用户 %d 的余额 %q 失败: %w", key.userID, balance, err)
		}
		f, err := money.Parse(froze)
		if err != nil {
			return nil, fmt.Errorf("解析用户 %d 的冻结额 %q 失败: %w", key.userID, froze, err)
		}
		out[key] = walletSnapshot{balance: b, frozen: f}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历钱包快照失败: %w", err)
	}
	return out, nil
}

// readLedgerSums 按 user 科目汇总账本。
//
// 只查这批用户的科目：一次查询的**结果**因此限制在一个分片之内。
//
// # 为什么谓词不是 account = ANY(规范写法)
//
// 逐字相等只认得一种写法，而写路径当初是把后缀读成整数的：`user:0<id>`
// 那样的科目**是**算进该用户钱包的。等值匹配看不见它，汇总出来的权威值
// 就偏小，对账据此把**正确的**余额改小。所以这里与写路径用同一套口径归位：
// 后缀按十进制整数解释，前导零与正号只是同一个 id 的另一种写法。
//
// # 代价：每个分片一次全表扫描
//
// 归一化表达式不是索引条件，`ledger_account_idx` 在计划里只作为 Filter
// 出现（没有 Index Cond），实测计划就是 `Seq Scan on ledger_entries`；
// 把 enable_seqscan 关掉也还是它。于是查询的代价不再由这一批用户决定，
// 而由 ledger_entries 的**总行数**决定——而 reconcileAll 是按分片循环调用
// 本函数的，总代价因此是「分片数 × 表行数」，而不是从前的「本分片的行数」。
//
// 这个取舍是有意的：认不出旧写法会改错钱，扫全表只是慢。但要说清楚它
// 现在**慢在哪儿**，以及什么时候才能退回去——收窄 ParseAccount 只挡住了
// 新的写入，库里已经存在的别名行没有清理路径（畸形科目只报告不修，
// 见 reconcileMalformed），所以「清理干净之后可以退回等值匹配」这句话
// 目前**缺一个执行者**：得先有人把那些行订正掉，等值匹配才重新够用。
func readLedgerSums(ctx context.Context, tx *sql.Tx, ids []int64) (map[userAsset]money.Amount, error) {
	accounts := make([]string, 0, len(ids))
	for _, id := range ids {
		accounts = append(accounts, ledger.UserAccount(id).String())
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT `+userAccountCanonicalSQL+` AS account, asset, SUM(amount)::text
		  FROM ledger_entries
		 WHERE account ~ '`+userAccountAliasPattern+`'
		   AND `+userAccountCanonicalSQL+` = ANY($1)
		 GROUP BY 1, 2`, accounts)
	if err != nil {
		return nil, fmt.Errorf("汇总账本失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[userAsset]money.Amount{}
	for rows.Next() {
		var account, asset, total string
		if err := rows.Scan(&account, &asset, &total); err != nil {
			return nil, fmt.Errorf("读账本汇总失败: %w", err)
		}
		userID, ok := ledger.UserIDFromAccount(account)
		if !ok {
			// 归一到规范写法之后必然读得出用户 id。读不出说明 SQL 那边的
			// 规则与 ledger 这边不一致——那是 Bug，报出来而不是跳过。
			return nil, fmt.Errorf("账本汇总里的科目 %q 读不出用户 id", account)
		}
		amt, err := money.Parse(total)
		if err != nil {
			return nil, fmt.Errorf("解析账本汇总 %q 失败: %w", total, err)
		}
		out[userAsset{userID: userID, asset: asset}] = amt
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历账本汇总失败: %w", err)
	}
	return out, nil
}

// readPendingReservations 按用户与资产汇总未结算的预留金额。
//
// 它就是 frozen 的权威值：冻结不进账本，只在预留表上留痕。
// 用 status = 'pending' 而不是别的判据：冻结加上去的时刻正是那一行变
// pending 的时刻，解冻（结 settle 或 release）时它就不再是 pending。
func readPendingReservations(ctx context.Context, tx *sql.Tx, ids []int64) (map[userAsset]money.Amount, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT user_id, reserved_asset, SUM(reserved_amount)::text
		  FROM billing_requests
		 WHERE status = 'pending' AND user_id = ANY($1)
		 GROUP BY user_id, reserved_asset`, ids)
	if err != nil {
		return nil, fmt.Errorf("汇总未结算预留失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[userAsset]money.Amount{}
	for rows.Next() {
		var (
			key      userAsset
			reserved string
		)
		if err := rows.Scan(&key.userID, &key.asset, &reserved); err != nil {
			return nil, fmt.Errorf("读预留汇总失败: %w", err)
		}
		amt, err := money.Parse(reserved)
		if err != nil {
			return nil, fmt.Errorf("解析预留汇总 %q 失败: %w", reserved, err)
		}
		out[key] = amt
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历预留汇总失败: %w", err)
	}
	return out, nil
}

// planFaceRow 是一个订阅在某一时刻的两笔数：面值余额与在途预留。
//
// 两笔数都不在 user_wallets 上：余额是 `prepaid:<订阅 id>` 科目的分录之和，
// 在途预留是 `billing_requests.plan_reserved_amount` 的 pending 之和。
type planFaceRow struct {
	planID  int64
	userID  int64
	asset   string
	balance money.Amount
	pending money.Amount
}

// readPlanFaces 读这批用户名下每个额度包订阅的面值余额与在途预留之和。
//
// # 为什么两笔数必须在一条语句里取
//
// 迁移 00053 写下的不变量是「prepaid 科目余额 ≥ 在途预留之和」，而这两笔数
// **都随请求进进出出**：预留会加上一笔、结算会在同一个事务里写分录并把它
// 从 pending 移走。分两条语句读，两条之间夹一次提交，算出来的差就属于两个
// 时点——那是一条假的不一致，而按它处置（或只是告警）都会把人引到一个
// 不存在的问题上。
//
// 本函数因此是一条语句：两个 LATERAL 子查询共用该语句的快照。事务是默认的
// read committed，**每条语句各自取快照**，所以「同一个事务里分两次查」不足
// 以保证同源，必须是同一条语句。
//
// # 为什么从 user_plans 驱动，而不是从 pending 行驱动
//
// 不变量对每个订阅都成立，在途为零时它是「余额 ≥ 0」。从 pending 行驱动
// 只能看见有在途的那些订阅，余额被写成负数、而恰好没有在途的那个订阅就
// 漏掉了——那正是「只有一条写入路径绕过 store」最容易留下的形状。
//
// 代价是每个分片扫一遍 user_plans（`user_plans_user_idx` 只覆盖 active，
// 带 `user_id = ANY(...)` 的查询用不上它）。这个取舍与 readLedgerSums 的
// 全表扫描同类，但这里扫的是**订阅表**而不是分录表——它的行数由用户数
// 决定，不随请求量增长，所以代价停在「分片数 × 订阅数」这一档。
//
// # 余额按订阅的资产取
//
// `prepaid:<订阅 id>` 只记一种资产（发放时按 plans.asset），这里与选源那条
// 路径（plancharge.go 的 reserveFromPlan）用同一个口径：按 `p.asset` 过滤。
// 科目原文精确匹配即可：prepaid 的后缀只放行规范写法（见 ledger.ParseAccount
// 对 prepaid 的收窄），不存在 user 科目那样的历史别名。
//
// # 增量扫描怎么看见它
//
// 候选用户里有**订阅的 owner** 这一侧（reconcileCandidates 的 user_plans
// 分支），它没有时间窗：每一轮、每个分片都会把订阅表扫一遍。所以只要
// user_plans 里有这一行，它的 owner 就一定进候选，这一维每轮都被比过一次
// ——包括「结算之后 pending 变 settled、此后再没有任何行变动」的那种用户。
//
// 那一侧并的是 `user_plans.user_id`，不是 `prepaid:` 科目：科目后缀是订阅 id，
// 并进候选集还得换一次 id；按 owner 进来一次，本函数随后把他名下**所有**订阅
// 逐条比完（不论 status）。
//
// 剩下的盲区只有一个形状：`prepaid:<id>` 的分录在、而 user_plans 里没有这一
// 行。本函数从 user_plans 驱动，看不见它；它也不在钱包那三类判据里（科目后缀
// 不是用户 id），今天没有判据报它。
//
// 剩下的是「从没报过、两条数也不再变动」的那种——它与本文件顶部说的增量盲区
// 是同一类，由定期全量扫描兜底。正常写入路径造不出它：预留受 faceAvail 约束、
// 结算按预留上界截断、作废只核销「余额 − 在途」，三者都不会让在途超过余额。
func readPlanFaces(ctx context.Context, tx *sql.Tx, ids []int64) ([]planFaceRow, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT up.id, up.user_id, p.asset,
		       COALESCE(bal.s, 0)::numeric(20, 8)::text,
		       COALESCE(pend.m, 0)::numeric(20, 8)::text
		  FROM user_plans up
		  JOIN plans p ON p.id = up.plan_id
		  LEFT JOIN LATERAL (
		      SELECT SUM(amount) AS s
		        FROM ledger_entries
		       WHERE account = 'prepaid:' || up.id AND asset = p.asset
		  ) bal ON true
		  LEFT JOIN LATERAL (
		      SELECT SUM(plan_reserved_amount) AS m
		        FROM billing_requests
		       WHERE user_plan_id = up.id AND status = 'pending'
		  ) pend ON true
		 WHERE up.user_id = ANY($1)
		 ORDER BY up.id`, ids)
	if err != nil {
		return nil, fmt.Errorf("读额度包面值与在途预留失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []planFaceRow
	for rows.Next() {
		var (
			row              planFaceRow
			balance, pending string
		)
		if err := rows.Scan(&row.planID, &row.userID, &row.asset, &balance, &pending); err != nil {
			return nil, fmt.Errorf("读额度包面值与在途预留失败: %w", err)
		}
		b, err := money.Parse(balance)
		if err != nil {
			return nil, fmt.Errorf("额度包订阅 %d 的面值余额读不出来（原文 %q）: %w", row.planID, balance, err)
		}
		p, err := money.Parse(pending)
		if err != nil {
			return nil, fmt.Errorf("额度包订阅 %d 的在途预留读不出来（原文 %q）: %w", row.planID, pending, err)
		}
		row.balance, row.pending = b, p
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历额度包面值与在途预留失败: %w", err)
	}
	return out, nil
}

// comparePlanFaces 比对每个订阅的面值与在途预留，报出「在途超过余额」的那些。
//
// # 判据是严格大于
//
// 相等**不算**不一致：作废一条还有在途预留的订阅时，核销的是「余额 − 在途」，
// 剩下的余额恰好等于在途之和。那一格是合法状态，写成 `>=` 会让每次作废都
// 报一条出来。
//
// # 只报不修
//
// 差额的两边都不能由对账来改：补面值等于凭空记一笔平台欠用户的额度；减在途
// 等于替一笔可能还在途的请求决定它该占多少面值。前者是造钱，后者与
// staleReservationAfter 那条「只报不自动释放」同一个理由。所以这一类不进
// repairPlanFace 之类的分支，Unrepairable 写明原因。
//
// 顺序由查询的 `ORDER BY up.id` 定：map 或并集那种随机顺序让同一份数据两次
// 跑出不同的报告顺序，测试断言不了，人也没法对比两次的结果。
func comparePlanFaces(runID int64, plans []planFaceRow) []Finding {
	var findings []Finding
	for _, p := range plans {
		if p.pending.Cmp(p.balance) <= 0 {
			continue
		}
		findings = append(findings, Finding{
			RunID: runID, UserID: p.userID, Asset: p.asset,
			Kind: FindingPlanFaceShortfall,
			// 两列的取值见 Finding 上这两个字段的注释：Snapshot 是
			// prepaid 科目的余额，Ledger 是在途预留之和，Diff 因此是缺口
			// （正数），不是「账本减快照」那个方向的差额。
			Snapshot:     p.balance,
			Ledger:       p.pending,
			Key:          strconv.FormatInt(p.planID, 10),
			Unrepairable: "在途预留超过了订阅余额，补面值等于凭空记一笔平台欠款，对账不代平台补",
			Detail: fmt.Sprintf("额度包订阅 %d 的面值余额 %s 小于在途预留之和 %s",
				p.planID, p.balance, p.pending),
		})
	}
	return findings
}

// compareAndRepair 逐行比对，并按配置修正。
//
// 遍历顺序按 (user_id, asset) 排定：map 的随机顺序会让同一份数据两次跑出
// 不同的报告顺序，测试断言不了，人也没法对比两次的结果。
//
// # 只有钱包行里有的那些键也要比
//
// 三份数据的键集合不一定相同，而**每一个**方向上的差别都是一条不一致：
//
//   - 只有钱包、没有账本分录：余额不可能凭空存在（除非是直改快照），
//     比出来就是一条「账本为 0」的差额。
//   - 只有账本、没有钱包：见 FindingWalletMissing。
//   - 只有预留、没有钱包：同样是缺钱包行，冻结额无处安放。
func compareAndRepair(
	ctx context.Context,
	tx *sql.Tx,
	cfg ReconcileConfig,
	runID int64,
	wallets map[userAsset]walletSnapshot,
	ledgerSums map[userAsset]money.Amount,
	pending map[userAsset]money.Amount,
) ([]Finding, error) {
	var findings []Finding

	for _, key := range unionKeys(wallets, ledgerSums, pending) {
		ledgerSum := ledgerSums[key]
		pendingSum := pending[key]
		snapshot, hasWallet := wallets[key]

		if !hasWallet {
			// 有分录或有预留，却没有钱包行。修它要新建一行钱包，而那需要
			// 知道用户是否存在、默认用哪种资产——都不该由对账任务来猜。
			// 报出来，让人去查这一行为什么不在（开户当前由
			// `larkaxctl wallet create` 做，见 internal/ledger/store/wallet.go；
			// 注册流程不建钱包行，所以别去那里找漏掉的一步）。
			//
			// Detail 要把**是哪一侧有钱**写清楚：只有分录、只有预留、
			// 还是两者都有（金额为 0 的那一侧就是没有的那一侧）。三种情况
			// 要查的地方不同（分录写错用户 id、预留写了但钱包被删、
			// 压根没开过户），而只报一句「缺钱包行」会让人从三处里挨个试。
			//
			// 两侧的金额都要写出来，**包括为 0 的那一侧**。这一行落库之后
			// 只有 snapshot 与 ledger 两个数字列，而 ledger 列在这一支里
			// 只表示分录之和（见 internal/migrate/sql/00004_reconcile.sql 的
			// 列注释）。文字里只提预留、不提分录之和时，读的人对着 ledger
			// 列的 0 分不清那笔预留算没算进去——文字说的数在列里找不到，
			// 正是这条缺陷的形状。所以预留那一侧要标明它不在 ledger 列里。
			f := Finding{
				RunID: runID, UserID: key.userID, Asset: key.asset,
				Kind: FindingWalletMissing, Ledger: ledgerSum,
				Unrepairable: "没有钱包行，对账不负责新建",
				Detail: fmt.Sprintf(
					"账本分录之和 %s（即 ledger 列）、未结算预留之和 %s（冻结侧，不在 ledger 列），钱包行不存在",
					ledgerSum, pendingSum),
			}
			findings = append(findings, f)
			continue
		}

		if ledgerSum.Cmp(snapshot.balance) != 0 {
			f := Finding{
				RunID: runID, UserID: key.userID, Asset: key.asset,
				Kind: FindingBalanceMismatch, Snapshot: snapshot.balance, Ledger: ledgerSum,
			}
			if ledgerSum.IsNegative() {
				// 账本值为负说明这个用户欠平台钱。它在两种口径下都不该被
				// 自动抹平：钱包的 CHECK 不允许负余额，而「用户欠钱」只能由
				// 人去处置（补扣、冻结账号）。夹到零等于平台悄悄认下差额，
				// 而那笔钱从此在任何报表上都看不见。
				f.Unrepairable = "账本值为负，钱包不允许负余额"
			}
			if err := repairBalance(ctx, tx, cfg, &f, key); err != nil {
				return nil, err
			}
			findings = append(findings, f)
		}

		if pendingSum.Cmp(snapshot.frozen) != 0 {
			f := Finding{
				RunID: runID, UserID: key.userID, Asset: key.asset,
				Kind: FindingFrozenMismatch, Snapshot: snapshot.frozen, Ledger: pendingSum,
			}
			if pendingSum.IsNegative() {
				// 预留金额那一列上**没有** CHECK（见 00001_init.sql 的
				// billing_requests.reserved_amount），所以负的和写得进去：
				// 它可能来自数据被直接改过，也可能来自写路径算错。
				// 修正它等于把冻结额设成一个负数，那既会让
				// 「可用 = 余额 − 冻结」大于余额，也会撞上钱包那一侧的
				// wallet_non_negative，把整轮对账带成失败。
				f.Unrepairable = "未结算预留之和为负"
			}
			if err := repairFrozen(ctx, tx, cfg, &f, key); err != nil {
				return nil, err
			}
			findings = append(findings, f)
		}
	}
	return findings, nil
}

// repairBalance 按修正口径把余额修正到权威值。
func repairBalance(ctx context.Context, tx *sql.Tx, cfg ReconcileConfig, f *Finding, key userAsset) error {
	if !cfg.AutoRepair || f.Unrepairable != "" {
		return nil
	}

	switch cfg.RepairMode {
	case RepairResyncSnapshot:
		// 账本值就是目标值。不写分录：账本没记错，偏离的是快照，修正它
		// 只是重算一个派生值（理由见 reconcile.go 的文件注释）。
		if err := resyncBalance(ctx, tx, key, f.Ledger); err != nil {
			return err
		}
		f.Repaired = true

	case RepairAdjustLedger:
		// 快照值就是目标值：写一条分录把账本改到快照的数值。
		//
		// delta 是「快照 − 账本」：账本要多出这么多才等于快照。它与
		// Finding.Diff()（账本 − 快照）符号相反，两处都写成显式取负，
		// 而不是靠调用点记住哪边减哪边。
		delta := f.Diff().Neg()
		adj, err := ledger.Adjustment(ledger.AdjustmentParams{
			TxnID:          ledger.NewTxnID(),
			Account:        ledger.UserAccount(key.userID),
			Amount:         delta,
			Asset:          ledger.Asset(key.asset),
			CounterAccount: cfg.DifferenceAccount,
			Memo:           fmt.Sprintf("对账修正：快照 %s 与账本 %s 不一致", f.Snapshot, f.Ledger),
		})
		if err != nil {
			return fmt.Errorf("构造对账调账分录失败: %w", err)
		}
		if err := insertEntries(ctx, tx, adj.Entries); err != nil {
			return err
		}
		f.Repaired = true
	}
	return nil
}

// resyncBalance 把快照余额改成账本值。
func resyncBalance(ctx context.Context, tx *sql.Tx, key userAsset, target money.Amount) error {
	res, err := tx.ExecContext(ctx, `
		UPDATE user_wallets
		   SET balance = $3::numeric, updated_at = now()
		 WHERE user_id = $1 AND asset = $2`,
		key.userID, key.asset, target.String())
	if err != nil {
		return constraintFailure(fmt.Errorf("修正余额失败: %w", err))
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("读取影响行数失败: %w", err)
	}
	if affected == 0 {
		// 钱包行在加锁之后消失了——那是加锁顺序之外的一条写入路径。
		// 报出来而不是当作成功：当作成功会让修正看起来做了而其实没有。
		return fmt.Errorf("修正余额时钱包行不见了: 用户 %d 资产 %s", key.userID, key.asset)
	}
	return nil
}

// repairFrozen 把冻结额修正到未结算预留之和。
//
// 冻结不参与口径的选择：它的权威值只有一个（预留表），修正它也不涉及
// 任何资金流动，所以两种口径下的处理相同。
func repairFrozen(ctx context.Context, tx *sql.Tx, cfg ReconcileConfig, f *Finding, key userAsset) error {
	if !cfg.AutoRepair || f.Unrepairable != "" {
		return nil
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE user_wallets
		   SET frozen = $3::numeric, updated_at = now()
		 WHERE user_id = $1 AND asset = $2`,
		key.userID, key.asset, f.Ledger.String())
	if err != nil {
		return constraintFailure(fmt.Errorf("修正冻结额失败: %w", err))
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("读取影响行数失败: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("修正冻结额时钱包行不见了: 用户 %d 资产 %s", key.userID, key.asset)
	}
	f.Repaired = true
	return nil
}

// unionKeys 返回三份数据涉及的全部键，按 (user_id, asset) 排序。
//
// 三份数据的值类型不同，这里逐个写明而不是用 any 收：调用点看不出
// any 里装的是什么，而弄错一份就是漏比一类不一致。
func unionKeys(
	wallets map[userAsset]walletSnapshot,
	ledgerSums map[userAsset]money.Amount,
	pending map[userAsset]money.Amount,
) []userAsset {
	seen := map[userAsset]struct{}{}
	for k := range wallets {
		seen[k] = struct{}{}
	}
	for k := range ledgerSums {
		seen[k] = struct{}{}
	}
	for k := range pending {
		seen[k] = struct{}{}
	}

	out := make([]userAsset, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].userID != out[j].userID {
			return out[i].userID < out[j].userID
		}
		return out[i].asset < out[j].asset
	})
	return out
}
