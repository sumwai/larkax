package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/sumwai/larkax/internal/calendar"
)

// Admin 是日历数据的写入面：把一份 Document 灌进库里。
//
// 它不在请求热路径上——那是 Source 的事。分成两个类型之后，热路径拿到的
// 那个对象拿不到写入能力。
type Admin struct {
	db *sql.DB
}

// NewAdmin 包装一个已打开的数据库句柄。
func NewAdmin(db *sql.DB) *Admin { return &Admin{db: db} }

// Result 汇总一次导入实际写了什么。
//
// 它是这条命令的**正向证据**：导入成功时不会有任何错误输出，而「什么都没写」
// 与「写成功了」在安静的终端里长得一样（见 LESSONS.md 第 16 条）。
type Result struct {
	Region string
	Year   int
	// Status 是这一年导入后的状态。
	Status calendar.YearState
	// DaysInYear 是落在本年之内的天数。status 为 synced 时它必然等于 365
	// 或 366——这一点在写入之前就校验过了（见 Document.validateDays）。
	DaysInYear int
	// DaysOutsideYear 是本文件带的年外天数：跨年的假期安排，例如元旦把
	// 去年 12 月 31 日挪成休息。它们同样写进库，所以要报出来。
	DaysOutsideYear int
	// Replaced 是被这次导入清掉的原有行数——同一地区、同一年份来源的行。
	// 同一年再灌一次时它不为零，这正是「覆盖而不是追加」的证据。
	Replaced int64
}

// Import 把一份文件写进库里，返回写了什么。
//
// # 同一年再灌一次是覆盖
//
// 导入先在同一个事务里删掉 `(region, source_year = year)` 的原有行，再写新的。
// 用 source_year 而不是「本文件里出现的那些日期」作删除条件：文件可能比上一次
// 少几天（数据源改了口径），按日期删就会把删掉的那几天留在库里，它们继续参与
// 判定，而文件里已经没有它们了。
//
// # 事务
//
// 删、写、更新年份状态三步在一个事务里。中途失败时库不会被留在「删了旧的、
// 新的还没写完」的状态上——那是一种不报错的坏法：那一年看起来还在，只是
// 每一天都变成了未知。
func (a *Admin) Import(ctx context.Context, doc Document) (Result, error) {
	prep, err := doc.prepare()
	if err != nil {
		return Result{}, err
	}
	result := Result{
		Region:          doc.Region,
		Year:            doc.Year,
		Status:          allowedStatuses[doc.Status],
		DaysInYear:      prep.inYear,
		DaysOutsideYear: prep.outsideYear,
	}

	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, fmt.Errorf("开启事务失败: %w", err)
	}
	// 提交之后再回滚是空操作，所以这里不需要一个「提交了没有」的开关。
	defer func() { _ = tx.Rollback() }()

	replaced, err := deleteYearRows(ctx, tx, doc.Region, doc.Year)
	if err != nil {
		return Result{}, err
	}
	result.Replaced = replaced

	if err := insertDays(ctx, tx, doc, prep.rows); err != nil {
		return Result{}, err
	}
	if err := upsertYear(ctx, tx, doc, prep.inYear); err != nil {
		return Result{}, err
	}

	if err := tx.Commit(); err != nil {
		return Result{}, fmt.Errorf("提交导入失败: %w", err)
	}
	return result, nil
}

// deleteYearRows 清掉这一年上一次导入写下的行，返回被清掉的行数。
func deleteYearRows(ctx context.Context, tx *sql.Tx, region string, year int) (int64, error) {
	res, err := tx.ExecContext(ctx,
		`DELETE FROM calendar_days WHERE region = $1 AND source_year = $2`, region, year)
	if err != nil {
		return 0, fmt.Errorf("清理 %s %d 年的原有日历数据失败: %w", region, year, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("确认清理结果失败: %w", err)
	}
	return affected, nil
}

// insertDays 写入每一天的判定。
//
// 冲突时改写成新值：同一天可以被相邻两年的数据集提到（跨年边界），而主键是
// (region, day)，一条只能有一个来源。**最后一次导入为准**——这与「同一年再灌
// 一次是覆盖」是同一条规则，写在这里免得两处各有一套。
func insertDays(ctx context.Context, tx *sql.Tx, doc Document, rows []calendar.DayRow) error {
	for _, row := range rows {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO calendar_days
				(region, day, holiday_name, is_off_day, is_makeup_workday, source, source_year, synced_at)
			VALUES ($1, $2::date, $3, $4, $5, $6, $7, now())
			ON CONFLICT (region, day) DO UPDATE SET
				holiday_name = EXCLUDED.holiday_name,
				is_off_day = EXCLUDED.is_off_day,
				is_makeup_workday = EXCLUDED.is_makeup_workday,
				source = EXCLUDED.source,
				source_year = EXCLUDED.source_year,
				synced_at = EXCLUDED.synced_at`,
			doc.Region, row.Date.String(), textOrNull(row.Name),
			row.OffDay, row.MakeupWorkday, doc.Source, doc.Year)
		if err != nil {
			return fmt.Errorf("写入 %s %s 的日历数据失败: %w", doc.Region, row.Date, err)
		}
	}
	return nil
}

// upsertYear 写年份状态。
//
// synced_at 每次导入都刷新：它记的是「这一行是什么时候写下的」。表上没有
// 单独的 updated_at，而「上一次同步是什么时候」正是排查数据陈旧时要看的。
func upsertYear(ctx context.Context, tx *sql.Tx, doc Document, dayCount int) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO calendar_years
			(region, year, status, day_count, source_paper_urls, sources_agree, last_error, synced_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, now())
		ON CONFLICT (region, year) DO UPDATE SET
			status = EXCLUDED.status,
			day_count = EXCLUDED.day_count,
			source_paper_urls = EXCLUDED.source_paper_urls,
			sources_agree = EXCLUDED.sources_agree,
			last_error = EXCLUDED.last_error,
			synced_at = EXCLUDED.synced_at`,
		doc.Region, doc.Year, doc.Status, dayCount,
		nonNilStrings(doc.SourcePaperURLs), doc.SourcesAgree, textOrNull(doc.LastError))
	if err != nil {
		return fmt.Errorf("写入 %s %d 年的日历状态失败: %w", doc.Region, doc.Year, err)
	}
	return nil
}

// textOrNull 把空串落成 NULL。
//
// calendar_days.holiday_name 与 calendar_years.last_error 都是可空的，读取侧
// 把 NULL 折成空串——所以「空串」与「NULL」不该是两种状态：存哪一种读出来都
// 一样，而看库的人会以为它们有区别。
func textOrNull(value string) any {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return value
}

// nonNilStrings 把 nil 切片换成空切片。
//
// source_paper_urls 是 NOT NULL DEFAULT '{}'：驱动把 nil 切片发成 NULL，
// 而 NULL 会被列约束拒掉，报错说的是「不能为空」，可调用方只是没填。
func nonNilStrings(value []string) []string {
	if value == nil {
		return []string{}
	}
	return value
}
