package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/pipeline"
)

// 本文件给补冻那条语句本身立三条判据：
//
//   - 余额这条路只走**一次**驱动调用（拆回两次往返会红）；
//   - 同一个用户两条请求同时补冻时，钱包与账单行一起停在「装得下多少就冻多少」；
//   - 拿锁方向是「钱包 → 账单行」，与 Save（结算）一致（反了会在同一笔预留上死锁）。
//
// 三条都落在真库上：语句数、行锁与死锁检测都是数据库的行为，SQL 文本里看不出来。

// countingConnector 在 pgx 连接器外面数**驱动被调用了几次**。
//
// 判据是驱动的 QueryContext / ExecContext 调用次数，不是 SQL 文本里有几个
// 分号——把一条 CTE 拆回两条语句时，文本上仍然可以写成一句话，但驱动会多收
// 到一次调用，这一层数得出来。
type countingConnector struct {
	base driver.Connector
	n    *atomic.Int64
}

func (c countingConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &countingConn{Conn: conn, n: c.n}, nil
}

func (c countingConnector) Driver() driver.Driver { return c.base.Driver() }

// countingConn 把可选的驱动接口原样转发给 pgx 的连接。
//
// 不转发时 database/sql 会退回「Prepare + Stmt」那条路：不但调用次数对不上，
// 测到的也不是命令真正走的那条路。
type countingConn struct {
	driver.Conn
	n *atomic.Int64
}

func (c *countingConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	beginner, ok := c.Conn.(driver.ConnBeginTx)
	if !ok {
		return nil, errors.New("底层连接不支持 ConnBeginTx")
	}
	return beginner.BeginTx(ctx, opts)
}

func (c *countingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.n.Add(1)
	queryer, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		return nil, errors.New("底层连接不支持 QueryerContext")
	}
	return queryer.QueryContext(ctx, query, args)
}

func (c *countingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.n.Add(1)
	execer, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, errors.New("底层连接不支持 ExecerContext")
	}
	return execer.ExecContext(ctx, query, args)
}

// countingDB 连上本包的测试库，返回一个会数驱动调用的句柄。
//
// DSN 由 TestMain 换成了这个包自己的库（见 internal/testdb），所以这里直接读；
// 没设 DSN 时与 testDB 一样跳过。池的大小跟 testdb.Open 一致：本机默认
// `max_connections` 就那么多，开一条不受限的池会把别的包挤掉。
func countingDB(t *testing.T) (*sql.DB, *atomic.Int64) {
	t.Helper()

	dsn := os.Getenv("LARKAX_DB_DSN")
	if dsn == "" {
		t.Skip("未设置 LARKAX_DB_DSN，跳过集成测试（CI 里会跑）")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("解析测试库 DSN 失败: %v", err)
	}
	n := new(atomic.Int64)
	db := sql.OpenDB(countingConnector{base: stdlib.GetConnector(*cfg), n: n})
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db, n
}

// TestAddReservationIsOneStatement 钉住「余额那条路只有一次驱动调用」。
//
// 把 CTE 拆回「钱包 UPDATE + 账单行 UPDATE」两条语句时，这个计数变成 2，
// 用例红——这正是 #996 要压掉的那几次往返。判据不看 SQL 文本，只看驱动
// 实际收到了几次调用；BEGIN / COMMIT 不计在内（它们不是这条语句的往返）。
func TestAddReservationIsOneStatement(t *testing.T) {
	db, calls := countingDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := freezeRequest(ctx, t, db, userID, requestID)
	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "10", "CNY")); err != nil {
		t.Fatalf("初始预留失败: %v", err)
	}

	// 初始预留自己也走驱动；从这里开始只数补冻那一次。
	calls.Store(0)
	if err := s.AddReservation(ctx, req, mustMoney(t, "3"), 0); err != nil {
		t.Fatalf("补冻失败: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("AddReservation 走了 %d 次驱动调用，期望 1 次：压成一条 CTE 之后"+
			"「锁钱包、改冻结额、改账单行」应当在同一次往返里完成", got)
	}

	// 正向证据：这一条真的动了数据，不是「什么都没做所以只有一次调用」。
	if _, frozen := walletOf(t, db, userID); frozen != "13.00000000" {
		t.Errorf("冻结 = %s，期望 13", frozen)
	}
	if got := reservedAmountOf(t, db, requestID); got != "13.00000000" {
		t.Errorf("账单行预留 = %s，期望 13", got)
	}
}

// TestAddReservationConcurrentFreezesDoNotOverdraw 让两条请求同时补冻，判据是
// 「装得下多少就冻多少」：钱包的 frozen 与两行 reserved_amount 之和相等，
// 且没有一边多冻。
//
// 余额 20，两笔各预留 8 之后可用 4；两条各补 3 —— 只够一条。先读后写会在
// 两条之间开出窗口：两边都看到「够」于是都加，frozen 超过余额；或者后写的
// 覆盖先写的，frozen 与两行预留之和对不上。两种都会让这条用例红。
func TestAddReservationConcurrentFreezesDoNotOverdraw(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "20")
	reqA := freezeRequest(ctx, t, db, userID, reqID(t))
	reqB := freezeRequest(ctx, t, db, userID, reqID(t))
	for i, req := range []pipeline.Request{reqA, reqB} {
		if _, err := s.SaveReservation(ctx, req, reservation(t, userID, req.RequestID, "8", "CNY")); err != nil {
			t.Fatalf("第 %d 笔初始预留失败: %v", i+1, err)
		}
	}
	if _, frozen := walletOf(t, db, userID); frozen != "16.00000000" {
		t.Fatalf("初始冻结 = %s，期望 16", frozen)
	}

	delta := mustMoney(t, "3")
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, req := range []pipeline.Request{reqA, reqB} {
		go func(r pipeline.Request) {
			<-start
			results <- s.AddReservation(ctx, r, delta, 0)
		}(req)
	}
	close(start)

	var succeeded, refused int
	for i := 0; i < 2; i++ {
		err := <-results
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrInsufficientAvailable):
			refused++
		default:
			t.Fatalf("第 %d 条补冻报了意料之外的错: %v", i+1, err)
		}
	}
	if succeeded != 1 || refused != 1 {
		t.Fatalf("两条并发补冻的结果 = %d 成功 / %d 余额不足，期望恰好各一条", succeeded, refused)
	}

	_, frozen := walletOf(t, db, userID)
	var reserved string
	if err := db.QueryRowContext(ctx, `
		SELECT SUM(reserved_amount)::numeric(20, 8)::text
		  FROM billing_requests
		 WHERE request_id = ANY($1)`, []string{reqA.RequestID, reqB.RequestID}).Scan(&reserved); err != nil {
		t.Fatalf("读两行预留之和失败: %v", err)
	}
	if frozen != "19.00000000" {
		t.Errorf("并发补冻之后冻结 = %s，期望 19（只该成功一条）", frozen)
	}
	if reserved != frozen {
		t.Errorf("两行预留之和 = %s，钱包冻结 = %s——对账不变量被破坏", reserved, frozen)
	}
}

// TestAddReservationLocksWalletBeforeBillingRow 判「钱包先、账单行后」这个方向。
//
// 另起一个事务按 Save（结算）的顺序先拿住钱包行，再等补冻真的卡在那把锁上，
// 然后去拿账单行。方向对了这一步立刻拿到；方向反了（补冻先拿账单行）两边
// 互等，PostgreSQL 的死锁检测会把其中一边以 40P01 掐掉，下面的断言红。
//
// 不用「起两个 goroutine 跑真 Save」：Save 从锁钱包到锁账单行之间没有可以插
// 同步点的语句，靠睡眠去撞窗口只会得到一条时灵时不灵的用例。这里直接把那个
// 中间状态造出来。
func TestAddReservationLocksWalletBeforeBillingRow(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := freezeRequest(ctx, t, db, userID, requestID)
	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "10", "CNY")); err != nil {
		t.Fatalf("初始预留失败: %v", err)
	}

	hog, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("开启另一个事务失败: %v", err)
	}
	defer func() { _ = hog.Rollback() }()
	var one int
	if err := hog.QueryRowContext(ctx, `
		SELECT 1 FROM user_wallets WHERE user_id = $1 AND asset = $2 FOR UPDATE`,
		userID, "CNY").Scan(&one); err != nil {
		t.Fatalf("先锁钱包失败: %v", err)
	}

	runCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	delta := mustMoney(t, "1")
	go func() { done <- s.AddReservation(runCtx, req, delta, 0) }()

	// 等补冻真的停在钱包锁上：那说明它已经走到 frozen，没有先拿账单行。
	waitUntilBlocked(t, db, 1)

	// 这一步对应 Save 的第二步。方向对了，账单行还没人拿；反了就会互等。
	if err := hog.QueryRowContext(ctx, `
		SELECT 1 FROM billing_requests WHERE request_id = $1 FOR UPDATE`,
		requestID).Scan(&one); err != nil {
		t.Fatalf("先钱包后账单行的事务在账单行上被挡住了——补冻的锁序反了: %v", err)
	}
	if err := hog.Rollback(); err != nil {
		t.Fatalf("释放另一个事务失败: %v", err)
	}

	if err := <-done; err != nil {
		t.Fatalf("补冻失败: %v", err)
	}
	if _, frozen := walletOf(t, db, userID); frozen != "11.00000000" {
		t.Errorf("冻结 = %s，期望 11", frozen)
	}
	if got := reservedAmountOf(t, db, requestID); got != "11.00000000" {
		t.Errorf("账单行预留 = %s，期望 11", got)
	}
}

// TestAddReservationAndSaveOnSameRequestDoNotDeadlock 让补冻与结算在同一笔
// 预留上同时跑（#996 的用户可见症状：两个方向相遇时不出现死锁）。
//
// 这条用例只是**烟雾**：两个 goroutine 的相遇时机由调度决定，方向错了也不
// 一定每次都撞上。真正判定方向的是上面那条（它把 Save 的中间状态直接造出
// 来），这里补的是「真家伙跑起来也不出 40P01、账仍然是平的」。
func TestAddReservationAndSaveOnSameRequestDoNotDeadlock(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := freezeRequest(ctx, t, db, userID, requestID)
	billID, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "10", "CNY"))
	if err != nil {
		t.Fatalf("初始预留失败: %v", err)
	}

	charge := &pipeline.Charge{
		RequestID: requestID, UserID: userID, Asset: "CNY",
		PricedAt: pricedAt(),
		Entries:  consumeEntries(t, userID, "6", billID),
	}
	charge.Settlement.Actual = money.MustParse("6")

	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	delta := mustMoney(t, "5")
	start := make(chan struct{})
	freezeErr := make(chan error, 1)
	saveErr := make(chan error, 1)
	go func() {
		<-start
		freezeErr <- s.AddReservation(runCtx, req, delta, 0)
	}()
	go func() {
		<-start
		saveErr <- s.Save(runCtx, req, charge)
	}()
	close(start)

	// 结算一定会成功：这一笔请求只在补冻与它之间竞争。
	if err := <-saveErr; err != nil {
		if isDeadlock(err) {
			t.Fatalf("补冻与结算互相等锁，被数据库判成死锁: %v", err)
		}
		t.Fatalf("结算失败: %v", err)
	}
	// 补冻要么赶在结算之前成功，要么撞上「已经不在 pending」——两种都是对的。
	if err := <-freezeErr; err != nil {
		if isDeadlock(err) {
			t.Fatalf("补冻与结算互相等锁，被数据库判成死锁: %v", err)
		}
		if !strings.Contains(err.Error(), "不在 pending 状态") {
			t.Fatalf("补冻报了意料之外的错: %v", err)
		}
	}

	// 结算之后冻结额必须归零：补冻若抢在前面，它冻的那笔也在同一次结算里
	// 释放掉了；抢在后面则整笔回滚。留着不动的冻结就是一笔再也解不开的钱。
	balance, frozen := walletOf(t, db, userID)
	if frozen != "0.00000000" {
		t.Errorf("结算之后冻结 = %s，期望 0", frozen)
	}
	if balance != "94.00000000" {
		t.Errorf("余额 = %s，期望 94（100 − 6）", balance)
	}
	if got, want := balance, ledgerBalance(t, db, userID).String(); got != want {
		t.Errorf("钱包 %s 与账本 %s 不一致", got, want)
	}
	// 补冻成功时账单行会停在 15，没赶上则停在 10——两种都合法，别的都不行。
	if got := reservedAmountOf(t, db, requestID); got != "10.00000000" && got != "15.00000000" {
		t.Errorf("账单行预留 = %s，期望 10（补冻没赶上）或 15（赶上了）", got)
	}
}

// TestAddReservationRejectedWhenSettlementWinsTheRowLock 钉住 `billedRows == 0`
// 那半条判据：`target` 的快照里这一行还在 pending，等账单行锁的时候被结算了。
//
// 这是 READ COMMITTED 下唯一会走到那里的路径，靠并发去撞是撞不稳的，所以
// 把中间状态人为造出来：另一个事务先拿住账单行，补冻因此卡在 `billed` 上
// （此时 `frozen` 已经把钱加了），那个事务再把这一行置成 settled 并提交。
// 补冻醒来时 UPDATE 重判谓词（EPQ）看到的不再是 pending，影响 0 行。
//
// 断言两件事：报「不在 pending」，**并且**钱包那一半退回去了——后者同时钉住
// 事务回滚与 EPQ 重判：少了任何一个，frozen 都会多出这一笔再也解不开的钱。
func TestAddReservationRejectedWhenSettlementWinsTheRowLock(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := freezeRequest(ctx, t, db, userID, requestID)
	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "10", "CNY")); err != nil {
		t.Fatalf("初始预留失败: %v", err)
	}

	hog, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("开启另一个事务失败: %v", err)
	}
	defer func() { _ = hog.Rollback() }()
	var one int
	err = hog.QueryRowContext(ctx, `
		SELECT 1 FROM billing_requests WHERE request_id = $1 FOR UPDATE`,
		requestID).Scan(&one)
	if err != nil {
		t.Fatalf("先锁账单行失败: %v", err)
	}

	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	delta := mustMoney(t, "2")
	done := make(chan error, 1)
	go func() { done <- s.AddReservation(runCtx, req, delta, 0) }()
	// 卡在账单行上 = 钱包那一半已经加了，账单行那一半被挡住。
	waitUntilBlocked(t, db, 1)

	_, err = hog.ExecContext(ctx,
		`UPDATE billing_requests SET status = 'settled', settled_at = now() WHERE request_id = $1`,
		requestID)
	if err != nil {
		t.Fatalf("把这一行结算掉失败: %v", err)
	}
	if err = hog.Commit(); err != nil {
		t.Fatalf("提交结算失败: %v", err)
	}

	err = <-done
	if err == nil {
		t.Fatal("这一行已经被结算，补冻应当失败")
	}
	if !strings.Contains(err.Error(), "不在 pending 状态") {
		t.Fatalf("err = %v，期望「不在 pending 状态」", err)
	}
	// EPQ 重判 + 回滚：钱包那一半必须原样退回。
	if _, frozen := walletOf(t, db, userID); frozen != "10.00000000" {
		t.Errorf("被拒的补冻把冻结额留在了钱包上: %s，期望 10", frozen)
	}
	if got := reservedAmountOf(t, db, requestID); got != "10.00000000" {
		t.Errorf("账单行预留 = %s，期望 10（这次补冻一分都没加上）", got)
	}
}

// TestAddReservationRejectsEmptyAsset 钉住空资产的报错。
//
// 这条校验原先在 `lockWallets` 里（它逐个校验资产非空）。压成一条语句之后
// 不再经过那把锁，校验挪到了 `frozenRows == 0` 那一支：不挪的话，空资产会
// 掉进 `diagnoseFreezeFailure`，被报成「没有对应的钱包」——那是在说一件
// 不一样的事（用户该开户），会把排查方向带偏。
//
// 位置也钉住：放在额度包那一支**之后**。额度包那条路从来不碰钱包，空资产在
// 它那里不算错（改动前也是如此：`lockWallets` 在额度包分支返回之后才调用）。
func TestAddReservationRejectsEmptyAsset(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := freezeRequest(ctx, t, db, userID, requestID)
	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "10", "CNY")); err != nil {
		t.Fatalf("初始预留失败: %v", err)
	}

	empty := req
	empty.Asset = ""
	err := s.AddReservation(ctx, empty, mustMoney(t, "1"), 0)
	if err == nil {
		t.Fatal("空资产应当被拒绝")
	}
	if !strings.Contains(err.Error(), "钱包的资产品种为空") {
		t.Errorf("err = %v，期望「钱包的资产品种为空」", err)
	}
	if errors.Is(err, ErrNoWallet) {
		t.Error("空资产被报成了「没有钱包」——这是两件事，处置也不同")
	}
	if _, frozen := walletOf(t, db, userID); frozen != "10.00000000" {
		t.Errorf("被拒的补冻改动了冻结额: %s", frozen)
	}
}

// TestAddReservationTwoRefreezesOnSameRequestKeepBothHalvesInStep 让**同一笔**
// 预留上两条补冻同时跑，两条都该成功（余额够）。
//
// 它们是排在同一把钱包行锁上的：后到的那条在 EPQ 重算时，`billed` 里那句
// `EXISTS (SELECT 1 FROM frozen)` 必须仍然为真——只加钱包不加账单行的话，
// 两半就分叉了，而分叉出来的冻结额再也解不开。
//
// 用另起事务先占住钱包行来制造确定的重算：两条都真的停在锁上之后才放行，
// 所以「后到的那条走了重算」不是靠调度撞出来的。
func TestAddReservationTwoRefreezesOnSameRequestKeepBothHalvesInStep(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := freezeRequest(ctx, t, db, userID, requestID)
	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "10", "CNY")); err != nil {
		t.Fatalf("初始预留失败: %v", err)
	}

	hog, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("开启另一个事务失败: %v", err)
	}
	defer func() { _ = hog.Rollback() }()
	var one int
	if err := hog.QueryRowContext(ctx, `
		SELECT 1 FROM user_wallets WHERE user_id = $1 AND asset = $2 FOR UPDATE`,
		userID, "CNY").Scan(&one); err != nil {
		t.Fatalf("先锁钱包失败: %v", err)
	}

	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	delta := mustMoney(t, "3")
	start := make(chan struct{})
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			results <- s.AddReservation(runCtx, req, delta, 0)
		}()
	}
	close(start)
	waitUntilBlocked(t, db, 2)

	if err := hog.Rollback(); err != nil {
		t.Fatalf("释放钱包失败: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatalf("第 %d 条补冻失败: %v", i+1, err)
		}
	}

	_, frozen := walletOf(t, db, userID)
	if frozen != "16.00000000" {
		t.Errorf("冻结 = %s，期望 16（10 + 3 + 3）", frozen)
	}
	if got := reservedAmountOf(t, db, requestID); got != frozen {
		t.Errorf("账单行预留 = %s，钱包冻结 = %s——两半分叉了", got, frozen)
	}
}

// waitUntilBlocked 等到本包之外有 n 个后端停在锁上。
//
// 用例串行跑，所以那几条只可能是刚起的补冻 goroutine。比固定睡一段可靠：
// 睡得短了会在它拿到锁之前就去拿账单行，睡得长了只是慢。
//
// **本包不要加 t.Parallel**：判据是「库里有几个后端停在锁上」，并行跑的用例
// 会让别的用例的等待也算进来。今天包内全串行，这条成立。
func waitUntilBlocked(t *testing.T, db *sql.DB, n int) {
	t.Helper()

	// 轮询本身也要有上限：连接池被占满时 `QueryRowContext` 会**卡在拿连接**上，
	// 不报错也不返回。用 Background 时那就成了整包超时，而这里只想在有问题的
	// 时候直接说清楚。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var blocked int
		if err := db.QueryRowContext(ctx, `
			SELECT count(*) FROM pg_stat_activity
			 WHERE datname = current_database()
			   AND wait_event_type = 'Lock'
			   AND pid <> pg_backend_pid()`).Scan(&blocked); err != nil {
			t.Fatalf("查等待中的后端失败: %v", err)
		}
		if blocked >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等锁的后端一直没有达到 %d 个", n)
}
