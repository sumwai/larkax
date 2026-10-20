package consoleapi

import (
	"errors"
	"net/http"

	"github.com/sumwai/larkax/internal/auth"
	"github.com/sumwai/larkax/internal/ledger/store"
)

// 本文件是钱包余额与分录。

// walletJSON 是钱包快照。
type walletJSON struct {
	Asset     string `json:"asset"`
	Balance   string `json:"balance"`
	Frozen    string `json:"frozen"`
	UpdatedAt string `json:"updated_at"`
}

// handleWallet 返回当前用户的钱包。
//
// # 没有钱包行不是故障
//
// 一个用户还没有过任何资金动作时，user_wallets 里没有他那一行。那是 0 元
// 余额，与「读出了这一行、余额就是 0」对使用者是同一件事，所以回 200 与
// 两个 0。**不在这里建行**：GET 是读路径，写一行出来会让「有没有钱包」
// 再也区分不出来，还会把一次并发读变成一次写冲突。
//
// 金额列解析失败仍然是 500：那是这一行读不出来，不是「余额为 0」。
// 一个偏小的余额会被当成「少扣了钱」，一个假的 0 也好不到哪去。
func (a *API) handleWallet(w http.ResponseWriter, r *http.Request, session auth.Session) {
	userID := session.UserID
	wallets, err := a.deps.usage.Wallets(r.Context(), &userID)
	if err != nil {
		a.unavailable(w, r, err, "读取钱包失败")
		return
	}

	var found *store.Wallet
	for i := range wallets {
		if string(wallets[i].Asset) == a.currency {
			found = &wallets[i]
			break
		}
	}
	if found == nil {
		// updated_at 留空：没有这一行，也就没有「余额更新于」这个时刻。
		a.ok(w, http.StatusOK, walletJSON{
			Asset:   a.currency,
			Balance: "0.00000000",
			Frozen:  "0.00000000",
		})
		return
	}
	if found.BalanceErr != nil || found.FrozenErr != nil {
		a.internal(w, r, errors.Join(found.BalanceErr, found.FrozenErr),
			"钱包金额解析失败")
		return
	}

	a.ok(w, http.StatusOK, walletJSON{
		Asset:     string(found.Asset),
		Balance:   found.Balance.String(),
		Frozen:    found.Frozen.String(),
		UpdatedAt: formatTime(found.UpdatedAt),
	})
}

// entryJSON 是一条钱包分录。
type entryJSON struct {
	ID               int64  `json:"id"`
	CreatedAt        string `json:"created_at"`
	Type             string `json:"type"`
	Asset            string `json:"asset"`
	Amount           string `json:"amount"`
	Memo             string `json:"memo"`
	BillingRequestID *int64 `json:"billing_request_id"`
}

// entriesResponse 是分录的一页。
type entriesResponse struct {
	Items      []entryJSON `json:"items"`
	NextCursor *string     `json:"next_cursor"`
}

// handleWalletEntries 读一页钱包分录。
//
// `amount` 就是余额变动本身（正 = 入账，负 = 扣费），与账本里用户科目的
// 符号约定一致；前端不要再翻一次符号。
func (a *API) handleWalletEntries(w http.ResponseWriter, r *http.Request, session auth.Session) {
	q := r.URL.Query()

	from, to, err := a.parseWindow(r, a.now())
	if err != nil {
		a.invalid(w, r, err)
		return
	}

	limit, err := parseLimit(q.Get("limit"),
		store.EntryPageDefaultLimit, store.EntryPageMaxLimit)
	if err != nil {
		a.invalid(w, r, err)
		return
	}

	var cursor *store.EntryCursor
	if raw := q.Get("cursor"); raw != "" {
		at, id, cursorErr := decodeCursor(raw)
		if cursorErr != nil {
			a.invalid(w, r, cursorErr)
			return
		}
		cursor = &store.EntryCursor{CreatedAt: at, ID: id}
	}

	page, err := a.deps.usage.UserEntries(r.Context(), store.UserEntryQuery{
		UserID: session.UserID,
		Types:  q["type"],
		From:   from,
		To:     to,
		Cursor: cursor,
		Limit:  limit,
	})
	if err != nil {
		a.usageError(w, r, err)
		return
	}

	items := make([]entryJSON, 0, len(page.Entries))
	for _, entry := range page.Entries {
		items = append(items, entryJSON{
			ID:               entry.ID,
			CreatedAt:        formatTime(entry.CreatedAt),
			Type:             string(entry.Type),
			Asset:            string(entry.Asset),
			Amount:           entry.Amount.String(),
			Memo:             entry.Memo,
			BillingRequestID: entry.BillingRequestID,
		})
	}

	out := entriesResponse{Items: items}
	if page.More && len(page.Entries) > 0 {
		last := page.Entries[len(page.Entries)-1]
		next := encodeCursor(last.CreatedAt, last.ID)
		out.NextCursor = &next
	}
	a.ok(w, http.StatusOK, out)
}
