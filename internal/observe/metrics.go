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

// 本文件是这套服务的最小指标面：手写的计数器，按 Prometheus 的文本格式暴露。
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
//	observe.Metrics.CappedSettlements.Inc()
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

// Value 返回当前值。它只用于测试与断言，不参与渲染之外的逻辑。
func (c *Counter) Value() uint64 { return c.v.Load() }

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
		out = append(out, sample{labels: c.renderLabels(key), value: ct.v.Load()})
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

// sample 是一条待渲染的样本：一组标签值，加一个数值。
type sample struct {
	// labels 是渲染好的 {k="v"}；没有标签时为空串。
	labels string
	value  uint64
}

// family 是注册表里的一条指标：名字、说明，以及怎么取它当前的样本。
type family struct {
	name    string
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

	// CappedSettlements 是结算金额被预留上界截住的次数。
	//
	// 按设计它应当一直是 0。持续大于 0 说明预留上界算错了，差额由平台
	// 承担，而**账目仍然是平的**——对账看不出来，只有这个数能看见。
	CappedSettlements *Counter

	// UnbilledSettlements 是未计费的结算次数。
	//
	// 两个原因都走这里：上游失败，以及请求在结算之前就中止了
	// （见 pipeline.Outcome.UpstreamFailed）。在这一层分不出是哪一个。
	UnbilledSettlements *Counter

	families []family
}

// NewRegistry 造一个空的注册表，并登记本包声明的全部计数器。
func NewRegistry() *Registry {
	r := &Registry{}
	r.Settlements = r.newLabeled("larkax_billing_settlements_total",
		"按实际用量结算完成的次数，按用量来源分（measured / tokenized / estimated）。",
		"usage_source")
	r.CappedSettlements = r.newCounter("larkax_billing_capped_settlements_total",
		"结算金额被预留上界截住的次数。按设计应一直为 0；持续大于 0 说明预留上界算错了，差额由平台承担而账目仍然是平的。")
	r.UnbilledSettlements = r.newCounter("larkax_billing_unbilled_settlements_total",
		"未计费的结算次数：上游失败，或请求在结算之前中止。")
	return r
}

func (r *Registry) newCounter(name, help string) *Counter {
	c := &Counter{name: name}
	r.families = append(r.families, family{
		name:    name,
		help:    help,
		samples: func() []sample { return []sample{{value: c.v.Load()}} },
	})
	return c
}

func (r *Registry) newLabeled(name, help string, labels ...string) *LabeledCounter {
	c := &LabeledCounter{name: name, labels: labels, series: map[string]*Counter{}}
	r.families = append(r.families, family{
		name:    name,
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
// 没有任何序列的指标整族不输出：带标签的计数器在第一次自增之前没有取值，
// 凭空输出一行 0 会让人以为它已经接上了。不带标签的计数器则总是输出，
// 包括 0——「一直是 0」本身就是要看的那个值（见 docs/metrics.md）。
func (r *Registry) Render(w io.Writer) error {
	for _, f := range r.families {
		samples := f.samples()
		if len(samples) == 0 {
			continue
		}
		if _, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n",
			f.name, escapeHelp(f.help), f.name); err != nil {
			return err
		}
		for _, s := range samples {
			if _, err := fmt.Fprintf(w, "%s%s %s\n",
				f.name, s.labels, strconv.FormatUint(s.value, 10)); err != nil {
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
