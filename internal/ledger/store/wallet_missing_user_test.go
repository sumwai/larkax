package store

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// 这一组用例钉的是 ErrNoSuchUser 的**边界**，不是它的存在。
//
// 判据是「23503 **且** 约束名是 user_wallets 上那一条」。少判哪一半都会让
// 别的事实被说成「用户不存在」，而调用方照这个分类给退出码——退出码于是在
// 说谎，脚本按 4 去处置一件根本不是「这一行不存在」的事。
//
// 两条路各钉一半：
//
//	TestMissingUserViolationIsNarrow      拿手搓的 PgError 逐个改字段，把两个条件
//	                                      各自单独测一遍（与真库无关，但精确）
//	TestMissingUserViolationAgainstRealFK 拿真库上**另一条**外键的冲突，验它的
//	                                      形状确实被判成「不是」

// TestMissingUserViolationIsNarrow 逐个改驱动错误的字段，看判据在哪一步翻面。
//
// 拿手搓的 PgError 而不是去真库上造：真库上造得出的组合只有「这一条外键冲突」
// 与「另一条外键冲突」两种，而判据里的两个条件是**与**的关系，要分开验就得能
// 单独控制每一个字段。
func TestMissingUserViolationIsNarrow(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "user_wallets 上那条外键",
			err: &pgconn.PgError{
				Code:           pgForeignKeyViolation,
				ConstraintName: userWalletsUserIDConstraint,
			},
			want: true,
		},
		{
			name: "别的表上的外键冲突",
			err: &pgconn.PgError{
				Code:           pgForeignKeyViolation,
				ConstraintName: "billing_requests_channel_id_fkey",
			},
			want: false,
		},
		{
			name: "同一个约束名但错误码不是外键冲突",
			err: &pgconn.PgError{
				Code:           pgUniqueViolation,
				ConstraintName: userWalletsUserIDConstraint,
			},
			want: false,
		},
		{
			name: "不是 PostgreSQL 错误（连接断了之类）",
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
			if got := isMissingUserViolation(tc.err); got != tc.want {
				t.Errorf("isMissingUserViolation(%v) = %v，期望 %v", tc.err, got, tc.want)
			}
		})
	}
}

// billingRequestsChannelConstraint 是「另一条外键」——`billing_requests.channel_id
// → channels(id)`，PostgreSQL 自动生成的名字。
//
// 写死在测试里而不是抽到生产代码里：生产代码**不认**这条约束（认了才是判据太宽），
// 它出现在这里只是为了断言「用例造出来的确实是另一条外键的冲突」。真库里这个名字
// 变了，这条用例会红在它自己那句 Fatalf 上——那是它该有的表现：对照组失效时，
// 「判据是窄的」这个结论也就没了依据。
const billingRequestsChannelConstraint = "billing_requests_channel_id_fkey"

// TestMissingUserViolationAgainstRealFK 拿真库上另一条外键的冲突，验它不被认成
// 「用户不存在」。
//
// 与上一条的分工：上一条钉判据的每一半，这一条钉**判据面对的输入在真库里长什么
// 样**。驱动给的 PgError 上「约束名」这个字段填的是不是 PostgreSQL 自动生成的那个
// 名字（`<表>_<列>_fkey`），只有真库能回答；名字对不上时判据会静默失效——认不出来
// 就按原错误上抛，而那只表现为「分类少了一层」，没有任何东西会喊（与 errors.go 里
// wallet_non_negative 那条注释同一个道理）。
//
// 这条同时是对照组：**同一个错误码、同一个判据函数**，只因约束名不同就翻面。
func TestMissingUserViolationAgainstRealFK(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	userID := seedBareUser(t, db)

	// billing_requests.channel_id 有外键指向 channels，插一个不存在的渠道号
	// 会被挡下——这是**另一张表上**的外键冲突，与用户存不存在无关。
	_, err := db.ExecContext(ctx, `
		INSERT INTO billing_requests (request_id, user_id, channel_id, canonical_model, protocol)
		VALUES ($1, $2, $3, 'placeholder-model', 'openai')`,
		"req-"+itoa(nextID()), userID, int64(-1))
	if err == nil {
		t.Fatal("指向不存在的渠道应当被外键挡下")
	}

	// 先确认造出来的确实是「另一条外键的 23503」——否则这条用例可能因为
	// 别的原因（表结构变了、约束被删了）拿到一个别的错误，从而什么都没证明。
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("错误链里没有驱动的原始错误：%v", err)
	}
	if pgErr.Code != pgForeignKeyViolation {
		t.Fatalf("SQLSTATE = %q，期望 %q（这条用例要的是外键冲突）：%v",
			pgErr.Code, pgForeignKeyViolation, err)
	}
	if pgErr.ConstraintName != billingRequestsChannelConstraint {
		t.Fatalf("约束名 = %q，期望 %q（另一条外键的冲突才说明判据真的窄）",
			pgErr.ConstraintName, billingRequestsChannelConstraint)
	}

	if isMissingUserViolation(err) {
		t.Error("另一张表上的外键冲突被认成了「用户不存在」——判据太宽")
	}
}
