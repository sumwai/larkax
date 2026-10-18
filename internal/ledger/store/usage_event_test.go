package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/billing"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/pipeline"
	"github.com/sumwai/larkax/internal/tokens"
)

// usageEventRow 是 usage_events 里的一行，按落库层写进去的那几列取出来。
//
// 可空的列一律用 sql.Null*：这一组用例要断言的正是「哪几列是 NULL」，
// 而把 NULL 扫进零值会让「没写这一列」与「写了个零」在测试里长得一样。
type usageEventRow struct {
	UserID           int64
	ChannelID        sql.NullInt64
	CanonicalModel   string
	PromptTokens     int64
	CompletionTokens int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64
	UnitCounts       []byte
	UnitPrices       []byte
	BillingPath      string
	Estimator        sql.NullString
	Cost             string
	UpstreamCost     sql.NullString
	Asset            string
	PricedAt         time.Time
	LocalHour        sql.NullInt64
	IsOffDay         sql.NullBool
	DurationMS       sql.NullInt64
	CreatedAt        time.Time
}

// usageEventColumns 与 usageEventRow 的字段顺序一一对应。
//
// 写成常量是为了让 SELECT 与 Scan 两处的列名不会各写一遍——两边不一致时
// 报的是「扫描列数不匹配」，看不出是哪一列写错了。
const usageEventColumns = `user_id, channel_id, canonical_model,
	prompt_tokens, completion_tokens, cache_read_tokens, cache_write_tokens,
	reasoning_tokens, unit_counts, unit_prices, billing_path, estimator,
	cost, upstream_cost, asset, priced_at, local_hour, is_off_day,
	duration_ms, created_at`

// usageEventOf 读一条用量事件。没有则让用例失败——这一组用例里「读不到」
// 从来不表示「没写」，而表示写没写对本身就是断言的内容。
func usageEventOf(ctx context.Context, t *testing.T, db *sql.DB, requestID string) usageEventRow {
	t.Helper()
	var r usageEventRow
	err := db.QueryRowContext(ctx, `
		SELECT `+usageEventColumns+`
		  FROM usage_events WHERE request_id = $1`, requestID).
		Scan(&r.UserID, &r.ChannelID, &r.CanonicalModel,
			&r.PromptTokens, &r.CompletionTokens, &r.CacheReadTokens, &r.CacheWriteTokens,
			&r.ReasoningTokens, &r.UnitCounts, &r.UnitPrices, &r.BillingPath, &r.Estimator,
			&r.Cost, &r.UpstreamCost, &r.Asset, &r.PricedAt, &r.LocalHour, &r.IsOffDay,
			&r.DurationMS, &r.CreatedAt)
	if err != nil {
		t.Fatalf("读用量事件失败: %v", err)
	}
	return r
}

// countUsageEvents 数一次请求落了几行用量事件。
func countUsageEvents(ctx context.Context, t *testing.T, db *sql.DB, requestID string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM usage_events WHERE request_id = $1`, requestID).Scan(&n); err != nil {
		t.Fatalf("数用量事件失败: %v", err)
	}
	return n
}

// billedCharge 造一次「用量已知、来源已知、受理时刻已知」的结算。
//
// 三个值都必须由用例显式给出，而不是靠零值：零值落库时分别是「全是 0 的用量」
// 「来源 0（实测）」与「公元 1 年」，三者看起来都是**合法**的数据，
// 于是「漏设」与「真的是这样」在库里分不开。
//
// billID 原样传给分录，理由见 consumeEntries。
func billedCharge(t *testing.T, userID int64, requestID string, billID int64, amount string, source tokens.Source, at time.Time) *pipeline.Charge {
	t.Helper()
	charge := &pipeline.Charge{
		RequestID: requestID, UserID: userID, Asset: "CNY",
		Entries:     consumeEntries(t, userID, amount, billID),
		PricedAt:    at,
		UsageSource: source,
		Usage: billing.Usage{
			PromptTokens:     1000,
			CompletionTokens: 250,
			CacheReadTokens:  400,
			CacheWriteTokens: 30,
			ReasoningTokens:  20,
			// 非 token 计量：搜索次数与图片张数。它落 jsonb，与 token 那几列
			// 走的是两条路（各家口径不同，由适配器归一化）。
			UnitCounts: map[string]int64{"searches": 2, "images": 1},
		},
	}
	charge.Settlement.Actual = money.MustParse(amount)
	return charge
}

// ── 用量事件 ────────────────────────────────────────────────────────────

// 一次结算之后 usage_events 里要有对应的一行，且字段值与 Charge 一致。
//
// # 为什么逐字段比
//
// 这张表的全部价值是「事后拿它对账」：少一列、串一列，对出来的差额都没有来历
// （而账目仍然是平的，看不出异常）。所以这里不是「有一行就算过」——每个字段都
// 拿 Charge 上的值比一遍，**包括哪几列必须是 NULL**：只断言「有值」的话，
// 把 NULL 写成零值也能通过。
func TestSaveWritesUsageEvent(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	channelID := seedChannel(t, ctx, db)
	requestID := reqID(t)

	// 预留：渠道未知，与生产同一条路（预留发生在选路之前）。
	req := testRequest(ctx, t, db, userID, requestID)
	billID, reserveErr := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "30", "CNY"))
	if reserveErr != nil {
		t.Fatal(reserveErr)
	}

	// 结算：渠道已知，用量与受理时刻都由计费层给出。
	settleReq := req
	settleReq.ChannelID = channelID

	acceptAt := time.Date(2026, 10, 17, 9, 30, 0, 0, time.UTC)
	// 本地词表数出来的用量——它不能记成 measured，见 billingPathOf。
	charge := billedCharge(t, userID, requestID, billID, "12", tokens.SourceTokenized, acceptAt)

	if err := s.Save(ctx, settleReq, charge); err != nil {
		t.Fatalf("结算失败: %v", err)
	}

	got := usageEventOf(ctx, t, db, requestID)

	if got.UserID != userID {
		t.Errorf("user_id = %d，期望 %d", got.UserID, userID)
	}
	if !got.ChannelID.Valid || got.ChannelID.Int64 != channelID {
		t.Errorf("channel_id = %v，期望 %d（这次请求实际走的渠道）", got.ChannelID, channelID)
	}
	if got.CanonicalModel != settleReq.CanonicalModel {
		t.Errorf("canonical_model = %q，期望 %q", got.CanonicalModel, settleReq.CanonicalModel)
	}

	// 五个 token 数逐个比：它们各自乘各自的单价，串一列就是一笔对不出来的差额。
	for _, tc := range []struct {
		column string
		got    int64
		want   int64
	}{
		{"prompt_tokens", got.PromptTokens, charge.Usage.PromptTokens},
		{"completion_tokens", got.CompletionTokens, charge.Usage.CompletionTokens},
		{"cache_read_tokens", got.CacheReadTokens, charge.Usage.CacheReadTokens},
		{"cache_write_tokens", got.CacheWriteTokens, charge.Usage.CacheWriteTokens},
		{"reasoning_tokens", got.ReasoningTokens, charge.Usage.ReasoningTokens},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d，期望 %d", tc.column, tc.got, tc.want)
		}
	}

	// unit_counts 比解析后的 map，不比字面量：jsonb 会按自己的规则重排键，
	// 比字面量等于在比 jsonb 的键序。
	var counts map[string]int64
	if err := json.Unmarshal(got.UnitCounts, &counts); err != nil {
		t.Fatalf("unit_counts 不是 JSON 对象（%s）: %v", got.UnitCounts, err)
	}
	if !reflect.DeepEqual(counts, charge.Usage.UnitCounts) {
		t.Errorf("unit_counts = %v，期望 %v", counts, charge.Usage.UnitCounts)
	}

	// 本地词表数出来的用量落 estimated，不是 measured：billing_path 说的是
	// 「能不能拿去跟上游对账」，而本地值不能（见 billingPathOf）。
	if got.BillingPath != "estimated" {
		t.Errorf("billing_path = %q，期望 %q", got.BillingPath, "estimated")
	}
	if !got.Estimator.Valid || got.Estimator.String != "tokenized" {
		t.Errorf("estimator = %v，期望 %q", got.Estimator, "tokenized")
	}

	// 收的钱取 Settlement.Actual（按实际用量算出的金额），不是预留的 30。
	if got.Cost != "12.00000000" {
		t.Errorf("cost = %s，期望 12（按实际用量算出的金额，不是预留的 30）", got.Cost)
	}
	if got.Asset != "CNY" {
		t.Errorf("asset = %q，期望 CNY", got.Asset)
	}
	if !got.PricedAt.Equal(acceptAt) {
		t.Errorf("priced_at = %s，期望 %s（受理时刻）", got.PricedAt, acceptAt)
	}

	// 下面这几列在这个夹具上应当是 NULL，理由各自写在 insertUsageEvent 上：
	// 这个夹具的 Charge 没带当地口径（LocalTime 为 nil），上游成本的口径也还
	// 没定（issue #163 的第 2 条）。断言 NULL 是为了让「拿零值顶上」这种改法
	// 必须显式改这一处——那种值在库里长得像「求值算出来就是当地 0 点、
	// 不是休息日」，而它照样算得出数来。当地口径**有值**的那一支由
	// TestSaveWritesLocalBasis 钉住。
	for _, tc := range []struct {
		column string
		isNull bool
	}{
		{"local_hour", !got.LocalHour.Valid},
		{"is_off_day", !got.IsOffDay.Valid},
		{"upstream_cost", !got.UpstreamCost.Valid},
		// duration_ms 是**有条件的 NULL**：Charge.Duration 为零时写 NULL。
		// 这个夹具（billedCharge）没有给耗时，所以这里期望 NULL；有值的
		// 那一支由 TestSaveWritesUsageEventDuration 钉住。
		{"duration_ms", !got.DurationMS.Valid},
	} {
		if !tc.isNull {
			t.Errorf("%s 有值，期望 NULL——这个夹具没有给它值", tc.column)
		}
	}

	// unit_prices 不在「NULL」那一档里，因为它是 NOT NULL DEFAULT '{}'：这个
	// 夹具（billedCharge）没有给单价，落库层因此写空对象。空对象现在的含义是
	// **这次结算没有单价可记**，不是「这一列没人填」——有单价的那一支由
	// usage_event_prices_test.go 的一组用例逐键钉住。
	if string(got.UnitPrices) != "{}" {
		t.Errorf("unit_prices = %s，期望 {}——这个夹具没有给单价", got.UnitPrices)
	}
}

// 耗时按毫秒写进 duration_ms，零值写 NULL。
//
// # 为什么两种都要钉
//
// 这一列是渠道健康度**延迟**那一维唯一的样本来源：写入方按桶对它取分位数
// （见 internal/channels 的 rollupSignalsSQL）。所以「有值」那一支错了会让
// 分位数算错，而「零值写 NULL」那一支错了会让没测到的调用落成 0 毫秒
// 样本——它把分位数往下拉，读出来的渠道看起来「很快」，没有任何东西报出来。
//
// 不足 1 毫秒的耗时向上取整：向下取整会变成 0，于是那次调用被当成「没测到」
// 丢掉，等于把最快的那批样本从分位数里删掉。
//
// 等价变异（gremlins CONDITIONALS_BOUNDARY，durationMS 开头 `d <= 0` 的 `<=`
// 改成 `<`）：d 恰好为 0 时两支返回的都是 0——`0 / Millisecond` 是 0，
// `0 % Millisecond` 也是 0，于是取整那一支不会加 1，行为逐点相同。
func TestSaveWritesUsageEventDuration(t *testing.T) {
	cases := []struct {
		name string
		in   time.Duration
		want sql.NullInt64
	}{
		{"整毫秒", 1500 * time.Millisecond, sql.NullInt64{Int64: 1500, Valid: true}},
		{"不足 1 毫秒向上取整", 300 * time.Microsecond, sql.NullInt64{Int64: 1, Valid: true}},
		{"零值写 NULL", 0, sql.NullInt64{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := testDB(t)
			s := New(db)
			ctx := context.Background()

			userID := seed(t, db, "100")
			channelID := seedChannel(t, ctx, db)
			requestID := reqID(t)

			req := testRequest(ctx, t, db, userID, requestID)
			billID, reserveErr := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "30", "CNY"))
			if reserveErr != nil {
				t.Fatal(reserveErr)
			}

			settleReq := req
			settleReq.ChannelID = channelID

			charge := billedCharge(t, userID, requestID, billID, "12",
				tokens.SourceMeasured, time.Date(2026, 10, 17, 9, 30, 0, 0, time.UTC))
			charge.Duration = tc.in

			if err := s.Save(ctx, settleReq, charge); err != nil {
				t.Fatalf("结算失败: %v", err)
			}

			got := usageEventOf(ctx, t, db, requestID).DurationMS
			if got != tc.want {
				t.Errorf("duration_ms = %+v，期望 %+v（输入 %s）", got, tc.want, tc.in)
			}
		})
	}
}

// created_at 是**落库时刻**，不是受理时刻。
//
// 它是分区键（表按月分区），而这张表有兜底分区 usage_events_default，
// 它的注释写着「非空即告警，说明分区创建任务漏跑了」。一次迟到的写入
// （上游跑得久、跨过了月末）如果带着受理时刻，那一行就会落进兜底分区，
// 于是告警指向一次正常的写入。
//
// 用一个明显陈旧的受理时刻来钉：如果哪天有人把 created_at 改成 charge.PricedAt，
// 这一行会落到兜底分区，而 created_at 会变成 2020 年——两种表现都能被这里抓住。
func TestUsageEventCreatedAtIsWriteTime(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := testRequest(ctx, t, db, userID, requestID)
	billID, reserveErr := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "30", "CNY"))
	if reserveErr != nil {
		t.Fatal(reserveErr)
	}

	acceptAt := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	charge := billedCharge(t, userID, requestID, billID, "12", tokens.SourceMeasured, acceptAt)

	start := time.Now()
	if err := s.Save(ctx, req, charge); err != nil {
		t.Fatalf("结算失败: %v", err)
	}

	got := usageEventOf(ctx, t, db, requestID)
	if !got.PricedAt.Equal(acceptAt) {
		t.Errorf("priced_at = %s，期望受理时刻 %s", got.PricedAt, acceptAt)
	}
	// 留一分钟余量：库与进程的钟可能有微小偏差，而这条判据要抓的是
	// 「把受理时刻写成了 created_at」那种量级的错（这里是 2020 年）。
	if got.CreatedAt.Before(start.Add(-time.Minute)) {
		t.Errorf("created_at = %s，早于这次落库（%s）——它是分区键，取的是落库时刻而不是受理时刻",
			got.CreatedAt, start)
	}
}

// 重放不写第二行。
//
// 重放由 billing_requests 的唯一键挡下（见 TestSettlementIsIdempotent），
// 而用量事件写在那一关**之后**——写在之前的话，一次重投就会多出一行用量，
// 对账时表现为「用量比账单多」，而钱只扣了一次。
func TestUsageEventIsNotWrittenOnReplay(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := testRequest(ctx, t, db, userID, requestID)
	billID, reserveErr := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "30", "CNY"))
	if reserveErr != nil {
		t.Fatal(reserveErr)
	}

	charge := billedCharge(t, userID, requestID, billID, "12", tokens.SourceMeasured, pricedAt())
	if err := s.Save(ctx, req, charge); err != nil {
		t.Fatalf("结算失败: %v", err)
	}
	if err := s.Save(ctx, req, charge); !errors.Is(err, ErrAlreadySettled) {
		t.Fatalf("重放应当返回 ErrAlreadySettled，得到 %v", err)
	}

	if n := countUsageEvents(ctx, t, db, requestID); n != 1 {
		t.Errorf("重放之后有 %d 行用量事件，期望 1 行", n)
	}
}

// 用量事件写不进去时，同一次结算里先写下的东西一条都不该落。
//
// # 失败是注入的，而且必须是**真的**数据库错误
//
// 注入点在 priced_at：它的取值超出 timestamptz 的范围，PostgreSQL 拒绝这一行。
// 选这一列是因为**只有这条 INSERT 会写它**——拿金额去撑爆 numeric(20,8) 是
// 不行的：markSettled 与 ledger_entries 写的也是 numeric(20,8)，那样失败的是
// 更早那一步，这条用例就变成在证明别的东西，而它照样会「通过」。
//
// 断言里连着错误文本一起钉，理由同上：失败点若被挪到前面，这条用例要**报出来**，
// 而不是继续以「分录没落」的名义通过。
//
// # 四条断言各自的牙
//
// 失败点落在**哪一步**，决定了这四条不是同一种断言。逐条写清楚，免得下一个人
// 以为它们等价（把 Save 改成「用量事件失败就提交」时，只有两条会挂）：
//
//   - **分录：有牙。** 分录写在失败点**之前**，所以它确实进过这次事务——
//     留在库里就说明回滚没发生。前提是那组分录带着 billing_request_id：
//     夹具不带时它是 NULL，按它查恒为空（见 consumeEntries）。
//   - **用量事件：今天的注入点下恒真。** 失败的那条语句本身没写成任何东西，
//     所以「一行都没有」在回滚与不回滚两种情形下都成立。它钉的是「这次请求
//     在 usage_events 里没有半行」这个可读的结论；哪天失败点挪到它之后
//     （例如挪到 COMMIT），它才分辨得出。
//   - **钱包：同样恒真。** applyToWallets 与 releaseFrozen 排在那条 INSERT
//     **之后**，这条路上根本不会执行。它守的是**顺序**：谁把用量事件挪到
//     那两步之后，余额会变成 88、冻结会变成 0，这条立刻报出来。钱包那两步的
//     回滚由 unfreeze_underflow_test.go 的 TestSettleRefusesFrozenUnderflow
//     覆盖——那一条的失败点排在 applyToWallets 之后，真的分辨得出。
//   - **账单状态：有牙。** markSettled 也写在失败点之前，事务提交了它就会
//     是 settled。
func TestUsageEventFailureRollsBackSettlement(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := testRequest(ctx, t, db, userID, requestID)

	billID, reserveErr := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "30", "CNY"))
	if reserveErr != nil {
		t.Fatal(reserveErr)
	}

	charge := billedCharge(t, userID, requestID, billID, "12", tokens.SourceMeasured,
		time.Date(-5000, 1, 1, 0, 0, 0, 0, time.UTC))

	saveErr := s.Save(ctx, req, charge)
	if saveErr == nil {
		t.Fatal("priced_at 超出 timestamptz 的范围，这次结算应当失败")
	}
	if !strings.Contains(saveErr.Error(), "写用量事件失败") {
		t.Fatalf("失败的不是用量事件那一步，这条用例证明的东西就不是它要证明的: %v", saveErr)
	}

	// 分录：一条都不该落。分录在事务里写在用量事件**之前**，所以它确实
	// 进过这次事务——留在库里说明回滚没发生。
	//
	// 这个查询有牙的前提是那组分录带着 billing_request_id（夹具传了 billID）。
	// 不带时那一列是 NULL，按它查**恒为空**，回滚有没有发生都看不出区别。
	var entries int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM ledger_entries WHERE billing_request_id = $1`, billID).
		Scan(&entries); err != nil {
		t.Fatalf("数分录失败: %v", err)
	}
	if entries != 0 {
		t.Errorf("结算失败后落了 %d 条分录，期望 0 条", entries)
	}

	// 用量事件本身也不该有。这一条在今天的注入点下恒真（失败的那条 INSERT
	// 什么都没写成），见上面「四条断言各自的牙」。
	if n := countUsageEvents(ctx, t, db, requestID); n != 0 {
		t.Errorf("结算失败后落了 %d 行用量事件，期望 0 行", n)
	}

	// 钱包：一分钱没动，冻结额也照旧。这两条**今天证明不了回滚**——
	// applyToWallets 与 releaseFrozen 排在那条失败的 INSERT 之后，这条路上
	// 根本没执行，所以事务提交与否它们都不会变。它们守的是顺序：谁把用量
	// 事件挪到那两步之后，余额会变成 88、冻结会变成 0。
	balance, frozen := walletOf(t, db, userID)
	if balance != "100.00000000" {
		t.Errorf("结算失败后余额 = %s，期望 100（那次事务整个回滚了）", balance)
	}
	if frozen != "30.00000000" {
		t.Errorf("结算失败后冻结 = %s，期望 30（预留还在，没有被这次失败的结算解冻）", frozen)
	}

	// 账单：还是 pending，所以这次请求之后仍可以被正常结算或释放。
	var status string
	if err := db.QueryRowContext(ctx,
		`SELECT status FROM billing_requests WHERE id = $1`, billID).Scan(&status); err != nil {
		t.Fatalf("读账单状态失败: %v", err)
	}
	if status != "pending" {
		t.Errorf("结算失败后账单状态 = %q，期望 pending", status)
	}
}

// 漏设受理时刻要**报错**，而不是往那一列写公元 1 年。
//
// 公元 1 年落库不报错，事后也看不出那一行是「当时就是按这一刻算的价」还是
// 「漏设了」——而这一列正是复算唯一的依据。分辨只有写进去的那一刻还做得到。
func TestSaveRejectsMissingPricedAt(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)
	req := testRequest(ctx, t, db, userID, requestID)
	billID, reserveErr := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "30", "CNY"))
	if reserveErr != nil {
		t.Fatal(reserveErr)
	}

	charge := billedCharge(t, userID, requestID, billID, "12", tokens.SourceMeasured, pricedAt())
	charge.PricedAt = time.Time{}

	if err := s.Save(ctx, req, charge); !errors.Is(err, ErrMissingPricedAt) {
		t.Fatalf("漏设 PricedAt 应当返回 ErrMissingPricedAt，得到 %v", err)
	}

	// 一个副作用都不该留下：它在开事务之前就被挡下了。
	if n := countUsageEvents(ctx, t, db, requestID); n != 0 {
		t.Errorf("被挡下的结算写了 %d 行用量事件，期望 0 行", n)
	}
	if balance, frozen := walletOf(t, db, userID); balance != "100.00000000" || frozen != "30.00000000" {
		t.Errorf("被挡下的结算动了钱包：余额 %s、冻结 %s，期望 100 与 30", balance, frozen)
	}
}

// 上游没报 usage 的那一行，落库之后要能与「上游报了 0」分开。
//
// 两者落库的 token 列与 cost 逐列相同（都是 0），区别只在
// billing_path 与 estimator：没报 → estimated + unreported。
// 「报了 0」那一侧落 measured + estimator NULL，由 TestBillingPathOf
// 里 SourceMeasured 那一行负责。这里钉住整条写库路径，而不只是翻译函数。
func TestSaveUsageEventUnreportedUsage(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	channelID := seedChannel(t, ctx, db)
	requestID := reqID(t)

	req := testRequest(ctx, t, db, userID, requestID)
	if _, err := s.SaveReservation(ctx, req, reservation(t, userID, requestID, "30", "CNY")); err != nil {
		t.Fatal(err)
	}
	settleReq := req
	settleReq.ChannelID = channelID

	acceptAt := time.Date(2026, 10, 17, 9, 30, 0, 0, time.UTC)
	// 上游整块没报 usage：用量是零值占位，金额为零。
	charge := &pipeline.Charge{
		RequestID:   requestID,
		UserID:      userID,
		Asset:       "CNY",
		PricedAt:    acceptAt,
		UsageSource: tokens.SourceUnreported,
	}
	charge.Settlement.Actual = money.MustParse("0")

	if err := s.Save(ctx, settleReq, charge); err != nil {
		t.Fatalf("结算失败: %v", err)
	}

	got := usageEventOf(ctx, t, db, requestID)
	if got.BillingPath != "estimated" {
		t.Errorf("billing_path = %q，期望 %q（占位的零不能拿去对账）", got.BillingPath, "estimated")
	}
	if !got.Estimator.Valid || got.Estimator.String != "unreported" {
		t.Errorf("estimator = %v，期望 %q", got.Estimator, "unreported")
	}
	// 与「上游报了 0」逐列相同的部分：这正是旧行为分不开它们的原因。
	if got.PromptTokens != 0 || got.CompletionTokens != 0 {
		t.Errorf("用量 = %d/%d，期望全零", got.PromptTokens, got.CompletionTokens)
	}
	if got.Cost != "0.00000000" {
		t.Errorf("cost = %s，期望 0", got.Cost)
	}
}

// ── 用量来源到两列的翻译 ────────────────────────────────────────────────

// 三个来源各自落到哪一档，逐个钉住。
//
// # 为什么 measured 与 estimated 之外还要看 estimator
//
// billing_path 只有两档（表上有 CHECK），回答的是「能不能拿去跟上游对账」；
// 「它是怎么来的」由 estimator 说。tokenized 与 estimated 都落 estimated 那一档，
// 但一个能说明是本地词表数的、一个是启发式估的——两列缺一，事后就分不出
// 「这次是准的」还是「这次差得多」。
func TestBillingPathOf(t *testing.T) {
	for _, tc := range []struct {
		src       tokens.Source
		path      string
		estimator string
	}{
		// 上游实测：没有估算器，留空而不是填一个名字——它本来就不是估出来的。
		{tokens.SourceMeasured, "measured", ""},
		// 本地词表：准，但**不能**记成 measured（见 billingPathOf）。
		{tokens.SourceTokenized, "estimated", "tokenized"},
		{tokens.SourceEstimated, "estimated", "char_heuristic"},
		// 上游没报：同样不能对账（所以是 estimated），由 estimator
		// 说明它连估算都不是——零值是占位。
		{tokens.SourceUnreported, "estimated", "unreported"},
	} {
		path, estimator, err := billingPathOf(tc.src)
		if err != nil {
			t.Errorf("billingPathOf(%v) 报错: %v", tc.src, err)
			continue
		}
		if path != tc.path {
			t.Errorf("billingPathOf(%v) 的 path = %q，期望 %q", tc.src, path, tc.path)
		}
		if estimator != tc.estimator {
			t.Errorf("billingPathOf(%v) 的 estimator = %q，期望 %q", tc.src, estimator, tc.estimator)
		}
	}

	// 未知来源要报错，而不是落一个表上 CHECK 挡得下的值：那一列只有两档，
	// 表会拦下它，但拦下来的是一句「violates check constraint」。
	if _, _, err := billingPathOf(tokens.Source(9)); !errors.Is(err, ErrUnknownUsageSource) {
		t.Errorf("未知来源应当返回 ErrUnknownUsageSource，得到 %v", err)
	}
}

// 空与 nil 的非 token 用量都落 {}，不是 JSON 的 null。
//
// 那一列是 NOT NULL DEFAULT '{}'，而 JSON 的 null 不是 SQL 的 NULL：
// 写进去之后 `unit_counts->>'searches'` 得到 NULL，与「这次没有搜索这一项」
// 分不开——两者在对账里是两件事。
func TestUnitCountsJSONEmptyIsObject(t *testing.T) {
	for _, counts := range []map[string]int64{nil, {}} {
		got, err := unitCountsJSON(counts)
		if err != nil {
			t.Fatalf("序列化 %v 失败: %v", counts, err)
		}
		if got != "{}" {
			t.Errorf("unitCountsJSON(%v) = %s，期望 {}", counts, got)
		}
	}
}
