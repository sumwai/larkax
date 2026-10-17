package dberr

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// 这一组用例钉的是**唯一的判据**：只有外键（23503）与唯一键（23505）算
// 约束冲突，CHECK（23514）不算，非数据库错误也不算。各包套的都是这一份，
// 所以它翻面时所有写路径一起翻面——这正是把判据收在一处要换的东西，
// 也正因为如此，它必须被钉住。

// TestIsConstraintViolation 逐个改驱动错误的字段，看判据在哪一步翻面。
func TestIsConstraintViolation(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "外键冲突",
			err:  &pgconn.PgError{Code: pgForeignKeyViolation, ConstraintName: "api_keys_user_id_fkey"},
			want: true,
		},
		{
			name: "唯一键冲突",
			err:  &pgconn.PgError{Code: pgUniqueViolation, ConstraintName: "plugins_key_version_key"},
			want: true,
		},
		{
			name: "CHECK 违反不是这一类",
			err:  &pgconn.PgError{Code: "23514", ConstraintName: "wallet_non_negative"},
			want: false,
		},
		{
			name: "包了一层驱动错误",
			err: fmt.Errorf("写入失败: %w",
				&pgconn.PgError{Code: pgForeignKeyViolation, ConstraintName: "idempotency_keys_api_key_id_fkey"}),
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
			if got := IsConstraintViolation(tc.err); got != tc.want {
				t.Errorf("IsConstraintViolation(%v) = %v，期望 %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestConstraintFailureLeavesOthersAlone 钉住「认不出来时一个字都不加」。
//
// 这个性质让它可以无差别地套在写语句的错误上：非约束错误必须**原样**
// 返回（同一个错误值），否则调用方原有的分类会被这层兜底搅乱。
func TestConstraintFailureLeavesOthersAlone(t *testing.T) {
	plain := errors.New("连接断了")
	//nolint:errorlint // 这里要钉的正是「同一个错误值」，不是「链上有没有它」：
	// 多包一层同文本的错误也能过 errors.Is，却会改变调用方的错误链形状。
	if got := ConstraintFailure(plain); got != plain {
		t.Errorf("非约束错误应当原样返回同一个值，得到 %v", got)
	}
	if got := ConstraintFailure(nil); got != nil {
		t.Errorf("nil 应当原样返回，得到 %v", got)
	}
}

// TestConstraintFailureKeepsDriverError 钉住「只加分类，不替掉现场」。
//
// 运维要看的是「哪张表、哪条约束」，那只有驱动错误回答得了。哨兵在链首，
// 驱动错误必须在链里，且约束名原样可取回。
func TestConstraintFailureKeepsDriverError(t *testing.T) {
	pgErr := &pgconn.PgError{
		Code:           pgForeignKeyViolation,
		ConstraintName: "api_keys_user_id_fkey",
	}
	err := ConstraintFailure(fmt.Errorf("写入 API Key 失败: %w", pgErr))

	if !errors.Is(err, ErrConstraintViolation) {
		t.Fatalf("错误链里没有 ErrConstraintViolation：%v", err)
	}
	var got *pgconn.PgError
	if !errors.As(err, &got) {
		t.Fatalf("错误链里没有驱动的原始错误：%v", err)
	}
	if got.ConstraintName != "api_keys_user_id_fkey" {
		t.Errorf("约束名 = %q，期望 %q", got.ConstraintName, "api_keys_user_id_fkey")
	}
	if !strings.Contains(err.Error(), "违反数据库约束") {
		t.Errorf("错误文本 %q 里没有写明这是约束冲突", err)
	}
}
