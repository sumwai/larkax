package route

import (
	"time"

	"github.com/sumwai/larkax/internal/channel"
)

// Verdict 是一次上游调用的结论，决定它怎么记进渠道健康度。
//
// 三种而不是两种，是因为「失败」这个词在这里有两层意思：调用没有正常
// 结束，与**这次失败该记在渠道头上**。两者分开之后，「连续失败 N 次就别再试」
// 这条保护才不会被别人的坏请求或客户端的断开触发——那种触发一旦发生，
// 一个健康的渠道会被摘掉，而摘掉之后没有流量，计数也不会自己降回去。
type Verdict string

const (
	// VerdictSuccess 上游正常返回。
	VerdictSuccess Verdict = "success"
	// VerdictFailure 失败，且这次失败记在渠道头上：连续失败加一。
	VerdictFailure Verdict = "failure"
	// VerdictUnattributed 这次调用没有正常结束，但也不是渠道的问题：
	// 客户端中途断开、请求本身有错（参数错、模型名不存在）。
	//
	// 它照常算一次请求（分母要诚实），但既不加连续失败、也不清零——
	// 那一串失败与它无关，不该被它打断。
	VerdictUnattributed Verdict = "unattributed"
)

// CallOutcome 是一次上游调用的结局，交给渠道健康度与额度状态的写入方。
//
// 与 Attempt 的分工：Attempt 记录的是**路由过程**（第几次尝试、值不值得换
// 下一个），CallOutcome 记录的是**这一次调用的结果**（成没成、用了多少），
// 由写入方落到 channel_health_rollup 与 channel_quota_state。
//
// 它由 gateway 构造：结局的判定要看整条请求（流式那条路上「客户端断开」与
// 「上游中途断了」是两回事），而只有编排层看得见。
type CallOutcome struct {
	ChannelID int64
	// Model 是这次请求的**规范模型名**，不是发往上游的别名。
	//
	// 它是健康度桶的模型维度：写入方按它落一行模型桶，同时维护那一行
	// model = '' 的渠道级合并行（见 internal/channels 的 rollup）。成本按
	// （渠道，规范模型）定价，所以选路侧按模型取成本时读的就是这一维。
	//
	// 空串留给「不知道模型」的调用方（测试夹具、将来的其它写入方）：
	// 它只会落进合并行，按模型查询时读作未知。
	Model string
	// At 是这次调用结束的时刻，也是健康度桶的归属时刻。
	At time.Time
	// Verdict 是这次调用的结论。见 Verdict 的注释。
	Verdict Verdict
	// Usage 是上游给出的用量，用于自记账扣减额度。
	//
	// 只有成功的调用有值：失败的调用上游没有回执，用量未知。
	// 未知时它是零值，写入方不得把它当成「用零个 token」——
	// 自记账扣零等于没扣，而那是**偏高**的方向（以为额度还剩着）。
	//
	// 它本身不参与缓存与延迟那两列：那两列由写入方按「渠道 × 小时桶」从
	// usage_events 重算（见 internal/channels 的 rollupSignalsSQL），
	// 所以调用方要在结算落库**之后**才记这一次——结算才写 usage_events。
	Usage channel.Usage
}
