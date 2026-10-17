package store

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/calendar"
)

func boolPtr(v bool) *bool { return &v }

// showPtr 把一个可空值渲染成可读文本，供断言失败时打印。
//
// 直接 %v 一个指向基本类型的指针打出来的是地址（`0xc000...`），而报错信息
// 本身是要给人读的——那串地址看不出实际值是什么，也就看不出这次失败是
// 「值算错了」还是「字段根本没给」。
func showPtr[T any](p *T) string {
	if p == nil {
		return "<nil>"
	}
	return fmt.Sprint(*p)
}

// yearDays 独立地列出某年的每一天。
//
// **刻意不复用生产代码里的 daysOfYear**：两者共用一段循环时，一段写错的循环
// 会让「覆盖检查」与「夹具」同时认可一份少了一天的数据，于是用例全绿而库里
// 缺行。下面 TestYearDaysCount 拿字面量把天数钉住，就是为了让这个夹具自己
// 也被验一遍。
func yearDays(year int) []calendar.Date {
	var out []calendar.Date
	for t := time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC); t.Year() == year; t = t.AddDate(0, 0, 1) {
		out = append(out, calendar.Date{Year: t.Year(), Month: t.Month(), Day: t.Day()})
	}
	return out
}

// TestYearDaysCount 拿字面量钉住闰年与平年的天数。
func TestYearDaysCount(t *testing.T) {
	cases := []struct {
		year int
		want int
	}{
		{2031, 365},
		{2032, 366}, // 闰年
		{2100, 365}, // 能被 100 整除但不能被 400 整除，不是闰年
		{2000, 366}, // 能被 400 整除，是闰年
	}
	for _, tc := range cases {
		if got := len(yearDays(tc.year)); got != tc.want {
			t.Errorf("%d 年有 %d 天，期望 %d", tc.year, got, tc.want)
		}
	}
}

// syncedDoc 造一份覆盖全年、默认「周末休息」的文档。
//
// 默认规则是「假设没有调休」：真实数据集里每一天都有一个明确的判定，而
// 调休只是其中少数几天，所以底色按周末铺、再挑几天覆盖。
func syncedDoc(year int) Document {
	days := make([]Day, 0, 366)
	for _, date := range yearDays(year) {
		off := date.IsWeekend()
		days = append(days, Day{Date: date.String(), OffDay: boolPtr(off)})
	}
	return Document{Region: "CN", Year: year, Status: "synced", Source: "manual", Days: days}
}

// setDay 覆盖某一天的判定。找不到那一天说明夹具写错了，直接失败。
func setDay(t *testing.T, doc *Document, date calendar.Date, off bool, name string, makeup bool) {
	t.Helper()
	for i := range doc.Days {
		if doc.Days[i].Date != date.String() {
			continue
		}
		doc.Days[i].OffDay = boolPtr(off)
		doc.Days[i].Name = name
		doc.Days[i].MakeupWorkday = makeup
		return
	}
	t.Fatalf("夹具里没有 %s 这一天", date)
}

// findWeekday 找某年第一个星期几是 wd 的日子。
func findWeekday(year int, wd time.Weekday) calendar.Date {
	for _, date := range yearDays(year) {
		if date.Weekday() == wd {
			return date
		}
	}
	panic("一年里一定会有每一个星期几")
}

// ── 解析 ────────────────────────────────────────────────────────────────

func TestParseDocumentRoundTrip(t *testing.T) {
	agree := true
	want := Document{
		Region:          "CN",
		Year:            2031,
		Status:          "synced",
		Source:          "manual",
		SourcePaperURLs: []string{"https://example.com/notice"},
		SourcesAgree:    &agree,
		Days: []Day{
			{Date: "2031-01-01", Name: "元旦", OffDay: boolPtr(true)},
			{Date: "2031-01-02", OffDay: boolPtr(false)},
			{Date: "2031-01-04", OffDay: boolPtr(false), MakeupWorkday: true},
		},
	}
	// 补齐其余各天，否则通不过「synced 要覆盖全年」这一条。
	explicit := map[string]bool{"2031-01-01": true, "2031-01-02": true, "2031-01-04": true}
	for _, date := range yearDays(2031) {
		if explicit[date.String()] {
			continue
		}
		want.Days = append(want.Days, Day{Date: date.String(), OffDay: boolPtr(date.IsWeekend())})
	}

	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("编码夹具失败: %v", err)
	}
	got, err := ParseDocument(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("ParseDocument 出错: %v", err)
	}
	if got.Region != "CN" || got.Year != 2031 || got.Status != "synced" || got.Source != "manual" {
		t.Errorf("头部字段解出来是 %+v", got)
	}
	if len(got.SourcePaperURLs) != 1 || got.SourcePaperURLs[0] != "https://example.com/notice" {
		t.Errorf("source_paper_urls = %v", got.SourcePaperURLs)
	}
	if got.SourcesAgree == nil || !*got.SourcesAgree {
		t.Errorf("sources_agree = %s，期望 true", showPtr(got.SourcesAgree))
	}
	if len(got.Days) != len(want.Days) {
		t.Fatalf("解出 %d 天，期望 %d 天", len(got.Days), len(want.Days))
	}
	// 三天各自的判定都要原样解出来：漏一个字段就会让那一天按默认算。
	first := got.Days[0]
	if first.Date != "2031-01-01" || first.Name != "元旦" || first.OffDay == nil || !*first.OffDay {
		t.Errorf("第一天解出来是 %+v", first)
	}
	third := got.Days[2]
	if third.OffDay == nil || *third.OffDay || !third.MakeupWorkday {
		t.Errorf("第三天解出来是 %+v", third)
	}
}

// 每一个键名都要被**手写的 JSON**钉住。
//
// 键名是这份文件格式的对外契约：docs/calendar-data.md 与
// `larkaxctl calendar import --help` 都照着它写。而 TestParseDocumentRoundTrip
// 拿 json.Marshal(Document) 造输入，编码与解码共用同一批 struct tag——把
// `off_day` 改成 `offDay`，它照样绿，文档与实现却当场对不上。所以这里手写
// JSON：键名与 struct tag 成了两个独立的来源，改一个另一个就挂。
//
// days 要覆盖全年才通得过校验（synced 的硬要求），所以那一段由一条手写的
// 模板铺满 2031 年——模板里的键名同样只出现在这里，不是从 struct tag 反推的。
//
// 这份文件不是一份真实的日历数据：每个取值都只为覆盖一个键，例如 synced 的
// 年也带着 last_error（校验不管这一对组合）。
func TestParseDocumentPinsEveryKeyName(t *testing.T) {
	const header = `{"region":"CN","year":2031,"status":"synced","source":"manual",` +
		`"source_paper_urls":["https://example.com/notice","https://example.com/notice-2"],` +
		`"sources_agree":false,"last_error":"上一次同步失败的原因","days":[`

	var days strings.Builder
	for _, date := range yearDays(2031) {
		if days.Len() > 0 {
			days.WriteString(",")
		}
		switch date.String() {
		case "2031-01-01":
			// 四个键全写的一天。
			days.WriteString(`{"date":"2031-01-01","name":"元旦","off_day":true,"makeup_workday":false}`)
		case "2031-01-04":
			// 补班日：off_day 为 false 且 makeup_workday 为 true。
			days.WriteString(`{"date":"2031-01-04","off_day":false,"makeup_workday":true}`)
		default:
			// 只写必填的两个键：可选的 name 与 makeup_workday 缺省时该是零值。
			fmt.Fprintf(&days, `{"date":%q,"off_day":false}`, date.String())
		}
	}
	raw := header + days.String() + `]}`

	doc, err := ParseDocument(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("ParseDocument 出错: %v", err)
	}

	// 头部的八个键，逐个断言具体的值。
	if doc.Region != "CN" || doc.Year != 2031 || doc.Status != "synced" || doc.Source != "manual" {
		t.Errorf("头部解出来是 %+v", doc)
	}
	if len(doc.SourcePaperURLs) != 2 ||
		doc.SourcePaperURLs[0] != "https://example.com/notice" ||
		doc.SourcePaperURLs[1] != "https://example.com/notice-2" {
		t.Errorf("source_paper_urls = %v", doc.SourcePaperURLs)
	}
	// 指针字段：写 false 与不写是两回事，这里写的是 false，所以指针必须非 nil。
	if doc.SourcesAgree == nil || *doc.SourcesAgree {
		t.Errorf("sources_agree = %s，期望 false 而不是「没写」", showPtr(doc.SourcesAgree))
	}
	if doc.LastError != "上一次同步失败的原因" {
		t.Errorf("last_error = %q", doc.LastError)
	}

	if len(doc.Days) != 365 {
		t.Fatalf("解出 %d 天，期望 365", len(doc.Days))
	}
	// days 的四个键，各挑一天断言。顺序与文件里的顺序一致。
	if first := doc.Days[0]; first.Date != "2031-01-01" || first.Name != "元旦" ||
		first.OffDay == nil || !*first.OffDay || first.MakeupWorkday {
		t.Errorf("第一天解出来是 %+v", first)
	}
	if second := doc.Days[1]; second.Date != "2031-01-02" || second.Name != "" ||
		second.OffDay == nil || *second.OffDay || second.MakeupWorkday {
		t.Errorf("第二天解出来是 %+v", second)
	}
	if makeup := doc.Days[3]; makeup.Date != "2031-01-04" || makeup.Name != "" ||
		makeup.OffDay == nil || *makeup.OffDay || !makeup.MakeupWorkday {
		t.Errorf("补班日解出来是 %+v", makeup)
	}
}

// 键名写错必须报错。JSON 解码默认静默忽略未知字段，而 "offDay" 被忽略的
// 后果是那一天按工作日算——节假日按工作日计价，账单看上去完全正常。
func TestParseDocumentRejectsUnknownField(t *testing.T) {
	const raw = `{"region":"CN","year":2031,"status":"pending","offDay":true}`
	_, err := ParseDocument(strings.NewReader(raw))
	if err == nil {
		t.Fatal("未知字段应当报错，实际通过了")
	}
	if !strings.Contains(err.Error(), "offDay") {
		t.Errorf("报错里要点出是哪个字段：%v", err)
	}
}

// 第一个对象之后还有内容：多半是两个文件被拼在了一起，后面的内容不能被丢掉。
func TestParseDocumentRejectsTrailingContent(t *testing.T) {
	const raw = `{"region":"CN","year":2031,"status":"pending"}{"region":"CN","year":2032,"status":"pending"}`
	_, err := ParseDocument(strings.NewReader(raw))
	if err == nil {
		t.Fatal("对象之后还有内容应当报错")
	}
}

func TestParseDocumentRejectsGarbage(t *testing.T) {
	if _, err := ParseDocument(strings.NewReader("这不是 JSON")); err == nil {
		t.Fatal("非 JSON 应当报错")
	}
}

// ── 校验 ────────────────────────────────────────────────────────────────

func TestValidateRejects(t *testing.T) {
	year := 2031
	ok := syncedDoc(year)

	missingOneDay := syncedDoc(year)
	missingOneDay.Days = missingOneDay.Days[1:]

	duplicated := syncedDoc(year)
	duplicated.Days = append(duplicated.Days, duplicated.Days[0])

	noOffDay := syncedDoc(year)
	noOffDay.Days[3].OffDay = nil

	contradictory := syncedDoc(year)
	contradictory.Days[3].OffDay = boolPtr(true)
	contradictory.Days[3].MakeupWorkday = true

	badFormat := syncedDoc(year)
	badFormat.Days[3].Date = "2031-1-05"

	impossible := syncedDoc(year)
	impossible.Days[3].Date = "2031-02-30"

	farAway := syncedDoc(year)
	farAway.Days[3].Date = "2035-01-05"

	pendingWithDays := Document{Region: "CN", Year: year, Status: "pending", Days: ok.Days}
	pendingWithSource := Document{Region: "CN", Year: year, Status: "pending", Source: "manual"}
	syncedWithoutSource := Document{Region: "CN", Year: year, Status: "synced", Days: ok.Days}
	syncedWithoutDays := Document{Region: "CN", Year: year, Status: "synced", Source: "manual"}

	cases := []struct {
		name string
		doc  Document
		want string
	}{
		{"地区为空", Document{Year: year, Status: "pending"}, "region"},
		{"地区没登记过", Document{Region: "ZZ", Year: year, Status: "pending"}, "region"},
		{"年份为 0", Document{Region: "CN", Year: 0, Status: "pending"}, "year"},
		{"年份超出公历", Document{Region: "CN", Year: 10000, Status: "pending"}, "year"},
		{"状态认不出", Document{Region: "CN", Year: year, Status: "published"}, "status"},
		{"状态为空", Document{Region: "CN", Year: year}, "status"},
		{"synced 没有 days", syncedWithoutDays, "days"},
		{"synced 没有 source", syncedWithoutSource, "source"},
		{"pending 带 days", pendingWithDays, "days"},
		{"pending 带 source", pendingWithSource, "source"},
		{"少了一天", missingOneDay, "没有数据"},
		{"同一天出现两次", duplicated, "两次"},
		{"没有 off_day", noOffDay, "off_day"},
		{"既放假又补班", contradictory, "矛盾"},
		{"日期格式不对", badFormat, "YYYY-MM-DD"},
		{"日期不成立", impossible, "2031-02-30"},
		{"年外超过一年", farAway, "相邻"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.doc.validate()
			if err == nil {
				t.Fatal("应当被拒绝，实际通过了校验")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("报错里应当提到 %q，实际为 %v", tc.want, err)
			}
		})
	}

	// 合法的那几份不该被上面的用例误伤。
	for _, doc := range []Document{
		ok,
		{Region: "CN", Year: year, Status: "pending"},
		{Region: "CN", Year: year, Status: "conflict", SourcesAgree: boolPtr(false)},
		{Region: "CN", Year: year, Status: "degraded", LastError: "拉取超时"},
	} {
		if err := doc.validate(); err != nil {
			t.Errorf("status=%s 的文档应当通过校验，实际为 %v", doc.Status, err)
		}
	}
}

// 跨年边界上的日子可以写进相邻年份的文件里：元旦的安排可能把去年 12 月 31 日
// 挪成休息，而那条记录出自后一年的数据集。
func TestValidateAcceptsAdjacentYearDays(t *testing.T) {
	year := 2031
	doc := syncedDoc(year)
	doc.Days = append(doc.Days,
		Day{Date: "2030-12-31", Name: "元旦", OffDay: boolPtr(true)},
		Day{Date: "2032-01-01", OffDay: boolPtr(true)},
	)
	if err := doc.validate(); err != nil {
		t.Fatalf("相邻年份的日子应当被接受: %v", err)
	}

	prep, err := doc.prepare()
	if err != nil {
		t.Fatalf("prepare 出错: %v", err)
	}
	if prep.inYear != 365 {
		t.Errorf("本年天数 = %d，期望 365", prep.inYear)
	}
	if prep.outsideYear != 2 {
		t.Errorf("年外天数 = %d，期望 2", prep.outsideYear)
	}
	if len(prep.rows) != 367 {
		t.Errorf("行数 = %d，期望 367", len(prep.rows))
	}
}

// prepare 要把每一行原样翻出来：名称去空白、判定照抄、日期解成 Date。
func TestPrepareKeepsEveryField(t *testing.T) {
	year := 2031
	doc := syncedDoc(year)
	makeup := findWeekday(year, time.Sunday)
	setDay(t, &doc, makeup, false, "", true)
	holiday := calendar.Date{Year: year, Month: 10, Day: 1}
	setDay(t, &doc, holiday, true, "  国庆节  ", false)

	prep, err := doc.prepare()
	if err != nil {
		t.Fatalf("prepare 出错: %v", err)
	}
	byDate := make(map[calendar.Date]calendar.DayRow, len(prep.rows))
	for _, row := range prep.rows {
		byDate[row.Date] = row
	}

	got, ok := byDate[holiday]
	if !ok {
		t.Fatalf("没有翻出 %s 这一行", holiday)
	}
	if !got.OffDay || got.MakeupWorkday || got.Name != "国庆节" {
		t.Errorf("%s 翻出来是 %+v", holiday, got)
	}

	got, ok = byDate[makeup]
	if !ok {
		t.Fatalf("没有翻出 %s 这一行", makeup)
	}
	if got.OffDay || !got.MakeupWorkday {
		t.Errorf("补班日 %s 翻出来是 %+v，期望工作日且带补班标记", makeup, got)
	}
}

// 校验不过的文档，prepare 也不该放行——它同样不碰数据库。
func TestPrepareRejectsInvalid(t *testing.T) {
	doc := syncedDoc(2031)
	doc.Days = doc.Days[1:]
	if _, err := doc.prepare(); err == nil {
		t.Fatal("缺行的文档不该被 prepare 接受")
	}
}

// ── 年份状态的解析 ──────────────────────────────────────────────────────

func TestParseYearState(t *testing.T) {
	for raw, want := range map[string]calendar.YearState{
		"synced":   calendar.YearSynced,
		"pending":  calendar.YearPending,
		"conflict": calendar.YearConflict,
		"degraded": calendar.YearDegraded,
	} {
		got, err := parseYearState(raw)
		if err != nil {
			t.Errorf("parseYearState(%q) 出错: %v", raw, err)
		}
		if got != want {
			t.Errorf("parseYearState(%q) = %q，期望 %q", raw, got, want)
		}
	}
	// 约束外的取值要报错，不能回落成「从没同步过」——那会把数据问题伪装成
	// 同步任务没跑，两件事的下一步动作不一样。
	for _, raw := range []string{"", "SYNCED", "ok", "unknown"} {
		if _, err := parseYearState(raw); err == nil {
			t.Errorf("parseYearState(%q) 应当报错", raw)
		}
	}
}
