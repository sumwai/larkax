package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/textshape"
)

// 本文件是 api_keys 的**管理面**：读一把已有 key、改它的额度 / 状态 / 备注 /
// 过期时间，以及列出 key。
//
// 它与 store.go 的分工按调用场景来分，不按表来分：store.go 是请求热路径
// （鉴权，每个请求都走一次），这里是运维手工跑的命令，一天可能一次都没有。
// 两者共用同一个 Store 与同一个 *sql.DB——拆文件只是为了让改动落在不重叠
// 的地方，也让「这个包在热路径上要做什么」不被管理面的代码稀释。

// KeyParams 描述对一把已有 key 的一次修改。
//
// **零值表示什么都不改**，所以每一项要两个字段：一个说「这一项要不要动」，
// 一个说「动成什么」。这里没有别的办法——「不动」与「清空」都是 nil 指针，
// 靠一个字段表达不了三态（不动 / 清空 / 设值），而把「清空」和「设成 0」
// 合并是这一整件事里最不能犯的错：一个是不限，一个是一个都不放行。
//
// 五项收进一个结构体、由一条语句写完，而不是每一项一个方法：分开写时，
// 一条同时改状态与额度的命令会变成两次写，第二次失败时第一次已经落库，
// 而命令只报一句失败——调用方重跑一次，或者以为什么都没改。
type KeyParams struct {
	// SetRateLimit 为 true 时，把 rate_limit_rpm 改成 RateLimitRPM。
	SetRateLimit bool
	// RateLimitRPM 为 nil（且 SetRateLimit 为 true）表示写回 NULL。
	RateLimitRPM *int
	// SetQuota 为 true 时，把 quota_limit 改成 QuotaLimit。
	SetQuota bool
	// QuotaLimit 为 nil（且 SetQuota 为 true）表示写回 NULL。
	QuotaLimit *money.Amount

	// SetStatus 为 true 时，把 status 改成 Status。
	SetStatus bool
	// Status 取 KeyStatuses 里的值之一。
	Status string

	// SetName 为 true 时，把 name 改成 Name。
	SetName bool
	// Name 是备注。空串是**合法取值**（列是 NOT NULL DEFAULT ''），
	// 与创建时不给备注是同一个意思：没有备注。
	Name string

	// SetExpiresAt 为 true 时，把 expires_at 改成 ExpiresAt。
	SetExpiresAt bool
	// ExpiresAt 为 nil（且 SetExpiresAt 为 true）表示写回 NULL：不过期。
	ExpiresAt *time.Time
}

// KeyStatuses 返回 api_keys.status 的全部合法取值。
//
// 收在一处是因为这一件事有三份表述：迁移里的 CHECK 约束、鉴权那一侧的判定
// （store.go 的 lookup），以及命令行收不收这个值。让命令行自己抄一份的话，
// 改了三处里的一处，另两处不会报错——表现是「命令行收下一个鉴权侧不认的状态」，
// 而它要等到有人拿这把 key 打请求时才露出来。
func KeyStatuses() []string {
	return []string{keyStatusActive, keyStatusDisabled, keyStatusRevoked}
}

// ErrKeyNotFound 表示按 id 找不到那一把 key。
//
// 单独一个哨兵是为了让命令行把它归到「要查的那一行不存在」（退出码 4），
// 而不是与「其他失败」（退出码 1）混进同一个退出码：前者的下一步是先去看
// 库里现在是什么，后者是去看这次的报错本身。理由与 ErrUserNotFound 相同。
var ErrKeyNotFound = errors.New("API Key 不存在")

// ValidateKeyName 校验 api_keys.name（备注）的新值。
//
// 空串是**合法取值**：列是 NOT NULL DEFAULT ”，它表示「没有备注」，与创建
// 时不给 --name 是同一个意思。这一列会进 `apikey list` 的「备注」那一栏与
// `apikey get` 的同一栏，两处都按一栏一行排版——换行会把一栏拆成两行，ESC
// 这类转义序列会原样进终端。
//
// 判在原值上、不去空白：写入路径也不去（见 UpdateKey 与 Store.Create），
// 于是被拒的那个串与本来会落库的那个串是同一个。要去掉两头的空格是另一件
// 事——这一列今天没有这条判据，别在这里顺手加上。
func ValidateKeyName(name string) error {
	return textshape.Validate("--name", name)
}

// UpdateKey 改一把已有 key，返回改完之后的整行。
//
// 按 id 定位而不是按 key_prefix：前缀只有 11 个字符，它的用途是「在列表里
// 认出是哪把钥匙」，**不保证唯一**。拿它当主键去改数据，撞上同一个用户的
// 两把同前缀 key 时会安静地改错那一把。
//
// 没被要求改的列写回它自己的值，于是一条语句就能表达「只改其中几项」，
// 不必按参数拼 SQL。拼 SQL 的那种写法在有第五个可选列时会把「哪里少了一个
// 逗号」变成一类运行期才现形的错误。
//
// RETURNING 交回改完的那一行：调用方不必再查一次，而「这次改成了什么」来自
// 这一条语句本身，不是来自之后另一条读——两次读之间插进来的改动会让那一份
// 静默地旧一格。
func (s *Store) UpdateKey(ctx context.Context, keyID int64, p KeyParams) (Key, error) {
	if keyID <= 0 {
		return Key{}, fmt.Errorf("key ID 必须是正整数，得到 %d", keyID)
	}
	if !p.SetRateLimit && !p.SetQuota && !p.SetStatus && !p.SetName && !p.SetExpiresAt {
		return Key{}, errors.New("没有要改的字段：额度、状态、备注、过期时间至少要给一项")
	}

	// 没被要求改的那几列给 nil：CASE 的另一支不会用到它，而每个参数都写了
	// :: 转换，驱动不必去猜一个没用上的值的类型。
	var rpm, quota, status, name, expires any
	if p.SetRateLimit && p.RateLimitRPM != nil {
		rpm = *p.RateLimitRPM
	}
	if p.SetQuota && p.QuotaLimit != nil {
		quota = p.QuotaLimit.String()
	}
	if p.SetStatus {
		status = p.Status
	}
	if p.SetName {
		// 判据在 auth 这一层，不只挡命令行的 --name：别的调用方（将来的
		// 管理面、脚本）从这一头写进来的值同样会进清单与回显。
		if err := ValidateKeyName(p.Name); err != nil {
			return Key{}, err
		}
		name = p.Name
	}
	if p.SetExpiresAt && p.ExpiresAt != nil {
		expires = *p.ExpiresAt
	}

	// FROM users 是为了让 RETURNING 里的 u.username 有处可来：交回去的 Key
	// 才是完整的一行，调用方不必为了一个用户名再查一次。u.id 是主键，这个
	// 连接不会让一行变成两行。
	row := s.db.QueryRowContext(ctx, `
		UPDATE api_keys AS k
		   SET rate_limit_rpm = CASE WHEN $2 THEN $3::int ELSE k.rate_limit_rpm END,
		       quota_limit    = CASE WHEN $4 THEN $5::numeric ELSE k.quota_limit END,
		       status         = CASE WHEN $6 THEN $7::text ELSE k.status END,
		       name           = CASE WHEN $8 THEN $9::text ELSE k.name END,
		       expires_at     = CASE WHEN $10 THEN $11::timestamptz ELSE k.expires_at END
		  FROM users AS u
		 WHERE u.id = k.user_id AND k.id = $1
		RETURNING `+keyColumns,
		keyID, p.SetRateLimit, rpm, p.SetQuota, quota,
		p.SetStatus, status, p.SetName, name, p.SetExpiresAt, expires)

	key, err := scanKey(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Key{}, fmt.Errorf("%w: id=%d", ErrKeyNotFound, keyID)
	}
	if err != nil {
		return Key{}, fmt.Errorf("改 API Key 失败: %w", err)
	}
	return key, nil
}

// Key 是 api_keys 表的一行，供运维列表展示。
//
// 它**不带 key_hash**：那个值没有任何展示价值，而把它读进一个会被打印、
// 会被日志记下的结构里，等于给「哈希意外出现在某处」留了一条路。
type Key struct {
	// ID 是 api_keys.id，改额度时用它定位。
	ID int64
	// UserID 与 Username 是归属用户。
	UserID   int64
	Username string
	// Name 是创建时给的备注。
	Name string
	// Prefix 是 key_prefix，用于在列表里认出是哪把钥匙。
	Prefix string
	// Suffix 是 key_suffix：明文的最后 6 个字符。空串表示这一列是 NULL
	// （迁移 00050 之前建的 key，只存过哈希，后缀恢复不出来）。
	Suffix string
	// Status 取 active / disabled / revoked。
	Status string
	// RateLimitRPM 与 QuotaLimit 的 nil 表示列值为 NULL。
	RateLimitRPM *int
	QuotaLimit   *money.Amount
	// ExpiresAt 为 nil 表示不过期。
	ExpiresAt *time.Time
	// LastUsedAt 为 nil 表示建出来之后一次都没用过。
	LastUsedAt *time.Time
	// CreatedAt 是创建时刻。
	CreatedAt time.Time
}

// keyColumns 是 Key 的列清单，本文件里几条语句共用。
//
// 别名固定写成 k（api_keys）与 u（users）：列名带上限定名才能整段拼进不同
// 形状的语句（JOIN 的 SELECT、UPDATE … RETURNING）。手抄几遍的表现是
// 「某一列永远读不出来」，而它不报错。
const keyColumns = `k.id, k.user_id, u.username, k.name, k.key_prefix, k.key_suffix, k.status,
		       k.rate_limit_rpm, k.quota_limit::text, k.expires_at, k.last_used_at,
		       k.created_at`

// rowScanner 是 *sql.Row 与 *sql.Rows 共同的那一件能力。
//
// 抽出来是为了让 GetKey 与 ListKeys 共用同一个扫描函数：NULL 与 0 的区分、
// 金额那一步解析，两处各写一遍时总有一处会漏，而漏掉的表现是「某一栏读出来
// 是另一个数」——两个读点给出两个答案，账上找不到谁对。
type rowScanner interface{ Scan(dest ...any) error }

// scanKey 把一行扫成 Key。
func scanKey(row rowScanner) (Key, error) {
	var (
		k          Key
		rpm        sql.NullInt64
		quota      sql.NullString
		suffix     sql.NullString
		expiresAt  sql.NullTime
		lastUsedAt sql.NullTime
	)
	if err := row.Scan(&k.ID, &k.UserID, &k.Username, &k.Name, &k.Prefix, &suffix,
		&k.Status, &rpm, &quota, &expiresAt, &lastUsedAt, &k.CreatedAt); err != nil {
		return Key{}, err
	}
	k.Suffix = suffix.String

	// NULL 与 0 必须分得开（见 store.go 的 lookup），所以两列都用可空类型接，
	// 再转成指针。金额多一步解析：列是 numeric，而 Go 这一侧的金额类型是
	// 定点数，没有能直接扫进去的驱动类型。
	if rpm.Valid {
		value := int(rpm.Int64)
		k.RateLimitRPM = &value
	}
	if quota.Valid {
		amount, parseErr := money.Parse(quota.String)
		if parseErr != nil {
			return Key{}, fmt.Errorf("api_keys.id=%d 的 quota_limit %q 解析失败: %w",
				k.ID, quota.String, parseErr)
		}
		k.QuotaLimit = &amount
	}
	if expiresAt.Valid {
		k.ExpiresAt = &expiresAt.Time
	}
	if lastUsedAt.Valid {
		k.LastUsedAt = &lastUsedAt.Time
	}
	return k, nil
}

// GetKey 按 id 读一把 key 的整行。
//
// 只准入正整数：id 是 bigserial，0 与负数在库里不可能有一行，而让它们走到
// 查询里只会得到一句「找不到」——那句话对着一个不可能存在的 id 说是误导。
func (s *Store) GetKey(ctx context.Context, keyID int64) (Key, error) {
	if keyID <= 0 {
		return Key{}, fmt.Errorf("key ID 必须是正整数，得到 %d", keyID)
	}

	key, err := scanKey(s.db.QueryRowContext(ctx, `
		SELECT `+keyColumns+`
		  FROM api_keys k
		  JOIN users u ON u.id = k.user_id
		 WHERE k.id = $1`, keyID))
	if errors.Is(err, sql.ErrNoRows) {
		return Key{}, fmt.Errorf("%w: id=%d", ErrKeyNotFound, keyID)
	}
	if err != nil {
		return Key{}, fmt.Errorf("查 API Key 失败: %w", err)
	}
	return key, nil
}

// ListKeys 列出 API Key，按 id 排序。
//
// userID 为 nil 时列全部用户的 key。命令行那条路径会先把这个标识解析成
// 用户，所以这里收的是已经解析好的 ID——两处各解析一遍的话，将来「按用户名
// 查不到时要不要回落到 ID」这类改动就得改两遍，而漏掉一处不会报错。
func (s *Store) ListKeys(ctx context.Context, userID *int64) ([]Key, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+keyColumns+`
		  FROM api_keys k
		  JOIN users u ON u.id = k.user_id
		 WHERE $1::bigint IS NULL OR k.user_id = $1
		 ORDER BY k.id`, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []Key
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 API Key 行: %w", err)
	}
	return out, nil
}

// ── 控制台要的分页与归属读法 ────────────────────────────────────────────
//
// 下面这几个类型与方法是给控制台（internal/consoleapi）用的。它们与上面的
// ListKeys / GetKey 共用同一份列清单与扫描函数，差别只在**筛什么、取多少**：
// 上面的那一对是运维全量读，这里的是「一个用户自己的、按游标走的一页」。

// 控制台分页的边界。
const (
	// KeyPageDefaultLimit 是调用方没说过 limit 时读几行。
	KeyPageDefaultLimit = 50
	// KeyPageMaxLimit 是一次最多读几行。再大就不是「一页」而是一次全量拉取。
	KeyPageMaxLimit = 200
)

// KeyCursor 是 keyset 分页的定位点：上一页最后一行的 (created_at, id)。
//
// 用两列而不是只用一个 id：排序是 `created_at DESC, id DESC`，而 created_at
// 允许相同（同一微秒建的两把 key）。只按 id 走的游标会在同刻的两行之间漏行或
// 重复——那正是 offset 分页的问题，换一个名字并不能解决它。
type KeyCursor struct {
	CreatedAt time.Time
	ID        int64
}

// KeyListQuery 是一次分页读的条件。
type KeyListQuery struct {
	// UserID 是当前会话的用户。这一项没有「为空」的写法：控制台只列自己的 key。
	UserID int64
	// Status 为空表示不按状态筛；否则取 KeyStatuses 里的一个。
	Status string
	// Cursor 为 nil 表示从第一页开始。
	Cursor *KeyCursor
	// Limit 为 0 时取 KeyPageDefaultLimit，超过 KeyPageMaxLimit 时压到上限。
	Limit int
}

// KeyListPage 是一页 key。
type KeyListPage struct {
	Keys []Key
	// More 表示后面还有。它只由「多读了一行」得出，不由 len(Keys) == Limit
	// 推断：正好读完最后一行时两者相等，而那时说「还有更多」是错的。
	More bool
}

// ListKeysPage 读一个用户自己的 key，按 (created_at DESC, id DESC) 走游标。
//
// # 为什么不是给 ListKeys 加参数
//
// 那一条的调用方是运维命令，它按 id 升序列出全库，且不带分页——两者的排序、
// 筛选与返回形状都不同。把两者合成一条，就得为每一种组合写分支，而分支之间
// 的差异（尤其是排序）是最容易被一次「顺手统一」改掉的东西。
func (s *Store) ListKeysPage(ctx context.Context, q KeyListQuery) (KeyListPage, error) {
	if q.UserID <= 0 {
		return KeyListPage{}, fmt.Errorf("用户 ID 必须是正整数，得到 %d", q.UserID)
	}
	if q.Status != "" {
		known := false
		for _, status := range KeyStatuses() {
			if q.Status == status {
				known = true
				break
			}
		}
		if !known {
			return KeyListPage{}, fmt.Errorf("未知的 key 状态 %q：只认 %s",
				q.Status, strings.Join(KeyStatuses(), "、"))
		}
	}

	limit := q.Limit
	if limit <= 0 {
		limit = KeyPageDefaultLimit
	}
	if limit > KeyPageMaxLimit {
		limit = KeyPageMaxLimit
	}

	var (
		args  []any
		conds []string
	)
	param := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	conds = append(conds, "k.user_id = "+param(q.UserID))
	if q.Status != "" {
		conds = append(conds, "k.status = "+param(q.Status))
	}
	if q.Cursor != nil {
		// 行值比较写出来是「created_at 更早，或同一刻而 id 更小」——与
		// ORDER BY 的两列严格同序。拆成两个布尔条件时最容易写成的
		// `created_at < X OR id < Y` 会把同刻的两行全部漏掉。
		conds = append(conds, "(k.created_at, k.id) < ("+param(q.Cursor.CreatedAt)+", "+param(q.Cursor.ID)+")")
	}

	// 多读一行只用来回答「后面还有没有」，它不进结果（见 KeyListPage.More）。
	query := `SELECT ` + keyColumns + `
	  FROM api_keys k
	  JOIN users u ON u.id = k.user_id
	 WHERE ` + strings.Join(conds, " AND ") + `
	 ORDER BY k.created_at DESC, k.id DESC
	 LIMIT ` + strconv.Itoa(limit+1)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return KeyListPage{}, fmt.Errorf("查询 API Key 清单失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var page KeyListPage
	for rows.Next() {
		key, err := scanKey(rows)
		if err != nil {
			return KeyListPage{}, err
		}
		page.Keys = append(page.Keys, key)
	}
	if err := rows.Err(); err != nil {
		return KeyListPage{}, fmt.Errorf("遍历 API Key 行: %w", err)
	}
	if len(page.Keys) > limit {
		page.Keys = page.Keys[:limit]
		page.More = true
	}
	return page, nil
}

// KeyForUser 按「归属 + id」读一把 key。
//
// 归属检查收进 SQL 的 WHERE，而不是先读出来再在调用方比 user_id：后者在
// 每一处调用点都要记得比一次，而漏掉的那一处不会报错——它返回的是别人的 key。
// 这里把「不属于你」与「不存在」折成同一个 ErrKeyNotFound，于是调用方只有
// 一条分支，也就没有「漏比一次」这条路。
func (s *Store) KeyForUser(ctx context.Context, userID, keyID int64) (Key, error) {
	if userID <= 0 {
		return Key{}, fmt.Errorf("用户 ID 必须是正整数，得到 %d", userID)
	}
	if keyID <= 0 {
		return Key{}, fmt.Errorf("%w: id=%d", ErrKeyNotFound, keyID)
	}
	key, err := scanKey(s.db.QueryRowContext(ctx, `
		SELECT `+keyColumns+`
		  FROM api_keys k
		  JOIN users u ON u.id = k.user_id
		 WHERE k.user_id = $1 AND k.id = $2`, userID, keyID))
	if errors.Is(err, sql.ErrNoRows) {
		return Key{}, fmt.Errorf("%w: id=%d", ErrKeyNotFound, keyID)
	}
	if err != nil {
		return Key{}, fmt.Errorf("查 API Key 失败: %w", err)
	}
	return key, nil
}
