package store

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// 本文件是控制台聚合读的夹具：账单行与用量事件各一个插入助手。
//
// 两件事必须走插入语句而不能靠上层写路径造出来：用量事件只在真实计量之后
// 才落库，而聚合读要的正是**任意形状的历史行**（同一请求两行、pending 没有
// 事件、结算额与预留额不同）。走上层路径造这些形状要多绕好几层，而绕出来的
// 仍然是同一张表上的行。

// billingFixture 是一条 billing_requests 的字段。
//
// Final 为空串表示写 NULL——pending 行本来就没有最终额。SettledAt 为 nil
// 同理。KeyID 为 nil 表示这一行不属于任何 key（历史行）。
type billingFixture struct {
	UserID    int64
	KeyID     *int64
	ChannelID int64
	RequestID string
	Model     string
	Status    string
	Reserved  string
	Final     string
	Protocol  string
	Streamed  bool
	StartedAt time.Time
	SettledAt *time.Time
}

// insertBillingRequest 写一条账单行，返回它的 id。
func insertBillingRequest(ctx context.Context, t *testing.T, db *sql.DB, f billingFixture) int64 {
	t.Helper()

	var (
		keyID     any
		channelID any
		final     any
		settled   any
	)
	if f.KeyID != nil {
		keyID = *f.KeyID
	}
	if f.ChannelID > 0 {
		channelID = f.ChannelID
	}
	if f.Final != "" {
		final = f.Final
	}
	if f.SettledAt != nil {
		settled = *f.SettledAt
	}

	protocol := f.Protocol
	if protocol == "" {
		protocol = "openai"
	}
	status := f.Status
	if status == "" {
		status = BillingStatusSettled
	}

	var id int64
	if err := db.QueryRowContext(ctx, `
		INSERT INTO billing_requests
		       (request_id, user_id, api_key_id, channel_id, canonical_model,
		        protocol, streamed, reserved_amount, status, final_amount,
		        started_at, settled_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::numeric, $9, $10::numeric,
		        $11, $12)
		RETURNING id`,
		f.RequestID, f.UserID, keyID, channelID, f.Model, protocol, f.Streamed,
		f.Reserved, status, final, f.StartedAt, settled,
	).Scan(&id); err != nil {
		t.Fatalf("写账单行 %s 失败: %v", f.RequestID, err)
	}
	return id
}

// usageEventFixture 是一条 usage_events 的字段。
//
// CreatedAt 决定它落进哪个分区；本包的用例只写 2026-10 与 2026-11 两个
// 建好的分区，不碰兜底分区（它非空本身就是一条要被监控的告警）。
type usageEventFixture struct {
	RequestID        string
	UserID           int64
	ChannelID        int64
	Model            string
	PromptTokens     int64
	CompletionTokens int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64
	Path             string
	Cost             string
	DurationMS       *int
	CreatedAt        time.Time
}

// insertUsageEvent 写一条用量事件。
func seedUsageEvent(ctx context.Context, t *testing.T, db *sql.DB, f usageEventFixture) {
	t.Helper()

	var (
		channelID  any
		durationMS any
	)
	if f.ChannelID > 0 {
		channelID = f.ChannelID
	}
	if f.DurationMS != nil {
		durationMS = *f.DurationMS
	}
	path := f.Path
	if path == "" {
		path = "measured"
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO usage_events
		       (request_id, user_id, channel_id, canonical_model,
		        prompt_tokens, completion_tokens, cache_read_tokens,
		        cache_write_tokens, reasoning_tokens, billing_path, cost,
		        duration_ms, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::numeric,
		        $12, $13)`,
		f.RequestID, f.UserID, channelID, f.Model,
		f.PromptTokens, f.CompletionTokens, f.CacheReadTokens,
		f.CacheWriteTokens, f.ReasoningTokens, path, f.Cost,
		durationMS, f.CreatedAt,
	); err != nil {
		t.Fatalf("写用量事件 %s 失败: %v", f.RequestID, err)
	}
}

// usageTestTime 把一个 RFC3339 字面量解析成 UTC 时刻。
//
// 用例里的时刻必须是**字面量**：用 time.Now() 拼出来的窗口会让「这条行为
// 什么落在 10 月」变成运行时刻的函数，而分区是按月建的——月初跑与月末跑
// 落在不同的分区里，失败的样子还不一样。
func usageTestTime(t *testing.T, value string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("解析时刻 %q 失败: %v", value, err)
	}
	return at.UTC()
}

// seedUsageUser 建一个只有 users 行的用户，不建钱包与分录。
//
// 用量聚合的读只碰 users / api_keys / billing_requests / usage_events，
// 这几条用例不查余额，所以不必走 seed 那条「建用户 + 充值」的完整路径。
// 它显式收 ctx：由调用方决定取消与超时，而不是自己另起一个。
func seedUsageUser(ctx context.Context, t *testing.T, db *sql.DB) int64 {
	t.Helper()
	id := 100000 + nextID()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO users (id, username, email) VALUES ($1, $2, $3)`,
		id, "u"+itoa(id), "u"+itoa(id)+"@example.com"); err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	return id
}

// requestIDOf 给一个用例内唯一的请求号。
func requestIDOf(prefix string) string { return prefix + "-" + itoa(nextID()) }
