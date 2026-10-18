package assembly

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/channel"
)

// usage_events 的当地口径两列：一次真请求走完整条链之后，库里那两格写的是
// **这次计价用的**那一档依据（地区时区的钟点与那一天算不算休息日）。
//
// # 为什么要跑到装配这一层
//
// 判定那一半在 expr 的用例里钉（什么算「用到当地口径」、值怎么解析出来），
// 落库那一半在 ledger/store 的用例里钉（写值还是写 NULL）。夹在中间那一段
// ——求值的结果经 pipeline 走到 Charge、再被落库层读出来——只有从 Build 装出来
// 的对象出发、真发一条请求才看得见接没接上。日历那一根线已经有过同样的理由
// （见 calendar_test.go 顶部：只有测试在调的符号，在覆盖率上与真在用的符号
// 一模一样）。
//
// # 跨档对拍
//
// 同一条规则、同一个模型，在三个受理时刻各发一次：工作日的高峰、工作日的
// 低谷、休息日。三行落出来的 local_hour / is_off_day 与那一档的单价都要各自
// 对得上——只断言「有值」的话，一个永远写同一个钟点的实现也能过。
//
// 时刻冻结在 `newAppAt` 注入的时钟上（**受理时刻**），所以这不是「今天恰好
// 是工作日」那种碰运气的用例；日历数据由用例自己灌进 calendar_days
// （见 seedCalendarDay），也不随真实的节假日安排变。
//
// 没有 LARKAX_DB_DSN 时跳过，说明见 e2e_test.go 顶部。

// localBasis 是 usage_events 上这一组用例要看的那几格。
type localBasis struct {
	localHour  sql.NullInt64
	isOffDay   sql.NullBool
	promptUnit sql.NullString
	cost       string
	pricedAt   time.Time
}

// newestUsageEvent 取这个用户最近一次结算落下的用量事件。
//
// 按 billing_requests.id 倒序取，不按 usage_events.created_at：后者是落库那一刻
// 的 now()，同一秒里的两行谁新谁旧分不出来（见 insertUsageEventSQL 里
// 「created_at 为什么不在这里写」）。billing_requests.id 是序列，次序确定。
func newestUsageEvent(ctx context.Context, t *testing.T, db *sql.DB, userID int64) localBasis {
	t.Helper()
	var got localBasis
	if err := db.QueryRowContext(ctx, `
		SELECT ue.local_hour, ue.is_off_day, ue.unit_prices->>'prompt', ue.cost::text,
		       ue.priced_at
		  FROM usage_events ue
		  JOIN billing_requests br ON br.request_id = ue.request_id
		 WHERE br.user_id = $1
		 ORDER BY br.id DESC
		 LIMIT 1`, userID,
	).Scan(&got.localHour, &got.isOffDay, &got.promptUnit, &got.cost, &got.pricedAt); err != nil {
		t.Fatalf("读用量事件失败: %v", err)
	}
	return got
}

// 三档对拍：工作日高峰、工作日低谷、休息日。
func TestUsageEventRecordsTheLocalBasisThatPricedTheRequest(t *testing.T) {
	db := testDB(t)
	sealer := testSealer(t)
	ctx := context.Background()

	const balance = "100"
	model := unique("e2e-model-localbasis")

	merchant := seedUser(t, db, true, "0")
	user := seedUser(t, db, false, balance)
	key := issueKey(t, db, user)

	fake := &fakeUpstream{protocol: channel.ProtocolOpenAI, db: db, userID: user, model: model}
	upstreamSrv := httptest.NewServer(fake.handler())
	t.Cleanup(upstreamSrv.Close)

	seedChannel(t, db, sealer, merchant, upstreamSrv.URL, model)

	// 两档价差一个数量级，于是「那一行是按哪一档算的」从单价上看得出来——
	// 库里没有档位名那一列，档位落在单价与金额上。输出侧那一格也要写：
	// 上游报了输出 token，规则没给这个价的话结算直接失败（不是静默按零算）。
	const peakPrice, offPeakPrice = "0.00002", "0.00001"
	seedRule(t, db, model, `isWorkday() && isPeak("08:00-22:00")
		? tier("peak", {"prompt": `+peakPrice+`, "completion": `+peakPrice+`})
		: tier("off_peak", {"prompt": `+offPeakPrice+`, "completion": `+offPeakPrice+`})`)

	// 2031-02-04 是**周二**、2031-02-08 是**周六**，都按当地（Asia/Shanghai，
	// 出厂地区是 CN）的那一天算。挑非节假日的工作日与普通周末，是因为
	// 「哪一天算工作、哪一天算休息」要由日历表说了算：下面把这两天灌进去。
	workday := time.Date(2031, 2, 4, 0, 0, 0, 0, time.UTC)
	saturday := time.Date(2031, 2, 8, 0, 0, 0, 0, time.UTC)
	seedCalendarDay(t, db, workday, "", false)
	seedCalendarDay(t, db, saturday, "", true)

	// 时钟由用例推动：每一次请求前把它拨到这次请求的受理时刻。
	// 初值取哪一刻都行（第一发之前就会拨过去），取第一行那一档是为了读起来
	// 不出现一个与用例无关的时刻。
	accepted := shanghaiAt(t, 2031, 2, 4, 10, 0)
	app := newAppAt(t, db, sealer, func() time.Time { return accepted })
	public := httptest.NewServer(app.Public)
	t.Cleanup(public.Close)

	for _, tc := range []struct {
		name string
		// at 是这次请求的受理时刻。
		at time.Time
		// 下面三格是规则**当时**判出来的那一档，以及据此应当落库的值。
		hour     int
		offDay   bool
		price    string
		tierName string
	}{
		{
			// 10:00（当地）在 08:00–22:00 里，周二又是工作日 → 高峰档。
			name: "工作日高峰", at: shanghaiAt(t, 2031, 2, 4, 10, 0),
			hour: 10, offDay: false, price: peakPrice, tierName: "peak",
		},
		{
			// 同一个工作日的 23:00：出了窗口 → 低谷档。当地日的属性不变。
			name: "工作日低谷", at: shanghaiAt(t, 2031, 2, 4, 23, 0),
			hour: 23, offDay: false, price: offPeakPrice, tierName: "off_peak",
		},
		{
			// 周六的 10:00：钟点与第一行相同，而这一天 `!isWorkday()` → 低谷档。
			// 「同一钟点、不同档」这一格正说明 is_off_day 是判档依据的一部分，
			// 而不只是钟点的附属。
			name: "休息日高峰时段", at: shanghaiAt(t, 2031, 2, 8, 10, 0),
			hour: 10, offDay: true, price: offPeakPrice, tierName: "off_peak",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accepted = tc.at

			status, body := postChat(ctx, t, public.URL+"/v1/chat/completions", key, chatBody(model))
			if status != http.StatusOK {
				t.Fatalf("状态码 = %d，期望 200。响应体：%s", status, body)
			}

			got := newestUsageEvent(ctx, t, db, user)

			// 先看这一行是按哪一档算的：下面那两个值必须与它说的是同一件事。
			if !got.promptUnit.Valid || !sameDecimal(got.promptUnit.String, tc.price) {
				t.Fatalf("prompt 单价 = %v，期望 %s（%s 那一档）——"+
					"这次请求没有按预期的那一档计价，下面两条就无从谈起",
					got.promptUnit, tc.price, tc.tierName)
			}
			if !got.pricedAt.Equal(tc.at) {
				t.Errorf("priced_at = %v，期望受理时刻 %v（档位按受理那一刻选）",
					got.pricedAt, tc.at)
			}

			if !got.localHour.Valid || int(got.localHour.Int64) != tc.hour {
				t.Errorf("local_hour = %v，期望 %d——"+
					"规则判那一档时用的当地钟点是这个数", got.localHour, tc.hour)
			}
			if !got.isOffDay.Valid || got.isOffDay.Bool != tc.offDay {
				t.Errorf("is_off_day = %v，期望 %t——它与规则里 isWorkday() 当时"+
					"判的是同一件事", got.isOffDay, tc.offDay)
			}
		})
	}

	// 三行都读一遍：上面每一条只管自己那一行，这一条钉的是「三行之间有差别」
	// ——一个把两列写成同一个常数的实现，三行各自看都可能「对」，而这里看得
	// 出来。按 request_id 去重后按顺序取，次序由 billing_requests.id 定。
	rows, err := db.QueryContext(ctx, `
		SELECT ue.local_hour, ue.is_off_day
		  FROM usage_events ue
		  JOIN billing_requests br ON br.request_id = ue.request_id
		 WHERE br.user_id = $1
		 ORDER BY br.id`, user)
	if err != nil {
		t.Fatalf("读用量事件失败: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var seen []localBasis
	for rows.Next() {
		var r localBasis
		if err := rows.Scan(&r.localHour, &r.isOffDay); err != nil {
			t.Fatalf("扫描用量事件失败: %v", err)
		}
		seen = append(seen, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("读用量事件失败: %v", err)
	}
	if len(seen) != 3 {
		t.Fatalf("落了 %d 行用量事件，期望 3 行", len(seen))
	}
	if seen[0].localHour.Int64 == seen[1].localHour.Int64 {
		t.Errorf("工作日高峰与低谷两行的 local_hour 都是 %d——"+
			"两格的钟点本来就不同，写成同一个数说明它没跟着求值走",
			seen[0].localHour.Int64)
	}
	if seen[0].isOffDay.Bool == seen[2].isOffDay.Bool {
		t.Errorf("工作日与休息日两行的 is_off_day 都是 %t——"+
			"那一格正是这一档的判据", seen[0].isOffDay.Bool)
	}
}

// shanghaiAt 按当地（Asia/Shanghai）时刻造一个含时区的时刻。
//
// 用例里写的是「北京的上午十点」这种当地时刻，而受理时刻是 UTC——折错这一步
// 会让每一格都错八小时，从断言里看不出来是哪一边错的，所以集中在这里。
func shanghaiAt(t *testing.T, y int, mo time.Month, d, h, mi int) time.Time {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("加载时区失败: %v", err)
	}
	return time.Date(y, mo, d, h, mi, 0, 0, loc)
}
