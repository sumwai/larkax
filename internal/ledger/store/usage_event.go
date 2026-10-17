package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/sumwai/larkax/internal/pipeline"
	"github.com/sumwai/larkax/internal/tokens"
)

// ErrUnknownUsageSource 表示 Charge.UsageSource 不是已知的用量来源。
//
// 与 ErrUnknownProtocol 分开：那个是**协议**，这个是**用量是怎么来的**。
//
// 注意它挡不住「漏了赋值」：tokens.Source 的零值**就是** SourceMeasured，
// 而 measured 是合法取值（非流式的用量只可能来自上游响应体），两者在这一层
// 分不开。一个漏设的来源因此会落成「实测值」——那正是最不该凭空声称的一档。
//
// 而生产那一侧**也分不开**，至少非流式这条路不行：internal/gateway 的 Chat
// 无条件写 tokens.SourceMeasured，而上游响应体里没有 usage 块时 usageOf
// 返回零值。于是落库那一行是 billing_path='measured'、estimator=NULL、cost=0、
// prompt_tokens=0、completion_tokens=0——**与「上游确实报了 0 token」逐列
// 相同**，读这一行的人会把「上游压根没报」读成「上游报了 0」。
//
// 流式那条路不同：来源由事件流与入口一起决定，缺 usage 时落 estimated
// （见 internal/gateway 的 Result.Estimated 与 stream.go）。
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
// # local_hour / is_off_day 为什么写 NULL
//
// 它们是「这次求值用了哪个时区、算的是哪一天」，而求值那一侧今天不报这两个
// 值，Charge 上对应的两个字段**结算路径从不填**（理由见 pipeline.Charge
// 的注释：时区由规则作者逐次给出，填一个 Env.Region 的时区会得到一个看起来
// 权威、却可能与规则实际用的那个不同的数）。
//
// 所以这里写 NULL——「不知道」——而不是把 Charge 上的零值写成 0 与 false。
// 后者在库里长得像「求值算出来就是当地 0 点、不是休息日」，而它照样算得出数
// 来，照它对账会得到一个错的结论。列本身可空，正是为这种情形留的。
//
// # upstream_cost 为什么不写
//
// 上游成本的**口径还没定**（适配器按自己的计价口径算，还是从上游响应的用量
// 字段折算），而填一个猜出来的数比不填更糟——见 issue #163 的第 2 条。
// 这一列留给那一半。
//
// # unit_prices 为什么不写
//
// 这一列**不在这条 INSERT 的列表里**，于是取库上的默认值 `'{}'`。
// 它比可空的那几列更难发现：落库之后看起来像「这次没有单价」，而实际是
// 「这一列这一轮不填」——两种含义在那一列上长得一样，而它是 NOT NULL，
// 连「没写」这个信号都没有。
//
// 这次结算按哪些单价算的，要复算得回到 billing_requests 的 pricing_rule_id
// 与 pricing_expr_snapshot：那是求值当时的规则快照，比事后补一份单价更接近
// 事实。等真要在这里落单价时，先看下面这一条。
//
// # Capped 时 Settlement.Breakdown.Total 没有被截
//
// billing.SettleWithCharges 撞上预留上界时把 Settlement.Actual 截成预留额，
// 而 Breakdown 留的是**截之前**的金额——这是有意的，有用例钉住
// （internal/billing 的 TestSettleCapsOvershoot：「明细仍要保留未截断的金额，
// 供排查超了多少」）。所以 Capped 时 Actual 与 Breakdown.Total **本来就不等**，
// 而 cost 这一列取的是 Actual。
//
// 后果：哪天有人从 Breakdown 去填 unit_prices，那一行的 cost 会与明细对不上
// （明细算出来的总额大于 cost）。要填得先决定填哪一侧，而不是顺手取一个。
//
// # duration_ms 为什么不写
//
// 与 unit_prices 同一个形状：它也不在上面那条 INSERT 的列表里，于是落成
// NULL。这一列是「这次请求花了多久」，而 pipeline.Charge 上没有承载它的
// 字段——耗时只有网关量得到，而它今天没把那个数带进结算。这一轮不写，
// 等那条路接上再说。
const insertUsageEventSQL = `
	INSERT INTO usage_events (
		request_id, user_id, channel_id, canonical_model,
		prompt_tokens, completion_tokens, cache_read_tokens, cache_write_tokens,
		reasoning_tokens, unit_counts, billing_path, estimator,
		cost, asset, priced_at, local_hour, is_off_day
	) VALUES (
		$1, $2, NULLIF($3, 0), $4,
		$5, $6, $7, $8,
		$9, $10::jsonb, $11, NULLIF($12, ''),
		$13::numeric, $14, $15, NULL, NULL
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

	usage := charge.Usage
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
		path,
		estimator,
		// 收的钱取 Settlement.Actual（按实际用量算出的金额），不是预留额：
		// 对账要问的是「这笔用量该收多少」，而预留只是上界。撞上上界被截住
		// 时（Capped）Actual 是截住后的值，与分录里扣的钱一致——而那一档下
		// 它与 Breakdown.Total 不相等，见 insertUsageEventSQL 的注释。
		charge.Settlement.Actual.String(),
		charge.Asset,
		charge.PricedAt,
	); err != nil {
		return fmt.Errorf("写用量事件失败: %w", err)
	}
	return nil
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

// billingPathOf 把用量来源翻译成表上的两列。
//
// # 为什么是两列
//
// billing_path 回答「这个数能不能拿去跟上游对账」，只有三档
// （measured / estimated / fallback，表上有 CHECK）；estimator 回答「它是
// 怎么来的」，是自由文本。
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
func billingPathOf(src tokens.Source) (path, estimator string, err error) {
	switch src {
	case tokens.SourceMeasured:
		return "measured", "", nil
	case tokens.SourceTokenized:
		return "estimated", "tokenized", nil
	case tokens.SourceEstimated:
		return "estimated", "char_heuristic", nil
	default:
		// 表上的 CHECK 也会挡下它，但那句话是「new row ... violates check
		// constraint」，读的人得先知道这一列有哪些取值才看得懂。在能说清楚
		// 的地方说清楚。
		return "", "", fmt.Errorf("%w: %d", ErrUnknownUsageSource, src)
	}
}
