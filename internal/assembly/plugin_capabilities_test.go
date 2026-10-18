package assembly

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/calendar"
	"github.com/sumwai/larkax/internal/plugin/sandbox"
	"github.com/sumwai/larkax/internal/settings"
)

// 这一组用例管的是**能力清单从装配推导**这一条：允许申请哪些能力，由这次
// 装配真的接上了什么算出来（host.Available），不再是一份手写的字面量。
//
// 两份清单各自维护过一次的代价写在 issue 里：插件申请 kv **注册成功**，
// 运行期 globalThis.__larkax.kv 是 undefined，插件拿到的是 JS 的 TypeError，
// 而不是文档承诺的「能力未启用」。
//
// # 没有 LARKAX_DB_DSN 时跳过
//
// 与仓库里其他集成测试一致，说明见 e2e_test.go 顶部。

// 判据的另一半（正向）：这份装配接上了 kv 与 calendar，于是申请它们的插件
// 该注册成功，而且生效的能力清单就是它申请的那两个。
//
// 少了这一条，「把所有插件都跳过」的实现也能过下面那条负向用例。
func TestBuildRegistersPluginAskingForWiredCapabilities(t *testing.T) {
	db := testDB(t)
	key := insertPluginWithCapabilities(t, db,
		[]string{sandbox.CapabilityKV, sandbox.CapabilityCalendar})

	app, err := buildAppWithSettings(t, db, map[string]string{"plugin.enabled": `true`})
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	p, ok := app.Plugins.Get(key)
	if !ok {
		t.Fatalf("申请了 kv 与 calendar 的插件该上线——这份装配两项都接上了")
	}
	want := []string{sandbox.CapabilityCalendar, sandbox.CapabilityKV}
	if len(p.Capabilities) != len(want) {
		t.Fatalf("生效的能力 = %v，期望 %v", p.Capabilities, want)
	}
	for i := range want {
		if p.Capabilities[i] != want[i] {
			t.Fatalf("生效的能力 = %v，期望 %v（这个顺序是排序后的）", p.Capabilities, want)
		}
	}
}

// 判据（这一片的核心）：只提供 log 的装配下，申请 kv 的插件**注册失败**。
//
// 这是「允许集由装配推导」的可执行证据——回答「允许申请哪些」的那份清单就是
// wiring 里真的接上了什么。把它换回一个手写的字面量（["log","calendar","kv"]）
// 时这条用例立刻红：插件会注册成功，运行期拿到一个 undefined 的能力。
//
// 用一份**手动构造的** wiring（两个依赖都是 nil）而不是改生产装配：生产那一份
// 两样都接上了，构造不出「没接」这个状态，而它恰恰是要判的那一端。这也是
// pluginWiring 的字段可空的意义（见那个类型的注释）。
func TestBuildSkipsPluginAskingUnwiredCapability(t *testing.T) {
	db := testDB(t)
	values := enablePlugins(t, db)

	wantsKV := insertPluginWithCapabilities(t, db, []string{sandbox.CapabilityKV})
	wantsLog := insertPluginWithCapabilities(t, db, []string{sandbox.CapabilityLog})

	var logged bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logged, nil))

	bare := pluginWiring{logger: logger}
	if got := bare.provided(); len(got) != 1 || got[0] != sandbox.CapabilityLog {
		t.Fatalf("这份 wiring 什么都没接，provided 该只有 log，实际 %v", got)
	}

	reg, _, err := buildPlugins(Config{DB: db, Settings: values}, logger, bare)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if _, ok := reg.Get(wantsKV); ok {
		t.Error("这份装配没接 kv，申请 kv 的插件不该上线——它该在注册期就失败，" +
			"而不是注册成功、运行期拿到一个 undefined 的能力")
	}
	// 跳过是安静的：日志里不点名的话，线上看到的只是「我那个插件没生效」。
	if !strings.Contains(logged.String(), wantsKV) {
		t.Errorf("跳过的插件该在日志里被点名:\n%s", logged.String())
	}
	if _, ok := reg.Get(wantsLog); !ok {
		t.Error("只申请 log 的插件该照常上线——少了这一半，一个什么都不装的实现也能过")
	}

	// 同一行数据，把两个依赖接上，它就上线了。这一条分辨的是「按 wiring 判」
	// 与「按某个写死的清单判」：写死的清单要么永远放行、要么永远拒绝，
	// 两种都过不了这一对用例。
	reg, _, err = buildPlugins(Config{DB: db, Settings: values}, logger,
		pluginWiring{logger: logger, now: time.Now,
			calendar: pluginCalendar{cal: testCalendar(t, db)}, kv: recordingKVForWiring{}})
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if _, ok := reg.Get(wantsKV); !ok {
		t.Error("接上依赖之后，同一个插件该上线")
	}
}

// 日历那一根线接上之后，插件侧拿到的是**库里的判定**。
//
// 断的是具体值（哪一天是休息日、当地几点、星期几），不是「不是 nil」：
// 一个把三态压成工作日、或者把时区折错一天的实现也能过「不是 nil」那种断言。
//
// # 本地时刻与星期是这一条的另一个重点
//
// 插件拿到的是时间戳，折算到当地时区这一步由适配器做（host.DayInfo 里的
// LocalHour / LocalWeekday）。折错的表现是插件在午夜前后按前一天判断。
func TestPluginCalendarDayUsesDatabase(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	// 2031-02-03 是**周一**，库里把它标成休息日：按星期推不出这个结论，
	// 于是「用了库里那一行」与「按星期推的」在断言上是可分辨的
	// （与 calendar_test.go 用同一个日子）。
	holiday := time.Date(2031, 2, 3, 0, 0, 0, 0, time.UTC)
	const holidayName = "测试节假日"
	seedCalendarDay(t, db, holiday, holidayName, true)

	src := pluginCalendar{cal: testCalendar(t, db), peak: pluginPeakSchedule}

	// 北京时间的上午十点：换算过去仍落在同一天。
	instant := holiday.Add(2 * time.Hour)
	info, err := src.Day(ctx, calendarRegion, instant)
	if err != nil {
		t.Fatalf("Day 失败: %v", err)
	}
	if info.Kind != "rest_day" {
		t.Errorf("Kind = %q，期望 rest_day（库里那一行标的是休息日）", info.Kind)
	}
	if info.Name != holidayName {
		t.Errorf("Name = %q，期望 %q", info.Name, holidayName)
	}
	if info.LocalHour != 10 {
		t.Errorf("LocalHour = %d，期望 10（北京时间的上午十点）", info.LocalHour)
	}
	if info.LocalWeekday != 1 {
		t.Errorf("LocalWeekday = %d，期望 1——周一，而 JS 的 getDay() 里周日是 0", info.LocalWeekday)
	}

	// 库里没有那一行：三态里的 unknown 如实传出去，不压成工作日，也不压成休息日。
	// 压成任何一个都是一次替插件做的猜测，而两侧的价差不会报错。
	missing := time.Date(2031, 3, 5, 0, 0, 0, 0, time.UTC)
	info, err = src.Day(ctx, calendarRegion, missing)
	if err != nil {
		t.Fatalf("缺数据的那一天该给出 unknown，而不是报错: %v", err)
	}
	if info.Kind != "unknown" {
		t.Errorf("缺数据的那一天 Kind = %q，期望 unknown", info.Kind)
	}

	// 地区认不出来时不给判定：报错让插件这条分支断掉，好过给一个猜的答案。
	if _, err := src.Day(ctx, "不存在的地区", instant); err == nil {
		t.Error("认不出的地区该报错")
	}
}

// Peak 是**真实现**：它调的是 calendar.PeakAt，三个取值都从判定里出来。
//
// 它今天没有插件能调到（SDK 的 calendar 命名空间里没有 peak 函数），所以
// 这条用例是它唯一的调用点——这也是 scripts/deadmethods-allowlist.txt 里
// `pluginCalendar.Peak` 那一条要写清「它为什么留着」的原因：规则那一半已经
// 接上（窗口随规则走），挡在这里的是插件侧没有 peak 可调。
func TestPluginCalendarPeakDelegatesToCalendar(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	holiday := time.Date(2031, 2, 3, 0, 0, 0, 0, time.UTC)
	seedCalendarDay(t, db, holiday, "测试节假日", true)
	cal := testCalendar(t, db)

	// 上午十点（北京）与晚上十一点（北京），同一天。
	morning, evening := holiday.Add(2*time.Hour), holiday.Add(15*time.Hour)

	// 一、生产那一份排班是空的：高峰窗口从哪来还没定（见 pluginPeakSchedule），
	// 于是已知的日子一律 off_peak，不会凭「今天是休息日」猜成高峰。
	prod := pluginCalendar{cal: cal, peak: pluginPeakSchedule}
	if got, err := prod.Peak(ctx, calendarRegion, morning); err != nil || got != "off_peak" {
		t.Errorf("生产排班下 Peak(上午十点) = (%q, %v)，期望 off_peak", got, err)
	}

	// 二、给一份带窗口的排班，判定的三段就都能看见。这一条证明适配器把窗口
	// **传下去了**，而不是自己按某种规则算了一个答案。
	schedule, err := calendar.NewPeakSchedule(false,
		calendar.PeakWindow{StartMinute: 8 * 60, EndMinute: 22 * 60})
	if err != nil {
		t.Fatalf("构造排班失败: %v", err)
	}
	windowed := pluginCalendar{cal: cal, peak: schedule}

	if got, err := windowed.Peak(ctx, calendarRegion, morning); err != nil || got != "peak" {
		t.Errorf("上午十点落在 [08:00, 22:00) 里，Peak = (%q, %v)，期望 peak", got, err)
	}
	if got, err := windowed.Peak(ctx, calendarRegion, evening); err != nil || got != "off_peak" {
		t.Errorf("晚上十一点在窗口之外，Peak = (%q, %v)，期望 off_peak", got, err)
	}
	// 日子未知时判定为 unknown：不套用「周末算低谷」之类的星期五规则。
	missing := time.Date(2031, 3, 5, 2, 0, 0, 0, time.UTC)
	if got, err := windowed.Peak(ctx, calendarRegion, missing); err != nil || got != "unknown" {
		t.Errorf("缺数据的那一天 Peak = (%q, %v)，期望 unknown", got, err)
	}
}

// ── 助手 ──────────────────────────────────────────────────────────────

// testCalendar 装一份连着测试库的日历。
func testCalendar(t *testing.T, db *sql.DB) *calendar.Calendar {
	t.Helper()
	cal, _, err := buildCalendar(db)
	if err != nil {
		t.Fatalf("构造日历失败: %v", err)
	}
	return cal
}

// recordingKVForWiring 是一个不存东西的 KV，只为把 wiring 的 kv 那一项填上。
//
// 这一组用例判的是**允许集从哪来**，不是 KV 的读写（那由 internal/plugin/store
// 的集成用例逐条钉）。真实现要连库、要用户，而这里只需要「非 nil」。
type recordingKVForWiring struct{}

func (recordingKVForWiring) Get(context.Context, string, int64, string) (string, bool, error) {
	return "", false, nil
}

func (recordingKVForWiring) Set(context.Context, string, int64, string, string, time.Duration) error {
	return nil
}

func (recordingKVForWiring) Del(context.Context, string, int64, string) (bool, error) {
	return false, nil
}

// insertPluginWithCapabilities 往库里放一条 active 插件，它的 meta 声明了这些能力。
func insertPluginWithCapabilities(t *testing.T, db *sql.DB, caps []string) string {
	t.Helper()
	key := newPluginKey(t, "wiring-caps")
	quoted := make([]string, 0, len(caps))
	for _, c := range caps {
		quoted = append(quoted, fmt.Sprintf("%q", c))
	}
	source := fmt.Sprintf(`import { definePlugin } from "larkax";
export const meta = { key: %q, version: "1.0.0", apiVersion: 1, capabilities: [%s] };
export default definePlugin({ onUsage() { return { promptTokens: 1 }; } });
`, key, strings.Join(quoted, ", "))
	return insertPluginRow(t, db, key, source)
}

// enablePlugins 把插件开关打开（用完还原），返回一份加载好的运行期配置。
//
// 开关改在库里而不是传参：buildPlugins 读的就是 settings 表（见
// plugin.LoadConfig），绕开它去构造一个假的 Values 就等于把被测的那一步
// ——「配置真的被读了」——从用例里拿掉。
func enablePlugins(t *testing.T, db *sql.DB) *settings.Values {
	t.Helper()
	ctx := context.Background()

	var before string
	if err := db.QueryRowContext(ctx,
		`SELECT value FROM settings WHERE key = 'plugin.enabled'`).Scan(&before); err != nil {
		t.Fatalf("读 settings.plugin.enabled 失败: %v", err)
	}
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.WithoutCancel(ctx),
			`UPDATE settings SET value = $1 WHERE key = 'plugin.enabled'`, before); err != nil {
			t.Errorf("还原 settings.plugin.enabled 失败: %v", err)
		}
	})
	if _, err := db.ExecContext(ctx,
		`UPDATE settings SET value = 'true' WHERE key = 'plugin.enabled'`); err != nil {
		t.Fatalf("打开插件开关失败: %v", err)
	}

	values, err := settings.Load(ctx, settings.NewPgSource(db))
	if err != nil {
		t.Fatalf("加载运行期配置失败: %v", err)
	}
	return values
}
