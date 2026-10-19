package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

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
// 三处边界要说清：
//
//   - 行基本不删。users、api_keys、钱包、账本分录、快照、采集授权记录都留着，
//     销户改的是它们的状态与身份信息。唯一的例外是用户态插件 KV（plugin_kv）
//     里属于这个用户的行——它们随匿名化删掉。账单是财务记录，不随账号删除清除
//     （见 docs/request-retention.md）。
//   - 鉴权不需要为这一档加判断：lookup 只认 status = active，而两个新状态都不是
//     active，所以打标之后用户手里的 key 立刻失效；撤销之后同一个 key 又可以用。
//     key 本身在打标这一步**一条都不动**，正是为了让撤销真的能回到原样。
//   - 销户这一族的锁序是 api_keys → users → user_wallets。计费那一侧的顺序不同：
//     出处是 internal/ledger/store 的 SaveReservation，keyquota 先锁那把 key，再
//     锁钱包改冻结额，最后 INSERT billing_requests，经外键在 users 上取 KEY SHARE
//     （store.go 里那句「没有任何路径按先钱包后 key 拿锁」说的就是它）。
//
//     两条路都把 api_keys 放在最前面，所以不会成环：一条是 key → users → 钱包，
//     另一条是 key → 钱包 → users（KEY SHARE）。方向相反的只有后两段，而两段都
//     从同一把 key 出发——拿不到 key 锁的计费事务停在第一把锁上，不会已经握着
//     钱包再来等 users。反过来（销户走 users → 钱包 → key）一次「初始预留 + 销户」
//     并发就能撞 40P01。
//
//     为什么 users 在钱包之前：user_wallets 的新行经外键在 users 上取 KEY SHARE，
//     而 FOR UPDATE 与它冲突。先锁 users 就堵死了「余额查完、事务提交前插进一个
//     新钱包行」这个窗口。
//
//     下面每一次「先无锁读一行」都只是快速失败：拿到锁之后会重新读、重新校验。
//     锁序之外还剩一个窗口——定位之后新建的 key 不在我们锁住的集合里——所以
//     整笔事务对 40P01 留了有限次重跑（见 retryDeletion），只作退路。

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
	// WalletRows 是读到的钱包行数：0 表示这个账号从没开过户，回执不该说
	// 「行保留」——那一行不存在。
	WalletRows int64
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

// readDeletionRow 按 ref 读一行 users，**不加锁**。
//
// 锁序要求 api_keys 与 users 先于钱包，而定位一个人只能先按 ref 读一次：ref 既
// 可能是 id 也可能是用户名，而用户名要等 users 锁之后再读一遍才是判据。这一次
// 读只用来快速失败与拿到 id，结果不参与任何判定。
func readDeletionRow(ctx context.Context, tx *sql.Tx, ref string) (deletionRow, error) {
	where, arg := userRefWhere(ref, 1)
	var row deletionRow
	err := tx.QueryRowContext(ctx,
		`SELECT id, username, email, display_name, status, deletion_prior_status, deleted_at
		   FROM users WHERE `+where, arg,
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

// lockDeletionRowByID 按 id 锁住一行 users，并把锁之后的那一版读出来。
//
// 用 id 而不是 ref：ID 不随改名变化，而用户名正是匿名化要换掉的那一列。
// 锁之后读到的是**锁内**的版本，并发改名或并发匿名化都在这里被看见。
func lockDeletionRowByID(ctx context.Context, tx *sql.Tx, id int64) (deletionRow, error) {
	var row deletionRow
	err := tx.QueryRowContext(ctx,
		`SELECT id, username, email, display_name, status, deletion_prior_status, deleted_at
		   FROM users WHERE id = $1 FOR UPDATE`, id,
	).Scan(&row.id, &row.username, &row.email, &row.displayName,
		&row.status, &row.prior, &row.deletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return deletionRow{}, fmt.Errorf("%w: id %d", ErrUserNotFound, id)
	}
	if err != nil {
		return deletionRow{}, fmt.Errorf("锁定用户失败: %w", err)
	}
	return row, nil
}

// lockUserAPIKeys 按锁序先锁住这个用户名下的 api_keys 行。
//
// 锁的是**行**，不读值：只 SELECT 一个恒量，拦的是计费路径里 keyquota 对同一行
// 的 FOR UPDATE。不按 status 过滤是有意的——UPDATE 那一句只锁 active 的行，
// 而这里要的是「这个用户的 key 集合在前、别的锁在后」这条顺序，与某一行的状态
// 无关。结果集必须读完并关闭，否则驱动会留着一个打开的游标。
func lockUserAPIKeys(ctx context.Context, tx *sql.Tx, userID int64) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM api_keys WHERE user_id = $1 FOR UPDATE`, userID)
	if err != nil {
		return fmt.Errorf("锁定用户 %d 的 API Key 失败: %w", userID, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("读锁定中的 API Key 失败: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("锁定用户 %d 的 API Key 失败: %w", userID, err)
	}
	return nil
}

// deletionTxRetries 是撞上死锁时整笔事务重跑的次数上限。
//
// 锁序已经避开已知的环，这里只兜「定位之后又新建了一把 key」那种边角：重跑
// 一次会把它一起锁进来。做成重跑而不是在代码里绕开，是因为绕过它的写法要在
// 别的路径上加锁，那会把一条不变式散到三处。
const deletionTxRetries = 2

// retryDeletion 在整笔事务因死锁被回滚时重跑它。
//
// 只认 PostgreSQL 的 40P01（deadlock_detected），别的错误原样返回——把唯一键
// 冲突也拿来重试，只会把一次失败变慢。次数用尽后把最后一次的错误包一层说明：
// 那时候库里什么都没留下（事务已回滚），原样重跑是安全的。
func retryDeletion(run func() error) error {
	var err error
	for attempt := 0; attempt <= deletionTxRetries; attempt++ {
		if err = run(); err == nil || !isDeadlock(err) {
			return err
		}
	}
	return fmt.Errorf("销户事务反复撞上死锁，已重跑 %d 次仍未成功（每一笔都已回滚，可以原样重跑）: %w",
		deletionTxRetries, err)
}

// isDeadlock 判断一个错误是不是 PostgreSQL 的死锁。
func isDeadlock(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40P01"
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
// walletRows 是读到的行数，回执用它区分「没有钱包行」与「两个数都是 0」。
func checkDeletionBalance(ctx context.Context, tx *sql.Tx, userID int64, username string) (balance, frozen money.Amount, walletRows int64, err error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT asset, balance::text, frozen::text
		  FROM user_wallets
		 WHERE user_id = $1
		 ORDER BY asset
		 FOR UPDATE`, userID)
	if err != nil {
		return money.Amount{}, money.Amount{}, 0, fmt.Errorf("查询钱包失败: %w", err)
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
			return money.Amount{}, money.Amount{}, 0, fmt.Errorf("读钱包行失败: %w", scanErr)
		}
		walletRows++
		b, err = money.Parse(rawBalance)
		if err != nil {
			return money.Amount{}, money.Amount{}, 0, deletionWalletError(asset, "余额", rawBalance, err)
		}
		f, err = money.Parse(rawFrozen)
		if err != nil {
			return money.Amount{}, money.Amount{}, 0, deletionWalletError(asset, "冻结额", rawFrozen, err)
		}
		sumB = sumB.Add(b)
		sumF = sumF.Add(f)
		if !b.IsZero() || !f.IsZero() {
			nonZero = true
			details = append(details, fmt.Sprintf("%s 余额 %s、冻结 %s", asset, b, f))
		}
	}
	if err := rows.Err(); err != nil {
		return money.Amount{}, money.Amount{}, 0, fmt.Errorf("遍历钱包行失败: %w", err)
	}
	if nonZero {
		return sumB, sumF, walletRows, fmt.Errorf(
			"%w（账号 %s：%s。先把它们处理到 0，例如用 wallet adjust 调账，"+
				"或等退款与还没结算完的请求处理完，再重试）",
			ErrDeletionBalanceNotZero, username, strings.Join(details, "；"))
	}
	return sumB, sumF, walletRows, nil
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

	var mark DeletionMark
	err := retryDeletion(func() error {
		m, err := s.requestDeletionOnce(ctx, ref)
		mark = m
		return err
	})
	if err != nil {
		return DeletionMark{}, err
	}
	return mark, nil
}

func (s *Store) requestDeletionOnce(ctx context.Context, ref string) (DeletionMark, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DeletionMark{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 先无锁地定位一次，只用来快速失败；判据是下面锁内那一版。
	row, err := readDeletionRow(ctx, tx, ref)
	if err != nil {
		return DeletionMark{}, err
	}
	err = requestDeletionGate(row)
	if err != nil {
		return DeletionMark{}, err
	}

	// 锁序：api_keys → users → user_wallets（见文件头）。打标不动 key，
	// 但这里仍要先锁它：不锁的话本事务会握着 users 去等钱包，而计费事务
	// 可能正握着钱包等 users。
	err = lockUserAPIKeys(ctx, tx, row.id)
	if err != nil {
		return DeletionMark{}, err
	}
	row, err = lockDeletionRowByID(ctx, tx, row.id)
	if err != nil {
		return DeletionMark{}, err
	}
	if err := requestDeletionGate(row); err != nil {
		return DeletionMark{}, err
	}

	if _, _, _, err := checkDeletionBalance(ctx, tx, row.id, row.username); err != nil {
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

// requestDeletionGate 是打标的两个状态前提，单独一处给「无锁快失败」与
// 「锁内判据」共用。
func requestDeletionGate(row deletionRow) error {
	switch row.status {
	case UserStatusDeletionRequested:
		return fmt.Errorf(
			"%w：要撤回请用 larkaxctl user delete revert --user %s",
			ErrDeletionAlreadyRequested, row.username)
	case UserStatusDeleted:
		return fmt.Errorf(
			"%w：账号 %s 已经匿名化（删除时间 %s），不能再次打标",
			ErrDeletionAlreadyDeleted, row.username, formatDeletionTime(row.deletedAt))
	}
	return nil
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

	// status 是 deletion_requested 时，prior 一定有值：迁移 00039 的
	// users_deletion_prior_required 管着这一对，直接改库也写不出 NULL。真读到
	// NULL 说明那条约束被绕过了，这时不替它编一个「原来的状态」——落回 active
	// 会让一次 suspended 或 banned 的撤销变成提权。
	if !row.prior.Valid {
		return DeletionMark{}, fmt.Errorf(
			"用户 %s 停在删除要求上却没有记录原来的状态，库里的约束被绕过了", row.username)
	}
	restored := row.prior.String
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

	var result DeletionResult
	err := retryDeletion(func() error {
		r, err := s.purgeDeletionOnce(ctx, ref, confirm)
		result = r
		return err
	})
	if err != nil {
		return DeletionResult{}, err
	}
	return result, nil
}

func (s *Store) purgeDeletionOnce(ctx context.Context, ref, confirm string) (DeletionResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DeletionResult{}, fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 先无锁地定位一次，只用来快速失败；判据是下面锁内那一版。并发改名、
	// 并发匿名化、或者另一个 purge 抢先，都在那把 users 锁里被看见。
	row, err := readDeletionRow(ctx, tx, ref)
	if err != nil {
		return DeletionResult{}, err
	}
	err = purgeDeletionGate(row, confirm)
	if err != nil {
		return DeletionResult{}, err
	}

	// 锁序：api_keys → users → user_wallets（见文件头）。key 先锁，计费事务
	// 就不会「握着钱包等 users」；如果校验在锁内失败，回滚把 key 还原。
	err = lockUserAPIKeys(ctx, tx, row.id)
	if err != nil {
		return DeletionResult{}, err
	}
	row, err = lockDeletionRowByID(ctx, tx, row.id)
	if err != nil {
		return DeletionResult{}, err
	}
	err = purgeDeletionGate(row, confirm)
	if err != nil {
		return DeletionResult{}, err
	}

	balance, frozen, walletRows, err := checkDeletionBalance(ctx, tx, row.id, row.username)
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
		WalletRows:     walletRows,
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

	// 按 UserIDFromAccount 的口径数：旧写路径留下过 `user:042`、`user:+42` 这类
	// 写法，按字符串相等只数得到规范的那一种，回执会少报。SQL 里只做一次窄预筛，
	// 「读成哪个 id」仍由 UserIDFromAccount 回答——科目归属只有那一处判定。
	pattern := `^user:\+?0*` + strconv.FormatInt(row.id, 10) + `$`
	ledgerRows, err := tx.QueryContext(ctx,
		`SELECT account FROM ledger_entries WHERE account ~ $1`, pattern)
	if err != nil {
		return DeletionResult{}, fmt.Errorf("统计用户 %s 的账本分录失败: %w", row.username, err)
	}
	for ledgerRows.Next() {
		var account string
		if scanErr := ledgerRows.Scan(&account); scanErr != nil {
			_ = ledgerRows.Close()
			return DeletionResult{}, fmt.Errorf("读用户 %s 的账本分录失败: %w", row.username, scanErr)
		}
		if userID, ok := ledger.UserIDFromAccount(account); ok && userID == row.id {
			result.LedgerEntries++
		}
	}
	if rowsErr := ledgerRows.Err(); rowsErr != nil {
		_ = ledgerRows.Close()
		return DeletionResult{}, fmt.Errorf("遍历用户 %s 的账本分录失败: %w", row.username, rowsErr)
	}
	if closeErr := ledgerRows.Close(); closeErr != nil {
		return DeletionResult{}, fmt.Errorf("关闭账本分录结果集失败: %w", closeErr)
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

// purgeDeletionGate 是匿名化的三个前提：状态、确认串，一次写全。
//
// 无锁快失败与锁内判据共用同一份文案，两处不各写一遍——那正是「快失败看到的
// 是旧值、判据看到的是新值」时最容易分岔的地方。
func purgeDeletionGate(row deletionRow, confirm string) error {
	switch row.status {
	case UserStatusDeletionRequested:
	case UserStatusDeleted:
		return fmt.Errorf(
			"%w：账号 %s 已经匿名化（删除时间 %s），不能重复执行",
			ErrDeletionAlreadyDeleted, row.username, formatDeletionTime(row.deletedAt))
	default:
		return fmt.Errorf(
			"%w：账号 %s 当前是 %s。先运行 larkaxctl user delete request --user %s",
			ErrDeletionNotRequested, row.username, row.status, row.username)
	}
	if confirm != row.username {
		return fmt.Errorf(
			"%w：--confirm 给的是 %q，当前用户名是 %q",
			ErrDeletionConfirmMismatch, confirm, row.username)
	}
	return nil
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
