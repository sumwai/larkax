package snapshotwire

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sumwai/larkax/internal/textshape"
)

// 授权范围的取值，与 capture_consents.scope 上的 CHECK 一致。
//
// 两个范围对应授权记录上的两种形状（见表上的 capture_consents_scope_shape）：
// 按单条请求（认 request_id）与按一段时间（认窗口的两端）。
const (
	ConsentScopeSingleRequest = "single_request"
	ConsentScopeTimeWindow    = "time_window"
)

// ConsentText 是操作者在落库之前要看到的同意文案。
//
// 它是一段**代码里的常量**，与 ConsentVersion 一起写进 capture_consents 的
// consent_text / consent_version 两列：那两列是拿来做合规举证的，所以
// 「操作者当时同意的是什么」必须能从库里读回来，而它不能由命令行临时拼。
// `larkaxctl consent grant` 打的正是这一段，写入的也是这一段——两处同源，
// 不存在「屏幕上看到的」与「库里记下的」不是同一份的可能。
//
// 改这段文字就要把 ConsentVersion 加一：版本号是这两列唯一能回答
// 「同一句同意、还是改过之后的另一句」的东西，而旧记录里的文案仍是当时那一份。
const ConsentText = `我同意开启请求原文采集。

开启后，授权范围内的请求与响应会原样存进数据库：用户发来的提示词、
模型返回的正文、工具调用的参数与图片数据都在其中，不做脱敏。这些内容
与结构快照在同一张表里，按 request_snapshot.retention_days 的留存期
一起删除。

授权到点自动失效，不需要回来手动关闭；也可以随时撤销，撤销立即生效。
读取这些原文会写进访问日志。`

// ConsentVersion 是 ConsentText 的版本号，落进 smallint 那一列。
const ConsentVersion int16 = 1

// ErrConsentNotFound 表示要撤销的那条授权不在库里，或者已经撤销过。
//
// 两种情形共用一个哨兵：对操作者来说下一步动作是同一条——先看清单里现在
// 是什么，所以命令给它们同一个退出码（见 cmd/larkaxctl 的冲突类）。
var ErrConsentNotFound = errors.New("授权不存在或已经撤销")

// ConsentInvalidError 表示这次申请本身不合法：字段给错、时刻已经过去、
// 申请时长超过上限。
//
// 单独一个类型而不是普通错误，是为了让调用方能把它与「写库失败」分开：
// 前者是参数错（改命令重发），后者要看库。两者的下一步动作不同，而它们
// 在错误信息上可能都只是一句话。
//
// 时长上限只有连上库、读出 settings 才知道，所以这一类校验**不能**像别的
// 参数错那样全部发生在连库之前——调用方按类型认它，而不是按调用顺序。
type ConsentInvalidError struct {
	// Reason 是给人读的那一句话，本身已经说清了哪里不对。
	Reason string
}

func (e *ConsentInvalidError) Error() string { return e.Reason }

// invalid 造一个申请不合法的错误。
func invalid(format string, args ...any) error {
	return &ConsentInvalidError{Reason: fmt.Sprintf(format, args...)}
}

// Consent 是一条授权记录。
//
// 它按库里的列一一对应地摊开，包括同意文案与版本：读清单的人要能回答
// 「这一条当初同意的是什么」，而这句话只有 consent_text 那一列有。
type Consent struct {
	ID             int64
	UserID         int64
	Scope          string
	RequestID      string
	WindowStart    *time.Time
	WindowEnd      *time.Time
	ConsentText    string
	ConsentVersion int16
	AgreedAt       time.Time
	RevokedAt      *time.Time
	ExpiresAt      time.Time
}

// 授权记录在清单上的状态。它只服务于**显示**：判定那件事由 consentAllowedSQL
// 在库里做（见 DBConsentSource），这里的取值是给读清单的人一眼扫的。
const (
	// ConsentStateActive 是此刻真的能换来原文的那一档。
	ConsentStateActive = "active"
	// ConsentStatePending 是时间窗还没开始的那一档。
	ConsentStatePending = "pending"
	// ConsentStateExpired 是过了到期时间的那一档。
	ConsentStateExpired = "expired"
	// ConsentStateRevoked 是被撤销的那一档。
	ConsentStateRevoked = "revoked"
)

// StateAt 给出这条记录在 now 这一刻的显示状态。
//
// 撤销先于到期：撤销过的记录不再可能生效，无论窗口有没有过完——先判它，
// 清单上那一栏才与「撤销立即生效」这条判据一致。
//
// 单条请求的授权没有窗口，它在到期前都是 active：那一条只对某个 request_id
// 生效，而清单上回答的是「这条授权还有没有可能换来原文」，不是「它此刻正在
// 为哪一次请求生效」——后者要拿一个 request_id 来问，是判定的问题。
func (c Consent) StateAt(now time.Time) string {
	switch {
	case c.RevokedAt != nil:
		return ConsentStateRevoked
	case !now.Before(c.ExpiresAt):
		return ConsentStateExpired
	case c.WindowStart != nil && now.Before(*c.WindowStart):
		return ConsentStatePending
	default:
		return ConsentStateActive
	}
}

// ConsentRequest 是一次授权申请。
//
// 三组字段按范围二选一（见 Grant 的校验）：单条请求给 RequestID 与 ExpiresAt，
// 一段时间给 WindowStart 与 WindowEnd。两组都给、或两组都不给，都是参数错。
//
// 同意文案与版本**不在**这里：它们由本包的常量写死（见 ConsentText），
// 调用方没有机会写进另一份文案。
type ConsentRequest struct {
	UserID    int64
	Scope     string
	RequestID string

	// WindowStart / WindowEnd 只在 ConsentScopeTimeWindow 下用。
	WindowStart time.Time
	WindowEnd   time.Time

	// ExpiresAt 只在 ConsentScopeSingleRequest 下用：一条只对某次请求生效的
	// 授权能撑多久。时间窗那一档的到期时间就是窗口结束时间，不必另给。
	ExpiresAt time.Time
}

// ConsentStore 读写 capture_consents。
//
// 读的那一半（判定）在 DBConsentSource 里：那是请求路径上的调用，形状与这里
// 不同——它只回答是或否，且必须往安全那边倒。
type ConsentStore struct {
	db DB

	// now 是判定「申请是否已经过期」「窗口是否已经过去」用的时钟，
	// nil 表示 time.Now。留这个口子是为了让边界用例能钉在一个固定时刻上。
	now func() time.Time
}

// NewConsentStore 建一个授权记录读写层。
func NewConsentStore(db DB) *ConsentStore { return &ConsentStore{db: db} }

// clock 返回本实例的时钟。
func (s *ConsentStore) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// consentInsertSQL 写一条授权。
//
// agreed_at 用库里的 now() 而不是传进来的时刻：那一列是「操作者什么时候同意的」，
// 由库统一给，省得命令行进程的时钟快了几分钟就在合规记录里留下一个说不清的时刻。
// expires_at 反过来由调用方给——它是申请的一部分，校验也在调用方那一侧做完了。
const consentInsertSQL = `
	INSERT INTO capture_consents (
		user_id, scope, request_id, window_start, window_end,
		consent_text, consent_version, expires_at
	) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	RETURNING id`

// Grant 写一条授权，返回它的 id。
//
// # 为什么上限是参数而不是常量
//
// 上限来自 settings 表的 capture.consent_max_window_hours，而这一层不认识
// 配置。调用方读出那个值再传进来，于是「谁在读这个键」在代码里看得见。
//
// # 为什么超限是拒绝而不是夹到上限
//
// 夹一下会让操作者以为自己申请的那段时长已经生效，而实际生效的比它短——
// 短的那一段到期后不再留原文，而那时人已经不在看着它了。所以超限返回错误，
// 错误里同时给出上限与申请值：只给其中一个，读的人还得自己去查另一个。
func (s *ConsentStore) Grant(ctx context.Context, req ConsentRequest, maxWindow time.Duration) (int64, error) {
	if err := req.validate(s.clock(), maxWindow); err != nil {
		return 0, err
	}

	// 两组的列按范围给值，另一组留 NULL：表上的 shape 约束只要求「该有的
	// 那一组非空」，而多写一组会让「这条授权到底按什么范围生效」变成一件
	// 要读代码才知道的事。
	var requestID, windowStart, windowEnd any
	var expiresAt time.Time
	switch req.Scope {
	case ConsentScopeSingleRequest:
		requestID = req.RequestID
		expiresAt = req.ExpiresAt
	case ConsentScopeTimeWindow:
		windowStart, windowEnd = req.WindowStart, req.WindowEnd
		// 窗口结束就是到期：两列写同一个时刻，撤销与到期两条路各自独立成立。
		expiresAt = req.WindowEnd
	}

	rows, err := s.db.QueryContext(ctx, consentInsertSQL,
		req.UserID, req.Scope, requestID, windowStart, windowEnd,
		ConsentText, ConsentVersion, expiresAt)
	if err != nil {
		return 0, fmt.Errorf("写入授权记录失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return 0, fmt.Errorf("写入授权记录失败: %w", err)
		}
		return 0, errors.New("写入授权记录没有返回 id")
	}
	var id int64
	if err := rows.Scan(&id); err != nil {
		return 0, fmt.Errorf("读取新授权记录的 id 失败: %w", err)
	}
	return id, rows.Err()
}

// validate 校验一次申请。
//
// 除时长上限之外都在连库之前就能判完，而**上限那一项不能**：它来自 settings
// 表。所以这里返回的每一句都必须是「改命令就能成」的那种，调用方据此给参数错的
// 退出码（见 ConsentInvalidError）。
func (r ConsentRequest) validate(now time.Time, maxWindow time.Duration) error {
	if r.UserID <= 0 {
		return invalid("授权必须指向一个用户")
	}
	if maxWindow <= 0 {
		return invalid("授权时长上限必须为正数，收到 %s", humanDuration(maxWindow))
	}

	switch r.Scope {
	case ConsentScopeSingleRequest:
		if r.RequestID == "" {
			return invalid("按单条请求授权要给出 --request-id")
		}
		// request_id 同时进 `consent grant` 的回执与 `consent list` 的 RANGE
		// 那一栏：换行会把一栏拆成两行，ESC 会原样进终端。
		if err := textshape.Validate("--request-id", r.RequestID); err != nil {
			return err
		}
		if !r.WindowStart.IsZero() || !r.WindowEnd.IsZero() {
			return invalid("按单条请求授权不带时间窗：换 --scope time_window 才能给 --from / --to")
		}
		if r.ExpiresAt.IsZero() {
			return invalid("按单条请求授权要给出到期时间")
		}
		if !r.ExpiresAt.After(now) {
			return invalid("到期时间 %s 已经过去", r.ExpiresAt.Format(time.RFC3339))
		}
		return checkWindow(r.ExpiresAt.Sub(now), maxWindow, "到期时间")

	case ConsentScopeTimeWindow:
		if r.RequestID != "" {
			return invalid("按时间窗授权不带 --request-id：换 --scope single_request 才能给")
		}
		if r.WindowStart.IsZero() || r.WindowEnd.IsZero() {
			return invalid("按时间窗授权要同时给出 --from 与 --to")
		}
		if !r.WindowEnd.After(r.WindowStart) {
			return invalid("时间窗的结束时间 %s 不晚于开始时间 %s",
				r.WindowEnd.Format(time.RFC3339), r.WindowStart.Format(time.RFC3339))
		}
		if !r.WindowEnd.After(now) {
			return invalid("时间窗的结束时间 %s 已经过去", r.WindowEnd.Format(time.RFC3339))
		}
		return checkWindow(r.WindowEnd.Sub(r.WindowStart), maxWindow, "时间窗长度")

	default:
		return invalid("未知的授权范围 %q：只有 %s 与 %s",
			r.Scope, ConsentScopeSingleRequest, ConsentScopeTimeWindow)
	}
}

// checkWindow 把申请时长与上限比一次，超了就给出一句话的错误。
//
// 上限与申请值都写进错误里：只给其中一个，读的人还得自己去查另一个，而
// 查出来的那个值可能已经被人改过了。
func checkWindow(window, maxWindow time.Duration, what string) error {
	if window <= maxWindow {
		return nil
	}
	return invalid("%s %s 超过上限 %s（settings 的 capture.consent_max_window_hours）",
		what, humanDuration(window), humanDuration(maxWindow))
}

// humanDuration 把时长写成小时数。
//
// 上限配的单位就是小时，申请值也用同一个单位报：报成「1h30m0s」时读的人要
// 自己在脑子里换算一次才能与上限比。
func humanDuration(d time.Duration) string {
	hours := d.Hours()
	if hours == float64(int64(hours)) {
		return fmt.Sprintf("%d 小时", int64(hours))
	}
	return fmt.Sprintf("%.2f 小时", hours)
}

// consentSelectSQL 读授权记录。
//
// $1 为 0 表示不按用户过滤（运维看全部）。排序按 id 倒序：清单回答的是
// 「最近开了哪些授权」，而 id 是唯一能定序的那一列——agreed_at 同秒内的两条
// 分不出先后。
const consentSelectSQL = `
	SELECT id, user_id, scope, request_id, window_start, window_end,
	       consent_text, consent_version, agreed_at, revoked_at, expires_at
	FROM capture_consents
	WHERE ($1 = 0 OR user_id = $1)
	ORDER BY id DESC`

// List 读授权记录，userID 为 0 表示不按用户过滤。
func (s *ConsentStore) List(ctx context.Context, userID int64) ([]Consent, error) {
	rows, err := s.db.QueryContext(ctx, consentSelectSQL, userID)
	if err != nil {
		return nil, fmt.Errorf("读取授权记录失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]Consent, 0, 8)
	for rows.Next() {
		var (
			c                      Consent
			requestID              sql.NullString
			windowStart, windowEnd sql.NullTime
			revokedAt              sql.NullTime
		)
		if err := rows.Scan(&c.ID, &c.UserID, &c.Scope, &requestID,
			&windowStart, &windowEnd, &c.ConsentText, &c.ConsentVersion,
			&c.AgreedAt, &revokedAt, &c.ExpiresAt); err != nil {
			return nil, fmt.Errorf("读取授权记录失败: %w", err)
		}
		c.RequestID = requestID.String
		c.WindowStart = nullTimePtr(windowStart)
		c.WindowEnd = nullTimePtr(windowEnd)
		c.RevokedAt = nullTimePtr(revokedAt)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取授权记录失败: %w", err)
	}
	return out, nil
}

// nullTimePtr 把可空列转成指针：NULL 与零值时刻在这里是两件事
// （「这条授权没有时间窗」与「时间窗从公元 1 年开始」）。
func nullTimePtr(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

// consentRevokeSQL 撤销一条授权。
//
// 只改还没撤销过的那一行：撤销过的时间不能被第二次撤销覆盖——那一列是
// 「这条授权什么时候失效的」，而它一旦写下就是一个事实。
const consentRevokeSQL = `
	UPDATE capture_consents SET revoked_at = now()
	WHERE id = $1 AND revoked_at IS NULL`

// Revoke 撤销一条授权，返回是否真的改了那一行。
//
// 返回 false 的两种情形（那一条不在、已经撤销过）由调用方合成一句话报出去：
// 这里能给出的事实只有「没有任何一行被改」。
func (s *ConsentStore) Revoke(ctx context.Context, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, consentRevokeSQL, id)
	if err != nil {
		return false, fmt.Errorf("撤销授权失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("撤销授权失败: %w", err)
	}
	return n > 0, nil
}

// consentAllowedSQL 判一次请求此刻有没有有效的原样采集授权。
//
// # 三条条件都在读的那一刻判
//
// 撤销、到期、时间窗，三件事都在这一条查询里现判：撤销要立刻生效，靠的正是
// 「读的那一刻再问一次库」，而不是靠某个后台任务把状态改掉。少判一条，就会
// 留下一段「已经不该留原文、却还在留」的窗口。
//
// # request_id 那一支为什么要排除空串
//
// 等值比较在 $2 是空串时会匹配上 request_id 也是空串的那一行，而那一行是一张
// **发给所有请求**的授权——方向是放行，不是拒绝。命令行走不到这里（validate
// 要求 --request-id 非空），但手工插一行就到了，而这一支的代价是整个用户的
// 原文。所以匹配那一支的 WHERE 还要写上「request_id 不等于空串」：空串不算
// NULL，表上的 shape 约束拦不住它。
//
// # 时刻由调用方传进来
//
// 不用库里的 now()：一次判定里的三处比较必须看同一个时刻，而库的 now() 在
// 一条语句里虽然一致，跨语句（比如清单与判定各查一次）就不一定了。传进来
// 还让边界用例能钉在固定时刻上。
const consentAllowedSQL = `
	SELECT 1
	FROM capture_consents
	WHERE user_id = $1
	  AND revoked_at IS NULL
	  AND expires_at > $3
	  AND (
	        (scope = 'single_request' AND request_id = $2 AND request_id <> '')
	     OR (scope = 'time_window' AND window_start <= $3 AND window_end >= $3)
	  )
	LIMIT 1`

// DBConsentSource 从 capture_consents 读授权，回答「这次请求要不要留原文」。
//
// # 每次判定查一次库
//
// 没有缓存，也不加缓存：撤销要立刻生效，而任何一段缓存都正好与这条判据打架
// ——缓存期内撤销过的授权仍在放行原文，而那一窗口有多长取决于缓存，不取决于
// 撤销这个动作。
//
// 这条查询的谓词与表上那个部分索引的定义一致：索引是
// capture_consents_active_idx (user_id, expires_at) WHERE revoked_at IS NULL，
// 这里的 user_id = $1、expires_at > $3 与 revoked_at IS NULL 正落在它上面。
// 规划器走不走它没有在这里验过（本机没有 EXPLAIN 的条件），所以这里只说谓词
// 对得上——那是能验的那一句。
//
// # 出错时朝安全那边倒
//
// 查库失败、结果说不清，一律按「没有授权」返回：落下来的是骨架，而不是原文。
// 这条规则来自 ConsentSource 的注释，这里只是把它落到实处。失败会记一条
// WARN 日志——它不影响这次判定的结论（结论永远是安全的那一个），但「判定
// 一直在失败」这件事本身要能被看见。
type DBConsentSource struct {
	db     DB
	logger *slog.Logger

	// now 是判定用的时钟，nil 表示 time.Now。
	now func() time.Time
}

// NewDBConsentSource 建一个查库的授权来源。
func NewDBConsentSource(db DB, logger *slog.Logger) *DBConsentSource {
	if logger == nil {
		logger = slog.Default()
	}
	return &DBConsentSource{db: db, logger: logger}
}

// AllowsRawCapture 见 ConsentSource。
func (s *DBConsentSource) AllowsRawCapture(ctx context.Context, userID int64, requestID string) bool {
	// 没有用户就没有授权记录能对上（user_id 上有外键，那一列不可能为 0）。
	// 这一句省掉的是一次注定查不到东西的往返，而它在请求路径上。
	if userID <= 0 {
		return false
	}

	now := time.Now()
	if s.now != nil {
		now = s.now()
	}

	rows, err := s.db.QueryContext(ctx, consentAllowedSQL, userID, requestID, now)
	if err != nil {
		s.logger.WarnContext(ctx, "查询原样采集授权失败，按未授权处理",
			"user_id", userID, "request_id", requestID, "err", err)
		return false
	}
	defer func() { _ = rows.Close() }()

	found := rows.Next()
	if err := rows.Err(); err != nil {
		s.logger.WarnContext(ctx, "读取原样采集授权失败，按未授权处理",
			"user_id", userID, "request_id", requestID, "err", err)
		return false
	}
	return found
}

// 编译期断言：查库的授权来源就是那一层要的那个接口。
var _ ConsentSource = (*DBConsentSource)(nil)
