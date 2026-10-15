package calendar

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMemorySourceEmptyReturnsNothing(t *testing.T) {
	m := NewMemorySource()
	_, exists, err := m.LookupDay(context.Background(), RegionCN, d(2026, time.January, 5))
	if err != nil {
		t.Fatalf("LookupDay 出错: %v", err)
	}
	if exists {
		t.Error("空数据源不该返回数据行")
	}
	state, err := m.YearState(context.Background(), RegionCN, 2026)
	if err != nil {
		t.Fatalf("YearState 出错: %v", err)
	}
	if state != YearUnknown {
		t.Errorf("没有年份记录时状态 = %q，期望零值 %q", state, YearUnknown)
	}
}

func TestMemorySourceAddAndLookup(t *testing.T) {
	m := NewMemorySource()
	row := DayRow{Date: d(2026, time.January, 1), Name: "元旦", OffDay: true}
	if err := m.AddDay(RegionCN, row); err != nil {
		t.Fatalf("AddDay 出错: %v", err)
	}

	got, exists, err := m.LookupDay(context.Background(), RegionCN, row.Date)
	if err != nil {
		t.Fatalf("LookupDay 出错: %v", err)
	}
	if !exists {
		t.Fatal("登记过的日期应当查得到")
	}
	if got != row {
		t.Errorf("读回的行 = %+v，期望 %+v", got, row)
	}
}

func TestMemorySourceAddRejectsBadInput(t *testing.T) {
	m := NewMemorySource()
	if err := m.AddDay(RegionCN, DayRow{Date: d(2026, time.February, 30)}); !errors.Is(err, ErrInvalidDate) {
		t.Errorf("非法日期应报 ErrInvalidDate，得到 %v", err)
	}
	if err := m.AddDay("XX", DayRow{Date: d(2026, time.January, 1)}); !errors.Is(err, ErrUnknownRegion) {
		t.Errorf("未知地区应报 ErrUnknownRegion，得到 %v", err)
	}
}

// 覆盖同一天要能查得出来：多源同步时这是常事，而静默覆盖会让
// 「到底哪一份生效」无从追查。
func TestMemorySourceDuplicateAddReports(t *testing.T) {
	m := NewMemorySource()
	day := d(2026, time.January, 1)
	if err := m.AddDay(RegionCN, DayRow{Date: day, OffDay: true}); err != nil {
		t.Fatalf("第一次 AddDay 出错: %v", err)
	}
	if err := m.AddDay(RegionCN, DayRow{Date: day, OffDay: false}); err == nil {
		t.Error("重复登记同一天应当报错")
	}

	got, exists, err := m.LookupDay(context.Background(), RegionCN, day)
	if err != nil || !exists {
		t.Fatalf("覆盖后仍应查得到: exists=%v err=%v", exists, err)
	}
	if got.OffDay {
		t.Error("重复登记应以后写入的为准")
	}
}

// allWorkdays 是占位形态的默认模板：区间内每一天都算工作日。
// 用它的用例都不涉及周末，这样判定只取决于单独登记的那几天。
func allWorkdays(day Date) DayRow { return DayRow{Date: day} }

func TestMemorySourceDefaultRange(t *testing.T) {
	m := NewMemorySource()
	if err := m.SetDefaultRange(RegionCN,
		d(2026, time.January, 5), d(2026, time.January, 9), allWorkdays); err != nil {
		t.Fatalf("SetDefaultRange 出错: %v", err)
	}
	ctx := context.Background()

	in := d(2026, time.January, 7)
	row, exists, err := m.LookupDay(ctx, RegionCN, in)
	if err != nil || !exists {
		t.Fatalf("区间内的日期应查得到: exists=%v err=%v", exists, err)
	}
	if row.Date != in {
		t.Errorf("默认行的日期未按查询日回填：%s", row.Date)
	}

	for _, out := range []Date{
		d(2026, time.January, 4),  // 区间前一天
		d(2026, time.January, 10), // 区间后一天
	} {
		if _, exists, _ := m.LookupDay(ctx, RegionCN, out); exists {
			t.Errorf("%s 在区间之外，不该返回数据行", out)
		}
	}

	// 闭区间两端都要覆盖到。
	for _, edge := range []Date{d(2026, time.January, 5), d(2026, time.January, 9)} {
		if _, exists, _ := m.LookupDay(ctx, RegionCN, edge); !exists {
			t.Errorf("%s 是区间端点，应当覆盖到", edge)
		}
	}

	// 跨年的默认区间也要能覆盖。
	if err := m.SetDefaultRange(RegionCN,
		d(2025, time.December, 30), d(2026, time.January, 2), allWorkdays); err != nil {
		t.Fatalf("跨年 SetDefaultRange 出错: %v", err)
	}
	for _, day := range []Date{
		d(2025, time.December, 30), d(2025, time.December, 31),
		d(2026, time.January, 1), d(2026, time.January, 2),
	} {
		if _, exists, _ := m.LookupDay(ctx, RegionCN, day); !exists {
			t.Errorf("%s 在跨年区间内，应当覆盖到", day)
		}
	}
}

func TestMemorySourceDefaultRangeRejectsBadInput(t *testing.T) {
	m := NewMemorySource()
	if err := m.SetDefaultRange(RegionCN,
		d(2026, time.January, 9), d(2026, time.January, 5), allWorkdays); err == nil {
		t.Error("起点晚于终点应当报错")
	}
	if err := m.SetDefaultRange(RegionCN,
		d(2026, time.February, 30), d(2026, time.March, 1), allWorkdays); !errors.Is(err, ErrInvalidDate) {
		t.Errorf("非法起点应报 ErrInvalidDate，得到 %v", err)
	}
	if err := m.SetDefaultRange("XX",
		d(2026, time.January, 1), d(2026, time.January, 5), allWorkdays); !errors.Is(err, ErrUnknownRegion) {
		t.Errorf("未知地区应报 ErrUnknownRegion，得到 %v", err)
	}
	// 模板为空时铺出来的每一天都是「没有数据」，那会让查不到与查得到
	// 混同，所以直接拒绝。
	if err := m.SetDefaultRange(RegionCN,
		d(2026, time.January, 1), d(2026, time.January, 5), nil); err == nil {
		t.Error("模板为空应当报错")
	}
}

// 单独登记的行优先于默认区间，且与两者设置的先后无关。
func TestMemorySourceExplicitDayWinsOverDefault(t *testing.T) {
	holiday := d(2026, time.January, 1)
	row := DayRow{Date: holiday, Name: "元旦", OffDay: true}

	// 先铺默认，再登记单日。
	first := NewMemorySource()
	if err := first.SetDefaultRange(RegionCN,
		d(2026, time.January, 1), d(2026, time.December, 31), plainYear); err != nil {
		t.Fatalf("SetDefaultRange 出错: %v", err)
	}
	if err := first.AddDay(RegionCN, row); err != nil {
		t.Fatalf("AddDay 出错: %v", err)
	}

	// 先登记单日，再铺默认。
	second := NewMemorySource()
	if err := second.AddDay(RegionCN, row); err != nil {
		t.Fatalf("AddDay 出错: %v", err)
	}
	if err := second.SetDefaultRange(RegionCN,
		d(2026, time.January, 1), d(2026, time.December, 31), plainYear); err != nil {
		t.Fatalf("SetDefaultRange 出错: %v", err)
	}

	for name, m := range map[string]*MemorySource{"先默认后单日": first, "先单日后默认": second} {
		got, exists, err := m.LookupDay(context.Background(), RegionCN, holiday)
		if err != nil || !exists {
			t.Fatalf("%s: exists=%v err=%v", name, exists, err)
		}
		if got != row {
			t.Errorf("%s: 读回 %+v，期望单独登记的那一行 %+v", name, got, row)
		}
	}
}

// 同一地区可以有多个默认区间，先设置的先命中；已单独登记的行始终优先。
func TestMemorySourceMultipleDefaultRanges(t *testing.T) {
	m := NewMemorySource()
	first := d(2026, time.January, 1)
	if err := m.SetDefaultRange(RegionCN, first, d(2026, time.January, 31), plainYear); err != nil {
		t.Fatalf("SetDefaultRange 出错: %v", err)
	}
	if err := m.SetDefaultRange(RegionCN,
		d(2026, time.February, 1), d(2026, time.February, 28), plainYear); err != nil {
		t.Fatalf("SetDefaultRange 出错: %v", err)
	}
	for _, day := range []Date{
		d(2026, time.January, 15),
		d(2026, time.February, 15),
	} {
		if _, exists, _ := m.LookupDay(context.Background(), RegionCN, day); !exists {
			t.Errorf("%s 应落在某个默认区间里", day)
		}
	}
}

// 模板返回的日期与查询日不一致时，以查询日为准：调用方从返回的行里读到
// 的日期必须是它问的那一天，否则按行里的日期记账会记到别的日子上。
func TestMemorySourceTemplateDateIsNormalized(t *testing.T) {
	m := NewMemorySource()
	wrong := d(2020, time.March, 3)
	if err := m.SetDefaultRange(RegionCN, d(2026, time.January, 1), d(2026, time.January, 31),
		func(Date) DayRow { return DayRow{Date: wrong} }); err != nil {
		t.Fatalf("SetDefaultRange 出错: %v", err)
	}

	asked := d(2026, time.January, 15)
	row, exists, err := m.LookupDay(context.Background(), RegionCN, asked)
	if err != nil || !exists {
		t.Fatalf("exists=%v err=%v", exists, err)
	}
	if row.Date != asked {
		t.Errorf("返回行的日期 = %s，期望查询日 %s", row.Date, asked)
	}
}

func TestMemorySourceSetYearState(t *testing.T) {
	m := NewMemorySource()
	m.SetYearState(RegionCN, 2027, YearPending)

	state, err := m.YearState(context.Background(), RegionCN, 2027)
	if err != nil {
		t.Fatalf("YearState 出错: %v", err)
	}
	if state != YearPending {
		t.Errorf("状态 = %q，期望 %q", state, YearPending)
	}

	// 没有设置过的年份仍是 YearUnknown：不能把「没设」与「已设成 pending」混同。
	other, err := m.YearState(context.Background(), RegionCN, 2028)
	if err != nil {
		t.Fatalf("YearState 出错: %v", err)
	}
	if other != YearUnknown {
		t.Errorf("未设置的年份 = %q，期望 %q", other, YearUnknown)
	}
}

// 不同地区的同名日期互不干扰。
func TestMemorySourceIsolatesRegions(t *testing.T) {
	// 目前只有 CN 登记在册，所以「另一个地区」的隔离只能通过未知地区
	// 被拒绝来验证——这正是我们希望的行为：不登记的地区不该悄悄读 CN 的数据。
	m := NewMemorySource()
	day := d(2026, time.January, 1)
	if err := m.AddDay(RegionCN, DayRow{Date: day, OffDay: true}); err != nil {
		t.Fatalf("AddDay 出错: %v", err)
	}
	if _, exists, _ := m.LookupDay(context.Background(), "XX", day); exists {
		t.Error("未登记的地区不该读到 CN 的数据")
	}
}

func TestMemorySourceZeroValueUsable(t *testing.T) {
	var m MemorySource
	_, exists, err := m.LookupDay(context.Background(), RegionCN, d(2026, time.January, 5))
	if err != nil || exists {
		t.Errorf("零值应可用且返回空: exists=%v err=%v", exists, err)
	}
}

// 并发读写下不能有数据竞争（配合 -race 跑）。
func TestMemorySourceConcurrentAccess(t *testing.T) {
	m := NewMemorySource()
	if err := m.SetDefaultRange(RegionCN,
		d(2026, time.January, 1), d(2026, time.December, 31), plainYear); err != nil {
		t.Fatalf("SetDefaultRange 出错: %v", err)
	}
	cal, err := New(m)
	if err != nil {
		t.Fatalf("New 出错: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			_ = m.AddDay(RegionCN, DayRow{Date: d(2026, time.January, i%28+1), OffDay: i%2 == 0})
			m.SetYearState(RegionCN, 2026, YearSynced)
		}
	}()

	for i := 0; i < 50; i++ {
		day := d(2026, time.March, i%28+1)
		if _, err := cal.Lookup(context.Background(), RegionCN, day); err != nil {
			t.Fatalf("并发读取出错: %v", err)
		}
	}
	<-done
}
