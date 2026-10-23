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
// 现在的结算路径保证一个请求只落一行（见 UsageSummary 的 requests 口径），
// 但 usage_events 表上没有唯一约束，历史形状与绕过结算路径的写入都可能多行。
// 把它们加起来会让一次请求被计两遍；取最新一条才是「这次请求最终算了多少」。
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
	// Page 是页码，从 1 起；0 与负数视作 1。
	//
	// 这条读用的是**页码偏移**而不是 keyset：控制台要给「共 N 条、共几页」，
	// 就得先数总数，keyset 答不了这个。代价是翻页时插进来的新行会把后面
	// 的页整体推移——请求日志按时间倒序，新的请求排最前，恰好是调用方
	// 要先看的那部分，推移可接受。命令行那条路（BillingRequestFilter）
	// 仍是 keyset，两边不共用一个类型正是为此（见 usage_read.go 的注释）。
	Page  int
	Limit int
}

// UsageRequestRow 是请求日志里的一行。
type UsageRequestRow struct {
	ID        int64
	RequestID string
	Model     string
	// KeyID 为 nil 表示这次请求没有关联到 key（历史行）。
	KeyID   *int64
	KeyName string
	// Provider 是这次请求打到哪个渠道（channels.name）；渠道已被删掉时为
	// 空串——快照不级联删除，读侧必须接受查不到。
	Provider string
	Status   string
	// Cost 在 pending 时是钱包预留额，结算后是最终额，其余状态为 0。
	// 额度包覆盖的请求在 pending 时这里是 0（钱包一分没冻）；它占着的面值
	// 不在这个字段里，见 PlanReservedAmount。
	Cost money.Amount
	// ReservedAmount 是钱包那一部分预留（billing_requests.reserved_amount）：
	// 额度包覆盖的请求在这一列是 0，钱一分没冻。
	ReservedAmount money.Amount
	// PlanReservedAmount 是这笔请求占着的额度包面值
	// （billing_requests.plan_reserved_amount）。它与 ReservedAmount 并列，
	// 不合并成一个数：加在一起就答不了「钱包冻了多少」这个问题。
	PlanReservedAmount money.Amount
	Asset              string
	Streamed           bool
	Protocol           string
	// 五个 token 槽位取该请求最新的一条 usage_events；没有事件时全 0，
	// BillingPath 为 nil。
	PromptTokens     int64
	CompletionTokens int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64
	BillingPath      *string
	DurationMS       *int64
	// FirstTokenMS 是「受理到第一个内容块」的毫秒数；非流式与没吐过字的
	// 失败都是 NULL。UserAgent 是调用方请求头原文，没带这个头时是 NULL。
	FirstTokenMS *int64
	UserAgent    *string
	// ToolResultTail 是「这次提交的最后一句是工具结果回填」，由入口判定。
	ToolResultTail bool
	StartedAt      time.Time
	SettledAt      *time.Time
}

// UsageRequestPage 是一页请求日志。
type UsageRequestPage struct {
	Requests []UsageRequestRow
	// Total 是筛选命中的总行数，与当页为不为空无关：页码翻过头时本页
	// 没有行，Total 仍是那个总数。
	Total int64
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
	page := max(q.Page, 1)

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

	// 总数先数一遍：响应里要给「共 N 条」，调用方才知道一共几页。
	// 筛选条件只落在 b 自己的列上，count 不必带三个 LEFT JOIN。
	var total int64
	if err := s.db.QueryRowContext(ctx, `
		SELECT count(*)
		  FROM billing_requests b
		 WHERE `+strings.Join(cond, " AND "), args...).Scan(&total); err != nil {
		return UsageRequestPage{}, fmt.Errorf("统计请求日志条数失败: %w", err)
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT b.id, b.request_id, b.canonical_model, b.api_key_id,
		       COALESCE(k.name, ''), COALESCE(c.name, ''),
		       b.status, b.reserved_amount::text,
		       b.plan_reserved_amount::text,
		       b.final_amount::text, b.reserved_asset, b.streamed, b.protocol,
		       b.first_token_ms, b.user_agent, b.tool_result_tail,
		       b.started_at, b.settled_at,
		       e.prompt_tokens, e.completion_tokens, e.cache_read_tokens,
		       e.cache_write_tokens, e.reasoning_tokens, e.billing_path,
		       e.duration_ms
		  FROM billing_requests b
		  LEFT JOIN api_keys k ON k.id = b.api_key_id
		  LEFT JOIN channels c ON c.id = b.channel_id
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
		 LIMIT `+strconv.Itoa(limit)+` OFFSET `+strconv.Itoa((page-1)*limit), args...)
	if err != nil {
		return UsageRequestPage{}, fmt.Errorf("查询请求日志失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var result UsageRequestPage
	result.Total = total
	for rows.Next() {
		var (
			row          UsageRequestRow
			keyID        sql.NullInt64
			reserved     string
			planReserved string
			final        sql.NullString
			prompt       sql.NullInt64
			completion   sql.NullInt64
			cacheRead    sql.NullInt64
			cacheWrite   sql.NullInt64
			reasoning    sql.NullInt64
			path         sql.NullString
			duration     sql.NullInt64
			settledAt    sql.NullTime
			provider     string
			firstToken   sql.NullInt64
			userAgent    sql.NullString
			toolTail     bool
		)
		if err := rows.Scan(&row.ID, &row.RequestID, &row.Model, &keyID,
			&row.KeyName, &provider, &row.Status, &reserved, &planReserved, &final, &row.Asset,
			&row.Streamed, &row.Protocol, &firstToken, &userAgent, &toolTail,
			&row.StartedAt, &settledAt,
			&prompt, &completion, &cacheRead, &cacheWrite, &reasoning,
			&path, &duration); err != nil {
			return UsageRequestPage{}, fmt.Errorf("读请求日志失败: %w", err)
		}
		row.Provider = provider
		if firstToken.Valid {
			ms := firstToken.Int64
			row.FirstTokenMS = &ms
		}
		if userAgent.Valid {
			ua := userAgent.String
			row.UserAgent = &ua
		}
		row.ToolResultTail = toolTail
		if keyID.Valid {
			id := keyID.Int64
			row.KeyID = &id
		}

		// 两笔预留分列读：reserved 是钱包冻住的那部分（额度包覆盖的请求在这
		// 一列是 0），planReserved 是这笔请求占着的额度包面值。两者并列，才能
		// 看出「钱包没冻钱」不等于「没占额度」。
		walletReserved, err := money.Parse(reserved)
		if err != nil {
			return UsageRequestPage{}, fmt.Errorf("请求 %s 的预留金额 %q 解析失败: %w",
				row.RequestID, reserved, err)
		}
		row.ReservedAmount = walletReserved
		planReservedAmount, err := money.Parse(planReserved)
		if err != nil {
			return UsageRequestPage{}, fmt.Errorf("请求 %s 的额度包预留面值 %q 解析失败: %w",
				row.RequestID, planReserved, err)
		}
		row.PlanReservedAmount = planReservedAmount

		// 金额取哪一列取决于状态：pending 时最终额还不存在，用预留额；
		// 结算后以最终额为准。released / failed / refunded 没有从用户身上
		// 扣走钱，所以是 0——把预留额显示出来会让一次失败看起来像花了钱。
		var amountText string
		switch row.Status {
		case BillingStatusPending:
			amountText = row.ReservedAmount.String()
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
		result.Requests = append(result.Requests, row)
	}
	if err := rows.Err(); err != nil {
		return UsageRequestPage{}, fmt.Errorf("遍历请求日志失败: %w", err)
	}
	return result, nil
}
