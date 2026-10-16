package assembly

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/billing/expr"
	"github.com/sumwai/larkax/internal/calendar"
)

// 日历那一根线：数据源 → 判定 → 适配器 → 计价环境。
//
// # 这一组用例要证明什么
//
// 日历的三段实现（internal/calendar、internal/calendar/store、
// internal/billing/calendaradapter）各自都有测试，但「它们接不接得上」在那
// 些测试里没有位置——测试自己就能构造调用方，所以一个只有测试在调的符号，
// 在覆盖率上与真在用的符号一模一样（见 LESSONS.md 第 22 条）。
//
// 所以这里断言的是**装配之后**能拿到什么：从 Build 装出来的对象出发，一路
// 问到数据库里那一行。断言具体值（哪一天是休息日、结算金额是多少），不是
// 「不是 nil」。
//
// # 没有 LARKAX_DB_DSN 时跳过
//
// 与仓库里其他集成测试一致，说明见 e2e_test.go 顶部。

// calendarRegion 是 settings 里 calendar.default_region 的出厂值。
//
// 用例里写死它而不是从配置读：这一组要证明的正是「这个值真的走到了日历上」，
// 拿被测对象里的值当期望值，就等于让被测对象给自己出题。
const calendarRegion = "CN"

// seedCalendarDay 把某地区某年标成已同步，并写进一天的数据行。
func seedCalendarDay(t *testing.T, db *sql.DB, day time.Time, name string, offDay bool) {
	t.Helper()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `
		INSERT INTO calendar_years (region, year, status, day_count)
		VALUES ($1, $2, 'synced', 365)
		ON CONFLICT (region, year) DO UPDATE SET status = 'synced'`,
		calendarRegion, day.Year()); err != nil {
		t.Fatalf("插日历年份失败: %v", err)
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO calendar_days
			(region, day, holiday_name, is_off_day, is_makeup_workday, source, source_year)
		VALUES ($1, $2::date, $3, $4, false, 'test-fixture', $5)
		ON CONFLICT (region, day) DO UPDATE
		   SET holiday_name = EXCLUDED.holiday_name,
		       is_off_day   = EXCLUDED.is_off_day`,
		calendarRegion, day.Format("2006-01-02"), name, offDay, day.Year()); err != nil {
		t.Fatalf("插日历数据行失败: %v", err)
	}
}

// 装配之后计价环境里的日历连着库里的数据。
func TestBuildWiresCalendarToDatabase(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)

	// 2031-02-03 是**周一**。挑工作日而不是周末是刻意的：周末用纯星期规则
	// 也能判成休息日，那样这条用例分辨不出「用了库里那一行」与「按星期推的」。
	holiday := time.Date(2031, 2, 3, 0, 0, 0, 0, time.UTC)
	const holidayName = "测试节假日"
	seedCalendarDay(t, db, holiday, holidayName, true)

	app := newApp(t, db, sealer)

	cal := app.Gateway.Pipeline.Calendar
	if cal == nil {
		t.Fatal("装配之后的 Env.Calendar 是 nil——日历那一根线没接上，" +
			"用到日历的规则会在运行期报 ErrCalendarRequired")
	}

	info, err := cal.DayInfo(calendarRegion, holiday)
	if err != nil {
		t.Fatalf("查 %s 失败: %v", holiday.Format("2006-01-02"), err)
	}
	if !info.OffDay {
		t.Errorf("%s（周一）的 OffDay = false，期望 true——库里那一行没被用上",
			holiday.Format("2006-01-02"))
	}
	if info.HolidayName != holidayName {
		t.Errorf("HolidayName = %q，期望 %q", info.HolidayName, holidayName)
	}

	// 时区是 Calendar 的另一半：规则靠它把时刻折成当地日期，折错了会在
	// 午夜前后落进相邻的一天。它不走数据库，但同样是这条线上的东西。
	tz, err := cal.TimeZone(calendarRegion)
	if err != nil {
		t.Fatalf("查 %s 的时区失败: %v", calendarRegion, err)
	}
	if tz != "Asia/Shanghai" {
		t.Errorf("时区 = %q，期望 Asia/Shanghai", tz)
	}

	// 库里没有那一行、而年份标着已同步：判定必须是「未知」并带出成因，
	// 不能回落到「按工作日算」。这一条顺带证明年份状态那一跳也接上了
	// ——它走的正是 Source 的另一个方法。
	missing := time.Date(2031, 3, 5, 0, 0, 0, 0, time.UTC)
	_, err = cal.DayInfo(calendarRegion, missing)
	if err == nil {
		t.Fatalf("%s 在库里没有数据行，却判出了一个结果——未知不能回落到某个默认值",
			missing.Format("2006-01-02"))
	}
	if !errors.Is(err, expr.ErrCalendarRequired) {
		t.Errorf("错误 = %v，期望能被 errors.Is 认成 ErrCalendarRequired", err)
	}
	if !strings.Contains(err.Error(), string(calendar.ReasonRowMissing)) {
		t.Errorf("错误 = %v，期望带出成因 %q（已同步的年份缺行与公告未发布，处置完全不同）",
			err, calendar.ReasonRowMissing)
	}
}

// 用到日历的计价规则真的按库里的日历算钱。
//
// 这一条覆盖的是地区那一跳：地区从 settings 的 calendar.default_region 读出
// 来，经 pipeline.Region → expr.Env.Region 走到日历上。少了它，日历接上了而
// 地区是空的，用到日历的规则照样算不出价——两种坏法在响应上都是 5xx。
func TestCalendarReachesPricingRules(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := context.Background()

	model := unique("e2e-model-calendar")

	// 周一，被安排成休息日。按星期推不出这个结论。
	holiday := time.Date(2031, 2, 3, 0, 0, 0, 0, time.UTC)
	seedCalendarDay(t, db, holiday, "测试节假日", true)

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, "100")
	key := issueKey(t, db, user)

	fake := &fakeUpstream{db: db, userID: user, model: model}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	seedChannel(t, db, sealer, merchant, upstreamSrv.URL, model)
	// 工作日与休息日两档价写死在规则里，于是期望金额可以逐位算出来。
	//
	// completion 那一档必须写一个价，而且要够大：预留按**最坏情况**取上界，
	// 输入侧用的是我们自己估的 prompt 数，而假上游固定报 12 个（比估出来的
	// 多）。若 completion 为 0，上界就只剩 prompt 那一项，会比实际结算还小，
	// 于是结算撞上 Capped、金额被钳回预留值——那时用例测的就不再是日历了。
	seedRule(t, db, model, `{"prompt": isWorkday() ? 0.0000010 : 0.0000030, "completion": 0.00001}`)

	// 北京时间的 2031-02-03 上午：换算过去仍落在同一天，用例不依赖
	// 「At 折成哪一天」这件事本身（那由 internal/billing/expr 的用例覆盖）。
	app := newAppAt(t, db, sealer, func() time.Time { return holiday.Add(2 * time.Hour) })
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	status, body := postChat(t, public.URL+"/v1/chat/completions", key, chatBody(model))
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200。响应体：%s", status, body)
	}

	// 假上游固定报 12 个 prompt token、7 个 completion token。
	// 12 × 0.0000030 + 7 × 0.00001 = 0.000106；若这一天被当成工作日则是
	// 0.000082，两者不会混。
	var final string
	if err := db.QueryRowContext(ctx, `
		SELECT final_amount::text FROM billing_requests
		 WHERE user_id = $1 ORDER BY id DESC LIMIT 1`, user).Scan(&final); err != nil {
		t.Fatalf("查账单失败: %v", err)
	}
	if !sameDecimal(final, "0.000106") {
		t.Errorf("结算金额 = %s，期望 0.000106（12 × 0.0000030 + 7 × 0.00001，"+
			"库里那一行标的是休息日）。响应体：%s", final, body)
	}
}

// 日历缺数据不阻止启动。
//
// 这一条钉的是日历数据的边界：数据逐年补，未来年份查不到属于正常状态，不能
// 拿它当启动条件。用例里连**今天**都查不到——那正是「库里没有日历数据」这个
// 前提的证据；少了它，这条用例可能只是「库里恰好有数据所以启动成功了」。
func TestBuildSucceedsWithoutCalendarData(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)

	app := newApp(t, db, sealer)

	cal := app.Gateway.Pipeline.Calendar
	if cal == nil {
		t.Fatal("装配之后的 Env.Calendar 是 nil——日历那一根线没接上")
	}

	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if _, err := cal.DayInfo(calendarRegion, today); err == nil {
		t.Fatalf("%s 在库里查得到日历数据，这条用例就证明不了「缺数据也能启动」——"+
			"换一个库，或换一个没有被灌过数据的年份", today.Format("2006-01-02"))
	}
}
