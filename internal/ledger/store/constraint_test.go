package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// 这一组用例钉的是 ErrConstraintViolation 这条**兜底**判据，以及它与
// #391 那个具名哨兵的关系。
//
// 具名哨兵的边界由 wallet_missing_user_test.go 钉住；这里的重点相反：
// 没被具名认出来的外键、以及唯一键，要能被同一个通用哨兵认出来，而
// CHECK 违反与非数据库错误不能。

// TestIsConstraintViolation 逐个改驱动错误的字段，看判据在哪一步翻面。
//
// 手搓 PgError 而不是去真库上造：真库造不出「同一个错误码、不同约束名」
// 之外的各种组合，而这里要验的是**只看错误码**这条判据本身。
func TestIsConstraintViolation(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "外键冲突",
			err:  &pgconn.PgError{Code: pgForeignKeyViolation, ConstraintName: "billing_requests_channel_id_fkey"},
			want: true,
		},
		{
			name: "唯一键冲突",
			err:  &pgconn.PgError{Code: pgUniqueViolation, ConstraintName: "billing_requests_request_id_key"},
			want: true,
		},
		{
			name: "CHECK 违反不是这一类",
			err:  &pgconn.PgError{Code: pgCheckViolation, ConstraintName: walletNonNegativeConstraint},
			want: false,
		},
		{
			name: "包了一层驱动错误",
			err: fmt.Errorf("写预留记录失败: %w",
				&pgconn.PgError{Code: pgForeignKeyViolation, ConstraintName: "billing_requests_channel_id_fkey"}),
			want: true,
		},
		{
			name: "不是 PostgreSQL 错误",
			err:  errors.New("connection reset by peer"),
			want: false,
		},
		{
			name: "完全没有错误",
			err:  nil,
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isConstraintViolation(tc.err); got != tc.want {
				t.Errorf("isConstraintViolation(%v) = %v，期望 %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestConstraintFailureWrapsOnlyConstraints 钉住「认不出来时一个字都不加」。
//
// 这个性质让它可以无差别地套在每一条写语句的错误上：非约束错误必须原样
// 返回（同一个错误值），否则调用方原有的分类会被这层兜底搅乱。
func TestConstraintFailureWrapsOnlyConstraints(t *testing.T) {
	plain := errors.New("连接断了")
	// 比的是**同一个值**，不是同一段文本：写成 `got.Error() != plain.Error()`
	// 的话，把这一层改成 `fmt.Errorf("%v", err)` 照样绿，而那样会把
	// SaveReservation 里 ErrClaimed 的透传打断（见 store.go 里
	// constraintFailure(ClaimInTx 的错误) 那一处）。
	//
	//nolint:errorlint // 这里要的正是**身份**而不是 errors.Is：包一层 `%w` 也能过
	// errors.Is，而「原样返回」这个性质要求连包装都不许加。
	if got := constraintFailure(plain); got != plain {
		t.Errorf("非约束错误应当原样返回同一个值，得到 %v", got)
	}
	if got := constraintFailure(nil); got != nil {
		t.Errorf("nil 应当原样返回，得到 %v", got)
	}

	pgErr := &pgconn.PgError{
		Code:           pgForeignKeyViolation,
		ConstraintName: "billing_requests_channel_id_fkey",
	}
	err := constraintFailure(pgErr)
	if !errors.Is(err, ErrConstraintViolation) {
		t.Errorf("错误链里没有 ErrConstraintViolation：%v", err)
	}
	// 表名与约束名要能从链里取回来——这一层只加分类，不替掉现场依据。
	var got *pgconn.PgError
	if !errors.As(err, &got) {
		t.Fatalf("错误链里没有驱动的原始错误：%v", err)
	}
	if got.ConstraintName != "billing_requests_channel_id_fkey" {
		t.Errorf("约束名 = %q，期望 %q", got.ConstraintName, "billing_requests_channel_id_fkey")
	}
	if !strings.Contains(err.Error(), "违反数据库约束") {
		t.Errorf("错误文本 %q 里没有写明这是约束冲突", err)
	}
}

// TestSaveReservationForeignKeyIsConstraintConflict 用真库上**另一条**外键
// 的冲突，验写路径能被通用哨兵兜住。
//
// 造法：预留行里的 channel_id 指向一个不存在的渠道。这条外键在预留时还
// 不知道渠道（正常路径写 0、落成 NULL），所以驱动的 23503 只会在这条用例
// 刻意造的场景里出现。
func TestSaveReservationForeignKeyIsConstraintConflict(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := testRequest(ctx, t, db, userID, requestID)
	// 选一个 int4 装得下的值：INSERT 里写的是 NULLIF($3, 0)，PostgreSQL
	// 由字面量 0 把 $3 推成 int4，更大的值会在驱动编码阶段就失败，
	// 走不到外键那一步。
	req.ChannelID = 2000000000 // channels 里没有这一行

	_, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "10", "CNY"))
	if err == nil {
		t.Fatal("指向不存在的渠道应当被外键挡下")
	}
	if !errors.Is(err, ErrConstraintViolation) {
		t.Errorf("错误链里没有 ErrConstraintViolation：%v", err)
	}
	// 兜底不是具名：别把「别的表的外键冲突」说成「用户不存在」。
	if errors.Is(err, ErrNoSuchUser) {
		t.Errorf("渠道外键冲突被认成了「用户不存在」——兜底哨兵在冒充具名的那个：%v", err)
	}

	// 分类之外，现场依据要原样保留：这张用例造的是哪条约束，只有驱动
	// 错误上的名字能回答。
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("错误链里没有驱动的原始错误：%v", err)
	}
	if pgErr.Code != pgForeignKeyViolation {
		t.Errorf("SQLSTATE = %q，期望 %q", pgErr.Code, pgForeignKeyViolation)
	}
	if pgErr.ConstraintName != billingRequestsChannelConstraint {
		t.Errorf("约束名 = %q，期望 %q", pgErr.ConstraintName, billingRequestsChannelConstraint)
	}
}

// TestDuplicateReservationIsConstraintConflict 用唯一键冲突验同一条兜底判据。
//
// 唯一键与 #391 那条外键是两种不同的约束，而命令行的退出码表把两者并列
// 算作「约束冲突」——所以这里不能只盖住外键那一半。
func TestDuplicateReservationIsConstraintConflict(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)

	if _, err := s.SaveReservation(ctx, testRequest(ctx, t, db, userID, requestID),
		reservation(t, userID, requestID, "10", "CNY")); err != nil {
		t.Fatal(err)
	}

	_, err := s.SaveReservation(ctx, testRequest(ctx, t, db, userID, requestID),
		reservation(t, userID, requestID, "10", "CNY"))
	if err == nil {
		t.Fatal("重复的 request_id 应当被唯一键挡下")
	}
	if !errors.Is(err, ErrConstraintViolation) {
		t.Errorf("错误链里没有 ErrConstraintViolation：%v", err)
	}

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("错误链里没有驱动的原始错误：%v", err)
	}
	if pgErr.Code != pgUniqueViolation {
		t.Errorf("SQLSTATE = %q，期望 %q（这条用例要的是唯一键冲突）", pgErr.Code, pgUniqueViolation)
	}
	if pgErr.ConstraintName != "billing_requests_request_id_key" {
		t.Errorf("约束名 = %q，期望 %q", pgErr.ConstraintName, "billing_requests_request_id_key")
	}
}

// TestNamedUserConstraintAlsoSatisfiesGeneric 钉住两条判据并存时的形状。
//
// OpenWallet 撞上 user_wallets 那条外键时返回的是具名的 ErrNoSuchUser
// （调用方能直接问出「是用户不存在」），但它的链上同时挂着通用哨兵。
// 少任何一半都会让某类调用方漏掉：只判具名的不认识「别的约束」，只判
// 通用的分不出「用户不存在」。
func TestNamedUserConstraintAlsoSatisfiesGeneric(t *testing.T) {
	db := testDB(t)
	_, err := New(db).OpenWallet(context.Background(), 900000000000000, "CNY")
	if err == nil {
		t.Fatal("不存在的用户应当报错")
	}
	if !errors.Is(err, ErrNoSuchUser) {
		t.Errorf("错误链里没有具名的 ErrNoSuchUser：%v", err)
	}
	if !errors.Is(err, ErrConstraintViolation) {
		t.Errorf("错误链里没有兜底的 ErrConstraintViolation：%v", err)
	}
}
