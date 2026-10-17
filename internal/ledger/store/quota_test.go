package store

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"

	"github.com/sumwai/larkax/internal/keyquota"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/pipeline"
)

// 本文件是额度判定在**预留**这条路上的用例。
//
// 口径（哪一段时间的钱、哪些状态算、预留中的钱算不算）只在真库上测得出来：
// 它是 spentSQL 里那个 CASE 的语义，换成内存实现就是在测「我以为数据库会
// 怎么算」。同理，并发那一条要的是行锁真的存在。

// setQuotaLimit 把一把 key 的额度上限写成指定值。传 nil 写回 NULL。
func setQuotaLimit(ctx context.Context, t *testing.T, db *sql.DB, keyID int64, limit *string) {
	t.Helper()

	var value any
	if limit != nil {
		value = *limit
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE api_keys SET quota_limit = $2 WHERE id = $1`, keyID, value); err != nil {
		t.Fatalf("改 key 的额度上限失败: %v", err)
	}
}

// settle 把一个 requestID 对应的预留结算掉，最终金额是 amount。
//
// 走的是与生产同一条路（Save + Charge），而不是直接 UPDATE 那一行：额度算的
// 是 final_amount，而 final_amount 只有这条路写得进去。直接改库会让用例
// 测到一个真实流程到不了的状态。
//
// billID 是这次预留拿到的 billing_requests.id，原样传给分录——生产路径上
// 它一定非零（见 consumeEntries）。
func settle(ctx context.Context, t *testing.T, s *Store, req pipeline.Request, billID int64, amount string) {
	t.Helper()

	charge := &pipeline.Charge{
		RequestID: req.RequestID, UserID: req.UserID, Asset: "CNY",
		// 受理时刻是 Save 的必填项（它决定按哪个档位定价），生产路径上由
		// pipeline.Settle 填。夹具与既有的其它 Charge 字面量一致。
		PricedAt: pricedAt(),
		Entries:  consumeEntries(t, req.UserID, amount, billID),
	}
	charge.Settlement.Actual = money.MustParse(amount)
	if err := s.Save(ctx, req, charge); err != nil {
		t.Fatalf("结算失败: %v", err)
	}
}

// billingRowsOf 数一把 key 名下有几行账单。超限的预留必须**一行都不留**。
func billingRowsOf(ctx context.Context, t *testing.T, db *sql.DB, keyID int64) int {
	t.Helper()

	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM billing_requests WHERE api_key_id = $1`, keyID).Scan(&n); err != nil {
		t.Fatalf("数账单行失败: %v", err)
	}
	return n
}

// TestReservationRejectedOverKeyQuota 钉住最基本的一条：上限 100 的 key，
// 两笔各 60 的预留不能都过。
//
// 同时钉住「拒绝时不留痕迹」：超限那次不该插账单行，也不该多冻一分钱。
// 一次留下半截状态的拒绝，比一次放行更难查——账上多了一行而钱没冻，
// 对账时会表现为一笔找不平的差额。
func TestReservationRejectedOverKeyQuota(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "1000")
	keyID := seedAPIKey(ctx, t, db, userID)
	limit := "100"
	setQuotaLimit(ctx, t, db, keyID, &limit)

	first := testRequestWithKey(userID, reqID(t), keyID)
	if _, err := s.SaveReservation(ctx, first, reservation(t, userID, first.RequestID, "60", "CNY")); err != nil {
		t.Fatalf("第一笔 60（上限 100）应当放行: %v", err)
	}

	second := testRequestWithKey(userID, reqID(t), keyID)
	_, err := s.SaveReservation(ctx, second, reservation(t, userID, second.RequestID, "60", "CNY"))
	if err == nil {
		t.Fatal("第二笔 60 会让累计到 120，超过上限 100，应当被拒")
	}
	if !errors.Is(err, keyquota.ErrExceeded) {
		t.Errorf("期望 keyquota.ErrExceeded，得到 %v", err)
	}

	if n := billingRowsOf(ctx, t, db, keyID); n != 1 {
		t.Errorf("账单行 = %d，期望 1：被拒的那一笔不该留下任何行", n)
	}
	if _, frozen := walletOf(t, db, userID); frozen != "60.00000000" {
		t.Errorf("冻结 = %s，期望 60：被拒的那一笔不该冻钱", frozen)
	}
}

// TestQuotaCountsSettledFinalAmount 钉住「已结算的按 final_amount 算」。
//
// 结算之后那一行从 pending 变成 settled，计入的金额也从预留额换成最终额。
// 这一条与上一条合起来才覆盖了 spentSQL 那个 CASE 的两个分支。
func TestQuotaCountsSettledFinalAmount(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "1000")
	keyID := seedAPIKey(ctx, t, db, userID)
	limit := "100"
	setQuotaLimit(ctx, t, db, keyID, &limit)

	first := testRequestWithKey(userID, reqID(t), keyID)
	billID, reserveErr := s.SaveReservation(ctx, first, reservation(t, userID, first.RequestID, "30", "CNY"))
	if reserveErr != nil {
		t.Fatal(reserveErr)
	}
	settle(ctx, t, s, first, billID, "30")

	// 已结算 30，再要 80 —— 合计 110，超过 100。
	over := testRequestWithKey(userID, reqID(t), keyID)
	if _, err := s.SaveReservation(ctx, over, reservation(t, userID, over.RequestID, "80", "CNY")); !errors.Is(err, keyquota.ErrExceeded) {
		t.Fatalf("已结算 30 之后再要 80 应当超限，得到 %v", err)
	}

	// 恰好填满：30 + 70 = 100。
	exact := testRequestWithKey(userID, reqID(t), keyID)
	if _, err := s.SaveReservation(ctx, exact, reservation(t, userID, exact.RequestID, "70", "CNY")); err != nil {
		t.Fatalf("已结算 30 之后再要 70 恰好用满，应当放行: %v", err)
	}
}

// TestQuotaCountsPendingReservedAmount 钉住「预留中的钱算进去」。
//
// 这一条正是并发打满时能不能穿过去的关键：每一笔都还挂着 pending，若只算
// settled，则每一笔都看到 0，于是每一笔都放行。上一条与这一条必须分开写，
// 因为只测其中一条时，把 CASE 里的一个分支删掉都测不出来。
func TestQuotaCountsPendingReservedAmount(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "1000")
	keyID := seedAPIKey(ctx, t, db, userID)
	limit := "100"
	setQuotaLimit(ctx, t, db, keyID, &limit)

	// 一笔 100 的预留一直挂着不结算。
	held := testRequestWithKey(userID, reqID(t), keyID)
	if _, err := s.SaveReservation(ctx, held, reservation(t, userID, held.RequestID, "100", "CNY")); err != nil {
		t.Fatal(err)
	}

	// 再要一分钱都超。
	next := testRequestWithKey(userID, reqID(t), keyID)
	_, err := s.SaveReservation(ctx, next, reservation(t, userID, next.RequestID, "0.00000001", "CNY"))
	if !errors.Is(err, keyquota.ErrExceeded) {
		t.Fatalf("挂着一笔 100 的 pending 时再要一分钱应当超限，得到 %v", err)
	}
}

// TestQuotaIgnoresReleasedRequests 钉住「只认 settled 与 pending」。
//
// 上游失败的那一笔被释放掉，钱一分没扣。把它算进累计花费，等于让一次失败
// 的请求吃掉用户的额度——而那笔钱根本没花出去。
func TestQuotaIgnoresReleasedRequests(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "1000")
	keyID := seedAPIKey(ctx, t, db, userID)
	limit := "100"
	setQuotaLimit(ctx, t, db, keyID, &limit)

	first := testRequestWithKey(userID, reqID(t), keyID)
	if _, err := s.SaveReservation(ctx, first, reservation(t, userID, first.RequestID, "100", "CNY")); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(ctx, first, nil); err != nil {
		t.Fatalf("释放预留失败: %v", err)
	}

	// 释放之后额度应当整个回来：再要 100 仍然放行。
	second := testRequestWithKey(userID, reqID(t), keyID)
	if _, err := s.SaveReservation(ctx, second, reservation(t, userID, second.RequestID, "100", "CNY")); err != nil {
		t.Fatalf("上一笔已释放，再要 100 应当放行: %v", err)
	}
}

// TestQuotaZeroBlocksEverything 钉住「0 不是不限」。
//
// NULL 才是不限。把两者合并是这一整件事里最不能犯的错：一个是不限，
// 一个是一个都不放行。限额写 0 的运维意图是「先停掉这把 key」。
func TestQuotaZeroBlocksEverything(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "1000")
	keyID := seedAPIKey(ctx, t, db, userID)
	zero := "0"
	setQuotaLimit(ctx, t, db, keyID, &zero)

	req := testRequestWithKey(userID, reqID(t), keyID)
	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, req.RequestID, "0.00000001", "CNY")); !errors.Is(err, keyquota.ErrExceeded) {
		t.Fatalf("上限为 0 时一分钱都不该放行，得到 %v", err)
	}
}

// TestQuotaNullMeansUnlimited 钉住 NULL 那一支：没设上限的 key 不受任何限制。
//
// 这一支必须真的**不查**已花金额（见 keyquota.CheckInTx），否则每个没设
// 上限的请求都会白白多一次汇总——而绝大多数 key 都是这一种。
func TestQuotaNullMeansUnlimited(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "1000000")
	keyID := seedAPIKey(ctx, t, db, userID)
	setQuotaLimit(ctx, t, db, keyID, nil)

	req := testRequestWithKey(userID, reqID(t), keyID)
	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, req.RequestID, "999999", "CNY")); err != nil {
		t.Fatalf("没设上限的 key 应当放行: %v", err)
	}
}

// TestConcurrentReservationsCannotBothPassQuota 是这一整件事的**核心用例**：
// 同一把 key 同时打两笔，加起来超上限。
//
// 光在入口加一次检查挡不住它——两个请求都读到「已花 0」，于是都放行。挡住它
// 的是预留事务里那把 api_keys 行锁（见 keyquota 的 limitSQL）：两笔必须串行，
// 后一笔会看到前一笔已经提交的 pending 行。
//
// 用 start 闸门把两笔放出去，而不是让它们各自起跑：不设闸门时两个 goroutine
// 常常一个跑完另一个才开始，那样这条用例即使把锁去掉也照样通过——它就成了
// 一条永远为真的断言。跑多轮是为了让「恰好撞上」不至于靠运气。
func TestConcurrentReservationsCannotBothPassQuota(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	const (
		rounds   = 8
		amount   = "60"
		keyQuota = "100" // 两笔 60 合计 120，必然超
	)

	for round := range rounds {
		// 每一轮一把新 key：额度是累计的，上一轮花掉的钱会一直留在那里。
		userID := seed(t, db, "1000")
		keyID := seedAPIKey(ctx, t, db, userID)
		limit := keyQuota
		setQuotaLimit(ctx, t, db, keyID, &limit)

		// 请求与预留都在起 goroutine **之前**造好：这两个辅助函数都会调
		// t.Fatalf，而从测试 goroutine 之外调它是不允许的（见
		// testRequestWithKey 的注释）。goroutine 里只剩下被测的那一次调用。
		type attempt struct {
			req pipeline.Request
			res *pipeline.Reservation
		}
		attempts := make([]attempt, 2)
		for i := range attempts {
			req := testRequestWithKey(userID, reqID(t), keyID)
			attempts[i] = attempt{req: req, res: reservation(t, userID, req.RequestID, amount, "CNY")}
		}

		start := make(chan struct{})
		var (
			wg      sync.WaitGroup
			mu      sync.Mutex
			okCount int
			refused int
			other   error
		)
		for _, a := range attempts {
			wg.Add(1)
			go func(a attempt) {
				defer wg.Done()
				<-start
				_, err := s.SaveReservation(ctx, a.req, a.res)
				mu.Lock()
				defer mu.Unlock()
				switch {
				case err == nil:
					okCount++
				case errors.Is(err, keyquota.ErrExceeded):
					refused++
				default:
					other = err
				}
			}(a)
		}
		close(start)
		wg.Wait()

		if other != nil {
			t.Fatalf("第 %d 轮并发预留出错: %v", round, other)
		}
		if okCount != 1 {
			t.Fatalf("第 %d 轮放行 %d 笔，期望恰好 1 笔（上限 %s，两笔各 %s）",
				round, okCount, keyQuota, amount)
		}
		if refused != 1 {
			t.Fatalf("第 %d 轮拒绝 %d 笔，期望 1 笔", round, refused)
		}
		// 放行的那一笔是唯一一行账单，冻的钱也只有一笔。
		if n := billingRowsOf(ctx, t, db, keyID); n != 1 {
			t.Fatalf("第 %d 轮账单行 = %d，期望 1", round, n)
		}
	}
}

// TestCheckQuotaEntryLevel 覆盖入口那一层的只读检查（api.QuotaChecker 的实现）。
//
// 它**不是判据**（不加锁、可能被并发绕过），所以这里只钉两件事：读的是同一份
// 口径，以及 nil 上限时一次查询都不发。
func TestCheckQuotaEntryLevel(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "1000")
	keyID := seedAPIKey(ctx, t, db, userID)
	limit := "100"
	setQuotaLimit(ctx, t, db, keyID, &limit)
	limitAmount := money.MustParse(limit)

	// 一笔都没有：放行。
	if err := s.CheckQuota(ctx, keyID, &limitAmount); err != nil {
		t.Errorf("一笔都没花时应当放行: %v", err)
	}

	// 花掉 60（还挂着 pending）：仍在 100 以内，放行。
	req := testRequestWithKey(userID, reqID(t), keyID)
	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, req.RequestID, "60", "CNY")); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckQuota(ctx, keyID, &limitAmount); err != nil {
		t.Errorf("已花 60、上限 100 时应当放行: %v", err)
	}

	// 把上限下调到 50 —— 已花的 60 已经越过去了。
	lowered := "50"
	setQuotaLimit(ctx, t, db, keyID, &lowered)
	loweredAmount := money.MustParse(lowered)
	if err := s.CheckQuota(ctx, keyID, &loweredAmount); !errors.Is(err, keyquota.ErrExceeded) {
		t.Errorf("已花 60 而上限是 50，应当报超限，得到 %v", err)
	}

	// nil 上限 = 没设上限：一次查询都不发，直接放行。
	// 给一个不存在的 key id 也不该出错——出了错就说明它去查库了。
	if err := s.CheckQuota(ctx, 0, nil); err != nil {
		t.Errorf("没设上限时不该查库也不该报错，得到 %v", err)
	}
}
