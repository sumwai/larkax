package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sumwai/larkax/internal/api"
	"github.com/sumwai/larkax/internal/assembly"
	"github.com/sumwai/larkax/internal/channel"
	"github.com/sumwai/larkax/internal/config"
	"github.com/sumwai/larkax/internal/money"
	"github.com/sumwai/larkax/internal/observe"
	"github.com/sumwai/larkax/internal/pipeline"
	"github.com/sumwai/larkax/internal/ratelimit"
	"github.com/sumwai/larkax/internal/tokens"
)

// worker 角色的公共端口是占位响应，而不是装配失败：它本来就不承接推理流量，
// 而起不来的进程连「端口通、TLS 通、负载均衡转发通」都验证不了。
func TestWorkerRoleServesPlaceholder(t *testing.T) {
	app, err := publicHandler(context.Background(), &config.Config{Role: config.RoleWorker}, nil, nil, nil)
	if err != nil {
		t.Fatalf("worker 角色不该装配失败: %v", err)
	}
	h := app.Public

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/chat/completions", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotImplemented {
		t.Errorf("状态码 = %d，期望 501", rec.Code)
	}
	// 这个 501 要自己说清来由。与「功能还没接上」的 501 长得一样的话，
	// 看到它的人会去查功能，而真正的问题是部署把 worker 摆在了公共端口上。
	if !strings.Contains(rec.Body.String(), "worker") {
		t.Errorf("占位响应应当点名角色: %s", rec.Body)
	}
}

// 本该接推理流量的角色缺主密钥时必须**启动失败**，而不是静默退化成占位响应：
// 后者在编排系统看来是「起来了」，要等到第一波真实流量才暴露，而现场只是一个
// 与「未实现」无从区分的 501。
func TestTrafficRoleWithoutMasterKeyFailsToStart(t *testing.T) {
	for _, role := range []config.Role{config.RoleGateway, config.RoleControl} {
		t.Run(string(role), func(t *testing.T) {
			app, err := publicHandler(context.Background(), &config.Config{Role: role}, nil, nil, nil)
			if err == nil {
				t.Fatalf("%s 角色缺主密钥时应当装配失败，实际拿到 %v", role, app)
			}
			if !strings.Contains(err.Error(), "主密钥") {
				t.Errorf("错误信息应当说明缺的是主密钥: %v", err)
			}
		})
	}
}

// 预热的判据不是「调过 Prepare」，而是**预热之后词表真的在内存里**。
//
// 只断言调用发生了的话，把 Prepare 换成一句空实现、或者让它在某个角色上
// 被跳过，用例照样通过——而那时第一个请求仍然要多付约 25ms。
func TestWarmVocabLoadsVocab(t *testing.T) {
	counter := tokens.NewCounter()
	var logs bytes.Buffer
	warmVocab(context.Background(), counter, slog.New(slog.NewTextHandler(&logs, nil)))

	loaded := counter.Loaded()
	for _, want := range []string{"o200k_base", "cl100k_base"} {
		if !slices.Contains(loaded, want) {
			t.Errorf("预热之后 %s 不在 Loaded() 里: %v", want, loaded)
		}
	}
	// 日志是这条链路上唯一的现场：「预热了」与「预热失败后退到惰性加载」
	// 在行为上没有区别，只在日志里有。
	if !strings.Contains(logs.String(), "词表已预热") {
		t.Errorf("启动日志里没有预热的记录: %s", logs.String())
	}
}

// 预热失败只记日志，不拦住启动。
//
// 不预热只是慢，不是不能用（词表会惰性加载）。这条用例同时钉住两件事：
// 日志里留下痕迹（否则「这台机器上预热没成功」是静默的），
// 以及计数在预热失败之后**仍然可用**。
func TestWarmVocabFailureStillServes(t *testing.T) {
	counter := tokens.NewCounter()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var logs bytes.Buffer
	warmVocab(ctx, counter, slog.New(slog.NewTextHandler(&logs, nil)))

	if !strings.Contains(logs.String(), "词表预热失败") {
		t.Errorf("取消后的预热应当在日志里留下痕迹: %s", logs.String())
	}
	// 判据取「一种都没加载上」而不是「没崩」：后者对空实现也成立。
	if loaded := counter.Loaded(); len(loaded) != 0 {
		t.Errorf("取消之后不该有已加载编码, 得到 %v", loaded)
	}
	got, err := counter.CountText(context.Background(), "gpt-4o", "hello world")
	if err != nil {
		t.Fatalf("预热失败后计数仍应可用: %v", err)
	}
	if got.Tokens == 0 {
		t.Error("预热失败后算出的 token 数为 0")
	}
}

// ruleSource 是一条固定的计价规则：每 token 输入 1e-6 元、输出 2e-6 元。
//
// 这里测的是计数，不是规则匹配，所以来源是写死的。
const ruleSource = `{"prompt": 0.000001, "completion": 0.000002}`

type stubRules struct{}

func (stubRules) Match(context.Context, pipeline.RuleQuery) (pipeline.Rule, error) {
	return pipeline.Rule{ID: 1, Version: 1, Source: ruleSource}, nil
}

func (stubRules) MatchAll(ctx context.Context, q pipeline.RuleQuery) ([]pipeline.Rule, error) {
	rule, err := stubRules{}.Match(ctx, q)
	if err != nil {
		return nil, err
	}
	return []pipeline.Rule{rule}, nil
}

func settleRequest() pipeline.Request {
	return pipeline.Request{
		RequestID:      "req-metrics",
		UserID:         42,
		CanonicalModel: "test-model",
		Candidates:     []pipeline.RuleCandidate{{ChannelID: 7, OwnerUserID: 9}},
		ChannelID:      7,
		OwnerUserID:    9,
		InputTokens:    1000,
		MaxTokens:      500,
		// 资产品种由入口从运行期配置填（见 pipeline.Request.Asset），
		// 计费层不再兜默认值。夹具给一个具体的值，与生产的形状一致。
		Asset: "CNY",
		// 协议由入口填（见 pipeline.Request.Protocol）。夹具给一条具体的，
		// 与生产的形状一致：capped 计数器按它分序列，空值会造出一条生产上
		// 不该有的序列。
		Protocol: channel.ProtocolOpenAI,
	}
}

// settleOutcome 是一次正常的结算结果：输入 1000 token，按规则值 0.001 元。
func settleOutcome() pipeline.Outcome {
	return pipeline.Outcome{
		Usage:       channel.Usage{PromptTokens: 1000},
		UsageSource: tokens.SourceMeasured,
	}
}

// scrape 走**生产用的那一份**内部端点装配抓一次 /metrics。
func scrape(t *testing.T) []string {
	t.Helper()
	rec := httptest.NewRecorder()
	newInternalHandler(nil, nil).ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("抓 /metrics 得到状态码 %d，期望 200；正文:\n%s", rec.Code, rec.Body.String())
	}
	// 媒体类型是格式契约的一部分：写错或漏写会被采集器当成未知格式，
	// 而那种失败是「抓到了但解析不出东西」，排查方向会指向采集器。
	if got := rec.Header().Get("Content-Type"); got != observe.ContentTypeMetrics {
		t.Errorf("Content-Type = %q，期望 %q", got, observe.ContentTypeMetrics)
	}
	return strings.Split(strings.TrimSuffix(rec.Body.String(), "\n"), "\n")
}

// assertLine 断言某一**整行**存在。
//
// 不用 strings.Contains：那样 "…_total 3" 会被 "…_total 30" 里的前缀命中，
// 一个错掉的数于是看起来是对的。
func assertLine(t *testing.T, lines []string, want string) {
	t.Helper()
	for _, line := range lines {
		if line == want {
			return
		}
	}
	t.Errorf("指标里没有这一行: %q\n实际输出:\n%s", want, strings.Join(lines, "\n"))
}

// settlementCounts 是这一条用例关心的三个计数的取值。
type settlementCounts struct {
	capped   uint64
	unbilled uint64
	measured uint64
}

// snapshotSettlementCounts 读一遍**本进程那一份**全局注册表。
//
// pipeline 的增量点写死 observe.Metrics，/metrics 端点渲染的也是它，
// 所以这条用例只能用这一份。读 With 会顺带把序列建出来（值为 0），
// 但那条序列本来就会被下面这几次结算建出来，不改变被测的行为。
func snapshotSettlementCounts() settlementCounts {
	return settlementCounts{
		capped:   observe.Metrics.CappedSettlements.With(string(channel.ProtocolOpenAI)).Value(),
		unbilled: observe.Metrics.UnbilledSettlements.Value(),
		measured: observe.Metrics.Settlements.With("measured").Value(),
	}
}

// 这一条钉的是**整条链**：真实的生产增量点（pipeline.Settle）→ 计数器 →
// 内部监听上的 /metrics 输出。
//
// 只断言「响应里有 larkax_ 开头的行」是不够的：那样把计数器改成不自增、
// 把标签名拼错、把端点挂到别的监听上，用例都照样通过——而这几件事正是
// 这一轮要防的（一个一直是 0 的计数器与一个根本没接的计数器长得一样）。
//
// 断言落在**增量**上而不是绝对值。计数器是进程级的全局状态，而这条用例
// 不是它唯一的写入者：`-count=2` 的第二轮、以及将来任何一条也走 Settle
// 的用例，都会把起点抬起来。绝对值断言于是变成「这个进程里跑过几次」的
// 函数——要么随机变红，要么得配一句「先确认起点是 0」把自己限定成只能跑
// 一次，而那句守卫挂掉时指向的是「别的用例动过计数器」，不是被测的那条链。
func TestMetricsEndpointReportsRealSettlementCounts(t *testing.T) {
	before := snapshotSettlementCounts()

	p := &pipeline.Pipeline{Rules: stubRules{}}
	// 预留只有 1 个最小单位，而按规则算出来是 0.001 元——必然撞上上界。
	//
	// RuleMatchedAt 是必填的：结算按**受理那一刻**的档位定价，而不是结算
	// 这一刻的墙钟，所以缺了它 Settle 会直接拒绝（这条预留没有「哪一刻的价」
	// 可言）。真实的预留由 Reserve 填上，这里手工构造，就得自己给。
	reserved := &pipeline.Reservation{
		RequestID:     "req-metrics",
		Amount:        money.FromUnits(1),
		RuleMatchedAt: time.Now(),
	}

	for i := 1; i <= 3; i++ {
		if _, err := p.Settle(context.Background(), settleRequest(), reserved, settleOutcome()); err != nil {
			t.Fatalf("第 %d 次结算失败: %v", i, err)
		}
	}
	// 一次上游失败：不计费，但必须留下一个数。
	if _, err := p.Settle(context.Background(), settleRequest(), reserved, pipeline.Outcome{
		UpstreamFailed: true,
		UpstreamStatus: http.StatusBadGateway,
		ErrorCode:      "upstream_error",
	}); err == nil {
		t.Fatal("上游失败时 Settle 应当返回错误")
	}

	// 3 次正常结算各撞一次预留上界，各记一次用量来源；1 次上游失败不计费，
	// 但要留下一个数。这些增量是这条用例真正要钉的东西。
	lines := scrape(t)
	for _, want := range []string{
		"# TYPE larkax_billing_capped_settlements_total counter",
		fmt.Sprintf(`larkax_billing_capped_settlements_total{protocol="openai"} %d`, before.capped+3),
		fmt.Sprintf("larkax_billing_unbilled_settlements_total %d", before.unbilled+1),
		fmt.Sprintf(`larkax_billing_settlements_total{usage_source="measured"} %d`, before.measured+3),
	} {
		assertLine(t, lines, want)
	}
}

// 公共端口上不该有 /metrics。
//
// 这里能拿到的是 worker 角色的公共 handler：gateway 角色那一份由
// internal/assembly 装配，起它要连库，命令行这一层测不动。所以这条用例
// 钉住的是「两个 handler 的装配是分开的」这个结构，而不是某一个角色。
func TestMetricsEndpointIsNotOnPublicHandler(t *testing.T) {
	rec := httptest.NewRecorder()
	workerPlaceholder().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))

	if rec.Code == http.StatusOK {
		t.Errorf("公共端口把 /metrics 服务出去了，状态码 %d；正文:\n%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "larkax_") {
		t.Errorf("公共端口的响应里出现了指标名:\n%s", rec.Body.String())
	}
}

// 健康检查与指标共用同一个内部 handler，抽函数时不能只搬走其中一个。
func TestInternalHandlerServesHealthAndMetrics(t *testing.T) {
	handler := newInternalHandler(nil, nil)

	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil))
			if rec.Code != http.StatusOK {
				t.Errorf("%s 状态码 = %d，期望 200", path, rec.Code)
			}
		})
	}
}

// 限流用量的只读查询挂在内部端口上，而且**只在**内部端口上。
//
// 这条挂错不会 404——公共端口是路径全捕获的，它只会安静地从另一个端口
// 回一份 501（与 /metrics 同一个陷阱），而这条比 /metrics 更敏感：它答的是
// 「某个 Key 这一分钟用了多少」，挂错了等于把它交给每一个能发推理请求的人。
//
// 两半都在这一条里：内部端口要 200，公共端口要 501。
func TestRateLimitUsageIsOnInternalHandlerOnly(t *testing.T) {
	limiter := ratelimit.New(func() time.Time { return time.Date(2026, 10, 17, 12, 0, 30, 0, time.UTC) })
	rpm := 5
	limiter.Allow(7, &rpm)

	app := &assembly.App{RateLimiter: limiter}
	handler := newInternalHandler(nil, app)

	target := api.PathRateLimitUsage + "?key_id=7"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, target, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("内部端口上 %s 的状态码 = %d，期望 200；正文:\n%s", target, rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"count":1`) {
		t.Errorf("响应体里没有用量:\n%s", rec.Body.String())
	}

	// 公共端口：worker 角色那一份就够（它是公共端口的形状），判据是 501。
	rec = httptest.NewRecorder()
	workerPlaceholder().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, target, nil))
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("公共端口上 %s 的状态码 = %d，期望 501", target, rec.Code)
	}

	// 没有限流器的实例（worker 角色）：这条路径不注册。回 404 而不是回一个
	// 「永远是 0」的答案——后者会让查询的人以为那个实例上确实没被打过。
	rec = httptest.NewRecorder()
	newInternalHandler(nil, nil).ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, target, nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("没有限流器时 %s 的状态码 = %d，期望 404", target, rec.Code)
	}
}

// 服务这一侧必须继续校验主密钥：gateway（角色未设置时的默认值）缺它就拒绝启动。
//
// 它与 config.LoadOps 是一对——运维命令的加载入口不校验角色与主密钥，而服务的
// 入口照旧校验。这条用例钉的是**服务的入口**：把 run() 里的 config.Load 换成
// LoadOps 会让它挂，而 internal/config 里那几条用例照不到这一处——它们测的是
// 两个函数各自的行为，不是「谁在用哪一个」。
//
// 上面那条 TestTrafficRoleWithoutMasterKeyFailsToStart 测的是装配层
// （publicHandler），绕过了配置加载；两层都要有，缺哪一层都能被改坏而不报。
func TestServiceRefusesToStartWithoutMasterKey(t *testing.T) {
	// 角色清掉而不是设成 gateway：默认值就是 gateway，而「不设」正是
	// 手工部署时的情形，也是 issue #343 里出错的那一种。
	t.Setenv("LARKAX_ROLE", "")
	t.Setenv("LARKAX_MASTER_KEYS", "")
	// 指向一个没有监听者的端口。配置加载失败时走不到这里；万一配置那一步
	// 被放松了，连库失败也会让 run() 返回，用例不会挂在那里等。
	t.Setenv("LARKAX_DB_DSN", "postgres://larkax:placeholder@127.0.0.1:1/larkax?sslmode=disable")

	err := run()
	if err == nil {
		t.Fatal("gateway 角色缺主密钥时服务不该启动")
	}
	if !strings.Contains(err.Error(), "LARKAX_MASTER_KEYS") {
		t.Errorf("错误信息应当点明缺的是主密钥: %v", err)
	}
}

// syncBuffer 是并发安全的日志落点。
//
// 下面那条用例让服务在后台 goroutine 里打日志、自己在前台读，而 bytes.Buffer
// 并发读写会踩坏数据（CI 带 -race 跑，这里会直接报出来）。日志是那条用例的
// 判据本身，所以它不能被读坏。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write 收下一条日志。
func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String 取当前已经落下的全部日志。
func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// listenTCP 绑一个本地地址，供用例自己占端口 / 借端口。
//
// 走 ListenConfig 而不是 net.Listen：noctx 检查器要求带 context（它只用于
// 解析地址，不影响返回的监听器）。调用方自己决定绑不上时怎么报，所以这里
// 只把错误交出去。
func listenTCP(addr string) (net.Listener, error) {
	var lc net.ListenConfig
	return lc.Listen(context.Background(), "tcp", addr)
}

// busyAddr 返回一个**已经被占住**的地址，并在用例结束时放开。
//
// 拿它去监听必定失败，而且是真实的那种失败（内核说 address already in use），
// 不是造出来的假错误——「绑不上时会怎样」正是这几条用例要测的东西。
func busyAddr(t *testing.T) string {
	t.Helper()
	ln, err := listenTCP("127.0.0.1:0")
	if err != nil {
		t.Fatalf("占端口失败: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().String()
}

// freeAddr 返回一个**当前空闲的具体地址**。
//
// :0 给不出这个地址——内核分配完的端口只存在于监听器里，调用方拿不到，
// 而下面几条用例需要「一个必定绑得上、且事后能再去连它」的具体地址。
// 从放开到用例再绑上之间有理论上的争抢窗口，但真被抢走时用例会直接报
// 「监听失败」，不会静默地测错东西。
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := listenTCP("127.0.0.1:0")
	if err != nil {
		t.Fatalf("借端口失败: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("放开借来的端口失败: %v", err)
	}
	return addr
}

// waitForLog 等到日志里出现 want 为止。
//
// 不等的话，「连不上」既可能是日志说了假话、也可能只是服务还没轮到跑，
// 两者分不开——而这一条用例要断言的恰恰是前者。
func waitForLog(t *testing.T, logs *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(logs.String(), want) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("等了 5s 也没等到日志 %q:\n%s", want, logs.String())
}

// 两个地址都绑不上时，**两条原因都要出现**。
//
// 只报第一条是这个 bug 的另一半：另一条错误留在没人读的 channel 里，于是
// 「另一个为什么也没起来」要再改一次端口、再跑一次才知道。这里刻意让两个
// 端口都被占，断言的就是「一次说清两条」。
func TestServeUntilSignalReportsEveryBindFailure(t *testing.T) {
	first, second := busyAddr(t), busyAddr(t)

	var logs bytes.Buffer
	err := serveUntilSignal(context.Background(), slog.New(slog.NewTextHandler(&logs, nil)), time.Second, []namedServer{
		{name: "internal", srv: &http.Server{Addr: first}},
		{name: "public", srv: &http.Server{Addr: second}},
	})
	if err == nil {
		t.Fatal("两个地址都绑不上时应当启动失败")
	}
	for _, name := range []string{"internal", "public"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("错误里缺 %s 那一条——只报第一条就是这个形状: %v", name, err)
		}
	}
	// 一条都没绑上，日志里就不该有「开始监听」。
	if strings.Contains(logs.String(), "开始监听") {
		t.Errorf("绑不上时不该打「开始监听」: %s", logs.String())
	}
}

// 一个绑不上、另一个绑得上时：整体退出，且**不留下一个半死的监听器**。
//
// 半死的判据不是「返回了错误」（那是设计），而是「已经绑上的那个端口有没有
// 被放开」——没放开的话，一个端口在听、另一个没起来，而进程正在退出；
// 端口要等到进程真的消失才归还。
//
// 两种排列都跑：绑不上的排在前或在后，已经绑上的那些都该被关掉。
func TestServeUntilSignalReleasesBoundListenerWhenAnotherFails(t *testing.T) {
	for _, failingFirst := range []bool{true, false} {
		name := "failing-second"
		if failingFirst {
			name = "failing-first"
		}
		t.Run(name, func(t *testing.T) {
			busy, free := busyAddr(t), freeAddr(t)
			busyServer := namedServer{name: "internal", srv: &http.Server{Addr: busy}}
			freeServer := namedServer{name: "public", srv: &http.Server{Addr: free}}
			servers := []namedServer{freeServer, busyServer}
			if failingFirst {
				servers = []namedServer{busyServer, freeServer}
			}

			var logs bytes.Buffer
			err := serveUntilSignal(context.Background(), slog.New(slog.NewTextHandler(&logs, nil)), time.Second, servers)
			if err == nil {
				t.Fatal("有一个地址绑不上时应当启动失败，而不是带着半个端口继续跑")
			}
			if !strings.Contains(err.Error(), "internal 监听失败") {
				t.Errorf("错误里应当点名绑不上的那一个: %v", err)
			}
			if strings.Contains(err.Error(), "public 监听失败") {
				t.Errorf("绑成功的那一个不该被报成失败: %v", err)
			}
			if strings.Contains(logs.String(), "开始监听") {
				t.Errorf("有一个地址没绑上时不该打「开始监听」: %s", logs.String())
			}

			ln, err := listenTCP(free)
			if err != nil {
				t.Fatalf("绑成功过的端口在返回之后仍被占着，说明留下了一个半死的监听器: %v", err)
			}
			_ = ln.Close()
		})
	}
}

// 「开始监听」这行日志的全部价值在于它是「已经在听」的证据，所以这里不满足于
// 断言日志里出现过这句话，而是**拿它当凭据去连**：它出现之后，那个端口必须
// 真的能服务。绑上了却没交给 Serve（或者把监听器交错了）会在这里挂——端口
// 连得上，但没人应答。
//
// 它管的是这条性质的正向那一半。「绑不上时不许打这行」是反向的那一半，由上面
// 两条用例钉住：那条路上连不上是必然的，所以只有「不打日志」才是可判定的。
func TestServeUntilSignalServesThePortItClaims(t *testing.T) {
	addr := freeAddr(t)
	var logs syncBuffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- serveUntilSignal(ctx, logger, 5*time.Second, []namedServer{{
			name: "public",
			srv: &http.Server{
				Addr: addr,
				Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusNoContent)
				}),
			},
		}})
	}()

	waitForLog(t, &logs, "开始监听")

	reqCtx, reqCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer reqCancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, "http://"+addr+"/", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("日志说 %s 已经在听，实际连不上: %v", addr, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("状态码 = %d，期望 204", resp.StatusCode)
	}

	// 退出信号那条路顺带走一遍：它同时证明上面那个监听器是被 Serve 接管的
	// （没接管的话 Shutdown 会因为「没有监听器」而立刻返回，看起来一样绿）。
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("收到退出信号后应当正常排空: %v", err)
	}
	if !strings.Contains(logs.String(), "已退出") {
		t.Errorf("排空之后应当留下「已退出」: %s", logs.String())
	}
}
