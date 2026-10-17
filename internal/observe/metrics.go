package observe

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// 本文件是这套服务的最小指标面：手写的计数器与仪表，按 Prometheus 的文本格式暴露。
//
// # 为什么不引一个指标库
//
// 那个格式本身只有三种行（# HELP / # TYPE / 样本），渲染它几十行就够，
// 而被任何现成采集器抓走靠的正是这个格式。多引一个库是拿一份长期的
// 升级与安全跟进成本，换这几十行代码。
//
// # 为什么计数器是包级的一份
//
// 增量点散在计费、网关、上游各处，而那些包的构造点不在本包手里——要让
// 它们各自拿一个注册表，就得改它们的结构体与装配处，「加一个计数器」
// 于是变成一次跨包改动。包级的 Metrics 让增量点始终是**一行**：
//
//	observe.Metrics.Settlements.With("measured").Inc()
//
// # 指标与日志的分工
//
// 指标回答「有没有在发生」，日志回答「是哪一次、为什么」。两者不互相
// 替代：撞上预留上界这件事由计数器的**取值**报警，是哪一次请求则由日志里
// 的 request_id 去查。
//
// 名字与标签的约定见 docs/metrics.md。

// Metrics 是本进程在用的注册表。
//
// 代价要说清楚：它是一份进程级的全局状态。要断言具体数值的测试应当自己
// NewRegistry() 造一个，而不是往这一份上加。
var Metrics = NewRegistry()

// Counter 是一个只增不减的计数器。
//
// 用原子而不是互斥锁：增量点都在请求路径上，一个计数器不该在那里制造
// 锁竞争，也不该成为「加一个计数器就要重新考虑热路径」的理由。
type Counter struct {
	name string
	v    atomic.Uint64
}

// Inc 把计数器加一。
func (c *Counter) Inc() { c.v.Add(1) }

// Add 把计数器加上 n。
//
// 唯一的用处是金额侧的计数器（见 Registry.CappedAmount）：它累加的是最小
// 单位的整数，不是次数。入参是无符号的——负数在这里根本表达不出来，
// 而这正是想要的：一个只增不减的计数器收到负数，回绕之后是一个巨大的
// 值，采集端会把它读成「忽然暴涨」，排查方向就被引到上游去了。
func (c *Counter) Add(n uint64) { c.v.Add(n) }

// Value 返回当前值。它只用于测试与断言，不参与渲染之外的逻辑。
func (c *Counter) Value() uint64 { return c.v.Load() }

// Gauge 是一个可升可降的即时值。
//
// 与 Counter 分成两个类型，而不是给 Counter 加一个 Set：一个只增不减的数
// 被谁 Set 回去是这里最坏的一类 bug（曲线忽然回头，而「一直涨」正是看它
// 的人唯一的判据），分成两个类型之后那种写法根本表达不出来。
//
// 它**只在被设置过之后才输出**，与不带标签的计数器「总是输出、包括 0」
// 的做法相反。理由是这里的 0 是一个有意义的取值（见 Registry.RateLimitExact），
// 于是「没被设置过」必须与「值就是 0」分得开——分不开时，一个没有接入仪表
// 的进程会输出一行 0，看起来与「接入了、当前是 0」一模一样，而那正是这个包
// 要防的那种「安静地不输出」。
type Gauge struct {
	name string
	// set 记录它有没有被设置过。用原子而不是普通 bool：Set 在请求路径上。
	set atomic.Bool
	v   atomic.Int64
}

// Set 把仪表设成 v。
func (g *Gauge) Set(v int64) {
	g.v.Store(v)
	g.set.Store(true)
}

// Add 把仪表加上 n（n 可以为负）。
func (g *Gauge) Add(n int64) {
	// 顺序是「先加、再标记」。反过来的话，读的人会看到一个已经标记成
	// 「设置过」、值却还是初值的仪表——那个 0 会被当成一次真实读数。
	g.v.Add(n)
	g.set.Store(true)
}

// Value 返回当前值。它只用于测试与断言，不参与渲染之外的逻辑。
func (g *Gauge) Value() int64 { return g.v.Load() }

// Set 过没有。没设置过时整族不输出。
func (g *Gauge) samples() []sample {
	if !g.set.Load() {
		return nil
	}
	return []sample{{value: strconv.FormatInt(g.v.Load(), 10)}}
}

// LabeledCounter 是一族按标签区分的计数器。
//
// **标签值必须来自有限集合。** 用户 id、渠道 id、请求 id、模型名都不能
// 做标签：每个取值各占一条时间序列，而序列数没有上限，采集器的内存与
// 查询开销会跟着请求数一起涨。这类值属于日志。
type LabeledCounter struct {
	name   string
	labels []string // 标签名，顺序与 With 的入参一致
	mu     sync.Mutex
	series map[string]*Counter
}

// With 取回一组标签值对应的计数器，没有就建一个。
//
// 入参个数与标签名个数不一致时直接 panic：那是编码错误，而它若被静默
// 接受，表现为「多出一条没人认识的序列」，比当场炸掉更难发现。
func (c *LabeledCounter) With(values ...string) *Counter {
	if len(values) != len(c.labels) {
		panic(fmt.Sprintf("计数器 %s 声明了 %d 个标签，收到 %d 个值",
			c.name, len(c.labels), len(values)))
	}
	key := strings.Join(values, "\x00")

	c.mu.Lock()
	defer c.mu.Unlock()
	if ct, ok := c.series[key]; ok {
		return ct
	}
	ct := &Counter{name: c.name}
	c.series[key] = ct
	return ct
}

// samples 取当前全部序列的快照，按标签排序。
//
// 排序是为了让同一份数据每次渲染出来的顺序一致：顺序随 map 迭代而变的
// 输出没法直接比对，而「抓两次、diff 一下」是排查时最常用的一步。
func (c *LabeledCounter) samples() []sample {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := make([]sample, 0, len(c.series))
	for key, ct := range c.series {
		out = append(out, sample{
			labels: c.renderLabels(key),
			value:  strconv.FormatUint(ct.v.Load(), 10),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].labels < out[j].labels })
	return out
}

func (c *LabeledCounter) renderLabels(key string) string {
	values := strings.Split(key, "\x00")

	var b strings.Builder
	b.WriteByte('{')
	for i, name := range c.labels {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(name)
		b.WriteString(`="`)
		b.WriteString(escapeLabelValue(values[i]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

// sample 是一条待渲染的样本：一组标签值，加一个已经格式化好的数值。
type sample struct {
	// labels 是渲染好的 {k="v"}；没有标签时为空串。
	labels string
	// value 是**已经格式化好的**数值文本。
	//
	// 存字符串而不是 uint64：这个包里现在既有只增不减的计数器（无符号），
	// 也有可升可降的仪表（有符号）。存成无符号的话，仪表上的 -1 会被渲染成
	// 一个巨大的正整数，那是一条看起来正常、含义完全相反的序列；而存成一个
	// 有符号数则会把这个取舍推给计数器（它上亿的数要经过一次可能变号的转换）。
	// 格式化只在渲染时发生一次，两边都不必猜对方是什么类型。
	value string
}

// family 是注册表里的一条指标：名字、类型、说明，以及怎么取它当前的样本。
type family struct {
	name    string
	typ     string // Prometheus 的类型名：counter 或 gauge
	help    string
	samples func() []sample
}

// Registry 是一组计数器。
//
// 字段在 NewRegistry 里创建，**并在同一行登记进 families**。分开写会出现
// 「声明了字段但忘了登记」，而那种计数器安静地不输出，与「根本没接上」
// 长得一模一样——正是这个包要解决的那个问题。
type Registry struct {
	// Settlements 是结算完成的次数，按用量来源分。
	//
	// 它是 CappedSettlements 的分母。只有它同时在涨，「Capped 一直是 0」
	// 才说明真的没撞上，而不是计数器没接上。
	Settlements *LabeledCounter

	// CappedSettlements 是结算金额被预留上界截住的次数，按入口协议分。
	//
	// 按设计它应当一直是 0。持续大于 0 说明预留上界算错了，差额由平台
	// 承担，而**账目仍然是平的**——对账看不出来，只有这个数能看见。
	//
	// 标签值取自 pipeline.Request.Protocol（channel.Protocol 的取值集合，
	// 闭合枚举）。三种 chat 协议与决策协议的上界来源不同：chat 按
	// max_tokens 与正文估算，决策表面按 internal/systemone 的两个常量。
	// 混成一条序列时分不出是哪一条协议的上界在漏。
	CappedSettlements *LabeledCounter

	// CappedAmount 是结算被预留上界截住的那部分**金额**，按入口协议与
	// 资产品种分，单位是 money.Scale 位定点的最小单位（1e-8 元）。
	//
	// 它是 CappedSettlements 的金额侧孪生：那个数回答「撞了几次」，这个数
	// 回答「少收了多少钱」。后者才是这套系统最贵的错误——撞上界时账目仍然
	// 是平的（两边记的是同一个被截小的数），对账永远看不出来。
	//
	// 按设计它应当一直是 0。差额取自 Settlement.Breakdown.Total −
	// Settlement.Actual（撞上界时 Actual 被截成 Reserved，明细不动），
	// 也就是落库时 computed_amount − final_amount 的同一个差。**累计值非 0
	// 就是这段时间里平台实际承担掉的钱**。
	//
	// 单位用最小单位而不是浮点元：f64 元在这一套金额上会丢精度（单价细到
	// 1e-6 元，一笔小额结算乘出来常常只有几十个最小单位），而差额要能和
	// billing_requests 的 computed_amount − final_amount 对得上——那一列是
	// numeric(20, 8)，同样的 Scale。浮点元会让「累计值」与「逐行相减再求和」
	// 对不上，而这两个数正是要互相印证的两个口径。
	//
	// 标签取值：protocol 同 CappedSettlements（channel.Protocol 的取值集合）；
	// asset 取结算用的资产品种，即 ledger.Asset 的取值集合——今天只有 CNY，
	// 装配期由 ledger.Asset.Valid 挡在启动那一步，所以它也是闭合的。
	// 分开两个标签而不是合成一个：上界算错按协议改，少收的钱按资产记，
	// 混成一条序列时两个问题得一起拆。
	CappedAmount *LabeledCounter

	// UnbilledSettlements 是未计费的结算次数。
	//
	// 两个原因都走这里：上游失败，以及请求在结算之前就中止了
	// （见 pipeline.Outcome.UpstreamFailed）。在这一层分不出是哪一个。
	UnbilledSettlements *Counter

	// StreamUnderfrozen 是流式请求**补冻没冻上而放行**的次数。
	//
	// 流式那条路的冻结是随流递进的（见 internal/gateway 的 streamFreeze 与
	// #498）：每产出过一个阈值段就补冻一次，而补冻那一步可能以两种方式失败，
	// 两者都记在这里：
	//
	//   - **写库失败**：补冻的存储写入报错（库在报错）；
	//   - **算不出价**：按结算口径取规则 / 编译 / 求值那一步失败（定价侧坏了）。
	//
	// 两类都归到这一个计数：它们的后果是同一个（这一次没冻上），分的是**怎么
	// 处置**——而「是哪一类」由日志里那条带 request_id 的告警给出（它把失败那
	// 个 error 原样记下来），这个计数器只答「发生了多少次」。把两类拆成两个
	// 计数器是另一条路，但告警方向本来就该按日志里的 error 判，这里先不拆。
	//
	// 两种失败都不中断那条流——捂不住的那一侧是平台自己的问题，不该让用户的
	// 正常请求失败——但这次请求从此是「未充分冻结」的：之后产出的那些 token
	// 没有对应的冻结额，结算到它们时实际金额会超过预留额（撞 Capped，差额由
	// 平台承担）。
	//
	// 按设计它应当一直是 0。非 0 说明我们（库或定价侧）在拒绝补冻，而**这一次
	// 请求的账已经对不上预留了**——它是那件事唯一的即时信号（对账看不到它：
	// 撞上界时两边记的是同一个被截小的数，账目仍然是平的）。
	//
	// 要查「是哪一次请求、哪一类失败」，看日志：补冻失败那一刻有一条带
	// request_id 的告警（见 gateway 的 streamFreeze.observe）。这个数答的是
	// 「发生了多少次」，与 CappedSettlements 和 computed_amount 的分工一样。
	StreamUnderfrozen *Counter

	// StreamBalanceExhausted 是因可用余额不足被**我们切断**的流式请求次数。
	//
	// 补冻够不着余额时这条流不再继续：中断就是中断，本轮不引入透支（见 #498
	// 的「不做的事」）。调用方那一侧看到一条流内错误帧，它的文案说的是充值而
	// 不是重试。
	//
	// 它取代了原先的那个形状：一条 max_tokens 填得很大、实际只产几十个 token
	// 的流式请求，在开始之前就被按声明上界冻住而遭 429 拒掉——而拒绝率上看不出
	// 被拒的是真没钱还是被预留冻住了（见 issue 的「后果」一节）。现在这一类
	// 请求能跑起来，真正跑不完的那些计数在这里。
	StreamBalanceExhausted *Counter

	// AuthFailures 是认证失败被回以 401 的次数，按失败原因分。
	//
	// reason 的取值来自 internal/auth 的 Reason（八个取值，闭合枚举），
	// 外加一个 unclassified——那是「回了 401 但取不到 auth.Error」。
	// 后者是**接线缺陷的信号**，按设计不该出现任何一条序列。
	//
	// 这里**不**按 key 或用户分：401 是攻击者可控的，拿垃圾 key 打就能
	// 刷，而标签值必须来自有限集合。这条路径上也不写日志——同一个理由。
	// 代价是「是哪一把 key」答不了，那要靠报障的人给前缀去库里定位。
	AuthFailures *LabeledCounter

	// RateLimitExact 是「RPM 限流的计数当前取自共享存储」这个信号：
	// 1 表示精确（计数在 Redis 里，各实例共用一份，放行量等于配置值），
	// 0 表示近似（退回进程内计数，各实例各算各的，放行量偏大）。
	//
	// 它是一个**仪表**而不是计数器：这个问题的答案是「现在是什么」，
	// 而不是「发生过几次」——计数器答不了「现在」，而采集端拿 rate()
	// 去推的次数阈值在没有流量的窗口里会断掉。
	//
	// 它不区分「配置里就没写共享存储」与「配了但连不上」：那两件事由
	// RateLimitStoreErrors 分开（前者那个计数器不涨）。两个数要连着读。
	RateLimitExact *Gauge

	// RateLimitStoreErrors 是共享计数存储（Redis）调用失败的次数。
	//
	// 每一次因为存储不可用而退回进程内计数的判定记一次。它与
	// RateLimitExact 是一对：仪表回答「现在准不准」，这个数回答「有没有
	// 在失败、失败了多少次」。**仪表是 0 而它一直不动**，说明这个部署
	// 本来就没配共享存储（配置如此）；**两者同时在动**说明配了但连不上
	// （故障）——排查方向完全不同。
	RateLimitStoreErrors *Counter

	families []family
}

// NewRegistry 造一个空的注册表，并登记本包声明的全部计数器。
func NewRegistry() *Registry {
	r := &Registry{}
	r.Settlements = r.newLabeled("larkax_billing_settlements_total",
		"按实际用量结算完成的次数，按用量来源分（measured / tokenized / estimated / unreported）。unreported 表示上游响应里没有 usage 块，用量是零值占位。",
		"usage_source")
	r.CappedSettlements = r.newLabeled("larkax_billing_capped_settlements_total",
		"结算金额被预留上界截住的次数，按入口协议分（openai / anthropic / openai-responses / systemone）。按设计应一直为 0；某一协议持续大于 0 说明那条协议的预留上界算错了，差额由平台承担而账目仍然是平的。",
		"protocol")
	r.CappedAmount = r.newLabeled("larkax_billing_capped_amount_total",
		"结算被预留上界截住的那部分金额，按入口协议与资产品种分，单位是最小计价单位（money.Scale 位定点，1e-8 元）。与 capped_settlements_total 是一对：那个数次数，这个数少收了多少钱。按设计应一直为 0；非 0 就是平台在这段时间里实际承担掉的金额，账目仍然是平的。",
		"protocol", "asset")
	r.UnbilledSettlements = r.newCounter("larkax_billing_unbilled_settlements_total",
		"未计费的结算次数：上游失败，或请求在结算之前中止。")
	r.StreamUnderfrozen = r.newCounter("larkax_billing_stream_underfrozen_total",
		"流式请求补冻时存储写失败而放行、因而未被充分冻结的次数。按设计应一直为 0；非 0 说明库在拒绝写入，而这次请求之后产出的 token 没有对应的冻结额，结算到它们时会撞上界（差额由平台承担，账目仍然是平的）。")
	r.StreamBalanceExhausted = r.newCounter("larkax_billing_stream_balance_exhausted_total",
		"因可用余额不足以补冻而被切断的流式请求次数。这一类请求在调用方那一侧看到一条流内错误帧（文案指向充值而不是重试），已产出的部分照常结算。")
	r.AuthFailures = r.newLabeled("larkax_auth_failures_total",
		"认证失败被回以 401 的次数，按失败原因分（取值见 internal/auth 的 Reason）。reason=\"unclassified\" 说明有一条 401 不是由 auth.Error 产生的，按设计不该出现。",
		"reason")
	r.RateLimitExact = r.newGauge("larkax_ratelimit_exact",
		"RPM 限流的计数当前是否取自共享存储：1 表示精确（计数在 Redis 里，各实例共用一份，放行总量不超过配置值），0 表示近似（退回进程内计数，各实例各算各的，放行量偏大）。配置里没写共享存储时它一直是 0，且 store_errors 不涨；配了但连不上时两者同时在动。只有设置过之后才输出：这行不存在表示这个实例上根本没接限流器（例如 worker 角色）。")
	r.RateLimitStoreErrors = r.newCounter("larkax_ratelimit_store_errors_total",
		"共享计数存储（Redis）调用失败的次数。每发生一次就有一个判定退回进程内计数，也就是上面那个仪表变成 0 的原因。")
	return r
}

func (r *Registry) newCounter(name, help string) *Counter {
	c := &Counter{name: name}
	r.families = append(r.families, family{
		name:    name,
		typ:     "counter",
		help:    help,
		samples: func() []sample { return []sample{{value: strconv.FormatUint(c.v.Load(), 10)}} },
	})
	return c
}

func (r *Registry) newGauge(name, help string) *Gauge {
	g := &Gauge{name: name}
	r.families = append(r.families, family{
		name:    name,
		typ:     "gauge",
		help:    help,
		samples: g.samples,
	})
	return g
}

func (r *Registry) newLabeled(name, help string, labels ...string) *LabeledCounter {
	c := &LabeledCounter{name: name, labels: labels, series: map[string]*Counter{}}
	r.families = append(r.families, family{
		name:    name,
		typ:     "counter",
		help:    help,
		samples: c.samples,
	})
	return c
}

// ContentTypeMetrics 是 Prometheus 文本格式的媒体类型。
//
// 版本号是格式的一部分，采集器按它决定怎么解析；写错或漏写会被当成
// 未知格式，而那种失败是「抓到了但解析不出东西」。
const ContentTypeMetrics = "text/plain; version=0.0.4; charset=utf-8"

// Render 按 Prometheus 文本格式写出全部指标。
//
// 一条序列都没有的指标整族不输出：带标签的计数器在第一次自增之前没有取值，
// 凭空输出一行 0 会让人以为它已经接上了。不带标签的计数器则总是输出，
// 包括 0——「一直是 0」本身就是要看的那个值（见 docs/metrics.md）。
// 仪表按它自己那一套：没被设置过就不输出（见 Gauge）。
func (r *Registry) Render(w io.Writer) error {
	for _, f := range r.families {
		samples := f.samples()
		if len(samples) == 0 {
			continue
		}
		if _, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n",
			f.name, escapeHelp(f.help), f.name, f.typ); err != nil {
			return err
		}
		for _, s := range samples {
			if _, err := fmt.Fprintf(w, "%s%s %s\n", f.name, s.labels, s.value); err != nil {
				return err
			}
		}
	}
	return nil
}

// Handler 返回抓取这个注册表的 HTTP handler。
//
// 只接受 GET 与 HEAD：抓取器只用这两个，放行别的等于凭空多出一条写路径。
//
// 先渲染进缓冲区再写出去：渲染中途出错时不会已经写了一半正文，也就
// 不会留下一个「200 但内容截断」的响应——那种响应在采集器那边表现为
// 解析错误，而排查方向会指向采集器。
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "指标端点只接受 GET 与 HEAD", http.StatusMethodNotAllowed)
			return
		}

		var buf bytes.Buffer
		if err := r.Render(&buf); err != nil {
			http.Error(w, "渲染指标失败", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", ContentTypeMetrics)
		w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
		_, _ = w.Write(buf.Bytes())
	})
}

// RegisterMetricsHandler 把 /metrics 注册到 mux 上。
//
// 它只该挂在**内部**监听上：这个端点面向编排系统与抓取器，挂到公共端口
// 等于把内部状态暴露给所有能发推理请求的人。公共端口的路径是全捕获的
// （见 cmd/larkax 的 notImplemented），所以挂错了不会 404，只会安静地
// 从另一个端口回一份 501。
func RegisterMetricsHandler(mux *http.ServeMux) {
	mux.Handle("/metrics", Metrics.Handler())
}

// escapeHelp 转义 HELP 文本里的反斜杠与换行。
//
// 不转义的话，说明里出现一个换行就会让下一行被当成一条新的指标行——
// 那要么让采集器解析失败，要么读进一条根本不存在的序列。说明文字里
// 排版换行是很自然的写法，所以这里必须转义，而不是靠约定不写换行。
func escapeHelp(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(s)
}

// escapeLabelValue 转义标签值里的反斜杠、双引号与换行。
//
// 双引号不转义会让标签值提前闭合，剩下的部分变成语法错误。
func escapeLabelValue(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}
