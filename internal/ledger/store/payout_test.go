package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/money"
)

// 提现那一族的集成用例：申请 → 批准 / 驳回 → 打款，每一档都断言**具体金额与
// 分录**，而不只断言「没报错」。
//
// 这一层动的是钱，而写反方向、写两遍、少写一组都不会报错：账本自己仍然平
// （每组分录的和都是零），只有把科目与金额逐条比出来才看得见。重放与冲突两条
// 也必须在这里比出来——它们是「同一条命令重跑」与「同一张单被推两次」的区别。
//
// 用例之间共用同一个库且不清库（见 helpers_test.go 的说明），所以每一处汇总都
// **按这次用例自己的商户或单子**收窄；不收窄的求和会把别条用例留下的行算进来，
// 而那种错的表象是「单独跑绿、整包跑红」。

// bareUser 建一个**没有钱包行**的用户。
//
// 提现那一族一分钱都不发到 user 科目上（钱从 merchant_payable 挪进 payout），
// 所以它不该要求那个商户已经开户。seed() 会顺手开一个钱包，用它就测不出这一条
// ——而「先给每个商户开户」会变成一个假的必要步骤。
func bareUser(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	id := 200000 + nextID()
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO users (id, username, email) VALUES ($1, $2, $3)`,
		id, "p"+itoa(id), "p"+itoa(id)+"@example.com"); err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	return id
}

// fundPayable 通过**真实路径**给一个商户记下分成：一个已开户的付款人消费一次，
// 分成落进 merchant_payable:<商户>。
//
// 不直接改余额、也不手插分录：那样造出来的分成在账本里没有出处，而「余额等于
// 账本推出来的值」正是本包要保证的东西（同 seed 的说明）。提现申请现在会先核
// 这个科目的余额，所以每一条要申请成功的用例都得先走这一步。
//
// merchantID 不要求真实存在：给幽灵商户记分成正是「外键守卫」那条用例要造的
// 状态——申请最后该倒在 payout_requests 的外键上。
func fundPayable(t *testing.T, db *sql.DB, merchantID int64, amount string) {
	t.Helper()
	share, err := money.Parse(amount)
	if err != nil {
		t.Fatalf("解析分成金额失败: %v", err)
	}
	payer := seed(t, db, amount)
	txn, buildErr := ledger.Consume(ledger.ConsumeParams{
		TxnID: ledger.NewTxnID(), UserID: payer, Amount: share,
		Asset: ledger.AssetCNY, MerchantID: merchantID, MerchantShare: share,
	})
	if buildErr != nil {
		t.Fatalf("构造分成消费失败: %v", buildErr)
	}
	if _, postErr := New(db).PostTransaction(context.Background(), txn); postErr != nil {
		t.Fatalf("记分成失败: %v", postErr)
	}
}

// payoutOf 读一条提现单现在的样子。
//
// 走 scanPayout（生产的那一份解析），不在这里另写一遍 SELECT：另写一遍的话，
// 解析里的 bug（例如金额读错、状态没认出来）会被用例的解析一起掩盖过去。
func payoutOf(t *testing.T, db *sql.DB, id int64) Payout {
	t.Helper()
	row, err := scanPayout(db.QueryRowContext(context.Background(),
		`SELECT `+payoutColumns+` FROM payout_requests WHERE id = $1`, id).Scan)
	if err != nil {
		t.Fatalf("读提现单 %d 失败: %v", id, err)
	}
	return row
}

// payoutRows 数一个商户名下的提现单有几行。
func payoutRows(t *testing.T, db *sql.DB, merchantID int64) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM payout_requests WHERE merchant_user_id = $1`,
		merchantID).Scan(&n); err != nil {
		t.Fatalf("数提现单失败: %v", err)
	}
	return n
}

// accountTotal 从分录现算一个科目的余额。
//
// 显式按 numeric(20,8) 收口再转文本：没有 CAST 时 `COALESCE(SUM(amount), 0)`
// 在**一行都没有**时给出的是整数零的 `"0"`，与有行时的 `"0.00000000"` 不是同一个
// 字符串——而「科目被核销干净」这件事正是要判「等于零」，两种写法混在一处会让
// 判据的成败取决于那一行在不在。
func accountTotal(t *testing.T, db *sql.DB, account ledger.Account) string {
	t.Helper()
	var total string
	if err := db.QueryRowContext(context.Background(),
		`SELECT COALESCE(SUM(amount), 0)::numeric(20,8)::text FROM ledger_entries WHERE account = $1`,
		account.String()).Scan(&total); err != nil {
		t.Fatalf("汇总科目 %s 失败: %v", account, err)
	}
	return total
}

// merchantPayoutTotal 算一个商户名下所有提现单在待付科目上还剩多少。
//
// 三个条件缺一不可：按这些单子的 payout_id 收窄（不然会把别条用例的单子算进
// 来）、只看 payout 科目（每一组分录都同时挂着商户的分成科目，两组一起求和恒为
// 零——那正是「这一组分录是平的」，测不出冻结还剩多少）、只看这一个商户。
func merchantPayoutTotal(t *testing.T, db *sql.DB, merchantID int64) string {
	t.Helper()
	var total string
	if err := db.QueryRowContext(context.Background(), `
		SELECT COALESCE(SUM(amount), 0)::numeric(20,8)::text FROM ledger_entries
		 WHERE account LIKE 'payout:%'
		   AND payout_id IN (SELECT id FROM payout_requests WHERE merchant_user_id = $1)`,
		merchantID).Scan(&total); err != nil {
		t.Fatalf("汇总待付科目失败: %v", err)
	}
	return total
}

// merchantEntryCount 数一个商户名下（分成科目 + 他那几条单子的待付科目）的分录条数。
func merchantEntryCount(t *testing.T, db *sql.DB, merchantID int64) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), `
		SELECT count(*) FROM ledger_entries
		 WHERE account = $1
		    OR payout_id IN (SELECT id FROM payout_requests WHERE merchant_user_id = $2)`,
		ledger.MerchantPayableAccount(merchantID).String(), merchantID).Scan(&n); err != nil {
		t.Fatalf("数分录失败: %v", err)
	}
	return n
}

// wantEntries 逐条比对某个交易在库里的分录。
//
// 比的是**整行**（科目、资产、金额、分录类型、备注），不是只比金额：写反科目、
// 写错 entry_type 都不会让账本不平，而这两样正是将来对账时要靠的字段。
func wantEntries(t *testing.T, db *sql.DB, txnID ledger.TxnID, want []entryRow) {
	t.Helper()
	got := entriesOf(t, db, txnID)
	if len(got) != len(want) {
		t.Fatalf("交易 %s 写下 %d 条分录，期望 %d 条：\n得到 %+v\n期望 %+v",
			txnID, len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 条分录（按科目排序）= %+v，期望 %+v", i+1, got[i], want[i])
		}
	}
}

// 申请：建一行 pending 的单子，并把申请额从商户的分成挪进待付。
func TestRequestPayoutWritesRowAndFreeze(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()
	merchant := bareUser(t, db)
	fundPayable(t, db, merchant, "12.5")
	txnID := ledger.NewTxnID()

	row, applied, err := s.RequestPayout(ctx, RequestPayoutInput{
		TxnID: txnID, UserID: merchant, Amount: money.MustParse("12.5"),
		Asset: ledger.AssetCNY, Destination: []byte(`{"bank":"placeholder-bank"}`),
		Ref: "REQ-A",
	})
	if err != nil {
		t.Fatalf("申请提现失败: %v", err)
	}
	if !applied {
		t.Error("第一次申请应当返回 applied=true")
	}
	if row.Status != PayoutPending {
		t.Errorf("状态 = %q，期望 %q", row.Status, PayoutPending)
	}
	if row.UserID != merchant {
		t.Errorf("商户 = %d，期望 %d", row.UserID, merchant)
	}
	if row.Amount.String() != "12.50000000" {
		t.Errorf("金额 = %s，期望 12.50000000", row.Amount)
	}
	if string(row.Destination) != `{"bank": "placeholder-bank"}` {
		t.Errorf("收款信息 = %q，期望写下给的那一段 JSON", row.Destination)
	}
	if row.ReviewedBy != nil || row.ReviewedAt != nil {
		t.Error("还没审批的单子不该有审批人与审批时刻")
	}
	if row.PaidAt != nil || row.PayoutRef != "" {
		t.Error("还没打款的单子不该有打款时刻与凭据")
	}
	if row.CreatedAt.IsZero() {
		t.Error("建单时刻由库里的 now() 给，不该是零值")
	}

	// 分录：贷 merchant_payable（他还能申请的钱少了一笔）、借 payout:<单id>
	// （这条单子的待付多了一笔）。两处都挂上同一个 payout_id。
	wantEntries(t, db, txnID, []entryRow{
		{
			Account: ledger.MerchantPayableAccount(merchant).String(), Asset: "CNY",
			Amount: "-12.50000000", EntryType: string(ledger.EntryPayoutRequest),
		},
		{
			Account: ledger.PayoutAccount(row.ID).String(), Asset: "CNY",
			Amount: "12.50000000", EntryType: string(ledger.EntryPayoutRequest),
			Memo: "提现申请（单据 REQ-A）",
		},
	})

	// 待付科目上正好是这一笔：申请之后钱**冻着**，打款或驳回才动它。
	if got := accountTotal(t, db, ledger.PayoutAccount(row.ID)); got != "12.50000000" {
		t.Errorf("待付科目 = %s，期望 12.50000000", got)
	}
	// 这一族不改用户余额：那个商户连钱包行都没有（bareUser），
	// 而申请照样写得进去。
	var wallets int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM user_wallets WHERE user_id = $1`, merchant).Scan(&wallets); err != nil {
		t.Fatalf("数钱包行失败: %v", err)
	}
	if wallets != 0 {
		t.Errorf("申请提现给商户开了 %d 行钱包：这一族不该碰 user_wallets", wallets)
	}
}

// 提现申请在写之前要排到分成科目的锁后面。
//
// 余额校验是「读余额 → 判断够不够 → 写」，另开一条连接持着那把科目锁时，申请
// 必须拿到锁之后才读数。删掉 RequestPayout 里那次 lockAccount，它会在 500ms 的
// 观测窗内就跑完，这条用例必红——余额不足与 destination 那些用例都是单线程，
// 碰不到这把锁。
func TestRequestPayoutWaitsOnPayableLock(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()
	merchant := bareUser(t, db)
	fundPayable(t, db, merchant, "5")
	txnID := ledger.NewTxnID()
	in := RequestPayoutInput{
		TxnID: txnID, UserID: merchant, Amount: money.MustParse("5"),
		Asset: ledger.AssetCNY, Ref: "REQ-LOCK",
	}

	holder, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("开启持锁事务失败: %v", err)
	}
	defer func() { _ = holder.Rollback() }()
	if _, err := holder.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		ledger.MerchantPayableAccount(merchant).String()); err != nil {
		t.Fatalf("用例自己拿锁失败: %v", err)
	}

	var (
		row     Payout
		applied bool
		postErr error
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		row, applied, postErr = s.RequestPayout(ctx, in)
	}()

	select {
	case <-done:
		t.Fatal("分成科目已经被锁着时，RequestPayout 不该完成：申请侧的科目锁没生效")
	case <-time.After(500 * time.Millisecond):
	}
	// 等待期间看不到新单与新分录，意思是它们还没有提交。这条只说明「没有提前
	// 提交」：READ COMMITTED 下另一条连接本来就看不见未提交的插入。锁的位置由
	// 上面那条「没完成」证明。
	if n := payoutRows(t, db, merchant); n != 0 {
		t.Errorf("还在等锁时库里已经有 %d 行提现单", n)
	}
	if got := entriesOf(t, db, txnID); len(got) != 0 {
		t.Errorf("还在等锁时已经写下了 %d 条分录：%+v", len(got), got)
	}

	if err := holder.Rollback(); err != nil {
		t.Fatalf("释放持锁事务失败: %v", err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("放锁之后 RequestPayout 没有接着跑完")
	}
	if postErr != nil {
		t.Fatalf("申请失败: %v", postErr)
	}
	if !applied {
		t.Error("等到的这一次应当是真正建单的那一次（applied=true）")
	}
	if row.Status != PayoutPending {
		t.Errorf("状态 = %q，期望 %q", row.Status, PayoutPending)
	}
	if n := payoutRows(t, db, merchant); n != 1 {
		t.Errorf("放锁之后库里 %d 行提现单，期望 1 行", n)
	}
	if got := entriesOf(t, db, txnID); len(got) != 2 {
		t.Errorf("这个交易有 %d 条分录，期望 2 条", len(got))
	}
	if got := accountTotal(t, db, ledger.PayoutAccount(row.ID)); got != "5.00000000" {
		t.Errorf("待付科目 = %s，期望 5.00000000", got)
	}
}

// 重放：同一个申请单号再来一次，什么都不写，返回的还是那一行。
func TestRequestPayoutReplayKeepsTheSameRow(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()
	merchant := bareUser(t, db)
	fundPayable(t, db, merchant, "7")
	txnID := ledger.NewTxnID()
	in := RequestPayoutInput{
		TxnID: txnID, UserID: merchant, Amount: money.MustParse("7"),
		Asset: ledger.AssetCNY, Destination: []byte(`{"bank":"b"}`), Ref: "REQ-B",
	}

	first, applied, err := s.RequestPayout(ctx, in)
	if err != nil || !applied {
		t.Fatalf("第一次申请: applied=%v err=%v", applied, err)
	}
	second, applied, err := s.RequestPayout(ctx, in)
	if err != nil {
		t.Fatalf("重放不该报错: %v", err)
	}
	if applied {
		t.Error("重放应当返回 applied=false")
	}
	if second.ID != first.ID {
		t.Errorf("重放拿到的单号 = %d，期望还是 %d", second.ID, first.ID)
	}
	if !second.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("重放拿到的建单时刻 = %v，期望还是 %v（没有重新建一行）",
			second.CreatedAt, first.CreatedAt)
	}
	if n := payoutRows(t, db, merchant); n != 1 {
		t.Errorf("重放之后库里 %d 行提现单，期望 1 行", n)
	}
	if got := accountTotal(t, db, ledger.PayoutAccount(first.ID)); got != "7.00000000" {
		t.Errorf("重放之后待付科目 = %s，期望仍是 7.00000000（没有被冻两次）", got)
	}
	// 分录也还是那两条：重放不是「又写一遍内容一样的」。
	if got := entriesOf(t, db, txnID); len(got) != 2 {
		t.Errorf("重放之后这笔交易有 %d 条分录，期望 2 条", len(got))
	}
}

// 同一个申请单号配不同的金额不是重放，是冲突：一笔都不写。
func TestRequestPayoutSameRefDifferentAmountIsAConflict(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()
	merchant := bareUser(t, db)
	fundPayable(t, db, merchant, "7")
	txnID := ledger.NewTxnID()

	first, _, err := s.RequestPayout(ctx, RequestPayoutInput{
		TxnID: txnID, UserID: merchant, Amount: money.MustParse("7"), Asset: ledger.AssetCNY,
		Ref: "REQ-C",
	})
	if err != nil {
		t.Fatalf("第一次申请失败: %v", err)
	}

	_, _, err = s.RequestPayout(ctx, RequestPayoutInput{
		TxnID: txnID, UserID: merchant, Amount: money.MustParse("9"), Asset: ledger.AssetCNY,
		Ref: "REQ-C",
	})
	if !errors.Is(err, ErrTxnConflict) {
		t.Fatalf("同键异参应当返回 ErrTxnConflict，得到 %v", err)
	}
	if n := payoutRows(t, db, merchant); n != 1 {
		t.Errorf("冲突之后库里 %d 行提现单，期望仍是 1 行", n)
	}
	if got := accountTotal(t, db, ledger.PayoutAccount(first.ID)); got != "7.00000000" {
		t.Errorf("冲突之后待付 = %s，期望仍是 7.00000000（没有被写成 9）", got)
	}
}

// 同一个申请单号配不同的收款信息不是重放，而是冲突。
//
// 收款信息不在分录里，同一条交易比不出它来，所以这一栏单独判一次：放过去的话，
// 重跑一条旧命令会把那张单子的收款账号悄悄换掉，而账面上没有任何痕迹。
//
// 比较按 jsonb 的等值口径：键序与空白不同、内容相同仍是重放——那是同一份收款
// 信息，把它判成冲突会让重跑一条命令看起来像换了账号。
func TestRequestPayoutSameRefDifferentDestinationIsAConflict(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()
	merchant := bareUser(t, db)
	fundPayable(t, db, merchant, "7")
	txnID := ledger.NewTxnID()
	in := RequestPayoutInput{
		TxnID: txnID, UserID: merchant, Amount: money.MustParse("7"), Asset: ledger.AssetCNY,
		Destination: []byte(`{"bank":"placeholder-bank"}`), Ref: "REQ-DEST",
	}

	first, applied, err := s.RequestPayout(ctx, in)
	if err != nil || !applied {
		t.Fatalf("第一次申请: applied=%v err=%v", applied, err)
	}

	// 空白不同、内容相同：jsonb 归一之后是同一份，按重放处理。
	same := in
	same.Destination = []byte(`{ "bank" : "placeholder-bank" }`)
	if _, replayApplied, replayErr := s.RequestPayout(ctx, same); replayErr != nil || replayApplied {
		t.Errorf("内容相同的收款信息应当判成重放: applied=%v err=%v", replayApplied, replayErr)
	}

	// 换一份内容：冲突，且库里那一份一个字都不能变。
	changed := in
	changed.Destination = []byte(`{"bank":"another-bank"}`)
	_, _, err = s.RequestPayout(ctx, changed)
	if !errors.Is(err, ErrTxnConflict) {
		t.Fatalf("同键异收款信息应当返回 ErrTxnConflict，得到 %v", err)
	}
	if n := payoutRows(t, db, merchant); n != 1 {
		t.Errorf("冲突之后库里 %d 行提现单，期望仍是 1 行", n)
	}
	if got := string(payoutOf(t, db, first.ID).Destination); got != `{"bank": "placeholder-bank"}` {
		t.Errorf("冲突之后收款信息 = %s，期望仍是第一份", got)
	}
}

// 分成余额不够时拒掉，一笔都不写，报错里带两个数。
//
// 「够不够」要拿两个数说清：只说「余额不足」，运维得自己去查那一格，而这一条
// 最常见的成因是分成本身还没记进账本（见 funding 的说明）。
func TestRequestPayoutRejectsInsufficientPayable(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()
	merchant := bareUser(t, db)
	fundPayable(t, db, merchant, "5")
	txnID := ledger.NewTxnID()

	_, _, err := s.RequestPayout(ctx, RequestPayoutInput{
		TxnID: txnID, UserID: merchant, Amount: money.MustParse("5.01"),
		Asset: ledger.AssetCNY, Ref: "REQ-NSF",
	})
	if !errors.Is(err, ErrInsufficientPayable) {
		t.Fatalf("余额不足应当返回 ErrInsufficientPayable，得到 %v", err)
	}
	for _, want := range []string{"商户 " + itoa(merchant), "5.00000000", "5.01000000"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("报错 = %v，期望含 %q", err, want)
		}
	}
	if n := payoutRows(t, db, merchant); n != 0 {
		t.Errorf("被拒的申请留下了 %d 行提现单", n)
	}
	if got := entriesOf(t, db, txnID); len(got) != 0 {
		t.Errorf("被拒的申请写下了 %d 条分录：%+v", len(got), got)
	}
	// 分成科目原样：被拒的那一次一分钱都没动它。
	if got := accountTotal(t, db, ledger.MerchantPayableAccount(merchant)); got != "5.00000000" {
		t.Errorf("被拒之后分成科目 = %s，期望仍是 5.00000000", got)
	}
}

// 申请的输入校验：金额、资产、收款信息与 txn_id 各判一遍。
//
// 这些判据与命令行那几道门重复，但它们在**这一层**是必须的：提现那一族将来会有
// 不止一个调用方（回调、批量脚本），而命令行不是守门人。
func TestRequestPayoutRejectsBadInput(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()
	merchant := bareUser(t, db)

	cases := []struct {
		name string
		in   RequestPayoutInput
		want string
	}{
		// 每条都断言**报的是哪一句**，而不只是「报了错」：守卫放过去之后，
		// 这些输入会落到库上，报出来的是外键 / 约束那一类的话——同样是一个
		// error，于是「只断言非 nil」的用例在守卫边界被改掉时仍然绿。
		{"金额为零", RequestPayoutInput{TxnID: ledger.NewTxnID(), UserID: merchant,
			Amount: money.Zero, Asset: ledger.AssetCNY}, "金额必须为正数"},
		{"金额为负", RequestPayoutInput{TxnID: ledger.NewTxnID(), UserID: merchant,
			Amount: money.MustParse("-1"), Asset: ledger.AssetCNY}, "金额必须为正数"},
		// 0 是「没给」：商户 id 0 绕过守卫会去插一行 merchant_user_id = 0，
		// 报出来的是外键违反而不是这一句。
		{"缺商户 id", RequestPayoutInput{TxnID: ledger.NewTxnID(), UserID: 0,
			Amount: money.MustParse("1"), Asset: ledger.AssetCNY}, "商户 id 非法: 0"},
		{"资产不认识", RequestPayoutInput{TxnID: ledger.NewTxnID(), UserID: merchant,
			Amount: money.MustParse("1"), Asset: ledger.Asset("USDT")}, "不受支持"},
		{"缺交易号", RequestPayoutInput{UserID: merchant,
			Amount: money.MustParse("1"), Asset: ledger.AssetCNY}, "缺少 txn_id"},
		{"收款信息不是 JSON", RequestPayoutInput{TxnID: ledger.NewTxnID(), UserID: merchant,
			Amount: money.MustParse("1"), Asset: ledger.AssetCNY, Destination: []byte("not-json")},
			"收款信息必须是合法的 JSON 对象"},
		// 合法 JSON 但不是对象：字符串、数组、null 都取不出字段，而打款通道按
		// 字段取值。三种形状各占一条，判「首字符是 {」的那一格才算逐条钉住。
		{"收款信息是字符串", RequestPayoutInput{TxnID: ledger.NewTxnID(), UserID: merchant,
			Amount: money.MustParse("1"), Asset: ledger.AssetCNY, Destination: []byte(`"abc"`)},
			"收款信息必须是合法的 JSON 对象"},
		{"收款信息是数组", RequestPayoutInput{TxnID: ledger.NewTxnID(), UserID: merchant,
			Amount: money.MustParse("1"), Asset: ledger.AssetCNY, Destination: []byte(`[1]`)},
			"收款信息必须是合法的 JSON 对象"},
		{"收款信息是 null", RequestPayoutInput{TxnID: ledger.NewTxnID(), UserID: merchant,
			Amount: money.MustParse("1"), Asset: ledger.AssetCNY, Destination: []byte(`null`)},
			"收款信息必须是合法的 JSON 对象"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := s.RequestPayout(ctx, tc.in)
			if err == nil {
				t.Fatal("这条输入应当被拒，却建出了单子")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("报错 = %v，期望含 %q（这一条该在写库之前就被拒）", err, tc.want)
			}
		})
	}
	if n := payoutRows(t, db, merchant); n != 0 {
		t.Errorf("被拒的申请留下了 %d 行提现单", n)
	}
	if got := accountTotal(t, db, ledger.MerchantPayableAccount(merchant)); got != "0.00000000" {
		t.Errorf("被拒的申请在分成科目上留下了 %s", got)
	}
}

// 批准 / 驳回 / 打款三条路上那五个 `<= 0` 的边界：单号与审批人为 0 时拒在写库之前。
//
// 0 要在这里拒，而不是让它往下走：单号 0 会落到「没有这条提现单」，审批人 0 会
// 落到外键违反——两句话都指向库，操作者从那里看不出是参数没给。所以下面断言的是
// **报错的那一句话**：把 `<=` 放宽成 `<`，每一条都会换成另一句话，而「只断言 err
// 非 nil」的写法在那里仍然是绿的。
func TestPayoutFamilyRejectsZeroIDs(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()
	merchant := bareUser(t, db)
	reviewer := bareUser(t, db)
	fundPayable(t, db, merchant, "1")

	// 一条待审批的单子：「审批人 / 驳回人为 0」那两格要一个真的存在的单号，
	// 否则先撞在「找不到单子」上，测不到要测的那一个守卫。
	row, _, err := s.RequestPayout(ctx, RequestPayoutInput{
		TxnID: ledger.NewTxnID(), UserID: merchant,
		Amount: money.MustParse("1"), Asset: ledger.AssetCNY, Ref: "REQ-ZERO",
	})
	if err != nil {
		t.Fatalf("建待审批的单子失败: %v", err)
	}

	cases := []struct {
		name string
		call func() error
		want string
	}{
		{"批准：单号为 0", func() error {
			_, _, err := s.ApprovePayout(ctx, ApprovePayoutInput{PayoutID: 0, Reviewer: reviewer})
			return err
		}, "提现单 id 非法: 0"},
		{"批准：审批人为 0", func() error {
			_, _, err := s.ApprovePayout(ctx, ApprovePayoutInput{PayoutID: row.ID})
			return err
		}, "审批人 id 非法: 0"},
		{"驳回：单号为 0", func() error {
			_, _, err := s.RejectPayout(ctx, ReviewPayoutInput{
				PayoutID: 0, Reviewer: reviewer, TxnID: ledger.NewTxnID()})
			return err
		}, "提现单 id 非法: 0"},
		{"驳回：驳回人为 0", func() error {
			_, _, err := s.RejectPayout(ctx, ReviewPayoutInput{
				PayoutID: row.ID, TxnID: ledger.NewTxnID()})
			return err
		}, "驳回人 id 非法: 0"},
		{"打款：单号为 0", func() error {
			_, _, err := s.PayPayout(ctx, PayPayoutInput{
				PayoutID: 0, TxnID: ledger.NewTxnID(), Gateway: "manual"})
			return err
		}, "提现单 id 非法: 0"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatal("0 应该被拒")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("报错 = %v，期望含 %q（0 是「没给」，要在写库之前拒）", err, tc.want)
			}
		})
	}

	// 被拒的调用一条都不该落下去：单子还停在 pending，也没有审批人。
	got := payoutOf(t, db, row.ID)
	if got.Status != PayoutPending {
		t.Errorf("被拒的调用把状态改成了 %q，期望仍是 pending", got.Status)
	}
	if got.ReviewedBy != nil {
		t.Errorf("被拒的调用写上了审批人 %d", *got.ReviewedBy)
	}
}

// 三种处置走一遍：申请之后钱冻在待付科目上，驳回把它解冻，打款把它核销。
//
// 一条用例走完两条终态而不是两条用例各走一遍：这里要证明的是**同一张余额表**
// 在三条路径之后的读法（12.5 冻着、7 被驳回退回、9 被打款出去）。三条各写一遍
// 的话，「驳回之后待付归零」与「打款之后待付归零」这两个判据会各自孤立地漂，
// 而它们要说明的其实是同一件事。
func TestPayoutFreezeIsReleasedOrSettled(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()
	merchant := bareUser(t, db)
	reviewer := bareUser(t, db)
	fundPayable(t, db, merchant, "28.5")

	request := func(amount string) Payout {
		t.Helper()
		row, _, err := s.RequestPayout(ctx, RequestPayoutInput{
			TxnID: ledger.NewTxnID(), UserID: merchant, Amount: money.MustParse(amount),
			Asset: ledger.AssetCNY, Ref: "REQ-FREEZE-" + amount,
		})
		if err != nil {
			t.Fatalf("申请 %s 失败: %v", amount, err)
		}
		return row
	}

	pending := request("12.5")
	rejected := request("7")
	toBePaid := request("9")

	// 分成科目上先是 +28.5（fundPayable 记下的那一笔），三条申请各冻走一份，
	// 合计正好把它清到 0；待付科目上是同样多的正数。钱在申请这一步就全部冻住了。
	const funded = "28.50000000"
	if got := merchantPayoutTotal(t, db, merchant); got != "28.50000000" {
		t.Fatalf("申请之后待付合计 = %s，期望 28.50000000", got)
	}
	if got := accountTotal(t, db, ledger.MerchantPayableAccount(merchant)); got != "0.00000000" {
		t.Fatalf("申请之后分成科目 = %s，期望 0.00000000（%s 全部冻走了）", got, funded)
	}

	// 驳回：解冻那 7 元。分录是申请那一组的镜像。
	rejTxn := ledger.NewTxnID()
	row, applied, err := s.RejectPayout(ctx, ReviewPayoutInput{
		PayoutID: rejected.ID, Reviewer: reviewer, Note: "收款信息不全", TxnID: rejTxn,
	})
	if err != nil {
		t.Fatalf("驳回失败: %v", err)
	}
	if !applied {
		t.Error("第一次驳回应当返回 applied=true")
	}
	if row.Status != PayoutRejected {
		t.Errorf("状态 = %q，期望 %q", row.Status, PayoutRejected)
	}
	if row.ReviewNote != "收款信息不全" {
		t.Errorf("驳回原因 = %q", row.ReviewNote)
	}
	if row.ReviewedBy == nil || *row.ReviewedBy != reviewer {
		t.Errorf("驳回人 = %v，期望 %d", row.ReviewedBy, reviewer)
	}
	if row.ReviewedAt == nil {
		t.Error("驳回没有记下时刻")
	}
	wantEntries(t, db, rejTxn, []entryRow{
		{
			Account: ledger.MerchantPayableAccount(merchant).String(), Asset: "CNY",
			Amount: "7.00000000", EntryType: string(ledger.EntryPayoutReject),
		},
		{
			Account: ledger.PayoutAccount(rejected.ID).String(), Asset: "CNY",
			Amount: "-7.00000000", EntryType: string(ledger.EntryPayoutReject),
		},
	})
	if got := accountTotal(t, db, ledger.PayoutAccount(rejected.ID)); got != "0.00000000" {
		t.Errorf("驳回之后那条待付 = %s，期望 0.00000000", got)
	}
	// 分成科目跟着回到 7：那 7 元回到「他还能申请」的那一份里。
	if got := accountTotal(t, db, ledger.MerchantPayableAccount(merchant)); got != "7.00000000" {
		t.Errorf("驳回之后分成科目 = %s，期望 7.00000000（解冻的那 7 元回来了）", got)
	}

	// 打款：9 元从待付出账。它必须先被批准。
	approved, applied, err := s.ApprovePayout(ctx, ApprovePayoutInput{
		PayoutID: toBePaid.ID, Reviewer: reviewer, Note: "核对无误",
	})
	if err != nil {
		t.Fatalf("批准失败: %v", err)
	}
	if !applied {
		t.Error("第一次批准应当返回 applied=true")
	}
	if approved.Status != PayoutApproved {
		t.Fatalf("批准之后状态 = %q，期望 %q", approved.Status, PayoutApproved)
	}
	payTxn := ledger.NewTxnID()
	payRow, applied, err := s.PayPayout(ctx, PayPayoutInput{
		PayoutID: toBePaid.ID, TxnID: payTxn, Gateway: "placeholder-gateway", Ref: "PAYOUT-1",
	})
	if err != nil {
		t.Fatalf("打款失败: %v", err)
	}
	if !applied {
		t.Error("第一次打款应当返回 applied=true")
	}
	if payRow.Status != PayoutPaid {
		t.Errorf("状态 = %q，期望 %q", payRow.Status, PayoutPaid)
	}
	if payRow.PayoutRef != "PAYOUT-1" {
		t.Errorf("打款凭据 = %q，期望 PAYOUT-1", payRow.PayoutRef)
	}
	if payRow.PaidAt == nil {
		t.Error("打款没有记下时刻")
	}
	// 凭据号那句备注挂在**待付**那一条上（与充值挂在用户那一条上同一个形状），
	// 通道那一条没有备注。
	wantEntries(t, db, payTxn, []entryRow{
		{
			Account: ledger.PaymentGatewayAccount("placeholder-gateway").String(), Asset: "CNY",
			Amount: "9.00000000", EntryType: string(ledger.EntryPayoutPaid),
		},
		{
			Account: ledger.PayoutAccount(toBePaid.ID).String(), Asset: "CNY",
			Amount: "-9.00000000", EntryType: string(ledger.EntryPayoutPaid),
			Memo: "提现打款（单据 PAYOUT-1）",
		},
	})
	if got := accountTotal(t, db, ledger.PayoutAccount(toBePaid.ID)); got != "0.00000000" {
		t.Errorf("打款之后那条待付 = %s，期望 0.00000000", got)
	}

	// 收尾：只剩那条还停在 pending 的 12.5——三种处置各留下一条可追溯的路径。
	if got := merchantPayoutTotal(t, db, merchant); got != "12.50000000" {
		t.Errorf("收尾时待付合计 = %s，期望只剩没人动过的那条 12.50000000", got)
	}
	if row := payoutOf(t, db, pending.ID); row.Status != PayoutPending {
		t.Errorf("没人动过的那条单子状态 = %q，期望 %q", row.Status, PayoutPending)
	}
}

// 驳回的重放：什么都不写，那一行原样返回。
//
// 它比别的重放更容易写错：重放的判定必须在**状态判定之前**——重放到达时这一行
// 已经是 rejected，而驳回那道门只放行 pending。两条判据写反时，重跑同一条命令
// 拿到的是「状态不允许」，看起来像一次拒绝，实际上它一次都没改过任何东西。
func TestRejectPayoutReplayWritesNothing(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()
	merchant := bareUser(t, db)
	reviewer := bareUser(t, db)
	fundPayable(t, db, merchant, "4")
	row, _, err := s.RequestPayout(ctx, RequestPayoutInput{
		TxnID: ledger.NewTxnID(), UserID: merchant, Amount: money.MustParse("4"),
		Asset: ledger.AssetCNY, Ref: "REQ-REJ",
	})
	if err != nil {
		t.Fatalf("申请失败: %v", err)
	}
	txnID := ledger.NewTxnID()
	in := ReviewPayoutInput{PayoutID: row.ID, Reviewer: reviewer, Note: "驳回", TxnID: txnID}

	if _, applied, rejectErr := s.RejectPayout(ctx, in); rejectErr != nil || !applied {
		t.Fatalf("第一次驳回: applied=%v err=%v", applied, rejectErr)
	}
	again, applied, err := s.RejectPayout(ctx, in)
	if err != nil {
		t.Fatalf("重放不该报错（重放的判定要在状态判定之前）: %v", err)
	}
	if applied {
		t.Error("重放应当返回 applied=false")
	}
	if again.Status != PayoutRejected {
		t.Errorf("重放拿到的状态 = %q，期望 %q", again.Status, PayoutRejected)
	}
	if got := accountTotal(t, db, ledger.PayoutAccount(row.ID)); got != "0.00000000" {
		t.Errorf("重放之后待付 = %s，期望 0.00000000（没有被解冻两次）", got)
	}
	if got := accountTotal(t, db, ledger.MerchantPayableAccount(merchant)); got != "4.00000000" {
		t.Errorf("重放之后分成科目 = %s，期望 4.00000000（那 4 元只被还回来一次）", got)
	}
	if got := entriesOf(t, db, txnID); len(got) != 2 {
		t.Errorf("重放之后这笔交易有 %d 条分录，期望 2 条", len(got))
	}
}

// 出账的唯一一道门：没批准的单子打不了款，而且**一笔都不写**。
//
// 这一条是本族的硬要求（「不许存在没经过 approved 就能出钱的路径」），所以它
// 同时判三件事：报错、状态没动、账本上一个字节都没加。
func TestPayPayoutRequiresApproveFirst(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()
	merchant := bareUser(t, db)
	fundPayable(t, db, merchant, "5")
	row, _, err := s.RequestPayout(ctx, RequestPayoutInput{
		TxnID: ledger.NewTxnID(), UserID: merchant, Amount: money.MustParse("3"),
		Asset: ledger.AssetCNY, Ref: "REQ-PAY-A",
	})
	if err != nil {
		t.Fatalf("申请失败: %v", err)
	}

	txnID := ledger.NewTxnID()
	_, _, err = s.PayPayout(ctx, PayPayoutInput{
		PayoutID: row.ID, TxnID: txnID, Gateway: "placeholder-gateway",
	})
	if !errors.Is(err, ErrPayoutState) {
		t.Fatalf("未批准就打款应当返回 ErrPayoutState，得到 %v", err)
	}
	if got := entriesOf(t, db, txnID); len(got) != 0 {
		t.Errorf("被拒的打款写下了 %d 条分录：%+v", len(got), got)
	}
	if now := payoutOf(t, db, row.ID); now.Status != PayoutPending {
		t.Errorf("被拒之后状态 = %q，期望仍是 %q", now.Status, PayoutPending)
	}
	if got := accountTotal(t, db, ledger.PayoutAccount(row.ID)); got != "3.00000000" {
		t.Errorf("被拒之后待付 = %s，期望仍是 3.00000000", got)
	}

	// 驳回过的单子同样打不了款：终态就是终态。
	rejected, _, rerr := s.RequestPayout(ctx, RequestPayoutInput{
		TxnID: ledger.NewTxnID(), UserID: merchant, Amount: money.MustParse("2"),
		Asset: ledger.AssetCNY, Ref: "REQ-PAY-B",
	})
	if rerr != nil {
		t.Fatalf("申请失败: %v", rerr)
	}
	if _, _, err := s.RejectPayout(ctx, ReviewPayoutInput{
		PayoutID: rejected.ID, Reviewer: merchant, TxnID: ledger.NewTxnID(),
	}); err != nil {
		t.Fatalf("驳回失败: %v", err)
	}
	afterRejectTxn := ledger.NewTxnID()
	if _, _, err := s.PayPayout(ctx, PayPayoutInput{
		PayoutID: rejected.ID, TxnID: afterRejectTxn, Gateway: "placeholder-gateway",
	}); !errors.Is(err, ErrPayoutState) {
		t.Errorf("给驳回过的单子打款应当返回 ErrPayoutState，得到 %v", err)
	}
	if got := entriesOf(t, db, afterRejectTxn); len(got) != 0 {
		t.Errorf("被拒的打款写下了 %d 条分录：%+v", len(got), got)
	}

	// 换一个通道也不行：门挡的是状态，不是调用方给的通道。
	if _, _, err := s.PayPayout(ctx, PayPayoutInput{
		PayoutID: row.ID, TxnID: ledger.NewTxnID(), Gateway: "another-gateway",
	}); !errors.Is(err, ErrPayoutState) {
		t.Errorf("给未批准的单子换个通道打款应当返回 ErrPayoutState，得到 %v", err)
	}
}

// 打款的重放与「换一个凭据号再打一次」。
//
// 后者是这条命令最要紧的一条：幂等键取的是**单子自己的 id**，不是通道给的凭据号。
// 键取凭据号的话，同一张单换一个号会被算成一笔新交易，待付科目被扣成负数——
// 那是重复打款，而它一路都不会报错（那一组分录自己仍然平）。
func TestPayPayoutReplayAndRefChangeIsAConflict(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()
	merchant := bareUser(t, db)
	fundPayable(t, db, merchant, "5")
	row, _, err := s.RequestPayout(ctx, RequestPayoutInput{
		TxnID: ledger.NewTxnID(), UserID: merchant, Amount: money.MustParse("5"),
		Asset: ledger.AssetCNY, Ref: "REQ-PAID",
	})
	if err != nil {
		t.Fatalf("申请失败: %v", err)
	}
	if _, _, approveErr := s.ApprovePayout(ctx, ApprovePayoutInput{
		PayoutID: row.ID, Reviewer: merchant,
	}); approveErr != nil {
		t.Fatalf("批准失败: %v", approveErr)
	}

	txnID := ledger.NewTxnID()
	in := PayPayoutInput{PayoutID: row.ID, TxnID: txnID, Gateway: "placeholder-gateway", Ref: "PAYOUT-7"}
	if _, applied, payErr := s.PayPayout(ctx, in); payErr != nil || !applied {
		t.Fatalf("第一次打款: applied=%v err=%v", applied, payErr)
	}
	again, applied, err := s.PayPayout(ctx, in)
	if err != nil {
		t.Fatalf("重放不该报错（重放的判定要在状态判定之前）: %v", err)
	}
	if applied {
		t.Error("重放应当返回 applied=false")
	}
	if again.PayoutRef != "PAYOUT-7" {
		t.Errorf("重放拿到的凭据 = %q，期望还是 PAYOUT-7", again.PayoutRef)
	}

	// 换一个凭据号：键是单子 id，所以它撞的是同一笔交易、内容不同 → 冲突。
	_, _, err = s.PayPayout(ctx, PayPayoutInput{
		PayoutID: row.ID, TxnID: txnID, Gateway: "placeholder-gateway", Ref: "PAYOUT-8",
	})
	if !errors.Is(err, ErrTxnConflict) {
		t.Fatalf("换凭据号应当撞成内容冲突，得到 %v", err)
	}
	if got := accountTotal(t, db, ledger.PayoutAccount(row.ID)); got != "0.00000000" {
		t.Errorf("待付 = %s，期望 0.00000000（没有被扣第二次）", got)
	}
	// 凭据号也还是第一次那个：冲突那一次一个字段都没写进去。
	if now := payoutOf(t, db, row.ID); now.PayoutRef != "PAYOUT-7" {
		t.Errorf("凭据 = %q，期望仍是 PAYOUT-7", now.PayoutRef)
	}
}

// 批准：记名、幂等、不写分录。
func TestApprovePayoutRecordsReviewerAndIsIdempotent(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()
	merchant := bareUser(t, db)
	first := bareUser(t, db)
	second := bareUser(t, db)
	fundPayable(t, db, merchant, "6")

	row, _, err := s.RequestPayout(ctx, RequestPayoutInput{
		TxnID: ledger.NewTxnID(), UserID: merchant, Amount: money.MustParse("6"),
		Asset: ledger.AssetCNY, Ref: "REQ-APPROVE",
	})
	if err != nil {
		t.Fatalf("申请失败: %v", err)
	}
	before := merchantEntryCount(t, db, merchant)

	approved, applied, err := s.ApprovePayout(ctx, ApprovePayoutInput{
		PayoutID: row.ID, Reviewer: first, Note: "第一次核对",
	})
	if err != nil {
		t.Fatalf("批准失败: %v", err)
	}
	if !applied {
		t.Error("第一次批准应当返回 applied=true")
	}
	if approved.Status != PayoutApproved {
		t.Errorf("状态 = %q，期望 %q", approved.Status, PayoutApproved)
	}
	if approved.ReviewedBy == nil || *approved.ReviewedBy != first {
		t.Errorf("审批人 = %v，期望 %d", approved.ReviewedBy, first)
	}
	if approved.ReviewedAt == nil {
		t.Error("批准没有记下时刻")
	}
	if approved.ReviewNote != "第一次核对" {
		t.Errorf("审批说明 = %q", approved.ReviewNote)
	}
	// 批准不写分录：钱在申请那一步就冻住了。
	if after := merchantEntryCount(t, db, merchant); after != before {
		t.Errorf("批准写下了 %d 条分录，期望一条都不写", after-before)
	}

	// 重跑：库里那一行必须**一个字都没变**——包括审批人与说明。换一个人再批
	// 一次不许把「谁批的」改成他。（回执上那句「未改动」由调用方渲染。）
	again, applied, err := s.ApprovePayout(ctx, ApprovePayoutInput{
		PayoutID: row.ID, Reviewer: second, Note: "第二次核对",
	})
	if err != nil {
		t.Fatalf("重跑批准不该报错: %v", err)
	}
	if applied {
		t.Error("重跑批准应当返回 applied=false")
	}
	if again.ReviewedBy == nil || *again.ReviewedBy != first {
		t.Errorf("重跑之后审批人 = %v，期望仍是 %d（重跑不该改写谁批的）",
			again.ReviewedBy, first)
	}
	if again.ReviewNote != "第一次核对" {
		t.Errorf("重跑之后说明 = %q，期望仍是第一次那一句", again.ReviewNote)
	}
	if again.ReviewedAt == nil || !again.ReviewedAt.Equal(*approved.ReviewedAt) {
		t.Errorf("重跑之后审批时刻 = %v，期望仍是 %v", again.ReviewedAt, approved.ReviewedAt)
	}
	if after := merchantEntryCount(t, db, merchant); after != before {
		t.Errorf("重跑批准写下了 %d 条分录，期望一条都不写", after-before)
	}
}

// 状态不允许时，报出来的那一句要说清**现在是什么、这一步要什么**。
//
// 只说「不允许」的话，运维看不出是单子还没批、还是批过又驳回了，而两者的下一步
// 完全相反（前者去批，后者重新申请）。
func TestPayoutStateErrorNamesBothStates(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()
	merchant := bareUser(t, db)
	fundPayable(t, db, merchant, "1")
	row, _, err := s.RequestPayout(ctx, RequestPayoutInput{
		TxnID: ledger.NewTxnID(), UserID: merchant, Amount: money.MustParse("1"),
		Asset: ledger.AssetCNY, Ref: "REQ-STATE",
	})
	if err != nil {
		t.Fatalf("申请失败: %v", err)
	}

	_, _, err = s.PayPayout(ctx, PayPayoutInput{
		PayoutID: row.ID, TxnID: ledger.NewTxnID(), Gateway: "placeholder-gateway",
	})
	if err == nil {
		t.Fatal("未批准就打款应当报错")
	}
	for _, want := range []string{"pending", "待审批", "approved", "已批准"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息 %q 里没有 %q：这一句要同时点出当前状态与这一步要的状态",
				err.Error(), want)
		}
	}
}

// 按 id 找不到单子：三个动作都要报同一个哨兵，而不是一个笼统的失败。
func TestPayoutMissingIDIsAConflict(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()
	missing := int64(1 << 40)

	if _, _, err := s.ApprovePayout(ctx, ApprovePayoutInput{PayoutID: missing, Reviewer: 1}); !errors.Is(err, ErrNoPayoutRequest) {
		t.Errorf("批准一条不存在的单子应当返回 ErrNoPayoutRequest，得到 %v", err)
	}
	if _, _, err := s.RejectPayout(ctx, ReviewPayoutInput{
		PayoutID: missing, Reviewer: 1, TxnID: ledger.NewTxnID(),
	}); !errors.Is(err, ErrNoPayoutRequest) {
		t.Errorf("驳回一条不存在的单子应当返回 ErrNoPayoutRequest，得到 %v", err)
	}
	if _, _, err := s.PayPayout(ctx, PayPayoutInput{
		PayoutID: missing, TxnID: ledger.NewTxnID(), Gateway: "placeholder-gateway",
	}); !errors.Is(err, ErrNoPayoutRequest) {
		t.Errorf("给不存在的单子打款应当返回 ErrNoPayoutRequest，得到 %v", err)
	}
}

// 建单要求商户真实存在：外键挡下，并且**这次申请在账本上一笔都不留**。
//
// 这一条挡的是账本那一边的漏洞：merchant_payable:<id> 是自由拼出来的科目，
// 没有外键指着 users，所以「这个商户真的存在」只能靠 payout_requests 那一列。
// 少了它，一张挂在幽灵用户上的单子会让分成科目上多出一笔永远对不上的余额。
//
// 先给幽灵商户记一笔分成：没有它，申请会先倒在「分成余额不足」上，外键那一道
// 守卫就测不到了。那正是这道守卫要防的状态——账上已经有分成了，却没有这个人。
func TestRequestPayoutForUnknownUserIsRejected(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()
	ghost := int64(1 << 40)
	fundPayable(t, db, ghost, "1")
	txnID := ledger.NewTxnID()

	_, _, err := s.RequestPayout(ctx, RequestPayoutInput{
		TxnID: txnID, UserID: ghost, Amount: money.MustParse("1"),
		Asset: ledger.AssetCNY, Ref: "REQ-GHOST",
	})
	if err == nil {
		t.Fatal("给一个不存在的商户建单应当被拒")
	}
	if !errors.Is(err, ErrConstraintViolation) {
		t.Errorf("期望外键冲突（ErrConstraintViolation），得到 %v", err)
	}
	if got := entriesOf(t, db, txnID); len(got) != 0 {
		t.Errorf("被拒的申请写下了 %d 条分录：%+v", len(got), got)
	}
	if got := accountTotal(t, db, ledger.MerchantPayableAccount(ghost)); got != "1.00000000" {
		t.Errorf("被拒的申请动了 merchant_payable:%d，现在 %s，期望仍是记下的 1.00000000", ghost, got)
	}
	if n := payoutRows(t, db, ghost); n != 0 {
		t.Errorf("被拒的申请留下了 %d 行提现单", n)
	}
}
