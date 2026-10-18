package expr

import (
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/billing"
)

// 这一组用例钉住 Result.LocalTime：求值要把**这次计价用的**当地口径报回来
// ——地区时区的钟点与那一天算不算休息日。这两个值最终落进 usage_events 的
// local_hour / is_off_day（见 internal/ledger/store 的 insertUsageEvent），
// 是事后复算一笔分时单子缺的输入。
//
// 断言不止「有值」：每一处都同时断言**规则当时走的那一支**。只钉「有值」的话，
// 一个永远报同一个钟点的实现也能通过，而它记下来的数复算不出当时的价——
// 这正是这两列要防的事。

// 工作日那一档：值要与判档所用的那一份一致，而两个值是同一份解析出来的。
func TestEvalReportsLocalBasisForWorkdayRule(t *testing.T) {
	const source = `isWorkday()
		? tier("workday", {"prompt": 0.0000015})
		: tier("rest_day", {"prompt": 0.0000040})`
	p := mustCompile(t, source)

	// testAt 是 2026-10-16 09:30 UTC = 北京时间 17:30，周五（工作日）。
	res, err := p.Eval(Env{At: testAt, Region: "CN", Calendar: newTestCalendar()})
	if err != nil {
		t.Fatal(err)
	}
	// 先看规则判成了哪一档：下面那几个值必须与它说的是同一件事。
	if res.Prices.Prompt.String() != "0.00000150" {
		t.Fatalf("单价 = %s，期望走工作日那一支的 0.00000150", res.Prices.Prompt)
	}
	if res.LocalTime == nil {
		t.Fatal("规则用了 isWorkday()，当地口径却是 nil——这两列会写成 NULL，" +
			"而这次请求的价正是按那一天算出来的")
	}
	if res.LocalTime.Hour != 17 {
		t.Errorf("当地钟点 = %d，期望 17（09:30 UTC 在北京时间就是 17 点）", res.LocalTime.Hour)
	}
	if res.LocalTime.OffDay {
		t.Error("OffDay = true，但规则走的是工作日那一支——" +
			"落库的 is_off_day 会与当时判的那一档相反")
	}

	// 同一个钟点、只剩日历日变了：价换档，休息日那一格跟着翻转。
	// 这一半是必要的——一个永远报「不是休息日」的实现也能通过上面那几条。
	saturday := time.Date(2026, 10, 17, 9, 30, 0, 0, time.UTC)
	res, err = p.Eval(Env{At: saturday, Region: "CN", Calendar: newTestCalendar()})
	if err != nil {
		t.Fatal(err)
	}
	if res.Prices.Prompt.String() != "0.00000400" {
		t.Fatalf("单价 = %s，期望走休息日那一支的 0.00000400", res.Prices.Prompt)
	}
	if res.LocalTime == nil {
		t.Fatal("当地口径是 nil")
	}
	if res.LocalTime.Hour != 17 {
		t.Errorf("当地钟点 = %d，期望 17", res.LocalTime.Hour)
	}
	if !res.LocalTime.OffDay {
		t.Error("OffDay = false，但规则走的是休息日那一支")
	}
}

// 只问峰谷的规则也要报：钟点正是 isPeak 的窗口量在哪个钟表上——
// 窗口按当地零点起算，所以这一格就是复算缺的那个输入。
func TestEvalReportsLocalBasisForPeakRule(t *testing.T) {
	cal := newTestCalendar()
	cal.peakMinutes = map[int]bool{17*60 + 30: true}

	p := mustCompile(t, `{"prompt": isPeak("08:00-22:00") ? 0.0000030 : 0.0000010}`)
	res, err := p.Eval(Env{At: testAt, Region: "CN", Calendar: cal})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Prices.Prompt.String(); got != "0.00000300" {
		t.Fatalf("单价 = %s，期望高峰那一档的 0.00000300", got)
	}
	if res.LocalTime == nil {
		t.Fatal("规则用了 isPeak()，当地口径却是 nil——这一格正是「当时当地几点」")
	}
	if res.LocalTime.Hour != 17 {
		t.Errorf("当地钟点 = %d，期望 17（那一分钟正是传去判峰谷的当地钟点）",
			res.LocalTime.Hour)
	}
	// 这一天的休息日属性来自同一份解析：规则没问它，但它是同一件事的另一半，
	// 与钟点一起落库（2026-10-16 在夹具里是普通工作日）。
	if res.LocalTime.OffDay {
		t.Error("OffDay = true，但这一天是工作日")
	}
}

// hour(tz) 不算「用到当地口径」：它的时区由规则作者逐次给出，问的是那一个
// 钟表，与地区的日历不是一套口径。
//
// 这类规则的两格写 NULL。理由是**没有东西可记**：那个时区名随规则源码落在
// billing_requests.pricing_expr_snapshot 里，复算拿它与 At 就够；而按地区
// 填一个钟点上去，会得到一个看起来权威、却与规则实际用的那个不同的数
// （下面这一条就是那种情形：规则问的是纽约，地区是 CN）。
func TestEvalLocalBasisIsNilForHourOnlyRule(t *testing.T) {
	for _, source := range []string{
		// 钟表与地区一致的情形也一样：这不是「值对不对」的问题，
		// 而是这两格的定义是**地区**那一套口径。
		`{"prompt": hour("Asia/Shanghai") >= 17 ? 0.0000020 : 0.0000010}`,
		// 钟表与地区不一致：按地区填会填出 17 点，而规则判档用的是 5 点。
		`{"prompt": hour("America/New_York") >= 17 ? 0.0000020 : 0.0000010}`,
	} {
		res := eval(t, source, billing.Usage{})
		if res.LocalTime != nil {
			t.Errorf("规则 %q 只问了 hour(tz)，当地口径却是 %+v——"+
				"这一格不该跟着规则自带的钟表走", source, *res.LocalTime)
		}
	}
}

// 一次都没碰时间与日历的规则同样写 NULL：值与「有没有值」是一件事的两面。
func TestEvalLocalBasisIsNilForFlatRule(t *testing.T) {
	res := eval(t, `{"prompt": 0.0000015, "completion": 0.0000075}`, billing.Usage{})
	if res.LocalTime != nil {
		t.Errorf("平铺价规则没有用到当地口径，却是 %+v", *res.LocalTime)
	}
}

// 最坏情况求值里那两格没有答案：条件整个被换成 __worst，这几个函数根本不会
// 被调到——与 Tier 留空同源。预留那一次的结果本来也不落库。
func TestEvalWorstCaseHasNoLocalBasis(t *testing.T) {
	cal := newTestCalendar()
	p := mustCompile(t, `isWorkday()
		? tier("workday", {"prompt": 0.0000015})
		: tier("rest_day", {"prompt": 0.0000040})`)

	res, err := p.EvalWorstCase(Env{Usage: billing.Usage{PromptTokens: 1},
		At: testAt, Region: "CN", Calendar: cal})
	if err != nil {
		t.Fatal(err)
	}
	if res.LocalTime != nil {
		t.Errorf("最坏情况求值报出了当地口径 %+v——那一侧「命中了哪一档」都没有"+
			"答案（见 EvalWorstCase）", *res.LocalTime)
	}
	if cal.dayCalls != 0 {
		t.Errorf("最坏情况求值查了 %d 次日历——条件不求值，这几次查询都不该发生",
			cal.dayCalls)
	}
}

// 同一次求值里「当地」只解析一次，问几遍都是同一份。
//
// 这一条钉的是这两列的来历：写进库的值与规则当时判的必须是**同一份解析**，
// 而不是两处各算一遍凑出来的两个数——两次查询之间日历被改过的话，
// 那样凑出来的两个数会不一样，而库里看不出哪个是判档用的那个。
//
// 变异：把 resolveLocal 的缓存去掉（每次都重新查），这条会看到 2 次查询。
func TestEvalResolvesLocalBasisOncePerEvaluation(t *testing.T) {
	cal := newTestCalendar()
	p := mustCompile(t, `{"prompt": (isWorkday() ? 1.0 : 0.0) + (isHoliday() ? 1.0 : 0.0)}`)

	res, err := p.Eval(Env{At: testAt, Region: "CN", Calendar: cal})
	if err != nil {
		t.Fatal(err)
	}
	// 2026-10-16 是工作日且不是节假日：两个判据一起给出 1.0。
	if got := res.Prices.Prompt.String(); got != "1.00000000" {
		t.Fatalf("单价 = %s，期望 1.0（工作日且非节假日）", got)
	}
	if cal.dayCalls != 1 {
		t.Errorf("一次求值查了 %d 次日历，期望 1 次——"+
			"同一个当地日在一次求值里只有一个答案", cal.dayCalls)
	}
	if res.LocalTime == nil || res.LocalTime.OffDay {
		t.Errorf("当地口径 = %+v，期望「不是休息日」", res.LocalTime)
	}
}
