// Package store 把 calendar_days / calendar_years 两张表接到 internal/calendar
// 的 Source 接口上，并提供把一整年的日历数据灌进库的写入路径。
//
// # 为什么单独一个包
//
// internal/calendar 的判定逻辑刻意不碰数据库（见那个包的文档），数据由调用方
// 通过 Source 注入。本包是那个接口的 PostgreSQL 实现，与 internal/ledger/store
// 对 internal/ledger 的分工相同：判定与存储分开之后，边界分支（未来年份、
// 数据缺失、记录自相矛盾）能在内存数据源上穷举，而不必在真库上构造。
//
// # 读与写是两个类型
//
// Source 在计费热路径上——用到日历的规则每个请求都要查一次；Admin 只在运维
// 动作里被调用。分成两个类型之后，热路径拿到的那个对象**没有**写入能力，
// 这是编译期的事实，不是一句约定。
//
// # 只有 synced 的年份带数据行
//
// 写入路径只允许 status 为 synced 的年份带 days（见 Document）。理由是
// calendar 的判定优先级：某一天只要有行就以行为准，年份状态只在行缺失时
// 才被问到。于是给一个 pending 或 conflict 的年份写进部分数据行，等于把
// 「这一年还没定下来」静默换成「这几天就这么算」——而账上看不出来。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/sumwai/larkax/internal/calendar"
)

// Source 从 calendar_days / calendar_years 读日历数据。
type Source struct {
	db *sql.DB
}

// NewSource 包装一个已打开的数据库句柄。
func NewSource(db *sql.DB) *Source { return &Source{db: db} }

// 编译期确认它满足 calendar.Source：接口改了要在这里报错，而不是在某个
// 计费请求上。
var _ calendar.Source = (*Source)(nil)

// LookupDay 查某地区某天的数据行。
//
// 查不到不是错误，exists 返回 false：调用方据此去问 YearState，由它区分
// 「这一年从没同步过」与「公告还没发布」——两者的处置完全不同，而在这里
// 合成一个错误就把那个区分丢掉了。
func (s *Source) LookupDay(ctx context.Context, region calendar.Region, day calendar.Date) (calendar.DayRow, bool, error) {
	var (
		name   sql.NullString
		offDay bool
		makeup bool
	)
	// 日期按 `YYYY-MM-DD` 文本传、在 SQL 里转 date，与 internal/catalog 的
	// 做法一致：直接把 time.Time 交给驱动会被当 timestamptz 发出去，而
	// timestamptz → date 的转换按**会话时区**算，同一个瞬间在 UTC+8 与 UTC-5
	// 下可能落在不同的日期上。日期本来就没有时区，这里不让它经过一次换算。
	err := s.db.QueryRowContext(ctx, `
		SELECT holiday_name, is_off_day, is_makeup_workday
		  FROM calendar_days
		 WHERE region = $1 AND day = $2::date`,
		string(region), day.String()).Scan(&name, &offDay, &makeup)
	if errors.Is(err, sql.ErrNoRows) {
		return calendar.DayRow{}, false, nil
	}
	if err != nil {
		return calendar.DayRow{}, false, fmt.Errorf("查询 %s %s 的日历数据失败: %w", region, day, err)
	}
	return calendar.DayRow{
		// 日期用请求的那一天，不用从库里扫回来的值：day 是主键的一半，
		// 「查到的就是这一天」由 WHERE 保证，而扫回来的 time.Time 还带着
		// 一个时区，再换算成 Date 就多一处会出错的地方。
		Date:          day,
		Name:          name.String,
		OffDay:        offDay,
		MakeupWorkday: makeup,
	}, true, nil
}

// YearState 查某地区某年的同步状态。
//
// 库里没有这一年的记录返回 YearUnknown（「从没同步过」），与 YearPending
// （「公告还没发布」）分开：前者说明同步任务没跑过，后者是未来年份的正常状态。
func (s *Source) YearState(ctx context.Context, region calendar.Region, year int) (calendar.YearState, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `
		SELECT status FROM calendar_years WHERE region = $1 AND year = $2`,
		string(region), year).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return calendar.YearUnknown, nil
	}
	if err != nil {
		return calendar.YearUnknown, fmt.Errorf("查询 %s %d 年的日历状态失败: %w", region, year, err)
	}

	state, err := parseYearState(raw)
	if err != nil {
		return calendar.YearUnknown, fmt.Errorf("%s %d 年: %w", region, year, err)
	}
	return state, nil
}

// parseYearState 把 calendar_years.status 的文本翻成 calendar 的取值。
//
// 单独一个函数是为了能脱离数据库测「读到一个约束外的值」这条分支：表上那一列
// 有 CHECK 约束，在真库上造出这样的行要先摘掉约束，而那会把库留在一个
// 没有保护的状态上。
//
// 认不出就报错，不回落成 YearUnknown：回落会把它伪装成「从没同步过」，
// 而调用方对这两种情况的处置完全不同（一个去查同步任务，一个去查那一行数据）。
func parseYearState(raw string) (calendar.YearState, error) {
	state, ok := yearStates[raw]
	if !ok {
		return calendar.YearUnknown, fmt.Errorf(
			"日历状态 %q 不在 calendar_years.status 的取值范围内（synced / pending / conflict / degraded）", raw)
	}
	return state, nil
}

// yearStates 把库里的状态文本翻成 calendar 的取值。
//
// 显式列出四个而不是做字符串转换：YearState 是个有零值的类型，多一个
// 认不出的取值时，转换会把一个约束外的值原样带进判定链路。
var yearStates = map[string]calendar.YearState{
	string(calendar.YearSynced):   calendar.YearSynced,
	string(calendar.YearPending):  calendar.YearPending,
	string(calendar.YearConflict): calendar.YearConflict,
	string(calendar.YearDegraded): calendar.YearDegraded,
}
