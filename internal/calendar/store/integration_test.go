package store

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/calendar"
	"github.com/sumwai/larkax/internal/testdb"
)

// 集成测试需要一个真实的 PostgreSQL。
//
// # 这一层测的是「判定到底从哪来」
//
// 本包要钉住的是：库里的行读出来之后，判定与内存数据源给出的一样；没有数据的
// 那一年走的是「未知」而不是「工作日」；同一年再灌一次是覆盖。这三件事都发生在
// SQL 与映射的接缝上——拿内存假装数据库，测的是「我以为数据库会怎么做」。
//
// 没有 LARKAX_DB_DSN 时跳过而不是失败：本机未必起着库。
// **注意「跳过」与「通过」在输出里长得一样**，CI 里另有一道门禁确认这个包
// 真的跑了（见 .github/actions/check-integration-tests）。
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	return testdb.Open(t)
}

// TestMain 给这个包建一个独立的库，跑完删掉（见 internal/testdb）。
func TestMain(m *testing.M) { os.Exit(testdb.Main(m)) }

// newYear 给每个用例一个互不相同的年份，并在用例结束时把它从库里清掉。
//
// 年份必须互不相同：日历表的主键是 (region, day)，而地区目前只有 CN——同一年里
// 两个用例写的数据会直接撞上。序列取「时间戳打底 + 进程内计数」，纯计数在同一个
// 库上跑第二次 `go test` 时会从头开始，撞上上一次留下的行。
//
// 年份取在 2500 附近而不是当下：这些是编出来的测试数据，落在未来太远的地方不会
// 被任何「今年/明年」的判断误伤，也一眼能看出不是真实数据。
//
// **序列刻意避开闰年**：用例里写着 365 这样的字面量，而闰年会让它变成 366。
// 基年取一个不是 4 的倍数的年份，再每次加 4，于是「是不是闰年」在整条序列上是
// 固定的（year%4 不变），不随运行时刻漂移。
//
// 结束时清理与选年份同样要紧：打底的年份只有有限的取值，两次运行撞上同一个
// 基年是可能的。那时上一次留下的行会让「这一年什么都没有」这类用例读到别人的
// 数据，而它报出来的是一句看不出所以然的断言失败。
func newYear(t *testing.T, db *sql.DB) int {
	t.Helper()

	seq.Lock()
	seq.n++
	year := baseYear + 4*seq.n
	seq.Unlock()

	t.Cleanup(func() { cleanupYear(t, db, year) })
	return year
}

// cleanupYear 把一个年份来源写下的数据行与它的年份状态从库里清掉。
//
// 单独一个函数是为了让「用到相邻年份」的用例也能清掉那一侧：newYear 只认它
// 自己发的那个年份，而跨年用例会往 year-1 里写行。
func cleanupYear(t *testing.T, db *sql.DB, year int) {
	t.Helper()
	ctx := context.Background()
	// 按 source_year 删数据行：那一年写下的每一行（含跨到相邻年份的那几天）
	// 都带着这个年份来源，所以这一条覆盖得住。
	if _, err := db.ExecContext(ctx,
		`DELETE FROM calendar_days WHERE region = 'CN' AND source_year = $1`, year); err != nil {
		t.Errorf("清理 %d 年的数据行失败: %v", year, err)
	}
	if _, err := db.ExecContext(ctx,
		`DELETE FROM calendar_years WHERE region = 'CN' AND year = $1`, year); err != nil {
		t.Errorf("清理 %d 年的年份状态失败: %v", year, err)
	}
}

var seq struct {
	sync.Mutex
	n int
}

var baseYear = 2501 + int(time.Now().UnixNano()%1000)*4

func importDoc(t *testing.T, db *sql.DB, doc Document) Result {
	t.Helper()
	res, err := NewAdmin(db).Import(context.Background(), doc)
	if err != nil {
		t.Fatalf("Import(%s %d) 出错: %v", doc.Region, doc.Year, err)
	}
	return res
}

func newCalendar(t *testing.T, db *sql.DB) *calendar.Calendar {
	t.Helper()
	cal, err := calendar.New(NewSource(db))
	if err != nil {
		t.Fatalf("构造 Calendar 出错: %v", err)
	}
	return cal
}

func lookup(t *testing.T, cal *calendar.Calendar, date calendar.Date) calendar.DayStatus {
	t.Helper()
	status, err := cal.Lookup(context.Background(), calendar.RegionCN, date)
	if err != nil {
		t.Fatalf("Lookup(%s) 出错: %v", date, err)
	}
	return status
}

// countYearRows 数出某个年份来源写下的行数。
func countYearRows(t *testing.T, db *sql.DB, year int) int {
	t.Helper()
	var n int
	err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM calendar_days WHERE region = 'CN' AND source_year = $1`, year).Scan(&n)
	if err != nil {
		t.Fatalf("统计 %d 年的行数失败: %v", year, err)
	}
	return n
}

// countDayRow 数出某一天在库里有几行。
func countDayRow(t *testing.T, db *sql.DB, date calendar.Date) int {
	t.Helper()
	var n int
	err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM calendar_days WHERE region = 'CN' AND day = $1::date`, date.String()).Scan(&n)
	if err != nil {
		t.Fatalf("统计 %s 的行数失败: %v", date, err)
	}
	return n
}

// yearState 读某年的状态，出错就失败。
func yearState(t *testing.T, db *sql.DB, year int) calendar.YearState {
	t.Helper()
	state, err := NewSource(db).YearState(context.Background(), calendar.RegionCN, year)
	if err != nil {
		t.Fatalf("YearState(%d) 出错: %v", year, err)
	}
	return state
}

// weekdayAfter 找某年里第一个星期几是 wd、且不等于 skip 的日子。
//
// 「且不等于 skip」是必要的：元旦可能正好落在那一天上，而用例会把它改成另一
// 种判定——不避开的话，那条「普通周日是休息日」的断言就会读到被覆盖过的值。
func weekdayAfter(t *testing.T, year int, wd time.Weekday, skip calendar.Date) calendar.Date {
	t.Helper()
	for _, date := range yearDays(year) {
		if date.Weekday() == wd && date != skip {
			return date
		}
	}
	t.Fatalf("%d 年里找不到不是 %s 的星期 %v", year, skip, wd)
	return calendar.Date{}
}

// ── 从库里读出来的判定，与内存数据源一致 ────────────────────────────────

// 这是本包最要紧的一条：**判定来自哪张表、怎么读，不该改变结论**。
//
// 内存实现（internal/calendar/memory.go）是判定逻辑的对照物——它没有 SQL、
// 没有映射、没有时区换算，同一份数据喂给它，一年里的每一天都该给出同一个
// 结果。任何一处「读出来差一天」「name 读成 NULL」「补班标记丢了」都会在这里
// 露出来，而它们全都不会报错，只会让某几天的价档悄悄用错。
func TestImportedJudgmentsMatchMemorySource(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	year := newYear(t, db)

	// 一份编出来的数据：元旦放假，再挑一个周六补班，其余按「周末休息」铺开。
	newYear := calendar.Date{Year: year, Month: 1, Day: 1}
	makeup := weekdayAfter(t, year, time.Saturday, newYear)
	plainSunday := weekdayAfter(t, year, time.Sunday, newYear)
	plainMonday := weekdayAfter(t, year, time.Monday, newYear)

	doc := syncedDoc(year)
	setDay(t, &doc, newYear, true, "元旦", false)
	setDay(t, &doc, makeup, false, "", true)

	res := importDoc(t, db, doc)
	if res.Status != calendar.YearSynced {
		t.Errorf("导入后状态 = %q，期望 synced", res.Status)
	}
	if res.DaysInYear != 365 || res.DaysOutsideYear != 0 {
		t.Errorf("导入结果 = 本年 %d 天、年外 %d 天，期望 365 / 0", res.DaysInYear, res.DaysOutsideYear)
	}
	if res.Replaced != 0 {
		t.Errorf("首次导入替换了 %d 行，期望 0", res.Replaced)
	}

	cal := newCalendar(t, db)

	// 具体日期、具体判定。
	cases := []struct {
		date   calendar.Date
		kind   calendar.DayKind
		name   string
		makeup bool
	}{
		{newYear, calendar.DayRestDay, "元旦", false},
		{makeup, calendar.DayWorkday, "", true},
		{plainSunday, calendar.DayRestDay, "", false},
		{plainMonday, calendar.DayWorkday, "", false},
		{calendar.Date{Year: year, Month: 12, Day: 31}, weekendKind(year, 12, 31), "", false},
	}
	for _, tc := range cases {
		status := lookup(t, cal, tc.date)
		if status.Kind != tc.kind {
			t.Errorf("%s 判定 = %q，期望 %q", tc.date, status.Kind, tc.kind)
		}
		if status.Name != tc.name {
			t.Errorf("%s 的名称 = %q，期望 %q", tc.date, status.Name, tc.name)
		}
		if status.MakeupWorkday != tc.makeup {
			t.Errorf("%s 的补班标记 = %v，期望 %v", tc.date, status.MakeupWorkday, tc.makeup)
		}
		if !status.Kind.Known() {
			t.Errorf("%s 落在未知上，而它明明有数据", tc.date)
		}
	}

	// 内存数据源装上同一份数据，逐天对照。
	mem := calendar.NewMemorySource()
	mem.SetYearState(calendar.RegionCN, year, calendar.YearSynced)
	for _, day := range doc.Days {
		date, err := parseDayDate(day.Date)
		if err != nil {
			t.Fatalf("夹具里的 %q 解不出来: %v", day.Date, err)
		}
		if err := mem.AddDay(calendar.RegionCN, calendar.DayRow{
			Date:          date,
			Name:          day.Name,
			OffDay:        *day.OffDay,
			MakeupWorkday: day.MakeupWorkday,
		}); err != nil {
			t.Fatalf("往内存数据源里放 %s 失败: %v", date, err)
		}
	}
	memCal, err := calendar.New(mem)
	if err != nil {
		t.Fatalf("构造内存 Calendar 失败: %v", err)
	}

	compared := 0
	for _, date := range yearDays(year) {
		fromDB := lookup(t, cal, date)
		fromMemory, err := memCal.Lookup(ctx, calendar.RegionCN, date)
		if err != nil {
			t.Fatalf("内存数据源判定 %s 出错: %v", date, err)
		}
		if fromDB.Kind != fromMemory.Kind ||
			fromDB.Name != fromMemory.Name ||
			fromDB.MakeupWorkday != fromMemory.MakeupWorkday {
			t.Errorf("%s：库给出 %+v，内存给出 %+v", date, fromDB, fromMemory)
		}
		compared++
	}
	// 条数是这一条的正向证据：一天都没比过时，上面的循环会一条错都不报。
	if compared != 365 {
		t.Fatalf("只比了 %d 天，期望 365", compared)
	}
	if got := yearState(t, db, year); got != calendar.YearSynced {
		t.Errorf("年份状态 = %q，期望 synced", got)
	}
}

// weekendKind 给出某一天的纯星期规则判定，供夹具对照用。
func weekendKind(year, month, day int) calendar.DayKind {
	if (calendar.Date{Year: year, Month: time.Month(month), Day: day}).IsWeekend() {
		return calendar.DayRestDay
	}
	return calendar.DayWorkday
}

// ── 没有数据的那一年 ────────────────────────────────────────────────────

// 库里没有这一年的记录时，判定必须是**未知**。
//
// 静默按工作日算会在节假日按工作日计价，而账单看上去完全正常——这正是
// internal/calendar 把判定做成三态要防的事。这里把它钉住。
func TestYearWithNoRowIsUnknownNotWorkday(t *testing.T) {
	db := testDB(t)
	year := newYear(t, db) // 什么都不导入

	if got := yearState(t, db, year); got != calendar.YearUnknown {
		t.Errorf("年份状态 = %q，期望 unknown（库里没有这一年的记录）", got)
	}

	cal := newCalendar(t, db)
	status := lookup(t, cal, calendar.Date{Year: year, Month: 1, Day: 1})
	if status.Kind != calendar.DayUnknown {
		t.Fatalf("判定 = %q，期望 unknown", status.Kind)
	}
	if status.Kind == calendar.DayWorkday {
		t.Error("未知被当成了工作日")
	}
	if status.Kind.Known() {
		t.Error("未知不该是「已知」")
	}
	if status.Reason != calendar.ReasonYearUnknown {
		t.Errorf("成因 = %q，期望 %q", status.Reason, calendar.ReasonYearUnknown)
	}
}

// pending 是未来年份的**正常状态**（公告还没发布），但它同样不能变成工作日。
func TestPendingYearIsUnknownNotWorkday(t *testing.T) {
	db := testDB(t)
	year := newYear(t, db)

	importDoc(t, db, Document{Region: "CN", Year: year, Status: "pending"})

	if got := countYearRows(t, db, year); got != 0 {
		t.Errorf("pending 的年份写了 %d 行数据，期望 0 行", got)
	}
	if got := yearState(t, db, year); got != calendar.YearPending {
		t.Errorf("年份状态 = %q，期望 pending", got)
	}

	cal := newCalendar(t, db)
	status := lookup(t, cal, calendar.Date{Year: year, Month: 5, Day: 1})
	if status.Kind != calendar.DayUnknown {
		t.Fatalf("判定 = %q，期望 unknown", status.Kind)
	}
	if status.Reason != calendar.ReasonYearPending {
		t.Errorf("成因 = %q，期望 %q", status.Reason, calendar.ReasonYearPending)
	}
	if status.Kind == calendar.DayWorkday {
		t.Error("公告未发布的年份被当成了工作日")
	}
}

// 一个标着 synced 却没有这一天的行：这是**数据不完整**，仍然算未知。
//
// 它只能靠直接写库造出来（导入路径要求 synced 覆盖全年），而这正是真实世界里
// 的样子——同步任务跑到一半失败了，年份状态已经翻成 synced，行还没写完。
func TestSyncedYearWithMissingRowIsUnknown(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	year := newYear(t, db)

	if _, err := db.ExecContext(ctx,
		`INSERT INTO calendar_years (region, year, status, day_count) VALUES ('CN', $1, 'synced', 0)`,
		year); err != nil {
		t.Fatalf("直接写年份行失败: %v", err)
	}

	if got := yearState(t, db, year); got != calendar.YearSynced {
		t.Errorf("年份状态 = %q，期望 synced", got)
	}
	status := lookup(t, newCalendar(t, db), calendar.Date{Year: year, Month: 3, Day: 1})
	if status.Kind != calendar.DayUnknown {
		t.Fatalf("判定 = %q，期望 unknown", status.Kind)
	}
	if status.Reason != calendar.ReasonRowMissing {
		t.Errorf("成因 = %q，期望 %q", status.Reason, calendar.ReasonRowMissing)
	}
}

// ── 跨年边界 ────────────────────────────────────────────────────────────

// 元旦的安排可能把去年 12 月 31 日挪成休息，而那条记录出自后一年的数据集。
// 判定按**行**优先，所以它必须压过「上一年没有任何记录」这件事。
func TestCrossYearDayIsUsedAndNeighbourStaysUnknown(t *testing.T) {
	db := testDB(t)
	year := newYear(t, db)
	prevYear := year - 1
	borrowed := calendar.Date{Year: prevYear, Month: 12, Day: 31}
	untouched := calendar.Date{Year: prevYear, Month: 12, Day: 30}

	doc := syncedDoc(year)
	doc.Days = append(doc.Days, Day{Date: borrowed.String(), Name: "元旦", OffDay: boolPtr(true)})
	res := importDoc(t, db, doc)
	if res.DaysOutsideYear != 1 {
		t.Errorf("年外天数 = %d，期望 1", res.DaysOutsideYear)
	}

	cal := newCalendar(t, db)
	status := lookup(t, cal, borrowed)
	if status.Kind != calendar.DayRestDay {
		t.Errorf("%s 判定 = %q，期望 rest_day（行优先于年份状态）", borrowed, status.Kind)
	}
	if status.Name != "元旦" {
		t.Errorf("%s 的名称 = %q", borrowed, status.Name)
	}

	// 上一年没有年份记录，紧挨着的那一天仍然是未知——借来的那一天只借那一天。
	status = lookup(t, cal, untouched)
	if status.Kind != calendar.DayUnknown || status.Reason != calendar.ReasonYearUnknown {
		t.Errorf("%s 判定 = %q（成因 %q），期望 unknown / %q",
			untouched, status.Kind, status.Reason, calendar.ReasonYearUnknown)
	}
}

// ── 重灌 ────────────────────────────────────────────────────────────────

// 同一年再灌一次是覆盖而不是追加：行数不变，改过的判定要生效。
func TestReimportOverwritesInsteadOfAppending(t *testing.T) {
	db := testDB(t)
	year := newYear(t, db)
	newYear := calendar.Date{Year: year, Month: 1, Day: 1}

	first := syncedDoc(year)
	setDay(t, &first, newYear, true, "元旦", false)
	res := importDoc(t, db, first)
	if res.Replaced != 0 {
		t.Errorf("首次导入替换了 %d 行，期望 0", res.Replaced)
	}
	if got := countYearRows(t, db, year); got != 365 {
		t.Fatalf("首次导入后有 %d 行，期望 365", got)
	}

	// 第二次：元旦改成上班（调休挪走了），其余不动。
	second := syncedDoc(year)
	setDay(t, &second, newYear, false, "", true)
	res = importDoc(t, db, second)
	if res.Replaced != 365 {
		t.Errorf("重灌替换了 %d 行，期望 365（先清掉上一次写下的）", res.Replaced)
	}
	if got := countYearRows(t, db, year); got != 365 {
		t.Errorf("重灌后有 %d 行，期望仍是 365（不是 730）", got)
	}

	status := lookup(t, newCalendar(t, db), newYear)
	if status.Kind != calendar.DayWorkday || !status.MakeupWorkday {
		t.Errorf("%s 判定 = %q（补班 %v），期望重灌后的工作日", newYear, status.Kind, status.MakeupWorkday)
	}
	if status.Name != "" {
		t.Errorf("%s 的名称 = %q，期望被重灌清掉", newYear, status.Name)
	}
}

// 新文件里不再有的那一天，要被清掉而不是留在库里继续参与判定。
//
// 这正是「按 source_year 删」而不是「按文件里的日期删」的理由：按日期删的话，
// 文件里没有的那几天恰好一条都不会被碰到。
func TestReimportDropsDaysTheNewFileNoLongerHas(t *testing.T) {
	db := testDB(t)
	year := newYear(t, db)
	extra := calendar.Date{Year: year + 1, Month: 1, Day: 1}

	first := syncedDoc(year)
	first.Days = append(first.Days, Day{Date: extra.String(), OffDay: boolPtr(true)})
	importDoc(t, db, first)
	if got := countDayRow(t, db, extra); got != 1 {
		t.Fatalf("%s 在库里有 %d 行，期望 1", extra, got)
	}

	importDoc(t, db, syncedDoc(year))
	if got := countDayRow(t, db, extra); got != 0 {
		t.Errorf("%s 还有 %d 行，期望被清掉", extra, got)
	}
	if got := countYearRows(t, db, year); got != 365 {
		t.Errorf("重灌后有 %d 行，期望 365", got)
	}
}

// ── 年份状态的四个取值各自怎么来 ────────────────────────────────────────

func TestYearStateComesFromTheRow(t *testing.T) {
	db := testDB(t)
	year := newYear(t, db)
	cal := newCalendar(t, db)
	day := calendar.Date{Year: year, Month: 6, Day: 15}

	steps := []struct {
		name   string
		doc    *Document
		state  calendar.YearState
		reason calendar.UnknownReason
	}{
		{"从没同步过", nil, calendar.YearUnknown, calendar.ReasonYearUnknown},
		{"公告未发布", &Document{Region: "CN", Year: year, Status: "pending"}, calendar.YearPending, calendar.ReasonYearPending},
		{"双源不一致", &Document{Region: "CN", Year: year, Status: "conflict", SourcesAgree: boolPtr(false)}, calendar.YearConflict, calendar.ReasonYearConflict},
		{"同步失败", &Document{Region: "CN", Year: year, Status: "degraded", LastError: "拉取超时"}, calendar.YearDegraded, calendar.ReasonYearDegraded},
	}
	for _, step := range steps {
		if step.doc != nil {
			importDoc(t, db, *step.doc)
		}
		if got := yearState(t, db, year); got != step.state {
			t.Errorf("%s：年份状态 = %q，期望 %q", step.name, got, step.state)
		}
		status := lookup(t, cal, day)
		if status.Kind != calendar.DayUnknown {
			t.Errorf("%s：判定 = %q，期望 unknown", step.name, status.Kind)
		}
		if status.Reason != step.reason {
			t.Errorf("%s：成因 = %q，期望 %q", step.name, status.Reason, step.reason)
		}
	}

	// 最后灌一份完整数据：状态翻成 synced，那一天也就有判定了。
	doc := syncedDoc(year)
	importDoc(t, db, doc)
	if got := yearState(t, db, year); got != calendar.YearSynced {
		t.Errorf("导入完整数据后年份状态 = %q，期望 synced", got)
	}
	if status := lookup(t, cal, day); !status.Kind.Known() {
		t.Errorf("导入完整数据后 %s 仍然是 %q", day, status.Kind)
	}
}

// 一个已经灌过完整数据的年份，被改标成 pending 时，那些行要被清掉——
// 否则「这一年还没定下来」会被那些行压过去，判定照旧给出确定值。
func TestPendingImportClearsEarlierData(t *testing.T) {
	db := testDB(t)
	year := newYear(t, db)
	day := calendar.Date{Year: year, Month: 4, Day: 1}

	doc := syncedDoc(year)
	importDoc(t, db, doc)
	cal := newCalendar(t, db)
	if status := lookup(t, cal, day); !status.Kind.Known() {
		t.Fatalf("先灌完整数据后 %s 应当有判定，实际是 %q", day, status.Kind)
	}

	importDoc(t, db, Document{Region: "CN", Year: year, Status: "pending"})

	if got := countYearRows(t, db, year); got != 0 {
		t.Errorf("改标 pending 之后还剩 %d 行，期望 0 行", got)
	}
	status := lookup(t, cal, day)
	if status.Kind != calendar.DayUnknown || status.Reason != calendar.ReasonYearPending {
		t.Errorf("%s 判定 = %q（成因 %q），期望 unknown / %q",
			day, status.Kind, status.Reason, calendar.ReasonYearPending)
	}
}

// 上一年写下的那一行不会被本年的 pending 导入清掉——删除按 source_year 走，
// 而那一行的来源是**上一年**。
//
// 这不是漏网，是「跨年安排本来就是确定判定」的直接后果：元旦的安排出自上一年
// 的数据集，那一天的判定是确定的，与本年的公告发没发无关。于是本年标成 pending
// 之后，那一天仍然按行给出确定值，本年其余各天才落到「公告未发布」。
//
// 写出来是因为这条路径没有别的地方钉住：TestPendingImportClearsEarlierData
// 覆盖的是 source_year 等于本年那种情况，两者的结局相反。
func TestPendingImportKeepsAdjacentYearRows(t *testing.T) {
	db := testDB(t)
	year := newYear(t, db)
	prev := year - 1
	t.Cleanup(func() { cleanupYear(t, db, prev) })

	// 上一年的文件是 synced，且带了属于本年的那一天。
	carried := calendar.Date{Year: year, Month: 1, Day: 1}
	prevDoc := syncedDoc(prev)
	prevDoc.Days = append(prevDoc.Days, Day{Date: carried.String(), Name: "元旦", OffDay: boolPtr(true)})
	importDoc(t, db, prevDoc)

	// 关键的一步：那一行挂在**上一年**的来源下。
	var sourceYear int
	if err := db.QueryRowContext(context.Background(),
		`SELECT source_year FROM calendar_days WHERE region = 'CN' AND day = $1::date`,
		carried.String()).Scan(&sourceYear); err != nil {
		t.Fatalf("读 %s 的来源年份失败: %v", carried, err)
	}
	if sourceYear != prev {
		t.Fatalf("%s 的 source_year = %d，期望 %d", carried, sourceYear, prev)
	}
	if got := countYearRows(t, db, year); got != 0 {
		t.Fatalf("本年名下已经有 %d 行，期望 0——用例的前提是那一行挂在上一年的来源下", got)
	}

	// 本年标成 pending：删的是 source_year = 本年的行，那一行不在其中。
	importDoc(t, db, Document{Region: "CN", Year: year, Status: "pending"})

	if got := countDayRow(t, db, carried); got != 1 {
		t.Fatalf("%s 被清掉了（剩 %d 行）：跨年安排是确定判定，不跟本年度的状态走", carried, got)
	}
	// 行在，判定就按行走——这是「某一天只要有行就以行为准」的直接体现。
	cal := newCalendar(t, db)
	if status := lookup(t, cal, carried); status.Kind != calendar.DayRestDay {
		t.Errorf("%s 判定 = %q，期望 %q", carried, status.Kind, calendar.DayRestDay)
	}
	// 本年其余各天没有行，落到「公告未发布」。
	other := calendar.Date{Year: year, Month: 6, Day: 15}
	status := lookup(t, cal, other)
	if status.Kind != calendar.DayUnknown || status.Reason != calendar.ReasonYearPending {
		t.Errorf("%s 判定 = %q（成因 %q），期望 %q / %q",
			other, status.Kind, status.Reason, calendar.DayUnknown, calendar.ReasonYearPending)
	}
}

// ── 校验发生在碰库之前 ──────────────────────────────────────────────────

// 一份通不过校验的文件，一个字节都不该落库。
//
// 用一个 nil 句柄调 Import 就能证明这一点：校验不过时它必须在 BeginTx 之前
// 返回，否则这里会 panic。
func TestImportValidatesBeforeTouchingDatabase(t *testing.T) {
	year := 2031
	bad := syncedDoc(year)
	bad.Days = bad.Days[1:] // 少一天，synced 不再成立

	if _, err := NewAdmin(nil).Import(context.Background(), bad); err == nil {
		t.Fatal("缺行的文档应当被拒绝，实际通过了（而且没有 panic 说明它没碰库）")
	}
}

// 真库上再确认一次：被拒绝的导入不留任何痕迹。
func TestRejectedImportLeavesNothingBehind(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	year := newYear(t, db)

	bad := syncedDoc(year)
	bad.Days = bad.Days[1:]

	if _, err := NewAdmin(db).Import(ctx, bad); err == nil {
		t.Fatal("缺行的文档应当被拒绝")
	}
	if got := countYearRows(t, db, year); got != 0 {
		t.Errorf("被拒绝的导入留下了 %d 行数据", got)
	}

	var years int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM calendar_years WHERE region = 'CN' AND year = $1`, year).Scan(&years); err != nil {
		t.Fatalf("统计年份行失败: %v", err)
	}
	if years != 0 {
		t.Errorf("被拒绝的导入留下了 %d 行年份状态", years)
	}
}
