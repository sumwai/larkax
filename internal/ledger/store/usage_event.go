package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/sumwai/larkax/internal/billing"
	"github.com/sumwai/larkax/internal/pipeline"
	"github.com/sumwai/larkax/internal/tokens"
)

// ErrUnknownUsageSource 表示 Charge.UsageSource 不是已知的用量来源。
//
// 与 ErrUnknownProtocol 分开：那个是**协议**，这个是**用量是怎么来的**。
//
// 注意它挡不住「漏了赋值」：tokens.Source 的零值**就是** SourceMeasured，
// 而 measured 是合法取值，两者在这一层分不开。一个漏设的来源因此会落成
// 「实测值」——那正是最不该凭空声称的一档。
//
// 上游没报 usage 的那条路**已经分得开**：internal/gateway 的 Chat 在记录
// 用量的那一刻看响应体里有没有 usage 块，没有就记 SourceUnreported
// （见 usageSourceOf）。它落成 billing_path='estimated'、
// estimator='unreported'，与「上游报了 0」（measured、estimator 为 NULL）
// 逐列不同。
//
// 流式那条路不同：来源由事件流与入口一起决定，缺 usage 时落 estimated
// （见 internal/gateway 的 Result.Estimated 与 stream.go）。决策那条路
// （Decide）与 chat 共用同一条判据——看上游报没报 usage 块（usageSourceOf
// 读 channel.Usage.Reported），所以它也不再无条件记实测。
var ErrUnknownUsageSource = errors.New("未知的用量来源")

// insertUsageEventSQL 写一行用量事件。
//
// 列的来源逐项写在 insertUsageEvent 的实参处。
//
// # created_at 为什么不在这里写
//
// 它是**分区键**（表按月分区，见迁移 00001），这里取库上的默认值 now()——
// 也就是落库这一刻——而不是 charge.PricedAt。
//
// 一次迟到的写入（上游跑了好几分钟、跨过了月末）如果带着受理时刻，那一行
// 就会落进 usage_events_default 兜底分区，而那个分区「非空即告警」。
// 告警该指向「分区预建漏跑了」，不该指向一次正常的写入——这与
// internal/snapshotwire 写入前先确认分区是同一个理由。事件时刻由
// priced_at 那一列单独承载，两个时刻分得开。
//
// # local_hour / is_off_day 写什么
//
// 写这次求值**实际用到**的「当地」口径：地区时区的钟点与那一天算不算休息日，
// 取自 Charge.LocalTime——它由 Settle 从同一次求值的结果搬过来，与这一行的
// 单价、金额出自同一个 expr.Result。所以这两格与规则当时判的那一档是同一个
// 值，事后拿它们复算得出当时的价；而在落库层或结算层按 Region 重算一遍的话，
// 那两格会是一个**看起来权威、却与规则实际用的那个可能不同**的数。
//
// 规则没有用到当地口径（isWorkday / isHoliday / isPeak 一个都没走）时
// Charge.LocalTime 为 nil，这里写 NULL——「不知道」，而不是把零值写成 0 与
// false。后者在库里长得像「求值算出来就是当地 0 点、不是休息日」，而它照样
// 算得出数来，照它对账会得到一个错的结论。列本身可空，正是为这种情形留的。
//
// 什么算「用到」由 expr.Result.LocalTime 定义，这里不重判一遍：两处各判一份
// 的表现是「求值说用了、落库说没用」这种谁也看不出错的组合。
//
// # upstream_cost 为什么不写
//
// 上游成本的**口径还没定**（适配器按自己的计价口径算，还是从上游响应的用量
// 字段折算），而填一个猜出来的数比不填更糟——见 issue #163 的第 2 条。
// 这一列留给那一半：**今天全仓库没有任何写入点**，
// pipeline.Outcome.UpstreamCost 也还没有生产者（它那处注释同样标了「没有来源」）。
//
// 尤其不能拿这次收的钱顶上：那是售价，混起来毛利永远是零。
//
// # unit_prices 写什么
//
// 写这次结算**按哪些单价算的**，取自 Charge.Prices——它是这次求值用的那一组
// 价（见 pipeline.Charge.Prices）。键与 unit_counts 同名同源：
// 五个 token 槽位（prompt / completion / cache_read / cache_write /
// reasoning，名字出自 billing.TokenSlots）+ 非 token 计量项名。
//
// 只写规则**显式写过**的那几个 token 槽位（billing.Prices.Declared）：
// 「写了 0」（这一项免费）与「没写」（规则漏了）在 money.Amount 上分不出来，
// 写进去会让两件事在库里长得一样。少一个键，就说明当时没给这个价。
//
// 值是 money 的定点十进制**字符串**（"0.00000100"），不是 JSON 数字：
// 每 token 1e-6 这种量级写成数字要先变成 float64，而 float64 表示不了所有
// 8 位小数的定点值——落库的价与当时用的价差一点点，复算时就对不上。
// unit_counts 用数字是因为那些是计数，整数在 JSON 里本来就没有精度问题。
//
// 撞名（一个非 token 计量项叫 prompt）在这里报错而不是挑一个写：这一列是
// 一个扁平对象，两个同名的单价只有一个键位，写下去的那一行会同时有「两个
// 都收过费」与「只写下了其中一个」两种读法。expr 求值时已把这种规则判为
// 非法（见 internal/billing/expr 的撞名检查），这里补的是那道判据可能的
// 漏网（它按 map 迭代顺序判，见 unitPricesJSON）。
//
// 空对象现在只有一个含义：**这次结算没有单价可记**——这一列**总是**在
// INSERT 的列表里，所以 {} 不再是「没人填」。一次零用量的结算、或者规则只
// 写了固定费时，它确实是 {}。这一列是 NOT NULL DEFAULT '{}'，NULL 不是
// 可选项——可空的那几列（local_hour 等）才有「写 NULL 表示不知道」这个选项。
//
// # Capped 时 Settlement.Breakdown.Total 没有被截
//
// billing.SettleWithCharges 撞上预留上界时把 Settlement.Actual 截成预留额，
// 而 Breakdown 留的是**截之前**的金额——这是有意的，有用例钉住
// （internal/billing 的 TestSettleCapsOvershoot：「明细仍要保留未截断的金额，
// 供排查超了多少」）。所以 Capped 时 Actual 与 Breakdown.Total **本来就不等**，
// 而 cost 这一列取的是 Actual。
//
// unit_prices 取的是**价格侧**（Charge.Prices），所以与这次截断无关：单价是
// 规则给的，不随金额被截而变化，两者在那一行里是对得上的（用量 × 单价 = 明细
// 的金额，而明细的总额在那次是被记在 Breakdown 里、不是 cost 里的那个数）。
// 反过来从 Breakdown 反推单价则要除、要猜零用量那一项，撞上界时还会推出一个
// 与规则无关的价——这一列存在的意义正是「事后能复算」，推出来的价让复算
// 看起来成立却对不上 cost。
//
// # duration_ms 从哪来
//
// 它是「这次上游调用花了多久」，量它的只有网关（非流式取最后一次尝试的
// 耗时，流式再加上读完那条流的时间），由 gateway 放进 pipeline.Outcome，
// Settle 搬到 Charge.Duration，这里落库。
//
// 零值写 NULL 而不是 0：0 毫秒是一个**看起来正常的样本**，它会把按桶算出
// 来的分位数往下拉，而那条渠道的延迟分因此在库里读作「很快」——没有任何
// 东西会报出来。NULL 在读侧是「没有样本」，聚合时被 FILTER 掉。
//
// duration_ms 是 integer 列，$19 传的是 Go 的 int64 毫秒数。这里**不加**
// 类型标注：裸 0 与 $3 那处不同——它与列宽一致（integer），而 $3 是 bigint，
// 不加标注会被推成 int4。超过 int32 的耗时（约 24 天）会在数据库这一侧
// 报溢出，不会静默截断。
//
// local_hour / is_off_day 两列的参数位同理不加标注：小整数与布尔按列宽
// 传过去即可，「没有值」由 Go 那一侧的 nil 表达（见下面那两个变量）。
//
// NULLIF($3, 0::bigint) 里的类型标注是必要的：channel_id 列为 bigint，
// 裸 0 会把 $3 推成 int4（见 store.go 的 SaveReservation）。
const insertUsageEventSQL = `
	INSERT INTO usage_events (
		request_id, user_id, channel_id, canonical_model,
		prompt_tokens, completion_tokens, cache_read_tokens, cache_write_tokens,
		reasoning_tokens, unit_counts, unit_prices, billing_path, estimator,
		cost, asset, priced_at, local_hour, is_off_day, duration_ms
	) VALUES (
		$1, $2, NULLIF($3, 0::bigint), $4,
		$5, $6, $7, $8,
		$9, $10::jsonb, $11::jsonb, $12, NULLIF($13, ''),
		$14::numeric, $15, $16, $17, $18, NULLIF($19, 0)
	)`

// insertUsageEvent 写一行用量事件：这次结算**按什么用量**算出来的。
//
// # 它回答的问题与账本不同
//
// 账本分录回答「动了多少钱」，这一行回答「那笔钱是按什么用量、哪个时刻、
// 哪条口径算出来的」。两者都只追加，而对账正是拿这两边对——只有分录的话，
// 「这笔钱算得对不对」无从查起：用量没留痕，事后复算没有输入。
//
// # 只在结算成功时调用
//
// 调用点在 Save 里 markSettled 之后：那一步返回 false 表示这是一次重放，
// 此时一个字节都不该再写。上游失败那一路根本走不到 Save（网关把结算结果
// 丢掉、只释放预留，见 internal/gateway 的 settleAndFail），所以这张表里
// 只有真正计过费的请求。
//
// # 金额为零的结算也写这一行
//
// 金额为零时 pipeline 不写分录（零金额分录会让「有分录就说明发生过资金流动」
// 这个判据失效），用量事件不适用那条：它不是账本，而是「这次请求用了多少」的
// 底稿。对零金额的那一类（上游压根没报 usage）来说，正是这一行说明了
// 「为什么这次没有分录」——没有它，那种请求在库里就只剩账单上的一条零金额。
func insertUsageEvent(ctx context.Context, tx *sql.Tx, req pipeline.Request, charge *pipeline.Charge) error {
	path, estimator, err := billingPathOf(charge.UsageSource)
	if err != nil {
		return err
	}
	counts, err := unitCountsJSON(charge.Usage.UnitCounts)
	if err != nil {
		return err
	}
	prices, err := unitPricesJSON(charge.Prices)
	if err != nil {
		return err
	}

	usage := charge.Usage

	// 当地口径那两列：取值与「有没有值」都由 Charge.LocalTime 决定，理由见
	// insertUsageEventSQL 的「local_hour / is_off_day 写什么」一节。nil 时
	// 两个参数都是 nil，写下去就是 NULL。
	var localHour, isOffDay any
	if lt := charge.LocalTime; lt != nil {
		localHour, isOffDay = lt.Hour, lt.OffDay
	}

	if _, err := tx.ExecContext(ctx, insertUsageEventSQL,
		charge.RequestID,
		charge.UserID,
		// 渠道为 0 表示「还不知道」（预留发生在选路之前），落成 NULL。
		// 与 markSettled 写 billing_requests.channel_id 同一个口径。
		req.ChannelID,
		req.CanonicalModel,
		usage.PromptTokens,
		usage.CompletionTokens,
		usage.CacheReadTokens,
		usage.CacheWriteTokens,
		usage.ReasoningTokens,
		counts,
		prices,
		path,
		estimator,
		// 收的钱取 Settlement.Actual（按实际用量算出的金额），不是预留额：
		// 对账要问的是「这笔用量该收多少」，而预留只是上界。撞上上界被截住
		// 时（Capped）Actual 是截住后的值，与分录里扣的钱一致——而那一档下
		// 它与 Breakdown.Total 不相等，见 insertUsageEventSQL 的注释。
		charge.Settlement.Actual.String(),
		charge.Asset,
		charge.PricedAt,
		// 当地钟点与「那一天算不算休息日」：这次求值用到的两个值，
		// 由 Settle 从同一次求值的结果搬过来（见 Charge.LocalTime）。
		localHour,
		isOffDay,
		durationMS(charge.Duration),
	); err != nil {
		// 这张表今天没有外键（user_id 与 channel_id 都是裸 bigint），
		// 套一层是为了「写路径的错误都从同一个出口分类」这个形状不破：
		// 将来加上外键时不必回来找这一处。
		return constraintFailure(fmt.Errorf("写用量事件失败: %w", err))
	}
	return nil
}

// durationMS 把耗时折成 usage_events.duration_ms 的整数毫秒。
//
// 零值返回 0，由 SQL 里的 NULLIF 写成 NULL（理由见 insertUsageEventSQL）。
//
// 向上取整到 1 毫秒：一次真实发生的调用再快也不是 0 秒，而 0 会被 NULLIF
// 当成「没测到」丢掉——那等于把最快的那一批样本从分位数里删掉，聚合出来
// 的 p95 会偏高，而偏高看起来像渠道变慢。
func durationMS(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	ms := d / time.Millisecond
	if d%time.Millisecond != 0 {
		ms++
	}
	return int64(ms)
}

// unitCountsJSON 把非 token 用量序列化成 jsonb 的字面量。
//
// 入参是裸的 map：这里要的只是「键到数量」这个形状，不必为此让落库层再直接
// 引一层计费层的类型（Charge.Usage 本来就是 billing.Usage，形状已经带过来了）。
//
// 空与 nil 都写成 {}，而不是让 json.Marshal 把 nil map 写成 JSON 的 null：
// 那一列是 NOT NULL DEFAULT '{}'，而 JSON 的 null 不是 SQL 的 NULL，NOT NULL
// 挡不住它。写进去之后 `unit_counts->>'searches'` 会得到 NULL，与「这次没有
// 搜索这一项」分不开——两者在对账里是两件事。
func unitCountsJSON(counts map[string]int64) (string, error) {
	if len(counts) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(counts)
	if err != nil {
		return "", fmt.Errorf("序列化非 token 用量失败: %w", err)
	}
	return string(b), nil
}

// unitPricesJSON 把这次结算按的单价序列化成 jsonb 的字面量。
//
// 键名不在这里手抄：token 那几项逐槽从 billing.TokenSlots 取，那份表是
// 「有哪些 token 槽位、它们在规则里叫什么」的唯一来源（它同时喂着
// Prices.Declared 与 expr 的结果键白名单）。这里再抄一份的话，加一个槽位时
// 漏改这一处不会编译失败，只会让新槽位的单价在账上消失。
//
// 只写规则显式写过的那几项，理由见 insertUsageEventSQL 的 unit_prices 一节。
//
// 空与 nil 都写成 {}，而不是让 json.Marshal 把 nil map 写成 JSON 的 null：
// 那一列是 NOT NULL，而 JSON 的 null 不是 SQL 的 NULL，NOT NULL 挡不住它
// （与 unit_countsJSON 同一条理由，那里还有「null 与 {} 在查询里分不开」）。
func unitPricesJSON(prices billing.Prices) (string, error) {
	out := make(map[string]string, len(billing.TokenSlots)+len(prices.Units))
	for _, slot := range billing.TokenSlots {
		if !prices.Declared[slot.Name] {
			continue
		}
		out[slot.Name] = slot.Price(prices).String()
	}
	// 非 token 计量项：键与 unit_counts 相同，于是同一行里「这个计量项多少钱、
	// 收了多少」是对得上的。
	for name, price := range prices.Units {
		if _, clash := out[name]; clash {
			// 计量项与 token 槽位撞名。两个都是这次真正用到的单价，而这一列
			// 只有一个键位——挑一个写进去，那一行就同时读得出「两项都收过费」
			// 与「只记下了其中一项」，而它照样算得出数来（见
			// insertUsageEventSQL 的 unit_prices 一节）。
			//
			// 挡在这里而不是放过去，是因为 expr 求值时的撞名检查按 map 迭代
			// 顺序判（先碰到哪一项决定了报不报），同一条规则可能这次报、下次
			// 不报。这里是它漏过之后的最后一道：写不下去就说出来。
			return "", fmt.Errorf("计量项名 %q 与 token 单价键名冲突，unit_prices 写不下两个同名的单价", name)
		}
		out[name] = price.String()
	}
	if len(out) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("序列化单价失败: %w", err)
	}
	return string(b), nil
}

// billingPathOf 把用量来源翻译成表上的两列。
//
// # 为什么是两列
//
// billing_path 回答「这个数能不能拿去跟上游对账」，只有两档
// （measured / estimated，表上有 CHECK）；estimator 回答「它是怎么来的」，
// 是自由文本。
//
// 表上原本还有第三档 'fallback'，它没有产出点，已由迁移 00020 从 CHECK 里
// 删掉。它没有被拿回来给「上游没报」用，因为那一档说不出比 estimated
// 多在哪：billing_path 只分「能不能对账」，而占位的零同样不能对账，
// 落 estimated 就是正确答案；具体是「没报」还是「估的」，由 estimator 说。
//
// 也不为它重新加一档 billing_path：那要一条新迁移改 CHECK，而这一列的
// 语义（能否对账）已经被两档分完了。
//
// # 本地计数落成 estimated，不是 measured
//
// tokens.SourceTokenized 是本地词表数出来的，比启发式准得多，但它**仍然是
// 本地值**：与上游实测的差是系统性的（消息框架 token、上游私有的特殊 token，
// 本地词表里没有），拿去对账会表现为「永远差一点」，而那个差值没有业务含义
// （见 tokens.Source.IsExact）。billing_path 说的是可不可以对账，不是准不准
// ——所以它落 estimated，由 estimator 说明它是词表数出来的。
//
// estimator 的取值：表上那一列举的例子是 tiktoken / hf / count_tokens_api /
// char_heuristic，那是举例不是约束。这里给的是**来源**那一层的名字，与
// tokens.Source 一一对应：实测值没有估算器，留空（落成 NULL）而不是填一个
// 名字——它本来就不是估出来的。
//
// SourceUnreported 落 estimated + "unreported"：那个数不是估出来的，而是
// **根本没有数**——上游响应体里没有 usage 块，零值是占位。它同样不能拿去
// 对账（所以是 estimated），而 estimator 把「没报」与「估的」分开。
func billingPathOf(src tokens.Source) (path, estimator string, err error) {
	switch src {
	case tokens.SourceMeasured:
		return "measured", "", nil
	case tokens.SourceTokenized:
		return "estimated", "tokenized", nil
	case tokens.SourceEstimated:
		return "estimated", "char_heuristic", nil
	case tokens.SourceUnreported:
		return "estimated", "unreported", nil
	default:
		// 表上的 CHECK 也会挡下它，但那句话是「new row ... violates check
		// constraint」，读的人得先知道这一列有哪些取值才看得懂。在能说清楚
		// 的地方说清楚。
		return "", "", fmt.Errorf("%w: %d", ErrUnknownUsageSource, src)
	}
}
