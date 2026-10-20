package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/money"
)

// 本文件读**用户钱包**的分录：控制台的「余额变动」清单。
//
// 它只读科目 `user:<id>` 的那一支。钱包余额按科目归组（见 read.go 的
// 「归一化」），所以谓词必须用仓库已有的那一段片段，不能自己写
// `account = 'user:12'`：旧写路径留下过 `user:012` 这样的行，逐字相等会让
// 它们从清单里消失，而余额里却算着它们——两个地方报两个数，且都没有报错。

// 会碰到用户钱包的四种分录类型。
//
// settle / payout_request / payout_paid 不在这里：它们是商家与平台之间的事，
// 用户的钱包没有这些行。把类型清单收在一处，是因为筛选参数、请求校验与
// 「默认看哪几种」三处都要认它。
var walletEntryTypes = []string{
	string(ledger.EntryTopup), string(ledger.EntryConsume),
	string(ledger.EntryRefund), string(ledger.EntryAdjustment),
}

// 钱包分录分页的边界。
const (
	EntryPageDefaultLimit = 50
	EntryPageMaxLimit     = 200
)

// EntryCursor 是钱包分录的 keyset 定位点。
type EntryCursor struct {
	CreatedAt time.Time
	ID        int64
}

// UserEntryQuery 是一次钱包分录的读取范围。
type UserEntryQuery struct {
	UserID int64
	// Types 为空表示不限类型；否则每一项取 WalletEntryTypes() 里的一个。
	Types []string
	// From / To 为零值表示不按时间筛。窗口是左闭右开。
	From   time.Time
	To     time.Time
	Cursor *EntryCursor
	Limit  int
}

// UserEntry 是一条钱包分录。
type UserEntry struct {
	ID        int64
	CreatedAt time.Time
	Type      ledger.EntryType
	Asset     ledger.Asset
	// Amount 是余额变动本身：正数为入账，负数为扣费。
	Amount money.Amount
	Memo   string
	// BillingRequestID 为 nil 表示这条分录不来自某一次计费请求
	// （充值、人工调整）。
	BillingRequestID *int64
}

// UserEntryPage 是一页钱包分录。
type UserEntryPage struct {
	Entries []UserEntry
	More    bool
}

// UserEntries 读一个用户钱包的分录，新的在前。
func (s *Store) UserEntries(ctx context.Context, q UserEntryQuery) (UserEntryPage, error) {
	if q.UserID <= 0 {
		return UserEntryPage{}, fmt.Errorf("%w: 用户 ID 必须是正整数，得到 %d", ErrInvalidQuery, q.UserID)
	}
	for _, t := range q.Types {
		known := false
		for _, allowed := range walletEntryTypes {
			if t == allowed {
				known = true
				break
			}
		}
		if !known {
			return UserEntryPage{}, fmt.Errorf("%w: 分录类型 %q，只认 %s",
				ErrInvalidQuery, t, strings.Join(walletEntryTypes, "、"))
		}
	}
	if !q.From.IsZero() && !q.To.IsZero() && !q.From.Before(q.To) {
		return UserEntryPage{}, fmt.Errorf("%w: 钱包分录的时间窗不合法", ErrInvalidQuery)
	}

	limit := q.Limit
	if limit <= 0 {
		limit = EntryPageDefaultLimit
	}
	if limit > EntryPageMaxLimit {
		limit = EntryPageMaxLimit
	}

	var (
		args  []any
		cond  []string
		param = func(v any) string {
			args = append(args, v)
			return "$" + strconv.Itoa(len(args))
		}
	)

	// 归一化谓词的两半必须一起用：表达式自己会把 `user:012` 折成 `user:12`，
	// 而 `account ~ pattern` 挡住别的科目走进这个表达式（见 reconcile.go）。
	cond = append(cond,
		`account ~ '`+userAccountAliasPattern+`'`,
		userAccountCanonicalSQL+" = "+param(ledger.UserAccount(q.UserID).String()))
	if len(q.Types) > 0 {
		cond = append(cond, "entry_type = ANY("+param(q.Types)+")")
	}
	if !q.From.IsZero() {
		cond = append(cond, "created_at >= "+param(q.From))
	}
	if !q.To.IsZero() {
		cond = append(cond, "created_at < "+param(q.To))
	}
	if q.Cursor != nil {
		cond = append(cond, "(created_at, id) < ("+
			param(q.Cursor.CreatedAt)+", "+param(q.Cursor.ID)+")")
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, created_at, entry_type, asset, amount::text,
		       COALESCE(memo, ''), billing_request_id
		  FROM ledger_entries
		 WHERE `+strings.Join(cond, " AND ")+`
		 ORDER BY created_at DESC, id DESC
		 LIMIT `+strconv.Itoa(limit+1), args...)
	if err != nil {
		return UserEntryPage{}, fmt.Errorf("查询钱包分录失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var page UserEntryPage
	for rows.Next() {
		var (
			entry     UserEntry
			amount    string
			requestID sql.NullInt64
		)
		if err := rows.Scan(&entry.ID, &entry.CreatedAt, &entry.Type,
			&entry.Asset, &amount, &entry.Memo, &requestID); err != nil {
			return UserEntryPage{}, fmt.Errorf("读钱包分录失败: %w", err)
		}
		parsed, err := money.Parse(amount)
		if err != nil {
			return UserEntryPage{}, fmt.Errorf("钱包分录 %d 的金额 %q 解析失败: %w",
				entry.ID, amount, err)
		}
		entry.Amount = parsed
		if requestID.Valid {
			id := requestID.Int64
			entry.BillingRequestID = &id
		}
		page.Entries = append(page.Entries, entry)
	}
	if err := rows.Err(); err != nil {
		return UserEntryPage{}, fmt.Errorf("遍历钱包分录失败: %w", err)
	}
	if len(page.Entries) > limit {
		page.Entries = page.Entries[:limit]
		page.More = true
	}
	return page, nil
}
