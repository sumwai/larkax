package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/money"
)

// 本文件是**用量与计费**的只读一侧：把 usage_events（计量底稿）与
// billing_requests（每次请求的预留与结算）读回来。
//
// 它一行都不写，也不参与计费：写入方分别在 usage_event.go 与 store.go 的
// 结算路径里。这里只是让这两张表在命令行上读得回来。
//
// # 时间窗按哪一列筛
//
// 两张表各按自己的**分区键 / 时间列**筛：
//
//   - usage_events 按 created_at（落库时刻，也是分区键，见迁移 00001）；
//   - billing_requests 按 started_at（受理时刻，表上有 (user_id, started_at) 索引）。
//
// 用量那一列不按 priced_at 筛：它是**事件时刻**，一次迟到的写入会让它落在
// 分区边界之外（见 insertUsageEventSQL 的「created_at 为什么不在这里写」），
// 按它筛就得扫全部分区才找得全，而按分区键筛时 PostgreSQL 自己会把范围外的
// 分区摘掉。

// UsageEventFilter 描述一次用量事件的读取范围。零值表示不限用户、不限时间、
// 不限条数。
type UsageEventFilter struct {
	// UserID 为 nil 表示不限用户。
	UserID *int64
	// From 是窗口起点（含），零值表示不限。
	From time.Time
	// To 是窗口终点（**不含**），零值表示不限。
	To time.Time
	// Limit 是一次最多读几行，0 表示不限。
	//
	// 它比调用方要的那一行多读一行：多出来的那一行只用来回答「后面还有没有」，
	// 不进结果（见 UsageEventPage.More）。
	Limit int
}

// UsageEventRow 是一条用量事件里读出来的列。
//
// 只带命令要展示的列，不写成 SELECT *：这张表的列会随 schema 演进，而 `*` 在
// 演进时会静默改变列序与含义。
//
// unit_counts / unit_prices / local_hour 那几列不在这里：它们服务于事后复算
// 与分时定价的追溯，不是「花了多少」这个问题的输入。要复算的人连库查那一行。
type UsageEventRow struct {
	ID             int64
	RequestID      string
	UserID         int64
	ChannelID      sql.NullInt64
	CanonicalModel string
	// 五个 token 槽位，与 insertUsageEventSQL 写进去的那几列同名同序。
	PromptTokens     int64
	CompletionTokens int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64
	// BillingPath 是 measured / estimated / fallback 之一（表上有 CHECK）。
	// 它决定这一行的金额能不能向用户解释，所以清单里带出来。
	BillingPath string
	// Cost 是这次结算实际收的钱（charged 侧，不是上游成本）。
	Cost      money.Amount
	Asset     string
	PricedAt  time.Time
	CreatedAt time.Time
}

// UsageEventPage 是一页用量事件。
type UsageEventPage struct {
	// Events 是这一页读到的行，最多 Limit 条（Limit 为 0 时是全部）。
	Events []UsageEventRow
	// More 表示后面还有没读出来的行。它只由「多读了一行」得出，不由
	// len(Events) == Limit 推断：正好读完最后一行时两者相等，而那时说
	// 「还有更多」是错的。
	More bool
}

// usageEventReadColumns 是读一条用量事件要选的列，顺序与下面的 Scan 一致。
//
// 名字避开 usage_event_test.go 里那个 usageEventColumns：那一个是**写入侧**
// 用例用来回读整行的列清单，两者列不同、用途也不同。
//
// cost 走 ::text 再经 money.Parse，与 read.go 的 entryColumns 同一条理由：
// 金额在库里是定点数，过一遍 float64 会丢精度。
const usageEventReadColumns = `id, request_id, user_id, channel_id, canonical_model,
	prompt_tokens, completion_tokens, cache_read_tokens, cache_write_tokens,
	reasoning_tokens, billing_path, cost::text, asset, priced_at, created_at`

// UsageEvents 读一页用量事件，新的在前。
//
// 排序补一个 id 是为了稳定：同一个 request_id 的多次尝试、以及同一微秒落库的
// 两行，只按 created_at 排时两次读出来的顺序可以不同，而那样比对两次输出就
// 变成只能靠肉眼做的事。
func (s *Store) UsageEvents(ctx context.Context, f UsageEventFilter) (UsageEventPage, error) {
	var (
		conds []string
		args  []any
	)
	param := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	if f.UserID != nil {
		conds = append(conds, "user_id = "+param(*f.UserID))
	}
	if !f.From.IsZero() {
		conds = append(conds, "created_at >= "+param(f.From))
	}
	if !f.To.IsZero() {
		conds = append(conds, "created_at < "+param(f.To))
	}

	query := "SELECT " + usageEventReadColumns + " FROM usage_events"
	if len(conds) > 0 {
		query += " WHERE " + strings.Join(conds, " AND ")
	}
	query += " ORDER BY created_at DESC, id DESC"
	if f.Limit > 0 {
		// 多读一行只为判「后面还有没有」，读到的第 Limit+1 行不进结果。
		query += " LIMIT " + strconv.Itoa(f.Limit+1)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return UsageEventPage{}, fmt.Errorf("查询用量事件失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var page UsageEventPage
	for rows.Next() {
		var (
			row     UsageEventRow
			costRaw string
		)
		if err := rows.Scan(&row.ID, &row.RequestID, &row.UserID, &row.ChannelID,
			&row.CanonicalModel, &row.PromptTokens, &row.CompletionTokens,
			&row.CacheReadTokens, &row.CacheWriteTokens, &row.ReasoningTokens,
			&row.BillingPath, &costRaw, &row.Asset, &row.PricedAt, &row.CreatedAt); err != nil {
			return UsageEventPage{}, fmt.Errorf("读用量事件失败: %w", err)
		}
		cost, err := money.Parse(costRaw)
		if err != nil {
			return UsageEventPage{}, fmt.Errorf("用量事件 %d 的金额 %q 解析失败: %w", row.ID, costRaw, err)
		}
		row.Cost = cost
		page.Events = append(page.Events, row)
	}
	if err := rows.Err(); err != nil {
		return UsageEventPage{}, fmt.Errorf("读用量事件失败: %w", err)
	}

	if f.Limit > 0 && len(page.Events) > f.Limit {
		page.Events = page.Events[:f.Limit]
		page.More = true
	}
	return page, nil
}

// BillingRequestFilter 描述一次计费请求的读取范围，与 UsageEventFilter 同构。
type BillingRequestFilter struct {
	UserID *int64
	From   time.Time
	To     time.Time
	Limit  int
}

// BillingRequestRow 是一条计费请求里读出来的列。
//
// FinalAmount 与 SettledAt 可空：只有 settled 的那些行才有值。PricingRuleID 也可空：
// 请求没有落到任何规则上时它写不进去。用 sql.Null* 而不是零值表示「没有」——一笔
// 真实结算出来的 0 元与「还没结算」是两件事，而它们在零值上长得一样。
type BillingRequestRow struct {
	ID             int64
	RequestID      string
	UserID         int64
	CanonicalModel string
	UpstreamModel  sql.NullString
	Protocol       string
	Streamed       bool
	ReservedAmount money.Amount
	FinalAmount    sql.NullString
	Asset          string
	Status         string
	ErrorCode      sql.NullString
	StartedAt      time.Time
	SettledAt      sql.NullTime
	// PricingRuleID 是定这一单价的那条规则（schema 里是 billing_requests.pricing_rule_id）。
	// 规则事后可变，而账单要能回答「当时按哪条规则算的」，所以它跟着请求一起读回来。
	// 写它的是预留路径（store.go 的 SaveReservation）：没有落到任何规则上时那里
	// 把 0 落成 NULL。
	PricingRuleID sql.NullInt64
}

// BillingRequestPage 是一页计费请求，More 的含义同 UsageEventPage。
type BillingRequestPage struct {
	Requests []BillingRequestRow
	More     bool
}

// billingRequestColumns 是读一条计费请求要选的列，顺序与下面的 Scan 一致。
//
// 这三列的取法各写一次，其余按原样扫：final_amount 取 ::text 之后仍是 NULL，按
// sql.NullString 读出来；pricing_rule_id 是可空 bigint，按 sql.NullInt64 读；
// reserved_amount 是 NOT NULL，直接 ::text 再解析。
const billingRequestColumns = `id, request_id, user_id, canonical_model, upstream_model,
	protocol, streamed, reserved_amount::text, final_amount::text, reserved_asset,
	status, error_code, started_at, settled_at, pricing_rule_id`

// BillingRequests 读一页计费请求，新的在前。
//
// 窗口按 started_at 筛（受理时刻），不是 settled_at：这条命令回答的是「这个用户
// 这段时间发过哪些请求、各自结成了什么」，而一次迟到的结算不该让它从它发生的
// 那个时间窗里消失。
func (s *Store) BillingRequests(ctx context.Context, f BillingRequestFilter) (BillingRequestPage, error) {
	var (
		conds []string
		args  []any
	)
	param := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	if f.UserID != nil {
		conds = append(conds, "user_id = "+param(*f.UserID))
	}
	if !f.From.IsZero() {
		conds = append(conds, "started_at >= "+param(f.From))
	}
	if !f.To.IsZero() {
		conds = append(conds, "started_at < "+param(f.To))
	}

	query := "SELECT " + billingRequestColumns + " FROM billing_requests"
	if len(conds) > 0 {
		query += " WHERE " + strings.Join(conds, " AND ")
	}
	query += " ORDER BY started_at DESC, id DESC"
	if f.Limit > 0 {
		query += " LIMIT " + strconv.Itoa(f.Limit+1)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return BillingRequestPage{}, fmt.Errorf("查询计费请求失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var page BillingRequestPage
	for rows.Next() {
		var (
			row               BillingRequestRow
			reservedAmountRaw string
		)
		if err := rows.Scan(&row.ID, &row.RequestID, &row.UserID, &row.CanonicalModel,
			&row.UpstreamModel, &row.Protocol, &row.Streamed, &reservedAmountRaw,
			&row.FinalAmount, &row.Asset, &row.Status, &row.ErrorCode,
			&row.StartedAt, &row.SettledAt, &row.PricingRuleID); err != nil {
			return BillingRequestPage{}, fmt.Errorf("读计费请求失败: %w", err)
		}
		// 与 UsageEvents 同一条：金额列取 ::text 再过 money.Parse。money.Amount
		// 没有实现 sql.Scanner（它是定点数的封装，不接受驱动给的字节），直接扫
		// 进去会在运行时报 unsupported Scan——编译期看不出来。
		reserved, err := money.Parse(reservedAmountRaw)
		if err != nil {
			return BillingRequestPage{}, fmt.Errorf(
				"计费请求 %s 的预留金额 %q 解析失败: %w", row.RequestID, reservedAmountRaw, err)
		}
		row.ReservedAmount = reserved
		page.Requests = append(page.Requests, row)
	}
	if err := rows.Err(); err != nil {
		return BillingRequestPage{}, fmt.Errorf("读计费请求失败: %w", err)
	}

	if f.Limit > 0 && len(page.Requests) > f.Limit {
		page.Requests = page.Requests[:f.Limit]
		page.More = true
	}
	return page, nil
}
