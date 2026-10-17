package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/money"
)

// 本文件是账本的**只读**一侧：余额查询与完整性校验。
//
// 它一行都不写：余额从分录现算（不读也不改 user_wallets 那个快照），
// 校验也只是把分录读回来看。差额一律只报出来，绝不补一笔调平分录
// ——补一笔会让账面继续错，只是把「不平」换成「一笔来路不明的分录」。
//
// # 读路径做多少校验
//
// 只做**类型**上的转换（text → Account / money.Amount），不做业务判据：
// 「类型认不认识」「方向对不对」「和是不是零」全部由 internal/ledger 判
// （见 ledger.VerifyEntries），因为同一份判据要同时用在「内存里构造的」
// 与「库里读回的」两条路径上，抄一份到这里就是两份会漂移的规则。
//
// 唯一的例外是科目：它是不透明类型，只能经 ledger.ParseAccount 产生，
// 所以那一处必须在这里解析（见 scanEntry）。

// accountCanonicalAnySQL 把任何科目的原文归一到账本认的写法。
//
// user 科目走 userAccountCanonicalSQL（旧写法的前导零与正号归回同一个 id），
// 其余科目原样返回——只有 user 科目有别名写法，也只有它的字符串参与
// 「这笔钱算给谁」的判断（见 reconcile.go 里那三个常量的说明）。
//
// 归一必须在**读的入口**做，不能让调用方各自处理：余额是按科目字符串归组的，
// 逐字相等看不见 `user:042`，汇总值就偏小——而偏小的方向恰好是「看起来少扣了
// 钱」，那是最难查的一类。这与对账的 readLedgerSums 是同一条口径。
const accountCanonicalAnySQL = `CASE WHEN account ~ '` + userAccountAliasPattern +
	`' THEN ` + userAccountCanonicalSQL + ` ELSE account END`

// entryColumns 是读一条分录要选的列，顺序与 scanEntry 的 Scan 一致。
//
// 科目的原文与归一化后的写法都要：后者是账本认的写法（余额按它归组），
// 前者是库里实际存着的东西——报错时要能照着它去库里查那一行。
const entryColumns = `txn_id::text, ` + accountCanonicalAnySQL + ` AS account,` +
	` account AS raw_account, asset, amount::text, entry_type,` +
	` billing_request_id, payout_id, memo`

// readScope 描述一次分录读取的范围。零值表示全库、全部资产、全部时间。
type readScope struct {
	// account 为 nil 表示不限科目。
	account *ledger.Account
	// asset 为空表示不限资产。
	asset ledger.Asset
	// from 是起点（含），零值表示不限。
	from time.Time
	// to 是终点（**不含**），零值表示不限。
	to time.Time
}

// where 拼出谓词与参数。
//
// 参数用 $n 现编而不是写死：谓词有几条取决于调用方给了哪些条件，写死编号
// 就得让每个调用点自己数到第几个——数错的表现是**参数错位**，而错位的谓词
// 照样查得出结果来，只是查的是别人的账。
func (sc readScope) where() (string, []any) {
	var (
		conds []string
		args  []any
	)
	param := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}

	if sc.account != nil {
		p := param(sc.account.String())
		if sc.account.Type() == ledger.AccountUser {
			// user 科目按归一化后的写法匹配，理由见 accountCanonicalAnySQL。
			conds = append(conds, `account ~ '`+userAccountAliasPattern+`' AND `+
				userAccountCanonicalSQL+` = `+p)
		} else {
			conds = append(conds, "account = "+p)
		}
	}
	if sc.asset != "" {
		conds = append(conds, "asset = "+param(string(sc.asset)))
	}
	if !sc.from.IsZero() {
		conds = append(conds, "created_at >= "+param(sc.from))
	}
	if !sc.to.IsZero() {
		conds = append(conds, "created_at < "+param(sc.to))
	}

	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// readEntries 按范围读回分录。
//
// 顺序不定（不加 ORDER BY）：调用它的两条路径都是**求和**，与顺序无关，
// 而为了一次求和把整段结果排一遍是白花的代价。要按交易分组的地方自己
// 拼查询（见 verifyQuery）。
//
// 科目读不出来的行**不算错误**，而是收进 unreadable 交回调用方：读的人
// 该拿到「还有几行归不到任何科目上」，而不是一个干净的数字（见
// AccountBalancesReport.Unreadable）。别的读错（连接断了、numeric 不是数字）
// 仍然直接返回。
func (s *Store) readEntries(ctx context.Context, sc readScope) ([]ledger.Entry, []string, error) {
	where, args := sc.where()
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+entryColumns+` FROM ledger_entries`+where, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("读分录失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var (
		out        []ledger.Entry
		unreadable []string
	)
	for rows.Next() {
		e, raw, err := scanEntry(rows)
		if err != nil {
			if !errors.Is(err, errUnreadableAccount) {
				return nil, nil, err
			}
			unreadable = append(unreadable, raw)
			continue
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("遍历分录失败: %w", err)
	}
	return out, unreadable, nil
}

// errUnreadableAccount 表示这一行的科目读不出来。
//
// 单独一个哨兵是因为两条路径对它的处置不同：**汇总**可以少算并把它说出来
// （见 AccountBalancesReport.Unreadable），而**校验**不行——校验的对象是
// 整笔交易，少一行就可能变成另一笔（甚至恰好是平的），所以那里必须停下报错。
var errUnreadableAccount = errors.New("科目读不出来")

// scanEntry 把一行读成一条分录，并把这一行科目的**原文**一并返回。
//
// 原文要带出来是因为报错与「没计进汇总」都要能照着它去库里查那一行；
// 而科目读不出来时返回的就是那个读不出来的原文。
func scanEntry(rows *sql.Rows) (ledger.Entry, string, error) {
	var (
		txnID, account, rawAccount string
		asset, amount, entryType   string
		billingRequestID, payoutID sql.NullInt64
		memo                       sql.NullString
	)
	if err := rows.Scan(&txnID, &account, &rawAccount, &asset, &amount, &entryType,
		&billingRequestID, &payoutID, &memo); err != nil {
		return ledger.Entry{}, "", fmt.Errorf("读分录失败: %w", err)
	}

	parsed, err := ledger.ParseAccount(account)
	if err != nil {
		// 不跳过、也不当成空科目：这两种处置都会让余额少算一笔而没有线索。
		// 读不出来的科目是**已经**存在的问题（旧写路径留下的写法、人工修数），
		// 它有自己的报告出口——对账的畸形科目清单（见 reconcileMalformed）。
		return ledger.Entry{}, rawAccount, fmt.Errorf(
			"%w: 交易 %s 的分录（库里写的是 %q）: %w；这类行由对账的畸形科目报告负责，先跑 larkaxctl reconcile",
			errUnreadableAccount, txnID, rawAccount, err)
	}
	amt, err := money.Parse(amount)
	if err != nil {
		return ledger.Entry{}, rawAccount, fmt.Errorf("交易 %s 的分录金额读不出来: %w", txnID, err)
	}

	e := ledger.Entry{
		TxnID:     ledger.TxnID(txnID),
		Account:   parsed,
		Asset:     ledger.Asset(asset),
		Amount:    amt,
		EntryType: ledger.EntryType(entryType),
		Memo:      memo.String,
	}
	if billingRequestID.Valid {
		id := billingRequestID.Int64
		e.BillingRequestID = &id
	}
	if payoutID.Valid {
		id := payoutID.Int64
		e.PayoutID = &id
	}
	return e, rawAccount, nil
}

// AccountBalance 读一个科目在一种资产上的余额。
//
// # 这是权威值，不是快照
//
// 它从 ledger_entries 现算，而不是读 user_wallets.balance。两者不一致时以
// 账本为准（见包注释「权威与快照」），所以「这个账户对不对」要比的正是
// 这两个数：这里给账本那一侧，快照那一侧在 user_wallets 里，比对由
// larkaxctl reconcile 负责（它还会把差额归集到 reconcile_difference）。
//
// # 代价随分录数增长
//
// 这不是一次 O(1) 查询：谓词落在 account 上（user 科目还要按归一化后的写法
// 匹配，见 readScope.where），命中的行会**逐条读回内存**再交给
// ledger.BalanceOf 相加。一个高频用户的分录会有很多条，查询因此随账龄变慢。
//
// 归一化那个谓词**用不上索引**，这是有意的：实测（5 万行的表、目标科目 50 行）
// 归一化走 Seq Scan、117ms，逐字相等走 ledger_account_idx、0.1ms。三个数量级
// 换来的是「旧写法的行也算得进来」——认不出它们会让余额偏小，而偏小看起来像
// 「少扣了钱」，那是最难查的一类。与对账的 readLedgerSums 是同一个取舍。
// 要提速的正确方向是给归一化写法建表达式索引（或物化），不是退回逐字相等。
//
// 这一轮**故意不做**缓存与物化视图：先把「余额怎么算」保持成只有一处实现
// （ledger.BalanceOf）。将来要提速时改的是这一处的取数方式，而不是把判据
// 再抄一份到 SQL 里——两份判据迟早不一致，而不一致的表现是两个地方报出
// 两个余额，且都没有报错。
//
// # 求和超出 money 的表示范围时报错
//
// 库里的金额列是 numeric(20, 8)，比 money 的 int64 宽，所以「逐条读回来的和」
// 真的可能放不进 int64。那时报错而不是给一个夹住的数：夹住的值不等于真实和
// （正向溢出时比真实和小），而余额偏小看起来正是「少扣了钱」（见
// ledger.BalanceOf）。
func (s *Store) AccountBalance(ctx context.Context, acct ledger.Account, asset ledger.Asset) (money.Amount, error) {
	entries, unreadable, err := s.readEntries(ctx, readScope{account: &acct, asset: asset})
	if err != nil {
		return money.Zero, err
	}
	if len(unreadable) > 0 {
		// 谓词是按科目匹配的，命中却读不出来说明「归一化」与「ParseAccount」
		// 两处的规则漂移了。宁可报错也不给一个少算了这些行的数——一个偏小的
		// 余额看起来像「少扣了钱」，那是最难查的一类。
		return money.Zero, fmt.Errorf("科目 %s 的余额读不出来：有 %d 行科目认不出来（%s）",
			acct, len(unreadable), strings.Join(unreadable, "、"))
	}
	return ledger.BalanceOf(entries, acct, asset)
}

// AccountBalancesReport 是一次全库汇总的结果。
type AccountBalancesReport struct {
	// ByAccount 是每个科目、每种资产各自的余额，与 ledger.Balances 的返回同形。
	ByAccount map[ledger.Asset]map[ledger.Account]money.Amount
	// Unreadable 是科目读不出来、**没有**计进汇总的那些行的科目原文（去重、排序）。
	//
	// 它们不是被静默丢掉的：调用方必须把它们说出来。一个「少了几行」的汇总
	// 如果不吭声，与一个完整的汇总长得一模一样（LESSONS 第 16 条），而这几行
	// 恰恰是账本里最该被看见的东西——钱在库里，却归不到任何科目上。
	Unreadable []string
}

// AccountBalances 汇总全库在一种资产上的余额，按科目分组。
//
// 用途是账本总览：平台收入、上游成本、应付商户、通道余额、挂着的对账差额
// 各是多少。逐项与快照比对不在这里，见 AccountBalance。
//
// # 读不出来的行不算错误，但要带回去
//
// 这是它与 AccountBalance 的一处**有意**不同：一条读不出来的科目不该让整份
// 汇总打不开——而账本脏的时候正是最需要看一眼总览的时候。代价是汇总可能少算，
// 所以那些行的原文一定要跟着结果回去（见 AccountBalancesReport.Unreadable）。
//
// # 代价：整表读进内存
//
// 它把这种资产的**全部**分录读回内存再交给 ledger.Balances 归组，所以内存
// 与耗时都随分录总数增长，比 AccountBalance 更重。理由与那一处相同：判据
// 只有一处实现。真到了撑不住的时候，要改的是取数方式（例如在库里按科目
// 分组求和），而不是在这里另写一套算法。
func (s *Store) AccountBalances(ctx context.Context, asset ledger.Asset) (*AccountBalancesReport, error) {
	entries, unreadable, err := s.readEntries(ctx, readScope{asset: asset})
	if err != nil {
		return nil, err
	}
	byAccount, err := ledger.Balances(entries)
	if err != nil {
		// 某个科目的和超出 money 能表示的范围时整份汇总都算不出来。与读不出来的
		// 科目不同（见 AccountBalancesReport.Unreadable）：那不是这一份汇总里的
		// 一个洞，而是**这一列的数**已经放不进金额类型了，少算一点交出去会让
		// 「平台收入 = 某个数」看起来像一次成功的汇总。
		return nil, err
	}
	return &AccountBalancesReport{
		ByAccount:  byAccount,
		Unreadable: distinctSorted(unreadable),
	}, nil
}

// distinctSorted 去重并排序。
//
// 排序是为了让同一份库两次跑出来的清单逐行相同（map 的顺序是随机的，
// 而这里的顺序来自查询，同样不保证）；去重是因为同一行的原文重复出现时，
// 看的人会以为行数与实际不符。
func distinctSorted(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// VerifyScope 是一次账本完整性校验的范围。
type VerifyScope struct {
	// From 是起点（含）。零值表示不限。
	From time.Time
	// To 是终点（**不含**）。零值表示不限。
	To time.Time
	// MaxTxns 是最多校验多少笔交易，<=0 表示不限。
	//
	// 单位是**交易**而不是行：按行截断会把一笔交易切成两半，而半笔交易
	// 恰好可能仍然是平的（见 ledger.VerifyEntries 的注释），于是截断本身
	// 会制造出「查过了、没问题」的假象。
	//
	// 上限只在读取侧生效：库那边仍要把结果排完序才给得出第一行，所以它省的是
	// 内存与校验时间，不是扫描时间。
	MaxTxns int
}

// VerifyFinding 是一笔没通过校验的交易。
type VerifyFinding struct {
	// TxnID 是哪一笔。它必须单独带着：ledger 的校验错误里只有「第 N 条分录」，
	// 而 N 是**组内**下标，光看错误信息回不到是哪一笔交易。
	TxnID ledger.TxnID
	// Entries 是这一笔的全部分录，按读回的顺序（txn_id, id）。
	//
	// 带着它是因为两类错误都得靠它才看得懂：「第 N 条分录」要对着一张按同一
	// 顺序排的表才数得出来，而「和不为零」那类错误根本不点名任何一条。
	Entries []ledger.Entry
	// Err 是校验报出来的问题，措辞由 internal/ledger 给。
	Err error
}

// VerifyReport 是一次完整性校验的结果。
type VerifyReport struct {
	// CheckedTxns 是校验过的交易数。
	CheckedTxns int
	// CheckedEntries 是读回并参与校验的分录条数。
	CheckedEntries int
	// Findings 是没通过校验的交易。
	Findings []VerifyFinding
	// Truncated 表示达到 MaxTxns 就停下了，后面还有没读的交易。
	//
	// 它是「这一轮不是全量」的唯一标志：没有它，一份被截断的干净报告与一份
	// 跑完的干净报告长得一模一样（LESSONS 第 16 条）。
	Truncated bool
}

// Verify 读回一段分录并逐笔校验。
//
// # 校验单位是交易，不是行
//
// 按 txn_id 分组、整组交给 ledger.VerifyEntries。范围（From/To）先定成
// 「在区间内出现过的 txn_id」，再按 txn_id 取回**整笔**——直接按行过滤会
// 把横跨区间边界的交易切两半，而半笔恰好可能仍然是平的，于是校验会放过它
// （见 ledger.VerifyEntries 的注释，这里正是它要求的那种调用方式）。
//
// # 出错也交回半份报告
//
// 读不下去时返回的是**已经算出来的那部分**加错误，而不是空报告：跑了一半的
// 结果至少说明比到了哪里、已经发现了什么，比一句「失败了」有用得多
// （与 reconcile 的处置一致）。
func (s *Store) Verify(ctx context.Context, sc VerifyScope) (*VerifyReport, error) {
	query, args := verifyQuery(sc)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("读分录失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	report := &VerifyReport{}
	// group 是当前这一笔交易的分录。按 txn_id 排序读，同一个 txn_id 的行
	// 因此是连续的，遇到新 id 就说明上一笔读完了。
	//
	// 每笔重新开一个切片（不是 group[:0] 复用）：Findings 里留着这些切片，
	// 复用会让先前那一笔的清单在后续读取中被覆盖掉。
	var group []ledger.Entry
	for rows.Next() {
		e, _, err := scanEntry(rows)
		if err != nil {
			// 读不出来的行在这里是**错误**，不是「少算一行」：校验的对象是
			// 整笔交易，少一行可能让它看起来是平的（见 ledger.VerifyEntries
			// 的注释），于是这一轮的结果不再可信，必须停下。
			return report, err
		}
		if len(group) > 0 && e.TxnID != group[0].TxnID {
			addVerifyGroup(report, group)
			group = nil
			if sc.MaxTxns > 0 && report.CheckedTxns >= sc.MaxTxns {
				report.Truncated = true
				break
			}
		}
		group = append(group, e)
	}
	if err := rows.Err(); err != nil {
		return report, fmt.Errorf("遍历分录失败: %w", err)
	}
	if len(group) > 0 {
		addVerifyGroup(report, group)
	}
	return report, nil
}

// verifyQuery 拼出校验用的查询。
//
// 范围写成子查询（`txn_id IN (…区间内出现过的 txn_id…)`）而不是直接过滤行，
// 理由见 Verify 的注释：被区间切一半的交易可能看起来是平的。
func verifyQuery(sc VerifyScope) (string, []any) {
	rangeWhere, args := readScope{from: sc.From, to: sc.To}.where()
	query := `SELECT ` + entryColumns + ` FROM ledger_entries`
	if rangeWhere != "" {
		query += ` WHERE txn_id IN (SELECT txn_id FROM ledger_entries` + rangeWhere + `)`
	}
	// 按 (txn_id, id) 排：前者是分组的前提，后者让同一笔里的顺序是插入顺序
	// ——「第 N 条分录」那句话要能对着库里的行数出来。
	return query + ` ORDER BY txn_id, id`, args
}

// addVerifyGroup 校验一笔交易，把它记进报告。
func addVerifyGroup(r *VerifyReport, entries []ledger.Entry) {
	r.CheckedTxns++
	r.CheckedEntries += len(entries)
	if err := ledger.VerifyEntries(entries); err != nil {
		r.Findings = append(r.Findings, VerifyFinding{
			TxnID:   entries[0].TxnID,
			Entries: entries,
			Err:     err,
		})
	}
}
