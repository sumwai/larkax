package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sumwai/larkax/internal/billing"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/pipeline"
	"github.com/sumwai/larkax/internal/tokens"
)

// 这一组用例钉的是 usage_events.unit_prices：这次结算**按哪些单价算的**。
//
// # 为什么它值得单独一组
//
// 这一列原先不在 INSERT 的列表里，取库上的 DEFAULT '{}'——落库之后看起来像
// 「这次没有单价」，而实际是「没人写」。两种含义在那一列上长得一样，而它是
// NOT NULL，连「没写」这个信号都没有（见 insertUsageEventSQL 的注释）。
//
// 所以下面每一条都断言**具体的键与值**，不是「非空」：只判非空的话，把金额
// 填进去、把五项写成一项、把它写成整条规则快照，都照样通过——而这一列的全部
// 价值是事后拿它对账复算。

// 这一组用例的受理时刻用 helpers_test.go 里的 pricedAt()。
// fiveSlotPrices 是「规则把五项 token 单价都写了一遍」时求值出来的价。
//
// 五项都用不同的值，且都不等于它们各自乘上用量之后的金额——这样「填的是单价
// 还是金额」在断言里分辨得出来（见 TestUsageEventUnitPricesAreSettlementPrices
// 里的对照）。第五项 cache_write 特意给了 8 位小数（0.00000125）：单价的精度
// 必须原样落库，写成浮点数字会在这里丢掉。
//
// 声明的那几个键是**字面量**，不从 billing.TokenSlots 派生：从那份表派生的话，
// 槽位名写错时用例跟着一起错，而这一列与 unit_counts 的键必须同名同源。
func fiveSlotPrices() billing.Prices {
	return billing.Prices{
		Prompt:     money.MustParse("0.000001"),
		Completion: money.MustParse("0.000002"),
		CacheRead:  money.MustParse("0.0000001"),
		CacheWrite: money.MustParse("0.00000125"),
		Reasoning:  money.MustParse("0.000004"),
		Units: map[string]money.Amount{
			"searches": money.MustParse("0.01"),
			"images":   money.MustParse("0.5"),
		},
		Declared: map[string]bool{
			"prompt": true, "completion": true, "cache_read": true,
			"cache_write": true, "reasoning": true,
		},
	}
}

// wantFiveSlotPrices 是上面那一组价在库里应有的样子。
//
// 值写成字符串：它是 money 的定点十进制（保留到小数点后 8 位，见 money.Amount
// 的 String），不是 JSON 数字——每 token 1e-6 这个量级经 float64 走一圈会掉
// 精度，落库的价就不再是当时用的那个价了。
func wantFiveSlotPrices() map[string]string {
	return map[string]string{
		"prompt":      "0.00000100",
		"completion":  "0.00000200",
		"cache_read":  "0.00000010",
		"cache_write": "0.00000125",
		"reasoning":   "0.00000400",
		"searches":    "0.01000000",
		"images":      "0.50000000",
	}
}

// unitPriceCharge 造一次「用量与金额显式给出」的结算，供下面几条用例补单价。
//
// 五档用量都非零：五项单价各自都有对应的用量，于是「哪一项没写进去」在断言里
// 看得出来（五档里有一档是 0 的话，Validate 不要求它声明，用例就分不清「写了」
// 与「本来就该没有」）。
func unitPriceCharge(t *testing.T, userID int64, requestID, amount string) *pipeline.Charge {
	t.Helper()
	charge := &pipeline.Charge{
		RequestID: requestID, UserID: userID, Asset: "CNY",
		PricedAt:    pricedAt(),
		UsageSource: tokens.SourceMeasured,
		Usage: billing.Usage{
			PromptTokens:     1000,
			CompletionTokens: 250,
			CacheReadTokens:  400,
			CacheWriteTokens: 30,
			ReasoningTokens:  20,
			UnitCounts:       map[string]int64{"searches": 2, "images": 1},
		},
	}
	charge.Settlement.Actual = money.MustParse(amount)
	return charge
}

// reserveAndSettle 走一遍「预留 → 结算」，把结果交给调用方判断。
//
// 它**不**替调用方断言成功：有一条用例要的就是写不进去（单价撞名），
// 断言放在用例里，这里只负责把生产过程走对——预留发生在选路之前，
// 结算时渠道已知，与生产同一条路。
func reserveAndSettle(ctx context.Context, t *testing.T, db *sql.DB, s *Store, charge *pipeline.Charge) error {
	t.Helper()
	req := testRequest(ctx, t, db, charge.UserID, charge.RequestID)
	billID, err := s.SaveReservation(ctx, req, reservation(t, charge.UserID, charge.RequestID, "30", "CNY"))
	if err != nil {
		t.Fatalf("落预留失败: %v", err)
	}
	// 分录按**实际收的钱**造（Capped 时那是被截住之后的数）：账本扣的就是它，
	// 让用例里的分录与 cost 不一致会造出一个库不可能出现的世界。
	charge.Entries = consumeEntries(t, charge.UserID, charge.Settlement.Actual.String(), billID)

	settled := req
	settled.ChannelID = seedChannel(t, ctx, db)
	return s.Save(ctx, settled, charge)
}

// settlePricedRequest 与上面相同，但要求结算成功，并读回那一行。
func settlePricedRequest(ctx context.Context, t *testing.T, db *sql.DB, s *Store, charge *pipeline.Charge) usageEventRow {
	t.Helper()
	if err := reserveAndSettle(ctx, t, db, s, charge); err != nil {
		t.Fatalf("结算失败: %v", err)
	}
	return usageEventOf(ctx, t, db, charge.RequestID)
}

// assertUnitPrices 逐键比 unit_prices，并把多余与缺失的键分别指出来。
//
// 不拿字面量比 JSON：jsonb 会按自己的规则重排键（与 unit_counts 那一条同样的
// 理由），比字面量等于在比键序。也不只比「有几个键」——键名错了、值取成了别
// 的量（金额、用量），都只有在逐键比的时候才现形。
func assertUnitPrices(t *testing.T, got []byte, want map[string]string) {
	t.Helper()
	var prices map[string]string
	if err := json.Unmarshal(got, &prices); err != nil {
		t.Fatalf("unit_prices 不是一个「键到十进制单价」的对象（%s）: %v", got, err)
	}
	for name, wantPrice := range want {
		gotPrice, ok := prices[name]
		if !ok {
			t.Errorf("unit_prices 里没有 %q（实际 %v）——这一项这次是收过费的", name, prices)
			continue
		}
		if gotPrice != wantPrice {
			t.Errorf("unit_prices[%q] = %s，期望 %s", name, gotPrice, wantPrice)
		}
	}
	for name := range prices {
		if _, ok := want[name]; !ok {
			t.Errorf("unit_prices 里多了 %q = %s——不是这次结算用到的单价", name, prices[name])
		}
	}
	if len(prices) != len(want) {
		t.Errorf("unit_prices 有 %d 项，期望 %d 项：%s", len(prices), len(want), got)
	}
}

// 一次按五项 token 单价结算的请求：unit_prices 是那五项（加两个非 token
// 计量项）的单价，逐键相等。
//
// # 为什么要把「单价」与「金额」摆在一起看
//
// 这条用量下 prompt 那一项的金额是 0.001（1000 token × 1e-6），而单价是
// 0.000001。两个数摆在同一条用例里，「填的是价格侧还是金额侧」就不是靠注释
// 声明，而是断言里看得见的事——从 Breakdown 反推单价的那条路会在这里挂。
//
// 等价变异（gremlins ARITHMETIC_BASE，unitPricesJSON 里 `len(billing.TokenSlots)
// + len(prices.Units)` 的 `+` 改成 `-`）：那只是 make 的**容量提示**，差变小
// （乃至为负）最多多扩容一次，键与值一个字节都不变。
func TestUsageEventUnitPricesAreSettlementPrices(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)

	charge := unitPriceCharge(t, userID, requestID, "12")
	charge.Prices = fiveSlotPrices()
	got := settlePricedRequest(ctx, t, db, s, charge)

	assertUnitPrices(t, got.UnitPrices, wantFiveSlotPrices())

	// 用量照旧落在它自己的列上：单价与用量是同一行的两半，合起来才复算得出
	// 那一笔金额。
	if got.PromptTokens != charge.Usage.PromptTokens {
		t.Errorf("prompt_tokens = %d，期望 %d", got.PromptTokens, charge.Usage.PromptTokens)
	}
	if got.Cost != "12.00000000" {
		t.Errorf("cost = %s，期望 12", got.Cost)
	}
}

// 只写规则**显式写过**的那几项：没写的槽位不进 unit_prices。
//
// 「写了 0」（这一项免费）与「没写」（规则漏了）在 money.Amount 上分不出来，
// 写进去会让两件事在库里长得一样。少一个键，读的人才知道当时没给这个价。
func TestUsageEventUnitPricesOnlyCoverDeclaredSlots(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)

	charge := unitPriceCharge(t, userID, requestID, "12")
	// 这条规则只给了输入与输出两个价（缓存与推理那三项这次一个 token 都没用，
	// 所以结算照样通过——见 billing.Validate）。
	charge.Prices = billing.Prices{
		Prompt:     money.MustParse("0.000001"),
		Completion: money.MustParse("0.000002"),
		Declared:   map[string]bool{"prompt": true, "completion": true},
	}
	charge.Usage.CacheReadTokens = 0
	charge.Usage.CacheWriteTokens = 0
	charge.Usage.ReasoningTokens = 0
	charge.Usage.UnitCounts = nil

	got := settlePricedRequest(ctx, t, db, s, charge)

	assertUnitPrices(t, got.UnitPrices, map[string]string{
		"prompt":     "0.00000100",
		"completion": "0.00000200",
	})
}

// 一次**零用量**的结算：unit_prices 是空对象。
//
// 空对象有确定含义——这次结算没有按任何单价算（上游没报 usage，规则只写了
// 一笔固定费）。这一列是 NOT NULL，写不出 NULL；落库层**总是**写它，
// 所以 {} 也不再表示「这一轮没人填」。
func TestUsageEventUnitPricesEmptyWhenSettlementHasNone(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)

	charge := unitPriceCharge(t, userID, requestID, "0.02")
	// 上游压根没报 usage：零用量，来源是 unreported（见 billingPathOf）。
	// 规则这时只写了固定费，Prices 一项都没写。
	charge.Usage = billing.Usage{}
	charge.UsageSource = tokens.SourceUnreported
	charge.Prices = billing.Prices{}

	got := settlePricedRequest(ctx, t, db, s, charge)

	if string(got.UnitPrices) != "{}" {
		t.Errorf("unit_prices = %s，期望 {}", got.UnitPrices)
	}
	// 用量那一列同样是空对象，两列在这一行里说的是同一件事（这次没有按量收费）。
	if string(got.UnitCounts) != "{}" {
		t.Errorf("unit_counts = %s，期望 {}", got.UnitCounts)
	}
}

// 撞上预留上界（Capped）的那一次：unit_prices 仍是单价，不受 Actual 被截的影响。
//
// # 这一条防的是什么
//
// SettleWithCharges 撞上界时把 Actual 截成预留额，而 Breakdown 留的是截之前
// 的金额（有意如此，见 internal/billing 的 TestSettleCapsOvershoot）。所以这一
// 行里出现两个数：cost 取 Actual（截过的），明细总额取 Breakdown（没截的）。
//
// 单价是规则给的，与这次截断无关。要是哪天有人改成「从金额反推单价」，这条
// 用例会挂——反推出来的价随 Actual 一起被截，复算时算出的金额会小于 cost，
// 而它看起来像个正常的价。
func TestUsageEventUnitPricesAreUnaffectedByCappedSettlement(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)

	// 按用量算是 12，预留只给了 5，于是 Actual 被截成 5。
	charge := unitPriceCharge(t, userID, requestID, "5")
	charge.Settlement.Capped = true
	charge.Settlement.Reserved = money.MustParse("5")
	charge.Settlement.Breakdown = billing.Breakdown{
		Prompt: money.MustParse("0.001"),
		Total:  money.MustParse("12"),
	}
	charge.Prices = fiveSlotPrices()

	got := settlePricedRequest(ctx, t, db, s, charge)

	assertUnitPrices(t, got.UnitPrices, wantFiveSlotPrices())
	if got.Cost != "5.00000000" {
		t.Errorf("cost = %s，期望 5（Actual 被截住之后的值）", got.Cost)
	}
	// 明细总额（12）与 cost（5）本来就不等，而 unit_prices 与两者都不是一回事：
	// 它是价格侧。这条断言把三者摆在一起，改错一侧就会在这里露出来。
	if got.Cost == charge.Settlement.Breakdown.Total.String() {
		t.Fatalf("夹具的前提不成立：Capped 时 cost 不该等于明细总额（%s）", charge.Settlement.Breakdown.Total)
	}
}

// upstream_cost 仍是 NULL——哪怕这一行里已经有售价、也已经有单价可填。
//
// 上游成本是**另一个口径**（适配器按自己的计价口径算，还是从上游响应的用量
// 字段折算，还没定），今天全仓库没有写入点。顺手拿售价顶上是最容易犯的那一
// 个错：毛利会永远是零，而账目仍然是平的。
func TestUsageEventUpstreamCostStaysNullWithPrices(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)

	charge := unitPriceCharge(t, userID, requestID, "12")
	charge.Prices = fiveSlotPrices()

	got := settlePricedRequest(ctx, t, db, s, charge)

	if got.UpstreamCost.Valid {
		t.Errorf("upstream_cost = %s，期望 NULL——上游成本与售价不是一个口径", got.UpstreamCost.String)
	}
	if got.Cost != "12.00000000" {
		t.Errorf("cost = %s，期望 12", got.Cost)
	}
}

// 计量项与 token 槽位撞名时**写不进去**，而不是挑一个写。
//
// 这一列是一个扁平对象，两个同名的单价只有一个键位：写下去的那一行会同时读
// 得出「两项都收过费」与「只记下了其中一项」，而它照样算得出数来。expr 求值时
// 已经把这种规则判为非法（见 internal/billing/expr 的撞名检查），只是那道判据
// 按 map 迭代顺序判，同一条规则可能这次报、下次不报——这里是它漏过之后的最后
// 一道。
//
// 报错而不是挑一个，是因为挑谁都错：token 单价与计量项单价都在这次结算里真
// 收过费。规则里给计量项换个名字就能修好。
func TestSaveRejectsClashingUnitPriceName(t *testing.T) {
	db := testDB(t)
	s := New(db)
	ctx := context.Background()

	userID := seed(t, db, "100")
	requestID := reqID(t)

	charge := unitPriceCharge(t, userID, requestID, "12")
	charge.Prices = billing.Prices{
		Prompt:   money.MustParse("0.000001"),
		Declared: map[string]bool{"prompt": true},
		Units:    map[string]money.Amount{"prompt": money.MustParse("0.5")},
	}

	err := reserveAndSettle(ctx, t, db, s, charge)
	if err == nil {
		t.Fatal("撞名的单价写进去了——这一行读不出哪个是哪个")
	}
	if !strings.Contains(err.Error(), "prompt") {
		t.Errorf("错误里没说是哪个名字撞了: %v", err)
	}
	if n := countUsageEvents(ctx, t, db, requestID); n != 0 {
		t.Errorf("写进去了 %d 行用量事件，期望 0——整组结算应当回滚", n)
	}
}
