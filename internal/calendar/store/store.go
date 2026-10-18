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
	"encoding/json"
	"errors"
	"fmt"
	"time"

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

// Year 是 calendar_years 里的一行：某地区某一年的同步状态。
//
// 字段与列一一对应，只多不少：这是给运维核对用的（「哪一年灌过、什么时候灌的、
// 双源对得上吗」），而它们要看的正是库里那一行现在是什么。
type Year struct {
	Region string
	Year   int
	// Status 是那一列的**原文**，不翻成 calendar.YearState。
	//
	// 清单要回答的是「库里现在是什么」，而翻一遍之后，一个约束外的值（约束被摘
	// 掉过、或者手工 SQL 写过）会让整张清单报错——那正是最需要看到清单的时候。
	// 判定那一条路另有 parseYearState 把关，两件事不必共用一套处置。
	Status string
	// DayCount 是这一年写下的数据行数（status 不是 synced 时是 0）。
	DayCount int
	// SourcePaperURLs 是公告原文地址，空表示没有。
	SourcePaperURLs []string
	// SourcesAgree 是双源交叉校验的结果。nil 表示没得比（那一列可空）。
	SourcesAgree *bool
	// LastError 是同步失败的原因，status 为 degraded 时有意义。
	LastError string
	// SyncedAt 是这一行写下的时刻。
	SyncedAt time.Time
}

// List 列出导入过的年份，同地区内按年份**从新到旧**。
//
// 倒序是因为问得最多的一句是「今年、明年的日历灌了没有」，而这两个总在末尾——
// 正序排列时它们被十几年的历史压在下面。同一年只可能有一行（主键是
// (region, year)），所以这个顺序里没有并列。
func (s *Source) List(ctx context.Context) ([]Year, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT region, year, status, day_count, array_to_json(source_paper_urls)::text,
		       sources_agree, last_error, synced_at
		  FROM calendar_years
		 ORDER BY region, year DESC`)
	if err != nil {
		return nil, fmt.Errorf("查询日历年份失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// 从空切片起：一年都没有是正常结果（还没导入过），不是「没有值」。
	out := []Year{}
	for rows.Next() {
		var (
			y        Year
			agree    sql.NullBool
			lastErr  sql.NullString
			papersJS string
		)
		if err := rows.Scan(&y.Region, &y.Year, &y.Status, &y.DayCount, &papersJS,
			&agree, &lastErr, &y.SyncedAt); err != nil {
			return nil, fmt.Errorf("读取日历年份行失败: %w", err)
		}
		// text[] 走 array_to_json 转成 JSON 文本再解（与 internal/catalog 同一
		// 条）：pgx 的 database/sql 那一层把数组原样给成一个字符串，直接扫进
		// []string 会报 unsupported Scan。
		y.SourcePaperURLs = []string{}
		if err := json.Unmarshal([]byte(papersJS), &y.SourcePaperURLs); err != nil {
			return nil, fmt.Errorf("解析 %s %d 年的公告地址失败: %w", y.Region, y.Year, err)
		}
		if agree.Valid {
			v := agree.Bool
			y.SourcesAgree = &v
		}
		y.LastError = lastErr.String
		out = append(out, y)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历日历年份失败: %w", err)
	}
	return out, nil
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
