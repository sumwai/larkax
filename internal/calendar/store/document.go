package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/sumwai/larkax/internal/calendar"
)

// Document 是一份日历数据文件的内容：某地区某一年的同步状态，以及该年
// 每一天的判定。
//
// 字段与 calendar_years / calendar_days 两张表的列一一对应，这样「文件里
// 写的」与「库里存的」不会各自漂移——写入侧少认一个字段，症状是那一列永远
// 是默认值，而导入本身报告成功。
type Document struct {
	// Region 是地区代码，必须是 internal/calendar 登记过的地区。
	Region string `json:"region"`

	// Year 是这份数据所属的年份，同时落进 calendar_days.source_year。
	Year int `json:"year"`

	// Status 取值与 calendar_years.status 一致：
	// synced / pending / conflict / degraded。
	Status string `json:"status"`

	// Source 是这批数据的出处（holiday-cn、manual 之类），落进
	// calendar_days.source。status 为 synced 时必填：那一年每一天的行都要
	// 带上它，否则「这一天的判定是谁给的」在库里答不上来。
	Source string `json:"source,omitempty"`

	// SourcePaperURLs 是公告原文地址，来自数据源。
	SourcePaperURLs []string `json:"source_paper_urls,omitempty"`

	// SourcesAgree 是双源交叉校验的结果。用指针是因为表上那一列可空：
	// 「两个源对不上」与「只有一个源、没得比」是两回事，而 bool 的零值
	// 表达不了后者。
	SourcesAgree *bool `json:"sources_agree,omitempty"`

	// LastError 是同步失败的原因，status 为 degraded 时才有意义。
	LastError string `json:"last_error,omitempty"`

	// Days 是该年每一天的判定。只有 status 为 synced 时才允许出现。
	Days []Day `json:"days,omitempty"`
}

// Day 是文件里的一天。
type Day struct {
	// Date 必须写成 YYYY-MM-DD。
	Date string `json:"date"`

	// Name 是节假日名称（「春节」「国庆节」），普通日子留空。
	Name string `json:"name,omitempty"`

	// OffDay 是这一天是否放假。**用指针是刻意的**：漏写与写 false 在 JSON
	// 里长得一样，而漏写会让那一整天按工作日算——节假日按工作日计价，账单
	// 看上去完全正常。所以这里把「没写」变成一个能被拒绝的状态。
	OffDay *bool `json:"off_day"`

	// MakeupWorkday 是调休补班日（把周末挪来上班），它必然与 OffDay 为
	// false 的日子配对。该列的取值由数据源按那一天是不是周末决定；
	// holiday-cn 只给「放不放假」，推断补班日那一步在
	// `larkaxctl price calendar sync` 的转换里。
	MakeupWorkday bool `json:"makeup_workday,omitempty"`
}

// ParseDocument 解一份日历数据文件并校验。
//
// 认不出的字段一律报错：JSON 解码默认静默忽略未知键，于是 "offDay": true
// 会被丢掉、那一天按工作日算，而文件看上去完全正常。
func ParseDocument(r io.Reader) (Document, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()

	var doc Document
	if err := dec.Decode(&doc); err != nil {
		return Document{}, fmt.Errorf("解析日历数据文件失败: %w", err)
	}
	// 第一个对象之后还有内容，说明这不是一个 JSON 文档（多半是两个文件被
	// 拼在了一起）。只看第一次 Decode 的话，后面的内容会被安静丢掉。
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return Document{}, errors.New("日历数据文件里在第一个 JSON 对象之后还有内容")
	}
	if err := doc.validate(); err != nil {
		return Document{}, err
	}
	return doc, nil
}

// 文件里允许出现的状态取值，与 calendar_years.status 的 CHECK 一致。
var allowedStatuses = map[string]calendar.YearState{
	string(calendar.YearSynced):   calendar.YearSynced,
	string(calendar.YearPending):  calendar.YearPending,
	string(calendar.YearConflict): calendar.YearConflict,
	string(calendar.YearDegraded): calendar.YearDegraded,
}

// validate 检查整份文件。它不碰数据库。
func (d Document) validate() error {
	if err := d.validateHeader(); err != nil {
		return err
	}
	return d.validateDays()
}

func (d Document) validateHeader() error {
	if _, err := calendar.Timezone(calendar.Region(d.Region)); err != nil {
		return fmt.Errorf("region %q 不可用: %w", d.Region, err)
	}
	// 上界取与 calendar.Date.Valid 相同的范围：年份超出它，本年最后一天
	// 也构造不出来，覆盖检查会先一步崩掉。
	if d.Year < 1 || d.Year > 9999 {
		return fmt.Errorf("year %d 不在 [1, 9999] 内", d.Year)
	}

	state, ok := allowedStatuses[d.Status]
	if !ok {
		return fmt.Errorf("status %q 不是 synced / pending / conflict / degraded 之一", d.Status)
	}

	switch state {
	case calendar.YearSynced:
		if strings.TrimSpace(d.Source) == "" {
			return errors.New("status 为 synced 时 source 必填：这一年的每一天都要记下判定是谁给的")
		}
	case calendar.YearPending, calendar.YearConflict, calendar.YearDegraded:
		// 这三种状态下年份还没有定论。它们的行一旦写进库里就会被判定优先采用
		//（见包文档），所以这里不放行：只记状态，不写数据。
		if len(d.Days) > 0 {
			return fmt.Errorf("status 为 %s 的年份不能带 days：那一年还没有定论，"+
				"写进去的数据行会被当成确定的判定使用", d.Status)
		}
		if strings.TrimSpace(d.Source) != "" {
			return fmt.Errorf("status 为 %s 的年份不该带 source：它只用于数据行，"+
				"而这个状态下不写数据行", d.Status)
		}
	}
	return nil
}

func (d Document) validateDays() error {
	if len(d.Days) == 0 {
		if d.Status == string(calendar.YearSynced) {
			return errors.New("status 为 synced 的年份必须带上 days，且要覆盖本年的每一天")
		}
		return nil
	}

	seen := make(map[calendar.Date]bool, len(d.Days))
	var missing []calendar.Date
	for i, day := range d.Days {
		date, err := parseDayDate(day.Date)
		if err != nil {
			return fmt.Errorf("第 %d 天: %w", i+1, err)
		}
		if seen[date] {
			return fmt.Errorf("第 %d 天 %s 在文件里出现了两次", i+1, date)
		}
		seen[date] = true

		// 年外的日子是允许的：元旦的安排可能把去年 12 月 31 日挪成休息，
		// 而那条记录出自后一年的数据集。但隔了两年一定是笔误——它会变成
		// 一条谁都没打算写的行，而判定按行优先，那一天就这么被悄悄改掉了。
		if diff := date.Year - d.Year; diff < -1 || diff > 1 {
			return fmt.Errorf("第 %d 天 %s 不属于 %d 年，也不在它相邻的两年里", i+1, date, d.Year)
		}
		if day.OffDay == nil {
			return fmt.Errorf("第 %d 天 %s 没有 off_day：漏写与写 false 在文件里长得一样，"+
				"而漏写会让那一天按工作日算", i+1, date)
		}
		if *day.OffDay && day.MakeupWorkday {
			return fmt.Errorf("第 %d 天 %s 既放假又标成调休补班日，这两条互相矛盾", i+1, date)
		}
	}

	for _, date := range DaysOfYear(d.Year) {
		if !seen[date] {
			missing = append(missing, date)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("status 为 synced，但 %d 年的 %d 天没有数据（例如 %s）；"+
			"缺行的年份不能标成 synced——那一天会被判定成未知，用到日历的计价规则会一直失败",
			d.Year, len(missing), exampleDates(missing))
	}
	return nil
}

// prepare 校验之后把文件翻成要写进库的数据行，并数出本年与年外各多少天。
func (d Document) prepare() (prepared, error) {
	if err := d.validate(); err != nil {
		return prepared{}, err
	}

	out := prepared{rows: make([]calendar.DayRow, 0, len(d.Days))}
	for _, day := range d.Days {
		// validate 已经把格式与合法性查过一遍，这里不会失败。
		date, err := parseDayDate(day.Date)
		if err != nil {
			return prepared{}, fmt.Errorf("第 %d 天: %w", len(out.rows)+1, err)
		}
		out.rows = append(out.rows, calendar.DayRow{
			Date:          date,
			Name:          strings.TrimSpace(day.Name),
			OffDay:        *day.OffDay,
			MakeupWorkday: day.MakeupWorkday,
		})
		if date.Year == d.Year {
			out.inYear++
		} else {
			out.outsideYear++
		}
	}
	return out, nil
}

// prepared 是一份文件翻出来的待写数据。
type prepared struct {
	rows        []calendar.DayRow
	inYear      int
	outsideYear int
}

// parseDayDate 按 YYYY-MM-DD 解一个日期。
//
// 用 time.Parse 而不是自己切字符串：它同时挡住格式不对（"2031-1-5"）与日期
// 不成立（"2031-02-30"）两类输入，而这两类都不该被规范化成另一天。
func parseDayDate(raw string) (calendar.Date, error) {
	parsed, err := time.Parse("2006-01-02", raw)
	if err != nil {
		return calendar.Date{}, fmt.Errorf("日期 %q 不是 YYYY-MM-DD 格式: %w", raw, err)
	}
	date := calendar.Date{Year: parsed.Year(), Month: parsed.Month(), Day: parsed.Day()}
	if !date.Valid() {
		return calendar.Date{}, fmt.Errorf("%w: %s", calendar.ErrInvalidDate, date)
	}
	return date, nil
}

// DaysOfYear 列出某一年公历上的每一天。
//
// 导出是因为导入方（`larkaxctl price calendar sync`）补齐「数据源没有这一天」
// 时也要生成同一条列表：那一份必须与覆盖检查用的是同一段循环，否则两处各算一
// 遍，闰年这类边界会在一边对、一边错。用例另有自己的一份（见 document_test.go
// 的 yearDays），那是刻意的交叉校验，不走这里。
func DaysOfYear(year int) []calendar.Date {
	start := time.Date(year, time.January, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(year+1, time.January, 1, 0, 0, 0, 0, time.UTC)

	var out []calendar.Date
	for t := start; t.Before(end); t = t.AddDate(0, 0, 1) {
		out = append(out, calendar.DateOf(t))
	}
	return out
}

// exampleDates 把缺的那几天列出来几个。
//
// 缺一整天时全列出来没有意义（一年 365 天），而只报个数字的话，人还得自己
// 对着日历找是哪一天。取前几个就够定位了。
func exampleDates(missing []calendar.Date) string {
	const shown = 3
	parts := make([]string, 0, shown+1)
	for i, date := range missing {
		if i == shown {
			parts = append(parts, "…")
			break
		}
		parts = append(parts, date.String())
	}
	return strings.Join(parts, "、")
}
