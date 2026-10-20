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

// 本文件是控制台的请求日志：账单行 + 该请求最新的一条用量事件。
//
// 它与 usage_read.go 的 BillingRequests 的区别在投影：那一条给命令行看，
// 读的是账单本身的列；这一条给用户看「这次请求花了多少、计量是怎么来的」，
// 所以要多带 token 明细、计费路径与时长，而这三样在 usage_events 上。
//
// # 每个请求只取最新一条事件
//
// 一次请求重试可能落多行 usage_events（见 UsageSummary 的 requests 口径）。
// 把它们加起来会让一次重试被计两遍；取最新一条才是「这次请求最终算了多少」。
// LATERAL 子查询走 usage_events_request_idx (request_id, created_at)。

// 账单请求的状态，与迁移里 billing_requests.status 的 CHECK 一致。
const (
	BillingStatusPending  = "pending"
	BillingStatusSettled  = "settled"
	BillingStatusReleased = "released"
	BillingStatusFailed   = "failed"
	BillingStatusRefunded = "refunded"
)

// BillingRequestStatuses 返回账单请求状态的全部合法取值。
//
// 收在一处：迁移里的 CHECK、请求日志的筛选参数、以及调用方拼 `status=` 时
// 都认这一张清单。抄三份的那一天，就是三份开始分岔的那一天。
func BillingRequestStatuses() []string {
	return []string{
		BillingStatusPending, BillingStatusSettled, BillingStatusReleased,
		BillingStatusFailed, BillingStatusRefunded,
	}
}

// 请求日志分页的边界，与 key 清单那一组同一个量级。
const (
	RequestPageDefaultLimit = 50
	RequestPageMaxLimit     = 200
)

// RequestCursor 是请求日志的 keyset 定位点。
//
// 排序按 (started_at DESC, id DESC)——受理时刻，不是结算时刻：一次迟到的
// 结算不该把请求挪到日志的另一头。
type RequestCursor struct {
	StartedAt time.Time
	ID        int64
}

// UsageRequestQuery 是请求日志的一次分页读。
type UsageRequestQuery struct {
	UserID int64
	From   time.Time
	To     time.Time
	// APIKeyID 为 nil 表示不限 key。
	APIKeyID *int64
	// Model 与 Status 为空表示不筛。
	Model  string
	Status string
	Cursor *RequestCursor
	Limit  int
}

// UsageRequestRow 是请求日志里的一行。
type UsageRequestRow struct {
	ID        int64
	RequestID string
	Model     string
	// KeyID 为 nil 表示这次请求没有关联到 key（历史行）。
	KeyID   *int64
	KeyName string
	Status  string
	// Cost 在 pending 时是预留额，结算后是最终额，其余状态为 0。
	Cost     money.Amount
	Asset    string
	Streamed bool
	Protocol string
	// 五个 token 槽位取该请求最新的一条 usage_events；没有事件时全 0，
	// BillingPath 为 nil。
	PromptTokens     int64
	CompletionTokens int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64
	BillingPath      *string
	DurationMS       *int64
	StartedAt        time.Time
	SettledAt        *time.Time
}

// UsageRequestPage 是一页请求日志。
type UsageRequestPage struct {
	Requests []UsageRequestRow
	More     bool
}

// UsageRequests 读一页请求日志。
func (s *Store) UsageRequests(ctx context.Context, q UsageRequestQuery) (UsageRequestPage, error) {
	if q.UserID <= 0 {
		return UsageRequestPage{}, fmt.Errorf("%w: 用户 ID 必须是正整数，得到 %d", ErrInvalidQuery, q.UserID)
	}
	if q.From.IsZero() || q.To.IsZero() || !q.From.Before(q.To) {
		return UsageRequestPage{}, fmt.Errorf("%w: 请求日志的时间窗不合法", ErrInvalidQuery)
	}
	if q.Status != "" {
		known := false
		for _, status := range BillingRequestStatuses() {
			if q.Status == status {
				known = true
				break
			}
		}
		if !known {
			// 这里是**查询参数**不合法，不是库出了毛病：包上 ErrInvalidQuery，
			// 控制台才会把它翻成 400。少包这一层时表现是「筛选参数写错」被
			// 报成 503（服务不可用），而重试一个写错的参数永远不会好。
			return UsageRequestPage{}, fmt.Errorf("%w: 未知的请求状态 %q：只认 %s",
				ErrInvalidQuery, q.Status, strings.Join(BillingRequestStatuses(), "、"))
		}
	}

	limit := q.Limit
	if limit <= 0 {
		limit = RequestPageDefaultLimit
	}
	if limit > RequestPageMaxLimit {
		limit = RequestPageMaxLimit
	}

	var (
		args  []any
		cond  = make([]string, 0, 6)
		param = func(v any) string {
			args = append(args, v)
			return "$" + strconv.Itoa(len(args))
		}
	)
	cond = append(cond,
		"b.user_id = "+param(q.UserID),
		"b.started_at >= "+param(q.From),
		"b.started_at < "+param(q.To))
	if q.APIKeyID != nil {
		cond = append(cond, "b.api_key_id = "+param(*q.APIKeyID))
	}
	if q.Model != "" {
		cond = append(cond, "b.canonical_model = "+param(q.Model))
	}
	if q.Status != "" {
		cond = append(cond, "b.status = "+param(q.Status))
	}
	if q.Cursor != nil {
		// 与 ORDER BY 同序的两列行值比较；拆开写成 `started_at < X OR id < Y`
		// 会把同一时刻的行整批漏掉。
		cond = append(cond, "(b.started_at, b.id) < ("+
			param(q.Cursor.StartedAt)+", "+param(q.Cursor.ID)+")")
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT b.id, b.request_id, b.canonical_model, b.api_key_id,
		       COALESCE(k.name, ''), b.status, b.reserved_amount::text,
		       b.final_amount::text, b.reserved_asset, b.streamed, b.protocol,
		       b.started_at, b.settled_at,
		       e.prompt_tokens, e.completion_tokens, e.cache_read_tokens,
		       e.cache_write_tokens, e.reasoning_tokens, e.billing_path,
		       e.duration_ms
		  FROM billing_requests b
		  LEFT JOIN api_keys k ON k.id = b.api_key_id
		  LEFT JOIN LATERAL (
		        SELECT ue.prompt_tokens, ue.completion_tokens,
		               ue.cache_read_tokens, ue.cache_write_tokens,
		               ue.reasoning_tokens, ue.billing_path, ue.duration_ms
		          FROM usage_events ue
		         WHERE ue.request_id = b.request_id
		         ORDER BY ue.created_at DESC
		         LIMIT 1
		       ) e ON true
		 WHERE `+strings.Join(cond, " AND ")+`
		 ORDER BY b.started_at DESC, b.id DESC
		 LIMIT `+strconv.Itoa(limit+1), args...)
	if err != nil {
		return UsageRequestPage{}, fmt.Errorf("查询请求日志失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var page UsageRequestPage
	for rows.Next() {
		var (
			row        UsageRequestRow
			keyID      sql.NullInt64
			reserved   string
			final      sql.NullString
			prompt     sql.NullInt64
			completion sql.NullInt64
			cacheRead  sql.NullInt64
			cacheWrite sql.NullInt64
			reasoning  sql.NullInt64
			path       sql.NullString
			duration   sql.NullInt64
			settledAt  sql.NullTime
		)
		if err := rows.Scan(&row.ID, &row.RequestID, &row.Model, &keyID,
			&row.KeyName, &row.Status, &reserved, &final, &row.Asset,
			&row.Streamed, &row.Protocol, &row.StartedAt, &settledAt,
			&prompt, &completion, &cacheRead, &cacheWrite, &reasoning,
			&path, &duration); err != nil {
			return UsageRequestPage{}, fmt.Errorf("读请求日志失败: %w", err)
		}
		if keyID.Valid {
			id := keyID.Int64
			row.KeyID = &id
		}

		// 金额取哪一列取决于状态：pending 时最终额还不存在，用预留额；
		// 结算后以最终额为准。released / failed / refunded 没有从用户身上
		// 扣走钱，所以是 0——把预留额显示出来会让一次失败看起来像花了钱。
		var amountText string
		switch row.Status {
		case BillingStatusPending:
			amountText = reserved
		case BillingStatusSettled:
			if final.Valid {
				amountText = final.String
			}
		default:
			amountText = "0"
		}
		amount, err := money.Parse(amountText)
		if err != nil {
			return UsageRequestPage{}, fmt.Errorf("请求 %s 的金额 %q 解析失败: %w",
				row.RequestID, amountText, err)
		}
		row.Cost = amount

		row.PromptTokens = prompt.Int64
		row.CompletionTokens = completion.Int64
		row.CacheReadTokens = cacheRead.Int64
		row.CacheWriteTokens = cacheWrite.Int64
		row.ReasoningTokens = reasoning.Int64
		if path.Valid {
			value := path.String
			row.BillingPath = &value
		}
		if duration.Valid {
			value := duration.Int64
			row.DurationMS = &value
		}
		if settledAt.Valid {
			row.SettledAt = &settledAt.Time
		}
		page.Requests = append(page.Requests, row)
	}
	if err := rows.Err(); err != nil {
		return UsageRequestPage{}, fmt.Errorf("遍历请求日志失败: %w", err)
	}
	if len(page.Requests) > limit {
		page.Requests = page.Requests[:limit]
		page.More = true
	}
	return page, nil
}
