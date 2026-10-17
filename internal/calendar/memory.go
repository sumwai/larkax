package calendar

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// MemorySource 是 Source 的内存实现。
//
// 它有两个用途：
//
//  1. **测试**：判定逻辑要能在没有数据库的情况下覆盖各种边界——未来年份、
//     数据缺失的月份、自相矛盾的记录。这些在真库上极难构造。
//  2. **兜底与离线使用**：把一份数据快照装在内存里，避免每次判定都查库。
//
// **目前只有测试在构造它**（`internal/calendar`、`internal/calendar/store`、
// `internal/billing/calendaradapter` 三处），上面第 2 条还没有调用方：
// 生产装配走的是 `internal/calendar/store` 的 Source。整个类型连同它的
// 五个方法与 NewMemorySource 都只被测试用到，是夹具而不是漏接的线。
//
// 它**不写回数据**，也不做任何默认填充：查不到就是查不到，
// 「未知」这条路径必须与真实数据源表现一致，否则测试会掩盖真实的回退行为。
//
// 零值可用。
type MemorySource struct {
	mu sync.RWMutex
	// days 按 (地区, 日期) 存行。用 Date 作键而不是字符串：
	// 字符串键会把「2026-1-05」这类写法与规范写法当成两天。
	days map[regionDay]DayRow
	// years 按 (地区, 年) 存状态。没有条目即为 YearUnknown。
	years map[regionYear]YearState
	// defaults 是 [start, end] 闭区间内、没有单独登记的日子所依据的模板。
	// 真实数据源按年整块给数据，测试里逐个登记一整年会淹没边界用例，
	// 所以允许「先铺一层底色，再挑几天覆盖」。
	//
	// 模板是一元的函数而不是一行固定的值：一年的日历里**每一天都不同**
	// （周末与工作日相反），拿一行常量铺一整年会让周末悄悄变成工作日。
	// 由调用方按日期给出该天应有的行，「周末是不是休息日」这条规则就留在
	// 调用方那里，本包不会替它决定。
	defaults map[regionRange]dayTemplate
}

type regionDay struct {
	region Region
	day    Date
}

type regionYear struct {
	region Region
	year   int
}

type regionRange struct {
	region Region
	start  Date
	end    Date
}

// dayTemplate 由日期算出那一天应有的数据行。
type dayTemplate func(Date) DayRow

// NewMemorySource 构造一个空的内存数据源：任何查询都返回「没有数据」。
func NewMemorySource() *MemorySource {
	return &MemorySource{}
}

// AddDay 登记某地区某一天的数据行。
//
// 重复登记同一天会覆盖前一条并返回错误：多源同步时同一天被写两次是常事
// （先铺默认、后由公告覆盖），但**静默**覆盖会让「覆盖了哪一天」无从追查，
// 所以查得出来。
func (m *MemorySource) AddDay(region Region, row DayRow) error {
	if !row.Date.Valid() {
		return fmt.Errorf("%w: %s", ErrInvalidDate, row.Date)
	}
	if _, err := Timezone(region); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.days == nil {
		m.days = make(map[regionDay]DayRow)
	}
	key := regionDay{region: region, day: row.Date}
	_, existed := m.days[key]
	m.days[key] = row
	if existed {
		return fmt.Errorf("%s %s 的数据行被覆盖", region, row.Date)
	}
	return nil
}

// SetDefaultRange 给 [start, end] 内的每一天铺上由 template 算出的行（闭区间）。
//
// 模板是逐日调用的函数而不是一行固定值：一年的日历里每一天都可能不同，
// 拿一行常量铺一整年会把周末与工作日铺成同一种。常见的用法是让模板返回
// 一行「OffDay 等于该日是不是周末」，也就是「假设没有调休」的状态；
// 真实数据源里那种状态对应 calendar_years 的 pending——但那不是本包的结论，
// 本包只认数据行：没有行就是未知，不会因为有默认区间就把未知当成某一天。
//
// 只对**没有单独登记**的日子生效，已在 days 里的行不受影响——
// 覆盖关系固定下来，测试才不会因为调用次序不同而结果不同。
func (m *MemorySource) SetDefaultRange(region Region, start, end Date, template dayTemplate) error {
	if !start.Valid() {
		return fmt.Errorf("%w: %s", ErrInvalidDate, start)
	}
	if !end.Valid() {
		return fmt.Errorf("%w: %s", ErrInvalidDate, end)
	}
	if end.cmp(start) < 0 {
		return fmt.Errorf("默认区间起点 %s 晚于终点 %s", start, end)
	}
	if template == nil {
		return errors.New("默认区间的模板为空")
	}
	if _, err := Timezone(region); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.defaults == nil {
		m.defaults = make(map[regionRange]dayTemplate)
	}
	m.defaults[regionRange{region: region, start: start, end: end}] = template
	return nil
}

// SetYearState 设置某地区某年的同步状态。
//
// 显式设成 YearPending 与「从来没设过」（YearUnknown）必须分开：
// 前者是「公告还没发」，后者是「我们不知道有没有发」。
func (m *MemorySource) SetYearState(region Region, year int, state YearState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.years == nil {
		m.years = make(map[regionYear]YearState)
	}
	m.years[regionYear{region: region, year: year}] = state
}

// LookupDay 实现 Source。
func (m *MemorySource) LookupDay(_ context.Context, region Region, day Date) (DayRow, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if row, ok := m.days[regionDay{region: region, day: day}]; ok {
		return row, true, nil
	}
	for r, template := range m.defaults {
		if r.region != region {
			continue
		}
		if day.cmp(r.start) < 0 || day.cmp(r.end) > 0 {
			continue
		}
		row := template(day)
		// 模板忘了按传入日期填 Date 时补上，否则调用方从返回的行里
		// 读到的会是模板里写死的那个日期。行本身仍以模板的取值为准。
		if row.Date != day {
			row.Date = day
		}
		return row, true, nil
	}
	return DayRow{}, false, nil
}

// YearState 实现 Source。
func (m *MemorySource) YearState(_ context.Context, region Region, year int) (YearState, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.years[regionYear{region: region, year: year}], nil
}

// 编译期确认 MemorySource 满足 Source。
var _ Source = (*MemorySource)(nil)
