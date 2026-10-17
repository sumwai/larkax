package store

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"

	"github.com/sumwai/larkax/internal/ledger"
)

// seedBareUser 只建 users 那一行，不建钱包、不写分录。
//
// 与 helpers_test.go 的 seed 分开：那个建的是「已经充过值的老用户」，
// 而开户用例要的正是**还没有钱包行**的用户。拿 seed 来测等于在一个
// 钱包已经存在的世界里测开户，最要紧的那条路径反而没走到。
func seedBareUser(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	id := 200000 + nextID()
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO users (id, username, email) VALUES ($1, $2, $3)`,
		id, "w"+itoa(id), "w"+itoa(id)+"@example.com"); err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	return id
}

// 开户落下的钱包：余额与冻结都是 0，且**账本里没有任何分录**。
//
// 最后一条是这个用例的重点：0 余额的户不需要一条金额为零的分录来「留痕」
// ——账本拒绝零金额分录，而对账要拿「分录之和」与快照比。这里没有分录，
// 所以权威值与快照值都是 0，两者自洽。
func TestOpenWalletCreatesZeroBalanceWallet(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()
	userID := seedBareUser(t, db)

	created, err := s.OpenWallet(ctx, userID, ledger.AssetCNY)
	if err != nil {
		t.Fatalf("开户失败: %v", err)
	}
	if !created {
		t.Fatal("第一次开户应当返回 created=true")
	}

	balance, frozen := walletOf(t, db, userID)
	if balance != "0.00000000" || frozen != "0.00000000" {
		t.Errorf("余额=%s 冻结=%s，期望都是 0", balance, frozen)
	}

	// 变量名不复用 err：上面那个 err 还在作用域里，这里再 `err :=` 是遮蔽，
	// govet 的 shadow 检查器会报。
	var entries int
	if countErr := db.QueryRowContext(ctx,
		`SELECT count(*) FROM ledger_entries WHERE account = $1`,
		ledger.UserAccount(userID).String()).Scan(&entries); countErr != nil {
		t.Fatalf("数分录失败: %v", countErr)
	}
	if entries != 0 {
		t.Errorf("开户写下了 %d 条分录，期望 0 条", entries)
	}

	// 权威值（分录之和）也是 0：快照与账本因此对得上，不需要对账去修。
	authoritative, err := s.AccountBalance(ctx, ledger.UserAccount(userID), ledger.AssetCNY)
	if err != nil {
		t.Fatalf("读权威余额失败: %v", err)
	}
	if !authoritative.IsZero() {
		t.Errorf("账本侧余额 = %s，期望 0", authoritative)
	}
}

// 对同一个用户开两次户：第二次什么都不做，余额一分不动。
func TestOpenWalletIsIdempotent(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()
	userID := seedBareUser(t, db)

	if created, err := s.OpenWallet(ctx, userID, ledger.AssetCNY); err != nil || !created {
		t.Fatalf("第一次开户: created=%v err=%v", created, err)
	}
	created, err := s.OpenWallet(ctx, userID, ledger.AssetCNY)
	if err != nil {
		t.Fatalf("第二次开户不该报错: %v", err)
	}
	if created {
		t.Error("第二次开户返回 created=true，期望 false（已存在）")
	}

	var rows int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM user_wallets WHERE user_id = $1`, userID).Scan(&rows); err != nil {
		t.Fatalf("数钱包行失败: %v", err)
	}
	if rows != 1 {
		t.Errorf("钱包行数 = %d，期望 1", rows)
	}
}

// 并发开户只有一行落库，且恰好一个调用拿到 created=true。
//
// 这条钉的是 OpenWallet 注释里那句「并发由主键 (user_id, asset) 兜底」——
// 把 `ON CONFLICT DO NOTHING` 换成「先查再插」，几个 goroutine 会各自看到
// 「不存在」而都去插，后到的撞唯一键报错，这条用例立刻红。串行的
// 「开两次」用例照不到这一点（见 TestOpenWalletIsIdempotent）。
//
// 要让这条用例真的钉住东西，靠的是三件事：**预热**、闸门、够宽的并发数。
// 前两件少一件，它就变成「看着通过、其实一条都没钉住」——四种组合都实测过，
// 跑的都是「先查再插」那版实现，各三轮：
//
//	预热 + 闸门    → 三轮全红（下面用的就是这一种）
//	预热、无闸门   → 三轮全红
//	无预热 + 闸门  → 三轮全绿（一条都没钉住）
//	无预热、无闸门 → 两绿一红（靠运气）
//
// 决定性的是**预热**：database/sql 懒建连接，而 MaxIdleConns 默认只有 2。
// 池子里没有现成连接时，先拿到连接的那一两个把行插完，其余几个还在建连接
// ——它们看到的都是「已经存在」，竞态窗口一次都撞不上。闸门只是把「同时出发」
// 写进用例里，不依赖调度顺序；没有预热时它反而最坏，goroutine 排好队一个
// 接一个跑完。
//
// 并发数也是同一件事：n=8 时窗口太窄，换成「先查再插」十轮里能漏一轮；n=16
// 四十轮里三十九轮红。漏的那一轮是竞态用例的固有性质——判据是「窗口真的被
// 撞上」，而这要调度恰好把几个 goroutine 叠在一起，不是每次都能。反过来是
// 确定的：真实实现（ON CONFLICT）跑多少轮都绿，所以这条用例不会误报。
//
// 所以下面先把池子开到 n 条并预热，再放闸门。
func TestOpenWalletConcurrent(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()
	userID := seedBareUser(t, db)

	const n = 16
	db.SetMaxOpenConns(n)
	db.SetMaxIdleConns(n)
	prewarmPool(t, db, n)

	start := make(chan struct{})
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		created  int
		existed  int
		firstErr error
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ok, err := s.OpenWallet(ctx, userID, ledger.AssetCNY)

			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				if firstErr == nil {
					firstErr = err
				}
			case ok:
				created++
			default:
				existed++
			}
		}()
	}
	close(start)
	wg.Wait()

	if firstErr != nil {
		t.Fatalf("并发开户出错: %v", firstErr)
	}
	if created != 1 {
		t.Errorf("created=true 的有 %d 个，期望恰好 1 个", created)
	}
	if existed != n-1 {
		t.Errorf("created=false 的有 %d 个，期望 %d 个", existed, n-1)
	}

	var rows int
	if countErr := db.QueryRowContext(ctx,
		`SELECT count(*) FROM user_wallets WHERE user_id = $1`, userID).Scan(&rows); countErr != nil {
		t.Fatalf("数钱包行失败: %v", countErr)
	}
	if rows != 1 {
		t.Errorf("钱包行数 = %d，期望 1", rows)
	}
}

// prewarmPool 让连接池里先有 n 条打开的连接，用完放回池子。
//
// 目的是把「建连接」这段耗时移出并发用例的竞态窗口：调用方与库都在本机，
// 一次查询是几十微秒，而建一条连接是它的若干倍——不预热的话，谁先拿到连接
// 谁就把整件事做完了，剩下的连窗口都进不去（见 TestOpenWalletConcurrent）。
//
// 拿 n 条连接要真的**同时**持有再一起放回：一条一条拿、拿完就还，池子只会
// 复用同一条，预热不到 n 条。
func prewarmPool(t *testing.T, db *sql.DB, n int) {
	t.Helper()
	ctx := context.Background()

	conns := make([]*sql.Conn, 0, n)
	for i := 0; i < n; i++ {
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("预热连接池失败（第 %d 条）: %v", i+1, err)
		}
		conns = append(conns, conn)
	}
	for _, conn := range conns {
		if err := conn.Close(); err != nil {
			t.Fatalf("归还预热的连接失败: %v", err)
		}
	}
}

// 已经有钱包（余额非 0）的用户：开户是一个空操作，不重置余额。
//
// 这条比「开两次」更关键：把已存在的行重建成 0 会让用户的钱凭空消失，
// 而命令照样退出 0。
func TestOpenWalletKeepsExistingBalance(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()
	userID := seed(t, db, "100")

	created, err := s.OpenWallet(ctx, userID, ledger.AssetCNY)
	if err != nil {
		t.Fatalf("开户失败: %v", err)
	}
	if created {
		t.Error("已经有钱包行，不该报告「刚建出来」")
	}

	balance, frozen := walletOf(t, db, userID)
	if balance != "100.00000000" {
		t.Errorf("余额变成 %s，期望保持 100", balance)
	}
	if frozen != "0.00000000" {
		t.Errorf("冻结额 = %s，期望 0", frozen)
	}
}

func TestOpenWalletRejectsUnsupportedAsset(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()
	userID := seedBareUser(t, db)

	if _, err := s.OpenWallet(ctx, userID, ledger.Asset("USDT")); err == nil {
		t.Fatal("不受支持的资产应当报错")
	} else if !strings.Contains(err.Error(), "不受支持") {
		t.Errorf("错误信息 %q 里没有「不受支持」", err)
	}

	var rows int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM user_wallets WHERE user_id = $1`, userID).Scan(&rows); err != nil {
		t.Fatalf("数钱包行失败: %v", err)
	}
	if rows != 0 {
		t.Errorf("被拒的开户留下了 %d 行钱包", rows)
	}
}

func TestOpenWalletRejectsNonPositiveUser(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	for _, id := range []int64{0, -1} {
		if _, err := s.OpenWallet(ctx, id, ledger.AssetCNY); err == nil {
			t.Errorf("用户 id %d 应当被拒", id)
		}
	}
}

// 用户不存在时，错误要指认「哪个用户不存在」，而不是一句外键冲突原文。
//
// 命令行的正常路径会先解析用户、走不到这里；这条覆盖的是绕过解析的调用方
// 与「解析之后用户被删」那个窗口。
func TestOpenWalletUnknownUser(t *testing.T) {
	db := testDB(t)
	s := New(db)

	_, err := s.OpenWallet(context.Background(), 900000000000000, ledger.AssetCNY)
	if err == nil {
		t.Fatal("不存在的用户应当报错")
	}
	if !strings.Contains(err.Error(), "不存在") {
		t.Errorf("错误信息 %q 里没有说清用户不存在", err)
	}
}
