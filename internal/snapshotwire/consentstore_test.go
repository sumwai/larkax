package snapshotwire

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// ── 这一组用的假实现 ────────────────────────────────────────────────────
//
// store_test.go 里那个 fakeRows 只认 *string（它服务的是分区清单那条查询），
// 而授权这两条 SQL 扫的是 int64 / int16 / sql.NullTime 这些列。所以这里另起
// 一套：按目标类型逐列回填，列数与目标数对不上就报错——一个静默错位的 Scan
// 会让「读出来的行是另一列的值」看起来完全正常。

// consentRows 是一份写死的结果集。
type consentRows struct {
	rows   [][]any
	i      int
	err    error
	closed bool
}

func (r *consentRows) Next() bool {
	if r.i >= len(r.rows) {
		return false
	}
	r.i++
	return true
}

func (r *consentRows) Scan(dest ...any) error {
	if r.i == 0 || r.i > len(r.rows) {
		return errors.New("没有当前行")
	}
	row := r.rows[r.i-1]
	if len(dest) != len(row) {
		return fmt.Errorf("这一行有 %d 列，扫描目标有 %d 个", len(row), len(dest))
	}
	for i := range dest {
		if err := assignColumn(dest[i], row[i]); err != nil {
			return fmt.Errorf("第 %d 列: %w", i+1, err)
		}
	}
	return nil
}

func (r *consentRows) Err() error   { return r.err }
func (r *consentRows) Close() error { r.closed = true; return nil }

// assignColumn 把一列的值放进扫描目标。
//
// nil 只对可空列（sql.NullTime / sql.NullString）有意义：非空列收到 nil 说明
// 库里的那一行不该长这样，报出来比悄悄填零值好。
func assignColumn(dest, v any) error {
	if v == nil {
		switch d := dest.(type) {
		case *sql.NullTime:
			*d = sql.NullTime{}
			return nil
		case *sql.NullString:
			*d = sql.NullString{}
			return nil
		default:
			return fmt.Errorf("非空列收到 NULL（目标 %T）", dest)
		}
	}
	switch d := dest.(type) {
	case *int64:
		n, ok := v.(int64)
		if !ok {
			return fmt.Errorf("想要 int64，收到 %T", v)
		}
		*d = n
	case *int16:
		n, ok := v.(int16)
		if !ok {
			return fmt.Errorf("想要 int16，收到 %T", v)
		}
		*d = n
	case *string:
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("想要 string，收到 %T", v)
		}
		*d = s
	case *time.Time:
		t, ok := v.(time.Time)
		if !ok {
			return fmt.Errorf("想要 time.Time，收到 %T", v)
		}
		*d = t
	case *sql.NullString:
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("想要 string，收到 %T", v)
		}
		*d = sql.NullString{String: s, Valid: true}
	case *sql.NullTime:
		t, ok := v.(time.Time)
		if !ok {
			return fmt.Errorf("想要 time.Time，收到 %T", v)
		}
		*d = sql.NullTime{Time: t, Valid: true}
	default:
		return fmt.Errorf("不支持的扫描目标 %T", dest)
	}
	return nil
}

// consentFakeDB 记录授权那两条路发出的每一条语句与参数。
type consentFakeDB struct {
	queries  []string
	qargs    [][]any
	execs    []string
	eargs    [][]any
	rows     *consentRows
	queryErr error
	execErr  error

	// affected 是 Exec 报告改了几行，默认 1。
	affected int64
}

func (f *consentFakeDB) ExecContext(_ context.Context, q string, args ...any) (sql.Result, error) {
	f.execs = append(f.execs, q)
	f.eargs = append(f.eargs, args)
	if f.execErr != nil {
		return nil, f.execErr
	}
	return consentResult{f.affected}, nil
}

func (f *consentFakeDB) QueryContext(_ context.Context, q string, args ...any) (Rows, error) {
	f.queries = append(f.queries, q)
	f.qargs = append(f.qargs, args)
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	if f.rows == nil {
		return &consentRows{}, nil
	}
	// 每次查询返回一份新的迭代器：结果集是一次性的。
	fresh := *f.rows
	fresh.i = 0
	return &fresh, nil
}

func (f *consentFakeDB) lastQueryArgs() []any {
	if len(f.qargs) == 0 {
		return nil
	}
	return f.qargs[len(f.qargs)-1]
}

func (f *consentFakeDB) lastExecArgs() []any {
	if len(f.eargs) == 0 {
		return nil
	}
	return f.eargs[len(f.eargs)-1]
}

// consentResult 是能报出「改了几行」的 sql.Result。
type consentResult struct{ n int64 }

func (r consentResult) LastInsertId() (int64, error) { return 0, nil }
func (r consentResult) RowsAffected() (int64, error) { return r.n, nil }

// consentNow 是这一组用例的固定时刻。
var consentNow = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

// newConsentStoreForTest 建一个时钟钉死的授权读写层。
func newConsentStoreForTest(db DB) *ConsentStore {
	s := NewConsentStore(db)
	s.now = func() time.Time { return consentNow }
	return s
}

// ── 申请校验 ────────────────────────────────────────────────────────────

// 不合法的申请一律在写库之前被拒，且一句话说清哪里不对。
//
// 这些错误必须是**参数错**那一类（ConsentInvalidError）：调用方按它给退出码，
// 而「改命令重发」与「去看库」在脚本里是两种处置。
func TestConsentGrantRejectsBadRequests(t *testing.T) {
	future := consentNow.Add(time.Hour)
	window := func(start, end time.Duration) ConsentRequest {
		return ConsentRequest{
			UserID:      1,
			Scope:       ConsentScopeTimeWindow,
			WindowStart: consentNow.Add(start),
			WindowEnd:   consentNow.Add(end),
		}
	}

	cases := []struct {
		name string
		req  ConsentRequest
		max  time.Duration
		want string
	}{
		{"没有用户",
			ConsentRequest{Scope: ConsentScopeSingleRequest, RequestID: "req-1", ExpiresAt: future},
			24 * time.Hour, "必须指向一个用户"},
		{"单条请求缺 request_id",
			ConsentRequest{UserID: 1, Scope: ConsentScopeSingleRequest, ExpiresAt: future},
			24 * time.Hour, "--request-id"},
		{"单条请求带了时间窗",
			ConsentRequest{UserID: 1, Scope: ConsentScopeSingleRequest, RequestID: "req-1",
				ExpiresAt: future, WindowStart: consentNow, WindowEnd: future},
			24 * time.Hour, "不带时间窗"},
		{"单条请求缺到期时间",
			ConsentRequest{UserID: 1, Scope: ConsentScopeSingleRequest, RequestID: "req-1"},
			24 * time.Hour, "到期时间"},
		{"单条请求的到期时间已经过去",
			ConsentRequest{UserID: 1, Scope: ConsentScopeSingleRequest, RequestID: "req-1",
				ExpiresAt: consentNow.Add(-time.Minute)},
			24 * time.Hour, "已经过去"},
		{"时间窗缺一端",
			ConsentRequest{UserID: 1, Scope: ConsentScopeTimeWindow, WindowStart: consentNow},
			24 * time.Hour, "--from 与 --to"},
		{"时间窗反了", window(time.Hour, 0), 24 * time.Hour, "不晚于开始时间"},
		{"时间窗结束在过去", window(-2*time.Hour, -time.Hour), 24 * time.Hour, "已经过去"},
		{"时间窗带了 request_id",
			ConsentRequest{UserID: 1, Scope: ConsentScopeTimeWindow, RequestID: "req-1",
				WindowStart: consentNow, WindowEnd: future},
			24 * time.Hour, "不带 --request-id"},
		{"未知范围",
			ConsentRequest{UserID: 1, Scope: "window"}, 24 * time.Hour, "未知的授权范围"},
		{"上限不是正数",
			ConsentRequest{UserID: 1, Scope: ConsentScopeSingleRequest, RequestID: "req-1", ExpiresAt: future},
			0, "上限必须为正数"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := &consentFakeDB{}
			_, err := newConsentStoreForTest(db).Grant(context.Background(), tc.req, tc.max)
			if err == nil {
				t.Fatal("期望报错，得到 nil")
			}
			var bad *ConsentInvalidError
			if !errors.As(err, &bad) {
				t.Errorf("错误类型 = %T，期望 ConsentInvalidError（参数错那一类）", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("错误 = %q，里面没有 %q", err.Error(), tc.want)
			}
			if len(db.queries) != 0 || len(db.execs) != 0 {
				t.Errorf("不合法的申请不该碰到库：查了 %d 次、写了 %d 次",
					len(db.queries), len(db.execs))
			}
		})
	}
}

// 超过上限的申请被拒绝，错误里同时给出上限与申请值。
//
// 只给其中一个，读的人还得自己去查另一个——而查出来的那个值可能已经被人改过。
// 恰好等于上限要通过：上限是「不超过」，差一个纳秒就拒会把边界变成一件要试
// 才知道的事。
func TestConsentGrantRejectsWindowOverLimit(t *testing.T) {
	const limit = 24 * time.Hour

	t.Run("单条请求超过上限", func(t *testing.T) {
		db := &consentFakeDB{}
		_, err := newConsentStoreForTest(db).Grant(context.Background(), ConsentRequest{
			UserID: 1, Scope: ConsentScopeSingleRequest, RequestID: "req-1",
			ExpiresAt: consentNow.Add(25 * time.Hour),
		}, limit)
		assertOverLimit(t, err, "25 小时", "24 小时")
		if len(db.queries) != 0 {
			t.Error("超限的申请不该写库")
		}
	})

	t.Run("时间窗超过上限", func(t *testing.T) {
		db := &consentFakeDB{}
		_, err := newConsentStoreForTest(db).Grant(context.Background(), ConsentRequest{
			UserID: 1, Scope: ConsentScopeTimeWindow,
			WindowStart: consentNow, WindowEnd: consentNow.Add(30 * time.Hour),
		}, limit)
		assertOverLimit(t, err, "30 小时", "24 小时")
		if len(db.queries) != 0 {
			t.Error("超限的申请不该写库")
		}
	})

	t.Run("恰好等于上限", func(t *testing.T) {
		db := &consentFakeDB{rows: &consentRows{rows: [][]any{{int64(7)}}}}
		id, err := newConsentStoreForTest(db).Grant(context.Background(), ConsentRequest{
			UserID: 1, Scope: ConsentScopeTimeWindow,
			WindowStart: consentNow, WindowEnd: consentNow.Add(limit),
		}, limit)
		if err != nil {
			t.Fatalf("恰好等于上限应当通过，得到 %v", err)
		}
		if id != 7 {
			t.Errorf("id = %d，期望 7", id)
		}
	})
}

func assertOverLimit(t *testing.T, err error, want ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("期望报错，得到 nil")
	}
	var bad *ConsentInvalidError
	if !errors.As(err, &bad) {
		t.Errorf("错误类型 = %T，期望 ConsentInvalidError", err)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("错误 = %q，里面没有 %q", err.Error(), w)
		}
	}
}

// ── 落库的形状 ──────────────────────────────────────────────────────────

// 两个范围各写各的列，另一组留 NULL。
//
// 表上的 shape 约束只要求「该有的那一组非空」，多写一组不会报错——而那时
// 「这条授权到底按什么范围生效」就得读代码才知道。所以这里按参数位逐列钉住。
func TestConsentGrantWritesScopeShape(t *testing.T) {
	t.Run("单条请求", func(t *testing.T) {
		db := &consentFakeDB{rows: &consentRows{rows: [][]any{{int64(11)}}}}
		expires := consentNow.Add(2 * time.Hour)
		id, err := newConsentStoreForTest(db).Grant(context.Background(), ConsentRequest{
			UserID: 3, Scope: ConsentScopeSingleRequest, RequestID: "req-abc", ExpiresAt: expires,
		}, 24*time.Hour)
		if err != nil {
			t.Fatalf("Grant 出错: %v", err)
		}
		if id != 11 {
			t.Errorf("id = %d，期望 11", id)
		}

		args := db.lastQueryArgs()
		if len(args) != 8 {
			t.Fatalf("参数有 %d 个，期望 8 个：%#v", len(args), args)
		}
		if args[0] != int64(3) || args[1] != ConsentScopeSingleRequest || args[2] != "req-abc" {
			t.Errorf("前三列 = %#v，期望 user_id=3 / scope=single_request / request_id=req-abc",
				args[:3])
		}
		if args[3] != nil || args[4] != nil {
			t.Errorf("单条请求不该写时间窗：%#v", args[3:5])
		}
		// 同意文案与版本由本包的常量写死：调用方没有机会写进另一份文案。
		if args[5] != ConsentText || args[6] != ConsentVersion {
			t.Errorf("同意文案与版本 = %#v / %#v，期望包里的常量", args[5], args[6])
		}
		if args[7] != expires {
			t.Errorf("expires_at = %#v，期望 %s", args[7], expires)
		}
	})

	t.Run("时间窗", func(t *testing.T) {
		db := &consentFakeDB{rows: &consentRows{rows: [][]any{{int64(12)}}}}
		start, end := consentNow, consentNow.Add(3*time.Hour)
		_, err := newConsentStoreForTest(db).Grant(context.Background(), ConsentRequest{
			UserID: 4, Scope: ConsentScopeTimeWindow, WindowStart: start, WindowEnd: end,
		}, 24*time.Hour)
		if err != nil {
			t.Fatalf("Grant 出错: %v", err)
		}

		args := db.lastQueryArgs()
		if args[2] != nil {
			t.Errorf("时间窗不该写 request_id：%#v", args[2])
		}
		if args[3] != start || args[4] != end {
			t.Errorf("时间窗 = %#v / %#v，期望 %s / %s", args[3], args[4], start, end)
		}
		// 到期时间就是窗口结束：两列写同一个时刻，撤销与到期两条路各自独立成立。
		if args[7] != end {
			t.Errorf("expires_at = %#v，期望与窗口结束相同（%s）", args[7], end)
		}
	})
}

// 插入没有返回 id 时不能当成写成功了：那会让调用方拿着一个不存在的 id 去
// 打给操作者，而人按那个 id 去撤销会得到「不存在」。
func TestConsentGrantWithoutReturnedIDIsAnError(t *testing.T) {
	db := &consentFakeDB{rows: &consentRows{}}
	_, err := newConsentStoreForTest(db).Grant(context.Background(), ConsentRequest{
		UserID: 1, Scope: ConsentScopeSingleRequest, RequestID: "req-1",
		ExpiresAt: consentNow.Add(time.Hour),
	}, 24*time.Hour)
	if err == nil {
		t.Fatal("没有返回 id 时应当报错")
	}
	if !strings.Contains(err.Error(), "没有返回 id") {
		t.Errorf("错误 = %q，没说清是没拿到 id", err.Error())
	}
}

// ── 读清单 ──────────────────────────────────────────────────────────────

// 可空列读出来是「没有」而不是零值：单条请求的授权没有时间窗，而
// 「没有时间窗」与「时间窗从公元 1 年开始」是两件事。
func TestConsentListReadsNullableColumns(t *testing.T) {
	agreed := consentNow.Add(-time.Hour)
	start, end := consentNow, consentNow.Add(time.Hour)
	revoked := consentNow.Add(-time.Minute)

	db := &consentFakeDB{rows: &consentRows{rows: [][]any{
		{int64(2), int64(3), ConsentScopeTimeWindow, nil, start, end,
			ConsentText, ConsentVersion, agreed, revoked, end},
		{int64(1), int64(4), ConsentScopeSingleRequest, "req-abc", nil, nil,
			ConsentText, ConsentVersion, agreed, nil, consentNow.Add(2 * time.Hour)},
	}}}

	rows, err := newConsentStoreForTest(db).List(context.Background(), 3)
	if err != nil {
		t.Fatalf("List 出错: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("读到 %d 条，期望 2 条", len(rows))
	}
	if args := db.lastQueryArgs(); len(args) != 1 || args[0] != int64(3) {
		t.Errorf("查询参数 = %#v，期望按用户 3 过滤", args)
	}

	win := rows[0]
	if win.WindowStart == nil || !win.WindowStart.Equal(start) || win.WindowEnd == nil || !win.WindowEnd.Equal(end) {
		t.Errorf("时间窗读错了：%#v", win)
	}
	if win.RevokedAt == nil || !win.RevokedAt.Equal(revoked) {
		t.Errorf("撤销时刻读错了：%#v", win.RevokedAt)
	}
	if win.RequestID != "" {
		t.Errorf("时间窗那一行的 request_id 应当是空串，得到 %q", win.RequestID)
	}

	single := rows[1]
	if single.RequestID != "req-abc" {
		t.Errorf("request_id = %q，期望 req-abc", single.RequestID)
	}
	if single.WindowStart != nil || single.WindowEnd != nil {
		t.Errorf("单条请求那一行不该有时间窗：%#v / %#v", single.WindowStart, single.WindowEnd)
	}
	if single.RevokedAt != nil {
		t.Errorf("没撤销的那一行 revoked_at 应当是 nil，得到 %#v", single.RevokedAt)
	}
	if single.ConsentText != ConsentText || single.ConsentVersion != ConsentVersion {
		t.Error("同意文案与版本要一起读回来：这两列是合规举证的依据")
	}
}

// userID 为 0 表示不按用户过滤：清单要能一眼看全。
func TestConsentListWithoutUserReadsAll(t *testing.T) {
	db := &consentFakeDB{rows: &consentRows{}}
	if _, err := newConsentStoreForTest(db).List(context.Background(), 0); err != nil {
		t.Fatalf("List 出错: %v", err)
	}
	if args := db.lastQueryArgs(); len(args) != 1 || args[0] != int64(0) {
		t.Errorf("查询参数 = %#v，期望 0（不过滤）", args)
	}
}

// ── 清单上那一栏状态 ────────────────────────────────────────────────────

// 撤销先于到期：撤销过的记录不再可能生效，无论窗口有没有过完。
func TestConsentStateAt(t *testing.T) {
	start, end := consentNow.Add(-time.Hour), consentNow.Add(time.Hour)
	past := consentNow.Add(-time.Minute)
	future := consentNow.Add(time.Hour)
	revoked := consentNow.Add(-time.Second)

	cases := []struct {
		name string
		c    Consent
		want string
	}{
		{"生效中的时间窗",
			Consent{Scope: ConsentScopeTimeWindow, WindowStart: &start, WindowEnd: &end, ExpiresAt: end},
			ConsentStateActive},
		{"还没开始的时间窗",
			Consent{Scope: ConsentScopeTimeWindow,
				WindowStart: &future, WindowEnd: ptrTime(future.Add(time.Hour)),
				ExpiresAt: future.Add(time.Hour)},
			ConsentStatePending},
		{"过期的单条请求",
			Consent{Scope: ConsentScopeSingleRequest, RequestID: "req-1", ExpiresAt: past},
			ConsentStateExpired},
		{"撤销过的时间窗（窗口还在）",
			Consent{Scope: ConsentScopeTimeWindow, WindowStart: &start, WindowEnd: &end,
				ExpiresAt: end, RevokedAt: &revoked},
			ConsentStateRevoked},
		{"撤销过的单条请求（还没到期）",
			Consent{Scope: ConsentScopeSingleRequest, RequestID: "req-1",
				ExpiresAt: future, RevokedAt: &revoked},
			ConsentStateRevoked},
		{"到期时刻就是此刻",
			Consent{Scope: ConsentScopeSingleRequest, RequestID: "req-1", ExpiresAt: consentNow},
			ConsentStateExpired},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.c.StateAt(consentNow); got != tc.want {
				t.Errorf("StateAt = %q，期望 %q", got, tc.want)
			}
		})
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

// ── 判定 ────────────────────────────────────────────────────────────────

// 判定用的是**传进去的时刻**，不是库里的 now()。
//
// 一次判定里的几处比较必须看同一个时刻；而把时刻收成一个参数，边界用例才能
// 钉在一个固定值上。
func TestConsentSourcePassesOneClock(t *testing.T) {
	db := &consentFakeDB{rows: &consentRows{rows: [][]any{{int64(1)}}}}
	src := NewDBConsentSource(db, quietLogger())
	src.now = func() time.Time { return consentNow }

	if !src.AllowsRawCapture(context.Background(), 5, "req-xyz") {
		t.Fatal("查到了行，应当判为有授权")
	}
	args := db.lastQueryArgs()
	if len(args) != 3 {
		t.Fatalf("查询参数有 %d 个，期望 3 个：%#v", len(args), args)
	}
	if args[0] != int64(5) || args[1] != "req-xyz" {
		t.Errorf("身份参数 = %#v，期望 user_id=5 / request_id=req-xyz", args[:2])
	}
	if args[2] != consentNow {
		t.Errorf("时刻参数 = %#v，期望固定时刻 %s", args[2], consentNow)
	}
}

// 一条都没查到就是没有授权，而且**不记日志**。
//
// 这是正常的判定路径：每次都记一条的实现会在屏幕上与「一切正常」分不出来，
// 而这个日志很快就会被所有人忽略（见 log_test.go 开头那条理由）。它与下面
// 那一条是一对——只判「失败时有日志」的话，一个永远记日志的实现照样通过。
func TestConsentSourceWithoutRowsIsFalse(t *testing.T) {
	db := &consentFakeDB{rows: &consentRows{}}
	logger, buf := captureLogger()
	src := NewDBConsentSource(db, logger)
	if src.AllowsRawCapture(context.Background(), 5, "req-xyz") {
		t.Error("没查到行时应当判为没有授权")
	}
	if out := buf.String(); strings.Contains(out, "WARN") || strings.Contains(out, "ERROR") {
		t.Errorf("正常的判定不该留下日志：%s", out)
	}
}

// 查库失败按「没有授权」处理：落下来的是骨架，而不是原文。
//
// 这是 ConsentSource 那条约定（关不上时朝安全那边倒）的落点。失败要记一条
// WARN——结论永远是安全的那一个，所以「判定一直在失败」这件事只能靠日志被
// 看见；不记的话，一次库抖动与「用户还没授权」在库里长得一模一样。
func TestConsentSourceFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		db   *consentFakeDB
		// want 是这一步的日志里那句话的开头：查库失败与遍历失败各记各的，
		// 混成一条就分不出失败发生在哪一段。
		want string
	}{
		{"查询报错", &consentFakeDB{queryErr: errors.New("库挂了")}, "查询原样采集授权失败"},
		{"遍历报错",
			&consentFakeDB{rows: &consentRows{rows: [][]any{{int64(1)}}, err: errors.New("连接断了")}},
			"读取原样采集授权失败"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logger, buf := captureLogger()
			src := NewDBConsentSource(tc.db, logger)
			if src.AllowsRawCapture(context.Background(), 5, "req-xyz") {
				t.Error("出错时应当判为没有授权")
			}
			out := buf.String()
			// 身份也一起钉：一条不说是谁的 WARN，在排障时只能告诉人「有人失败了」。
			for _, want := range []string{
				tc.want, `"level":"WARN"`, `"user_id":5`, `"request_id":"req-xyz"`,
			} {
				if !strings.Contains(out, want) {
					t.Errorf("日志里没有 %q：%s", want, out)
				}
			}
		})
	}
}

// 没有用户就没有授权记录能对上，一次库都不用查。
func TestConsentSourceSkipsLookupWithoutUser(t *testing.T) {
	db := &consentFakeDB{rows: &consentRows{rows: [][]any{{int64(1)}}}}
	src := NewDBConsentSource(db, quietLogger())
	if src.AllowsRawCapture(context.Background(), 0, "req-xyz") {
		t.Error("没有用户时应当判为没有授权")
	}
	if len(db.queries) != 0 {
		t.Errorf("没有用户时不该查库，实际查了 %d 次", len(db.queries))
	}
}

// 判定那一支自己把空 request_id 挡在放行之外。
//
// 假 DB 只回答「查到了没有」，看不见 WHERE 子句，所以这里的真凭据在集成档
// （request_id 为空串的那条同意记录不放行）。这一条留在单元档里，是为了让
// 「防线还在」在没设 DSN、集成用例整体跳过的时候也看得见：漏掉那一句的方向
// 是 fail-open——凡是拿不到 request id 的请求都被放行原文。
func TestConsentAllowedSQLExcludesEmptyRequestID(t *testing.T) {
	if !strings.Contains(consentAllowedSQL, "request_id <> ''") {
		t.Errorf("判定 SQL 里少了排除空 request_id 的那一句：\n%s", consentAllowedSQL)
	}
}

// ── 撤销 ────────────────────────────────────────────────────────────────

// 撤销只改「还没撤销过」的那一行，返回值如实反映改没改到。
//
// 改到 0 行有两种情形（那一条不在、已经撤销过），这里不替调用方猜是哪种：
// 它只知道没有任何一行被改。
func TestConsentRevokeReportsWhetherAnythingChanged(t *testing.T) {
	t.Run("改到一行", func(t *testing.T) {
		db := &consentFakeDB{affected: 1}
		revoked, err := newConsentStoreForTest(db).Revoke(context.Background(), 7)
		if err != nil || !revoked {
			t.Fatalf("Revoke = (%v, %v)，期望 (true, nil)", revoked, err)
		}
		if args := db.lastExecArgs(); len(args) != 1 || args[0] != int64(7) {
			t.Errorf("参数 = %#v，期望 id=7", args)
		}
	})

	t.Run("一行都没改到", func(t *testing.T) {
		db := &consentFakeDB{affected: 0}
		revoked, err := newConsentStoreForTest(db).Revoke(context.Background(), 7)
		if err != nil {
			t.Fatalf("Revoke 出错: %v", err)
		}
		if revoked {
			t.Error("一行都没改到时应当返回 false")
		}
	})

	t.Run("写库失败", func(t *testing.T) {
		db := &consentFakeDB{execErr: errors.New("库挂了")}
		if _, err := newConsentStoreForTest(db).Revoke(context.Background(), 7); err == nil {
			t.Error("写库失败时应当报错")
		}
	})
}
