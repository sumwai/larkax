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
	for _, f := range r.families {
		if !strings.HasPrefix(f.name, "larkax_") {
			t.Errorf("指标名 %q 没有 larkax_ 前缀", f.name)
		}
		if !strings.HasSuffix(f.name, "_total") {
			t.Errorf("计数器 %q 没有 _total 后缀", f.name)
		}
		if seen[f.name] {
			t.Errorf("指标名 %q 重复了", f.name)
		}
		seen[f.name] = true
		if f.help == "" {
			t.Errorf("指标 %q 没有说明", f.name)
		}
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
