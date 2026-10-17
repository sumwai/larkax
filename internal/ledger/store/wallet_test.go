package store

import (
	"context"
	"database/sql"
	"strings"
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

	var entries int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM ledger_entries WHERE account = $1`,
		ledger.UserAccount(userID).String()).Scan(&entries); err != nil {
		t.Fatalf("数分录失败: %v", err)
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
