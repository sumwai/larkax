package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sumwai/larkax/internal/tokens"
)

// 本文件用的月份，专供这里。
//
// 分区是**库里的全局状态**：本文件建出来的分区会留在库里，别的用例若用同一个
// 月份，两边的清理会互相拆台（一个刚建、另一个正好删掉）。所以挑一组远离种子
// （2026-10 / 2026-11）与其他用例的月份，各条用例之间也不重叠。
var (
	ensureMonth   = time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	writeMonth    = time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC)
	rolloverMonth = time.Date(2027, 7, 1, 0, 0, 0, 0, time.UTC)
	blockedMonth  = time.Date(2027, 8, 1, 0, 0, 0, 0, time.UTC)
	duplicateMon  = time.Date(2027, 9, 1, 0, 0, 0, 0, time.UTC)
)

// ── 纯函数：不碰库 ──────────────────────────────────────────────────────

func TestUsageEventPartitionNameIsUTCMonth(t *testing.T) {
	cases := []struct {
		name string
		at   time.Time
		want string
	}{
		{"月中", time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC), "usage_events_2026_10"},
		{"月初零点", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), "usage_events_2026_01"},
		{"月末最后一刻", time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC), "usage_events_2026_12"},
		{
			// 同一个时刻换个时区表示：本地是 11 月 1 日，UTC 还是 10 月 31 日。
			// 分区的边界由服务器时区决定，而部署地可能不在同一个时区，
			// 所以名字必须按 UTC 算——两边各算一次就会错开一个月，且不报错。
			"东八区的次日凌晨",
			time.Date(2026, 11, 1, 3, 0, 0, 0, time.FixedZone("UTC+8", 8*3600)),
			"usage_events_2026_10",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := UsageEventPartitionName(tc.at); got != tc.want {
				t.Errorf("UsageEventPartitionName(%s) = %q，期望 %q", tc.at, got, tc.want)
			}
		})
	}
}

// 边界必须逐月推进，不能用「30 天」这类近似。
//
// 月份长度不等（28~31 天），近似法会累积漂移：几个季度之后边界就落到别的
// 月份里去了，而那种错误不报错——只是分区覆盖的区间与它的名字不再对应。
func TestUsageEventMonthBoundsCoversWholeCalendarMonth(t *testing.T) {
	cases := []struct {
		name     string
		at       time.Time
		wantFrom string
		wantTo   string
	}{
		{"31 天的月份", time.Date(2026, 1, 15, 9, 0, 0, 0, time.UTC), "2026-01-01T00:00:00Z", "2026-02-01T00:00:00Z"},
		{"28 天的月份", time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC), "2026-02-01T00:00:00Z", "2026-03-01T00:00:00Z"},
		{"30 天的月份", time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC), "2026-04-01T00:00:00Z", "2026-05-01T00:00:00Z"},
		{"跨年", time.Date(2026, 12, 31, 23, 0, 0, 0, time.UTC), "2026-12-01T00:00:00Z", "2027-01-01T00:00:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			from, to := UsageEventMonthBounds(tc.at)
			if got := from.Format(time.RFC3339); got != tc.wantFrom {
				t.Errorf("下界 = %s，期望 %s", got, tc.wantFrom)
			}
			if got := to.Format(time.RFC3339); got != tc.wantTo {
				t.Errorf("上界 = %s，期望 %s", got, tc.wantTo)
			}
		})
	}
}

// 「应当存在哪些月份」要按**日历月**逐月推，且起点归一到一号。
func TestUsageEventMonthsAdvancesByCalendarMonth(t *testing.T) {
	// 起点取月末：不归一的话 31 号往后推一个月会落到下下个月。
	got, err := usageEventMonths(time.Date(2026, 1, 31, 12, 0, 0, 0, time.UTC), 3)
	if err != nil {
		t.Fatalf("算月份列表失败: %v", err)
	}

	want := []time.Time{
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("月份列表 = %v，期望 %v", got, want)
	}

	// 名字也要跟着走：相邻两个月不能算出同一个分区名。
	names := make([]string, 0, len(got))
	for _, m := range got {
		names = append(names, UsageEventPartitionName(m))
	}
	wantNames := []string{"usage_events_2026_01", "usage_events_2026_02", "usage_events_2026_03"}
	if !reflect.DeepEqual(names, wantNames) {
		t.Errorf("分区名 = %v，期望 %v", names, wantNames)
	}
}

func TestUsageEventMonthsRejectsNonPositive(t *testing.T) {
	for _, months := range []int{0, -1} {
		if _, err := usageEventMonths(time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC), months); err == nil {
			t.Errorf("months = %d 应当报错，而不是静默地什么都不建", months)
		}
	}
}

// 口径校验必须发生在碰数据库**之前**。
//
// 传 nil 当 Querier：真去查库的话这里会 panic 而不是返回错误。这条用例因此
// 同时钉住两件事——months <= 0 会报错，以及报错发生在任何一次往返之前。
// 它不需要 LARKAX_DB_DSN。
func TestPartitionMonthsValidatedBeforeTouchingDB(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)

	if _, err := EnsureUsageEventPartitions(ctx, nil, at, 0); err == nil {
		t.Error("EnsureUsageEventPartitions(months=0) 应当报错")
	}
	if _, err := MissingUsageEventPartitions(ctx, nil, at, -3); err == nil {
		t.Error("MissingUsageEventPartitions(months=-3) 应当报错")
	}
}

func TestUsageEventPartitionMonthParsesOnlyMonthShape(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"正常", "usage_events_2026_10", true},
		{"一月", "usage_events_2026_01", true},
		// 长度恰好相同（13 + 7），前缀也对，只有 time.Parse 认不出它。
		// 少那一步，兜底分区会被当成一个合法的月分区。
		{"兜底分区", "usage_events_default", false},
		{"没有补零", "usage_events_2026_1", false},
		{"月份越界", "usage_events_2026_13", false},
		{"月份为零", "usage_events_2026_00", false},
		{"多了一天", "usage_events_2026_10_15", false},
		{"别的表", "request_snapshots_2026_10_15", false},
		{"空串", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			month, ok := UsageEventPartitionMonth(tc.in)
			if ok != tc.want {
				t.Fatalf("UsageEventPartitionMonth(%q) 的 ok = %v，期望 %v", tc.in, ok, tc.want)
			}
			if ok && month.Format(usageEventPartitionLayout) != tc.in[len(usageEventPartitionPrefix):] {
				t.Errorf("解析出 %s，与名字里的月份对不上", month.Format(usageEventPartitionLayout))
			}
			if ok && month.Location() != time.UTC {
				t.Errorf("解析出的月份时区是 %s，期望 UTC", month.Location())
			}
		})
	}
}

// 并发建表的那个分支：只有「这个名字已经被占了」才该被当成成功。
//
// 最要紧的是 23514——兜底分区挡着建不出来时 PostgreSQL 给的就是它。
// 把它也吞掉，等于把一个「要人先把行搬走」的现场报成「建好了」。
func TestIsPartitionAlreadyExists(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"duplicate_table", &pgconn.PgError{Code: pgDuplicateTable}, true},
		{"unique_violation", &pgconn.PgError{Code: pgUniqueViolation}, true},
		{"包了一层", fmt.Errorf("建分区失败: %w", &pgconn.PgError{Code: pgDuplicateTable}), true},
		{
			"兜底分区挡着（真失败，必须上抛）",
			&pgconn.PgError{
				Code:    "23514",
				Message: `updated partition constraint for default partition "usage_events_default" would be violated by some row`,
			},
			false,
		},
		{"连接错误", errors.New("connection refused"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPartitionAlreadyExists(tc.err); got != tc.want {
				t.Errorf("isPartitionAlreadyExists(%v) = %v，期望 %v", tc.err, got, tc.want)
			}
		})
	}
}

// ── 真库 ────────────────────────────────────────────────────────────────

// relationExists 用 to_regclass 判断一个关系在不在。
//
// 刻意**不用** listUsageEventPartitionsSQL：那条查询是被测代码的一部分，
// 拿它当判据等于用被测对象证明它自己——SQL 写错时两边会一起错，而用例照样
// 绿（见 LESSONS.md 第 21 条）。to_regclass 是另一套机制（按名字查目录），
// 与那条 join 没有共同成分。
func relationExists(ctx context.Context, t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var got sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT to_regclass($1)::text`, name).Scan(&got); err != nil {
		t.Fatalf("查关系 %s 是否存在失败: %v", name, err)
	}
	return got.Valid && got.String != ""
}

// usageEventPartitionCount 数 usage_events 下有几个分区。
//
// 走 PostgreSQL 自己的分区树遍历（pg_partition_tree），同样刻意避开被测代码
// 那条 pg_inherits join。它把父表自己也算进去，所以减一。
func usageEventPartitionCount(ctx context.Context, t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM pg_partition_tree('usage_events')`).Scan(&n); err != nil {
		t.Fatalf("数 usage_events 的分区失败: %v", err)
	}
	return n - 1
}

func dropUsageEventPartition(ctx context.Context, t *testing.T, db *sql.DB, name string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, "DROP TABLE IF EXISTS "+name); err != nil {
		t.Fatalf("清理分区 %s 失败: %v", name, err)
	}
}

// countInDefault 数兜底分区里有几行属于这次请求。
func countInDefault(ctx context.Context, t *testing.T, db *sql.DB, requestID string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM `+UsageEventDefaultPartition+` WHERE request_id = $1`,
		requestID).Scan(&n); err != nil {
		t.Fatalf("数兜底分区里的行失败: %v", err)
	}
	return n
}

// insertDefaultUsageEvent 往兜底分区里塞一行用量事件。
//
// 走父表而不是直接写 usage_events_default：PostgreSQL 按 created_at 把它路由
// 到兜底分区。直接写分区测的是「这张分区收不收 INSERT」，走父表测的才是
// 「写入路径会把它放到哪」——而后者正是这条用例的前提。
func insertDefaultUsageEvent(ctx context.Context, t *testing.T, db *sql.DB, requestID string, at time.Time) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO usage_events (
			request_id, user_id, canonical_model, billing_path, cost, asset, priced_at, created_at
		) VALUES ($1, 1, 'gpt-4o', 'measured', 0, 'CNY', $2, $2)`,
		requestID, at); err != nil {
		t.Fatalf("往兜底分区插一行失败: %v", err)
	}
}

func TestEnsureUsageEventPartitionsCreatesMissingThenSkips(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	names := []string{
		UsageEventPartitionName(ensureMonth),
		UsageEventPartitionName(ensureMonth.AddDate(0, 1, 0)),
		UsageEventPartitionName(ensureMonth.AddDate(0, 2, 0)),
	}
	for _, n := range names {
		dropUsageEventPartition(ctx, t, db, n)
	}
	t.Cleanup(func() {
		for _, n := range names {
			dropUsageEventPartition(context.Background(), t, db, n)
		}
	})

	created, err := EnsureUsageEventPartitions(ctx, db, ensureMonth, len(names))
	if err != nil {
		t.Fatalf("预建失败: %v", err)
	}
	if !reflect.DeepEqual(created, names) {
		t.Errorf("第一次建出来的是 %v，期望 %v", created, names)
	}
	for _, n := range names {
		if !relationExists(ctx, t, db, n) {
			t.Errorf("分区 %s 建完之后仍然不存在", n)
		}
	}

	// 幂等：再跑一次一个都不建，库里分区数也不变。
	before := usageEventPartitionCount(ctx, t, db)
	again, err := EnsureUsageEventPartitions(ctx, db, ensureMonth, len(names))
	if err != nil {
		t.Fatalf("第二次预建失败: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("第二次预建建出了 %v，期望一个都不建", again)
	}
	if after := usageEventPartitionCount(ctx, t, db); after != before {
		t.Errorf("第二次预建之后分区数从 %d 变成 %d", before, after)
	}
}

// 真库上确认「并发建同一个分区」会撞的错误码，确实在 isPartitionAlreadyExists
// 认的那两个里面。
//
// 上面那条用例拿合成的 PgError 测判据本身，这一条测的是**判据与真库对得上**：
// PostgreSQL 换个版本改掉错误码的话，并发那条分支会把「别人建好了」当成失败，
// 而那是一次 DDL 报错——它会让写入侧的补建白白失败一整轮。
func TestPartitionAlreadyExistsMatchesPostgres(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	name := UsageEventPartitionName(duplicateMon)
	dropUsageEventPartition(ctx, t, db, name)
	t.Cleanup(func() { dropUsageEventPartition(context.Background(), t, db, name) })

	if _, err := EnsureUsageEventPartitions(ctx, db, duplicateMon, 1); err != nil {
		t.Fatalf("预建失败: %v", err)
	}

	// 再原样建一次，不带 IF NOT EXISTS——模拟并发里输的那一个。
	from, to := UsageEventMonthBounds(duplicateMon)
	_, err := db.ExecContext(ctx, fmt.Sprintf(
		`CREATE TABLE %s PARTITION OF usage_events FOR VALUES FROM ('%s') TO ('%s')`,
		name, from.Format(time.RFC3339), to.Format(time.RFC3339)))
	if err == nil {
		t.Fatal("同名分区重复建居然成功了，这条用例的前提不成立")
	}
	if !isPartitionAlreadyExists(err) {
		t.Errorf("真库给的是 %v；isPartitionAlreadyExists 认不出来，"+
			"并发建表那条分支会把「别人建好了」报成失败", err)
	}
}

// settleOnce 走一遍完整的「预留 → 结算」，返回这次请求的 id 与结算的错误。
func settleOnce(ctx context.Context, t *testing.T, db *sql.DB, s *Store, userID int64) (string, error) {
	t.Helper()
	requestID := reqID(t)
	req := testRequest(ctx, t, db, userID, requestID)
	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "30", "CNY")); err != nil {
		t.Fatalf("预留失败: %v", err)
	}
	charge := billedCharge(t, userID, requestID, "12", tokens.SourceTokenized,
		time.Date(2026, 10, 17, 9, 30, 0, 0, time.UTC))
	return requestID, s.Save(ctx, req, charge)
}

// 写入侧在结算之前把当月分区补出来，而这一行不落进兜底分区。
//
// 时钟指向一个与种子、与其他用例都不重叠的月份，于是「那个月的分区真的被
// 建出来了」可以被直接断言，不必去动系统时钟。
func TestSaveEnsuresUsageEventPartition(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	name := UsageEventPartitionName(writeMonth)
	dropUsageEventPartition(ctx, t, db, name)
	t.Cleanup(func() { dropUsageEventPartition(context.Background(), t, db, name) })

	s := New(db)
	s.now = func() time.Time { return writeMonth }

	userID := seed(t, db, "100")
	requestID, err := settleOnce(ctx, t, db, s, userID)
	if err != nil {
		t.Fatalf("结算失败: %v", err)
	}

	if !relationExists(ctx, t, db, name) {
		t.Fatalf("结算之后 %s 仍然不存在——写入侧没有补建分区", name)
	}

	// 兜底分区非空是「预建漏跑了」的告警，不该由一次正常的结算产生。
	if n := countInDefault(ctx, t, db, requestID); n != 0 {
		t.Errorf("这次结算有 %d 行落进了 %s", n, UsageEventDefaultPartition)
	}
	if n := countUsageEvents(ctx, t, db, requestID); n != 1 {
		t.Errorf("usage_events 里有 %d 行这次结算的记录，期望 1", n)
	}
}

// 补建每个月只做一次：缓存记的是「哪个月已经确认过」。
//
// 判据不是去数 CREATE TABLE 的次数，而是**把分区删掉再结算一次**：缓存命中时
// 写入侧不会再建它，于是「分区还在不在」就把两种行为分开了。
func TestSaveUsageEventPartitionCacheSkipsRepeatedDDL(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	name := UsageEventPartitionName(writeMonth)
	dropUsageEventPartition(ctx, t, db, name)
	t.Cleanup(func() { dropUsageEventPartition(context.Background(), t, db, name) })

	s := New(db)
	s.now = func() time.Time { return writeMonth }
	userID := seed(t, db, "100")

	if _, err := settleOnce(ctx, t, db, s, userID); err != nil {
		t.Fatalf("第一次结算失败: %v", err)
	}
	if !relationExists(ctx, t, db, name) {
		t.Fatalf("第一次结算之后 %s 不存在", name)
	}

	dropUsageEventPartition(ctx, t, db, name)
	if _, err := settleOnce(ctx, t, db, s, userID); err != nil {
		t.Fatalf("第二次结算失败: %v", err)
	}
	if relationExists(ctx, t, db, name) {
		t.Errorf("同一个月的第二次结算又建了一次 %s——缓存没生效，"+
			"热路径上每次结算都在做一次 DDL", name)
	}

	// 跨月之后必须重新确认：缓存记的是「哪个月」，不是一个 bool。
	next := UsageEventPartitionName(rolloverMonth)
	dropUsageEventPartition(ctx, t, db, next)
	t.Cleanup(func() { dropUsageEventPartition(context.Background(), t, db, next) })

	s.now = func() time.Time { return rolloverMonth }
	if _, err := settleOnce(ctx, t, db, s, userID); err != nil {
		t.Fatalf("跨月后的结算失败: %v", err)
	}
	if !relationExists(ctx, t, db, next) {
		t.Errorf("跨月之后 %s 没有被建出来——缓存没有跟着月份走，"+
			"新月份的第一笔结算会落进兜底分区", next)
	}
}

// 补建失败不让结算失败，但必须有人看得见。
//
// 造失败的方式是真的把那个月堵住：往兜底分区里塞一行那个月的事件，于是
// `CREATE TABLE ... PARTITION OF` 会被 PostgreSQL 拒掉（SQLSTATE 23514，
// 见 partition.go 的文件头）。这比拿桩件假装失败更接近现场——它同时证明了
// 那个错误**不会**被 isPartitionAlreadyExists 吞掉。
func TestSaveSettlesWhenPartitionEnsureFails(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	name := UsageEventPartitionName(blockedMonth)
	dropUsageEventPartition(ctx, t, db, name)
	t.Cleanup(func() { dropUsageEventPartition(context.Background(), t, db, name) })

	blocker := reqID(t)
	insertDefaultUsageEvent(ctx, t, db, blocker, blockedMonth)
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(),
			`DELETE FROM `+UsageEventDefaultPartition+` WHERE request_id = $1`, blocker)
	})
	// 前提：阻塞行确实落进了兜底分区。它没落进去的话，下面那次建表根本不会
	// 失败，而这条用例会以「结算成功」的样子绿掉——测的是别的东西。
	if n := countInDefault(ctx, t, db, blocker); n != 1 {
		t.Fatalf("阻塞行没有落进兜底分区（%d 行），这条用例的前提不成立", n)
	}

	var buf bytes.Buffer
	s := New(db)
	s.logger = slog.New(slog.NewTextHandler(&buf, nil))
	s.now = func() time.Time { return blockedMonth }

	userID := seed(t, db, "100")
	requestID, err := settleOnce(ctx, t, db, s, userID)
	if err != nil {
		t.Fatalf("补建分区失败不该让结算失败，实际报错: %v", err)
	}

	// 分区确实没建出来——否则上面那句「结算成功」证明不了什么。
	if relationExists(ctx, t, db, name) {
		t.Fatalf("%s 居然建出来了，这条用例没能造出补建失败", name)
	}

	// 账还是记下来了。这一行落进兜底分区是**已知的后果**，不是这条用例要
	// 消灭的东西：它正是 `larkaxctl partition check` 要报出来的现场。
	if n := countUsageEvents(ctx, t, db, requestID); n != 1 {
		t.Errorf("结算之后 usage_events 里有 %d 行，期望 1", n)
	}

	logged := buf.String()
	if !strings.Contains(logged, "level=ERROR") {
		t.Errorf("补建失败应当记一条 ERROR 日志，实际日志为 %q", logged)
	}
	if !strings.Contains(logged, name) {
		t.Errorf("日志里应当带上出问题的月份 %s，实际为 %q", name, logged)
	}
}

// 补建失败之后不写缓存：下一次结算还要再试一次。
//
// 写了缓存的话，一次瞬时失败会让**整个月**都放弃补建，而没有人会再试。
// 判据与上面那条缓存用例同构——把分区补上，看第二次结算会不会建它。
func TestSaveRetriesPartitionEnsureAfterFailure(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	name := UsageEventPartitionName(blockedMonth)
	dropUsageEventPartition(ctx, t, db, name)
	t.Cleanup(func() { dropUsageEventPartition(context.Background(), t, db, name) })

	blocker := reqID(t)
	insertDefaultUsageEvent(ctx, t, db, blocker, blockedMonth)
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(),
			`DELETE FROM `+UsageEventDefaultPartition+` WHERE request_id = $1`, blocker)
	})

	s := New(db)
	s.now = func() time.Time { return blockedMonth }
	userID := seed(t, db, "100")

	// 第一次：被阻塞行挡着，建不出来。
	if _, err := settleOnce(ctx, t, db, s, userID); err != nil {
		t.Fatalf("第一次结算失败: %v", err)
	}
	if relationExists(ctx, t, db, name) {
		t.Fatalf("%s 居然建出来了，这条用例的前提不成立", name)
	}

	// 把挡路的那一行搬走，再结算一次。失败没有被记成「这个月确认过了」，
	// 所以这一次应当把分区建出来。
	if _, err := db.ExecContext(ctx,
		`DELETE FROM `+UsageEventDefaultPartition+` WHERE request_id = $1`, blocker); err != nil {
		t.Fatalf("搬走阻塞行失败: %v", err)
	}
	if _, err := settleOnce(ctx, t, db, s, userID); err != nil {
		t.Fatalf("第二次结算失败: %v", err)
	}
	if !relationExists(ctx, t, db, name) {
		t.Errorf("障碍排除之后 %s 仍然没被建出来——一次失败让整个月都放弃了补建", name)
	}
}
