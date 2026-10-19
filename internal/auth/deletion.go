package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/ledger"
	"github.com/sumwai/larkax/internal/money"
)

// 本文件是账号销户的两步：打标（可撤）与匿名化（不可逆）。
//
// 它与 user.go 服务同一张 users 表，所以放在同一个包里；不同的是销户要在一个
// 事务里动到另外几张表（api_keys、user_wallets、ledger_entries、plugin_kv、
// capture_consents）。这些表的**日常读写**各有归属：key 在 store.go，钱包与
// 账本在 internal/ledger/store，插件 KV 在 internal/plugin/store。销户不经过
// 它们，直接在这里写 SQL——那几张表的所有者都没有开放「传一个事务进来」的口子，
// 而销户必须是**一个事务**：删了一半的账号（key 作废了、users 行还 active）在
// 输出上不报错，事后也分不出它停在哪一步。
//
// 两处边界要说清：
//
//   - 行永远不删。users 行、api_keys 行、钱包行、账本分录、快照、授权记录都留着，
//     销户改的是它们的状态与身份信息。账单是财务记录，不随账号删除清除
//     （见 docs/request-retention.md）。
//   - 鉴权不需要为这一档加判断：lookup 只认 status = active，而两个新状态都不是
//     active，所以打标之后用户手里的 key 立刻失效；撤销之后同一个 key 又可以用。
//     key 本身在打标这一步**一条都不动**，正是为了让撤销真的能回到原样。

// 销户引入的两个 users.status 取值，与迁移 00039 的 CHECK 约束一致。
//
// 它们不放进 UserStatuses()：那一组是 `user update --status` 与
// `user list --status` 认的取值，而销户的两个状态只能经 user delete 这一族
// 进入与离开。把它们塞进那一组，`user update --status deleted` 就能跳过
// 匿名化，直接给一行打上终态。
const (
	// UserStatusDeletionRequested 是「要求删除」：账号不能再鉴权，行与数据都还在，
	// 撤销之后回到删除要求之前的那一个状态。
	UserStatusDeletionRequested = "deletion_requested"
	// UserStatusDeleted 是终态：已经匿名化，不可逆。
	UserStatusDeleted = "deleted"
)

// 销户路径上的哨兵错误。
//
// 与 user.go 里那几个分开放：它们的下一步动作不同。这里的前四个是**数据不在
// 可执行那个动作的状态上**（先看库里现在是什么、先处理余额、先打标），命令行
// 把它们归到约束冲突（退出码 4）；最后一个是命令本身写错了（--confirm 与当前
// 用户名对不上），归参数错（退出码 2）。
var (
	// ErrDeletionNotRequested 表示这个账号不在「要求删除」状态，purge 或 revert
	// 走不到。
	ErrDeletionNotRequested = errors.New("账号不在「要求删除」状态")
	// ErrDeletionAlreadyRequested 表示这个账号已经打过标了。
	ErrDeletionAlreadyRequested = errors.New("账号已经处于「要求删除」状态")
	// ErrDeletionAlreadyDeleted 表示这个账号已经匿名化。终态不可逆，也不能重复执行。
	ErrDeletionAlreadyDeleted = errors.New("账号已经销户")
	// ErrDeletionBalanceNotZero 表示钱包里还有没结清的钱。
	//
	// 说的不只是 balance：冻结额也在判据里。冻结额是还没结算的请求预扣住的那部分，
	// 匿名化之后它们会照常结算，而那时账号已经认不出原来的主人——钱不能这样落地。
	ErrDeletionBalanceNotZero = errors.New("可用余额必须为 0，才能销户")
	// ErrDeletionConfirmMismatch 表示 --confirm 给的串与账号当前的用户名不一致。
	//
	// 它挡的是「照着上一次的输出又敲了一遍」这类误操作：销户不可逆，确认要落在
	// **这一行现在是谁**上，而不是一个记错的名字。
	ErrDeletionConfirmMismatch = errors.New("--confirm 与当前用户名不一致")
)

// DeletionMark 是一次打标或撤销的结果。
//
// 两个状态字段给的是**库里那一行**前后两次读数：打标是 active → deletion_requested，
// 撤销是 deletion_requested → 它原来的状态。
type DeletionMark struct {
	// UserID 是 users.id，销户之后仍然用它定位——用户名会被换掉。
	UserID int64
	// Username 是打标/撤销之前的用户名（撤销之后它仍然有效）。
	Username string
	// FromStatus 是这次操作之前的状态。
	FromStatus string
	// ToStatus 是这次操作之后的状态。
	ToStatus string
}

// DeletionResult 是一次匿名化的结果，逐项对应命令回执上那一栏。
//
// 计数都是**真的发生了**的行数，不是「本来就在那儿」的行数：命令的下一步常常是
// 核对，而「0 个键被删掉」与「这个用户本来就没有键」在回执上必须分得开。
type DeletionResult struct {
	UserID int64
	// OldUsername / NewUsername 是匿名化前后的用户名。
	OldUsername string
	NewUsername string
	// OldEmail / NewEmail 是匿名化前后的邮箱；空串表示库里是 NULL。
	OldEmail string
	NewEmail string
	// OldDisplayName / NewDisplayName 是匿名化前后的展示名；空串表示 NULL。
	OldDisplayName string
	NewDisplayName string
	// DeletedAt 是 users.deleted_at 写下的时刻。
	DeletedAt time.Time
	// KeysRevoked 是这次从 active 置为 revoked 的 key 数。
	KeysRevoked int64
	// KeysTotal 是这个用户名下 key 的总数——行都保留，回执用它说明这些行都留着。
	KeysTotal int64
	// PluginKVDeleted 是删掉的用户态插件 KV 键数。
	PluginKVDeleted int64
	// ConsentsRevoked 是这次撤销掉的采集授权条数；行保留。
	ConsentsRevoked int64
	// LedgerEntries 是这个用户在账本上留下的分录条数（销户不动它们）。
	LedgerEntries int64
	// Balance / Frozen 是销户那一刻钱包里的两个数（判据要求都是 0）。
	Balance money.Amount
	Frozen  money.Amount
}

// deletionRow 是销户要读的那一行 users。
type deletionRow struct {
	id          int64
	username    string
	email       sql.NullString
	displayName sql.NullString
	status      string
	prior       sql.NullString
	deletedAt   sql.NullTime
}

// lockDeletionRow 按「ID 或用户名」定位一行并锁住它。
//
// 标识的解释与 UserByRef / ResolveUser / UpdateUser 共用 userRefWhere：同一个
// 串在四条路径上认的是同一个人。
func lockDeletionRow(ctx context.Context, tx *sql.Tx, ref string) (deletionRow, error) {
	where, arg := userRefWhere(ref, 1)
	var row deletionRow
	err := tx.QueryRowContext(ctx,
		`SELECT id, username, email, display_name, status, deletion_prior_status, deleted_at
		   FROM users WHERE `+where+` FOR UPDATE`, arg,
	).Scan(&row.id, &row.username, &row.email, &row.displayName,
		&row.status, &row.prior, &row.deletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return deletionRow{}, fmt.Errorf("%w: %q", ErrUserNotFound, ref)
	}
	if err != nil {
		return deletionRow{}, fmt.Errorf("查询用户失败: %w", err)
	}
	return row, nil
}

// deletionWalletError 是钱包行读不出来时的错误。
//
// 读不出来就不能说「余额是 0」：那一列是 numeric(20, 8)，比定点数能表示的范围
// 宽（见 internal/ledger/store 的 Wallets）。把读不出来折成 0 会让一次本应被
// 拒绝的销户通过，而钱还在那一行里。
func deletionWalletError(asset, column, raw string, err error) error {
	return fmt.Errorf("钱包 %s 的 %s 读不出来（库里是 %q）：%w", asset, column, raw, err)
}

// checkDeletionBalance 在事务里读出并锁住这个用户的钱包行，要求余额与冻结额都是 0。
//
// 锁在与销户同一个事务里：检查与匿名化之间如果插进一笔入账或一次结算，判据就
// 落后于数据了。行锁挡住会改这两列的那几条路径（入账、调账、冻结、结算），
// 直到这个事务提交。
//
// 没有钱包行等同余额为 0——那样的账号从没开过户，销户不该逼人先 create。
func checkDeletionBalance(ctx context.Context, tx *sql.Tx, userID int64, username string) (balance, frozen money.Amount, err error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT asset, balance::text, frozen::text
		  FROM user_wallets
		 WHERE user_id = $1
		 ORDER BY asset
		 FOR UPDATE`, userID)
	if err != nil {
		return money.Amount{}, money.Amount{}, fmt.Errorf("查询钱包失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var (
		nonZero bool
		details []string
		sumB    = money.Amount{}
		sumF    = money.Amount{}
	)
	for rows.Next() {
		var (
			asset, rawBalance, rawFrozen string
			b, f                         money.Amount
		)
		if scanErr := rows.Scan(&asset, &rawBalance, &rawFrozen); scanErr != nil {
			return money.Amount{}, money.Amount{}, fmt.Errorf("读钱包行失败: %w", scanErr)
		}
		b, err = money.Parse(rawBalance)
		if err != nil {
			return money.Amount{}, money.Amount{}, deletionWalletError(asset, "余额", rawBalance, err)
		}
		f, err = money.Parse(rawFrozen)
		if err != nil {
			return money.Amount{}, money.Amount{}, deletionWalletError(asset, "冻结额", rawFrozen, err)
		}
		sumB = sumB.Add(b)
		sumF = sumF.Add(f)
		if !b.IsZero() || !f.IsZero() {
			nonZero = true
			details = append(details, fmt.Sprintf("%s 余额 %s、冻结 %s", asset, b, f))
		}
	}
	if err := rows.Err(); err != nil {
		return money.Amount{}, money.Amount{}, fmt.Errorf("遍历钱包行失败: %w", err)
	}
	if nonZero {
		return sumB, sumF, fmt.Errorf(
			"%w（账号 %s：%s。先把它们处理到 0，例如用 wallet adjust 调账，"+
				"或等退款与还没结算完的请求处理完，再重试）",
			ErrDeletionBalanceNotZero, username, strings.Join(details, "；"))
	}
	return sumB, sumF, nil
}

// RequestDeletion 打标：账号进入「要求删除」态，可撤销。
//
// 它**不动 api_keys**：鉴权只认 status = active，所以打标之后那些 key 已经全部
// 失效；而它们一条都没被改过，撤销之后原样还能用。真正的作废在匿名化那一步。
func (s *Store) RequestDeletion(ctx context.Context, ref string) (DeletionMark, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return DeletionMark{}, errors.New("用户标识为空")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DeletionMark{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	row, err := lockDeletionRow(ctx, tx, ref)
	if err != nil {
		return DeletionMark{}, err
	}
	switch row.status {
	case UserStatusDeletionRequested:
		return DeletionMark{}, fmt.Errorf(
			"%w：要撤回请用 larkaxctl user delete revert --user %s",
			ErrDeletionAlreadyRequested, row.username)
	case UserStatusDeleted:
		return DeletionMark{}, fmt.Errorf(
			"%w：账号 %s 已经匿名化（删除时间 %s），不能再次打标",
			ErrDeletionAlreadyDeleted, row.username, formatDeletionTime(row.deletedAt))
	}

	if _, _, err := checkDeletionBalance(ctx, tx, row.id, row.username); err != nil {
		return DeletionMark{}, err
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE users
		   SET status = $2, deletion_prior_status = $3, updated_at = now()
		 WHERE id = $1`, row.id, UserStatusDeletionRequested, row.status); err != nil {
		return DeletionMark{}, fmt.Errorf("标记用户 %s 失败: %w", row.username, err)
	}
	if err := tx.Commit(); err != nil {
		return DeletionMark{}, fmt.Errorf("提交用户 %s 的删除要求失败: %w", row.username, err)
	}
	return DeletionMark{
		UserID:     row.id,
		Username:   row.username,
		FromStatus: row.status,
		ToStatus:   UserStatusDeletionRequested,
	}, nil
}

// RevertDeletion 撤掉删除要求：状态回到打标之前的那一个。
//
// 撤销不改任何别的行——打标那一步没动 key，所以原来能用的 key 现在也能用。
// 原来 suspended / banned 的账号回到 suspended / banned，不会被顺手放回 active。
func (s *Store) RevertDeletion(ctx context.Context, ref string) (DeletionMark, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return DeletionMark{}, errors.New("用户标识为空")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DeletionMark{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	row, err := lockDeletionRow(ctx, tx, ref)
	if err != nil {
		return DeletionMark{}, err
	}
	switch row.status {
	case UserStatusDeletionRequested:
	case UserStatusDeleted:
		return DeletionMark{}, fmt.Errorf(
			"%w：账号 %s 已经匿名化（删除时间 %s），撤销不回来",
			ErrDeletionAlreadyDeleted, row.username, formatDeletionTime(row.deletedAt))
	default:
		return DeletionMark{}, fmt.Errorf(
			"%w：账号 %s 当前是 %s，没有要撤销的删除要求",
			ErrDeletionNotRequested, row.username, row.status)
	}

	// prior 为 NULL 只可能来自直接改库（打标那条语句一定写它）。那时没有
	// 「原来的状态」可回，落回 active——这一行此刻本来就不是 active，不补一个
	// 默认值它只能永远停在删除要求上。
	restored := UserStatusActive
	if row.prior.Valid {
		restored = row.prior.String
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE users
		   SET status = $2, deletion_prior_status = NULL, updated_at = now()
		 WHERE id = $1`, row.id, restored); err != nil {
		return DeletionMark{}, fmt.Errorf("撤销用户 %s 的删除要求失败: %w", row.username, err)
	}
	if err := tx.Commit(); err != nil {
		return DeletionMark{}, fmt.Errorf("提交用户 %s 的撤销失败: %w", row.username, err)
	}
	return DeletionMark{
		UserID:     row.id,
		Username:   row.username,
		FromStatus: UserStatusDeletionRequested,
		ToStatus:   restored,
	}, nil
}

// PurgeDeletion 匿名化一个已经打过标的账号。
//
// # 一个事务里做五件事
//
// 作废 key、删插件 KV、撤销采集授权、换掉身份信息、写下终态。少做一件都会留下
// 一个「说已经销户了，而某处还认得出这人」的状态；而它们各自都不会报错。
//
// 钱包与账本**不在**这五件事里：那是财务记录，按留存规则保留
// （见 docs/request-retention.md）。快照同样不动，它按自己的留存期由
// snapshot cleanup 删除。
//
// # 确认
//
// confirm 必须逐字等于当前用户名。它不是「再问一次」的仪式：匿名化不可逆，而
// 一个按 ID 打的命令很容易落到不是心里想的那个人身上——把名字写出来，确认就
// 落在这一行现在是谁上。
func (s *Store) PurgeDeletion(ctx context.Context, ref, confirm string) (DeletionResult, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return DeletionResult{}, errors.New("用户标识为空")
	}
	confirm = strings.TrimSpace(confirm)
	if confirm == "" {
		return DeletionResult{}, errors.New("缺少 --confirm（要逐字写出用户名）")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DeletionResult{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	row, err := lockDeletionRow(ctx, tx, ref)
	if err != nil {
		return DeletionResult{}, err
	}
	switch row.status {
	case UserStatusDeletionRequested:
	case UserStatusDeleted:
		return DeletionResult{}, fmt.Errorf(
			"%w：账号 %s 已经匿名化（删除时间 %s），不能重复执行",
			ErrDeletionAlreadyDeleted, row.username, formatDeletionTime(row.deletedAt))
	default:
		return DeletionResult{}, fmt.Errorf(
			"%w：账号 %s 当前是 %s。先运行 larkaxctl user delete request --user %s",
			ErrDeletionNotRequested, row.username, row.status, row.username)
	}

	if confirm != row.username {
		return DeletionResult{}, fmt.Errorf(
			"%w：--confirm 给的是 %q，当前用户名是 %q",
			ErrDeletionConfirmMismatch, confirm, row.username)
	}

	balance, frozen, err := checkDeletionBalance(ctx, tx, row.id, row.username)
	if err != nil {
		return DeletionResult{}, err
	}

	result := DeletionResult{
		UserID:         row.id,
		OldUsername:    row.username,
		OldEmail:       deletionText(row.email),
		OldDisplayName: deletionText(row.displayName),
		Balance:        balance,
		Frozen:         frozen,
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE api_keys SET status = 'revoked'
		 WHERE user_id = $1 AND status = 'active'`, row.id)
	if err != nil {
		return DeletionResult{}, fmt.Errorf("作废用户 %s 的 API Key 失败: %w", row.username, err)
	}
	if result.KeysRevoked, err = rowsAffected(res); err != nil {
		return DeletionResult{}, err
	}
	err = tx.QueryRowContext(ctx,
		`SELECT count(*) FROM api_keys WHERE user_id = $1`, row.id,
	).Scan(&result.KeysTotal)
	if err != nil {
		return DeletionResult{}, fmt.Errorf("统计用户 %s 的 API Key 失败: %w", row.username, err)
	}

	res, err = tx.ExecContext(ctx, `DELETE FROM plugin_kv WHERE user_id = $1`, row.id)
	if err != nil {
		return DeletionResult{}, fmt.Errorf("删除用户 %s 的插件 KV 失败: %w", row.username, err)
	}
	if result.PluginKVDeleted, err = rowsAffected(res); err != nil {
		return DeletionResult{}, err
	}

	// 已经撤销过的行不再动：撤销时刻是第一次的那个时刻，重复写会把举证时间
	// 往后挪。行本身保留，到期仍按各自的 expires_at。
	res, err = tx.ExecContext(ctx, `
		UPDATE capture_consents SET revoked_at = now()
		 WHERE user_id = $1 AND revoked_at IS NULL`, row.id)
	if err != nil {
		return DeletionResult{}, fmt.Errorf("撤销用户 %s 的采集授权失败: %w", row.username, err)
	}
	if result.ConsentsRevoked, err = rowsAffected(res); err != nil {
		return DeletionResult{}, err
	}

	err = tx.QueryRowContext(ctx,
		`SELECT count(*) FROM ledger_entries WHERE account = $1`,
		ledger.UserAccount(row.id).String(),
	).Scan(&result.LedgerEntries)
	if err != nil {
		return DeletionResult{}, fmt.Errorf("统计用户 %s 的账本分录失败: %w", row.username, err)
	}

	placeholder, err := freeDeletionPlaceholder(ctx, tx, row.id)
	if err != nil {
		return DeletionResult{}, err
	}
	result.NewUsername = placeholder
	result.NewEmail = placeholder + deletionEmailDomain
	result.NewDisplayName = fmt.Sprintf("已注销用户 #%d", row.id)

	var deletedAt time.Time
	err = tx.QueryRowContext(ctx, `
		UPDATE users
		   SET username = $2, email = $3, display_name = $4,
		       status = $5, deleted_at = now(), deletion_prior_status = NULL,
		       updated_at = now()
		 WHERE id = $1
		RETURNING deleted_at`,
		row.id, result.NewUsername, result.NewEmail, result.NewDisplayName, UserStatusDeleted,
	).Scan(&deletedAt)
	if err != nil {
		return DeletionResult{}, fmt.Errorf("匿名化用户 %s 失败: %w", row.username, err)
	}
	result.DeletedAt = deletedAt

	if err := tx.Commit(); err != nil {
		return DeletionResult{}, fmt.Errorf("提交用户 %s 的匿名化失败: %w", row.username, err)
	}
	return result, nil
}

// deletionEmailDomain 是匿名化之后邮箱的域名。
//
// example.com 是 RFC 2606 保留给示例的域名，也是项目占位约定里的那一档
// （见 AGENTS.md）：这个地址不会被投递，也不会与真实注册的域名撞上。整个地址
// 仍然满足 users.email 的形状，因为那一列上没有「只收真实地址」的约束。
const deletionEmailDomain = "@example.com"

// freeDeletionPlaceholder 找一个没被占用的占位用户名。
//
// 用户名上有唯一约束，而占位名是按 ID 造的：`deleted-user-42` 完全可能被另一个
// 用户在销户之前就注册了。撞上时不静默失败，而是按顺序往后找一个空的
// （-2、-3……）；找不到就把事务回滚，让人来处理，而不是写一半。
//
// email 与用户名同源，所以一并查：两列都有唯一约束，只查一半会在下一句 UPDATE
// 上撞回来，而那时的报错只剩一句驱动原文。
func freeDeletionPlaceholder(ctx context.Context, tx *sql.Tx, userID int64) (string, error) {
	for n := 1; n <= 100; n++ {
		base := fmt.Sprintf("deleted-user-%d", userID)
		if n > 1 {
			base = fmt.Sprintf("deleted-user-%d-%d", userID, n)
		}
		var taken bool
		err := tx.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM users
				 WHERE id <> $1 AND (username = $2 OR email = $3)
			)`, userID, base, base+deletionEmailDomain).Scan(&taken)
		if err != nil {
			return "", fmt.Errorf("检查占位用户名是否已被占用失败: %w", err)
		}
		if !taken {
			return base, nil
		}
	}
	return "", fmt.Errorf("前 100 个占位用户名都被占用了，无法匿名化用户 %d", userID)
}

// rowsAffected 读一条语句影响的行数。
//
// 行数是这条命令的**正向证据**：「什么都没删」与「删干净了」在安静的终端里
// 长得一样（见 LESSONS.md 第 16 条）。
func rowsAffected(res sql.Result) (int64, error) {
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("读取影响行数失败: %w", err)
	}
	return n, nil
}

// deletionText 把可空文本折成空串。空串在这一侧只表示「库里是 NULL」。
func deletionText(v sql.NullString) string {
	if !v.Valid {
		return ""
	}
	return v.String
}

// formatDeletionTime 把 deleted_at 说成人能读的 RFC3339；NULL 时写「未记录」。
func formatDeletionTime(v sql.NullTime) string {
	if !v.Valid {
		return "未记录"
	}
	return v.Time.Format(time.RFC3339)
}
