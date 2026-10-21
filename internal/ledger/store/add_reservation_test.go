package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/pipeline"
)

// 本文件是**补冻**（AddReservation）那一组用例：流式请求不再按声明的
// max_tokens 一次冻满，而是边转发边按真实产出追加冻结额（见 #498）。
//
// 它只加输出侧那一段，与初始预留走同一条准入谓词——所以这里要钉的是三件事：
// 两处（钱包与账单行）一起加、加不上时**一分钱都不动**、以及加不上时回给
// 调用方的错误值能被网关认出来（它据此中断那条流）。

// reservedAmountOf 读一条 billing_requests 的预留金额。
//
// 它是**补冻的另一半**：钱包那一半有 walletOf，而「两处一起加」这条不变量
// 只有在两处都读得到的用例里才谈得上断言。
func reservedAmountOf(t *testing.T, db *sql.DB, requestID string) string {
	t.Helper()
	var amount string
	qerr := db.QueryRowContext(context.Background(),
		`SELECT reserved_amount FROM billing_requests WHERE request_id = $1`,
		requestID).Scan(&amount)
	if qerr != nil {
		t.Fatalf("读账单行的预留额失败: %v", qerr)
	}
	return amount
}

// freezeRequest 造一条**补冻能用的**请求。
//
// Asset 必须非空：AddReservation 按 (user_id, asset) 定位钱包。落库层自带的
// testRequest 不带它，因为 SaveReservation 读的是**预留**上那个资产；而两条路
// 的资产来源在生产里是同一个值（pipeline.Reserve 把 req.Asset 抄进预留），
// 所以这里显式补上，把「补冻读的是请求上那一份」钉在这一处。
func freezeRequest(ctx context.Context, t *testing.T, db *sql.DB, userID int64, requestID string) pipeline.Request {
	t.Helper()
	req := testRequest(ctx, t, db, userID, requestID)
	req.Asset = "CNY"
	return req
}

func mustMoney(t *testing.T, s string) money.Amount {
	t.Helper()
	amt, err := money.Parse(s)
	if err != nil {
		t.Fatalf("解析金额 %q 失败: %v", s, err)
	}
	return amt
}

// 补冻把**钱包的冻结额**与**账单行的预留额**一起加上，余额不动。
//
// 两处必须一起动：准入判据读钱包那一处，释放读账单行那一处（见 Release），
// 只加一处就会破掉对账的不变量（frozen == 所有 pending 行的 reserved_amount
// 之和），而且破得很安静——先加钱包会多出一笔没有来历的冻结，先加账单行会在
// 释放时解冻它从未冻过的钱。
func TestAddReservationAddsToBothPlaces(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := freezeRequest(ctx, t, db, userID, requestID)
	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "10", "CNY")); err != nil {
		t.Fatalf("初始预留失败: %v", err)
	}

	if err := s.AddReservation(ctx, req, mustMoney(t, "3"), 0); err != nil {
		t.Fatalf("补冻失败: %v", err)
	}

	balance, frozen := walletOf(t, db, userID)
	if balance != "100.00000000" {
		t.Errorf("余额被改动了: %s（补冻只动冻结）", balance)
	}
	if frozen != "13.00000000" {
		t.Errorf("钱包冻结 = %s，期望 13", frozen)
	}
	if got := reservedAmountOf(t, db, requestID); got != "13.00000000" {
		t.Errorf("账单行的预留额 = %s，期望 13（两处必须一起加）", got)
	}
}

// 多补几次之后，钱包的冻结额仍然等于账单行上那个数——这正是对账读的那条
// 不变量，逐段加的过程里它必须一直成立。
func TestAddReservationKeepsReconcileInvariant(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := freezeRequest(ctx, t, db, userID, requestID)
	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "1", "CNY")); err != nil {
		t.Fatal(err)
	}

	for i, delta := range []string{"2", "4", "8"} {
		if err := s.AddReservation(ctx, req, mustMoney(t, delta), 0); err != nil {
			t.Fatalf("第 %d 次补冻失败: %v", i+1, err)
		}
		if _, frozen := walletOf(t, db, userID); frozen != reservedAmountOf(t, db, requestID) {
			t.Fatalf("第 %d 次补冻之后两边不一致：钱包 %s，账单行 %s",
				i+1, frozen, reservedAmountOf(t, db, requestID))
		}
	}
}

// 补冻够不着余额时**一分钱都不动**，并回「可用余额不足」。
//
// 部分成功是最坏的结果：钱冻上了而账单行没加（释放时少解冻一笔，那笔钱永久
// 冻着），或者账单行加了而钱没冻（结算按一个不存在的冻结额去算）。所以两条
// 语句在同一个事务里，失败就整笔回滚。
func TestAddReservationRefusesToOverdraw(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	// 余额 30，先冻 25（可用 5），再补 10。
	userID := seed(t, db, "30")
	requestID := reqID(t)
	req := freezeRequest(ctx, t, db, userID, requestID)
	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "25", "CNY")); err != nil {
		t.Fatal(err)
	}

	err := s.AddReservation(ctx, req, mustMoney(t, "10"), 0)
	if err == nil {
		t.Fatal("可用余额不足应当失败")
	}
	if !errors.Is(err, ErrInsufficientAvailable) {
		t.Errorf("err = %v，期望 ErrInsufficientAvailable", err)
	}

	// 两处都没动
	balance, frozen := walletOf(t, db, userID)
	if frozen != "25.00000000" {
		t.Errorf("失败的补冻改动了冻结额: %s", frozen)
	}
	if balance != "30.00000000" {
		t.Errorf("失败的补冻改动了余额: %s", balance)
	}
	if got := reservedAmountOf(t, db, requestID); got != "25.00000000" {
		t.Errorf("失败的补冻改动了账单行的预留额: %s", got)
	}
	// 「余额不为负」这条不变量在补冻这条路上同样成立：冻结额不许超过余额。
	if frozen > balance {
		t.Errorf("冻结 %s 超过余额 %s——透支了", frozen, balance)
	}
}

// 恰好够用的那一次要成功（判据是 `>=`，不是 `>`）。
//
// 这一条钉的是边界方向：写成 `>` 时可用余额与增量正好相等的那一次补冻会被
// 拒掉，而它恰恰是最常见的形状（规则里的价与余额都是整数）。
func TestAddReservationExactAvailable(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "30")
	requestID := reqID(t)
	req := freezeRequest(ctx, t, db, userID, requestID)
	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "25", "CNY")); err != nil {
		t.Fatal(err)
	}
	if err := s.AddReservation(ctx, req, mustMoney(t, "5"), 0); err != nil {
		t.Fatalf("可用余额恰好够用应当成功: %v", err)
	}
	if _, frozen := walletOf(t, db, userID); frozen != "30.00000000" {
		t.Errorf("冻结 = %s，期望 30", frozen)
	}
}

// 能不能认出「余额不够」这件事由**值**决定：网关（internal/gateway）读的是
// pipeline 里那个哨兵，而本包把它重新导出一次。
//
// 两个同义的哨兵会让 errors.Is 只在其中一侧成立，而那一侧之外的地方读到的都是
// 「不是余额问题」——于是补冻失败被当成存储故障放行，**透支就这么发生了**。
// 所以这一条断言的是「同一个值」，不是「两个都像余额问题」。
func TestAddReservationErrorIsTheSharedSentinel(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "1")
	requestID := reqID(t)
	req := freezeRequest(ctx, t, db, userID, requestID)
	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "1", "CNY")); err != nil {
		t.Fatal(err)
	}

	err := s.AddReservation(ctx, req, mustMoney(t, "1"), 0)
	if err == nil {
		t.Fatal("余额不够应当失败")
	}
	for _, sentinel := range []error{ErrInsufficientAvailable, pipeline.ErrInsufficientAvailable} {
		if !errors.Is(err, sentinel) {
			t.Errorf("errors.Is(err, %v) 不成立：err = %v", sentinel, err)
		}
	}
	// 这里要判的就是**值相等**（同一个哨兵，不是另造一个长得一样的），
	// 所以不用 errors.Is——它连包装过的都算数，那就判不出「另造」这件事。
	//nolint:errorlint // 见上：判的是身份，不是可达性
	if ErrInsufficientAvailable != pipeline.ErrInsufficientAvailable {
		t.Error("本包导出的哨兵必须是 pipeline 里那一个值，而不是另造的一个")
	}
}

// 没有钱包行时回的是「没有钱包」，不是「余额不足」：两者的处置不同
// （前者要开户/查用户，后者要充值），混成一个会让排查方向整个错掉。
func TestAddReservationWithoutWallet(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "10")
	requestID := reqID(t)
	req := freezeRequest(ctx, t, db, userID, requestID)
	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "1", "CNY")); err != nil {
		t.Fatal(err)
	}
	// 把钱包删掉，模拟「账单行在、钱包不在」这种不该出现但必须报清楚的状态。
	if _, err := db.ExecContext(ctx, `DELETE FROM user_wallets WHERE user_id = $1`, userID); err != nil {
		t.Fatal(err)
	}

	err := s.AddReservation(ctx, req, mustMoney(t, "1"), 0)
	if err == nil {
		t.Fatal("没有钱包应当失败")
	}
	if errors.Is(err, ErrInsufficientAvailable) {
		t.Errorf("应报告「没有钱包」而不是「余额不足」: %v", err)
	}
}

// 非正的增量被拒绝，而且什么都不改。
//
// 静默当成 0 会让「补冻了但没冻上」看起来像成功：调用方据此推进它的累计量，
// 于是钱包与账单行少冻一段，没有任何东西会报出来。
func TestAddReservationRejectsNonPositiveDelta(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := freezeRequest(ctx, t, db, userID, requestID)
	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "10", "CNY")); err != nil {
		t.Fatal(err)
	}

	for _, delta := range []string{"0", "-5"} {
		if err := s.AddReservation(ctx, req, mustMoney(t, delta), 0); err == nil {
			t.Errorf("增量 %s 应被拒绝", delta)
		}
	}
	if _, frozen := walletOf(t, db, userID); frozen != "10.00000000" {
		t.Errorf("被拒的补冻改动了冻结额: %s", frozen)
	}
}

// 预留已经不在 pending 状态时不能再追加，而且整笔回滚。
//
// 这一条挡的是「同一次请求的收尾已经跑完、补冻却姗姗来迟」这个窗口：那时
// 冻结已经结算或已释放，再冻一笔就成了一笔没有解除路径的钱——账单行不会
// 再被读一次，于是它永远冻着。
func TestAddReservationAfterReleaseIsRejected(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := freezeRequest(ctx, t, db, userID, requestID)
	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "10", "CNY")); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, req, nil); err != nil {
		t.Fatalf("释放失败: %v", err)
	}
	if _, frozen := walletOf(t, db, userID); frozen != "0.00000000" {
		t.Fatalf("释放之后冻结 = %s，期望 0", frozen)
	}

	if err := s.AddReservation(ctx, req, mustMoney(t, "5"), 0); err == nil {
		t.Fatal("已经释放的预留不该还能追加冻结额")
	}
	// 钱包那一半也必须被回滚
	if _, frozen := walletOf(t, db, userID); frozen != "0.00000000" {
		t.Errorf("被拒的追加改动了钱包（冻结 = %s）——两条语句不在同一个事务里", frozen)
	}
	// 账单行上那一列**保持不变**（10，不是 15）：Release 故意不动它——它是
	// 「当时冻了多少」的历史记录，而这一行已经不在 pending（见 Release）。
	if got := reservedAmountOf(t, db, requestID); got != "10.00000000" {
		t.Errorf("被拒的追加改动了账单行（预留额 = %s，期望仍是 10）", got)
	}
}

// 请求标识对不上时更新影响 0 行，报的是「不在 pending 状态」，而不是静默
// 成功。补冻按请求标识找账单行、按请求里的用户与资产找钱包，两者对不上时
// 只有前一半会失败——所以这条错必须报出来，不能吞。
func TestAddReservationUnknownRequest(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := freezeRequest(ctx, t, db, userID, requestID)
	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "10", "CNY")); err != nil {
		t.Fatal(err)
	}

	other := req
	other.RequestID = reqID(t)
	if err := s.AddReservation(ctx, other, mustMoney(t, "1"), 0); err == nil {
		t.Fatal("没有对应账单行时应当失败")
	}
	if _, frozen := walletOf(t, db, userID); frozen != "10.00000000" {
		t.Errorf("失败的追加改动了钱包: %s", frozen)
	}
}
