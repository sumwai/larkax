package auth

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// 重试这一段的判据分两处：不是死锁的错误一次都不重跑；是死锁时重跑，但次数
// 有上限。两处各来一条用例——写在同一条里时，失败只说明「某处不对」，分不出
// 是哪一处。

// deadlockError 造一个 PostgreSQL 的 40P01（deadlock_detected）。
func deadlockError() error {
	return &pgconn.PgError{Code: "40P01"}
}

// TestIsDeadlock 把「什么算死锁」钉在 SQLSTATE 上。
//
// 只认 40P01：序列化失败（40001）与唯一键冲突（23505）都不是死锁。
// 放宽成「任何 PgError」时，一次唯一键冲突会被重跑两遍，而它每一遍都以
// 同样的方式失败，只是把一次失败变慢了。
func TestIsDeadlock(t *testing.T) {
	if !isDeadlock(deadlockError()) {
		t.Error("40P01 应当算死锁")
	}
	if !isDeadlock(fmt.Errorf("包一层: %w", deadlockError())) {
		t.Error("包在错误链里的 40P01 应当算死锁")
	}
	if isDeadlock(&pgconn.PgError{Code: "40001"}) {
		t.Error("40001（序列化失败）不是死锁")
	}
	if isDeadlock(&pgconn.PgError{Code: "23505"}) {
		t.Error("23505（唯一键冲突）不是死锁")
	}
	if isDeadlock(errors.New("普通错误")) {
		t.Error("非 PgError 不是死锁")
	}
	if isDeadlock(nil) {
		t.Error("nil 不是死锁")
	}
}

// TestRetryDeletionGivesUpAfterTheConfiguredAttempts 钉住重跑的次数上限。
//
// 撞上死锁时整笔事务重跑，上限是 2 次，连第一次一共调用 3 次。用例让第 4 次
// 调用「本来会成功」：前三次返回死锁，第 4 次返回一个普通错误。上限生效时
// 第 4 次不会被调用，函数带着最后一次的死锁错误返回。
//
// 这样造而不是让每一次都返回死锁：全死锁时把 attempt++ 改成 attempt--
// 会让循环永远不终止（attempt 一路减下去，条件恒真），那种改法只能靠超时
// 被看见，而超时与「用例没能区分两版」在结果表上是两件事。第 4 次返回一个
// 能终止循环的错误之后，两版都跑得完，差别只剩「调用了几次」。
func TestRetryDeletionGivesUpAfterTheConfiguredAttempts(t *testing.T) {
	sentinel := errors.New("第 4 次本来会成功")
	calls := 0
	err := retryDeletion(func() error {
		calls++
		if calls <= 3 {
			return deadlockError()
		}
		return sentinel
	})

	if calls != 3 {
		t.Errorf("run 被调用了 %d 次，期望 3 次（第一次加 2 次重跑）", calls)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "40P01" {
		t.Errorf("次数用尽时应当带着最后一次的死锁错误返回，得到 %v", err)
	}
	if errors.Is(err, sentinel) {
		t.Error("第 4 次调用不该发生：上限是 2 次重跑")
	}
	if !strings.Contains(err.Error(), "已重跑 2 次") {
		t.Errorf("错误里应当点明重跑了 2 次：%v", err)
	}
}

// TestRetryDeletionDoesNotRetryOtherErrors 钉住「只重跑死锁」。
//
// run 每次返回同一个普通错误：原版调用一次就原样返回，不包「反复撞上死锁」
// 那层说明，也不再多调两次。
func TestRetryDeletionDoesNotRetryOtherErrors(t *testing.T) {
	sentinel := errors.New("普通错误")
	calls := 0
	err := retryDeletion(func() error {
		calls++
		return sentinel
	})

	if calls != 1 {
		t.Errorf("run 被调用了 %d 次，非死锁错误只该调 1 次", calls)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("错误链里应当有原错误，得到 %v", err)
	}
	if err.Error() != sentinel.Error() {
		t.Errorf("错误被包了一层：%v", err)
	}
}
