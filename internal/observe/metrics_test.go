package observe

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func render(t *testing.T, r *Registry) string {
	t.Helper()
	var buf bytes.Buffer
	if err := r.Render(&buf); err != nil {
		t.Fatalf("渲染失败: %v", err)
	}
	return buf.String()
}

// 逐字钉住渲染结果：格式是**对外契约**，采集器按它解析，
// 所以它不能是「大概长这样」。
//
// 这条用例同时钉住三件容易漂移的事：不带标签的计数器总是输出（包括 0）、
// 一条序列都没有的指标整族不输出、以及多次渲染的顺序稳定（map 的迭代
// 顺序会让输出每次都不一样，而「抓两次 diff 一下」是排查时的第一步）。
func TestRenderPrometheusTextFormat(t *testing.T) {
	r := &Registry{}
	plain := r.newCounter("test_plain_total", "一条说明")
	labeled := r.newLabeled("test_labeled_total", "带标签的说明", "kind")
	r.newCounter("test_zero_total", "从没自增过")

	plain.Inc()
	plain.Inc()
	labeled.With("b").Inc()
	labeled.With("a").Inc()
	labeled.With("a").Inc()

	want := `# HELP test_plain_total 一条说明
# TYPE test_plain_total counter
test_plain_total 2
# HELP test_labeled_total 带标签的说明
# TYPE test_labeled_total counter
test_labeled_total{kind="a"} 2
test_labeled_total{kind="b"} 1
# HELP test_zero_total 从没自增过
# TYPE test_zero_total counter
test_zero_total 0
`
	got := render(t, r)
	if got != want {
		t.Errorf("渲染结果不符\n得到:\n%s\n期望:\n%s", got, want)
	}
	if again := render(t, r); again != got {
		t.Errorf("两次渲染结果不一致\n第一次:\n%s\n第二次:\n%s", got, again)
	}
}

// 从没自增过的带标签计数器不输出任何行——连 HELP 与 TYPE 都不输出。
//
// 这条与上一条里「不带标签的计数器输出 0」是同一个决定的两面：
// 带标签的计数器在第一次自增之前没有取值可言，凭空写一行 0 会让人以为
// 它已经接上了；而不带标签的那一条，「一直是 0」恰恰是要看的那个值。
func TestLabeledCounterWithoutSeriesRendersNothing(t *testing.T) {
	r := &Registry{}
	r.newLabeled("test_labeled_total", "带标签的说明", "kind")

	if got := render(t, r); got != "" {
		t.Errorf("没有序列时不该输出任何行，得到:\n%s", got)
	}
}

// 说明文字与标签值里的换行、反斜杠、双引号必须被转义。
//
// 不转义的话，说明里的一个换行会让下一行被当成一条新的指标行；
// 标签值里的一个双引号会让它提前闭合，剩下的部分变成语法错误。
func TestHelpAndLabelValuesAreEscaped(t *testing.T) {
	r := &Registry{}
	help := "第一行\n第二行 反斜杠\\ 双引号\""
	labeled := r.newLabeled("test_labeled_total", help, "kind")
	labeled.With("带\"引号\\和\n换行").Inc()

	got := render(t, r)

	wantHelp := `# HELP test_labeled_total 第一行\n第二行 反斜杠\\ 双引号"`
	if !strings.Contains(got, wantHelp) {
		t.Errorf("HELP 未按格式转义\n得到:\n%s\n期望含:\n%s", got, wantHelp)
	}
	wantSample := `test_labeled_total{kind="带\"引号\\和\n换行"} 1`
	if !strings.Contains(got, wantSample) {
		t.Errorf("标签值未按格式转义\n得到:\n%s\n期望含:\n%s", got, wantSample)
	}
	// 转义之后的正文里不该再出现真的换行：出现了就说明转义漏了。
	for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		if strings.HasPrefix(line, "test_labeled_total{") && strings.Count(line, "{") != 1 {
			t.Errorf("样本行被换行切断了: %q", line)
		}
	}
}

// 标签值的个数与声明的标签名个数对不上是编码错误，必须当场炸掉：
// 静默接受的话，现场是「多出一条没人认识的序列」。
func TestLabeledCounterRejectsWrongLabelCount(t *testing.T) {
	r := &Registry{}
	labeled := r.newLabeled("test_labeled_total", "说明", "kind", "result")

	defer func() {
		if recover() == nil {
			t.Error("标签值个数不对时应当 panic")
		}
	}()
	labeled.With("只有一个")
}

// 渲染与自增会同时发生：抓取随时可能来，而增量在请求路径上。
// 这条用例在 -race 下跑，靠的是 race detector 而不是断言。
func TestCounterSurvivesConcurrentIncrements(t *testing.T) {
	r := &Registry{}
	c := r.newCounter("test_total", "说明")
	labeled := r.newLabeled("test_labeled_total", "说明", "kind")

	const goroutines, perGoroutine = 8, 100
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				c.Inc()
				labeled.With("same").Inc()
			}
		}()
	}
	wg.Wait()

	if got := c.Value(); got != goroutines*perGoroutine {
		t.Errorf("计数器 = %d，期望 %d", got, goroutines*perGoroutine)
	}
	if got := labeled.With("same").Value(); got != goroutines*perGoroutine {
		t.Errorf("带标签的计数器 = %d，期望 %d", got, goroutines*perGoroutine)
	}
}

func TestMetricsHandlerAcceptsGetAndHead(t *testing.T) {
	// 用本地注册表而不是全局那一份：这条用例只关心 handler 的行为，
	// 往全局计数器上加数会让别的用例看到不该有的初值。
	r := &Registry{}
	r.newCounter("test_total", "说明").Inc()

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), method, "/metrics", nil))

			if rec.Code != http.StatusOK {
				t.Fatalf("状态码 = %d，期望 200", rec.Code)
			}
			if got := rec.Header().Get("Content-Type"); got != ContentTypeMetrics {
				t.Errorf("Content-Type = %q，期望 %q", got, ContentTypeMetrics)
			}
			if method == http.MethodGet && !strings.Contains(rec.Body.String(), "test_total 1") {
				t.Errorf("正文里没有样本行:\n%s", rec.Body.String())
			}
		})
	}
}

func TestMetricsHandlerRejectsWrites(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			rec := httptest.NewRecorder()
			NewRegistry().Handler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), method, "/metrics", nil))

			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("状态码 = %d，期望 405", rec.Code)
			}
			if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
				t.Errorf("Allow = %q，期望 %q", got, "GET, HEAD")
			}
		})
	}
}

// 本包声明的指标名要符合 docs/metrics.md 里写下的约定。
//
// 约定写在文档里会随实现漂移：半年后有人加了一条不带前缀的指标，没人会
// 发现，直到采集端多出一堆没归类的东西。所以把它写成可执行的检查。
func TestRegistryMetricNamesFollowConvention(t *testing.T) {
	r := NewRegistry()
	if len(r.families) == 0 {
		t.Fatal("注册表里一条指标都没有——检查范围写错了")
	}

	seen := map[string]bool{}
	types := map[string]int{}
	for _, f := range r.families {
		if !strings.HasPrefix(f.name, "larkax_") {
			t.Errorf("指标名 %q 没有 larkax_ 前缀", f.name)
		}
		// 后缀与类型绑在一起：计数器以 _total 结尾，仪表**不**以它结尾。
		// 两者混起来时，采集端按名字推断类型会得到错的（`_total` 在
		// Prometheus 的约定里就是「只增不减」），而一个可升可降的数被当成
		// 计数器，rate() 与 increase() 会给出没有意义的结论。
		switch f.typ {
		case "counter":
			types["counter"]++
			if !strings.HasSuffix(f.name, "_total") {
				t.Errorf("计数器 %q 没有 _total 后缀", f.name)
			}
		case "gauge":
			types["gauge"]++
			if strings.HasSuffix(f.name, "_total") {
				t.Errorf("仪表 %q 带了 _total 后缀：那个后缀在 Prometheus 的约定里是「只增不减」", f.name)
			}
		default:
			t.Errorf("指标 %q 的类型是 %q，本包只声明 counter 与 gauge", f.name, f.typ)
		}
		if seen[f.name] {
			t.Errorf("指标名 %q 重复了", f.name)
		}
		seen[f.name] = true
		if f.help == "" {
			t.Errorf("指标 %q 没有说明", f.name)
		}
	}
	// 两种类型都要扫到：只声明计数器时上面的 gauge 分支一次都没执行，
	// 而「一条都没扫到」与「扫到的全对」在输出上长得一样（见 LESSONS.md 第 16 条）。
	if types["counter"] == 0 || types["gauge"] == 0 {
		t.Errorf("扫到 counter %d 条、gauge %d 条，两种类型都应当有", types["counter"], types["gauge"])
	}
}

// 仪表在渲染上有两条与计数器不同的性质，都要钉住。
//
// 一是类型行写 gauge：写错时采集端会把它当计数器，而对一个可升可降的数
// 用 rate() 得出的结论没有意义。二是**没设置过就整族不输出**——与不带标签的
// 计数器「总是输出、包括 0」相反，因为这里的 0 是一个有意义的取值
// （限流那个仪表上 0 表示「近似」），于是「没接上」与「值是 0」必须分得开。
func TestGaugeRendersOnlyAfterBeingSet(t *testing.T) {
	r := &Registry{}
	g := r.newGauge("test_level", "仪表说明")

	if got := render(t, r); strings.Contains(got, "test_level") {
		t.Errorf("没设置过的仪表被输出了：\n%s", got)
	}

	g.Set(0)
	want := "# HELP test_level 仪表说明\n# TYPE test_level gauge\ntest_level 0\n"
	if got := render(t, r); got != want {
		t.Errorf("设置成 0 之后渲染结果不符\n得到:\n%s\n期望:\n%s", got, want)
	}

	// 负值要按有符号渲染。把它当成无符号时 -1 会渲染成 18446744073709551615，
	// 那是一条看起来完全正常、含义正相反的序列。
	g.Set(-1)
	if got := render(t, r); !strings.Contains(got, "test_level -1\n") {
		t.Errorf("负值没有按有符号渲染：\n%s", got)
	}

	// Add 也算设置过：先把值加上去、再标记，读的人不会看到「标记了但值是初值」。
	r2 := &Registry{}
	g2 := r2.newGauge("test_delta", "说明")
	g2.Add(5)
	g2.Add(-2)
	if got := render(t, r2); !strings.Contains(got, "test_delta 3\n") {
		t.Errorf("Add 之后渲染结果里没有 test_delta 3：\n%s", got)
	}
}

// 声明了字段却忘了登记进注册表的计数器会**安静地不输出**，
// 与「根本没接上」长得一模一样。这条用例按字段清单逐个对照。
func TestEveryDeclaredCounterIsRegistered(t *testing.T) {
	r := NewRegistry()
	registered := map[string]bool{}
	for _, f := range r.families {
		registered[f.name] = true
	}

	rv := reflect.ValueOf(r).Elem()
	fields := 0
	for i := 0; i < rv.NumField(); i++ {
		f := rv.Field(i)
		if !f.CanInterface() {
			continue
		}
		switch c := f.Interface().(type) {
		case *Counter:
			fields++
			if !registered[c.name] {
				t.Errorf("字段 %s 的计数器 %q 没有登记进注册表，它不会被输出",
					rv.Type().Field(i).Name, c.name)
			}
		case *LabeledCounter:
			fields++
			if !registered[c.name] {
				t.Errorf("字段 %s 的计数器 %q 没有登记进注册表，它不会被输出",
					rv.Type().Field(i).Name, c.name)
			}
		case *Gauge:
			fields++
			if !registered[c.name] {
				t.Errorf("字段 %s 的仪表 %q 没有登记进注册表，它不会被输出",
					rv.Type().Field(i).Name, c.name)
			}
		}
	}
	if fields == 0 {
		t.Error("一个计数器字段都没扫到——检查范围写错了")
	}
}

// 认证失败的原因标签也是对外契约：采集端的告警规则按它写。
//
// 用独立注册表而不是包级那份：这条只关心指标的形状，而往包级计数器上加数
// 会让别的用例看到不该有的初值。至于「增量点有没有按 Reason 落进对的序列」，
// 那在增量点所在的那个包里钉（见 internal/api 的
// TestAuthFailureReasonIsCountedPerReason）——注册表不在那边的构造点手里。
func TestAuthFailureMetricShape(t *testing.T) {
	r := NewRegistry()
	r.AuthFailures.With("key_expired").Inc()
	r.AuthFailures.With("key_not_found").Inc()

	got := render(t, r)
	for _, want := range []string{
		`larkax_auth_failures_total{reason="key_expired"} 1`,
		`larkax_auth_failures_total{reason="key_not_found"} 1`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("渲染结果里没有 %s\n得到:\n%s", want, got)
		}
	}
}

// 生产注册表里的标签名与取值也是对外契约：采集端的查询按它们写。
func TestRegistryLabelsAreStable(t *testing.T) {
	r := NewRegistry()
	r.Settlements.With("measured").Inc()
	r.CappedSettlements.With("systemone").Inc()
	r.CappedAmount.With("systemone", "CNY").Add(9_800_000)

	got := render(t, r)
	for _, want := range []string{
		`larkax_billing_settlements_total{usage_source="measured"} 1`,
		// capped 按入口协议分：告警与排查按 protocol 筛，标签名与取值都不能漂。
		`larkax_billing_capped_settlements_total{protocol="systemone"} 1`,
		// 金额侧那条多一个 asset：协议说「哪条入口的上界错了」，资产说
		// 「少收的是哪一种钱」。取值同样来自闭合枚举（channel.Protocol 与
		// ledger.Asset），采集端的查询按它们写。
		`larkax_billing_capped_amount_total{protocol="systemone",asset="CNY"} 9800000`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("标签名或取值变了\n得到:\n%s\n期望含:\n%s", got, want)
		}
	}

	// 不带标签的计数器输出的样本行里不该出现花括号。
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "larkax_billing_unbilled_settlements_total{") {
			t.Errorf("这条计数器不该有标签: %q", line)
		}
	}
}

// ── samples 排序比较符的等价论证 ─────────────────────────────────────────
//
// LabeledCounter.samples（metrics.go:159）里的 `sort.Slice` 比较符
// `<` 改成 `<=` 是一个**等价**变异，不是缺口，所以这里没有为它补用例。
// 前提与推理写在这里备查：
//
//   - 比较键只有 sample.labels 一个，候选项来自 c.series 这个 map：
//     每条序列一个键，键互不相同。
//   - 键由 strings.Join(values, "\x00") 得到，renderLabels 再按 "\x00" 拆回去，
//     经 escapeLabelValue 转义后拼成 `{name="value",...}`。转义本身不碰撞
//     （反斜杠、双引号、换行各有不同的替身），所以在**标签值不含 \x00**
//     的前提下，不同的键一定渲染出不同的串——参与比较的两个 labels 恒不
//     相等，两版给出的顺序完全一致。
//
// 标签值里带 \x00 时前面那条前提不成立：With 不禁止这种值，而按分隔符拆回去
// 会丢掉后半段（With("a\x00b") 与 With("a") 都渲染成 {kind="a"}）。即便这样，
// 两版之间能差出来的也只有「渲染相同的两条样本谁先谁后」：sort.Slice 本身
// 不保证稳定，相等键的先后不是它对外承诺的一部分；而那些候选项是按 map
// 迭代顺序给出的，每次渲染都可能不同，两版都一样。所以没有确定性的断言能
// 把两版分开，补用例改变不了这一格。
//
// 本包声明的标签值取自闭合枚举（usage_source / protocol / reason / asset，
// 见 docs/metrics.md），没有一个含 \x00，前提在实际数据上成立。
