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
)

// api_keys.status 与 users.status 的取值，与迁移里的 CHECK 约束一致。
const (
	keyStatusActive   = "active"
	keyStatusDisabled = "disabled"
	keyStatusRevoked  = "revoked"

	userStatusActive = "active"
)

// lookup 按哈希定位一行，再逐条判断它能不能用。
//
// # 为什么状态与过期不写进 WHERE
//
// 写进 WHERE 的话，「查不到」只剩一种解释，日志里就分不清
// 「没有这个 key」和「这个 key 过期了」——而这两种情况指向的排查动作不同。
// 判断留在 Go 里，代价是每次鉴权读回几列并多做几次比较，
// 换到的是日志里能直接看出原因。
//
// 判断的**顺序**也固定：状态 → 过期 → 用户。它决定了两项都不满足时
// 日志里出现哪个原因，而固定的顺序让同一把 key 每次都给出同一个答案。
func (s *Store) lookup(ctx context.Context, plaintext string) (User, error) {
	var (
		user       User
		keyStatus  string
		userStatus string
		expiresAt  sql.NullTime
		rateLimit  sql.NullInt64
	)

	err := s.db.QueryRowContext(ctx, `
		SELECT k.id, k.status, k.expires_at, k.rate_limit_rpm, u.id, u.username, u.status
		  FROM api_keys k
		  JOIN users u ON u.id = k.user_id
		 WHERE k.key_hash = $1`, HashKey(plaintext),
	).Scan(&user.KeyID, &keyStatus, &expiresAt, &rateLimit, &user.ID, &user.Username, &userStatus)

	if errors.Is(err, sql.ErrNoRows) {
		return User{}, &Error{Reason: ReasonKeyNotFound}
	}
	if err != nil {
		// 这里**不**包成 ErrUnauthenticated：查询失败不是「凭证不对」。
		// 包上去会让调用方按 401 回应一次数据库故障。
		return User{}, fmt.Errorf("查询 API Key 失败: %w", err)
	}

	// NULL 与 0 必须分得开：把 NULL 直接扫进 int 会得到 0，而这两者在
	// 限流那一侧的意思正相反（不限流 / 一个都不放行）。用 NullInt64 接住
	// 再转成指针，NULL 就落在 nil 上。
	//
	// 转换是安全的：列类型是 integer（32 位），int 至少 32 位。
	if rateLimit.Valid {
		value := int(rateLimit.Int64)
		user.RateLimitRPM = &value
	}

	switch keyStatus {
	case keyStatusActive:
	case keyStatusDisabled:
		return User{}, &Error{Reason: ReasonKeyDisabled}
	case keyStatusRevoked:
		return User{}, &Error{Reason: ReasonKeyRevoked}
	default:
		// CHECK 约束之外的值只可能来自直接改库。当成不可用，
		// 但理由单独记一条：它说明有人在绕过应用写数据。
		return User{}, &Error{Reason: ReasonKeyInactive}
	}

	// 过期判定用应用侧的时钟，而不是 SQL 里的 now()：判断要留在 Go 里
	// 才能区分原因（见上）。代价是应用与数据库的时钟必须大体一致——
	// 差到分钟级时，过期边界的判定会跟着漂。
	if expiresAt.Valid && !expiresAt.Time.After(time.Now()) {
		return User{}, &Error{Reason: ReasonKeyExpired}
	}

	// key 有效不代表能用：封禁一个用户之后，他手里所有 key 必须一起失效，
	// 否则「封禁」只是让他换个 key 继续用。
	if userStatus != userStatusActive {
		return User{}, &Error{Reason: ReasonUserInactive}
	}

	s.touch(ctx, user.KeyID)
	return user, nil
}

// lastUsedWriteInterval 是 last_used_at 的最小写入间隔。
//
// 取 1 分钟，是由这个字段**要回答的问题**定下的：它给人看「这把 key 还在用吗」，
// 精度到分钟就够（见 touch）。1 分钟是仍满足这个精度的最粗窗口，
// 再细就是无谓的写。
//
// 为什么不再拉长：写放大在这一档已经与请求速率脱钩。一把每秒上千次请求的 key，
// 写量从「每请求一次」降到「每分钟一次」——三个数量级；而窗口从 1 分钟拉到
// 5 分钟只再降 5 倍，却把「刚用过」这条信息的分辨率一起拉粗，
// 那正是这个字段唯一的用途。1 分钟是这两者之间的取舍点。
//
// 它是常量而不是配置：改它不影响任何正确性，只影响观测数据的粗细。
// 将来真要可配，按项目的分界线它属于 settings 表（不必在连库前拿到），
// 不进环境变量。
const lastUsedWriteInterval = time.Minute

// touch 更新 last_used_at。
//
// 它是**观测数据**，不是正确性的一部分，所以错误显式丢弃：
// 一个本来有效的请求不该因为一列统计值写不进去而变成 401——
// 那等于把「记录它被用过」的重要性抬到了「它能用」之上。
//
// 代价是这一列可能悄悄停在旧值上。它只用于「这把 key 还在用吗」这类判断，
// 真需要精确时可以按 created_at 与访问日志对。
//
// # 为什么要节流
//
// 这次写落在每个推理请求的最前面，而且更新的是**同一行**：同一把 key 的
// 并发请求会在这行的行锁上排队。加了窗口条件之后，写量从「每请求一次」
// 降到「每 lastUsedWriteInterval 最多一次」，与请求速率脱钩。
//
// 条件是「为空或早于窗口」，所以**第一次一定写**：新建的 key 有没有被用过，
// 不受窗口影响，窗口只决定之后多久刷新一次。
//
// 时间比较留在 SQL 里（now()），和 SET 用的是同一个时钟；挪到 Go 里算
// 会引入应用与数据库的时钟差，而这个差没有任何地方能兜住。
//
// 不改异步：那会把错误处理丢掉（这个写失败现在不影响鉴权，异步之后
// 连日志都可能丢），也会在进程退出时留下没写完的写。
func (s *Store) touch(ctx context.Context, keyID int64) {
	_, _ = s.db.ExecContext(ctx, `
		UPDATE api_keys SET last_used_at = now()
		 WHERE id = $1
		   AND (last_used_at IS NULL
		        OR last_used_at < now() - make_interval(secs => $2))`,
		keyID, lastUsedWriteInterval.Seconds())
}

// CreateParams 描述要创建的一把 key。
type CreateParams struct {
	// UserID 是归属用户（users.id）。
	UserID int64
	// Name 是给人看的备注，可为空。
	Name string
	// ExpiresAt 为 nil 表示不过期。
	ExpiresAt *time.Time
	// RateLimitRPM 是这把 key 的每分钟请求额度。nil 表示写 NULL，即
	// 「继承用户默认」——而 users 表上目前没有这样一个默认值，所以
	// 它等价于不限。**0 不是「不限」**，是「一个都不放行」，两者必须分得开。
	RateLimitRPM *int
	// QuotaLimit 是这把 key 累计可花费的金额上限，单位是用户钱包的资产品种
	// （见 user_wallets.asset）。nil 表示写 NULL，即继承用户默认。
	//
	// 用 money.Amount 而不是 float64：列是 numeric(20, 8)，而这一层一旦
	// 让浮点进来，舍入点就跑到 money 包外面去了（见 scripts/check-layering.sh）。
	QuotaLimit *money.Amount
}

// Created 是一次创建的结果。
type Created struct {
	// ID 是 api_keys.id。
	ID int64
	// Prefix 是入库的 key_prefix，用于日后在列表里认出这把 key。
	Prefix string
	// Plaintext 是**仅此一次**的明文。
	Plaintext string
}

// Create 生成一把新 key，只把哈希落库，并返回仅此一次的明文。
//
// 明文不落库也不写日志：库里只有 key_hash，它不可逆——这正是这个设计的目的。
// 所以调用方拿到明文之后唯一的正确动作是立刻交给用户，
// 任何「先存下来回头再说」都是在库外面又造了一份明文副本。
func (s *Store) Create(ctx context.Context, p CreateParams) (Created, error) {
	if p.UserID <= 0 {
		return Created{}, fmt.Errorf("用户 ID 必须是正整数，得到 %d", p.UserID)
	}

	plaintext, err := NewKey()
	if err != nil {
		return Created{}, err
	}

	// 不先查用户是否存在：外键约束会挡下不存在的 user_id，
	// 而先查一次多一个竞态窗口（查完到插入之间用户可能被删）。
	// 代价是错误信息来自数据库，调用方（命令行）因此会先解析一次用户。
	var expiresAt sql.NullTime
	if p.ExpiresAt != nil {
		expiresAt = sql.NullTime{Time: *p.ExpiresAt, Valid: true}
	}

	prefix := KeyPrefix(plaintext)
	var id int64
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO api_keys (user_id, name, key_hash, key_prefix, expires_at,
		                      rate_limit_rpm, quota_limit)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id`,
		p.UserID, p.Name, HashKey(plaintext), prefix, expiresAt,
		nullableInt(p.RateLimitRPM), nullableAmount(p.QuotaLimit),
	).Scan(&id)
	if err != nil {
		return Created{}, fmt.Errorf("写入 API Key 失败: %w", err)
	}

	return Created{ID: id, Prefix: prefix, Plaintext: plaintext}, nil
}

// nullableInt 把一个可选的整数转成能写进可空列的驱动值。
func nullableInt(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

// nullableAmount 把一个可选的金额转成能写进 numeric 列的驱动值。
//
// 传的是十进制字符串（money.Amount.String 的输出），**不是**最小单位数：
// 列是 numeric(20, 8)，存的是 12.34 这个数本身，而不是它的 1e8 倍。
// 传 units 进去会安静地把金额写大八个数量级，而对账时它表现为一笔
// 怎么都找不平的差额。
func nullableAmount(v *money.Amount) any {
	if v == nil {
		return nil
	}
	return v.String()
}

// ResolveUser 把「用户 ID 或用户名」解析成一个用户。
//
// 命令行便利：运维手里通常只有用户名，而 api_keys 认的是 user_id。
//
// 纯数字一律按 ID 解析。于是用户名恰好全是数字时无法按名字指定——
// 那是个取舍：另一种做法（先按名字查、查不到再按 ID）会让
// 「用户名是 12345 的用户」与「ID 为 12345 的用户」静默地互相覆盖。
func (s *Store) ResolveUser(ctx context.Context, ref string) (User, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return User{}, errors.New("用户标识为空")
	}

	var (
		user User
		err  error
	)
	if id, parseErr := strconv.ParseInt(ref, 10, 64); parseErr == nil {
		err = s.db.QueryRowContext(ctx,
			`SELECT id, username FROM users WHERE id = $1`, id,
		).Scan(&user.ID, &user.Username)
	} else {
		err = s.db.QueryRowContext(ctx,
			`SELECT id, username FROM users WHERE username = $1`, ref,
		).Scan(&user.ID, &user.Username)
	}

	if errors.Is(err, sql.ErrNoRows) {
		return User{}, fmt.Errorf("找不到用户 %q", ref)
	}
	if err != nil {
		return User{}, fmt.Errorf("查询用户失败: %w", err)
	}
	return user, nil
}
