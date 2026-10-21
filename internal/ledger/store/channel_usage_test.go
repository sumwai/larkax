package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/money"
)

// 本文件是 ChannelUsageTotals 的真库用例。
//
// 它们**必须**写在这个包里：变异测试按包跑，调用方（internal/adminapi）
// 再全也覆盖不到这里的行——断言放在别的包，这个函数就是「一个变异都没跑到」。
//
// 数据都是字面量时刻，落在用例建好的 2026-10 / 2026-11 两个分区里，理由见
// usage_fixture_test.go 里 usageTestTime 的注释。

// seedOwnedChannel 建一条带归属的渠道，返回它的 id。
//
// seedChannel 不写 owner_user_id；渠道归属那条判据要它非空才验得出来。
func seedOwnedChannel(t *testing.T, ctx context.Context, db *sql.DB, owner int64) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRowContext(ctx, `
		INSERT INTO channels
		       (name, type, base_url, credential_enc, status, models, owner_user_id)
		VALUES ($1, 'openai', 'https://owner.example.com', '\x00'::bytea,
		        'enabled', ARRAY['gpt-4o'], $2)
		RETURNING id`, "chown"+itoa(nextID()), owner).Scan(&id); err != nil {
		t.Fatalf("建带归属的渠道失败: %v", err)
	}
	return id
}

// channelUsageWindow 是本文件的主窗口：2026-10-02 整天（左闭右开）。
func channelUsageWindow(t *testing.T) (from, to string) {
	t.Helper()
	return "2026-10-02T00:00:00Z", "2026-10-03T00:00:00Z"
}

// TestChannelUsageTotalsWindowIsHalfOpen 钉住三条：
// 窗口是 [from, to)——起点那一刻算、终点那一刻不算；以及四种坏窗口都报错。
func TestChannelUsageTotalsWindowIsHalfOpen(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	s := New(db)
	channelID := seedChannel(t, ctx, db)

	fromLiteral, toLiteral := channelUsageWindow(t)
	from := usageTestTime(t, fromLiteral)
	to := usageTestTime(t, toLiteral)

	// 起点那一刻：算（>= from）。
	seedUsageEvent(ctx, t, db, usageEventFixture{
		RequestID: requestIDOf("chw-in"), ChannelID: channelID,
		Model: "gpt-4o", PromptTokens: 10, Cost: "1.25", CreatedAt: from,
	})
	// 终点那一刻：不算（< to）。少了这一条，`<` 改成 `<=` 不会有任何用例红。
	seedUsageEvent(ctx, t, db, usageEventFixture{
		RequestID: requestIDOf("chw-at-to"), ChannelID: channelID,
		Model: "gpt-4o", PromptTokens: 20, Cost: "2.00", CreatedAt: to,
	})
	// 窗口中间：算。
	seedUsageEvent(ctx, t, db, usageEventFixture{
		RequestID: requestIDOf("chw-mid"), ChannelID: channelID,
		Model: "gpt-4o", PromptTokens: 30, Cost: "0.75",
		CreatedAt: usageTestTime(t, "2026-10-02T12:00:00Z"),
	})

	got, err := s.ChannelUsageTotals(ctx, nil, from, to)
	if err != nil {
		t.Fatalf("ChannelUsageTotals 失败: %v", err)
	}
	if want := money.MustParse("2.00"); got.Cost != want {
		t.Errorf("金额 = %s，期望 %s（1.25 + 0.75，终点那一刻 2.00 不算）", got.Cost, want)
	}
	if got.Requests != 2 {
		t.Errorf("请求数 = %d，期望 2（落在 to 上的那条不算）", got.Requests)
	}
	if got.PromptTokens != 40 {
		t.Errorf("prompt tokens = %d，期望 40（10 + 30）", got.PromptTokens)
	}

	// 坏窗口：零值、相等、倒置。零值这一条两种缺法各测一次——
	// `from.IsZero() || to.IsZero()` 的 `||` 改成 `&&` 时，只缺一边就不再报错。
	bad := []struct {
		name     string
		from, to string
	}{
		{"缺起点", "", toLiteral},
		{"缺终点", fromLiteral, ""},
		{"起止相同", fromLiteral, fromLiteral},
		{"起止倒置", toLiteral, fromLiteral},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			var fromArg, toArg = timeZero(t, tc.from), timeZero(t, tc.to)
			if _, err := s.ChannelUsageTotals(ctx, nil, fromArg, toArg); !errors.Is(err, ErrInvalidQuery) {
				t.Errorf("%s 应当报 ErrInvalidQuery，得到 %v", tc.name, err)
			}
		})
	}
}

// timeZero 把字面量转成时刻；空串给零值（对应「这一边没给」）。
func timeZero(t *testing.T, literal string) (zero time.Time) {
	t.Helper()
	if literal == "" {
		return zero
	}
	return usageTestTime(t, literal)
}

// TestChannelUsageTotalsScopesToOwner 钉住归属：nil 是全局合计，给了 ownerID
// 只算本人名下的渠道；ownerID 非正数直接拒绝。
func TestChannelUsageTotalsScopesToOwner(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	s := New(db)

	ownerA := seedUsageUser(ctx, t, db)
	ownerB := seedUsageUser(ctx, t, db)
	channelA := seedOwnedChannel(t, ctx, db, ownerA)
	channelB := seedOwnedChannel(t, ctx, db, ownerB)

	fromLiteral, toLiteral := channelUsageWindow(t)
	from := usageTestTime(t, fromLiteral)
	to := usageTestTime(t, toLiteral)
	at := usageTestTime(t, "2026-10-02T10:00:00Z")

	// 先取一次全局基线：同一个测试库里还有别的用例写的行，绝对数会随它们
	// 变化。断言的是**这次新增了多少**，这才与用例的执行顺序无关。
	before, err := s.ChannelUsageTotals(ctx, nil, from, to)
	if err != nil {
		t.Fatalf("取全局基线失败: %v", err)
	}

	// A 的那条请求落两行：归属判据算的是 SUM(cost) 与去重后的请求数。
	requestA := requestIDOf("chown-a")
	seedUsageEvent(ctx, t, db, usageEventFixture{
		RequestID: requestA, ChannelID: channelA, Model: "gpt-4o",
		Cost: "1.00", CreatedAt: at,
	})
	seedUsageEvent(ctx, t, db, usageEventFixture{
		RequestID: requestA, ChannelID: channelA, Model: "gpt-4o",
		Path: "estimated", Cost: "0.50", CreatedAt: at,
	})
	seedUsageEvent(ctx, t, db, usageEventFixture{
		RequestID: requestIDOf("chown-b"), ChannelID: channelB, Model: "gpt-4o",
		Cost: "2.50", CreatedAt: at,
	})
	// 没有渠道的事件：JOIN channels 把它丢掉，两个商家的合计里都不该有它。
	seedUsageEvent(ctx, t, db, usageEventFixture{
		RequestID: requestIDOf("chown-none"), Model: "gpt-4o",
		Cost: "9.99", CreatedAt: at,
	})
	// 售价与上游成本不同：合计取的是售价那一列。
	if _, err := db.ExecContext(ctx,
		`UPDATE usage_events SET upstream_cost = 9.9 WHERE request_id = $1`,
		requestA); err != nil {
		t.Fatalf("写上游成本失败: %v", err)
	}

	all, err := s.ChannelUsageTotals(ctx, nil, from, to)
	if err != nil {
		t.Fatalf("全局合计失败: %v", err)
	}
	// 1.00 + 0.50 + 2.50：不是上游成本，也不含无渠道那条。
	if want := before.Cost.Add(money.MustParse("4.00")); all.Cost != want {
		t.Errorf("全局金额 = %s，期望 %s（基线 %s + 4.00）", all.Cost, want, before.Cost)
	}
	if want := before.Requests + 2; all.Requests != want {
		t.Errorf("全局请求数 = %d，期望 %d（基线 %d + 2）", all.Requests, want, before.Requests)
	}

	a, err := s.ChannelUsageTotals(ctx, &ownerA, from, to)
	if err != nil {
		t.Fatalf("A 的合计失败: %v", err)
	}
	if want := money.MustParse("1.50"); a.Cost != want {
		t.Errorf("A 的金额 = %s，期望 %s", a.Cost, want)
	}
	if a.Requests != 1 {
		t.Errorf("A 的请求数 = %d，期望 1（同一次请求两行只算一次）", a.Requests)
	}

	b, err := s.ChannelUsageTotals(ctx, &ownerB, from, to)
	if err != nil {
		t.Fatalf("B 的合计失败: %v", err)
	}
	if want := money.MustParse("2.50"); b.Cost != want {
		t.Errorf("B 的金额 = %s，期望 %s", b.Cost, want)
	}
	if b.Requests != 1 {
		t.Errorf("B 的请求数 = %d，期望 1", b.Requests)
	}

	// 非正的 owner 是调用方的错，不是「没有这个人」：报错，不当成全局。
	for _, owner := range []int64{0, -5} {
		if _, err := s.ChannelUsageTotals(ctx, &owner, from, to); !errors.Is(err, ErrInvalidQuery) {
			t.Errorf("ownerID = %d 应当报 ErrInvalidQuery，得到 %v", owner, err)
		}
	}
}
