package snapshotwire

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/gateway"
	"github.com/sumwai/larkax/internal/observe"
)

// fakeRows 假装一次查询的结果集。
type fakeRows struct {
	names   []string
	i       int
	scanErr error
	err     error
	closed  bool
}

func (r *fakeRows) Next() bool {
	if r.i >= len(r.names) {
		return false
	}
	r.i++
	return true
}

func (r *fakeRows) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	p, ok := dest[0].(*string)
	if !ok {
		return fmt.Errorf("期望 *string，收到 %T", dest[0])
	}
	*p = r.names[r.i-1]
	return nil
}

func (r *fakeRows) Err() error   { return r.err }
func (r *fakeRows) Close() error { r.closed = true; return nil }

// fakeResult 是一个不需要真实驱动的 sql.Result。
type fakeResult struct{}

func (fakeResult) LastInsertId() (int64, error) { return 0, nil }
func (fakeResult) RowsAffected() (int64, error) { return 1, nil }

// fakeDB 记录下每一次调用，供断言。
type fakeDB struct {
	mu       sync.Mutex
	execs    []string
	args     [][]any
	queries  []string
	rows     *fakeRows
	execErr  error
	queryErr error
}

func (f *fakeDB) ExecContext(_ context.Context, q string, args ...any) (sql.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execs = append(f.execs, q)
	f.args = append(f.args, args)
	if f.execErr != nil {
		return nil, f.execErr
	}
	return fakeResult{}, nil
}

func (f *fakeDB) QueryContext(_ context.Context, q string, _ ...any) (Rows, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, q)
	if f.queryErr != nil {
		return nil, f.queryErr
	}
	if f.rows == nil {
		return &fakeRows{}, nil
	}
	// 每次查询返回一份新的迭代器：结果集是一次性的，复用同一个会让
	// 第二次查询直接看到「已经遍历完」——那是假实现的假象，不是被测行为。
	fresh := *f.rows
	fresh.i = 0
	return &fresh, nil
}

func (f *fakeDB) execCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.execs)
}

func (f *fakeDB) lastExec() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.execs) == 0 {
		return ""
	}
	return f.execs[len(f.execs)-1]
}

// lastArgs 返回最后一次 Exec 的参数。
func (f *fakeDB) lastArgs() []any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.args) == 0 {
		return nil
	}
	return f.args[len(f.args)-1]
}

var testDay = time.Date(2026, 10, 16, 15, 30, 0, 0, time.UTC)

// quietLogger 让预期中的 warn/error 不污染测试输出。
//
// 只丢掉输出，不改变行为：被断言的是返回值与执行过的 SQL，
// 日志本身没有可断言的内容。
func quietLogger() *slog.Logger {
	return observe.NewLoggerWithWriter(io.Discard, "error", "", false)
}

// newTestStore 建一个落库层，日志丢弃。
func newTestStore(db DB) *Store { return NewStore(db, quietLogger()) }

func newSnap() *gateway.Snapshot {
	return &gateway.Snapshot{
		RequestID:     "req-1",
		UserID:        42,
		Direction:     "request",
		Skeleton:      map[string]any{"model": "placeholder"},
		RedactedPaths: []string{"messages[0].content"},
		Version:       1,
	}
}

// ── 命名与边界 ──────────────────────────────────────────────────────────

func TestPartitionName(t *testing.T) {
	cases := []struct {
		in   time.Time
		want string
	}{
		{time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC), "request_snapshots_2026_10_16"},
		{time.Date(2026, 1, 2, 23, 59, 59, 0, time.UTC), "request_snapshots_2026_01_02"},
		// 跨时区时必须落在同一个 UTC 日期上，否则分区名与边界会错开一天。
		{time.Date(2026, 10, 16, 23, 30, 0, 0, time.FixedZone("UTC+9", 9*3600)), "request_snapshots_2026_10_16"},
	}
	for _, tc := range cases {
		if got := PartitionName(tc.in); got != tc.want {
			t.Errorf("PartitionName(%v) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}

func TestDayBounds(t *testing.T) {
	from, to := DayBounds(testDay)
	if !from.Equal(time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("下界 = %v，期望 2026-10-16T00:00:00Z", from)
	}
	if !to.Equal(time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("上界 = %v，期望 2026-10-17T00:00:00Z", to)
	}
	if to.Sub(from).Round(time.Hour) != 24*time.Hour {
		t.Errorf("区间长度 = %v，期望 24h", to.Sub(from))
	}

	// 跨月与跨年的边界：AddDate 与字符串格式都必须给出下一天。
	from, to = DayBounds(time.Date(2026, 12, 31, 12, 0, 0, 0, time.UTC))
	if PartitionName(from) != "request_snapshots_2026_12_31" || PartitionName(to) != "request_snapshots_2027_01_01" {
		t.Errorf("跨年边界 = %v..%v", from, to)
	}
}

func TestPartitionDate(t *testing.T) {
	ok := []struct {
		in   string
		want time.Time
	}{
		{"request_snapshots_2026_10_16", time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)},
		{"request_snapshots_2026_01_02", time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)},
	}
	for _, tc := range ok {
		got, valid := PartitionDate(tc.in)
		if !valid || !got.Equal(tc.want) {
			t.Errorf("PartitionDate(%q) = %v, %v；期望 %v, true", tc.in, got, valid, tc.want)
		}
	}

	// 这些名字都必须被拒绝：它们会落进兜底分区或别的表，
	// 而 DROP 一个猜错的名字不可逆。
	bad := []string{
		"request_snapshots_default",
		"request_snapshots",
		"request_snapshots_2026_10",
		"request_snapshots_2026_10_16_extra",
		"request_snapshots_2026_13_01",
		"request_snapshots_2026_10_32",
		"usage_events_2026_10_16",
		"request_snapshots_20261016",
		"",
	}
	for _, name := range bad {
		if _, valid := PartitionDate(name); valid {
			t.Errorf("PartitionDate(%q) 应当返回 false", name)
		}
	}
}

// 分区名与日期之间必须来回一致：一处按 UTC 算、另一处不按 UTC 算的话，
// 跨时区部署时补建的分区与清理时判定的分区会错开一天。
func TestNameAndDateRoundTrip(t *testing.T) {
	at := time.Date(2026, 10, 16, 3, 0, 0, 0, time.FixedZone("UTC-8", -8*3600))
	// 当地时间 03:00 -08:00 是 UTC 11:00，同一天。
	name := PartitionName(at)
	day, ok := PartitionDate(name)
	if !ok {
		t.Fatalf("自己产出的名字解析失败: %q", name)
	}
	from, _ := DayBounds(at)
	if !day.Equal(from) {
		t.Errorf("解析出的日期 %v 与边界下界 %v 不一致", day, from)
	}
}

// ── 写入 ────────────────────────────────────────────────────────────────

func TestInsertWritesAllColumns(t *testing.T) {
	db := &fakeDB{}
	s := newTestStore(db)

	if err := s.Insert(context.Background(), newSnap()); err != nil {
		t.Fatalf("Insert 出错: %v", err)
	}

	// 第一次 Exec 是补建分区，第二次才是插入。
	if db.execCount() != 2 {
		t.Fatalf("Exec 次数 = %d，期望 2（补建分区 + 插入）", db.execCount())
	}
	if !strings.HasPrefix(db.execs[0], "CREATE TABLE IF NOT EXISTS request_snapshots_") {
		t.Errorf("第一次 Exec 应当是建分区，实际为 %q", db.execs[0])
	}
	if !strings.Contains(db.execs[1], "INSERT INTO request_snapshots") {
		t.Errorf("第二次 Exec 应当是插入，实际为 %q", db.execs[1])
	}
	// 这两列**故意不写**：当前只采请求方向的快照，写入发生在调上游之前，
	// 那时上游结局还没发生。NULL 在这些行上读作「这一行不适用」，不是
	// 「上游没回状态码」；同一次请求的上游结局记在 billing_requests 上。
	// 哪天真接了响应方向的采集，这条断言会先失败，提醒改的人把语义一起改。
	for _, col := range []string{"upstream_status", "error_kind"} {
		if strings.Contains(db.execs[1], col) {
			t.Errorf("请求快照的插入不该写 %s（它取自调上游之前，结局还没发生）: %q", col, db.execs[1])
		}
	}

	args := db.lastArgs()
	// 插入的列顺序：request_id, user_id, direction, skeleton_version,
	// capture_mode, skeleton, skeleton_truncated, redacted_paths, created_at
	if len(args) != 9 {
		t.Fatalf("插入参数 %d 个，期望 9", len(args))
	}
	if args[0] != "req-1" || args[1] != int64(42) || args[2] != "request" {
		t.Errorf("前三列 = %v, %v, %v", args[0], args[1], args[2])
	}
	if args[3] != 1 {
		t.Errorf("skeleton_version = %v，期望 1", args[3])
	}
	if args[4] != "skeleton" {
		t.Errorf("capture_mode = %v，期望 skeleton", args[4])
	}
	if args[6] != false {
		t.Errorf("skeleton_truncated = %v，期望 false", args[6])
	}
	if got, ok := args[7].([]string); !ok || len(got) != 1 || got[0] != "messages[0].content" {
		t.Errorf("redacted_paths = %#v", args[7])
	}
	// created_at 由应用给出，而不是交给列上的 DEFAULT now()：补建的是
	// 应用算出来的那天的分区，写入的时刻必须同源，否则 UTC 午夜附近
	// 两者会差一天，行就落进兜底分区了。
	createdAt, ok := args[8].(time.Time)
	if !ok {
		t.Fatalf("created_at = %#v，期望 time.Time", args[8])
	}
	if PartitionName(createdAt) != dayOf(t, db.execs[0]) {
		t.Errorf("写入时刻 %v 与补建的分区 %q 不同源", createdAt, db.execs[0])
	}
}

// dayOf 从建分区的语句里取出分区名。
func dayOf(t *testing.T, ddl string) string {
	t.Helper()
	const marker = "CREATE TABLE IF NOT EXISTS "
	i := strings.Index(ddl, marker)
	if i < 0 {
		t.Fatalf("不是建分区语句: %q", ddl)
	}
	rest := ddl[i+len(marker):]
	j := strings.Index(rest, " ")
	if j < 0 {
		t.Fatalf("建分区语句里取不到表名: %q", ddl)
	}
	return rest[:j]
}

// nil 与空切片在 text[] 列上是两回事：写 NULL 会让
// `redacted_paths @> ...` 这类查询静默漏掉这些行。
func TestInsertNilPathsBecomesEmptyArray(t *testing.T) {
	db := &fakeDB{}
	s := newTestStore(db)

	snap := newSnap()
	snap.RedactedPaths = nil
	if err := s.Insert(context.Background(), snap); err != nil {
		t.Fatalf("Insert 出错: %v", err)
	}

	paths, ok := db.lastArgs()[7].([]string)
	if !ok {
		t.Fatalf("redacted_paths = %#v，期望 []string", db.lastArgs()[7])
	}
	if paths == nil || len(paths) != 0 {
		t.Errorf("redacted_paths = %#v，期望非 nil 的空切片", paths)
	}
}

func TestInsertRejectsNilSnapshot(t *testing.T) {
	db := &fakeDB{}
	s := newTestStore(db)
	if err := s.Insert(context.Background(), nil); err == nil {
		t.Error("空快照应当报错")
	}
	if db.execCount() != 0 {
		t.Errorf("空快照不该产生任何 SQL，实际执行了 %d 次", db.execCount())
	}
}

// 序列化不了说明骨架里有非 JSON 值——那是脱敏器的 bug。
// 这里报出来而不是存一个空对象：空骨架看起来像「这次请求什么都没发」。
func TestInsertFailsOnUnserializableSkeleton(t *testing.T) {
	db := &fakeDB{}
	s := newTestStore(db)

	snap := newSnap()
	snap.Skeleton = map[string]any{"bad": make(chan int)}
	err := s.Insert(context.Background(), snap)
	if err == nil {
		t.Fatal("不可序列化的骨架应当报错")
	}
	if !strings.Contains(err.Error(), "骨架无法序列化") {
		t.Errorf("错误信息应说明是序列化失败，实际为 %v", err)
	}
	for _, q := range db.execs {
		if strings.Contains(q, "INSERT") {
			t.Error("序列化失败时不该执行插入")
		}
	}
}

func TestInsertReportsExecError(t *testing.T) {
	db := &fakeDB{execErr: errors.New("库挂了")}
	s := newTestStore(db)
	err := s.Insert(context.Background(), newSnap())
	if err == nil {
		t.Fatal("Exec 失败时应当报错")
	}
	if !errors.Is(err, db.execErr) {
		t.Errorf("错误应当包住底层错误，实际为 %v", err)
	}
}

// 补建失败时仍然尝试插入：分区可能刚被别人建好，或者表根本不是分区表。
// 「补建失败但写进去了」与「补建失败且写不进去」是两种现场。
func TestInsertStillAttemptsWhenPartitionDDLFails(t *testing.T) {
	db := &fakeDB{}
	first := &failFirstDB{fake: db, failFirst: true}
	s := newTestStore(first)

	if err := s.Insert(context.Background(), newSnap()); err != nil {
		t.Fatalf("插入本身成功时不该报错: %v", err)
	}
	inserts := 0
	for _, q := range db.execs {
		if strings.Contains(q, "INSERT INTO request_snapshots") {
			inserts++
		}
	}
	if inserts != 1 {
		t.Errorf("补建失败后应当仍插入一次，实际 %d 次（执行的语句：%v）", inserts, db.execs)
	}
}

// failFirstDB 让第一次 Exec 失败，之后正常。
type failFirstDB struct {
	fake      *fakeDB
	failFirst bool
	calls     int
}

func (f *failFirstDB) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	f.calls++
	if f.calls == 1 && f.failFirst {
		// 记下这次调用，让断言仍能看到它发生过。
		f.fake.mu.Lock()
		f.fake.execs = append(f.fake.execs, q)
		f.fake.args = append(f.fake.args, args)
		f.fake.mu.Unlock()
		return nil, errors.New("没有建表权限")
	}
	return f.fake.ExecContext(ctx, q, args...)
}

func (f *failFirstDB) QueryContext(ctx context.Context, q string, args ...any) (Rows, error) {
	return f.fake.QueryContext(ctx, q, args...)
}

// 一天之内只补建一次：热路径上不该有反复的 DDL 往返。
func TestEnsurePartitionCachedPerDay(t *testing.T) {
	db := &fakeDB{}
	s := newTestStore(db)

	for i := 0; i < 5; i++ {
		if err := s.Insert(context.Background(), newSnap()); err != nil {
			t.Fatalf("第 %d 次 Insert 出错: %v", i, err)
		}
	}
	ddl := 0
	for _, q := range db.execs {
		if strings.HasPrefix(q, "CREATE TABLE IF NOT EXISTS") {
			ddl++
		}
	}
	if ddl != 1 {
		t.Errorf("补建分区执行 %d 次，期望 1（按天缓存）", ddl)
	}
}

// 跨天之后必须重新确认一次，否则新的一天里第一条快照会落进兜底分区。
func TestEnsurePartitionRechecksAfterDayChanges(t *testing.T) {
	db := &fakeDB{}
	s := newTestStore(db)

	day1 := time.Date(2026, 10, 16, 1, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 10, 17, 1, 0, 0, 0, time.UTC)

	if err := s.ensurePartition(context.Background(), day1); err != nil {
		t.Fatal(err)
	}
	if err := s.ensurePartition(context.Background(), day1); err != nil {
		t.Fatal(err)
	}
	if db.execCount() != 1 {
		t.Fatalf("同一天内 Exec 次数 = %d，期望 1", db.execCount())
	}
	if err := s.ensurePartition(context.Background(), day2); err != nil {
		t.Fatal(err)
	}
	if db.execCount() != 2 {
		t.Fatalf("跨天后 Exec 次数 = %d，期望 2", db.execCount())
	}
	if !strings.Contains(db.lastExec(), "request_snapshots_2026_10_17") {
		t.Errorf("跨天后的 DDL 应当指向新的一天，实际为 %q", db.lastExec())
	}
}

// DDL 的边界必须与 DayBounds 一致，否则分区会盖错时间段。
func TestPartitionDDLUsesDayBounds(t *testing.T) {
	db := &fakeDB{}
	s := newTestStore(db)

	at := time.Date(2026, 10, 16, 15, 30, 0, 0, time.UTC)
	if err := s.ensurePartition(context.Background(), at); err != nil {
		t.Fatal(err)
	}
	stmt := db.lastExec()
	if !strings.Contains(stmt, "FROM ('2026-10-16T00:00:00Z')") {
		t.Errorf("DDL 下界不对: %q", stmt)
	}
	if !strings.Contains(stmt, "TO ('2026-10-17T00:00:00Z')") {
		t.Errorf("DDL 上界不对: %q", stmt)
	}
	if !strings.Contains(stmt, "PARTITION OF request_snapshots") {
		t.Errorf("DDL 没挂在快照表下: %q", stmt)
	}
}

// ── 清理 ────────────────────────────────────────────────────────────────

func TestDropExpiredRemovesOnlyExpiredNamedPartitions(t *testing.T) {
	db := &fakeDB{rows: &fakeRows{names: []string{
		"request_snapshots_2026_09_01",
		"request_snapshots_2026_09_15",
		// 恰好卡在到期线上：留存 30 天、今天 10-16，到期线是 09-16，
		// 于是 09-16 当天保留、09-15 删除。
		"request_snapshots_2026_09_16",
		"request_snapshots_2026_10_16",
		"request_snapshots_default",
	}}}
	s := newTestStore(db)

	dropped, err := s.DropExpired(context.Background(), 30, testDay)
	if err != nil {
		t.Fatalf("DropExpired 出错: %v", err)
	}

	want := []string{"request_snapshots_2026_09_01", "request_snapshots_2026_09_15"}
	if len(dropped) != len(want) {
		t.Fatalf("删除了 %v，期望 %v", dropped, want)
	}
	for i := range want {
		if dropped[i] != want[i] {
			t.Errorf("第 %d 个被删的分区 = %q，期望 %q", i, dropped[i], want[i])
		}
	}

	for _, q := range db.execs {
		if strings.Contains(q, "request_snapshots_default") {
			t.Errorf("兜底分区绝不能被删，实际执行了 %q", q)
		}
		if strings.Contains(q, "request_snapshots_2026_09_16") ||
			strings.Contains(q, "request_snapshots_2026_10_16") {
			t.Errorf("未到期的分区不该被删，实际执行了 %q", q)
		}
	}
}

// 演练与执行必须给出同一份清单。
func TestExpiredPartitionsMatchesDrop(t *testing.T) {
	names := []string{
		"request_snapshots_2026_09_01",
		"request_snapshots_2026_09_16",
		"request_snapshots_2026_10_16",
	}
	dry := newTestStore(&fakeDB{rows: &fakeRows{names: names}})
	listed, err := dry.ExpiredPartitions(context.Background(), 30, testDay)
	if err != nil {
		t.Fatal(err)
	}

	db := &fakeDB{rows: &fakeRows{names: names}}
	real := newTestStore(db)
	dropped, err := real.DropExpired(context.Background(), 30, testDay)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Join(listed, ",") != strings.Join(dropped, ",") {
		t.Errorf("演练列出 %v，实际删除 %v；两者必须一致", listed, dropped)
	}
	if len(listed) != 1 || listed[0] != "request_snapshots_2026_09_01" {
		t.Errorf("演练清单 = %v，期望只含 2026_09_01", listed)
	}
}

// 到期线是「哪些分区到期」的定义处：清理的筛选与命令行打印的那句说明都从它取。
//
// 上面那条用例把「09-16 当天保留、09-15 删除」钉在了删除行为上，这一条把同一条
// 线钉在函数本身上——`snapshot cleanup` 在没有到期分区时打出来的就是这个数，
// 两处各算一遍的话，命令说的到期线会与实际删的错开一天，而那种错误不报错。
func TestRetentionCutoffIsTheExpiryLine(t *testing.T) {
	cases := []struct {
		days int
		want time.Time
	}{
		{30, time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)},
		{1, time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		got := RetentionCutoff(tc.days, testDay)
		if !got.Equal(tc.want) {
			t.Errorf("RetentionCutoff(%d, %s) = %s，期望 %s", tc.days, testDay, got, tc.want)
		}
		if got.Location() != time.UTC {
			t.Errorf("到期线应当是 UTC 零点，实际时区为 %s", got.Location())
		}
	}
}

// 留存期非正时拒绝执行：静默按一个默认值删，会删掉本来该留的数据。
func TestRetentionMustBePositive(t *testing.T) {
	for _, days := range []int{0, -1, -30} {
		db := &fakeDB{rows: &fakeRows{names: []string{"request_snapshots_2020_01_01"}}}
		s := newTestStore(db)
		if _, err := s.DropExpired(context.Background(), days, testDay); err == nil {
			t.Errorf("留存期 %d 应当报错", days)
		}
		if _, err := s.ExpiredPartitions(context.Background(), days, testDay); err == nil {
			t.Errorf("留存期 %d 时应报错（演练同样不该给出一份清单）", days)
		}
		if db.execCount() != 0 {
			t.Errorf("留存期 %d 时不该执行任何 DDL", days)
		}
	}
}

// 名字对不上的分区只记日志、不删，也不让整个清理失败。
func TestUnrecognizedNameIsSkippedNotFatal(t *testing.T) {
	db := &fakeDB{rows: &fakeRows{names: []string{
		"request_snapshots_index_backup",
		"request_snapshots_2026_09_01",
	}}}
	s := newTestStore(db)

	dropped, err := s.DropExpired(context.Background(), 30, testDay)
	if err != nil {
		t.Fatalf("一个不认识的名字不该让清理失败: %v", err)
	}
	if len(dropped) != 1 || dropped[0] != "request_snapshots_2026_09_01" {
		t.Errorf("删除清单 = %v，期望只含 2026_09_01", dropped)
	}
}

func TestDropExpiredReportsQueryError(t *testing.T) {
	db := &fakeDB{queryErr: errors.New("目录读不了")}
	s := newTestStore(db)
	if _, err := s.DropExpired(context.Background(), 30, testDay); err == nil {
		t.Fatal("列出分区失败时应当报错")
	}
}

func TestDropExpiredReportsScanError(t *testing.T) {
	db := &fakeDB{rows: &fakeRows{names: []string{"request_snapshots_2026_09_01"}, scanErr: errors.New("扫描失败")}}
	s := newTestStore(db)
	if _, err := s.DropExpired(context.Background(), 30, testDay); err == nil {
		t.Fatal("读取分区名失败时应当报错")
	}
}

func TestDropExpiredReportsIterationError(t *testing.T) {
	db := &fakeDB{rows: &fakeRows{
		names: []string{"request_snapshots_2026_09_01"},
		err:   errors.New("连接中断"),
	}}
	s := newTestStore(db)
	if _, err := s.DropExpired(context.Background(), 30, testDay); err == nil {
		t.Fatal("遍历出错时应当报错，不能默默认为只剩这些分区")
	}
}

// 一个分区删不掉时停下并报错，但已删的清单要交回调用方——
// 「删了哪些」必须说得清，下一次运行会重试剩下的（DROP 是幂等的）。
func TestDropExpiredStopsAndReportsPartialList(t *testing.T) {
	db := &dropFailDB{
		fakeDB: &fakeDB{rows: &fakeRows{names: []string{
			"request_snapshots_2026_09_01",
			"request_snapshots_2026_09_02",
		}}},
		failOn: "request_snapshots_2026_09_02",
	}
	s := newTestStore(db)

	dropped, err := s.DropExpired(context.Background(), 30, testDay)
	if err == nil {
		t.Fatal("删除失败时应当报错")
	}
	if len(dropped) != 1 || dropped[0] != "request_snapshots_2026_09_01" {
		t.Errorf("已删清单 = %v，期望含且仅含 2026_09_01", dropped)
	}
}

// dropFailDB 让针对某个分区的 DROP 失败。
type dropFailDB struct {
	fakeDB *fakeDB
	failOn string
}

func (d *dropFailDB) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	if strings.Contains(q, d.failOn) && strings.HasPrefix(q, "DROP TABLE") {
		return nil, errors.New("分区被占用")
	}
	return d.fakeDB.ExecContext(ctx, q, args...)
}

func (d *dropFailDB) QueryContext(ctx context.Context, q string, args ...any) (Rows, error) {
	return d.fakeDB.QueryContext(ctx, q, args...)
}

// 列表按名字排序返回：清理的输出会被打进日志与终端，
// 顺序随系统目录返回会让人以为两次运行删的东西不一样。
func TestExpiredListIsSorted(t *testing.T) {
	db := &fakeDB{rows: &fakeRows{names: []string{
		"request_snapshots_2026_09_02",
		"request_snapshots_2026_09_01",
		"request_snapshots_2026_08_30",
	}}}
	s := newTestStore(db)
	got, err := s.ExpiredPartitions(context.Background(), 30, testDay)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"request_snapshots_2026_08_30",
		"request_snapshots_2026_09_01",
		"request_snapshots_2026_09_02",
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("清单 = %v，期望 %v", got, want)
		}
	}
}

// 目录查询必须挂在快照表下：不加这个条件会把别的分区表也列出来，
// 而那些表的名字可能恰好符合形状。
func TestListPartitionsScopedToSnapshotTable(t *testing.T) {
	db := &fakeDB{}
	s := newTestStore(db)
	if _, err := s.DropExpired(context.Background(), 30, testDay); err != nil {
		t.Fatal(err)
	}
	if len(db.queries) != 1 {
		t.Fatalf("查询次数 = %d，期望 1", len(db.queries))
	}
	q := db.queries[0]
	if !strings.Contains(q, "request_snapshots") || !strings.Contains(q, "pg_inherits") {
		t.Errorf("目录查询应当限定在快照表下，实际为 %q", q)
	}
}
